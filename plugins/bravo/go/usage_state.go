package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	usageStateSchemaVersion  = 4
	usageSaveDebounce        = 250 * time.Millisecond
	usageSaveMaximumDelay    = 30 * time.Second
	sessionUsageWindow       = 5 * time.Hour
	weeklyUsageWindow        = 7 * 24 * time.Hour
	hourlyUsageRetention     = 31 * 24 * time.Hour
	dailyUsageRetention      = 400 * 24 * time.Hour
	dailyUsageBucketLayout   = "2006-01-02"
	usageDimensionKeyVersion = "v1"

	// Pending/Prepared are cumulative work ledgers, not provider percentages.
	// They may legitimately exceed 100 when several accepted requests are still
	// unobserved. Capping them at 100 loses provenance and can make a refresh
	// consume a live Prepared lease. MaxFloat64 is a conservative saturation
	// value for malformed/overflowing input: it blocks admission rather than
	// forgetting debt.
	adaptiveMaximumPersistedLedgerPercent  = math.MaxFloat64
	adaptiveMaximumPersistedPendingPercent = 100.0 // estimator exposure only
	adaptiveMaximumPersistedBurnPerMin     = 100.0
	adaptiveMaximumPersistedAuthRecords    = 4096
	adaptiveMaximumPersistedProfiles       = 4096
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

// persistedAdaptivePendingState is a conservative restart watermark. It is
// deliberately aggregate-only: request bodies, prompt features and attempt
// identifiers never enter the durable state file.
type persistedAdaptivePendingState struct {
	Percent   float64   `json:"percent"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type persistedAdaptiveWindowState struct {
	LearnedScale       float64 `json:"learned_scale,omitempty"`
	ObservedBurnPerMin float64 `json:"observed_burn_per_minute,omitempty"`
}

type persistedAdaptiveAggregateState struct {
	LearnedScale       float64   `json:"learned_scale,omitempty"`
	ObservedBurnPerMin float64   `json:"observed_burn_per_minute,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

type persistedAdaptiveProfileState struct {
	AuthIndex         string                       `json:"auth_index"`
	Provider          string                       `json:"provider,omitempty"`
	PhysicalModel     string                       `json:"physical_model,omitempty"`
	ModelFamily       string                       `json:"model_family"`
	EffortBucket      string                       `json:"effort_bucket"`
	ContextBucket     string                       `json:"context_bucket"`
	CostMode          string                       `json:"cost_mode,omitempty"`
	Session           persistedAdaptiveWindowState `json:"session,omitempty"`
	Weekly            persistedAdaptiveWindowState `json:"weekly,omitempty"`
	UnobservedPercent float64                      `json:"unobserved_percent,omitempty"`
	UpdatedAt         time.Time                    `json:"updated_at,omitempty"`
}

// persistedAdaptiveQuotaState is additive to the v3 JSON schema so an older
// binary can still decode the snapshot. Profiles are stored separately from
// auth-level pending watermarks because estimator buckets may evolve without
// changing the restart gate.
type persistedAdaptiveQuotaState struct {
	Pending           map[string]*persistedAdaptivePendingState   `json:"pending,omitempty"`
	Prepared          map[string]*persistedAdaptivePendingState   `json:"prepared,omitempty"`
	Aggregates        map[string]*persistedAdaptiveAggregateState `json:"aggregates,omitempty"`
	Profiles          map[string]*persistedAdaptiveProfileState   `json:"profiles,omitempty"`
	Revisions         map[string]uint64                           `json:"revisions,omitempty"`
	Saturated         bool                                        `json:"saturated,omitempty"`
	OverflowAuthCount int                                         `json:"overflow_auth_count,omitempty"`
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
	AdaptiveQuota                  persistedAdaptiveQuotaState                        `json:"adaptive_quota,omitempty"`
	DimensionalStartedAt           time.Time                                          `json:"dimensional_started_at,omitempty"`
	UpdatedAt                      time.Time                                          `json:"updated_at,omitempty"`
}

type usageStateStore struct {
	mu               sync.RWMutex
	path             string
	state            persistedUsageState
	saveTimer        *time.Timer
	savePendingSince time.Time
	generation       atomic.Uint64
}

var bravoUsageState = &usageStateStore{}
var adaptiveRoutingSaturated atomic.Bool
var adaptiveAdmissionMu sync.RWMutex
var usageSnapshotIOMu sync.Mutex
var usageSnapshotSequence atomic.Uint64
var usageSnapshotWritten = make(map[string]uint64) // guarded by usageSnapshotIOMu
var usageSnapshotIOHook func()

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
		AdaptiveQuota: persistedAdaptiveQuotaState{
			Pending:    make(map[string]*persistedAdaptivePendingState),
			Prepared:   make(map[string]*persistedAdaptivePendingState),
			Aggregates: make(map[string]*persistedAdaptiveAggregateState),
			Profiles:   make(map[string]*persistedAdaptiveProfileState),
			Revisions:  make(map[string]uint64),
		},
	}
}

func clonePersistedUsageState(source persistedUsageState) persistedUsageState {
	cloned := source
	cloned.GlobalTotal = cloneUsageAggregate(source.GlobalTotal)
	cloned.AuthTotals = cloneUsageAggregateMap(source.AuthTotals)
	cloned.ProjectTotals = cloneUsageAggregateMap(source.ProjectTotals)
	cloned.ProviderTotals = cloneUsageAggregateMap(source.ProviderTotals)
	cloned.ModelTotals = make(map[string]*modelUsageAggregate, len(source.ModelTotals))
	for key, value := range source.ModelTotals {
		if value == nil {
			continue
		}
		copyValue := *value
		copyValue.Usage = cloneUsageAggregate(value.Usage)
		cloned.ModelTotals[key] = &copyValue
	}
	cloned.ProjectSubscriptionModelTotals = make(map[string]*projectSubscriptionModelUsageAggregate, len(source.ProjectSubscriptionModelTotals))
	for key, value := range source.ProjectSubscriptionModelTotals {
		if value == nil {
			continue
		}
		copyValue := *value
		copyValue.Usage = cloneUsageAggregate(value.Usage)
		cloned.ProjectSubscriptionModelTotals[key] = &copyValue
	}
	cloned.Quotas = make(map[string]*credentialQuotaState, len(source.Quotas))
	for key, value := range source.Quotas {
		if value == nil {
			continue
		}
		copyValue := *value
		copyValue.ModelWeekly = append([]modelQuotaWindowState(nil), value.ModelWeekly...)
		copyValue.UsageRefresh = cloneQuotaRefreshState(value.UsageRefresh)
		copyValue.ProfileRefresh = cloneQuotaRefreshState(value.ProfileRefresh)
		cloned.Quotas[key] = &copyValue
	}
	cloned.Cooldowns = make(map[string]*persistedCooldownEntry, len(source.Cooldowns))
	for key, value := range source.Cooldowns {
		if value == nil {
			continue
		}
		copyValue := *value
		if value.ProviderError != nil {
			copyError := *value.ProviderError
			copyValue.ProviderError = &copyError
		}
		cloned.Cooldowns[key] = &copyValue
	}
	cloned.AdaptiveQuota = clonePersistedAdaptiveQuotaState(source.AdaptiveQuota)
	return cloned
}

