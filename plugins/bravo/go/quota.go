package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	quotaRefreshResourceUsage   = pluginapi.HostAuthQuotaScopeUsage
	quotaRefreshResourceProfile = pluginapi.HostAuthQuotaScopeProfile
	quotaFreshnessFresh         = "fresh"
	quotaFreshnessStale         = "stale"
	quotaFreshnessExpired       = "expired"
)

type quotaRefreshFailure struct {
	Code       string
	Message    string
	StatusCode int
	Retryable  bool
	RetryAfter string
	RetryAt    time.Time
}

func (failure *quotaRefreshFailure) Error() string {
	if failure == nil {
		return ""
	}
	if strings.TrimSpace(failure.Message) != "" {
		return failure.Message
	}
	return firstNonEmpty(failure.Code, "quota refresh failed")
}

type quotaRefreshCall struct {
	done chan struct{}
}

type quotaProviderGate struct {
	semaphore chan struct{}
	startMu   sync.Mutex
	lastStart time.Time
}

var (
	quotaRefreshNow       = func() time.Time { return time.Now().UTC() }
	quotaRefreshSleep     = time.Sleep
	quotaRefreshRuntimeWG sync.WaitGroup
	quotaRefreshRuntime   = struct {
		sync.Mutex
		calls         map[string]*quotaRefreshCall
		gates         map[string]*quotaProviderGate
		egressRetryAt map[string]time.Time
	}{
		calls:         make(map[string]*quotaRefreshCall),
		gates:         make(map[string]*quotaProviderGate),
		egressRetryAt: make(map[string]time.Time),
	}
)

// fetchQuotaSnapshot is intentionally a narrow seam. The host owns provider
// credentials and provider-specific quota HTTP calls; Bravo only consumes the
// normalized, secret-free result.
var fetchQuotaSnapshot = func(_ string, auth pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthQuotaGet, pluginapi.HostAuthQuotaRequest{
		AuthIndex: strings.TrimSpace(auth.AuthIndex),
		Scope:     strings.TrimSpace(resource),
	})
	if errCall != nil {
		return credentialQuotaState{}, errCall
	}
	var response pluginapi.HostAuthQuotaResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return credentialQuotaState{}, fmt.Errorf("decode host quota response: %w", errUnmarshal)
	}
	state := credentialQuotaState{
		Status:             response.Confidence,
		Confidence:         response.Confidence,
		Provider:           normalizeProvider(response.Provider),
		Plan:               strings.TrimSpace(response.PlanLabel),
		AccountLabel:       strings.TrimSpace(response.AccountLabel),
		WorkspaceLabel:     strings.TrimSpace(response.WorkspaceLabel),
		RefreshedAt:        firstQuotaObservedAt(response.UsageObservedAt, response.ObservedAt),
		ConfirmedAt:        firstQuotaObservedAt(response.UsageObservedAt, response.ObservedAt),
		ProfileRefreshedAt: response.ProfileObservedAt.UTC(),
	}
	if resource == quotaRefreshResourceProfile {
		if response.ProfileError != nil {
			return credentialQuotaState{}, quotaFailureFromHost(response.ProfileError)
		}
		if state.ProfileRefreshedAt.IsZero() {
			state.ProfileRefreshedAt = response.ObservedAt.UTC()
		}
		if state.ProfileRefreshedAt.IsZero() {
			return credentialQuotaState{}, &quotaRefreshFailure{Code: "response_invalid", Message: "host profile response has no observation time"}
		}
		return state, nil
	}
	usageError := response.UsageError
	if usageError == nil {
		usageError = response.Error
	}
	if usageError != nil {
		return credentialQuotaState{}, quotaFailureFromHost(usageError)
	}
	for _, window := range response.Windows {
		value := quotaWindowState{
			UsedPercent:      window.UsedPercent,
			RemainingPercent: window.RemainingPercent,
			ResetAt:          window.ResetAt.UTC(),
			ResetMode:        strings.ToLower(strings.TrimSpace(window.ResetMode)),
		}
		switch strings.ToLower(strings.TrimSpace(window.Kind)) {
		case pluginapi.HostAuthQuotaWindowKindSession:
			state.Session = value
		case pluginapi.HostAuthQuotaWindowKindWeekly:
			state.Weekly = value
		case pluginapi.HostAuthQuotaWindowKindModelWeekly:
			state.ModelWeekly = append(state.ModelWeekly, modelQuotaWindowState{
				Model:            strings.ToLower(strings.TrimSpace(window.ModelFamily)),
				quotaWindowState: value,
			})
		}
	}
	if quotaConfidence(state) == pluginapi.HostAuthQuotaConfidenceConfirmed {
		if !validConfirmedQuotaWindow(state.Session) || !validConfirmedQuotaWindow(state.Weekly) {
			return credentialQuotaState{}, fmt.Errorf("host confirmed quota without required windows")
		}
	}
	return normalizedQuotaState(state), nil
}

