package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveTokenUsageUsesProviderAwareActualCounters(t *testing.T) {
	claudeInput, claudeOutput, claudeUnits := adaptiveTokenUsageUnits("claude", pluginapi.UsageDetail{
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheCreationTokens: 30,
	})
	if claudeInput != 100 || claudeOutput != 20 || claudeUnits != 200 {
		t.Fatalf("Claude units = input %d output %d total %.0f, want 100/20/200", claudeInput, claudeOutput, claudeUnits)
	}

	codexInput, codexOutput, codexUnits := adaptiveTokenUsageUnits("codex", pluginapi.UsageDetail{
		InputTokens: 100, OutputTokens: 20, ReasoningTokens: 50, CachedTokens: 40, TotalTokens: 170,
	})
	if codexInput != 100 || codexOutput != 50 || codexUnits != 170 {
		t.Fatalf("Codex units = input %d output %d total %.0f, want 100/50/170", codexInput, codexOutput, codexUnits)
	}
}

func TestAdaptiveTokenCalibrationLearnsIndependentWindowRates(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)

	const authIndex = "token-calibration-auth"
	const model = "claude-fable-5"
	t0 := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	quota := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: t0,
		Session: quotaWindowState{RemainingPercent: 80},
		Weekly:  quotaWindowState{RemainingPercent: 80},
		ModelWeekly: []modelQuotaWindowState{{
			Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 60},
		}},
	}
	installQuotaRefreshTestState(t, authIndex, quota)

	for interval := 0; interval < adaptiveTokenMinimumWindowIntervals; interval++ {
		previousAt := t0.Add(time.Duration(interval) * 10 * time.Minute)
		observedAt := previousAt.Add(10 * time.Minute)
		for sample := 0; sample < 2; sample++ {
			bravoUsageState.record(pluginapi.UsageRecord{
				Generate: true, Provider: "claude", AuthIndex: authIndex, Model: model,
				ReasoningEffort: "max", RequestedAt: previousAt.Add(time.Duration(sample+1) * time.Minute),
				Latency: time.Second,
				Detail:  pluginapi.UsageDetail{InputTokens: 3500, OutputTokens: 1500, TotalTokens: 5000},
			})
		}
		previous := credentialQuotaState{
			Confidence: "confirmed", Provider: "claude", ConfirmedAt: previousAt,
			Session: quotaWindowState{RemainingPercent: 80 - float64(interval)},
			Weekly:  quotaWindowState{RemainingPercent: 80 - 0.1*float64(interval)},
			ModelWeekly: []modelQuotaWindowState{{
				Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 60 - 0.5*float64(interval)},
			}},
		}
		refreshed := credentialQuotaState{
			Confidence: "confirmed", Provider: "claude", ConfirmedAt: observedAt,
			Session: quotaWindowState{RemainingPercent: previous.Session.RemainingPercent - 1},
			Weekly:  quotaWindowState{RemainingPercent: previous.Weekly.RemainingPercent - 0.1},
			ModelWeekly: []modelQuotaWindowState{{
				Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: previous.ModelWeekly[0].RemainingPercent - 0.5},
			}},
		}
		reconcileAdaptiveTokenCalibration(cfg, authIndex, previous, refreshed, previousAt, observedAt)
	}

	testAt := t0.Add(41 * time.Minute)
	features := adaptiveShadowRequestFeatures{
		InputTokens: 1000, DeclaredOutputTokens: 4096, EstimatedTokens: 5096,
		ContextFactor: 1, OutputTrusted: true,
	}
	subscription := subscriptionPolicy(cfg, authIndex)
	tariff := effectiveTariff(cfg, subscription, "claude", quota)
	estimate := adaptiveShadowEstimateFor(
		cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		candidate{Provider: "claude", Model: model, Effort: "max"},
		tariff, quota, features, testAt,
	)
	if !strings.HasPrefix(estimate.Confidence, "token_calibrated_") {
		bravoUsageState.mu.RLock()
		usageProfiles := make(map[string]persistedAdaptiveTokenUsageProfile, len(bravoUsageState.state.AdaptiveTokenUsageProfiles))
		for key, profile := range bravoUsageState.state.AdaptiveTokenUsageProfiles {
			if profile != nil {
				usageProfiles[key] = *profile
			}
		}
		windowProfiles := make(map[string]persistedAdaptiveTokenWindowProfile, len(bravoUsageState.state.AdaptiveTokenWindowProfiles))
		for key, profile := range bravoUsageState.state.AdaptiveTokenWindowProfiles {
			if profile != nil {
				windowProfiles[key] = *profile
			}
		}
		bravoUsageState.mu.RUnlock()
		t.Fatalf("estimate confidence = %q tariff=%q usage=%#v windows=%#v", estimate.Confidence, tariff.ID,
			usageProfiles, windowProfiles)
	}
	if estimate.PredictedTokens != 3048 {
		t.Fatalf("predicted tokens = %.0f, want input 1000 + completion p90 bucket 2048", estimate.PredictedTokens)
	}
	if !(estimate.SessionReservationPercent > estimate.ModelWeeklyReservationPercent &&
		estimate.ModelWeeklyReservationPercent > estimate.WeeklyReservationPercent) {
		t.Fatalf("window estimates are not independent: session=%.6f model=%.6f weekly=%.6f",
			estimate.SessionReservationPercent, estimate.ModelWeeklyReservationPercent, estimate.WeeklyReservationPercent)
	}
	if estimate.ReservationPercent >= adaptiveShadowMaximumReservationPercent {
		t.Fatalf("token estimate remained pinned to legacy maximum: %#v", estimate)
	}
	if estimate.SessionReservationPercent <= 0 || estimate.WeeklyReservationPercent <= 0 ||
		estimate.ModelWeeklyReservationPercent <= 0 {
		t.Fatalf("non-positive calibrated estimate: %#v", estimate)
	}
	if !estimate.SessionTokenCalibrated || !estimate.WeeklyTokenCalibrated ||
		!estimate.ModelWeeklyTokenCalibrated {
		t.Fatalf("window-specific calibration proof missing: %#v", estimate)
	}

	view := adaptiveTokenCalibrationSummary([]string{authIndex}, testAt)
	if view.Status != "available" || view.ReadyWindowProfiles != 3 || view.TrackedUsageProfiles != 1 || len(view.Windows) != 3 {
		t.Fatalf("token calibration summary = %#v", view)
	}
	rates := make(map[string]float64, len(view.Windows))
	for _, window := range view.Windows {
		rates[window.WindowKind] = window.DropPPPerMillionTokens
	}
	if !(rates[pluginapi.HostAuthQuotaWindowKindSession] > rates[pluginapi.HostAuthQuotaWindowKindModelWeekly] &&
		rates[pluginapi.HostAuthQuotaWindowKindModelWeekly] > rates[pluginapi.HostAuthQuotaWindowKindWeekly]) {
		t.Fatalf("public per-window rates were combined or reordered: %#v", view.Windows)
	}
	encoded, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), authIndex) {
		t.Fatalf("public token summary leaked credential identity: %s", encoded)
	}
}