func cloneUsageAggregate(source usageAggregate) usageAggregate {
	cloned := source
	cloned.Hourly = make(map[string]usageCounters, len(source.Hourly))
	for key, value := range source.Hourly {
		cloned.Hourly[key] = value
	}
	cloned.Daily = make(map[string]usageCounters, len(source.Daily))
	for key, value := range source.Daily {
		cloned.Daily[key] = value
	}
	return cloned
}

func cloneUsageAggregateMap(source map[string]*usageAggregate) map[string]*usageAggregate {
	cloned := make(map[string]*usageAggregate, len(source))
	for key, value := range source {
		if value == nil {
			continue
		}
		copyValue := cloneUsageAggregate(*value)
		cloned[key] = &copyValue
	}
	return cloned
}

func cloneQuotaRefreshState(source quotaRefreshState) quotaRefreshState {
	cloned := source
	if source.Error != nil {
		copyError := *source.Error
		cloned.Error = &copyError
	}
	return cloned
}

func clonePersistedAdaptiveQuotaState(source persistedAdaptiveQuotaState) persistedAdaptiveQuotaState {
	cloned := source
	cloned.Pending = clonePersistedAdaptivePendingMap(source.Pending)
	cloned.Prepared = clonePersistedAdaptivePendingMap(source.Prepared)
	cloned.Aggregates = make(map[string]*persistedAdaptiveAggregateState, len(source.Aggregates))
	for key, value := range source.Aggregates {
		if value != nil {
			copyValue := *value
			cloned.Aggregates[key] = &copyValue
		}
	}
	cloned.Profiles = make(map[string]*persistedAdaptiveProfileState, len(source.Profiles))
	for key, value := range source.Profiles {
		if value != nil {
			copyValue := *value
			cloned.Profiles[key] = &copyValue
		}
	}
	cloned.Revisions = make(map[string]uint64, len(source.Revisions))
	for key, value := range source.Revisions {
		cloned.Revisions[key] = value
	}
	return cloned
}

func clonePersistedAdaptivePendingMap(source map[string]*persistedAdaptivePendingState) map[string]*persistedAdaptivePendingState {
	cloned := make(map[string]*persistedAdaptivePendingState, len(source))
	for key, value := range source {
		if value != nil {
			copyValue := *value
			cloned[key] = &copyValue
		}
	}
	return cloned
}

func newUsageAggregate() usageAggregate {
	return usageAggregate{
		Hourly: make(map[string]usageCounters),
		Daily:  make(map[string]usageCounters),
	}
}

func configureUsageState(path string) error {
	adaptiveAdmissionMu.Lock()
	defer adaptiveAdmissionMu.Unlock()
	// A previous generation may still have async absolute finalizations queued.
	// Drain them before replay/healing so truncation cannot race a late append.
	if errBarrier := adaptiveWALRuntime.barrier(); errBarrier != nil {
		return fmt.Errorf("drain adaptive quota WAL before configure: %w", errBarrier)
	}
	return bravoUsageState.configure(path)
}

var configureUsageStateStoreLockedHook func()
var configureUsageStateBeforeRuntimeRestoreHook func()

func (store *usageStateStore) configure(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultStatePath
	}
	store.mu.Lock()
	if store.saveTimer != nil {
		store.saveTimer.Stop()
		store.saveTimer = nil
	}
	if store.path != "" {
		if errFlush := store.saveLocked(); errFlush != nil {
			store.mu.Unlock()
			return errFlush
		}
		if store.path == path {
			cooldowns := clonePersistedCooldowns(store.state.Cooldowns)
			store.mu.Unlock()
			restoreRuntimeCooldowns(cooldowns, time.Now().UTC(), false)
			return nil
		}
		if adaptiveRoutingLedgerUnresolved(store.state.AdaptiveQuota) {
			store.mu.Unlock()
			return fmt.Errorf("cannot switch usage state path while adaptive quota work is unresolved")
		}
	}
	state, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		store.mu.Unlock()
		return errLoad
	}
	if errReplay := replayAdaptiveWALFile(adaptiveWALPath(path), &state); errReplay != nil {
		store.mu.Unlock()
		return errReplay
	}
	overCapacity, errCapacity := adaptiveWALRuntime.overCapacity(adaptiveWALPath(path))
	if errCapacity != nil {
		store.mu.Unlock()
		return fmt.Errorf("inspect adaptive quota WAL capacity: %w", errCapacity)
	}
	if overCapacity {
		// Upgrade/repair path: replay every valid absolute record first, then
		// checkpoint and compact in an unpublished candidate store. A fallible
		// capacity/stat/sync step must not replace the active path while runtime
		// state and generation still belong to the previous store.
		candidate := &usageStateStore{path: path, state: state}
		if errSave := candidate.saveLocked(); errSave != nil {
			store.mu.Unlock()
			return fmt.Errorf("checkpoint oversized adaptive quota WAL: %w", errSave)
		}
		state = candidate.state
	}
	store.path = path
	store.state = state
	store.savePendingSince = time.Time{}
	store.generation.Add(1)
	cooldowns := clonePersistedCooldowns(state.Cooldowns)
	pending := normalizePersistedAdaptivePending(state.AdaptiveQuota.Pending)
	prepared := normalizePersistedAdaptivePending(state.AdaptiveQuota.Prepared)
	aggregates := normalizePersistedAdaptiveAggregates(state.AdaptiveQuota.Aggregates)
	profiles := normalizePersistedAdaptiveProfiles(state.AdaptiveQuota.Profiles)
	saturated := state.AdaptiveQuota.Saturated
	adaptiveRoutingSaturated.Store(saturated)
	if configureUsageStateStoreLockedHook != nil {
		configureUsageStateStoreLockedHook()
	}
	store.mu.Unlock()
	if configureUsageStateBeforeRuntimeRestoreHook != nil {
		configureUsageStateBeforeRuntimeRestoreHook()
	}
	restoreRuntimeCooldowns(cooldowns, time.Now().UTC(), true)
	restoreAdaptivePendingState(pending, prepared, true)
	restoreAdaptiveEstimatorState(aggregates, profiles, true)
	return nil
}

func adaptiveRoutingLedgerUnresolved(state persistedAdaptiveQuotaState) bool {
	return state.Saturated || len(state.Pending) > 0 || len(state.Prepared) > 0
}