func firstQuotaObservedAt(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value.UTC()
		}
	}
	return time.Time{}
}

func quotaFailureFromHost(failure *pluginapi.HostAuthQuotaError) *quotaRefreshFailure {
	if failure == nil {
		return &quotaRefreshFailure{Code: "quota_unavailable", Message: "quota refresh failed", Retryable: true}
	}
	return &quotaRefreshFailure{
		Code: strings.TrimSpace(failure.Code), Message: strings.TrimSpace(failure.Message),
		StatusCode: failure.StatusCode, Retryable: failure.Retryable,
		RetryAfter: strings.TrimSpace(failure.RetryAfter), RetryAt: failure.RetryAt.UTC(),
	}
}

func refreshQuotaIfNeeded(hostCallbackID string, auth pluginapi.HostAuthFileEntry, force bool) credentialQuotaState {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	current := quotaSnapshot(authIndex)
	if strings.TrimSpace(hostCallbackID) == "" || authIndex == "" {
		return normalizedQuotaState(current)
	}
	cfg := loadedConfig()
	now := quotaRefreshNow().UTC()
	for _, resource := range []string{quotaRefreshResourceUsage, quotaRefreshResourceProfile} {
		// The existing management action means "refresh quotas", so force
		// applies to usage only. Profile keeps its independent long TTL.
		forceResource := force && resource == quotaRefreshResourceUsage
		if forceResource || quotaResourceNeedsRefresh(current, authIndex, resource, cfg, now) {
			startQuotaRefresh(hostCallbackID, auth, resource, forceResource)
		}
	}
	return normalizedQuotaState(current)
}

func quotaNeedsRefresh(quota credentialQuotaState, staleAfter time.Duration, now time.Time) bool {
	if quota.Dirty {
		return true
	}
	confirmedAt := quotaConfirmedAt(quota)
	if confirmedAt.IsZero() {
		return true
	}
	if staleAfter <= 0 {
		staleAfter = time.Minute
	}
	return now.Sub(confirmedAt) >= staleAfter
}

func refreshQuotaSnapshots(hostCallbackID string, auths []pluginapi.HostAuthFileEntry, force bool) {
	if strings.TrimSpace(hostCallbackID) == "" || len(auths) == 0 {
		return
	}
	for _, auth := range auths {
		provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type))
		if provider != "claude" && provider != "codex" {
			continue
		}
		if strings.TrimSpace(auth.AuthIndex) == "" || classifyBravoAuthPoolHealth(provider, auth, time.Now()) != bravoAuthReady {
			continue
		}
		_ = refreshQuotaIfNeeded(hostCallbackID, auth, force)
	}
}

func refreshQuotaResourceNow(hostCallbackID string, auth pluginapi.HostAuthFileEntry, resource string, force bool) credentialQuotaState {
	done := startQuotaRefresh(hostCallbackID, auth, resource, force)
	if done != nil {
		<-done
	}
	return normalizedQuotaState(quotaSnapshot(auth.AuthIndex))
}

