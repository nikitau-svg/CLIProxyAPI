package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSubscriptionViewExposesAdaptiveCanarySummary(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "summary-auth", Provider: "claude"}
	cfg := demandGuardConfig("summary-owner", auth.AuthIndex)
	cfg.AllocatorMode = "enforce"
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	tariff := tariffConfig{ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 5, ReservationPercent: 0.1}
	quota := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-5 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 70}, Weekly: quotaWindowState{RemainingPercent: 59},
	}
	shape := adaptiveRequestShapeFor([]byte(`{"messages":[{"content":"work"}]}`), candidate{Model: "claude-fable-5", Effort: "xhigh"})
	key := adaptiveProfileKey(auth.AuthIndex, shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, auth.AuthIndex, shape)
	profile.Session = adaptiveWindowEstimate{LearnedScale: 3, ObservedBurnPerMin: 2}
	profile.Weekly = adaptiveWindowEstimate{LearnedScale: 2, ObservedBurnPerMin: 1}
	adaptiveReserveRuntime.Unlock()
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent[auth.AuthIndex] = 4
	allocatorRuntime.InFlightPercent[auth.AuthIndex] = 1.5
	allocatorRuntime.PendingRequests[auth.AuthIndex] = 3
	allocatorRuntime.InFlightRequests[auth.AuthIndex] = 2
	allocatorRuntime.Unlock()
	ownerRelease := bravoProjectDemand.begin(executionAttempt{
		ProjectID: "summary-owner", Primary: true, Auth: auth, ReservationPercent: 1,
	}, now)
	ownerRelease(true, now)

	view := buildSubscriptionView(cfg, auth, subscriptionConfig{AuthIndex: auth.AuthIndex, Tariff: "x5"}, tariff, quota, []string{"summary-owner"})
	if view.Allocator.Mode != "enforce" || view.Allocator.ReservationPercent < 0.3 {
		t.Fatalf("allocator mode/reservation = %#v", view.Allocator)
	}
	if view.Allocator.Status != "amber" || view.Allocator.Reason != "adaptive_guard_active" ||
		view.Allocator.SessionHeadroomAfter >= view.Allocator.SessionHeadroomBefore {
		t.Fatalf("allocator decision summary = %#v", view.Allocator)
	}
	if view.Allocator.SnapshotAgeSeconds < 299 || view.Allocator.SessionExposureGuardPercent < 19.9 {
		t.Fatalf("snapshot age/burn guard = %#v", view.Allocator)
	}
	if view.Allocator.PendingPercent != 4 || view.Allocator.InFlightPercent != 1.5 || view.Allocator.DemandGuardPercent <= 0 {
		t.Fatalf("runtime guards = %#v", view.Allocator)
	}
	if view.Allocator.PendingRequestCount != 3 || view.Allocator.InFlightRequestCount != 2 {
		t.Fatalf("runtime request counts = %#v", view.Allocator)
	}
	if view.Allocator.Tempo1MRequestsPerMinute != 1 || view.Allocator.Tempo10MRequestsPerMinute != 0.1 ||
		view.Allocator.Tempo60MRequestsPerMinute != 1.0/60.0 {
		t.Fatalf("1/10/60 request tempo = %#v", view.Allocator)
	}
	if view.Allocator.EstimateMinPercent <= 0 || view.Allocator.EstimateMaxPercent < view.Allocator.EstimateMinPercent ||
		view.Allocator.SessionAdmissionCutoff <= tariff.SessionFloorPercent {
		t.Fatalf("estimate range/admission cutoff = %#v", view.Allocator)
	}
	if view.Allocator.ReasonMessageRU == "" || view.Allocator.RecoveryActionRU == "" {
		t.Fatalf("adaptive Russian explanation/action missing: %#v", view.Allocator)
	}
	raw, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, field := range []string{"reservation_percent", "estimate_min_percent", "estimate_max_percent", "snapshot_age_seconds", "session_burn_percent_per_minute", "demand_guard_percent", "pending_percent", "in_flight_percent", "pending_request_count", "in_flight_request_count", "tempo_1m_requests_per_minute", "tempo_10m_requests_per_minute", "tempo_60m_requests_per_minute", "session_admission_cutoff_percent", "mode", "status", "reason", "reason_message_ru", "recovery_action_ru", "session_headroom_before_percent", "session_headroom_after_percent"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("management JSON misses %q: %s", field, raw)
		}
	}
}