func clonePersistedCooldowns(source map[string]*persistedCooldownEntry) map[string]*persistedCooldownEntry {
	cloned := make(map[string]*persistedCooldownEntry, len(source))
	for key, value := range source {
		if runtimeValue, ok := runtimeCooldownFromPersisted(value); ok {
			cloned[key] = persistedCooldownFromRuntime(runtimeValue)
		}
	}
	return cloned
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
	case 3:
		migrateUsageStateV3(&state)
	case usageStateSchemaVersion:
		normalizePersistedUsageState(&state)
		prunePersistedUsageState(&state, time.Now().UTC())
	default:
		return newPersistedUsageState(), fmt.Errorf("unsupported state schema version %d", state.SchemaVersion)
	}
	return state, nil
}

func migrateUsageStateV3(state *persistedUsageState) {
	if state == nil {
		return
	}
	normalizePersistedUsageState(state)
	prunePersistedUsageState(state, time.Now().UTC())
	state.SchemaVersion = usageStateSchemaVersion
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
	normalizePersistedAdaptiveRoutingLedger(&state.AdaptiveQuota)
	state.AdaptiveQuota.Aggregates = normalizePersistedAdaptiveAggregates(state.AdaptiveQuota.Aggregates)
	state.AdaptiveQuota.Profiles = normalizePersistedAdaptiveProfiles(state.AdaptiveQuota.Profiles)
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

func normalizePersistedAdaptiveRevisions(source map[string]uint64) map[string]uint64 {
	normalized := make(map[string]uint64, len(source))
	for rawAuthIndex, revision := range source {
		authIndex := strings.TrimSpace(rawAuthIndex)
		if authIndex != "" && revision > 0 {
			normalized[authIndex] = revision
		}
	}
	return normalized
}

// normalizePersistedAdaptiveRoutingLedger applies one bound to the union of
// prepared and pending auth identities. An oversized legacy snapshot retains
// an explicit saturation marker, so omitted unresolved work blocks secondary
// routing instead of being silently forgotten.
func normalizePersistedAdaptiveRoutingLedger(state *persistedAdaptiveQuotaState) {
	if state == nil {
		return
	}
	state.Pending = normalizePersistedAdaptivePending(state.Pending)
	state.Prepared = normalizePersistedAdaptivePending(state.Prepared)
	state.Revisions = normalizePersistedAdaptiveRevisions(state.Revisions)

	updatedAt := make(map[string]time.Time, len(state.Pending)+len(state.Prepared))
	for authIndex, entry := range state.Pending {
		updatedAt[authIndex] = entry.UpdatedAt
	}
	for authIndex, entry := range state.Prepared {
		if entry.UpdatedAt.After(updatedAt[authIndex]) {
			updatedAt[authIndex] = entry.UpdatedAt
		}
	}
	keys := make([]string, 0, len(updatedAt))
	for authIndex := range updatedAt {
		keys = append(keys, authIndex)
	}
	sort.Slice(keys, func(i, j int) bool {
		if updatedAt[keys[i]].Equal(updatedAt[keys[j]]) {
			return keys[i] < keys[j]
		}
		return updatedAt[keys[i]].After(updatedAt[keys[j]])
	})
	if len(keys) > adaptiveMaximumPersistedAuthRecords {
		overflow := len(keys) - adaptiveMaximumPersistedAuthRecords
		state.Saturated = true
		if overflow > state.OverflowAuthCount {
			state.OverflowAuthCount = overflow
		}
		for _, authIndex := range keys[adaptiveMaximumPersistedAuthRecords:] {
			delete(state.Pending, authIndex)
			delete(state.Prepared, authIndex)
			delete(state.Revisions, authIndex)
		}
		keys = keys[:adaptiveMaximumPersistedAuthRecords]
	}

	// Unresolved revision watermarks are retained first. Dropping excess
	// resolved watermarks also sets saturation because a stale WAL suffix could
	// otherwise resurrect work after a power loss.
	if len(state.Revisions) > adaptiveMaximumPersistedAuthRecords {
		kept := make(map[string]uint64, adaptiveMaximumPersistedAuthRecords)
		for _, authIndex := range keys {
			kept[authIndex] = state.Revisions[authIndex]
		}
		resolved := make([]string, 0, len(state.Revisions))
		for authIndex := range state.Revisions {
			if _, unresolved := updatedAt[authIndex]; !unresolved {
				resolved = append(resolved, authIndex)
			}
		}
		sort.Slice(resolved, func(i, j int) bool {
			left, right := state.Revisions[resolved[i]], state.Revisions[resolved[j]]
			if left == right {
				return resolved[i] < resolved[j]
			}
			return left > right
		})
		for _, authIndex := range resolved {
			if len(kept) == adaptiveMaximumPersistedAuthRecords {
				break
			}
			kept[authIndex] = state.Revisions[authIndex]
		}
		dropped := len(state.Revisions) - len(kept)
		if dropped > 0 {
			state.Saturated = true
			if dropped > state.OverflowAuthCount {
				state.OverflowAuthCount = dropped
			}
		}
		state.Revisions = kept
	}
	if state.OverflowAuthCount < 0 {
		state.OverflowAuthCount = 0
	}
}

func normalizePersistedAdaptiveAggregates(source map[string]*persistedAdaptiveAggregateState) map[string]*persistedAdaptiveAggregateState {
	normalized := make(map[string]*persistedAdaptiveAggregateState, len(source))
	for rawAuthIndex, entry := range source {
		authIndex := strings.TrimSpace(rawAuthIndex)
		if authIndex == "" || entry == nil {
			continue
		}
		copyEntry := *entry
		copyEntry.LearnedScale = boundedLearnedScale(copyEntry.LearnedScale)
		copyEntry.ObservedBurnPerMin = boundedObservedBurn(copyEntry.ObservedBurnPerMin)
		copyEntry.UpdatedAt = copyEntry.UpdatedAt.UTC()
		normalized[authIndex] = &copyEntry
	}
	return boundAdaptiveRecords(normalized, adaptiveMaximumPersistedAuthRecords, func(value *persistedAdaptiveAggregateState) time.Time {
		return value.UpdatedAt
	})
}

func normalizePersistedAdaptiveProfiles(source map[string]*persistedAdaptiveProfileState) map[string]*persistedAdaptiveProfileState {
	normalized := make(map[string]*persistedAdaptiveProfileState, len(source))
	for _, entry := range source {
		if entry == nil {
			continue
		}
		copyEntry := *entry
		copyEntry.AuthIndex = strings.TrimSpace(copyEntry.AuthIndex)
		copyEntry.Provider = boundedAdaptiveLabel(copyEntry.Provider)
		copyEntry.PhysicalModel = boundedAdaptiveLabel(copyEntry.PhysicalModel)
		copyEntry.ModelFamily = boundedAdaptiveLabel(copyEntry.ModelFamily)
		copyEntry.EffortBucket = boundedAdaptiveLabel(copyEntry.EffortBucket)
		copyEntry.ContextBucket = boundedAdaptiveLabel(copyEntry.ContextBucket)
		copyEntry.CostMode = boundedAdaptiveLabel(copyEntry.CostMode)
		if copyEntry.AuthIndex == "" || copyEntry.ModelFamily == "" || copyEntry.EffortBucket == "" || copyEntry.ContextBucket == "" {
			continue
		}
		if copyEntry.Provider == "" {
			copyEntry.Provider = "legacy-unknown"
		}
		if copyEntry.PhysicalModel == "" {
			copyEntry.PhysicalModel = "legacy-unknown"
		}
		if copyEntry.CostMode == "" {
			copyEntry.CostMode = "legacy-unknown"
		}
		copyEntry.Session = normalizePersistedAdaptiveWindow(copyEntry.Session)
		copyEntry.Weekly = normalizePersistedAdaptiveWindow(copyEntry.Weekly)
		copyEntry.UnobservedPercent = boundedPercent(copyEntry.UnobservedPercent)
		copyEntry.UpdatedAt = copyEntry.UpdatedAt.UTC()
		key := adaptiveProfileKey(copyEntry.AuthIndex, adaptiveRequestShape{
			Provider: copyEntry.Provider, PhysicalModel: copyEntry.PhysicalModel, ModelFamily: copyEntry.ModelFamily,
			EffortBucket: copyEntry.EffortBucket, ContextBucket: copyEntry.ContextBucket, CostMode: copyEntry.CostMode,
		})
		normalized[key] = &copyEntry
	}
	return boundAdaptiveRecords(normalized, adaptiveMaximumPersistedProfiles, func(value *persistedAdaptiveProfileState) time.Time {
		return value.UpdatedAt
	})
}

func normalizePersistedAdaptiveWindow(value persistedAdaptiveWindowState) persistedAdaptiveWindowState {
	value.LearnedScale = boundedLearnedScale(value.LearnedScale)
	value.ObservedBurnPerMin = boundedObservedBurn(value.ObservedBurnPerMin)
	return value
}

func boundedAdaptiveLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

func boundedLearnedScale(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 1
	}
	return math.Min(math.Max(value, 1), adaptiveMaximumLearnedScale)
}