func startQuotaRefresh(hostCallbackID string, auth pluginapi.HostAuthFileEntry, resource string, force bool) <-chan struct{} {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	resource = strings.ToLower(strings.TrimSpace(resource))
	if strings.TrimSpace(hostCallbackID) == "" || authIndex == "" ||
		(resource != quotaRefreshResourceUsage && resource != quotaRefreshResourceProfile) {
		return closedQuotaRefreshChannel()
	}
	now := quotaRefreshNow().UTC()
	cfg := loadedConfig()
	current := quotaSnapshot(authIndex)
	refreshState := quotaResourceRefreshState(current, resource)
	if !refreshState.NextAttemptAt.IsZero() && now.Before(refreshState.NextAttemptAt) {
		return closedQuotaRefreshChannel()
	}
	if !force && !quotaResourceNeedsRefresh(current, authIndex, resource, cfg, now) {
		return closedQuotaRefreshChannel()
	}
	provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type, current.Provider))
	egressKey := quotaRefreshEgressKey(provider, auth.EgressID)
	key := authIndex + "\x1f" + resource
	quotaRefreshRuntime.Lock()
	if retryAt := quotaRefreshRuntime.egressRetryAt[egressKey]; retryAt.After(now) {
		quotaRefreshRuntime.Unlock()
		return closedQuotaRefreshChannel()
	}
	if existing := quotaRefreshRuntime.calls[key]; existing != nil {
		quotaRefreshRuntime.Unlock()
		return existing.done
	}
	call := &quotaRefreshCall{done: make(chan struct{})}
	quotaRefreshRuntime.calls[key] = call
	quotaRefreshRuntimeWG.Add(1)
	quotaRefreshRuntime.Unlock()
	go func() {
		defer quotaRefreshRuntimeWG.Done()
		defer func() {
			quotaRefreshRuntime.Lock()
			delete(quotaRefreshRuntime.calls, key)
			close(call.done)
			quotaRefreshRuntime.Unlock()
		}()
		runQuotaRefresh(hostCallbackID, auth, resource, provider, egressKey, cfg)
	}()
	return call.done
}

func runQuotaRefresh(hostCallbackID string, auth pluginapi.HostAuthFileEntry, resource, provider, egressKey string, cfg pluginConfig) {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	gate := quotaProviderRefreshGate(egressKey, cfg.QuotaRefreshProviderConcurrency)
	gate.semaphore <- struct{}{}
	defer func() { <-gate.semaphore }()
	gate.startMu.Lock()
	now := quotaRefreshNow().UTC()
	minimumInterval := time.Duration(cfg.QuotaRefreshProviderMinIntervalMS) * time.Millisecond
	if wait := gate.lastStart.Add(minimumInterval).Sub(now); wait > 0 {
		quotaRefreshSleep(wait)
		now = quotaRefreshNow().UTC()
	}
	gate.lastStart = now
	gate.startMu.Unlock()
	quotaRefreshRuntime.Lock()
	retryAt := quotaRefreshRuntime.egressRetryAt[egressKey]
	quotaRefreshRuntime.Unlock()
	if retryAt.After(now) {
		return
	}

	markQuotaRefreshAttempt(authIndex, resource, now)
	pendingAtStart := pendingReservationPercent(authIndex)
	refreshed, errFetch := fetchQuotaSnapshot(hostCallbackID, auth, resource)
	completedAt := quotaRefreshNow().UTC()
	if errFetch != nil {
		failure := normalizeQuotaRefreshFailure(errFetch)
		applyQuotaRefreshFailure(authIndex, resource, provider, egressKey, failure, completedAt)
		return
	}
	applyQuotaRefreshSuccess(authIndex, resource, provider, refreshed, pendingAtStart, completedAt)
}

func quotaProviderRefreshGate(egressKey string, concurrency int) *quotaProviderGate {
	if concurrency <= 0 {
		concurrency = 1
	}
	key := strings.TrimSpace(egressKey) + fmt.Sprintf("/%d", concurrency)
	quotaRefreshRuntime.Lock()
	defer quotaRefreshRuntime.Unlock()
	gate := quotaRefreshRuntime.gates[key]
	if gate == nil {
		gate = &quotaProviderGate{semaphore: make(chan struct{}, concurrency)}
		quotaRefreshRuntime.gates[key] = gate
	}
	return gate
}