func TestSubscriptionViewExposesSaturationWithoutLedgerIdentities(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	const secretAuth = "private-auth-identity-must-not-leak"
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 9
	bravoUsageState.state.AdaptiveQuota.Pending[secretAuth] = &persistedAdaptivePendingState{Percent: 1, UpdatedAt: time.Now().UTC()}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)

	view := adaptiveSubscriptionRuntimeView(
		defaultPluginConfig(),
		pluginapi.HostAuthFileEntry{AuthIndex: "visible-account", Provider: "claude"},
		tariffConfig{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, ReservationPercent: 0.5},
		credentialQuotaState{Confidence: "confirmed", Session: quotaWindowState{RemainingPercent: 100}, Weekly: quotaWindowState{RemainingPercent: 100}},
		time.Now().UTC(),
	)
	if view.Status != "red" || view.Reason != "adaptive_ledger_saturated" || !view.AdaptiveLedgerSaturated ||
		view.RetainedLedgerAuthCount != 1 || view.OverflowLedgerAuthCount != 9 || view.ReasonMessageRU == "" || view.RecoveryActionRU == "" {
		t.Fatalf("saturation management view = %#v", view)
	}
	raw, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(raw), secretAuth) {
		t.Fatalf("management saturation view leaked auth identity: %s", raw)
	}
}

func TestSubscriptionViewExposesEstimatorSaturationDistinctFromLedger(t *testing.T) {
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "estimator-red", Provider: "claude"}
	adaptiveReserveRuntime.Lock()
	markAdaptiveSaturatedLocked(auth.AuthIndex, now)
	adaptiveReserveRuntime.Unlock()
	view := adaptiveSubscriptionRuntimeView(
		defaultPluginConfig(), auth,
		tariffConfig{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, ReservationPercent: 0.5},
		credentialQuotaState{Confidence: "confirmed", ConfirmedAt: now, Session: quotaWindowState{RemainingPercent: 100}, Weekly: quotaWindowState{RemainingPercent: 100}},
		now,
	)
	if !view.EstimatorSaturated || view.AdaptiveLedgerSaturated || view.Status != "red" || view.Reason != "estimator_saturated" {
		t.Fatalf("estimator saturation view = %#v", view)
	}
}

func TestEstimatorSaturationRecoveryRequiresReconciledUnobservedWork(t *testing.T) {
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	now := time.Now().UTC()
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", PhysicalModel: "claude-fable-5", Provider: "claude", CostMode: "unknown"}
	key := adaptiveProfileKey("recovery-auth", shape)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.SaturationGlobal = true
	adaptiveReserveRuntime.Saturated["recovery-auth"] = now
	adaptiveReserveRuntime.Buckets[key] = &adaptiveReserveProfile{AuthIndex: "recovery-auth", Shape: shape, UnobservedPercent: 2, UpdatedAt: now}
	adaptiveReserveRuntime.Unlock()
	if errReady := adaptiveEstimatorReadyForReset(); errReady == nil {
		t.Fatal("estimator saturation reset accepted unresolved work")
	}
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Buckets[key].UnobservedPercent = 0
	adaptiveReserveRuntime.Unlock()
	if errReady := adaptiveEstimatorReadyForReset(); errReady != nil {
		t.Fatalf("reconciled estimator could not reset: %v", errReady)
	}
	resetAdaptiveEstimatorSaturationAfterReconciliation()
	adaptiveReserveRuntime.Lock()
	global, markers := adaptiveReserveRuntime.SaturationGlobal, len(adaptiveReserveRuntime.Saturated)
	adaptiveReserveRuntime.Unlock()
	if global || markers != 0 {
		t.Fatalf("estimator recovery left global=%t markers=%d", global, markers)
	}
}

