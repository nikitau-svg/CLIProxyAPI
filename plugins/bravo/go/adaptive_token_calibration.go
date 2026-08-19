package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	adaptiveTokenMaximumAccounts         = 4096
	adaptiveTokenMaximumEventsPerAccount = 2048
	adaptiveTokenMaximumRuntimeEvents    = 32768
	adaptiveTokenMaximumUsageProfiles    = 4096
	adaptiveTokenMaximumWindowProfiles   = 4096
	adaptiveTokenMinimumUsageSamples     = 8
	adaptiveTokenMinimumEffectiveUsage   = 4.0
	adaptiveTokenMinimumWindowIntervals  = 4
	adaptiveTokenMinimumEffectiveWindows = 3.5
	adaptiveTokenMinimumCoverage         = 30 * time.Minute
	adaptiveTokenProfileRetention        = 31 * 24 * time.Hour
	adaptiveTokenRateHalfLife            = 24 * time.Hour
	adaptiveTokenSafetyMultiplier        = 1.25
	adaptiveTokenQuantizationMarginPP    = 0.05
	adaptiveTokenMinimumReservationPP    = 0.001
	adaptiveTokenKeyVersion              = "v1"
)

var adaptiveTokenCompletionBuckets = [...]int64{
	64, 128, 256, 512, 1024, 2048, 4096, 8192,
	16384, 32768, 65536, 131072, 262144, 524288, 1048576,
}

type adaptiveTokenUsageEvent struct {
	CompletedAt  time.Time
	AuthIndex    string
	ProjectID    string
	Provider     string
	Model        string
	LogicalModel string
	Effort       string
	TariffID     string
	Multiplier   float64
	TokenUnits   float64
	InputTokens  int64
	OutputTokens int64
}

type adaptiveTokenEventAccount struct {
	Events    []adaptiveTokenUsageEvent
	Saturated bool
}

