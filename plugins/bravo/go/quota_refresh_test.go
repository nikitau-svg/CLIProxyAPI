package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	if !secondaryQuotaEligibleAt(cfg, quota, "claude-sonnet-5", tariff, "stale-secondary", 0, now) {
		t.Fatal("stale LKG should remain eligible above floors")
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