func boundedObservedBurn(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	return math.Min(value, adaptiveMaximumPersistedBurnPerMin)
}

func boundedPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	return math.Min(value, adaptiveMaximumPersistedPendingPercent)
}

func boundAdaptiveRecords[T any](source map[string]*T, maximum int, updatedAt func(*T) time.Time) map[string]*T {
	if len(source) <= maximum {
		return source
	}
	type candidate struct {
		key   string
		value *T
	}
	values := make([]candidate, 0, len(source))
	for key, value := range source {
		values = append(values, candidate{key: key, value: value})
	}
	sort.Slice(values, func(i, j int) bool {
		left, right := updatedAt(values[i].value), updatedAt(values[j].value)
		if left.Equal(right) {
			return values[i].key < values[j].key
		}
		return left.After(right)
	})
	bounded := make(map[string]*T, maximum)
	for _, item := range values[:maximum] {
		bounded[item.key] = item.value
	}
	return bounded
}

func normalizePersistedAdaptivePending(source map[string]*persistedAdaptivePendingState) map[string]*persistedAdaptivePendingState {
	normalized := make(map[string]*persistedAdaptivePendingState, len(source))
	for rawAuthIndex, entry := range source {
		authIndex := strings.TrimSpace(rawAuthIndex)
		if authIndex == "" || entry == nil || math.IsNaN(entry.Percent) || math.IsInf(entry.Percent, 0) || entry.Percent <= 0 {
			continue
		}
		copyEntry := *entry
		copyEntry.Percent = math.Min(copyEntry.Percent, adaptiveMaximumPersistedLedgerPercent)
		copyEntry.UpdatedAt = copyEntry.UpdatedAt.UTC()
		normalized[authIndex] = &copyEntry
	}
	return normalized
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
	now := time.Now()
	if store.savePendingSince.IsZero() {
		store.savePendingSince = now
	}
	delay := usageSaveDebounce
	if remaining := usageSaveMaximumDelay - now.Sub(store.savePendingSince); remaining < delay {
		delay = remaining
	}
	if delay < 0 {
		delay = 0
	}
	if store.saveTimer != nil {
		store.saveTimer.Stop()
	}
	store.saveTimer = time.AfterFunc(delay, func() {
		_ = store.flush()
	})
}

func (store *usageStateStore) flush() error {
	store.mu.Lock()
	if store.saveTimer != nil {
		store.saveTimer.Stop()
		store.saveTimer = nil
	}
	if store.path == "" {
		store.mu.Unlock()
		return nil
	}
	now := time.Now().UTC()
	normalizePersistedUsageState(&store.state)
	prunePersistedUsageState(&store.state, now)
	store.state.UpdatedAt = now
	snapshot := clonePersistedUsageState(store.state)
	path := store.path
	generation := store.generation.Load()
	sequence := usageSnapshotSequence.Add(1)
	store.mu.Unlock()

	errSave := persistUsageSnapshot(path, snapshot, sequence, false)
	if errSave == nil {
		store.mu.Lock()
		if store.path == path && store.generation.Load() == generation {
			store.savePendingSince = time.Time{}
		}
		store.mu.Unlock()
	}
	return errSave
}

func (store *usageStateStore) saveLocked() error {
	if store.path == "" {
		return nil
	}
	now := time.Now().UTC()
	normalizePersistedUsageState(&store.state)
	prunePersistedUsageState(&store.state, now)
	store.state.UpdatedAt = now
	snapshot := clonePersistedUsageState(store.state)
	sequence := usageSnapshotSequence.Add(1)
	if errSave := persistUsageSnapshot(store.path, snapshot, sequence, true); errSave != nil {
		return errSave
	}
	store.savePendingSince = time.Time{}
	return nil
}

func persistUsageSnapshot(path string, state persistedUsageState, sequence uint64, compactAll bool) error {
	raw, errMarshal := json.MarshalIndent(state, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode state snapshot: %w", errMarshal)
	}
	usageSnapshotIOMu.Lock()
	defer usageSnapshotIOMu.Unlock()
	if sequence < usageSnapshotWritten[path] {
		return nil
	}
	if usageSnapshotIOHook != nil {
		usageSnapshotIOHook()
	}
	dir := filepath.Dir(path)
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
	if errRename := os.Rename(tempName, path); errRename != nil {
		return fmt.Errorf("replace state snapshot: %w", errRename)
	}
	if errChmod := os.Chmod(path, 0o600); errChmod != nil {
		return fmt.Errorf("chmod state snapshot: %w", errChmod)
	}
	// The renamed snapshot must be power-loss durable before its WAL is
	// removed; otherwise a crash could lose both the old snapshot and ledger.
	if errSyncDir := adaptiveSyncDirectory(dir); errSyncDir != nil {
		return fmt.Errorf("sync state snapshot directory: %w", errSyncDir)
	}
	walPath := adaptiveWALPath(path)
	if compactAll {
		if errCompact := adaptiveWALRuntime.compact(walPath); errCompact != nil {
			return fmt.Errorf("compact adaptive WAL: %w", errCompact)
		}
	} else if errCompact := adaptiveWALRuntime.compactThrough(walPath, state.AdaptiveQuota.Revisions); errCompact != nil {
		return fmt.Errorf("compact adaptive WAL checkpoint: %w", errCompact)
	}
	usageSnapshotWritten[path] = sequence
	return nil
}

