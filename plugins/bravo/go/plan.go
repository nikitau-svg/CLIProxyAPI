package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type cooldownEntry struct {
	Until         time.Time
	ObservedAt    time.Time
	Reason        string
	Provider      string
	AuthID        string
	Model         string
	ProviderError providererror.Detail
}

var runtimeState = struct {
	sync.RWMutex
	Cooldowns map[string]cooldownEntry
	Attempts  []attemptRecord
}{
	Cooldowns: make(map[string]cooldownEntry),
}

func buildExecutionPlan(req rpcExecutorRequest, logicalName string, model logicalModel, contract requestCapabilityContract) ([]executionAttempt, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{
		"host_callback_id": req.HostCallbackID,
	})
	if errCall != nil {
		return nil, errCall
	}
	var authResp hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &authResp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host auth list: %w", errUnmarshal)
	}
	// The background quota daemon owns the trusted host-wide credential catalog.
	// Project pools are an execution/trace authorization boundary, not a reason
	// to drop other accounts from global limit/reset tracking. This only updates
	// the daemon's in-memory catalog; provider I/O stays asynchronous,
	// cadence-bound, and singleflight-coalesced.
	observeQuotaPolling(req.HostCallbackID, authResp.Files)
	sticky := executionStickyKey(req.ExecutorRequest)
	now := time.Now()
	cfg := loadedConfig()
	project, authenticatedProject := authenticatedExecutionProject(req, cfg)
	requestFeatures := adaptiveRequestFeatures{}
	if authenticatedProject && cfg.AllocatorMode != "off" {
		requestFeatures = buildAdaptiveRequestFeatures(executionBodyView(req))
	}
	if authenticatedProject {
		// allowed_auth_ids is an authorization boundary, not an allocator hint.
		// Apply it before provider eligibility, quota observation, primary
		// ordering, and every off/observe/enforce allocator branch.
		authResp.Files = filterProjectAllowedAuths(project, authResp.Files)
	}
	plan := make([]executionAttempt, 0)
	// Every candidate that drops out records why. An empty plan is reported as
	// "no healthy account", which is only one of the reasons a candidate can
	// fail — a rejected capability contract or a provider with no credentials
	// look identical to the client otherwise, and the true cause is lost.
	rejections := make([]candidateRejection, 0, len(model.Candidates))
	for _, item := range model.Candidates {
		resolved, errContract := resolveCandidateContract(item, contract)
		if errContract != nil {
			rejections = appendBoundedCandidateRejections(rejections, candidateRejection{
				Provider: normalizeProvider(item.Provider),
				Model:    item.Model,
				Stage:    "contract",
				Code:     capabilityContractCode(errContract),
				Reason:   errContract.Error(),
			})
			continue
		}
		eligible := eligibleAuths(resolved, authResp.Files, now)
		if len(eligible) == 0 {
			rejections = appendBoundedCandidateRejections(rejections, candidateRejection{
				Provider: normalizeProvider(resolved.Provider),
				Model:    resolved.Model,
				Stage:    "eligibility",
				Code:     "bravo_no_eligible_account",
				Reason:   authRejectionSummary(resolved, authResp.Files, now),
			})
			continue
		}
		orderAuths(eligible, sticky, resolved)
		attempts := make([]executionAttempt, 0, len(eligible))
		if authenticatedProject && cfg.AllocatorMode != "off" {
			requestShape := adaptiveRequestShapeFromFeatures(requestFeatures, resolved)
			allocated := allocateCandidateAuthsForShape(
				cfg, project, resolved, eligible, sticky, requestShape,
			)
			if cfg.AllocatorMode == "enforce" {
				attempts = allocated
				if len(attempts) == 0 {
					attempts = compactBypassCandidateAttempts(req, cfg, project, resolved, eligible, sticky, requestShape)
				}
			} else {
				// Observe mode executes the pre-v0.4 order while still refreshing
				// quota and calculating the allocation decision.
				attempts = observeAllocatorAttempts(cfg, project, resolved, eligible, allocated, requestShape)
			}
		} else {
			for _, auth := range eligible {
				attempts = append(attempts, executionAttempt{Candidate: resolved, Auth: auth})
			}
		}
		if len(attempts) == 0 {
			// Eligible credentials existed, so the allocator withheld all of
			// them: quota below the tariff floor, a disabled subscription, or an
			// unknown snapshot under a deny policy.
			rejections = appendBoundedCandidateRejections(rejections,
				allocatorCandidateRejections(cfg, project, resolved, eligible, requestFeatures)...)
			continue
		}
		for _, allocated := range attempts {
			allocated.LogicalModel = logicalName
			allocated.RequestedEffort = requestedEffortValue(contract.Effort)
			allocated.EffectiveEffort = normalizeEffort(resolved.Effort)
			plan = append(plan, allocated)
		}
	}
	if len(plan) == 0 {
		// Every compatible candidate has already been considered. Primary
		// subscriptions were allowed down to 0%; a secondary rejected here is
		// protected by the operator's hard floor. Silently rebuilding an
		// unmanaged plan would turn that floor into a display-only preference.
		// /compact has its own explicit, rate-limited exception path.
		cause := noEligibleCandidateError(logicalName, contract, rejections)
		return nil, &executionPlanError{Cause: cause, Rejections: append([]candidateRejection(nil), rejections...)}
	}
	if len(rejections) > 0 {
		plan[0].PreflightRejections = append([]candidateRejection(nil), rejections...)
	}
	return plan, nil
}