func markQuotaRefreshAttempt(authIndex, resource string, at time.Time) {
	quota := quotaSnapshot(authIndex)
	state := quotaResourceRefreshState(quota, resource)
	state.AttemptCount++
	state.LastAttemptAt = at.UTC()
	setQuotaResourceRefreshState(&quota, resource, state)
	storeQuotaSnapshot(authIndex, quota)
}

func applyQuotaRefreshSuccess(authIndex, resource, provider string, refreshed credentialQuotaState, pendingAtStart float64, completedAt time.Time) {
	current := quotaSnapshot(authIndex)
	state := quotaResourceRefreshState(current, resource)
	state.SuccessCount++
	state.LastSuccessAt = completedAt.UTC()
	state.LastFailureAt = time.Time{}
	state.ConsecutiveFailure = 0
	state.NextAttemptAt = time.Time{}
	state.Error = nil
	if resource == quotaRefreshResourceProfile {
		current.Provider = normalizeProvider(firstNonEmpty(refreshed.Provider, current.Provider, provider))
		current.Plan = strings.TrimSpace(firstNonEmpty(refreshed.Plan, current.Plan))
		current.AccountLabel = strings.TrimSpace(firstNonEmpty(refreshed.AccountLabel, current.AccountLabel))
		current.WorkspaceLabel = strings.TrimSpace(firstNonEmpty(refreshed.WorkspaceLabel, current.WorkspaceLabel))
		current.ProfileRefreshedAt = firstQuotaObservedAt(refreshed.ProfileRefreshedAt, completedAt)
		current.ProfileRefresh = state
		storeQuotaSnapshot(authIndex, current)
		return
	}
	refreshed = normalizedQuotaState(refreshed)
	confirmedAt := quotaConfirmedAt(refreshed)
	if confirmedAt.IsZero() {
		confirmedAt = completedAt.UTC()
	}
	if !quotaConfirmedAt(current).IsZero() && confirmedAt.Before(quotaConfirmedAt(current)) {
		current.UsageRefresh = state
		storeQuotaSnapshot(authIndex, current)
		return
	}
	// Preserve independently refreshed presentation state.
	refreshed.ProfileRefreshedAt = current.ProfileRefreshedAt
	refreshed.ProfileRefresh = current.ProfileRefresh
	refreshed.UsageRefresh = state
	refreshed.ConfirmedAt = confirmedAt
	refreshed.RefreshedAt = confirmedAt
	refreshed.Confidence = "confirmed"
	refreshed.Status = "confirmed"
	refreshed.Error = ""
	refreshed.Dirty = false
	refreshed.Provider = normalizeProvider(firstNonEmpty(refreshed.Provider, current.Provider, provider))
	if refreshed.Plan == "" {
		refreshed.Plan = current.Plan
	}
	if refreshed.AccountLabel == "" {
		refreshed.AccountLabel = current.AccountLabel
	}
	if refreshed.WorkspaceLabel == "" {
		refreshed.WorkspaceLabel = current.WorkspaceLabel
	}
	storeQuotaSnapshot(authIndex, refreshed)
	// The adaptive preview only consumes this already-fetched snapshot. It never
	// triggers or accelerates polling and therefore adds zero provider requests.
	reconcileAdaptiveShadow(loadedConfig(), authIndex, current, refreshed, confirmedAt)
	clearPendingReservation(authIndex, pendingAtStart)
}