func scheduleAdaptiveUsageCheckpoint() {
	bravoUsageState.mu.Lock()
	bravoUsageState.scheduleSaveLocked()
	bravoUsageState.mu.Unlock()
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
	// Reconfiguration holds the write side across state publication and runtime
	// replacement. The read side keeps the generation check, persisted merge,
	// and runtime reassertion in one transaction, but is released before any
	// snapshot encoding or filesystem work. Ordinary admissions also take the
	// read side and therefore remain concurrent with this short transaction.
	adaptiveAdmissionMu.RLock()
	bravoUsageState.mu.Lock()
	// The plugin is configured before routing. Keeping tests and an
	// unconfigured process memory-only avoids manufacturing a snapshot at an
	// implicit path.
	if bravoUsageState.path == "" {
		bravoUsageState.mu.Unlock()
		adaptiveAdmissionMu.RUnlock()
		return
	}
	if bravoUsageState.generation.Load() != generation {
		bravoUsageState.mu.Unlock()
		adaptiveAdmissionMu.RUnlock()
		removeRuntimeCooldownIfCurrent(key, entry)
		return
	}
	if bravoUsageState.state.Cooldowns == nil {
		bravoUsageState.state.Cooldowns = make(map[string]*persistedCooldownEntry)
	}
	if bravoUsageState.state.SchemaVersion == 0 {
		bravoUsageState.state.SchemaVersion = usageStateSchemaVersion
	}
	// Runtime is written before this function acquires store.mu. Two setters for
	// the same account/model can therefore complete in the opposite order. Do
	// not let a stale waiter shorten the durable barrier merely because its
	// snapshot receives a later sequence number.
	if current, ok := runtimeCooldownFromPersisted(bravoUsageState.state.Cooldowns[key]); ok &&
		!cooldownEntrySupersedes(entry, current) {
		persisted = persistedCooldownFromRuntime(current)
	} else {
		bravoUsageState.state.Cooldowns[key] = persisted
	}
	now := time.Now().UTC()
	normalizePersistedUsageState(&bravoUsageState.state)
	prunePersistedUsageState(&bravoUsageState.state, now)
	bravoUsageState.state.UpdatedAt = now
	snapshot := clonePersistedUsageState(bravoUsageState.state)
	path := bravoUsageState.path
	sequence := usageSnapshotSequence.Add(1)
	bravoUsageState.mu.Unlock()
	// The initial runtime write in setCooldown happens before persistence. It
	// may have been replaced while waiting for store.mu, so reassert the exact
	// durable entry while reconfiguration is still excluded. Do not reassert
	// after I/O: a confirmed refresh may legitimately clear it meanwhile.
	restoreRuntimeCooldowns(
		map[string]*persistedCooldownEntry{key: persisted},
		now,
		false,
	)
	adaptiveAdmissionMu.RUnlock()

	// Cooldowns are a routing safety barrier, not eventually-consistent
	// analytics. A container may restart as soon as the fallback response
	// completes, so finish the atomic temp+fsync+rename write before returning
	// to the request. Snapshot encoding and filesystem I/O deliberately happen
	// after releasing store.mu: unrelated admissions only need an in-memory
	// quota snapshot and must not queue behind a slow disk checkpoint.
	errSave := persistUsageSnapshot(path, snapshot, sequence, false)
	bravoUsageState.mu.Lock()
	if errSave != nil && bravoUsageState.path == path && bravoUsageState.generation.Load() == generation {
		// Keep the in-memory barrier and retry on the ordinary debounce rather
		// than deleting it after a transient filesystem error.
		bravoUsageState.scheduleSaveLocked()
	}
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
		if !exists || cooldownEntrySupersedes(entry, current) {
			runtimeState.Cooldowns[key] = entry
		}
	}
	runtimeState.Unlock()
}