type persistedAdaptiveTokenUsageProfile struct {
	AuthIndex         string    `json:"auth_index"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Effort            string    `json:"effort,omitempty"`
	TariffID          string    `json:"tariff_id,omitempty"`
	SampleCount       uint64    `json:"sample_count"`
	Samples           float64   `json:"samples"`
	InputTokens       float64   `json:"input_tokens"`
	OutputTokens      float64   `json:"output_tokens"`
	CompletionBuckets []float64 `json:"completion_buckets,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type persistedAdaptiveTokenWindowProfile struct {
	AuthIndex             string    `json:"auth_index"`
	Provider              string    `json:"provider"`
	Model                 string    `json:"model"`
	Effort                string    `json:"effort,omitempty"`
	TariffID              string    `json:"tariff_id,omitempty"`
	WindowKind            string    `json:"window_kind"`
	QuotaModel            string    `json:"quota_model,omitempty"`
	IntervalSamples       uint64    `json:"interval_samples"`
	EffectiveIntervals    float64   `json:"effective_intervals"`
	CoverageSeconds       float64   `json:"coverage_seconds"`
	EffectiveTokenUnits   float64   `json:"effective_token_units"`
	AttributedDropPercent float64   `json:"attributed_drop_percent"`
	ZeroDropIntervals     float64   `json:"zero_drop_intervals"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type adaptiveTokenWindowEstimate struct {
	Percent   float64
	Available bool
	Intervals float64
}

type adaptiveTokenCalibrationEstimate struct {
	PredictedTokens float64
	Session         adaptiveTokenWindowEstimate
	Weekly          adaptiveTokenWindowEstimate
	ModelWeekly     adaptiveTokenWindowEstimate
	ModelWeeklyName string
	UsageSamples    float64
	Confidence      string
}

type adaptiveTokenCalibrationPublicView struct {
	Status                   string                          `json:"status"`
	TrackedAccounts          int                             `json:"tracked_accounts"`
	TrackedUsageProfiles     int                             `json:"tracked_usage_profiles"`
	TrackedWindowProfiles    int                             `json:"tracked_window_profiles"`
	ReadyWindowProfiles      int                             `json:"ready_window_profiles"`
	Windows                  []adaptiveTokenWindowPublicView `json:"windows,omitempty"`
	RuntimeQueuedEvents      int                             `json:"runtime_queued_events"`
	RuntimeSaturatedAccounts int                             `json:"runtime_saturated_accounts"`
	RuntimeSaturated         bool                            `json:"runtime_saturated"`
	DroppedEvents            uint64                          `json:"dropped_events"`
	DroppedLateEvents        uint64                          `json:"dropped_late_events"`
	DroppedCapacityEvents    uint64                          `json:"dropped_capacity_events"`
	DroppedProfiles          uint64                          `json:"dropped_profiles"`
	PersistedSaturated       bool                            `json:"persisted_saturated"`
	Note                     string                          `json:"note"`
}

type adaptiveTokenWindowPublicView struct {
	Provider               string  `json:"provider"`
	WindowKind             string  `json:"window_kind"`
	QuotaModel             string  `json:"quota_model,omitempty"`
	Profiles               int     `json:"profiles"`
	ReadyProfiles          int     `json:"ready_profiles"`
	EffectiveIntervals     float64 `json:"effective_profile_intervals"`
	CoverageSeconds        float64 `json:"profile_coverage_seconds"`
	EffectiveTokenUnits    float64 `json:"effective_token_units"`
	AttributedDropPercent  float64 `json:"attributed_drop_percent"`
	DropPPPerMillionTokens float64 `json:"drop_pp_per_million_tokens"`
	Confidence             string  `json:"confidence"`
}

var adaptiveTokenRuntime = struct {
	sync.Mutex
	Accounts              map[string]*adaptiveTokenEventAccount
	TotalEvents           int
	Saturated             bool
	DroppedEvents         uint64
	DroppedLateEvents     uint64
	DroppedCapacityEvents uint64
}{Accounts: make(map[string]*adaptiveTokenEventAccount)}

func buildAdaptiveTokenUsageEvent(record pluginapi.UsageRecord) (adaptiveTokenUsageEvent, bool) {
	cfg := loadedConfig()
	if cfg.AdaptiveAllocatorMode != "observe" || !record.Generate {
		return adaptiveTokenUsageEvent{}, false
	}
	authIndex := strings.TrimSpace(record.AuthIndex)
	provider := normalizeProvider(firstNonEmpty(record.Provider, record.AuthType, record.ExecutorType))
	model := strings.TrimSpace(record.Model)
	if authIndex == "" || provider == "" || model == "" {
		return adaptiveTokenUsageEvent{}, false
	}
	input, output, units := adaptiveTokenUsageUnits(provider, record.Detail)
	if units <= 0 {
		return adaptiveTokenUsageEvent{}, false
	}
	completedAt := record.RequestedAt.UTC()
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	} else if record.Latency > 0 {
		completedAt = completedAt.Add(record.Latency)
	}
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	subscription := subscriptionPolicy(cfg, authIndex)
	tariff := effectiveTariff(cfg, subscription, provider, quota)
	event := adaptiveTokenUsageEvent{
		CompletedAt:  completedAt,
		AuthIndex:    authIndex,
		ProjectID:    projectIDFromUsagePrincipal(record.APIKey),
		Provider:     provider,
		Model:        model,
		LogicalModel: strings.TrimSpace(record.Alias),
		Effort:       normalizeEffort(record.ReasoningEffort),
		TariffID:     strings.TrimSpace(tariff.ID),
		Multiplier:   math.Max(tariff.Multiplier, 1),
		TokenUnits:   units,
		InputTokens:  input,
		OutputTokens: output,
	}
	adaptiveTokenRuntime.Lock()
	account := adaptiveTokenRuntime.Accounts[authIndex]
	if account == nil {
		if len(adaptiveTokenRuntime.Accounts) >= adaptiveTokenMaximumAccounts ||
			adaptiveTokenRuntime.TotalEvents >= adaptiveTokenMaximumRuntimeEvents {
			adaptiveTokenRuntime.Saturated = true
			adaptiveTokenRuntime.DroppedEvents++
			adaptiveTokenRuntime.DroppedCapacityEvents++
			adaptiveTokenRuntime.Unlock()
			return adaptiveTokenUsageEvent{}, false
		}
		account = &adaptiveTokenEventAccount{}
		adaptiveTokenRuntime.Accounts[authIndex] = account
	}
	if len(account.Events) >= adaptiveTokenMaximumEventsPerAccount ||
		adaptiveTokenRuntime.TotalEvents >= adaptiveTokenMaximumRuntimeEvents {
		account.Saturated = true
		adaptiveTokenRuntime.Saturated = true
		adaptiveTokenRuntime.DroppedEvents++
		adaptiveTokenRuntime.DroppedCapacityEvents++
		adaptiveTokenRuntime.Unlock()
		return adaptiveTokenUsageEvent{}, false
	}
	account.Events = append(account.Events, event)
	adaptiveTokenRuntime.TotalEvents++
	adaptiveTokenRuntime.Unlock()
	return event, true
}

func adaptiveTokenUsageUnits(provider string, detail pluginapi.UsageDetail) (int64, int64, float64) {
	input := maxInt64(detail.InputTokens, 0)
	output := maxInt64(detail.OutputTokens, 0)
	reasoning := maxInt64(detail.ReasoningTokens, 0)
	completion := maxInt64(output, reasoning)
	if output > 0 && reasoning > 0 && provider != "codex" {
		completion = output + reasoning
	}
	base := maxInt64(detail.TotalTokens, input+completion)
	if provider == "claude" {
		base = maxInt64(base, input+completion+maxInt64(detail.CacheReadTokens, 0)+maxInt64(detail.CacheCreationTokens, 0))
	}
	if base <= 0 {
		return input, completion, 0
	}
	return input, completion, float64(base)
}

func recordAdaptiveTokenUsageProfileLocked(state *persistedUsageState, event adaptiveTokenUsageEvent) {
	if state == nil || event.AuthIndex == "" || event.TokenUnits <= 0 {
		return
	}
	if state.AdaptiveTokenUsageProfiles == nil {
		state.AdaptiveTokenUsageProfiles = make(map[string]*persistedAdaptiveTokenUsageProfile)
	}
	key := adaptiveTokenUsageProfileKey(event.AuthIndex, event.Provider, event.Model, event.Effort, event.TariffID)
	profile := state.AdaptiveTokenUsageProfiles[key]
	if profile == nil {
		if len(state.AdaptiveTokenUsageProfiles) >= adaptiveTokenMaximumUsageProfiles {
			state.AdaptiveTokenCalibrationSaturated = true
			state.AdaptiveTokenDroppedProfiles++
			return
		}
		profile = &persistedAdaptiveTokenUsageProfile{
			AuthIndex: event.AuthIndex, Provider: event.Provider, Model: event.Model,
			Effort: event.Effort, TariffID: event.TariffID,
			CompletionBuckets: make([]float64, len(adaptiveTokenCompletionBuckets)+1),
		}
		state.AdaptiveTokenUsageProfiles[key] = profile
	}
	decayAdaptiveTokenUsageProfile(profile, event.CompletedAt)
	profile.SampleCount++
	profile.Samples++
	profile.InputTokens += float64(event.InputTokens)
	profile.OutputTokens += float64(event.OutputTokens)
	if profile.UpdatedAt.IsZero() || event.CompletedAt.After(profile.UpdatedAt) {
		profile.UpdatedAt = event.CompletedAt.UTC()
	}
	bucket := sort.Search(len(adaptiveTokenCompletionBuckets), func(index int) bool {
		return event.OutputTokens <= adaptiveTokenCompletionBuckets[index]
	})
	if bucket >= len(profile.CompletionBuckets) {
		bucket = len(profile.CompletionBuckets) - 1
	}
	profile.CompletionBuckets[bucket]++
	if state.AdaptiveTokenCalibrationStartedAt.IsZero() {
		state.AdaptiveTokenCalibrationStartedAt = event.CompletedAt.UTC()
	}
}

func decayAdaptiveTokenUsageProfile(profile *persistedAdaptiveTokenUsageProfile, at time.Time) {
	if profile == nil || profile.UpdatedAt.IsZero() || !at.After(profile.UpdatedAt) {
		return
	}
	decay := math.Pow(0.5, at.Sub(profile.UpdatedAt).Seconds()/adaptiveTokenRateHalfLife.Seconds())
	profile.Samples *= decay
	profile.InputTokens *= decay
	profile.OutputTokens *= decay
	for index := range profile.CompletionBuckets {
		profile.CompletionBuckets[index] *= decay
	}
}

func reconcileAdaptiveTokenCalibration(
	cfg pluginConfig,
	authIndex string,
	previous credentialQuotaState,
	refreshed credentialQuotaState,
	previousAt time.Time,
	observedAt time.Time,
) []adaptiveTokenUsageEvent {
	authIndex = strings.TrimSpace(authIndex)
	if cfg.AdaptiveAllocatorMode != "observe" || authIndex == "" || previousAt.IsZero() ||
		observedAt.IsZero() || !observedAt.After(previousAt) {
		return nil
	}
	adaptiveTokenRuntime.Lock()
	account := adaptiveTokenRuntime.Accounts[authIndex]
	if account == nil {
		adaptiveTokenRuntime.Unlock()
		return nil
	}
	covered := make([]adaptiveTokenUsageEvent, 0, len(account.Events))
	remaining := account.Events[:0]
	for _, event := range account.Events {
		switch {
		case event.CompletedAt.After(observedAt):
			remaining = append(remaining, event)
		case event.CompletedAt.After(previousAt):
			covered = append(covered, event)
		default:
			// Never move an old event into a newer provider interval: that could
			// lower its learned rate without proof that the new drop covered it.
			adaptiveTokenRuntime.DroppedEvents++
			adaptiveTokenRuntime.DroppedLateEvents++
		}
	}
	removed := len(account.Events) - len(remaining)
	account.Events = remaining
	adaptiveTokenRuntime.TotalEvents -= removed
	if adaptiveTokenRuntime.TotalEvents < 0 {
		adaptiveTokenRuntime.TotalEvents = 0
	}
	if adaptiveTokenRuntime.TotalEvents < adaptiveTokenMaximumRuntimeEvents {
		adaptiveTokenRuntime.Saturated = false
	}
	if len(account.Events) < adaptiveTokenMaximumEventsPerAccount {
		account.Saturated = false
	}
	if len(account.Events) == 0 && !account.Saturated {
		delete(adaptiveTokenRuntime.Accounts, authIndex)
	}
	adaptiveTokenRuntime.Unlock()
	if len(covered) == 0 {
		return nil
	}
	provider := normalizeProvider(firstNonEmpty(refreshed.Provider, previous.Provider, covered[0].Provider))
	observations := make([]adaptiveTokenCalibrationMutation, 0)
	observations = append(observations, buildAdaptiveTokenCalibrationMutations(
		authIndex, provider, pluginapi.HostAuthQuotaWindowKindSession, "",
		previous.Session, refreshed.Session, previousAt, observedAt, covered,
	)...)
	observations = append(observations, buildAdaptiveTokenCalibrationMutations(
		authIndex, provider, pluginapi.HostAuthQuotaWindowKindWeekly, "",
		previous.Weekly, refreshed.Weekly, previousAt, observedAt, covered,
	)...)
	refreshedModels := make(map[string]quotaWindowState, len(refreshed.ModelWeekly))
	for _, window := range refreshed.ModelWeekly {
		model := strings.ToLower(strings.TrimSpace(window.Model))
		if model != "" {
			refreshedModels[model] = window.quotaWindowState
		}
	}
	for _, window := range previous.ModelWeekly {
		model := strings.ToLower(strings.TrimSpace(window.Model))
		after, ok := refreshedModels[model]
		if model == "" || !ok {
			continue
		}
		observations = append(observations, buildAdaptiveTokenCalibrationMutations(
			authIndex, provider, pluginapi.HostAuthQuotaWindowKindModelWeekly, model,
			window.quotaWindowState, after, previousAt, observedAt, covered,
		)...)
	}
	if len(observations) == 0 {
		return covered
	}
	bravoUsageState.mu.Lock()
	normalizeAdaptiveTokenCalibrationState(&bravoUsageState.state)
	for _, mutation := range observations {
		applyAdaptiveTokenCalibrationMutationLocked(&bravoUsageState.state, mutation)
	}
	bravoUsageState.scheduleSaveLocked()
	bravoUsageState.mu.Unlock()
	return covered
}

func applyAdaptiveTokenWeightsToShadowCommits(
	commits []adaptiveShadowCommit,
	events []adaptiveTokenUsageEvent,
) []adaptiveShadowCommit {
	if len(commits) == 0 || len(events) == 0 {
		return commits
	}
	keyFor := func(projectID, provider, model, effort, tariffID string) string {
		return strings.Join([]string{
			strings.TrimSpace(projectID), normalizeProvider(provider), strings.TrimSpace(model),
			normalizeEffort(effort), strings.TrimSpace(tariffID),
		}, "\x1f")
	}
	actual := make(map[string]float64)
	for _, event := range events {
		if event.TokenUnits <= 0 || event.ProjectID == "" {
			continue
		}
		key := keyFor(event.ProjectID, event.Provider, event.Model, event.Effort, event.TariffID)
		actual[key] += event.TokenUnits
	}
	if len(actual) == 0 {
		return commits
	}
	counts := make(map[string]int)
	for _, commit := range commits {
		key := keyFor(commit.ProjectID, commit.Provider, commit.Model, commit.Effort, commit.TariffID)
		if actual[key] > 0 {
			counts[key]++
		}
	}
	out := append([]adaptiveShadowCommit(nil), commits...)
	for index := range out {
		key := keyFor(out[index].ProjectID, out[index].Provider, out[index].Model, out[index].Effort, out[index].TariffID)
		if counts[key] > 0 {
			out[index].TokenUnits = actual[key] / float64(counts[key])
		}
	}
	return out
}

type adaptiveTokenCalibrationMutation struct {
	Event            adaptiveTokenUsageEvent
	WindowKind       string
	QuotaModel       string
	CoverageSeconds  float64
	TokenUnits       float64
	AttributedDropPP float64
	ObservedAt       time.Time
}

func buildAdaptiveTokenCalibrationMutations(
	authIndex, provider, kind, quotaModel string,
	before, after quotaWindowState,
	previousAt, observedAt time.Time,
	events []adaptiveTokenUsageEvent,
) []adaptiveTokenCalibrationMutation {
	drop, valid := quotaWindowObservedDrop(before, after, previousAt, observedAt)
	if !valid {
		return nil
	}
	type group struct {
		event adaptiveTokenUsageEvent
		units float64
	}
	groups := make(map[string]*group)
	totalUnits := 0.0
	for _, event := range events {
		if kind == pluginapi.HostAuthQuotaWindowKindModelWeekly && !quotaModelMatches(event.Model, quotaModel) {
			continue
		}
		if event.TokenUnits <= 0 || math.IsNaN(event.TokenUnits) || math.IsInf(event.TokenUnits, 0) {
			continue
		}
		key := adaptiveTokenUsageProfileKey(authIndex, provider, event.Model, event.Effort, event.TariffID)
		item := groups[key]
		if item == nil {
			copyEvent := event
			copyEvent.Provider = provider
			item = &group{event: copyEvent}
			groups[key] = item
		}
		item.units += event.TokenUnits
		totalUnits += event.TokenUnits
	}
	if totalUnits <= 0 {
		return nil
	}
	coverage := math.Max(observedAt.Sub(previousAt).Seconds(), 0)
	out := make([]adaptiveTokenCalibrationMutation, 0, len(groups))
	for _, item := range groups {
		out = append(out, adaptiveTokenCalibrationMutation{
			Event: item.event, WindowKind: kind, QuotaModel: quotaModel,
			CoverageSeconds: coverage, TokenUnits: item.units,
			AttributedDropPP: drop * item.units / totalUnits, ObservedAt: observedAt.UTC(),
		})
	}
	return out
}

func applyAdaptiveTokenCalibrationMutationLocked(state *persistedUsageState, mutation adaptiveTokenCalibrationMutation) {
	if state == nil || mutation.TokenUnits <= 0 {
		return
	}
	key := adaptiveTokenWindowProfileKey(
		mutation.Event.AuthIndex, mutation.Event.Provider, mutation.Event.Model,
		mutation.Event.Effort, mutation.Event.TariffID, mutation.WindowKind, mutation.QuotaModel,
	)
	profile := state.AdaptiveTokenWindowProfiles[key]
	if profile == nil {
		if len(state.AdaptiveTokenWindowProfiles) >= adaptiveTokenMaximumWindowProfiles {
			state.AdaptiveTokenCalibrationSaturated = true
			state.AdaptiveTokenDroppedProfiles++
			return
		}
		profile = &persistedAdaptiveTokenWindowProfile{
			AuthIndex: mutation.Event.AuthIndex, Provider: mutation.Event.Provider,
			Model: mutation.Event.Model, Effort: mutation.Event.Effort, TariffID: mutation.Event.TariffID,
			WindowKind: mutation.WindowKind, QuotaModel: mutation.QuotaModel,
		}
		state.AdaptiveTokenWindowProfiles[key] = profile
	}
	decayAdaptiveTokenWindowProfile(profile, mutation.ObservedAt)
	profile.IntervalSamples++
	profile.EffectiveIntervals++
	profile.CoverageSeconds += mutation.CoverageSeconds
	profile.EffectiveTokenUnits += mutation.TokenUnits
	profile.AttributedDropPercent += math.Max(mutation.AttributedDropPP, 0)
	if mutation.AttributedDropPP <= 0 {
		profile.ZeroDropIntervals++
	}
	profile.UpdatedAt = mutation.ObservedAt.UTC()
}

func decayAdaptiveTokenWindowProfile(profile *persistedAdaptiveTokenWindowProfile, at time.Time) {
	if profile == nil || profile.UpdatedAt.IsZero() || !at.After(profile.UpdatedAt) {
		return
	}
	decay := math.Pow(0.5, at.Sub(profile.UpdatedAt).Seconds()/adaptiveTokenRateHalfLife.Seconds())
	profile.EffectiveIntervals *= decay
	profile.CoverageSeconds *= decay
	profile.EffectiveTokenUnits *= decay
	profile.AttributedDropPercent *= decay
	profile.ZeroDropIntervals *= decay
}

func adaptiveTokenCalibrationFor(
	authIndex, provider, model, effort, tariffID string,
	quota credentialQuotaState,
	features adaptiveShadowRequestFeatures,
	now time.Time,
) adaptiveTokenCalibrationEstimate {
	provider = normalizeProvider(provider)
	model = strings.TrimSpace(model)
	effort = normalizeEffort(effort)
	tariffID = strings.TrimSpace(tariffID)
	view := adaptiveTokenCalibrationEstimate{Confidence: "token_calibration_collecting"}
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	state := &bravoUsageState.state
	usage := state.AdaptiveTokenUsageProfiles[adaptiveTokenUsageProfileKey(authIndex, provider, model, effort, tariffID)]
	usageDecay := adaptiveTokenReadDecay(usageUpdatedAt(usage), now)
	if usage == nil || usage.SampleCount < adaptiveTokenMinimumUsageSamples ||
		usage.Samples*usageDecay < adaptiveTokenMinimumEffectiveUsage {
		return view
	}
	view.UsageSamples = float64(usage.SampleCount)
	completion := adaptiveTokenCompletionP90(usage)
	if features.OutputTrusted {
		completion = math.Min(completion, math.Max(features.DeclaredOutputTokens, 0))
	}
	view.PredictedTokens = math.Max(features.InputTokens+completion, 1)
	view.Session = adaptiveTokenWindowEstimateFromState(state, authIndex, provider, model, effort, tariffID,
		pluginapi.HostAuthQuotaWindowKindSession, "", view.PredictedTokens, now)
	view.Weekly = adaptiveTokenWindowEstimateFromState(state, authIndex, provider, model, effort, tariffID,
		pluginapi.HostAuthQuotaWindowKindWeekly, "", view.PredictedTokens, now)
	for _, window := range quota.ModelWeekly {
		if !quotaModelMatches(model, window.Model) {
			continue
		}
		candidate := adaptiveTokenWindowEstimateFromState(state, authIndex, provider, model, effort, tariffID,
			pluginapi.HostAuthQuotaWindowKindModelWeekly, strings.ToLower(strings.TrimSpace(window.Model)),
			view.PredictedTokens, now)
		if candidate.Available {
			view.ModelWeekly = candidate
			view.ModelWeeklyName = strings.ToLower(strings.TrimSpace(window.Model))
			break
		}
	}
	available := 0
	for _, item := range []adaptiveTokenWindowEstimate{view.Session, view.Weekly, view.ModelWeekly} {
		if item.Available {
			available++
		}
	}
	weeklyKind, _ := adaptiveShadowWeeklyWindow(quota, model)
	weeklyAvailable := view.Weekly.Available
	if weeklyKind == pluginapi.HostAuthQuotaWindowKindModelWeekly {
		weeklyAvailable = view.ModelWeekly.Available
	}
	// Only a decision whose two effective quota dimensions are calibrated may
	// enter the v2 audit cohort. A partially learned request still uses the
	// legacy shape estimate for at least one limiting window and would otherwise
	// make old false-withhold behaviour look like a failure of token calibration.
	if view.Session.Available && weeklyAvailable {
		view.Confidence = "token_calibrated_complete"
	} else if available > 0 {
		view.Confidence = fmt.Sprintf("partial_token_calibration_%d_windows", available)
	}
	return view
}

func adaptiveTokenWindowEstimateFromState(
	state *persistedUsageState,
	authIndex, provider, model, effort, tariffID, kind, quotaModel string,
	predictedTokens float64,
	now time.Time,
) adaptiveTokenWindowEstimate {
	if state == nil || predictedTokens <= 0 {
		return adaptiveTokenWindowEstimate{}
	}
	profile := state.AdaptiveTokenWindowProfiles[adaptiveTokenWindowProfileKey(
		authIndex, provider, model, effort, tariffID, kind, quotaModel,
	)]
	if profile == nil || profile.IntervalSamples < adaptiveTokenMinimumWindowIntervals {
		return adaptiveTokenWindowEstimate{}
	}
	decay := adaptiveTokenReadDecay(profile.UpdatedAt, now)
	intervals := profile.EffectiveIntervals * decay
	coverage := profile.CoverageSeconds * decay
	tokenUnits := profile.EffectiveTokenUnits * decay
	dropPercent := profile.AttributedDropPercent * decay
	if intervals < adaptiveTokenMinimumEffectiveWindows || coverage < adaptiveTokenMinimumCoverage.Seconds() || tokenUnits <= 0 {
		return adaptiveTokenWindowEstimate{}
	}
	margin := adaptiveTokenQuantizationMarginPP * math.Sqrt(math.Max(intervals, 1))
	rate := (dropPercent + margin) / tokenUnits
	percent := math.Min(math.Max(rate*predictedTokens*adaptiveTokenSafetyMultiplier, adaptiveTokenMinimumReservationPP),
		adaptiveShadowMaximumReservationPercent)
	return adaptiveTokenWindowEstimate{Percent: percent, Available: true, Intervals: intervals}
}

func usageUpdatedAt(profile *persistedAdaptiveTokenUsageProfile) time.Time {
	if profile == nil {
		return time.Time{}
	}
	return profile.UpdatedAt
}

func adaptiveTokenReadDecay(updatedAt time.Time, now time.Time) float64 {
	if updatedAt.IsZero() {
		return 0
	}
	age := now.UTC().Sub(updatedAt.UTC())
	if age <= 0 {
		return 1
	}
	if age >= adaptiveTokenProfileRetention {
		return 0
	}
	return math.Pow(0.5, age.Seconds()/adaptiveTokenRateHalfLife.Seconds())
}

func adaptiveTokenCompletionP90(profile *persistedAdaptiveTokenUsageProfile) float64 {
	if profile == nil || profile.Samples <= 0 || len(profile.CompletionBuckets) == 0 {
		return 0
	}
	target := profile.Samples * 0.9
	if target == 0 {
		target = 1
	}
	seen := 0.0
	for index, count := range profile.CompletionBuckets {
		seen += count
		if seen < target {
			continue
		}
		if index < len(adaptiveTokenCompletionBuckets) {
			return float64(adaptiveTokenCompletionBuckets[index])
		}
		return adaptiveShadowMaximumOutputTokens
	}
	return adaptiveShadowMaximumOutputTokens
}

func normalizeAdaptiveTokenCalibrationState(state *persistedUsageState) {
	if state == nil {
		return
	}
	if state.AdaptiveTokenUsageProfiles == nil {
		state.AdaptiveTokenUsageProfiles = make(map[string]*persistedAdaptiveTokenUsageProfile)
	}
	if state.AdaptiveTokenWindowProfiles == nil {
		state.AdaptiveTokenWindowProfiles = make(map[string]*persistedAdaptiveTokenWindowProfile)
	}
	for key, profile := range state.AdaptiveTokenUsageProfiles {
		if profile == nil || profile.AuthIndex == "" || profile.Provider == "" || profile.Model == "" {
			delete(state.AdaptiveTokenUsageProfiles, key)
			continue
		}
		if len(profile.CompletionBuckets) != len(adaptiveTokenCompletionBuckets)+1 {
			normalized := make([]float64, len(adaptiveTokenCompletionBuckets)+1)
			copy(normalized, profile.CompletionBuckets)
			profile.CompletionBuckets = normalized
		}
		if !adaptiveTokenFiniteNonNegative(profile.Samples) ||
			!adaptiveTokenFiniteNonNegative(profile.InputTokens) ||
			!adaptiveTokenFiniteNonNegative(profile.OutputTokens) {
			delete(state.AdaptiveTokenUsageProfiles, key)
			continue
		}
		for _, bucket := range profile.CompletionBuckets {
			if !adaptiveTokenFiniteNonNegative(bucket) {
				delete(state.AdaptiveTokenUsageProfiles, key)
				break
			}
		}
	}
	for key, profile := range state.AdaptiveTokenWindowProfiles {
		if profile == nil || profile.AuthIndex == "" || profile.Provider == "" || profile.Model == "" ||
			profile.WindowKind == "" || !adaptiveTokenFiniteNonNegative(profile.EffectiveIntervals) ||
			!adaptiveTokenFiniteNonNegative(profile.CoverageSeconds) ||
			!adaptiveTokenFiniteNonNegative(profile.EffectiveTokenUnits) ||
			!adaptiveTokenFiniteNonNegative(profile.AttributedDropPercent) {
			delete(state.AdaptiveTokenWindowProfiles, key)
		}
	}
	boundAdaptiveTokenCalibrationProfiles(state)
}

func boundAdaptiveTokenCalibrationProfiles(state *persistedUsageState) {
	if state == nil {
		return
	}
	type profileAge struct {
		key       string
		updatedAt time.Time
	}
	if len(state.AdaptiveTokenUsageProfiles) > adaptiveTokenMaximumUsageProfiles {
		profiles := make([]profileAge, 0, len(state.AdaptiveTokenUsageProfiles))
		for key, profile := range state.AdaptiveTokenUsageProfiles {
			if profile != nil {
				profiles = append(profiles, profileAge{key: key, updatedAt: profile.UpdatedAt})
			}
		}
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].updatedAt.Before(profiles[j].updatedAt) })
		for index := 0; index < len(profiles)-adaptiveTokenMaximumUsageProfiles; index++ {
			delete(state.AdaptiveTokenUsageProfiles, profiles[index].key)
			state.AdaptiveTokenDroppedProfiles++
		}
		state.AdaptiveTokenCalibrationSaturated = true
	}
	if len(state.AdaptiveTokenWindowProfiles) > adaptiveTokenMaximumWindowProfiles {
		profiles := make([]profileAge, 0, len(state.AdaptiveTokenWindowProfiles))
		for key, profile := range state.AdaptiveTokenWindowProfiles {
			if profile != nil {
				profiles = append(profiles, profileAge{key: key, updatedAt: profile.UpdatedAt})
			}
		}
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].updatedAt.Before(profiles[j].updatedAt) })
		for index := 0; index < len(profiles)-adaptiveTokenMaximumWindowProfiles; index++ {
			delete(state.AdaptiveTokenWindowProfiles, profiles[index].key)
			state.AdaptiveTokenDroppedProfiles++
		}
		state.AdaptiveTokenCalibrationSaturated = true
	}
}

func pruneAdaptiveTokenCalibrationState(state *persistedUsageState, reference time.Time) {
	if state == nil {
		return
	}
	cutoff := reference.UTC().Add(-adaptiveTokenProfileRetention)
	for key, profile := range state.AdaptiveTokenUsageProfiles {
		if profile == nil || !profile.UpdatedAt.After(cutoff) {
			delete(state.AdaptiveTokenUsageProfiles, key)
		}
	}
	for key, profile := range state.AdaptiveTokenWindowProfiles {
		if profile == nil || !profile.UpdatedAt.After(cutoff) {
			delete(state.AdaptiveTokenWindowProfiles, key)
		}
	}
	if len(state.AdaptiveTokenUsageProfiles) < adaptiveTokenMaximumUsageProfiles &&
		len(state.AdaptiveTokenWindowProfiles) < adaptiveTokenMaximumWindowProfiles {
		state.AdaptiveTokenCalibrationSaturated = false
	}
}

func adaptiveTokenCalibrationSummary(authIndexes []string, now time.Time) adaptiveTokenCalibrationPublicView {
	allowed := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
			allowed[authIndex] = struct{}{}
		}
	}
	filter := authIndexes != nil
	view := adaptiveTokenCalibrationPublicView{
		Status: "collecting",
		Note:   "Token calibration uses bounded numeric usage counters and existing quota snapshots; prompts are not stored and polling cadence is unchanged.",
	}
	accounts := make(map[string]struct{})
	windows := make(map[string]*adaptiveTokenWindowPublicView)
	bravoUsageState.mu.RLock()
	for _, profile := range bravoUsageState.state.AdaptiveTokenUsageProfiles {
		if profile == nil || filter && !adaptiveTokenAllowed(allowed, profile.AuthIndex) {
			continue
		}
		accounts[profile.AuthIndex] = struct{}{}
		view.TrackedUsageProfiles++
	}
	for _, profile := range bravoUsageState.state.AdaptiveTokenWindowProfiles {
		if profile == nil || filter && !adaptiveTokenAllowed(allowed, profile.AuthIndex) {
			continue
		}
		accounts[profile.AuthIndex] = struct{}{}
		view.TrackedWindowProfiles++
		windowKey := strings.Join([]string{profile.Provider, profile.WindowKind, profile.QuotaModel}, "\x1f")
		window := windows[windowKey]
		if window == nil {
			window = &adaptiveTokenWindowPublicView{
				Provider: profile.Provider, WindowKind: profile.WindowKind, QuotaModel: profile.QuotaModel,
				Confidence: "collecting",
			}
			windows[windowKey] = window
		}
		window.Profiles++
		decay := adaptiveTokenReadDecay(profile.UpdatedAt, now)
		effectiveIntervals := profile.EffectiveIntervals * decay
		coverageSeconds := profile.CoverageSeconds * decay
		window.EffectiveIntervals += effectiveIntervals
		window.CoverageSeconds += coverageSeconds
		window.EffectiveTokenUnits += profile.EffectiveTokenUnits * decay
		window.AttributedDropPercent += profile.AttributedDropPercent * decay
		if profile.IntervalSamples >= adaptiveTokenMinimumWindowIntervals &&
			effectiveIntervals >= adaptiveTokenMinimumEffectiveWindows &&
			coverageSeconds >= adaptiveTokenMinimumCoverage.Seconds() {
			view.ReadyWindowProfiles++
			window.ReadyProfiles++
		}
	}
	view.DroppedProfiles = bravoUsageState.state.AdaptiveTokenDroppedProfiles
	view.PersistedSaturated = bravoUsageState.state.AdaptiveTokenCalibrationSaturated
	bravoUsageState.mu.RUnlock()
	view.TrackedAccounts = len(accounts)
	adaptiveTokenRuntime.Lock()
	for authIndex, account := range adaptiveTokenRuntime.Accounts {
		if filter && !adaptiveTokenAllowed(allowed, authIndex) {
			continue
		}
		view.RuntimeQueuedEvents += len(account.Events)
		if account.Saturated {
			view.RuntimeSaturatedAccounts++
		}
	}
	view.DroppedEvents = adaptiveTokenRuntime.DroppedEvents
	view.DroppedLateEvents = adaptiveTokenRuntime.DroppedLateEvents
	view.DroppedCapacityEvents = adaptiveTokenRuntime.DroppedCapacityEvents
	view.RuntimeSaturated = adaptiveTokenRuntime.Saturated
	adaptiveTokenRuntime.Unlock()
	if view.ReadyWindowProfiles > 0 {
		view.Status = "available"
	}
	if view.PersistedSaturated || view.RuntimeSaturated || view.RuntimeSaturatedAccounts > 0 ||
		view.DroppedEvents > 0 || view.DroppedProfiles > 0 {
		view.Status = "degraded"
	}
	view.Windows = make([]adaptiveTokenWindowPublicView, 0, len(windows))
	for _, window := range windows {
		if window.ReadyProfiles > 0 {
			window.Confidence = "available"
		}
		if window.EffectiveTokenUnits > 0 {
			window.DropPPPerMillionTokens = window.AttributedDropPercent / window.EffectiveTokenUnits * 1_000_000
		}
		window.EffectiveIntervals = adaptiveShadowRound(window.EffectiveIntervals)
		window.CoverageSeconds = adaptiveShadowRound(window.CoverageSeconds)
		window.EffectiveTokenUnits = adaptiveShadowRound(window.EffectiveTokenUnits)
		window.AttributedDropPercent = adaptiveShadowRound(window.AttributedDropPercent)
		window.DropPPPerMillionTokens = adaptiveShadowRound(window.DropPPPerMillionTokens)
		view.Windows = append(view.Windows, *window)
	}
	sort.Slice(view.Windows, func(i, j int) bool {
		left := strings.Join([]string{view.Windows[i].Provider, view.Windows[i].WindowKind, view.Windows[i].QuotaModel}, "\x1f")
		right := strings.Join([]string{view.Windows[j].Provider, view.Windows[j].WindowKind, view.Windows[j].QuotaModel}, "\x1f")
		return left < right
	})
	return view
}

func adaptiveTokenAllowed(allowed map[string]struct{}, authIndex string) bool {
	_, ok := allowed[strings.TrimSpace(authIndex)]
	return ok
}

func adaptiveTokenUsageProfileKey(authIndex, provider, model, effort, tariffID string) string {
	return adaptiveTokenDigest("usage", authIndex, provider, model, effort, tariffID)
}

func adaptiveTokenWindowProfileKey(authIndex, provider, model, effort, tariffID, kind, quotaModel string) string {
	return adaptiveTokenDigest("window", authIndex, provider, model, effort, tariffID, kind, quotaModel)
}

func adaptiveTokenDigest(parts ...string) string {
	normalized := make([]string, 0, len(parts)+1)
	normalized = append(normalized, adaptiveTokenKeyVersion)
	for _, part := range parts {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(part)))
	}
	digest := sha256.Sum256([]byte(strings.Join(normalized, "\x1f")))
	return fmt.Sprintf("at_%x", digest[:16])
}

func adaptiveTokenFiniteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func resetAdaptiveTokenCalibrationForTest() {
	adaptiveTokenRuntime.Lock()
	adaptiveTokenRuntime.Accounts = make(map[string]*adaptiveTokenEventAccount)
	adaptiveTokenRuntime.TotalEvents = 0
	adaptiveTokenRuntime.Saturated = false
	adaptiveTokenRuntime.DroppedEvents = 0
	adaptiveTokenRuntime.DroppedLateEvents = 0
	adaptiveTokenRuntime.DroppedCapacityEvents = 0
	adaptiveTokenRuntime.Unlock()
}