func applyQuotaRefreshFailure(authIndex, resource, provider, egressKey string, failure *quotaRefreshFailure, at time.Time) {
	current := quotaSnapshot(authIndex)
	state := quotaResourceRefreshState(current, resource)
	state.FailureCount++
	state.LastFailureAt = at.UTC()
	state.ConsecutiveFailure++
	state.Error = &quotaRefreshErrorState{
		Code: failure.Code, Message: failure.Message, StatusCode: failure.StatusCode,
		Retryable: failure.Retryable, RetryAfter: failure.RetryAfter, RetryAt: failure.RetryAt.UTC(),
	}
	state.NextAttemptAt = failure.RetryAt.UTC()
	if state.NextAttemptAt.IsZero() || !state.NextAttemptAt.After(at) {
		state.NextAttemptAt = at.Add(quotaRefreshBackoff(authIndex, resource, state.ConsecutiveFailure))
	}
	setQuotaResourceRefreshState(&current, resource, state)
	if resource == quotaRefreshResourceUsage {
		// Error is a compatibility display alias only. Confidence and LKG are
		// deliberately untouched.
		current.Error = strings.TrimSpace(failure.Message)
	}
	storeQuotaSnapshot(authIndex, current)
	if failure.StatusCode == 429 && state.NextAttemptAt.After(at) {
		quotaRefreshRuntime.Lock()
		if existing := quotaRefreshRuntime.egressRetryAt[egressKey]; state.NextAttemptAt.After(existing) {
			quotaRefreshRuntime.egressRetryAt[egressKey] = state.NextAttemptAt
		}
		quotaRefreshRuntime.Unlock()
	}
}

func quotaRefreshEgressKey(provider, egressID string) string {
	egressID = strings.TrimSpace(egressID)
	if egressID == "" {
		egressID = "direct"
	}
	return normalizeProvider(provider) + "\x1f" + egressID
}

func normalizeQuotaRefreshFailure(err error) *quotaRefreshFailure {
	if typed, ok := err.(*quotaRefreshFailure); ok && typed != nil {
		if strings.TrimSpace(typed.Code) == "" {
			typed.Code = "quota_unavailable"
		}
		return typed
	}
	return &quotaRefreshFailure{Code: "quota_unavailable", Message: "confirmed quota refresh failed", Retryable: true}
}