// cooldownEntrySupersedes defines one monotonic ordering for both durable and
// runtime merges. A barrier may extend but never shorten. For an identical
// deadline, the later observation wins so refreshed provider metadata is not
// replaced by a stale waiter.
func cooldownEntrySupersedes(candidate, current cooldownEntry) bool {
	return candidate.Until.After(current.Until) ||
		(candidate.Until.Equal(current.Until) && candidate.ObservedAt.After(current.ObservedAt))
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

func restoreAdaptivePendingState(
	pendingSource map[string]*persistedAdaptivePendingState,
	preparedSource map[string]*persistedAdaptivePendingState,
	replace bool,
) {
	ledger := persistedAdaptiveQuotaState{Pending: pendingSource, Prepared: preparedSource}
	normalizePersistedAdaptiveRoutingLedger(&ledger)
	pending, prepared := ledger.Pending, ledger.Prepared
	if ledger.Saturated {
		adaptiveRoutingSaturated.Store(true)
	}
	allocatorRuntime.Lock()
	defer allocatorRuntime.Unlock()
	if replace || allocatorRuntime.PendingPercent == nil {
		allocatorRuntime.PendingPercent = make(map[string]float64, len(pending)+len(prepared))
	}
	if replace || allocatorRuntime.PendingRequests == nil {
		allocatorRuntime.PendingRequests = make(map[string]int, len(pending)+len(prepared))
	}
	if replace || allocatorRuntime.InFlightRequests == nil {
		allocatorRuntime.InFlightRequests = make(map[string]int)
	}
	if replace || allocatorRuntime.OrphanPreparedPercent == nil {
		allocatorRuntime.OrphanPreparedPercent = make(map[string]float64, len(prepared))
	}
	for authIndex, entry := range pending {
		if entry != nil && entry.Percent > allocatorRuntime.PendingPercent[authIndex] {
			allocatorRuntime.PendingPercent[authIndex] = entry.Percent
			// The v4 ledger persists a conservative scalar, not request bodies or
			// request identifiers. After restart expose a lower-bound count rather
			// than claiming that unresolved work is absent.
			allocatorRuntime.PendingRequests[authIndex] = 1
		}
	}
	for authIndex, entry := range prepared {
		if entry != nil {
			// A hot state-path switch keeps the process-local lease alive. Only
			// durable prepare that is not already represented by live in-flight
			// work becomes restart-style pending; otherwise release would charge
			// the same request twice (or leave a phantom after proven rejection).
			orphaned := entry.Percent - allocatorRuntime.InFlightPercent[authIndex]
			if orphaned > 0 {
				allocatorRuntime.PendingPercent[authIndex] += orphaned
				allocatorRuntime.OrphanPreparedPercent[authIndex] += orphaned
				if allocatorRuntime.PendingRequests[authIndex] == 0 {
					allocatorRuntime.PendingRequests[authIndex] = 1
				}
			}
		}
	}
}

func restoreAdaptiveEstimatorState(
	aggregates map[string]*persistedAdaptiveAggregateState,
	profiles map[string]*persistedAdaptiveProfileState,
	replace bool,
) {
	aggregates = normalizePersistedAdaptiveAggregates(aggregates)
	profiles = normalizePersistedAdaptiveProfiles(profiles)
	adaptiveReserveRuntime.Lock()
	defer adaptiveReserveRuntime.Unlock()
	if replace || adaptiveReserveRuntime.Profiles == nil {
		adaptiveReserveRuntime.Profiles = make(map[string]*adaptiveReserveProfile, len(aggregates))
	}
	if replace || adaptiveReserveRuntime.Buckets == nil {
		adaptiveReserveRuntime.Buckets = make(map[string]*adaptiveReserveProfile, len(profiles))
	}
	if replace || adaptiveReserveRuntime.Overflow == nil {
		adaptiveReserveRuntime.Overflow = make(map[string]*adaptiveReserveProfile)
	}
	for authIndex, persisted := range aggregates {
		if persisted == nil {
			continue
		}
		adaptiveReserveRuntime.Profiles[authIndex] = &adaptiveReserveProfile{
			AuthIndex: authIndex, LearnedScale: persisted.LearnedScale, ObservedBurnPerMin: persisted.ObservedBurnPerMin,
			UpdatedAt: persisted.UpdatedAt,
		}
	}
	for key, persisted := range profiles {
		if persisted == nil {
			continue
		}
		adaptiveReserveRuntime.Buckets[key] = &adaptiveReserveProfile{
			AuthIndex: persisted.AuthIndex,
			Shape: adaptiveRequestShape{
				Multiplier: 1, Provider: persisted.Provider, PhysicalModel: persisted.PhysicalModel, ModelFamily: persisted.ModelFamily,
				EffortBucket: persisted.EffortBucket, ContextBucket: persisted.ContextBucket, CostMode: persisted.CostMode,
			},
			Session: adaptiveWindowEstimate{
				LearnedScale: persisted.Session.LearnedScale, ObservedBurnPerMin: persisted.Session.ObservedBurnPerMin,
			},
			Weekly: adaptiveWindowEstimate{
				LearnedScale: persisted.Weekly.LearnedScale, ObservedBurnPerMin: persisted.Weekly.ObservedBurnPerMin,
			},
			UnobservedPercent:  persisted.UnobservedPercent,
			LearnedScale:       math.Max(persisted.Session.LearnedScale, persisted.Weekly.LearnedScale),
			ObservedBurnPerMin: math.Max(persisted.Session.ObservedBurnPerMin, persisted.Weekly.ObservedBurnPerMin),
			UpdatedAt:          persisted.UpdatedAt,
		}
	}
}

func stageAdaptiveEstimatorState(authIndex string, at time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return
	}
	adaptiveReserveRuntime.Lock()
	var aggregate *persistedAdaptiveAggregateState
	if source := adaptiveReserveRuntime.Profiles[authIndex]; source != nil {
		aggregate = &persistedAdaptiveAggregateState{
			LearnedScale: source.LearnedScale, ObservedBurnPerMin: source.ObservedBurnPerMin, UpdatedAt: at.UTC(),
		}
	}
	if overflow := adaptiveReserveRuntime.Overflow[authIndex]; overflow != nil {
		if aggregate == nil {
			aggregate = &persistedAdaptiveAggregateState{UpdatedAt: at.UTC()}
		}
		aggregate.LearnedScale = math.Max(aggregate.LearnedScale, overflow.LearnedScale)
		aggregate.ObservedBurnPerMin = math.Max(aggregate.ObservedBurnPerMin, overflow.ObservedBurnPerMin)
	}
	profiles := make(map[string]*persistedAdaptiveProfileState)
	for key, source := range adaptiveReserveRuntime.Buckets {
		if source == nil || strings.TrimSpace(source.AuthIndex) != authIndex {
			continue
		}
		profiles[key] = &persistedAdaptiveProfileState{
			AuthIndex: source.AuthIndex, Provider: source.Shape.Provider, PhysicalModel: source.Shape.PhysicalModel,
			ModelFamily: source.Shape.ModelFamily, EffortBucket: source.Shape.EffortBucket,
			ContextBucket: source.Shape.ContextBucket, CostMode: source.Shape.CostMode,
			Session: persistedAdaptiveWindowState{
				LearnedScale: source.Session.LearnedScale, ObservedBurnPerMin: source.Session.ObservedBurnPerMin,
			},
			Weekly: persistedAdaptiveWindowState{
				LearnedScale: source.Weekly.LearnedScale, ObservedBurnPerMin: source.Weekly.ObservedBurnPerMin,
			},
			UnobservedPercent: source.UnobservedPercent, UpdatedAt: at.UTC(),
		}
	}
	adaptiveReserveRuntime.Unlock()

	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	if bravoUsageState.path == "" {
		return
	}
	if bravoUsageState.state.AdaptiveQuota.Aggregates == nil {
		bravoUsageState.state.AdaptiveQuota.Aggregates = make(map[string]*persistedAdaptiveAggregateState)
	}
	if bravoUsageState.state.AdaptiveQuota.Profiles == nil {
		bravoUsageState.state.AdaptiveQuota.Profiles = make(map[string]*persistedAdaptiveProfileState)
	}
	if aggregate != nil {
		bravoUsageState.state.AdaptiveQuota.Aggregates[authIndex] = aggregate
	}
	for key, profile := range profiles {
		bravoUsageState.state.AdaptiveQuota.Profiles[key] = profile
	}
	bravoUsageState.scheduleSaveLocked()
}

// persistAdaptivePendingCommit makes accepted work durable before the request
// can return success. Only the small per-auth adaptive WAL is synced here; the
// full analytics snapshot remains on its ordinary debounced path.
func persistAdaptivePendingCommit(authIndex string, percent float64, at time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return
	}
	bravoUsageState.mu.Lock()
	if bravoUsageState.path == "" {
		bravoUsageState.mu.Unlock()
		return
	}
	if !adaptiveLedgerHasAuthLocked(authIndex) && adaptiveLedgerAuthCountLocked() >= adaptiveMaximumPersistedAuthRecords {
		bravoUsageState.state.AdaptiveQuota.Saturated = true
		bravoUsageState.state.AdaptiveQuota.OverflowAuthCount++
		adaptiveRoutingSaturated.Store(true)
		recordAuth := adaptiveLedgerRecordAuthLocked(authIndex)
		record, walPath := nextAdaptiveWALRecordLocked(recordAuth, at)
		bravoUsageState.mu.Unlock()
		if errAppend := adaptiveWALRuntime.append(walPath, record); errAppend != nil {
			_ = persistAdaptiveWALFallback()
		} else {
			scheduleAdaptiveUsageCheckpoint()
		}
		return
	}
	if bravoUsageState.state.AdaptiveQuota.Pending == nil {
		bravoUsageState.state.AdaptiveQuota.Pending = make(map[string]*persistedAdaptivePendingState)
	}
	entry := bravoUsageState.state.AdaptiveQuota.Pending[authIndex]
	if entry == nil {
		entry = &persistedAdaptivePendingState{}
		bravoUsageState.state.AdaptiveQuota.Pending[authIndex] = entry
	}
	entry.Percent = conservativeAdaptiveLedgerSum(entry.Percent, percent)
	entry.UpdatedAt = at.UTC()
	record, walPath := nextAdaptiveWALRecordLocked(authIndex, at)
	bravoUsageState.mu.Unlock()
	if errAppend := adaptiveWALRuntime.append(walPath, record); errAppend != nil {
		_ = persistAdaptiveWALFallback()
	} else {
		scheduleAdaptiveUsageCheckpoint()
	}
}