func allocatorCandidateRejections(
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	requestFeatures adaptiveRequestFeatures,
) []candidateRejection {
	requestShape := adaptiveRequestShapeFromFeatures(requestFeatures, item)
	now := time.Now()
	primaryIndexes := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	rejections := make([]candidateRejection, 0, min(len(auths), maxPersistedRouteTraceAttempts))
	for _, auth := range auths {
		authIndex := stableAuthIndex(auth)
		subscription := subscriptionPolicy(cfg, authIndex)
		if !subscriptionEnabled(subscription) {
			rejections = appendBoundedCandidateRejections(rejections, candidateRejection{
				Provider: normalizeProvider(item.Provider), Model: item.Model, Stage: "allocator",
				Code: "bravo_allocator_withheld", Reason: "Подписка выключена политикой Bravo.",
				AuthIndex: authIndex,
			})
			continue
		}
		quota := normalizedQuotaState(quotaSnapshot(authIndex))
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
		reservation := adaptiveReservationForShape(auth, tariff, requestShape, now)
		_, primary := primaryIndexes[authIndex]
		attempt := executionAttempt{
			Candidate: item, Auth: auth, ProjectID: project.ID, Primary: primary,
			AllocatorManaged: true, ReservationPercent: reservation,
			AdaptiveReserveKey:   adaptiveProfileKey(authIndex, requestShape),
			AdaptiveRequestShape: requestShape, AdaptiveBaselinePercent: tariff.ReservationPercent,
			TariffID: tariff.ID,
		}
		attempt.AdaptiveTrace = captureObserveAdaptiveDecision(cfg, attempt, false, now)
		attempt.AdaptiveTrace.mode = "enforce"
		code := adaptivePlanRejectionCode(attempt.AdaptiveTrace.rejectionCause)
		if code == "" {
			code = "bravo_allocator_withheld"
		}
		reason := routeTraceMessageRU(code)
		if code == "bravo_allocator_withheld" {
			reason = "Внутренний распределитель Bravo не выпустил подписку: проверьте подтверждение квоты и резервные пороги."
		}
		rejections = appendBoundedCandidateRejections(rejections, candidateRejection{
			Provider: normalizeProvider(item.Provider), Model: item.Model, Stage: "allocator",
			Code: code, Reason: reason, AuthIndex: authIndex, AdaptiveTrace: attempt.AdaptiveTrace,
		})
	}
	return rejections
}

func adaptivePlanRejectionCode(cause adaptiveAdmissionRejectionCause) string {
	switch cause {
	case adaptiveRejectionLedgerSaturated:
		return "bravo_adaptive_ledger_saturated"
	case adaptiveRejectionEstimatorSaturated:
		return "bravo_adaptive_estimator_saturated"
	case adaptiveRejectionDemandSaturated:
		return "bravo_adaptive_demand_saturated"
	case adaptiveRejectionDurabilityUnavailable:
		return "bravo_adaptive_durability_unavailable"
	case adaptiveRejectionQuotaStale:
		return "bravo_adaptive_quota_stale"
	case adaptiveRejectionPrimaryZero:
		return "bravo_adaptive_primary_zero"
	case adaptiveRejectionConcurrency:
		return "bravo_adaptive_concurrency_recheck"
	case adaptiveRejectionFloor:
		return "bravo_allocator_reserve_floor"
	default:
		return ""
	}
}