func quotaRefreshBackoff(authIndex, resource string, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	capDelay := 5 * time.Second
	for count := 1; count < failures && capDelay < 5*time.Minute; count++ {
		capDelay *= 2
		if capDelay > 5*time.Minute {
			capDelay = 5 * time.Minute
		}
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%s\x1f%d", authIndex, resource, failures)))
	value := binary.BigEndian.Uint64(digest[:8])
	if capDelay <= time.Millisecond {
		return capDelay
	}
	return time.Duration(value%uint64(capDelay-time.Millisecond)) + time.Millisecond
}

func quotaResourceRefreshState(quota credentialQuotaState, resource string) quotaRefreshState {
	if resource == quotaRefreshResourceProfile {
		return quota.ProfileRefresh
	}
	return quota.UsageRefresh
}

func setQuotaResourceRefreshState(quota *credentialQuotaState, resource string, state quotaRefreshState) {
	if quota == nil {
		return
	}
	if resource == quotaRefreshResourceProfile {
		quota.ProfileRefresh = state
		return
	}
	quota.UsageRefresh = state
}

func quotaResourceNeedsRefresh(quota credentialQuotaState, authIndex, resource string, cfg pluginConfig, now time.Time) bool {
	state := quotaResourceRefreshState(quota, resource)
	if state.NextAttemptAt.After(now) {
		return false
	}
	var observedAt time.Time
	var ttl time.Duration
	if resource == quotaRefreshResourceProfile {
		observedAt = quota.ProfileRefreshedAt
		ttl = time.Duration(cfg.QuotaProfileRefreshSeconds) * time.Second
	} else {
		observedAt = quotaConfirmedAt(quota)
		ttl = time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	}
	if observedAt.IsZero() {
		return true
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	due := observedAt.Add(ttl).Add(quotaRefreshJitter(authIndex, resource, ttl, cfg.QuotaRefreshJitterPercent))
	return !now.Before(due)
}

func quotaRefreshJitter(authIndex, resource string, ttl time.Duration, percent int) time.Duration {
	if percent <= 0 || ttl <= 0 {
		return 0
	}
	if percent > 100 {
		percent = 100
	}
	window := ttl * time.Duration(percent) / 100
	if window <= 0 {
		return 0
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(authIndex) + "\x1f" + resource))
	return time.Duration(binary.BigEndian.Uint64(digest[:8]) % uint64(window))
}

func closedQuotaRefreshChannel() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func resetQuotaRefreshRuntimeForTest() {
	quotaRefreshRuntime.Lock()
	quotaRefreshRuntime.calls = make(map[string]*quotaRefreshCall)
	quotaRefreshRuntime.gates = make(map[string]*quotaProviderGate)
	quotaRefreshRuntime.egressRetryAt = make(map[string]time.Time)
	quotaRefreshRuntime.Unlock()
}

func normalizedQuotaState(quota credentialQuotaState) credentialQuotaState {
	quota.Confidence = quotaConfidence(quota)
	quota.Status = quota.Confidence
	if quota.Confidence == "confirmed" {
		if quota.ConfirmedAt.IsZero() {
			quota.ConfirmedAt = quota.RefreshedAt.UTC()
		}
		if !quota.ConfirmedAt.IsZero() {
			quota.RefreshedAt = quota.ConfirmedAt.UTC()
		}
	}
	quota.Session = normalizeQuotaWindow(quota.Session)
	quota.Weekly = normalizeQuotaWindow(quota.Weekly)
	for index := range quota.ModelWeekly {
		quota.ModelWeekly[index].quotaWindowState = normalizeQuotaWindow(quota.ModelWeekly[index].quotaWindowState)
		quota.ModelWeekly[index].Model = strings.ToLower(strings.TrimSpace(quota.ModelWeekly[index].Model))
	}
	return quota
}

func quotaConfidence(quota credentialQuotaState) string {
	value := strings.ToLower(strings.TrimSpace(firstNonEmpty(quota.Confidence, quota.Status)))
	switch value {
	case "confirmed", "unknown":
		return value
	default:
		return "unknown"
	}
}

func quotaConfirmedAt(quota credentialQuotaState) time.Time {
	if !quota.ConfirmedAt.IsZero() {
		return quota.ConfirmedAt.UTC()
	}
	return quota.RefreshedAt.UTC()
}

func quotaFreshnessAt(quota credentialQuotaState, model string, cfg pluginConfig, now time.Time) string {
	if quotaConfidence(quota) != "confirmed" {
		return quotaFreshnessExpired
	}
	confirmedAt := quotaConfirmedAt(quota)
	// In-memory tests and legacy runtime callers constructed confirmed quota
	// before confirmation timestamps existed. Persisted v2 state is migrated
	// explicitly, so zero here remains a compatibility-only fresh snapshot.
	if confirmedAt.IsZero() {
		return quotaFreshnessFresh
	}
	session, weekly := effectiveQuotaWindows(quota, model)
	for _, window := range []quotaWindowState{session, weekly} {
		if window.ResetMode == pluginapi.HostAuthQuotaResetModeScheduled &&
			!window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			return quotaFreshnessExpired
		}
	}
	age := now.UTC().Sub(confirmedAt)
	if age < 0 {
		age = 0
	}
	refreshAfter := time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	if refreshAfter <= 0 {
		refreshAfter = time.Duration(cfg.QuotaRefreshSeconds) * time.Second
	}
	if refreshAfter <= 0 {
		refreshAfter = time.Minute
	}
	if age < refreshAfter {
		return quotaFreshnessFresh
	}
	maxStale := time.Duration(cfg.QuotaUsageMaxStaleSeconds) * time.Second
	if maxStale < refreshAfter {
		maxStale = 15 * time.Minute
		if maxStale < refreshAfter {
			maxStale = refreshAfter
		}
	}
	if age < maxStale {
		return quotaFreshnessStale
	}
	return quotaFreshnessExpired
}

func quotaRoutingConfidenceAt(quota credentialQuotaState, model string, cfg pluginConfig, now time.Time) string {
	if quotaConfidence(quota) != "confirmed" || quotaFreshnessAt(quota, model, cfg, now) == quotaFreshnessExpired {
		return "unknown"
	}
	return "confirmed"
}