func TestAdaptiveManagementStatusUsesNearFloorBand(t *testing.T) {
	confirmed := credentialQuotaState{Confidence: "confirmed"}
	for _, testCase := range []struct {
		name, want      string
		session, weekly float64
		guard           float64
	}{
		{name: "fresh far headroom", want: "green", session: 50, weekly: 50},
		{name: "learned guard far headroom", want: "green", session: 30, weekly: 30, guard: 4},
		{name: "near floor", want: "amber", session: 5, weekly: 8, guard: 2},
		{name: "protected", want: "red", session: 0, weekly: 5, guard: 3},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			view := subscriptionAllocatorRuntimeView{
				SessionHeadroomAfter:        testCase.session,
				WeeklyHeadroomAfter:         testCase.weekly,
				SessionExposureGuardPercent: testCase.guard,
			}
			status, _ := adaptiveAllocatorStatus(pluginConfig{AllocatorMode: "enforce"}, confirmed, view)
			if status != testCase.want {
				t.Fatalf("status = %q, want %q for %#v", status, testCase.want, view)
			}
		})
	}
}

func TestAdaptiveManagementEstimateMaxIncludesBurstScale(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "summary-burst-auth", Provider: "claude"}
	tariff := tariffConfig{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 0.1}
	shape := adaptiveRequestShape{
		Provider: "claude", PhysicalModel: "claude-fable-5", ModelFamily: "fable",
		EffortBucket: "xhigh", Multiplier: 5, ContextBucket: "large", CostMode: "unknown",
	}
	key := adaptiveProfileKey(auth.AuthIndex, shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, auth.AuthIndex, shape)
	profile.Session = adaptiveWindowEstimate{LearnedScale: 2}
	profile.Weekly = adaptiveWindowEstimate{LearnedScale: 2}
	adaptiveReserveRuntime.Unlock()
	for index := 0; index < 8; index++ {
		recordAdaptiveReservationCommitForKey(auth.AuthIndex, key, 2, now.Add(-10*time.Second))
	}
	actual := adaptiveReservationForShape(auth, tariff, shape, now)
	view := adaptiveSubscriptionRuntimeView(
		pluginConfig{AllocatorMode: "enforce", Tariffs: []tariffConfig{tariff}}, auth, tariff,
		credentialQuotaState{Confidence: "confirmed", ConfirmedAt: now, Session: quotaWindowState{RemainingPercent: 100}, Weekly: quotaWindowState{RemainingPercent: 100}},
		now,
	)
	if view.EstimateMaxPercent < actual || view.ReservationPercent < actual {
		t.Fatalf("management estimate max %.3f reservation %.3f below actual burst estimate %.3f", view.EstimateMaxPercent, view.ReservationPercent, actual)
	}
}