func appendBoundedCandidateRejections(dst []candidateRejection, values ...candidateRejection) []candidateRejection {
	remaining := maxPersistedRouteTraceAttempts - len(dst)
	if remaining <= 0 {
		return dst
	}
	if len(values) > remaining {
		values = values[:remaining]
	}
	return append(dst, values...)
}

// allocatorBypassPlan is retained as a compatibility seam for older tests and
// call sites, but generic allocator rejection is now always fail-closed. An
// unmanaged attempt would bypass unknown-quota policy, adaptive uncertainty,
// pending debt and hard secondary floors. The explicit /compact path has its
// own narrow, cooldown-controlled policy and does not use this helper.
func allocatorBypassPlan(
	logicalName string,
	model logicalModel,
	contract requestCapabilityContract,
	auths []pluginapi.HostAuthFileEntry,
	rejections []candidateRejection,
	sticky string,
	now time.Time,
) []executionAttempt {
	return nil
}

// logAllocatorBypass records that budget policy was overridden to keep a request
// alive. This is a silent quota overspend otherwise — the one thing an operator
// must be able to see after the fact.
func logAllocatorBypass(logicalName string, rejections []candidateRejection) {
	details := make([]string, 0, len(rejections))
	for _, rejection := range rejections {
		if rejection.Stage == "allocator" {
			details = append(details, rejection.String())
		}
	}
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   "warn",
		"message": "Bravo: внутренний распределитель удержал все подписки; запрос обслуживается с контролируемым обходом резервного порога",
		"fields": map[string]any{
			"logical_model": logicalName,
			"withheld":      strings.Join(details, "; "),
		},
	})
}

// candidateRejection records why one candidate of a logical model dropped out.
type candidateRejection struct {
	Provider      string
	Model         string
	Stage         string
	Code          string
	Reason        string
	AuthIndex     string
	AdaptiveTrace adaptiveRouteDecision
}

func (r candidateRejection) String() string {
	return fmt.Sprintf("%s/%s %s(%s): %s", r.Provider, r.Model, r.Stage, r.Code, r.Reason)
}

type executionPlanError struct {
	Cause      error
	Rejections []candidateRejection
}

func (e *executionPlanError) Error() string {
	if e == nil || e.Cause == nil {
		return "Bravo has no eligible execution plan"
	}
	return e.Cause.Error()
}