// persistAdaptivePrepare durably reserves a lease before any provider I/O.
// Returning false guarantees the caller can roll back without having called
// the provider.
func persistAdaptivePrepare(authIndex string, percent float64, at time.Time) bool {
	ok, _ := persistAdaptivePrepareDetailed(authIndex, percent, at)
	return ok
}

func persistAdaptivePrepareDetailed(authIndex string, percent float64, at time.Time) (bool, adaptiveAdmissionRejectionCause) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return false, adaptiveRejectionDurabilityUnavailable
	}
	bravoUsageState.mu.Lock()
	if bravoUsageState.path == "" {
		// Unit tests and an explicitly unconfigured plugin remain memory-only.
		bravoUsageState.mu.Unlock()
		return true, adaptiveRejectionNone
	}
	if bravoUsageState.state.AdaptiveQuota.Saturated {
		if !adaptiveLedgerHasAuthLocked(authIndex) {
			// Saturation can only account for the omitted cardinality globally.
			// New identities, including primaries, must not create untracked work.
			bravoUsageState.mu.Unlock()
			return false, adaptiveRejectionLedgerSaturated
		}
	}
	if !adaptiveLedgerHasAuthLocked(authIndex) && adaptiveLedgerAuthCountLocked() >= adaptiveMaximumPersistedAuthRecords {
		bravoUsageState.mu.Unlock()
		return false, adaptiveRejectionLedgerSaturated
	}
	if bravoUsageState.state.AdaptiveQuota.Prepared == nil {
		bravoUsageState.state.AdaptiveQuota.Prepared = make(map[string]*persistedAdaptivePendingState)
	}
	addPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Prepared, authIndex, percent, at)
	record, walPath := nextAdaptiveWALRecordLocked(authIndex, at)
	bravoUsageState.mu.Unlock()
	if errAppend := adaptiveWALRuntime.append(walPath, record); errAppend == nil {
		scheduleAdaptiveUsageCheckpoint()
		return true, adaptiveRejectionNone
	} else if persistAdaptiveWALFallback() {
		return true, adaptiveRejectionNone
	}
	// No provider call has happened, so a failed durability barrier is safe to
	// roll back and must fail admission closed.
	bravoUsageState.mu.Lock()
	subtractPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Prepared, authIndex, percent, at)
	bravoUsageState.mu.Unlock()
	return false, adaptiveRejectionDurabilityUnavailable
}

func persistAdaptiveFinalize(authIndex string, percent float64, commit bool, at time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return
	}
	bravoUsageState.mu.Lock()
	if bravoUsageState.path == "" {
		bravoUsageState.mu.Unlock()
		return
	}
	if bravoUsageState.state.AdaptiveQuota.Saturated && !adaptiveLedgerHasAuthLocked(authIndex) {
		bravoUsageState.mu.Unlock()
		return
	}
	subtractPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Prepared, authIndex, percent, at)
	if commit {
		if bravoUsageState.state.AdaptiveQuota.Pending == nil {
			bravoUsageState.state.AdaptiveQuota.Pending = make(map[string]*persistedAdaptivePendingState)
		}
		addPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Pending, authIndex, percent, at)
	}
	record, walPath := nextAdaptiveWALRecordLocked(authIndex, at)
	bravoUsageState.mu.Unlock()
	if errEnqueue := adaptiveWALRuntime.appendAsync(walPath, record); errEnqueue != nil {
		// The already-durable prepare remains conservative if enqueue itself
		// fails. Persist the finalized absolute state on the ordinary writer
		// without delaying the provider response.
		bravoUsageState.mu.Lock()
		bravoUsageState.scheduleSaveLocked()
		bravoUsageState.mu.Unlock()
	} else {
		scheduleAdaptiveUsageCheckpoint()
	}
}

func adaptiveLedgerHasAuthLocked(authIndex string) bool {
	if _, ok := bravoUsageState.state.AdaptiveQuota.Pending[authIndex]; ok {
		return true
	}
	_, ok := bravoUsageState.state.AdaptiveQuota.Prepared[authIndex]
	return ok
}

func adaptiveDurableLedgerTracksAuth(authIndex string) bool {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	return bravoUsageState.path == "" || adaptiveLedgerHasAuthLocked(strings.TrimSpace(authIndex))
}

func adaptiveLedgerAuthCountLocked() int {
	keys := make(map[string]struct{}, len(bravoUsageState.state.AdaptiveQuota.Pending)+len(bravoUsageState.state.AdaptiveQuota.Prepared))
	for authIndex := range bravoUsageState.state.AdaptiveQuota.Pending {
		keys[authIndex] = struct{}{}
	}
	for authIndex := range bravoUsageState.state.AdaptiveQuota.Prepared {
		keys[authIndex] = struct{}{}
	}
	return len(keys)
}

func adaptiveLedgerRecordAuthLocked(fallback string) string {
	if adaptiveLedgerHasAuthLocked(fallback) {
		return fallback
	}
	for authIndex := range bravoUsageState.state.AdaptiveQuota.Pending {
		return authIndex
	}
	for authIndex := range bravoUsageState.state.AdaptiveQuota.Prepared {
		return authIndex
	}
	return fallback
}

// clearAdaptiveRoutingSaturationAfterReconciliation is the explicit recovery
// path for an operator who has reconciled every provider account represented
// by an overflow marker. Retained per-auth debt must be cleared first. The
// global gate only opens after the absolute clear record is durable.
func clearAdaptiveRoutingSaturationAfterReconciliation(at time.Time) error {
	adaptiveAdmissionMu.Lock()
	defer adaptiveAdmissionMu.Unlock()
	return clearAdaptiveRoutingSaturationAfterReconciliationLocked(at)
}