func TestAdaptiveTokenCalibrationHonorsRefreshWatermark(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)

	t0 := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	before := adaptiveTokenUsageEvent{
		CompletedAt: t0.Add(time.Minute), AuthIndex: "watermark", Provider: "claude",
		Model: "claude-sonnet-5", TariffID: "x1", TokenUnits: 1000,
	}
	after := before
	after.CompletedAt = t0.Add(3 * time.Minute)
	adaptiveTokenRuntime.Lock()
	adaptiveTokenRuntime.Accounts["watermark"] = &adaptiveTokenEventAccount{Events: []adaptiveTokenUsageEvent{before, after}}
	adaptiveTokenRuntime.TotalEvents = 2
	adaptiveTokenRuntime.Unlock()

	previous := credentialQuotaState{Provider: "claude", Session: quotaWindowState{RemainingPercent: 80}}
	refreshed := credentialQuotaState{Provider: "claude", Session: quotaWindowState{RemainingPercent: 79}}
	reconcileAdaptiveTokenCalibration(cfg, "watermark", previous, refreshed, t0, t0.Add(2*time.Minute))

	adaptiveTokenRuntime.Lock()
	remaining := append([]adaptiveTokenUsageEvent(nil), adaptiveTokenRuntime.Accounts["watermark"].Events...)
	adaptiveTokenRuntime.Unlock()
	if len(remaining) != 1 || !remaining[0].CompletedAt.Equal(after.CompletedAt) {
		t.Fatalf("post-observation event was not retained: %#v", remaining)
	}
	bravoUsageState.mu.RLock()
	profile := bravoUsageState.state.AdaptiveTokenWindowProfiles[adaptiveTokenWindowProfileKey(
		"watermark", "claude", before.Model, "", "x1", pluginapi.HostAuthQuotaWindowKindSession, "",
	)]
	bravoUsageState.mu.RUnlock()
	if profile == nil || profile.EffectiveTokenUnits != 1000 || profile.AttributedDropPercent != 1 {
		t.Fatalf("covered calibration = %#v", profile)
	}
}