func (e *executionPlanError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func executionPlanFailure(err error) (executionFailure, []candidateRejection) {
	failure := executionFailure{
		Code: "bravo_no_eligible_account", Message: err.Error(), Status: http.StatusServiceUnavailable, Retryable: true,
	}
	var planErr *executionPlanError
	if !errors.As(err, &planErr) || planErr == nil {
		return failure, nil
	}
	var contractErr *capabilityContractError
	if errors.As(planErr.Cause, &contractErr) {
		return contractFailure(contractErr), planErr.Rejections
	}
	bestCode, bestPriority := "", 0
	allAdaptive := len(planErr.Rejections) > 0
	for _, rejection := range planErr.Rejections {
		priority := adaptiveFailureCodePriority(rejection.Code)
		if priority == 0 {
			allAdaptive = false
			continue
		}
		if priority > bestPriority {
			bestCode, bestPriority = rejection.Code, priority
		}
	}
	if allAdaptive && bestCode != "" {
		failure.Code = bestCode
		failure.Message = routeTraceMessageRU(bestCode)
		failure.Retryable = bestCode != "bravo_adaptive_ledger_saturated"
	}
	return failure, planErr.Rejections
}

func capabilityContractCode(err error) string {
	var contractErr *capabilityContractError
	if errors.As(err, &contractErr) && contractErr != nil && strings.TrimSpace(contractErr.Code) != "" {
		return contractErr.Code
	}
	return "bravo_contract_rejected"
}

// authRejectionSummary counts why each credential of a provider was skipped, so
// "no eligible account" carries the actual health tally instead of a bare count.
func authRejectionSummary(item candidate, auths []pluginapi.HostAuthFileEntry, now time.Time) string {
	provider := normalizeProvider(item.Provider)
	allowed := make(map[string]struct{}, len(item.AuthIDs))
	for _, id := range item.AuthIDs {
		allowed[strings.TrimSpace(id)] = struct{}{}
	}
	tally := make(map[string]int)
	providerTotal := 0
	for _, auth := range auths {
		authProvider := normalizeProvider(auth.Provider)
		if authProvider == "" {
			authProvider = normalizeProvider(auth.Type)
		}
		if authProvider != provider {
			continue
		}
		providerTotal++
		if len(allowed) > 0 {
			if _, ok := allowed[strings.TrimSpace(auth.ID)]; !ok {
				if _, ok = allowed[auth.AuthIndex]; !ok {
					if _, ok = allowed[auth.Name]; !ok {
						tally["not_in_candidate_auth_ids"]++
						continue
					}
				}
			}
		}
		tally[string(classifyBravoAuthHealthForModel(provider, auth, item.Model, now))]++
	}
	if providerTotal == 0 {
		return fmt.Sprintf("no %s credential is visible to this project", provider)
	}
	reasons := make([]string, 0, len(tally))
	for reason, count := range tally {
		reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
	}
	sort.Strings(reasons)
	return fmt.Sprintf("%d %s credential(s): %s", providerTotal, provider, strings.Join(reasons, " "))
}

// noEligibleCandidateError reports the first cause rather than the generic one.
// A contract rejection is distinguished from an exhausted pool because they call
// for opposite responses: change the request, or wait for quota.
func noEligibleCandidateError(logicalName string, contract requestCapabilityContract, rejections []candidateRejection) error {
	details := make([]string, 0, len(rejections))
	stages := make(map[string]int, len(rejections))
	for _, rejection := range rejections {
		details = append(details, rejection.String())
		stages[rejection.Stage]++
	}
	logPlanRejections(logicalName, contract, details)

	if len(rejections) > 0 && stages["contract"] == len(rejections) {
		// Nothing was wrong with the accounts — no candidate accepted the
		// request's capabilities. Surface the upstream code verbatim.
		first := rejections[0]
		return &capabilityContractError{
			Code:     first.Code,
			Provider: first.Provider,
			Protocol: contract.Protocol,
			Message: fmt.Sprintf("no candidate of logical model %s accepts this request: %s",
				logicalName, strings.Join(details, "; ")),
		}
	}
	if len(details) == 0 {
		return fmt.Errorf("Bravo has no healthy account for logical model %s", logicalName)
	}
	return fmt.Errorf("Bravo has no healthy account for logical model %s (%s)",
		logicalName, strings.Join(details, "; "))
}

func logPlanRejections(logicalName string, contract requestCapabilityContract, details []string) {
	if len(details) == 0 {
		return
	}
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   "warn",
		"message": "Bravo: для логической модели не осталось доступного маршрута",
		"fields": map[string]any{
			"logical_model": logicalName,
			"protocol":      contract.Protocol,
			"capabilities":  strings.Join(contract.RequiredCapabilities(), ","),
			"rejections":    strings.Join(details, "; "),
		},
	})
}

func filterProjectAllowedAuths(project smartKeyConfig, auths []pluginapi.HostAuthFileEntry) []pluginapi.HostAuthFileEntry {
	if len(project.AllowedAuthIDs) == 0 {
		return auths
	}
	allowed := resolvedProjectAuthIndexes(project.AllowedAuthIDs, auths)
	out := make([]pluginapi.HostAuthFileEntry, 0, len(allowed))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if authIndex == "" {
			continue
		}
		if _, exists := allowed[authIndex]; exists {
			out = append(out, auth)
		}
	}
	return out
}

func resolvedProjectAuthIndexes(configured []string, auths []pluginapi.HostAuthFileEntry) map[string]struct{} {
	resolved := make(map[string]struct{}, len(configured))
	for _, value := range normalizeOpaqueStrings(configured) {
		if authIndex, ok := resolvePrimaryAuthIndex(value, auths); ok && strings.TrimSpace(authIndex) != "" {
			resolved[strings.TrimSpace(authIndex)] = struct{}{}
		}
	}
	return resolved
}

