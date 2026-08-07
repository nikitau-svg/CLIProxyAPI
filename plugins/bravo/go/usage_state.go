package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	usageStateSchemaVersion  = 3
	usageSaveDebounce        = 250 * time.Millisecond
	sessionUsageWindow       = 5 * time.Hour
	weeklyUsageWindow        = 7 * 24 * time.Hour
	hourlyUsageRetention     = 31 * 24 * time.Hour
	dailyUsageRetention      = 400 * 24 * time.Hour
	dailyUsageBucketLayout   = "2006-01-02"
	usageDimensionKeyVersion = "v1"
)

type usageCounters struct {
	Requests            int64 `json:"requests"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	Failures            int64 `json:"failures"`
	LatencyMS           int64 `json:"latency_ms"`
	TTFTMS              int64 `json:"ttft_ms,omitempty"`
	TTFTSamples         int64 `json:"ttft_samples,omitempty"`
}

type usageAggregate struct {
	Total  usageCounters            `json:"total"`
	Hourly map[string]usageCounters `json:"hourly,omitempty"`
	Daily  map[string]usageCounters `json:"daily,omitempty"`
}

type modelUsageAggregate struct {
	Provider     string         `json:"provider,omitempty"`
	Model        string         `json:"model"`
	LogicalModel string         `json:"logical_model,omitempty"`
	Usage        usageAggregate `json:"usage"`
}

type projectSubscriptionModelUsageAggregate struct {
	ProjectID    string         `json:"project_id,omitempty"`
	AuthIndex    string         `json:"auth_index,omitempty"`
	Provider     string         `json:"provider,omitempty"`
	Model        string         `json:"model,omitempty"`
	LogicalModel string         `json:"logical_model,omitempty"`
	Usage        usageAggregate `json:"usage"`
}

type quotaWindowState struct {
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	ResetMode        string    `json:"reset_mode,omitempty"`
}

type modelQuotaWindowState struct {
	Model string `json:"model"`
	quotaWindowState
}

type quotaRefreshErrorState struct {
	Code       string    `json:"code"`
	Message    string    `json:"message,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	Retryable  bool      `json:"retryable,omitempty"`
	RetryAfter string    `json:"retry_after,omitempty"`
	RetryAt    time.Time `json:"retry_at,omitempty"`
}

type quotaRefreshState struct {
	AttemptCount       uint64                  `json:"attempt_count,omitempty"`
	SuccessCount       uint64                  `json:"success_count,omitempty"`
	FailureCount       uint64                  `json:"failure_count,omitempty"`
	LastAttemptAt      time.Time               `json:"last_attempt_at,omitempty"`
	LastSuccessAt      time.Time               `json:"last_success_at,omitempty"`
	LastFailureAt      time.Time               `json:"last_failure_at,omitempty"`
	ConsecutiveFailure int                     `json:"consecutive_failures,omitempty"`
	NextAttemptAt      time.Time               `json:"next_attempt_at,omitempty"`
	Error              *quotaRefreshErrorState `json:"error,omitempty"`
}

type credentialQuotaState struct {
	Status             string                  `json:"status,omitempty"` // retained for v0.4 state compatibility
	Confidence         string                  `json:"confidence"`       // confirmed, error, unknown
	Provider           string                  `json:"provider,omitempty"`
	Plan               string                  `json:"plan,omitempty"`
	AccountLabel       string                  `json:"account_label,omitempty"`
	WorkspaceLabel     string                  `json:"workspace_label,omitempty"`
	Session            quotaWindowState        `json:"session"`
	Weekly             quotaWindowState        `json:"weekly"`
	ModelWeekly        []modelQuotaWindowState `json:"model_weekly,omitempty"`
	RefreshedAt        time.Time               `json:"refreshed_at,omitempty"`
	Error              string                  `json:"error,omitempty"`
	ConfirmedAt        time.Time               `json:"confirmed_at,omitempty"`
	ProfileRefreshedAt time.Time               `json:"profile_refreshed_at,omitempty"`
	UsageRefresh       quotaRefreshState       `json:"usage_refresh,omitempty"`
	ProfileRefresh     quotaRefreshState       `json:"profile_refresh,omitempty"`
	Dirty              bool                    `json:"dirty,omitempty"`
}