func TestAdaptiveTokenCalibrationDropsLateUsageInsteadOfLoweringNewIntervalRate(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)
	t0 := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	late := adaptiveTokenUsageEvent{
		CompletedAt: t0.Add(-time.Minute), AuthIndex: "late-usage", Provider: "claude",
		Model: "claude-sonnet-5", TariffID: "x1", TokenUnits: 1200,
	}
	adaptiveTokenRuntime.Lock()
	adaptiveTokenRuntime.Accounts["late-usage"] = &adaptiveTokenEventAccount{Events: []adaptiveTokenUsageEvent{late}}
	adaptiveTokenRuntime.TotalEvents = 1
	adaptiveTokenRuntime.Unlock()
	covered := reconcileAdaptiveTokenCalibration(cfg, "late-usage",
		credentialQuotaState{Provider: "claude", Session: quotaWindowState{RemainingPercent: 80}},
		credentialQuotaState{Provider: "claude", Session: quotaWindowState{RemainingPercent: 79}},
		t0, t0.Add(2*time.Minute),
	)
	adaptiveTokenRuntime.Lock()
	dropped := adaptiveTokenRuntime.DroppedEvents
	adaptiveTokenRuntime.Unlock()
	if len(covered) != 0 || dropped != 1 {
		t.Fatalf("late event contaminated a newer interval: covered=%#v dropped=%d", covered, dropped)
	}
}

func TestAdaptiveShadowUsesWindowSpecificPendingInsteadOfOneScalar(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	recordAdaptiveShadowCommitValue("window-vector", adaptiveShadowCommit{
		At: now, Percent: 4, SessionPercent: 0.1, WeeklyPercent: 4, Model: "claude-sonnet-5",
	})
	attempt := executionAttempt{
		Auth:      pluginapi.HostAuthFileEntry{AuthIndex: "window-vector", Provider: "claude"},
		Candidate: candidate{Provider: "claude", Model: "claude-sonnet-5"}, Primary: true,
		AdaptiveSessionReservationPercent: 0.1,
		AdaptiveWeeklyReservationPercent:  0.1,
	}
	quota := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 3}, Weekly: quotaWindowState{RemainingPercent: 10},
	}
	decision, pending, before, after := adaptiveShadowDecisionFor(cfg, attempt, quota, tariffConfig{}, 4, now)
	if decision != adaptiveShadowDecisionAdmit || pending != 4 || before <= 0 || after <= 0 {
		t.Fatalf("vector decision = %s pending=%.2f before=%.2f after=%.2f", decision, pending, before, after)
	}
}

func TestAdaptiveShadowOverflowKeepsWindowPendingIndependent(t *testing.T) {
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)
	now := time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)
	for index := 0; index < adaptiveShadowMaximumCommitsPerAccount+32; index++ {
		recordAdaptiveShadowCommitValue("overflow-vector", adaptiveShadowCommit{
			At: now, Percent: 1, SessionPercent: 0.01, WeeklyPercent: 1,
			Model: "claude-fable-5", ModelWeeklyName: "fable", ModelWeeklyPercent: 0.5,
		})
	}
	session := adaptiveShadowEffectivePendingForWindow(
		"overflow-vector", pluginapi.HostAuthQuotaWindowKindSession, "", cfg, now,
	)
	weekly := adaptiveShadowEffectivePendingForWindow(
		"overflow-vector", pluginapi.HostAuthQuotaWindowKindWeekly, "", cfg, now,
	)
	modelWeekly := adaptiveShadowEffectivePendingForWindow(
		"overflow-vector", pluginapi.HostAuthQuotaWindowKindModelWeekly, "fable", cfg, now,
	)
	wantRequests := float64(adaptiveShadowMaximumCommitsPerAccount + 32)
	if math.Abs(session-wantRequests*0.01) > 0.000001 ||
		math.Abs(weekly-wantRequests) > 0.000001 ||
		math.Abs(modelWeekly-wantRequests*0.5) > 0.000001 {
		t.Fatalf("overflow mixed quota windows: session=%.4f weekly=%.4f model=%.4f",
			session, weekly, modelWeekly)
	}
}

