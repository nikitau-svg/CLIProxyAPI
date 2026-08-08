package main

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaRefreshUnprovenIncreaseRetainsPendingAndUncertainty(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	authIndex := "unproven-increase"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	previous := refreshQuotaWithReset(40, now.Add(-time.Minute), resetAt)
	storeQuotaSnapshot(authIndex, previous)
	setRuntimePendingForRefreshTest(t, authIndex, 3)
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "standard", ContextBucket: "small"}
	key := adaptiveProfileKey(authIndex, shape)
	recordAdaptiveReservationCommitForKey(authIndex, key, 3, now.Add(-30*time.Second))
	watermark := captureAdaptiveRefreshWatermark(authIndex)

	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude",
		refreshQuotaWithReset(80, now, resetAt), 3, now, watermark)
	if got := pendingReservationPercent(authIndex); got != 3 {
		t.Fatalf("unproven increase cleared pending: %.3f", got)
	}
	adaptiveReserveRuntime.Lock()
	unobserved := adaptiveReserveRuntime.Buckets[key].UnobservedPercent
	adaptiveReserveRuntime.Unlock()
	if unobserved != 3 {
		t.Fatalf("unproven increase cleared estimator uncertainty: %.3f", unobserved)
	}
	retained := quotaSnapshot(authIndex)
	if retained.Session.RemainingPercent != 40 || retained.Weekly.RemainingPercent != 40 {
		t.Fatalf("unproven increase replaced conservative LKG: session/weekly %.1f/%.1f", retained.Session.RemainingPercent, retained.Weekly.RemainingPercent)
	}
	cfg := defaultPluginConfig()
	cfg.UnknownSecondaryPolicy = "block"
	if secondaryQuotaEligibleAt(cfg, retained, "claude-fable-5", tariffByID(cfg, "x1"), authIndex, 3, now) {
		t.Fatal("unproven 40→80 increase changed a prior secondary rejection into admission")
	}
}

func TestQuotaRefreshProvenResetClearsOnlyCoveredWatermark(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	authIndex := "proven-reset"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	oldReset := now.Add(-time.Second)
	previous := refreshQuotaWithReset(40, now.Add(-time.Minute), oldReset)
	storeQuotaSnapshot(authIndex, previous)
	setRuntimePendingForRefreshTest(t, authIndex, 3)
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "standard", ContextBucket: "small"}
	key := adaptiveProfileKey(authIndex, shape)
	recordAdaptiveReservationCommitForKey(authIndex, key, 3, now.Add(-30*time.Second))
	watermark := captureAdaptiveRefreshWatermark(authIndex)

	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude",
		refreshQuotaWithReset(100, now, now.Add(5*time.Hour)), 3, now, watermark)
	if got := pendingReservationPercent(authIndex); got != 0 {
		t.Fatalf("proven reset retained covered pending: %.3f", got)
	}
	adaptiveReserveRuntime.Lock()
	unobserved := adaptiveReserveRuntime.Buckets[key].UnobservedPercent
	adaptiveReserveRuntime.Unlock()
	if unobserved != 0 {
		t.Fatalf("proven reset retained covered estimator watermark: %.3f", unobserved)
	}
}

func TestQuotaRefreshResetDuringInFlightKeepsPostStartCommit(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	if err := configureUsageState(filepath.Join(t.TempDir(), "reset-inflight.json")); err != nil {
		t.Fatal(err)
	}
	authIndex := "reset-inflight"
	now := time.Now().UTC()
	storeQuotaSnapshot(authIndex, refreshQuotaWithReset(40, now.Add(-time.Minute), now.Add(-time.Second)))
	previousConfig := loadedConfig()
	cfg := previousConfig
	cfg.AllocatorMode = "enforce"
	currentConfig.Store(cfg)
	defer currentConfig.Store(previousConfig)
	attempt := adaptivePersistenceAttempt(authIndex, 2)
	attempt.Primary = true
	release, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("primary in-flight lease was not acquired")
	}
	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude",
		refreshQuotaWithReset(100, now, now.Add(5*time.Hour)), 0, now)
	release(true)
	if got := pendingReservationPercent(authIndex); got != 2 {
		t.Fatalf("post-refresh in-flight commit = %.3f pending, want 2", got)
	}
}