func normalizeQuotaWindow(window quotaWindowState) quotaWindowState {
	if window.RemainingPercent == 0 && window.UsedPercent > 0 {
		window.RemainingPercent = 100 - window.UsedPercent
	}
	window.UsedPercent = clampPercent(window.UsedPercent)
	window.RemainingPercent = clampPercent(window.RemainingPercent)
	window.ResetMode = strings.ToLower(strings.TrimSpace(window.ResetMode))
	if !window.ResetAt.IsZero() && window.ResetMode == "" {
		window.ResetMode = pluginapi.HostAuthQuotaResetModeScheduled
	}
	return window
}

func validConfirmedQuotaWindow(window quotaWindowState) bool {
	window = normalizeQuotaWindow(window)
	switch window.ResetMode {
	case pluginapi.HostAuthQuotaResetModeScheduled:
		return !window.ResetAt.IsZero()
	case pluginapi.HostAuthQuotaResetModeInactive, pluginapi.HostAuthQuotaResetModeNotApplicable:
		return window.ResetAt.IsZero() && window.UsedPercent == 0 && window.RemainingPercent == 100
	default:
		return false
	}
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func effectiveQuotaWindows(quota credentialQuotaState, model string) (quotaWindowState, quotaWindowState) {
	session := normalizeQuotaWindow(quota.Session)
	weekly := normalizeQuotaWindow(quota.Weekly)
	model = strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range quota.ModelWeekly {
		if !quotaModelMatches(model, candidate.Model) {
			continue
		}
		window := normalizeQuotaWindow(candidate.quotaWindowState)
		if window.RemainingPercent < weekly.RemainingPercent {
			weekly = window
		}
	}
	return session, weekly
}

func quotaModelMatches(requested, quotaModel string) bool {
	quotaModel = strings.ToLower(strings.TrimSpace(quotaModel))
	if quotaModel == "" {
		return false
	}
	if requested == quotaModel || strings.Contains(requested, quotaModel) {
		return true
	}
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(quotaModel, family) && strings.Contains(requested, family) {
			return true
		}
	}
	return false
}

func tariffByID(cfg pluginConfig, id string) tariffConfig {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, tariff := range cfg.Tariffs {
		if tariff.ID == id {
			return tariff
		}
	}
	for _, tariff := range cfg.Tariffs {
		if tariff.ID == "x1" {
			return tariff
		}
	}
	return tariffConfig{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}
}

func subscriptionPolicy(cfg pluginConfig, authIndex string) subscriptionConfig {
	authIndex = strings.TrimSpace(authIndex)
	for _, subscription := range cfg.Subscriptions {
		if subscription.AuthIndex == authIndex {
			return subscription
		}
	}
	return subscriptionConfig{AuthIndex: authIndex, Tariff: "auto"}
}

func subscriptionEnabled(subscription subscriptionConfig) bool {
	return subscription.Enabled == nil || *subscription.Enabled
}

func effectiveTariff(cfg pluginConfig, subscription subscriptionConfig, provider string, quota credentialQuotaState) tariffConfig {
	id := strings.ToLower(strings.TrimSpace(subscription.Tariff))
	if id == "" || id == "auto" {
		id = inferredTariffID(firstNonEmpty(quota.Provider, provider), quota.Plan)
	}
	return tariffByID(cfg, id)
}

func inferredTariffID(provider, plan string) string {
	provider = normalizeProvider(provider)
	plan = strings.ToLower(strings.TrimSpace(plan))
	// CLIProxyAPI already derives Codex plan_type from the signed OpenAI token
	// and exposes it as the normalized host quota PlanLabel. "pro" is
	// provider-specific: ChatGPT Pro is the 20x tier, while Claude Pro remains
	// the baseline tier.
	if provider == "codex" && (plan == "pro" || strings.Contains(plan, "chatgpt pro") || strings.Contains(plan, "x20")) {
		return "x20"
	}
	for _, marker := range []string{"team", "business", "enterprise", "max", "x5"} {
		if strings.Contains(plan, marker) {
			return "x5"
		}
	}
	return "x1"
}