func TestAdaptiveTokenCalibrationPersistsAcrossRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "usage-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	now := time.Date(2026, 8, 14, 11, 0, 0, 0, time.UTC)
	usageKey := adaptiveTokenUsageProfileKey("restart-auth", "claude", "claude-fable-5", "max", "x5")
	windowKey := adaptiveTokenWindowProfileKey(
		"restart-auth", "claude", "claude-fable-5", "max", "x5",
		pluginapi.HostAuthQuotaWindowKindWeekly, "",
	)
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenUsageProfiles[usageKey] = &persistedAdaptiveTokenUsageProfile{
		AuthIndex: "restart-auth", Provider: "claude", Model: "claude-fable-5", Effort: "max", TariffID: "x5",
		SampleCount: 9, Samples: 9, InputTokens: 9000, OutputTokens: 1800,
		CompletionBuckets: []float64{0, 0, 0, 0, 9}, UpdatedAt: now,
	}
	bravoUsageState.state.AdaptiveTokenWindowProfiles[windowKey] = &persistedAdaptiveTokenWindowProfile{
		AuthIndex: "restart-auth", Provider: "claude", Model: "claude-fable-5", Effort: "max", TariffID: "x5",
		WindowKind: pluginapi.HostAuthQuotaWindowKindWeekly, IntervalSamples: 5, EffectiveIntervals: 5,
		CoverageSeconds: 3600, EffectiveTokenUnits: 10800, AttributedDropPercent: 1.5, UpdatedAt: now,
	}
	bravoUsageState.mu.Unlock()
	flushUsageState()

	loaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if loaded.AdaptiveTokenUsageProfiles[usageKey] == nil || loaded.AdaptiveTokenWindowProfiles[windowKey] == nil {
		t.Fatalf("token profiles did not survive restart: usage=%#v window=%#v",
			loaded.AdaptiveTokenUsageProfiles[usageKey], loaded.AdaptiveTokenWindowProfiles[windowKey])
	}
	if loaded.AdaptiveTokenWindowProfiles[windowKey].AttributedDropPercent != 1.5 {
		t.Fatalf("persisted calibration changed: %#v", loaded.AdaptiveTokenWindowProfiles[windowKey])
	}
}

func TestAdaptiveTokenActualUsageReplacesPredictedProjectWeight(t *testing.T) {
	commits := []adaptiveShadowCommit{
		{ProjectID: "prj_large", Provider: "claude", Model: "claude-sonnet-5", TariffID: "x1", Percent: 5, TokenUnits: 5000},
		{ProjectID: "prj_small", Provider: "claude", Model: "claude-sonnet-5", TariffID: "x1", Percent: 5, TokenUnits: 5000},
	}
	events := []adaptiveTokenUsageEvent{
		{ProjectID: "prj_large", Provider: "claude", Model: "claude-sonnet-5", TariffID: "x1", TokenUnits: 9000},
		{ProjectID: "prj_small", Provider: "claude", Model: "claude-sonnet-5", TariffID: "x1", TokenUnits: 1000},
	}
	weighted := applyAdaptiveTokenWeightsToShadowCommits(commits, events)
	if weighted[0].TokenUnits != 9000 || weighted[1].TokenUnits != 1000 {
		t.Fatalf("actual token weights = %#v", weighted)
	}
	if commits[0].TokenUnits != 5000 || commits[1].TokenUnits != 5000 {
		t.Fatalf("weighting mutated the audit commitments in place: %#v", commits)
	}
}