func TestAdaptiveManagementReadIsPureAndDoesNotPinEstimatorBuckets(t *testing.T) {
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "pure-management", Provider: "claude"}
	shape := adaptiveRequestShape{Provider: "claude", PhysicalModel: "claude-fable-5", ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large", CostMode: "tools", Multiplier: 4}
	key := adaptiveProfileKey(auth.AuthIndex, shape)
	updatedAt := now.Add(-time.Hour)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Buckets[key] = &adaptiveReserveProfile{AuthIndex: auth.AuthIndex, Shape: shape, UpdatedAt: updatedAt, Session: adaptiveWindowEstimate{LearnedScale: 3}}
	beforeBuckets, beforeMarkers, beforeGlobal := len(adaptiveReserveRuntime.Buckets), len(adaptiveReserveRuntime.Saturated), adaptiveReserveRuntime.SaturationGlobal
	adaptiveReserveRuntime.Unlock()
	quota := adaptivePersistenceQuota(90, now)
	for index := 0; index < 20; index++ {
		_ = adaptiveSubscriptionRuntimeView(defaultPluginConfig(), auth, tariffConfig{ID: "x1", ReservationPercent: 0.1}, quota, now)
	}
	adaptiveReserveRuntime.Lock()
	afterBuckets, afterMarkers, afterGlobal := len(adaptiveReserveRuntime.Buckets), len(adaptiveReserveRuntime.Saturated), adaptiveReserveRuntime.SaturationGlobal
	afterUpdatedAt := adaptiveReserveRuntime.Buckets[key].UpdatedAt
	adaptiveReserveRuntime.Unlock()
	if beforeBuckets != afterBuckets || beforeMarkers != afterMarkers || beforeGlobal != afterGlobal || !afterUpdatedAt.Equal(updatedAt) {
		t.Fatalf("management GET mutated estimator buckets %d→%d markers %d→%d global %t→%t updated %s→%s", beforeBuckets, afterBuckets, beforeMarkers, afterMarkers, beforeGlobal, afterGlobal, updatedAt, afterUpdatedAt)
	}
}

func TestAdaptiveManagementUsesStrictestModelWeeklyAndExpiredFreshness(t *testing.T) {
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	resetAdaptiveStatusRuntime()
	defer resetAdaptiveStatusRuntime()
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "strict-model-view", Provider: "claude"}
	shape := adaptiveRequestShape{Provider: "claude", PhysicalModel: "claude-fable-5", ModelFamily: "fable", CostMode: "unknown", Multiplier: 1}
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Buckets[adaptiveProfileKey(auth.AuthIndex, shape)] = &adaptiveReserveProfile{AuthIndex: auth.AuthIndex, Shape: shape, UpdatedAt: now}
	adaptiveReserveRuntime.Unlock()
	cfg := defaultPluginConfig()
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	cfg.QuotaUsageMaxStaleSeconds = 15 * 60
	tariff := tariffConfig{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, ReservationPercent: 0.1}
	quota := adaptivePersistenceQuota(90, now)
	quota.ModelWeekly = []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 20}}}
	view := adaptiveSubscriptionRuntimeView(cfg, auth, tariff, quota, now)
	if view.Status != "red" || view.WeeklyHeadroomAfter >= 0 {
		t.Fatalf("model-specific exhausted window displayed as %#v", view)
	}
	quota.ModelWeekly = nil
	quota.ConfirmedAt, quota.RefreshedAt = now.Add(-20*time.Minute), now.Add(-20*time.Minute)
	resetAdaptiveStatusRuntime()
	view = adaptiveSubscriptionRuntimeView(cfg, auth, tariff, quota, now)
	if view.Status != "red" || view.QuotaFreshness != quotaFreshnessExpired || view.Reason != "quota_unknown" {
		t.Fatalf("expired confirmed quota displayed as %#v", view)
	}
}

func TestAdaptiveStatusHysteresisTransitionSequence(t *testing.T) {
	resetAdaptiveStatusRuntime()
	defer resetAdaptiveStatusRuntime()
	quota := credentialQuotaState{Confidence: "confirmed"}
	cfg := pluginConfig{AllocatorMode: "enforce"}
	view := subscriptionAllocatorRuntimeView{AuthIndex: "hysteresis-auth", QuotaFreshness: quotaFreshnessFresh}
	for _, step := range []struct {
		headroom float64
		want     string
	}{{4.99, "amber"}, {5.01, "amber"}, {6.99, "amber"}, {7.01, "green"}, {0, "red"}, {1.99, "red"}, {2.01, "amber"}} {
		view.SessionHeadroomAfter, view.WeeklyHeadroomAfter = step.headroom, step.headroom
		status, _ := adaptiveAllocatorStatus(cfg, quota, view)
		if status != step.want {
			t.Fatalf("headroom %.2f transition status=%q want=%q", step.headroom, status, step.want)
		}
	}
}