// clearAdaptiveRoutingSaturationAfterReconciliationLocked requires the
// admission write gate. The management reconciler keeps that gate across the
// estimator/demand readiness checks and all related resets as one transaction.
func clearAdaptiveRoutingSaturationAfterReconciliationLocked(at time.Time) error {
	allocatorRuntime.Lock()
	for _, amount := range allocatorRuntime.InFlightPercent {
		if amount > 0 {
			allocatorRuntime.Unlock()
			return fmt.Errorf("adaptive quota ledger still contains in-flight work")
		}
	}
	for _, amount := range allocatorRuntime.PendingPercent {
		if amount > 0 {
			allocatorRuntime.Unlock()
			return fmt.Errorf("adaptive quota ledger still contains runtime pending work")
		}
	}
	allocatorRuntime.Unlock()
	// Async finalizations use the same ordered WAL writer. Drain everything
	// admitted before this reconciliation epoch before resetting revisions;
	// otherwise an old revN could land after compaction and shadow a new rev1.
	if errBarrier := adaptiveWALRuntime.barrier(); errBarrier != nil {
		return fmt.Errorf("drain adaptive quota WAL before reconciliation: %w", errBarrier)
	}
	bravoUsageState.mu.Lock()
	if bravoUsageState.path == "" || !bravoUsageState.state.AdaptiveQuota.Saturated {
		bravoUsageState.mu.Unlock()
		return nil
	}
	if adaptiveLedgerAuthCountLocked() != 0 {
		bravoUsageState.mu.Unlock()
		return fmt.Errorf("adaptive quota ledger still contains unresolved work")
	}
	previousRevisions := bravoUsageState.state.AdaptiveQuota.Revisions
	previousOverflow := bravoUsageState.state.AdaptiveQuota.OverflowAuthCount
	bravoUsageState.state.AdaptiveQuota.Saturated = false
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 0
	// With no unresolved work, old per-auth revisions no longer protect debt.
	// Replace them in the same durable snapshot that clears saturation, then
	// compact the old WAL. This prevents the first post-recovery auth from
	// becoming a 4097th record and being truncated on restart.
	bravoUsageState.state.AdaptiveQuota.Revisions = make(map[string]uint64)
	if errSave := bravoUsageState.saveLocked(); errSave != nil {
		bravoUsageState.state.AdaptiveQuota.Saturated = true
		bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = previousOverflow
		bravoUsageState.state.AdaptiveQuota.Revisions = previousRevisions
		bravoUsageState.mu.Unlock()
		adaptiveRoutingSaturated.Store(true)
		return fmt.Errorf("persist adaptive saturation reconciliation: %w", errSave)
	}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(false)
	return nil
}

func persistAdaptivePendingClear(authIndex string, pendingAmount, orphanPreparedAmount float64, at time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || (pendingAmount <= 0 && orphanPreparedAmount <= 0) ||
		math.IsNaN(pendingAmount) || math.IsInf(pendingAmount, 0) ||
		math.IsNaN(orphanPreparedAmount) || math.IsInf(orphanPreparedAmount, 0) {
		return
	}
	bravoUsageState.mu.Lock()
	if bravoUsageState.path == "" {
		bravoUsageState.mu.Unlock()
		return
	}
	// A confirmed refresh only covers the Pending watermark captured before
	// provider I/O. Prepared belongs to live/ambiguous calls and must never be
	// consumed as an inferred residual.
	subtractPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Pending, authIndex, pendingAmount, at)
	// Only the portion explicitly identified as crash-orphaned at refresh start
	// may be removed from Prepared. A newer live prepare is never inferred from
	// aggregate residual arithmetic.
	subtractPersistedAdaptivePercent(bravoUsageState.state.AdaptiveQuota.Prepared, authIndex, orphanPreparedAmount, at)
	record, walPath := nextAdaptiveWALRecordLocked(authIndex, at)
	bravoUsageState.mu.Unlock()
	if errAppend := adaptiveWALRuntime.append(walPath, record); errAppend != nil {
		_ = persistAdaptiveWALFallback()
	} else {
		scheduleAdaptiveUsageCheckpoint()
	}
}

func addPersistedAdaptivePercent(
	values map[string]*persistedAdaptivePendingState,
	authIndex string,
	percent float64,
	at time.Time,
) {
	entry := values[authIndex]
	if entry == nil {
		entry = &persistedAdaptivePendingState{}
		values[authIndex] = entry
	}
	entry.Percent = conservativeAdaptiveLedgerSum(entry.Percent, percent)
	entry.UpdatedAt = at.UTC()
}

func conservativeAdaptiveLedgerSum(current, increment float64) float64 {
	if current <= 0 {
		current = 0
	}
	if increment <= 0 {
		return current
	}
	if current > adaptiveMaximumPersistedLedgerPercent-increment {
		return adaptiveMaximumPersistedLedgerPercent
	}
	return current + increment
}

// subtractPersistedAdaptivePercent returns the amount that was not present in
// this ledger and still has to be reconciled against another ledger.
func subtractPersistedAdaptivePercent(
	values map[string]*persistedAdaptivePendingState,
	authIndex string,
	amount float64,
	at time.Time,
) float64 {
	if amount <= 0 || values == nil {
		return amount
	}
	entry := values[authIndex]
	if entry == nil {
		return amount
	}
	consumed := math.Min(entry.Percent, amount)
	entry.Percent -= consumed
	entry.UpdatedAt = at.UTC()
	if entry.Percent <= 0 {
		delete(values, authIndex)
	}
	return amount - consumed
}

func nextAdaptiveWALRecordLocked(authIndex string, at time.Time) (adaptiveWALRecord, string) {
	if bravoUsageState.state.AdaptiveQuota.Revisions == nil {
		bravoUsageState.state.AdaptiveQuota.Revisions = make(map[string]uint64)
	}
	revision := bravoUsageState.state.AdaptiveQuota.Revisions[authIndex] + 1
	bravoUsageState.state.AdaptiveQuota.Revisions[authIndex] = revision
	saturated := bravoUsageState.state.AdaptiveQuota.Saturated
	record := adaptiveWALRecord{
		Version:           adaptiveWALVersion,
		AuthIndex:         authIndex,
		Revision:          revision,
		Saturated:         &saturated,
		OverflowAuthCount: bravoUsageState.state.AdaptiveQuota.OverflowAuthCount,
		RecordedAt:        at.UTC(),
	}
	if pending := bravoUsageState.state.AdaptiveQuota.Pending[authIndex]; pending != nil {
		copyPending := *pending
		record.Pending = &copyPending
	}
	if prepared := bravoUsageState.state.AdaptiveQuota.Prepared[authIndex]; prepared != nil {
		copyPrepared := *prepared
		record.Prepared = &copyPrepared
	}
	return record, adaptiveWALPath(bravoUsageState.path)
}

func persistAdaptiveWALFallback() bool {
	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	if errSave := bravoUsageState.saveLocked(); errSave != nil {
		// The provider has already accepted the request. Keep the conservative
		// in-memory barrier and retry instead of manufacturing a false rejection.
		bravoUsageState.scheduleSaveLocked()
		return false
	}
	return true
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