func TestAdaptiveTokenRuntimeSaturationFallsBackWithoutRoutingEffect(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	installQuotaRefreshTestState(t, "saturated-auth", credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	})
	adaptiveTokenRuntime.Lock()
	adaptiveTokenRuntime.Accounts["saturated-auth"] = &adaptiveTokenEventAccount{
		Events: make([]adaptiveTokenUsageEvent, adaptiveTokenMaximumEventsPerAccount),
	}
	adaptiveTokenRuntime.TotalEvents = adaptiveTokenMaximumEventsPerAccount
	adaptiveTokenRuntime.Unlock()
	_, accepted := buildAdaptiveTokenUsageEvent(pluginapi.UsageRecord{
		Generate: true, Provider: "claude", AuthIndex: "saturated-auth", Model: "claude-sonnet-5",
		RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 10},
	})
	if accepted {
		t.Fatal("runtime ledger accepted an event beyond its hard bound")
	}
	adaptiveTokenRuntime.Lock()
	account := adaptiveTokenRuntime.Accounts["saturated-auth"]
	dropped := adaptiveTokenRuntime.DroppedEvents
	adaptiveTokenRuntime.Unlock()
	if len(account.Events) != adaptiveTokenMaximumEventsPerAccount || !account.Saturated || dropped != 1 {
		t.Fatalf("runtime bound = events %d saturated=%v dropped=%d", len(account.Events), account.Saturated, dropped)
	}
	adaptiveTokenRuntime.Lock()
	adaptiveTokenRuntime.TotalEvents = adaptiveTokenMaximumRuntimeEvents
	adaptiveTokenRuntime.Unlock()
	_, accepted = buildAdaptiveTokenUsageEvent(pluginapi.UsageRecord{
		Generate: true, Provider: "claude", AuthIndex: "global-cap-auth", Model: "claude-sonnet-5",
		RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 10},
	})
	adaptiveTokenRuntime.Lock()
	_, globalLeaked := adaptiveTokenRuntime.Accounts["global-cap-auth"]
	globalSaturated := adaptiveTokenRuntime.Saturated
	adaptiveTokenRuntime.Unlock()
	if accepted || globalLeaked || !globalSaturated {
		t.Fatalf("global runtime cap accepted=%v leaked=%v saturated=%v", accepted, globalLeaked, globalSaturated)
	}

	features := buildAdaptiveShadowRequestFeatures([]byte(`{"max_tokens":64,"messages":[]}`))
	tariff := effectiveTariff(cfg, subscriptionPolicy(cfg, "saturated-auth"), "claude", credentialQuotaState{})
	estimate := adaptiveShadowEstimateFor(cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: "saturated-auth", Provider: "claude"},
		candidate{Provider: "claude", Model: "claude-sonnet-5"}, tariff,
		credentialQuotaState{}, features, now,
	)
	if !strings.HasPrefix(estimate.Confidence, "shape_estimate") {
		t.Fatalf("saturation did not degrade to the legacy shadow estimate: %#v", estimate)
	}
	if adaptiveShadowEffect(cfg) != "shadow_only" {
		t.Fatalf("observe mode unexpectedly changed routing effect: %q", adaptiveShadowEffect(cfg))
	}
}

func TestAdaptiveTokenPersistedProfilesAreHardBounded(t *testing.T) {
	state := newPersistedUsageState()
	now := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	for index := 0; index < adaptiveTokenMaximumUsageProfiles+1; index++ {
		state.AdaptiveTokenUsageProfiles["oversized-"+strconv.Itoa(index)] = &persistedAdaptiveTokenUsageProfile{
			AuthIndex: "auth", Provider: "claude", Model: "claude-sonnet-5",
			SampleCount: 1, Samples: 1, CompletionBuckets: make([]float64, len(adaptiveTokenCompletionBuckets)+1),
			UpdatedAt: now.Add(time.Duration(index) * time.Second),
		}
	}
	normalizeAdaptiveTokenCalibrationState(&state)
	if len(state.AdaptiveTokenUsageProfiles) != adaptiveTokenMaximumUsageProfiles ||
		!state.AdaptiveTokenCalibrationSaturated || state.AdaptiveTokenDroppedProfiles != 1 {
		t.Fatalf("persisted bound = profiles %d saturated=%v dropped=%d",
			len(state.AdaptiveTokenUsageProfiles), state.AdaptiveTokenCalibrationSaturated, state.AdaptiveTokenDroppedProfiles)
	}
	if _, retainedNewest := state.AdaptiveTokenUsageProfiles["oversized-"+strconv.Itoa(adaptiveTokenMaximumUsageProfiles)]; !retainedNewest {
		t.Fatal("hard bound discarded the newest profile instead of the oldest")
	}
}