func TestQuotaRefreshEqualObservationNeverReconcilesNewPending(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	authIndex := "equal-observation"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	old := refreshQuotaWithReset(40, now, now.Add(time.Hour))
	storeQuotaSnapshot(authIndex, old)
	setRuntimePendingForRefreshTest(t, authIndex, 3)
	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude",
		refreshQuotaWithReset(20, now, now.Add(time.Hour)), 3, now)
	if got := pendingReservationPercent(authIndex); got != 3 {
		t.Fatalf("equal observation cleared new pending: %.3f", got)
	}
	if got := quotaSnapshot(authIndex).Session.RemainingPercent; got != 40 {
		t.Fatalf("equal observation replaced LKG remaining with %.3f", got)
	}
}

func TestQuotaRefreshMixedWindowsAcceptsDecreaseWithoutRejuvenatingIncrease(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	authIndex := "mixed-window-refresh"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	oldConfirmed := now.Add(-20 * time.Minute)
	current := refreshQuotaWithReset(40, oldConfirmed, now.Add(time.Hour))
	current.Weekly.RemainingPercent, current.Weekly.UsedPercent = 80, 20
	current.ModelWeekly = []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 70}}}
	storeQuotaSnapshot(authIndex, current)
	setRuntimePendingForRefreshTest(t, authIndex, 3)
	refreshed := refreshQuotaWithReset(2, now, now.Add(time.Hour))
	refreshed.Weekly.RemainingPercent, refreshed.Weekly.UsedPercent = 81, 19
	refreshed.ModelWeekly = []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 75}}}
	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude", refreshed, 3, now)

	got := quotaSnapshot(authIndex)
	if got.Session.RemainingPercent != 2 || got.Weekly.RemainingPercent != 80 || got.ModelWeekly[0].RemainingPercent != 70 {
		t.Fatalf("mixed conservative merge session/weekly/model = %.1f/%.1f/%.1f", got.Session.RemainingPercent, got.Weekly.RemainingPercent, got.ModelWeekly[0].RemainingPercent)
	}
	if !quotaConfirmedAt(got).Equal(oldConfirmed) {
		t.Fatalf("partially retained LKG was rejuvenated to %s, want %s", quotaConfirmedAt(got), oldConfirmed)
	}
	if pending := pendingReservationPercent(authIndex); pending != 3 {
		t.Fatalf("mixed unproven refresh cleared %.3f pending", 3-pending)
	}
	cfg := defaultPluginConfig()
	cfg.UnknownSecondaryPolicy = "block"
	cfg.QuotaUsageMaxStaleSeconds = 15 * 60
	if secondaryQuotaEligibleAt(cfg, got, "claude-fable-5", tariffByID(cfg, "x1"), authIndex, 3, now) {
		t.Fatal("mixed refresh admitted secondary despite 2% session and retained stale weekly LKG")
	}
}

func TestQuotaRefreshModelWeeklyIncreaseOrRemovalCannotClearDebtOrWidenAdmission(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		refreshed []modelQuotaWindowState
	}{
		{name: "increase", refreshed: []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 20}}}},
		{name: "removed", refreshed: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restore := isolateBravoUsageState(t)
			defer restore()
			resetAdaptiveReserveForTest()
			defer resetAdaptiveReserveForTest()
			authIndex := "model-window-" + testCase.name
			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			previous := adaptivePersistenceQuota(80, now.Add(-time.Minute))
			previous.ModelWeekly = []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 10}}}
			storeQuotaSnapshot(authIndex, previous)
			setRuntimePendingForRefreshTest(t, authIndex, 3)
			refreshed := adaptivePersistenceQuota(70, now)
			refreshed.ModelWeekly = testCase.refreshed
			applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude", refreshed, 3, now)

			got := quotaSnapshot(authIndex)
			if pending := pendingReservationPercent(authIndex); pending != 3 {
				t.Fatalf("model window %s cleared pending: %.3f", testCase.name, pending)
			}
			_, effectiveWeekly := effectiveQuotaWindows(got, "claude-fable-5")
			if effectiveWeekly.RemainingPercent != 10 {
				t.Fatalf("model window %s widened effective weekly to %.1f", testCase.name, effectiveWeekly.RemainingPercent)
			}
			cfg := defaultPluginConfig()
			cfg.UnknownSecondaryPolicy = "block"
			if secondaryQuotaEligibleAt(cfg, got, "claude-fable-5", tariffByID(cfg, "x1"), authIndex, 3, now) {
				t.Fatalf("model window %s changed prior rejection into admission", testCase.name)
			}
		})
	}
}

