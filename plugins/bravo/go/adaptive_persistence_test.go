package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptivePendingIsDurableBeforeSuccessfulReleaseReturns(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "restart-secondary", 56)

	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("restart-secondary", 3))
	if !acquired {
		t.Fatal("initial secondary lease was not acquired")
	}
	release(true)

	// The client may restart the container immediately after receiving success.
	// Do not rely on the ordinary usage-state debounce or plugin shutdown.
	raw, errRead := os.ReadFile(adaptiveWALPath(path))
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(raw), `"percent":3`) {
		t.Fatalf("accepted work was not synchronously persisted: %s", raw)
	}
	for _, forbidden := range []string{`"profiles"`, `"model_family"`, `"effort_bucket"`, `"context_bucket"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("hot-path WAL contains estimator detail %s: %s", forbidden, raw)
		}
	}

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("restart-secondary"); got != 3 {
		t.Fatalf("restored pending = %.3f, want 3", got)
	}
	if _, reopened := acquireAttemptLease(adaptivePersistenceAttempt("restart-secondary", 3)); reopened {
		t.Fatal("restart forgot accepted work and reopened the protected secondary")
	}
}

func TestAdaptiveLearnedProfilesSurviveRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shape := adaptiveRequestShape{
		Multiplier: 1, Provider: "claude", PhysicalModel: "claude-fable-5", ModelFamily: "fable",
		EffortBucket: "xhigh", ContextBucket: "large", CostMode: "uncached",
	}
	key := adaptiveProfileKey("learned-secondary", shape)
	recordAdaptiveReservationCommitForKey("learned-secondary", key, 5, now.Add(-time.Minute))
	observeAdaptiveQuotaRefresh(
		"learned-secondary",
		adaptivePersistenceQuota(70, now.Add(-10*time.Minute)),
		adaptivePersistenceQuota(50, now),
		5,
		now,
	)
	flushUsageState()

	adaptiveReserveRuntime.Lock()
	want := *adaptiveReserveRuntime.Buckets[key]
	adaptiveReserveRuntime.Unlock()
	if want.Session.LearnedScale <= 1 || want.Session.ObservedBurnPerMin <= 0 {
		t.Fatalf("profile did not learn before restart: %#v", want)
	}

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	adaptiveReserveRuntime.Lock()
	got := adaptiveReserveRuntime.Buckets[key]
	adaptiveReserveRuntime.Unlock()
	if got == nil || got.Session != want.Session || got.Weekly != want.Weekly || got.UnobservedPercent != 0 {
		t.Fatalf("restored learned profile = %#v, want session=%#v weekly=%#v", got, want.Session, want.Weekly)
	}
}

func TestAdaptiveExactModelAndCostBucketsSurviveRestartIndependently(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "exact-buckets.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	lowShape := adaptiveRequestShape{
		Multiplier: 1, Provider: "claude", PhysicalModel: "claude-fable-5-a", ModelFamily: "fable",
		EffortBucket: "xhigh", ContextBucket: "large", CostMode: "cache-read",
	}
	highShape := adaptiveRequestShape{
		Multiplier: 1, Provider: "claude", PhysicalModel: "claude-fable-5-b", ModelFamily: "fable",
		EffortBucket: "xhigh", ContextBucket: "large", CostMode: "uncached+tools",
	}
	lowKey := adaptiveProfileKey("exact-auth", lowShape)
	highKey := adaptiveProfileKey("exact-auth", highShape)
	adaptiveReserveRuntime.Lock()
	low := ensureAdaptiveBucketLocked(lowKey, "exact-auth", lowShape)
	low.Session, low.Weekly, low.UpdatedAt = adaptiveWindowEstimate{LearnedScale: 2}, adaptiveWindowEstimate{LearnedScale: 2}, now
	high := ensureAdaptiveBucketLocked(highKey, "exact-auth", highShape)
	high.Session, high.Weekly, high.UpdatedAt = adaptiveWindowEstimate{LearnedScale: 6}, adaptiveWindowEstimate{LearnedScale: 6}, now
	adaptiveReserveRuntime.Unlock()
	stageAdaptiveEstimatorState("exact-auth", now)
	flushUsageState()

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	adaptiveReserveRuntime.Lock()
	restoredLow := adaptiveReserveRuntime.Buckets[lowKey]
	restoredHigh := adaptiveReserveRuntime.Buckets[highKey]
	adaptiveReserveRuntime.Unlock()
	if restoredLow == nil || restoredHigh == nil || restoredLow.Shape != lowShape || restoredHigh.Shape != highShape ||
		restoredLow.Session.LearnedScale != 2 || restoredHigh.Session.LearnedScale != 6 {
		t.Fatalf("exact restored buckets low/high = %#v / %#v", restoredLow, restoredHigh)
	}
	tariff := tariffConfig{ID: "x1", ReservationPercent: 0.1}
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "exact-auth", Provider: "claude"}
	lowReservation := adaptiveReservationForShape(auth, tariff, lowShape, now.Add(time.Minute))
	highReservation := adaptiveReservationForShape(auth, tariff, highShape, now.Add(time.Minute))
	if highReservation <= lowReservation {
		t.Fatalf("exact learned reservations high/low = %.3f/%.3f", highReservation, lowReservation)
	}
}

func TestAdaptiveConfirmedWatermarkClearsOnlyCapturedPending(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "watermark-secondary", 80)

	first, acquired := acquireAttemptLease(adaptivePersistenceAttempt("watermark-secondary", 3))
	if !acquired {
		t.Fatal("first lease was not acquired")
	}
	first(true)
	watermark := pendingReservationPercent("watermark-secondary")

	second, acquired := acquireAttemptLease(adaptivePersistenceAttempt("watermark-secondary", 2))
	if !acquired {
		t.Fatal("second lease was not acquired")
	}
	second(true)
	completedAt := time.Now().UTC().Add(time.Minute)
	applyQuotaRefreshSuccess(
		"watermark-secondary", quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(70, completedAt), watermark, completedAt,
	)

	if got := pendingReservationPercent("watermark-secondary"); got != 2 {
		t.Fatalf("runtime pending after reconciliation = %.3f, want 2", got)
	}
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("watermark-secondary"); got != 2 {
		t.Fatalf("durable pending after reconciliation = %.3f, want 2", got)
	}
}

func TestAdaptivePersistenceLoadsOldV3AndBoundsUntrustedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	fixture := `{
		"schema_version": 3,
		"adaptive_quota": {
			"pending": {
				" ": {"percent": 50},
				"bounded-auth": {"percent": 999999}
			},
			"profiles": {
				"untrusted-key": {
					"auth_index": "bounded-auth",
					"model_family": "fable",
					"effort_bucket": "xhigh",
					"context_bucket": "large",
					"session": {"learned_scale": 999999, "observed_burn_per_minute": 999999},
					"weekly": {"learned_scale": 999999, "observed_burn_per_minute": 999999},
					"unobserved_percent": 999999
				}
			}
		}
	}`
	if errWrite := os.WriteFile(path, []byte(fixture), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	state, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if _, exists := state.AdaptiveQuota.Pending[" "]; exists {
		t.Fatal("blank auth key survived normalization")
	}
	got := state.AdaptiveQuota.Pending["bounded-auth"]
	if got == nil || got.Percent != 999999 {
		t.Fatalf("cumulative adaptive debt was truncated: %#v", got)
	}
	profileKey := adaptiveProfileKey("bounded-auth", adaptiveRequestShape{
		Provider: "legacy-unknown", PhysicalModel: "legacy-unknown", ModelFamily: "fable",
		EffortBucket: "xhigh", ContextBucket: "large", CostMode: "legacy-unknown",
	})
	profile := state.AdaptiveQuota.Profiles[profileKey]
	if profile == nil || profile.Session.LearnedScale != adaptiveMaximumLearnedScale ||
		profile.Session.ObservedBurnPerMin != adaptiveMaximumPersistedBurnPerMin ||
		profile.UnobservedPercent != adaptiveMaximumPersistedPendingPercent ||
		profile.Provider != "legacy-unknown" || profile.PhysicalModel != "legacy-unknown" || profile.CostMode != "legacy-unknown" {
		t.Fatalf("untrusted adaptive profile was not bounded: %#v", profile)
	}

	oldPath := filepath.Join(t.TempDir(), "old-v3.json")
	oldFixture, errMarshal := json.Marshal(persistedUsageState{SchemaVersion: 3})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(oldPath, oldFixture, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	oldState, errLoad := loadUsageStateFile(oldPath)
	if errLoad != nil {
		t.Fatalf("old v3 state did not load: %v", errLoad)
	}
	if oldState.SchemaVersion != usageStateSchemaVersion {
		t.Fatalf("old v3 state migrated to schema %d, want %d", oldState.SchemaVersion, usageStateSchemaVersion)
	}
	if oldState.AdaptiveQuota.Pending == nil || oldState.AdaptiveQuota.Prepared == nil || oldState.AdaptiveQuota.Profiles == nil ||
		oldState.AdaptiveQuota.Aggregates == nil || oldState.AdaptiveQuota.Revisions == nil {
		t.Fatal("old v3 state did not receive an empty adaptive map")
	}
}

func adaptivePersistenceAttempt(authIndex string, reservation float64) executionAttempt {
	return executionAttempt{
		Auth:               testAuthEntry(authIndex),
		Candidate:          candidate{Model: "claude-opus-5"},
		AllocatorManaged:   true,
		ReservationPercent: reservation,
		TariffID:           "x1",
	}
}

func testAuthEntry(authIndex string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"}
}

func setAdaptivePersistenceQuota(t testing.TB, authIndex string, remaining float64) {
	t.Helper()
	quota := adaptivePersistenceQuota(remaining, time.Now().UTC())
	storeQuotaSnapshot(authIndex, quota)
}

func adaptivePersistenceQuota(remaining float64, confirmedAt time.Time) credentialQuotaState {
	return credentialQuotaState{
		Status:      "confirmed",
		Confidence:  "confirmed",
		ConfirmedAt: confirmedAt,
		RefreshedAt: confirmedAt,
		Session: quotaWindowState{
			UsedPercent:      100 - remaining,
			RemainingPercent: remaining,
		},
		Weekly: quotaWindowState{
			UsedPercent:      100 - remaining,
			RemainingPercent: remaining,
		},
	}
}