func requestedEffortValue(requested requestEffort) string {
	if !requested.Specified {
		return ""
	}
	return normalizeEffort(requested.Value)
}

func eligibleAuths(item candidate, auths []pluginapi.HostAuthFileEntry, now time.Time) []pluginapi.HostAuthFileEntry {
	provider := normalizeProvider(item.Provider)
	allowedIDs := make(map[string]struct{}, len(item.AuthIDs))
	for _, id := range item.AuthIDs {
		allowedIDs[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		authProvider := normalizeProvider(auth.Provider)
		if authProvider == "" {
			authProvider = normalizeProvider(auth.Type)
		}
		if authProvider != provider {
			continue
		}
		if classifyBravoAuthHealthForModel(provider, auth, item.Model, now) != bravoAuthReady {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if len(allowedIDs) > 0 {
			if _, ok := allowedIDs[id]; !ok {
				if _, ok = allowedIDs[auth.AuthIndex]; !ok {
					if _, ok = allowedIDs[auth.Name]; !ok {
						continue
					}
				}
			}
		}
		out = append(out, auth)
	}
	return out
}

func orderAuths(auths []pluginapi.HostAuthFileEntry, sticky string, item candidate) {
	sort.SliceStable(auths, func(i, j int) bool {
		if auths[i].Priority != auths[j].Priority {
			return auths[i].Priority > auths[j].Priority
		}
		left := rendezvousScore(sticky, item.Provider, item.Model, authIdentifier(auths[i]))
		right := rendezvousScore(sticky, item.Provider, item.Model, authIdentifier(auths[j]))
		if left == right {
			return authIdentifier(auths[i]) < authIdentifier(auths[j])
		}
		return left > right
	})
}

func executionStickyKey(req pluginapi.ExecutorRequest) string {
	if req.Metadata != nil {
		for _, key := range []string{"idempotency_key", "execution_session_id", "request_id"} {
			if value, ok := req.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	sum := sha256.Sum256(req.OriginalRequest)
	return fmt.Sprintf("%x", sum[:12])
}

func rendezvousScore(parts ...string) uint64 {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return binary.BigEndian.Uint64(sum[:8])
}

func authIdentifier(auth pluginapi.HostAuthFileEntry) string {
	for _, value := range []string{auth.ID, auth.AuthIndex, auth.Name} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func pinnedAuthID(auth pluginapi.HostAuthFileEntry) string {
	return strings.TrimSpace(auth.ID)
}

// cooldownKey scopes a cooldown to a provider, an account and — for model
// scoped failures — the physical model that produced it. An empty model keeps
// the legacy account-wide scope, which is what credential-level failures
// (401/403) still require.
func cooldownKey(provider, authID, model string) string {
	key := normalizeProvider(provider) + "\x00" + strings.TrimSpace(authID)
	if scoped := baseModelKey(strings.TrimSpace(model)); scoped != "" {
		key += "\x00" + scoped
	}
	return key
}

// cooldownActive reports whether the account is cooling for the given model.
// An account-wide cooldown suppresses every model, while a model-scoped one
// only suppresses its own model so a rate limit on Opus never takes Haiku or
// Sonnet down with it.
func cooldownActive(provider, authID, model string, now time.Time) bool {
	if cooldownEntryActive(cooldownKey(provider, authID, ""), now) {
		return true
	}
	if strings.TrimSpace(model) == "" {
		return false
	}
	return cooldownEntryActive(cooldownKey(provider, authID, model), now)
}

func cooldownEntryActive(key string, now time.Time) bool {
	runtimeState.RLock()
	entry, ok := runtimeState.Cooldowns[key]
	runtimeState.RUnlock()
	if !ok {
		return false
	}
	if entry.Until.After(now) {
		return true
	}
	return removeExpiredCooldownIfCurrent(key, entry, now)
}

func removeExpiredCooldownIfCurrent(key string, observed cooldownEntry, now time.Time) bool {
	runtimeState.Lock()
	current, ok := runtimeState.Cooldowns[key]
	if !ok {
		runtimeState.Unlock()
		return false
	}
	if current.Until.After(now) {
		runtimeState.Unlock()
		return true
	}
	if !sameCooldownInstance(current, observed) {
		runtimeState.Unlock()
		return false
	}
	delete(runtimeState.Cooldowns, key)
	runtimeState.Unlock()
	removePersistedCooldown(observed)
	return false
}

func removeRuntimeCooldownIfCurrent(key string, observed cooldownEntry) {
	runtimeState.Lock()
	current, ok := runtimeState.Cooldowns[key]
	if ok && sameCooldownInstance(current, observed) {
		delete(runtimeState.Cooldowns, key)
	}
	runtimeState.Unlock()
}

func sameCooldownInstance(left, right cooldownEntry) bool {
	return left.Until.Equal(right.Until) &&
		left.ObservedAt.Equal(right.ObservedAt) &&
		left.Reason == right.Reason &&
		left.Provider == right.Provider &&
		left.AuthID == right.AuthID &&
		left.Model == right.Model &&
		left.ProviderError == right.ProviderError
}

func setCooldown(provider, authID, model, reason string, until time.Time) {
	setCooldownWithProviderError(provider, authID, model, reason, until, nil)
}

func setCooldownWithProviderError(
	provider, authID, model, reason string,
	until time.Time,
	detail *providererror.Detail,
) {
	now := time.Now()
	if until.IsZero() || !until.After(now) {
		return
	}
	stateGeneration := bravoUsageState.generation.Load()
	provider = normalizeProvider(provider)
	authID = strings.TrimSpace(authID)
	model = baseModelKey(strings.TrimSpace(model))
	entry := cooldownEntry{
		Until:      until,
		ObservedAt: now.UTC(),
		Reason:     providererror.Sanitize(providererror.Detail{Reason: reason}).Reason,
		Provider:   provider,
		AuthID:     authID,
		Model:      model,
	}
	if detail != nil {
		entry.ProviderError = providererror.Sanitize(*detail)
		if model != "" {
			entry.ProviderError.Model = providererror.Sanitize(providererror.Detail{Model: model}).Model
		}
		if entry.ProviderError.Scope == "" && model != "" {
			entry.ProviderError.Scope = "model"
		}
	}
	runtimeState.Lock()
	runtimeState.Cooldowns[cooldownKey(provider, authID, model)] = entry
	runtimeState.Unlock()
	persistRuntimeCooldown(entry, stateGeneration)
}

func activeProviderModelCooldowns(provider, authID string, now time.Time) []cooldownEntry {
	provider = normalizeProvider(provider)
	authID = strings.TrimSpace(authID)
	if provider == "" || authID == "" {
		return nil
	}

	runtimeState.Lock()
	entries := make([]cooldownEntry, 0)
	expired := make([]cooldownEntry, 0)
	for key, entry := range runtimeState.Cooldowns {
		if !entry.Until.After(now) {
			delete(runtimeState.Cooldowns, key)
			expired = append(expired, entry)
			continue
		}
		if entry.Provider != provider ||
			entry.AuthID != authID ||
			strings.TrimSpace(entry.Model) == "" {
			continue
		}
		entries = append(entries, entry)
	}
	runtimeState.Unlock()
	for _, entry := range expired {
		removePersistedCooldown(entry)
	}

	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Model == entries[right].Model {
			return entries[left].Until.Before(entries[right].Until)
		}
		return entries[left].Model < entries[right].Model
	})
	return entries
}

// accountWideCooldownStatus lists HTTP statuses that invalidate the whole
// credential rather than a single model. Reviewed account-quota signals carry
// an explicit internal scope from the precise provider message; ambiguous
// quota text, model rate limits and transient upstream errors remain scoped to
// the physical model that failed.
func accountWideCooldownStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	default:
		return false
	}
}

func appendAttempt(record attemptRecord) {
	const maxAttemptRecords = 250
	runtimeState.Lock()
	runtimeState.Attempts = append(runtimeState.Attempts, record)
	if excess := len(runtimeState.Attempts) - maxAttemptRecords; excess > 0 {
		copy(runtimeState.Attempts, runtimeState.Attempts[excess:])
		runtimeState.Attempts = runtimeState.Attempts[:maxAttemptRecords]
	}
	runtimeState.Unlock()
}