func installAdaptiveTokenTestConfig(t *testing.T) pluginConfig {
	t.Helper()
	previous := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
	return cfg
}

func TestAdaptiveTokenCalibrationNeverProducesNonFiniteReservation(t *testing.T) {
	now := time.Now().UTC()
	state := newPersistedUsageState()
	key := adaptiveTokenWindowProfileKey("finite", "claude", "claude-sonnet-5", "", "x1", "session", "")
	state.AdaptiveTokenWindowProfiles[key] = &persistedAdaptiveTokenWindowProfile{
		AuthIndex: "finite", Provider: "claude", Model: "claude-sonnet-5", WindowKind: "session",
		IntervalSamples: adaptiveTokenMinimumWindowIntervals, EffectiveIntervals: adaptiveTokenMinimumWindowIntervals,
		CoverageSeconds:     2 * adaptiveTokenMinimumCoverage.Seconds(),
		EffectiveTokenUnits: 1, AttributedDropPercent: math.MaxFloat64, UpdatedAt: now,
	}
	estimate := adaptiveTokenWindowEstimateFromState(&state, "finite", "claude", "claude-sonnet-5", "", "x1", "session", "", 1e6, now)
	if !estimate.Available || math.IsNaN(estimate.Percent) || math.IsInf(estimate.Percent, 0) || estimate.Percent != adaptiveShadowMaximumReservationPercent {
		t.Fatalf("non-finite/cap handling = %#v", estimate)
	}
}

func TestAdaptiveTokenCalibrationAuditConfidenceRequiresBothEffectiveWindows(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	cfg := installAdaptiveTokenTestConfig(t)
	now := time.Date(2026, 8, 14, 13, 30, 0, 0, time.UTC)
	const authIndex = "partial-auth"
	const model = "claude-sonnet-5"
	quota := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 70},
	}
	tariff := effectiveTariff(cfg, subscriptionPolicy(cfg, authIndex), "claude", quota)
	usage := adaptiveTokenBenchmarkUsageProfile(authIndex, model, now)
	usage.TariffID = tariff.ID
	usageKey := adaptiveTokenUsageProfileKey(authIndex, "claude", model, "", tariff.ID)
	window := func(kind string) *persistedAdaptiveTokenWindowProfile {
		return &persistedAdaptiveTokenWindowProfile{
			AuthIndex: authIndex, Provider: "claude", Model: model, TariffID: tariff.ID,
			WindowKind: kind, IntervalSamples: 8, EffectiveIntervals: 8,
			CoverageSeconds: 3600, EffectiveTokenUnits: 100_000,
			AttributedDropPercent: 1, UpdatedAt: now,
		}
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenUsageProfiles[usageKey] = usage
	bravoUsageState.state.AdaptiveTokenWindowProfiles[adaptiveTokenWindowProfileKey(
		authIndex, "claude", model, "", tariff.ID, pluginapi.HostAuthQuotaWindowKindSession, "",
	)] = window(pluginapi.HostAuthQuotaWindowKindSession)
	bravoUsageState.mu.Unlock()
	features := adaptiveShadowRequestFeatures{InputTokens: 1000, DeclaredOutputTokens: 2048, OutputTrusted: true}
	estimate := func() adaptiveShadowEstimate {
		return adaptiveShadowEstimateFor(cfg,
			pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
			candidate{Provider: "claude", Model: model}, tariff, quota, features, now,
		)
	}
	partial := estimate()
	if !strings.HasPrefix(partial.Confidence, "partial_token_calibration_") ||
		strings.HasPrefix(partial.Confidence, "token_calibrated_") {
		t.Fatalf("one learned dimension entered complete audit cohort: %#v", partial)
	}
	if !partial.SessionTokenCalibrated || partial.WeeklyTokenCalibrated || partial.ModelWeeklyTokenCalibrated {
		t.Fatalf("partial estimate lost window-specific calibration state: %#v", partial)
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenWindowProfiles[adaptiveTokenWindowProfileKey(
		authIndex, "claude", model, "", tariff.ID, pluginapi.HostAuthQuotaWindowKindWeekly, "",
	)] = window(pluginapi.HostAuthQuotaWindowKindWeekly)
	bravoUsageState.mu.Unlock()
	complete := estimate()
	if complete.Confidence != "token_calibrated_complete" || !complete.SessionTokenCalibrated ||
		!complete.WeeklyTokenCalibrated {
		t.Fatalf("both effective dimensions did not enter complete audit cohort: %#v", complete)
	}
}

func TestAdaptiveTokenCalibrationEvidenceCoolsAtReadTime(t *testing.T) {
	now := time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC)
	state := newPersistedUsageState()
	key := adaptiveTokenWindowProfileKey("cooling", "claude", "claude-sonnet-5", "", "x1", "session", "")
	state.AdaptiveTokenWindowProfiles[key] = &persistedAdaptiveTokenWindowProfile{
		AuthIndex: "cooling", Provider: "claude", Model: "claude-sonnet-5", WindowKind: "session",
		IntervalSamples: 4, EffectiveIntervals: 4, CoverageSeconds: 3600,
		EffectiveTokenUnits: 10000, AttributedDropPercent: 1, UpdatedAt: now,
	}
	fresh := adaptiveTokenWindowEstimateFromState(&state, "cooling", "claude", "claude-sonnet-5", "", "x1", "session", "", 1000, now)
	cooled := adaptiveTokenWindowEstimateFromState(&state, "cooling", "claude", "claude-sonnet-5", "", "x1", "session", "", 1000, now.Add(24*time.Hour))
	if !fresh.Available || cooled.Available {
		t.Fatalf("read-time cooling fresh=%#v cooled=%#v", fresh, cooled)
	}
}