// persistedCooldownEntry is the restart-safe subset of Bravo's routing
// barrier. ProviderError is already reviewed and sanitized; raw provider
// response bodies never enter this type.
type persistedCooldownEntry struct {
	Until         time.Time             `json:"until"`
	ObservedAt    time.Time             `json:"observed_at,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	Provider      string                `json:"provider"`
	AuthID        string                `json:"auth_id"`
	Model         string                `json:"model,omitempty"`
	ProviderError *providererror.Detail `json:"provider_error,omitempty"`
}

type persistedUsageState struct {
	SchemaVersion                  int                                                `json:"schema_version"`
	GlobalTotal                    usageAggregate                                     `json:"global_total"`
	AuthTotals                     map[string]*usageAggregate                         `json:"auth_totals"`
	ProjectTotals                  map[string]*usageAggregate                         `json:"project_totals"`
	ProviderTotals                 map[string]*usageAggregate                         `json:"provider_totals"`
	ModelTotals                    map[string]*modelUsageAggregate                    `json:"model_totals"`
	ProjectSubscriptionModelTotals map[string]*projectSubscriptionModelUsageAggregate `json:"project_subscription_model_totals"`
	Quotas                         map[string]*credentialQuotaState                   `json:"quotas"`
	Cooldowns                      map[string]*persistedCooldownEntry                 `json:"cooldowns,omitempty"`
	DimensionalStartedAt           time.Time                                          `json:"dimensional_started_at,omitempty"`
	UpdatedAt                      time.Time                                          `json:"updated_at,omitempty"`
}

type usageStateStore struct {
	mu         sync.RWMutex
	path       string
	state      persistedUsageState
	saveTimer  *time.Timer
	generation atomic.Uint64
}

var bravoUsageState = &usageStateStore{}

func newPersistedUsageState() persistedUsageState {
	return persistedUsageState{
		SchemaVersion:                  usageStateSchemaVersion,
		GlobalTotal:                    newUsageAggregate(),
		AuthTotals:                     make(map[string]*usageAggregate),
		ProjectTotals:                  make(map[string]*usageAggregate),
		ProviderTotals:                 make(map[string]*usageAggregate),
		ModelTotals:                    make(map[string]*modelUsageAggregate),
		ProjectSubscriptionModelTotals: make(map[string]*projectSubscriptionModelUsageAggregate),
		Quotas:                         make(map[string]*credentialQuotaState),
		Cooldowns:                      make(map[string]*persistedCooldownEntry),
	}
}

func newUsageAggregate() usageAggregate {
	return usageAggregate{
		Hourly: make(map[string]usageCounters),
		Daily:  make(map[string]usageCounters),
	}
}

func configureUsageState(path string) error {
	return bravoUsageState.configure(path)
}

func (store *usageStateStore) configure(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultStatePath
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saveTimer != nil {
		store.saveTimer.Stop()
		store.saveTimer = nil
	}
	if store.path != "" {
		if errFlush := store.saveLocked(); errFlush != nil {
			return errFlush
		}
		if store.path == path {
			restoreRuntimeCooldowns(store.state.Cooldowns, time.Now().UTC(), false)
			return nil
		}
	}
	state, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		return errLoad
	}
	store.path = path
	store.state = state
	store.generation.Add(1)
	restoreRuntimeCooldowns(state.Cooldowns, time.Now().UTC(), true)
	return nil
}

func loadUsageStateFile(path string) (persistedUsageState, error) {
	state := newPersistedUsageState()
	raw, errRead := os.ReadFile(path)
	if os.IsNotExist(errRead) {
		return state, nil
	}
	if errRead != nil {
		return state, fmt.Errorf("read state snapshot: %w", errRead)
	}
	if errUnmarshal := json.Unmarshal(raw, &state); errUnmarshal != nil {
		return newPersistedUsageState(), fmt.Errorf("decode state snapshot: %w", errUnmarshal)
	}
	switch state.SchemaVersion {
	case 1:
		migrateUsageStateV1(&state, time.Now().UTC())
	case 2:
		migrateUsageStateV2(&state)
	case usageStateSchemaVersion:
		normalizePersistedUsageState(&state)
		prunePersistedUsageState(&state, time.Now().UTC())
	default:
		return newPersistedUsageState(), fmt.Errorf("unsupported state schema version %d", state.SchemaVersion)
	}
	return state, nil
}

func migrateUsageStateV2(state *persistedUsageState) {
	if state == nil {
		return
	}
	normalizePersistedUsageState(state)
	for _, quota := range state.Quotas {
		if quota == nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(firstNonEmpty(quota.Confidence, quota.Status))) {
		case "confirmed":
			quota.ConfirmedAt = quota.RefreshedAt.UTC()
			quota.Confidence = "confirmed"
			quota.Status = "confirmed"
			quota.Error = ""
		case "error":
			// v2 copied old windows forward but overwrote RefreshedAt with the
			// failure time. Keep those values for display, but do not let the
			// unprovable timestamp authorize a secondary.
			quota.Confidence = "unknown"
			quota.Status = "unknown"
			quota.ConfirmedAt = time.Time{}
			if strings.TrimSpace(quota.Error) != "" {
				quota.UsageRefresh.Error = &quotaRefreshErrorState{
					Code: "legacy_refresh_failed", Message: strings.TrimSpace(quota.Error), Retryable: true,
				}
				quota.UsageRefresh.LastFailureAt = quota.RefreshedAt.UTC()
			}
		default:
			quota.Confidence = "unknown"
			quota.Status = "unknown"
		}
	}
	state.SchemaVersion = usageStateSchemaVersion
}

func migrateUsageStateV1(state *persistedUsageState, migratedAt time.Time) {
	if state == nil {
		return
	}
	normalizePersistedUsageState(state)
	for _, aggregate := range state.AuthTotals {
		migrateUsageAggregateV1(aggregate)
	}
	for _, aggregate := range state.ProjectTotals {
		migrateUsageAggregateV1(aggregate)
	}
	if usageCountersEmpty(state.GlobalTotal.Total) {
		source := state.AuthTotals
		if len(source) == 0 {
			source = state.ProjectTotals
		}
		for _, aggregate := range source {
			mergeUsageAggregate(&state.GlobalTotal, aggregate)
		}
	}
	state.SchemaVersion = usageStateSchemaVersion
	for _, quota := range state.Quotas {
		if quota != nil && quotaConfidence(*quota) == "confirmed" {
			quota.ConfirmedAt = quota.RefreshedAt.UTC()
		}
	}
	state.DimensionalStartedAt = migratedAt.UTC()
	prunePersistedUsageState(state, migratedAt.UTC())
}

func normalizePersistedUsageState(state *persistedUsageState) {
	if state == nil {
		return
	}
	ensureUsageAggregateMaps(&state.GlobalTotal)
	if state.AuthTotals == nil {
		state.AuthTotals = make(map[string]*usageAggregate)
	}
	if state.ProjectTotals == nil {
		state.ProjectTotals = make(map[string]*usageAggregate)
	}
	if state.ProviderTotals == nil {
		state.ProviderTotals = make(map[string]*usageAggregate)
	}
	if state.ModelTotals == nil {
		state.ModelTotals = make(map[string]*modelUsageAggregate)
	}
	if state.ProjectSubscriptionModelTotals == nil {
		state.ProjectSubscriptionModelTotals = make(map[string]*projectSubscriptionModelUsageAggregate)
	}
	if state.Quotas == nil {
		state.Quotas = make(map[string]*credentialQuotaState)
	}
	normalizedCooldowns := make(map[string]*persistedCooldownEntry, len(state.Cooldowns))
	for _, persisted := range state.Cooldowns {
		entry, ok := runtimeCooldownFromPersisted(persisted)
		if !ok {
			continue
		}
		normalizedCooldowns[cooldownKey(entry.Provider, entry.AuthID, entry.Model)] = persistedCooldownFromRuntime(entry)
	}
	state.Cooldowns = normalizedCooldowns
	for _, values := range []map[string]*usageAggregate{
		state.AuthTotals,
		state.ProjectTotals,
		state.ProviderTotals,
	} {
		for _, aggregate := range values {
			if aggregate != nil {
				ensureUsageAggregateMaps(aggregate)
			}
		}
	}
	for _, aggregate := range state.ModelTotals {
		if aggregate != nil {
			ensureUsageAggregateMaps(&aggregate.Usage)
		}
	}
	for _, aggregate := range state.ProjectSubscriptionModelTotals {
		if aggregate != nil {
			ensureUsageAggregateMaps(&aggregate.Usage)
		}
	}
}

func migrateUsageAggregateV1(aggregate *usageAggregate) {
	if aggregate == nil {
		return
	}
	ensureUsageAggregateMaps(aggregate)
	for key, value := range aggregate.Hourly {
		at, errParse := time.Parse(time.RFC3339, key)
		if errParse != nil {
			continue
		}
		dayKey := at.UTC().Format(dailyUsageBucketLayout)
		aggregate.Daily[dayKey] = mergeUsageCounters(aggregate.Daily[dayKey], value)
	}
}

func ensureUsageAggregateMaps(aggregate *usageAggregate) {
	if aggregate.Hourly == nil {
		aggregate.Hourly = make(map[string]usageCounters)
	}
	if aggregate.Daily == nil {
		aggregate.Daily = make(map[string]usageCounters)
	}
}

func mergeUsageAggregate(target *usageAggregate, source *usageAggregate) {
	if target == nil || source == nil {
		return
	}
	ensureUsageAggregateMaps(target)
	target.Total = mergeUsageCounters(target.Total, source.Total)
	for key, value := range source.Hourly {
		target.Hourly[key] = mergeUsageCounters(target.Hourly[key], value)
	}
	for key, value := range source.Daily {
		target.Daily[key] = mergeUsageCounters(target.Daily[key], value)
	}
}

func prunePersistedUsageState(state *persistedUsageState, reference time.Time) {
	if state == nil {
		return
	}
	pruneUsageAggregate(&state.GlobalTotal, reference)
	for _, values := range []map[string]*usageAggregate{
		state.AuthTotals,
		state.ProjectTotals,
		state.ProviderTotals,
	} {
		for _, aggregate := range values {
			pruneUsageAggregate(aggregate, reference)
		}
	}
	for _, aggregate := range state.ModelTotals {
		if aggregate != nil {
			pruneUsageAggregate(&aggregate.Usage, reference)
		}
	}
	for _, aggregate := range state.ProjectSubscriptionModelTotals {
		if aggregate != nil {
			pruneUsageAggregate(&aggregate.Usage, reference)
		}
	}
	for key, entry := range state.Cooldowns {
		if entry == nil || !entry.Until.After(reference) {
			delete(state.Cooldowns, key)
		}
	}
}

func usageCountersEmpty(value usageCounters) bool {
	return value == (usageCounters{})
}

func (store *usageStateStore) scheduleSaveLocked() {
	if store.path == "" {
		return
	}
	if store.saveTimer != nil {
		store.saveTimer.Stop()
	}
	store.saveTimer = time.AfterFunc(usageSaveDebounce, func() {
		_ = store.flush()
	})
}

func (store *usageStateStore) flush() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saveTimer != nil {
		store.saveTimer.Stop()
		store.saveTimer = nil
	}
	return store.saveLocked()
}

func (store *usageStateStore) saveLocked() error {
	if store.path == "" {
		return nil
	}
	now := time.Now().UTC()
	normalizePersistedUsageState(&store.state)
	prunePersistedUsageState(&store.state, now)
	store.state.UpdatedAt = now
	raw, errMarshal := json.MarshalIndent(store.state, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode state snapshot: %w", errMarshal)
	}
	dir := filepath.Dir(store.path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state directory: %w", errMkdir)
	}
	temp, errCreate := os.CreateTemp(dir, ".bravo-state-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create temporary state snapshot: %w", errCreate)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary state snapshot: %w", errChmod)
	}
	if _, errWrite := temp.Write(raw); errWrite != nil {
		_ = temp.Close()
		return fmt.Errorf("write state snapshot: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return fmt.Errorf("sync state snapshot: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close state snapshot: %w", errClose)
	}
	if errRename := os.Rename(tempName, store.path); errRename != nil {
		return fmt.Errorf("replace state snapshot: %w", errRename)
	}
	if errChmod := os.Chmod(store.path, 0o600); errChmod != nil {
		return fmt.Errorf("chmod state snapshot: %w", errChmod)
	}
	return nil
}

func flushUsageState() {
	_ = bravoUsageState.flush()
}

func persistRuntimeCooldown(entry cooldownEntry, generation uint64) {
	persisted := persistedCooldownFromRuntime(entry)
	if persisted == nil {
		return
	}
	key := cooldownKey(entry.Provider, entry.AuthID, entry.Model)
	bravoUsageState.mu.Lock()
	// The plugin is configured before routing. Keeping tests and an
	// unconfigured process memory-only avoids manufacturing a snapshot at an
	// implicit path.
	if bravoUsageState.path == "" {
		bravoUsageState.mu.Unlock()
		return
	}
	if bravoUsageState.generation.Load() != generation {
		bravoUsageState.mu.Unlock()
		removeRuntimeCooldownIfCurrent(key, entry)
		return
	}
	if bravoUsageState.state.Cooldowns == nil {
		bravoUsageState.state.Cooldowns = make(map[string]*persistedCooldownEntry)
	}
	if bravoUsageState.state.SchemaVersion == 0 {
		bravoUsageState.state.SchemaVersion = usageStateSchemaVersion
	}
	bravoUsageState.state.Cooldowns[key] = persisted
	// Cooldowns are a routing safety barrier, not eventually-consistent
	// analytics. A container may restart as soon as the fallback response
	// completes, so finish the existing atomic temp+fsync+rename write before
	// returning to the request.
	if errSave := bravoUsageState.saveLocked(); errSave != nil {
		// Keep the in-memory barrier and retry on the ordinary debounce rather
		// than deleting it after a transient filesystem error.
		bravoUsageState.scheduleSaveLocked()
	}
	// Reassert while holding store.mu. A path switch therefore either happens
	// before the generation check above (and rejects this setter) or after this
	// merge (and replaces it); it cannot land between the check and reassert.
	restoreRuntimeCooldowns(
		map[string]*persistedCooldownEntry{key: persisted},
		time.Now().UTC(),
		false,
	)
	bravoUsageState.mu.Unlock()
}

func removePersistedCooldown(entry cooldownEntry) {
	key := cooldownKey(entry.Provider, entry.AuthID, entry.Model)
	expected := persistedCooldownFromRuntime(entry)
	if expected == nil {
		return
	}
	expectedRuntime, okExpected := runtimeCooldownFromPersisted(expected)
	if !okExpected {
		return
	}
	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	if bravoUsageState.path == "" || bravoUsageState.state.Cooldowns == nil {
		return
	}
	currentPersisted, ok := bravoUsageState.state.Cooldowns[key]
	if !ok {
		return
	}
	currentRuntime, okCurrent := runtimeCooldownFromPersisted(currentPersisted)
	if !okCurrent || !sameCooldownInstance(currentRuntime, expectedRuntime) {
		return
	}
	delete(bravoUsageState.state.Cooldowns, key)
	bravoUsageState.scheduleSaveLocked()
}

func persistedCooldownFromRuntime(entry cooldownEntry) *persistedCooldownEntry {
	entry, ok := sanitizeCooldownEntry(entry)
	if !ok {
		return nil
	}
	persisted := &persistedCooldownEntry{
		Until:      entry.Until.UTC(),
		ObservedAt: entry.ObservedAt.UTC(),
		Reason:     entry.Reason,
		Provider:   entry.Provider,
		AuthID:     entry.AuthID,
		Model:      entry.Model,
	}
	if entry.ProviderError != (providererror.Detail{}) {
		detail := entry.ProviderError
		persisted.ProviderError = &detail
	}
	return persisted
}

func runtimeCooldownFromPersisted(persisted *persistedCooldownEntry) (cooldownEntry, bool) {
	if persisted == nil {
		return cooldownEntry{}, false
	}
	entry := cooldownEntry{
		Until:      persisted.Until,
		ObservedAt: persisted.ObservedAt,
		Reason:     persisted.Reason,
		Provider:   persisted.Provider,
		AuthID:     persisted.AuthID,
		Model:      persisted.Model,
	}
	if persisted.ProviderError != nil {
		entry.ProviderError = *persisted.ProviderError
	}
	return sanitizeCooldownEntry(entry)
}

func sanitizeCooldownEntry(entry cooldownEntry) (cooldownEntry, bool) {
	entry.Provider = normalizeProvider(entry.Provider)
	entry.AuthID = strings.TrimSpace(entry.AuthID)
	entry.Model = baseModelKey(strings.TrimSpace(entry.Model))
	entry.Reason = providererror.Sanitize(providererror.Detail{Reason: entry.Reason}).Reason
	entry.ProviderError = providererror.Sanitize(entry.ProviderError)
	if entry.ProviderError != (providererror.Detail{}) && entry.Model != "" {
		entry.ProviderError.Model = entry.Model
		if entry.ProviderError.Scope == "" {
			entry.ProviderError.Scope = "model"
		}
	} else if entry.ProviderError != (providererror.Detail{}) &&
		strings.EqualFold(entry.ProviderError.Scope, "model") {
		entry.ProviderError.Scope = ""
	}
	if entry.Provider == "" || entry.AuthID == "" || entry.Until.IsZero() {
		return cooldownEntry{}, false
	}
	return entry, true
}

func restoreRuntimeCooldowns(values map[string]*persistedCooldownEntry, now time.Time, replace bool) {
	restored := make(map[string]cooldownEntry, len(values))
	for _, persisted := range values {
		entry, ok := runtimeCooldownFromPersisted(persisted)
		if !ok || !entry.Until.After(now) {
			continue
		}
		restored[cooldownKey(entry.Provider, entry.AuthID, entry.Model)] = entry
	}
	runtimeState.Lock()
	if replace || runtimeState.Cooldowns == nil {
		runtimeState.Cooldowns = restored
		runtimeState.Unlock()
		return
	}
	for key, current := range runtimeState.Cooldowns {
		if !current.Until.After(now) {
			delete(runtimeState.Cooldowns, key)
		}
	}
	for key, entry := range restored {
		current, exists := runtimeState.Cooldowns[key]
		if !exists || entry.Until.After(current.Until) {
			runtimeState.Cooldowns[key] = entry
		}
	}
	runtimeState.Unlock()
}

func handleUsageRecord(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	bravoUsageState.record(record)
	return okEnvelope(map[string]any{})
}

func (store *usageStateStore) record(record pluginapi.UsageRecord) {
	authIndex := strings.TrimSpace(record.AuthIndex)
	projectID := projectIDFromUsagePrincipal(record.APIKey)
	if authIndex == "" && projectID == "" {
		return
	}
	provider := normalizeProvider(firstNonEmpty(record.Provider, record.AuthType, record.ExecutorType))
	model := strings.TrimSpace(record.Model)
	logicalModel := strings.TrimSpace(record.Alias)
	if logicalModel == model {
		logicalModel = ""
	}
	at := record.RequestedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tokens := record.Detail.TotalTokens
	if tokens <= 0 {
		tokens = record.Detail.InputTokens + record.Detail.OutputTokens + record.Detail.ReasoningTokens
	}
	counter := usageCounters{
		Requests:            1,
		InputTokens:         maxInt64(record.Detail.InputTokens, 0),
		OutputTokens:        maxInt64(record.Detail.OutputTokens, 0),
		ReasoningTokens:     maxInt64(record.Detail.ReasoningTokens, 0),
		CachedTokens:        maxInt64(record.Detail.CachedTokens, 0),
		CacheReadTokens:     maxInt64(record.Detail.CacheReadTokens, 0),
		CacheCreationTokens: maxInt64(record.Detail.CacheCreationTokens, 0),
		TotalTokens:         maxInt64(tokens, 0),
		LatencyMS:           maxInt64(record.Latency.Milliseconds(), 0),
	}
	if record.TTFT > 0 {
		counter.TTFTMS = record.TTFT.Milliseconds()
		counter.TTFTSamples = 1
	}
	if record.Failed {
		counter.Failures = 1
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state.AuthTotals == nil ||
		store.state.ProjectTotals == nil ||
		store.state.ProviderTotals == nil ||
		store.state.ModelTotals == nil ||
		store.state.ProjectSubscriptionModelTotals == nil ||
		store.state.Quotas == nil {
		normalizePersistedUsageState(&store.state)
	}
	if store.state.SchemaVersion == 0 {
		store.state.SchemaVersion = usageStateSchemaVersion
	}
	if store.state.DimensionalStartedAt.IsZero() {
		store.state.DimensionalStartedAt = at
	}
	addUsageCounter(&store.state.GlobalTotal, at, counter)
	if authIndex != "" {
		addUsageCounter(ensureUsageAggregate(store.state.AuthTotals, authIndex), at, counter)
		quota := store.state.Quotas[authIndex]
		if quota == nil {
			quota = &credentialQuotaState{Status: "unknown", Confidence: "unknown"}
			store.state.Quotas[authIndex] = quota
		}
		quota.Dirty = true
	}
	if projectID != "" {
		addUsageCounter(ensureUsageAggregate(store.state.ProjectTotals, projectID), at, counter)
	}
	if provider != "" {
		addUsageCounter(ensureUsageAggregate(store.state.ProviderTotals, provider), at, counter)
	}
	dimensions := projectSubscriptionModelUsageAggregate{
		ProjectID:    projectID,
		AuthIndex:    authIndex,
		Provider:     provider,
		Model:        model,
		LogicalModel: logicalModel,
	}
	if model != "" || logicalModel != "" {
		modelKey := usageDimensionKey("", "", provider, model, logicalModel)
		modelAggregate := store.state.ModelTotals[modelKey]
		if modelAggregate == nil {
			modelAggregate = &modelUsageAggregate{
				Provider:     provider,
				Model:        model,
				LogicalModel: logicalModel,
				Usage:        newUsageAggregate(),
			}
			store.state.ModelTotals[modelKey] = modelAggregate
		}
		addUsageCounter(&modelAggregate.Usage, at, counter)
	}
	crossKey := usageDimensionKey(projectID, authIndex, provider, model, logicalModel)
	crossAggregate := store.state.ProjectSubscriptionModelTotals[crossKey]
	if crossAggregate == nil {
		dimensions.Usage = newUsageAggregate()
		crossAggregate = &dimensions
		store.state.ProjectSubscriptionModelTotals[crossKey] = crossAggregate
	}
	addUsageCounter(&crossAggregate.Usage, at, counter)
	store.scheduleSaveLocked()
}

func usageDimensionKey(projectID, authIndex, provider, model, logicalModel string) string {
	raw := strings.Join([]string{
		usageDimensionKeyVersion,
		strings.TrimSpace(projectID),
		strings.TrimSpace(authIndex),
		normalizeProvider(provider),
		strings.TrimSpace(model),
		strings.TrimSpace(logicalModel),
	}, "\x1f")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum[:16])
}

func projectIDFromUsagePrincipal(principal string) string {
	principal = strings.TrimSpace(principal)
	if !strings.HasPrefix(principal, "bravo:") {
		return ""
	}
	projectID := strings.TrimSpace(strings.TrimPrefix(principal, "bravo:"))
	if !validProjectID(projectID) {
		return ""
	}
	return projectID
}

func ensureUsageAggregate(values map[string]*usageAggregate, key string) *usageAggregate {
	aggregate := values[key]
	if aggregate == nil {
		value := newUsageAggregate()
		aggregate = &value
		values[key] = aggregate
	}
	ensureUsageAggregateMaps(aggregate)
	return aggregate
}

func addUsageCounter(aggregate *usageAggregate, at time.Time, value usageCounters) {
	ensureUsageAggregateMaps(aggregate)
	aggregate.Total = mergeUsageCounters(aggregate.Total, value)
	at = at.UTC()
	hourKey := at.Truncate(time.Hour).Format(time.RFC3339)
	aggregate.Hourly[hourKey] = mergeUsageCounters(aggregate.Hourly[hourKey], value)
	dayKey := at.Format(dailyUsageBucketLayout)
	aggregate.Daily[dayKey] = mergeUsageCounters(aggregate.Daily[dayKey], value)
	pruneUsageAggregate(aggregate, at)
}

func pruneUsageAggregate(aggregate *usageAggregate, reference time.Time) {
	if aggregate == nil {
		return
	}
	ensureUsageAggregateMaps(aggregate)
	reference = reference.UTC()
	hourlyPruneBefore := reference.Add(-hourlyUsageRetention).Truncate(time.Hour)
	for key := range aggregate.Hourly {
		hour, errParse := time.Parse(time.RFC3339, key)
		if errParse != nil || hour.Before(hourlyPruneBefore) {
			delete(aggregate.Hourly, key)
		}
	}
	dailyPruneBefore := reference.Truncate(24 * time.Hour).Add(-dailyUsageRetention)
	for key := range aggregate.Daily {
		day, errParse := time.Parse(dailyUsageBucketLayout, key)
		if errParse != nil || day.Before(dailyPruneBefore) {
			delete(aggregate.Daily, key)
		}
	}
}

func mergeUsageCounters(left, right usageCounters) usageCounters {
	left.Requests += right.Requests
	left.InputTokens += right.InputTokens
	left.OutputTokens += right.OutputTokens
	left.ReasoningTokens += right.ReasoningTokens
	left.CachedTokens += right.CachedTokens
	left.CacheReadTokens += right.CacheReadTokens
	left.CacheCreationTokens += right.CacheCreationTokens
	left.TotalTokens += right.TotalTokens
	left.Failures += right.Failures
	left.LatencyMS += right.LatencyMS
	left.TTFTMS += right.TTFTMS
	left.TTFTSamples += right.TTFTSamples
	return left
}

func rollingUsage(aggregate *usageAggregate, now time.Time, window time.Duration) usageCounters {
	if aggregate == nil {
		return usageCounters{}
	}
	cutoff := now.UTC().Add(-window)
	var out usageCounters
	for key, value := range aggregate.Hourly {
		hour, errParse := time.Parse(time.RFC3339, key)
		if errParse == nil && !hour.Before(cutoff.Truncate(time.Hour)) {
			out = mergeUsageCounters(out, value)
		}
	}
	return out
}

type usageSummaryView struct {
	Total            usageCounters `json:"total"`
	Session          usageCounters `json:"session"`
	Weekly           usageCounters `json:"weekly"`
	AverageLatencyMS float64       `json:"average_latency_ms"`
	AverageTTFTMS    float64       `json:"average_ttft_ms,omitempty"`
}

func usageSummary(aggregate *usageAggregate, now time.Time) usageSummaryView {
	view := usageSummaryView{}
	if aggregate == nil {
		return view
	}
	view.Total = aggregate.Total
	view.Session = rollingUsage(aggregate, now, sessionUsageWindow)
	view.Weekly = rollingUsage(aggregate, now, weeklyUsageWindow)
	if view.Total.Requests > 0 {
		view.AverageLatencyMS = float64(view.Total.LatencyMS) / float64(view.Total.Requests)
	}
	if view.Total.TTFTSamples > 0 {
		view.AverageTTFTMS = float64(view.Total.TTFTMS) / float64(view.Total.TTFTSamples)
	}
	return view
}

func projectUsageSummary(projectID string, now time.Time) usageSummaryView {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	return usageSummary(bravoUsageState.state.ProjectTotals[strings.TrimSpace(projectID)], now)
}

func authUsageSummary(authIndex string, now time.Time) usageSummaryView {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	return usageSummary(bravoUsageState.state.AuthTotals[strings.TrimSpace(authIndex)], now)
}

func quotaSnapshot(authIndex string) credentialQuotaState {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	quota := bravoUsageState.state.Quotas[strings.TrimSpace(authIndex)]
	if quota == nil {
		return credentialQuotaState{Status: "unknown", Confidence: "unknown"}
	}
	return *quota
}

func storeQuotaSnapshot(authIndex string, quota credentialQuotaState) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state.Quotas[authIndex] = &quota
	bravoUsageState.scheduleSaveLocked()
	bravoUsageState.mu.Unlock()
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