func TestQuotaRefreshCachedObservationBeforeFetchStartCannotClearWatermark(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	authIndex := "cached-before-fetch"
	oldObserved := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	refreshStarted := oldObserved.Add(10 * time.Minute)
	completedAt := refreshStarted.Add(time.Minute)
	storeQuotaSnapshot(authIndex, adaptivePersistenceQuota(80, oldObserved))
	setRuntimePendingForRefreshTest(t, authIndex, 5)
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "standard", ContextBucket: "small"}
	key := adaptiveProfileKey(authIndex, shape)
	recordAdaptiveReservationCommitForKey(authIndex, key, 3, refreshStarted.Add(-time.Minute))
	watermark := captureAdaptiveRefreshWatermark(authIndex)
	watermark.CapturedAt = refreshStarted
	recordAdaptiveReservationCommitForKey(authIndex, key, 2, refreshStarted.Add(30*time.Second))

	// The cached provider observation is newer than the old LKG, but predates
	// this fetch and therefore proves neither the before-start nor after-start
	// local work was included.
	applyQuotaRefreshSuccess(authIndex, quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(70, refreshStarted.Add(-time.Minute)), 3, completedAt, watermark)
	if pending := pendingReservationPercent(authIndex); pending != 5 {
		t.Fatalf("cached pre-fetch observation cleared pending to %.3f", pending)
	}
	adaptiveReserveRuntime.Lock()
	unobserved := adaptiveReserveRuntime.Buckets[key].UnobservedPercent
	adaptiveReserveRuntime.Unlock()
	if unobserved != 5 {
		t.Fatalf("cached pre-fetch observation cleared estimator watermark to %.3f", unobserved)
	}
	if observed := quotaConfirmedAt(quotaSnapshot(authIndex)); !observed.Equal(oldObserved) {
		t.Fatalf("cached pre-fetch observation rejuvenated LKG to %s", observed)
	}
}

func refreshQuotaWithReset(remaining float64, confirmedAt, resetAt time.Time) credentialQuotaState {
	return credentialQuotaState{
		Status: "confirmed", Confidence: "confirmed", ConfirmedAt: confirmedAt, RefreshedAt: confirmedAt,
		Session: quotaWindowState{UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetAt: resetAt, ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
		Weekly:  quotaWindowState{UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetAt: resetAt, ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
	}
}

func setRuntimePendingForRefreshTest(t *testing.T, authIndex string, amount float64) {
	t.Helper()
	allocatorRuntime.Lock()
	previous, existed := allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.PendingPercent[authIndex] = amount
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		if existed {
			allocatorRuntime.PendingPercent[authIndex] = previous
		} else {
			delete(allocatorRuntime.PendingPercent, authIndex)
		}
		allocatorRuntime.Unlock()
	})
}

func TestFailedQuotaRefreshRetainsLastKnownGood(t *testing.T) {
	now := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "quota-lkg-429", Provider: "claude"}
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{
		Confidence:  "confirmed",
		ConfirmedAt: now.Add(-2 * time.Minute),
		RefreshedAt: now.Add(-2 * time.Minute),
		Session:     quotaWindowState{RemainingPercent: 76, UsedPercent: 24, ResetAt: now.Add(time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
		Weekly:      quotaWindowState{RemainingPercent: 91, UsedPercent: 9, ResetAt: now.Add(4 * 24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
		Dirty:       true,
	})
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, _ string) (credentialQuotaState, error) {
		return credentialQuotaState{}, &quotaRefreshFailure{
			Code: "rate_limited", StatusCode: 429, Retryable: true,
			RetryAfter: "120", RetryAt: now.Add(2 * time.Minute),
			Message: "provider quota endpoint is rate-limited",
		}
	})
	installQuotaRefreshClock(t, now)

	got := refreshQuotaResourceNow("callback", auth, quotaRefreshResourceUsage, true)
	if quotaConfidence(got) != "confirmed" || got.Session.RemainingPercent != 76 || got.Weekly.RemainingPercent != 91 {
		t.Fatalf("failed refresh replaced LKG: %#v", got)
	}
	if !got.ConfirmedAt.Equal(now.Add(-2*time.Minute)) || !got.Dirty {
		t.Fatalf("failed refresh changed confirmation/dirty: %#v", got)
	}
	if got.UsageRefresh.Error == nil || got.UsageRefresh.Error.Code != "rate_limited" ||
		got.UsageRefresh.Error.StatusCode != 429 || !got.UsageRefresh.NextAttemptAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("refresh error not stored separately: %#v", got.UsageRefresh)
	}
}