func TestAdaptiveTokenCalibrationReplaysLegacyOverReservationIncident(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := installAdaptiveTokenTestConfig(t)
	now := time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)
	quota := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 60}, Weekly: quotaWindowState{RemainingPercent: 60},
	}
	tariff := effectiveTariff(cfg, subscriptionPolicy(cfg, "incident-auth"), "claude", quota)
	usageKey := adaptiveTokenUsageProfileKey("incident-auth", "claude", "claude-fable-5", "max", tariff.ID)
	usage := adaptiveTokenBenchmarkUsageProfile("incident-auth", "claude-fable-5", now)
	usage.Effort = "max"
	usage.TariffID = tariff.ID
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenUsageProfiles[usageKey] = usage
	for _, fixture := range []struct {
		kind string
		drop float64
	}{
		{kind: pluginapi.HostAuthQuotaWindowKindSession, drop: 3},
		{kind: pluginapi.HostAuthQuotaWindowKindWeekly, drop: 0.3},
	} {
		key := adaptiveTokenWindowProfileKey("incident-auth", "claude", "claude-fable-5", "max", tariff.ID, fixture.kind, "")
		bravoUsageState.state.AdaptiveTokenWindowProfiles[key] = &persistedAdaptiveTokenWindowProfile{
			AuthIndex: "incident-auth", Provider: "claude", Model: "claude-fable-5", Effort: "max", TariffID: tariff.ID,
			WindowKind: fixture.kind, IntervalSamples: 8, EffectiveIntervals: 8,
			CoverageSeconds: 4 * 3600, EffectiveTokenUnits: 1_000_000,
			AttributedDropPercent: fixture.drop, UpdatedAt: now,
		}
	}
	bravoUsageState.mu.Unlock()
	features := adaptiveShadowRequestFeatures{
		InputTokens: 20_000, DeclaredOutputTokens: 65_536, EstimatedTokens: 85_536,
		ContextFactor: 4, OutputTrusted: true,
	}
	item := candidate{Provider: "claude", Model: "claude-fable-5", Effort: "max"}
	calibrated := adaptiveShadowEstimateFor(cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: "incident-auth", Provider: "claude"},
		item, tariff, quota, features, now,
	)
	cold := adaptiveShadowEstimateFor(cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: "cold-auth", Provider: "claude"},
		item, tariff, quota, features, now,
	)
	if !strings.HasPrefix(calibrated.Confidence, "token_calibrated_") ||
		calibrated.ReservationPercent >= cold.ReservationPercent/10 || calibrated.ReservationPercent <= 0 {
		t.Fatalf("incident replay calibrated=%#v cold=%#v", calibrated, cold)
	}
}