func TestQuotaRefreshTimeoutRetainsConfirmedSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "quota-lkg-timeout", Provider: "codex"}
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{
		Confidence:  "confirmed",
		ConfirmedAt: now.Add(-time.Minute),
		RefreshedAt: now.Add(-time.Minute),
		Session:     quotaWindowState{RemainingPercent: 80},
		Weekly:      quotaWindowState{RemainingPercent: 70},
	})
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, _ string) (credentialQuotaState, error) {
		return credentialQuotaState{}, &quotaRefreshFailure{Code: "timeout", Retryable: true, Message: "quota request timed out"}
	})
	installQuotaRefreshClock(t, now)

	got := refreshQuotaResourceNow("callback", auth, quotaRefreshResourceUsage, true)
	if quotaConfidence(got) != "confirmed" || got.Session.RemainingPercent != 80 || got.Weekly.RemainingPercent != 70 {
		t.Fatalf("timeout replaced LKG: %#v", got)
	}
	if got.UsageRefresh.Error == nil || got.UsageRefresh.Error.Code != "timeout" ||
		!got.UsageRefresh.NextAttemptAt.After(now) {
		t.Fatalf("timeout refresh state = %#v", got.UsageRefresh)
	}
}

func TestAllocationDoesNotBlockOnQuotaRefresh(t *testing.T) {
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "quota-nonblocking", Provider: "claude"}
	installQuotaRefreshTestConfig(t, func(cfg *pluginConfig) {
		cfg.QuotaUsageRefreshSeconds = 1
		cfg.QuotaProfileRefreshSeconds = 24 * 60 * 60
	})
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-time.Minute), RefreshedAt: now.Add(-time.Minute),
		ProfileRefreshedAt: now,
		Session:            quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
		if resource != quotaRefreshResourceUsage {
			return credentialQuotaState{}, errors.New("unexpected profile refresh")
		}
		close(entered)
		<-release
		return confirmedQuotaForTest(time.Now().UTC()), nil
	})

	started := time.Now()
	got := refreshQuotaIfNeeded("callback", auth, false)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("allocation waited for quota I/O: %v", elapsed)
	}
	if got.Session.RemainingPercent != 80 {
		t.Fatalf("allocation did not receive cached snapshot: %#v", got)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	close(release)
	waitQuotaRefreshIdle(t)
}

func TestQuotaRefreshSingleflightAndProviderConcurrency(t *testing.T) {
	installQuotaRefreshTestConfig(t, func(cfg *pluginConfig) {
		cfg.QuotaRefreshProviderConcurrency = 1
		cfg.QuotaRefreshProviderMinIntervalMS = 0
	})
	var calls atomic.Int64
	var active atomic.Int64
	var maximum atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
		if resource != quotaRefreshResourceUsage {
			return credentialQuotaState{}, errors.New("unexpected profile refresh")
		}
		calls.Add(1)
		value := active.Add(1)
		for {
			old := maximum.Load()
			if value <= old || maximum.CompareAndSwap(old, value) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return confirmedQuotaForTest(time.Now().UTC()), nil
	})
	authA := pluginapi.HostAuthFileEntry{AuthIndex: "quota-flight-a", Provider: "claude"}
	authB := pluginapi.HostAuthFileEntry{AuthIndex: "quota-flight-b", Provider: "claude"}
	installQuotaRefreshTestState(t, authA.AuthIndex, credentialQuotaState{})
	installQuotaRefreshTestState(t, authB.AuthIndex, credentialQuotaState{})

	startQuotaRefresh("callback", authA, quotaRefreshResourceUsage, true)
	startQuotaRefresh("callback", authA, quotaRefreshResourceUsage, true)
	startQuotaRefresh("callback", authB, quotaRefreshResourceUsage, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	// The second auth must be waiting at the provider gate, while the duplicate
	// for auth A must have joined its singleflight.
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls before release = %d, want 1", got)
	}
	close(release)
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("total calls = %d, want one per auth", got)
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("provider concurrency = %d, want 1", got)
	}
}

func TestQuotaFreshnessControlsSecondaryButNotUnknownPrimary(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	cfg := defaultPluginConfig()
	cfg.UnknownSecondaryPolicy = "block"
	cfg.QuotaUsageRefreshSeconds = 60
	cfg.QuotaUsageMaxStaleSeconds = 15 * 60
	tariff := tariffByID(cfg, "x1")
	quota := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-5 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
		Weekly:  quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
	}
	if got := quotaFreshnessAt(quota, "claude-sonnet-5", cfg, now); got != quotaFreshnessStale {
		t.Fatalf("freshness = %q, want stale", got)
	}
	if secondaryQuotaEligibleAt(cfg, quota, "claude-sonnet-5", tariff, "stale-secondary", 0, now) {
		t.Fatal("cold stale LKG authorized secondary without learned external-burn protection")
	}
	quota.ConfirmedAt = now.Add(-20 * time.Minute)
	if got := quotaFreshnessAt(quota, "claude-sonnet-5", cfg, now); got != quotaFreshnessExpired {
		t.Fatalf("freshness = %q, want expired", got)
	}
	if secondaryQuotaEligibleAt(cfg, quota, "claude-sonnet-5", tariff, "expired-secondary", 0, now) {
		t.Fatal("expired LKG authorized a secondary under block policy")
	}

	installQuotaRefreshTestConfig(t, func(installed *pluginConfig) {
		*installed = cfg
	})
	installQuotaRefreshTestState(t, "unknown-primary", credentialQuotaState{Confidence: "unknown"})
	attempt := executionAttempt{
		Auth: pluginapi.HostAuthFileEntry{AuthIndex: "unknown-primary"}, Primary: true,
		AllocatorManaged: true, TariffID: "x1", ReservationPercent: 0.1,
	}
	releaseLease, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("unknown primary was blocked by quota discovery")
	}
	releaseLease(false)
}

func confirmedQuotaForTest(at time.Time) credentialQuotaState {
	return credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: at, RefreshedAt: at,
		Session: quotaWindowState{UsedPercent: 10, RemainingPercent: 90, ResetAt: at.Add(time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
		Weekly:  quotaWindowState{UsedPercent: 20, RemainingPercent: 80, ResetAt: at.Add(24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
	}
}

func installQuotaRefreshTestConfig(t *testing.T, mutate func(*pluginConfig)) {
	t.Helper()
	previous := loadedConfig()
	cfg := defaultPluginConfig()
	mutate(&cfg)
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
}

func installQuotaRefreshTestState(t *testing.T, authIndex string, quota credentialQuotaState) {
	t.Helper()
	bravoUsageState.mu.Lock()
	if bravoUsageState.state.Quotas == nil {
		bravoUsageState.state.Quotas = make(map[string]*credentialQuotaState)
	}
	previous, existed := bravoUsageState.state.Quotas[authIndex]
	copyQuota := quota
	bravoUsageState.state.Quotas[authIndex] = &copyQuota
	bravoUsageState.mu.Unlock()
	t.Cleanup(func() {
		bravoUsageState.mu.Lock()
		if existed {
			bravoUsageState.state.Quotas[authIndex] = previous
		} else {
			delete(bravoUsageState.state.Quotas, authIndex)
		}
		bravoUsageState.mu.Unlock()
	})
}

func installQuotaRefreshFetch(t *testing.T, fetch func(string, pluginapi.HostAuthFileEntry, string) (credentialQuotaState, error)) {
	t.Helper()
	previous := fetchQuotaSnapshot
	fetchQuotaSnapshot = fetch
	t.Cleanup(func() { fetchQuotaSnapshot = previous })
	resetQuotaRefreshRuntimeForTest()
	t.Cleanup(resetQuotaRefreshRuntimeForTest)
}

func installQuotaRefreshClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := quotaRefreshNow
	quotaRefreshNow = func() time.Time { return now }
	t.Cleanup(func() { quotaRefreshNow = previous })
}

func waitQuotaRefreshIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		quotaRefreshRuntimeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("quota refresh workers did not stop")
	}
}
