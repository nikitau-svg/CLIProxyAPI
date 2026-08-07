package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaPollingDefaultsAndBounds(t *testing.T) {
	cfg := defaultPluginConfig()
	if cfg.QuotaUsageRefreshSeconds != 15*60 {
		t.Fatalf("usage interval = %d, want 900", cfg.QuotaUsageRefreshSeconds)
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatalf("normalize defaults: %v", errNormalize)
	}
	for _, value := range []int{minimumQuotaUsageRefreshSeconds - 1, maximumQuotaUsageRefreshSeconds + 1} {
		invalid := defaultPluginConfig()
		invalid.QuotaUsageRefreshSeconds = value
		if errNormalize := normalizeConfig(&invalid); errNormalize == nil {
			t.Fatalf("interval %d was accepted", value)
		}
	}
}

func TestQuotaRefreshCountsActualProviderRequests(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "quota-counter", Provider: "claude"}
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{})
	installQuotaRefreshTestConfig(t, func(cfg *pluginConfig) {
		cfg.QuotaRefreshProviderMinIntervalMS = 0
	})
	var fail atomic.Bool
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
		if resource != quotaRefreshResourceUsage {
			return credentialQuotaState{}, errors.New("unexpected resource")
		}
		if fail.Load() {
			return credentialQuotaState{}, &quotaRefreshFailure{Code: "timeout", Retryable: true}
		}
		return confirmedQuotaForTest(now), nil
	})
	installQuotaRefreshClock(t, now)

	refreshQuotaResourceNow("callback", auth, quotaRefreshResourceUsage, true)
	fail.Store(true)
	refreshQuotaResourceNow("callback", auth, quotaRefreshResourceUsage, true)
	state := quotaSnapshot(auth.AuthIndex).UsageRefresh
	if state.AttemptCount != 2 || state.SuccessCount != 1 || state.FailureCount != 1 {
		t.Fatalf("request counters = %#v, want attempts=2 success=1 failure=1", state)
	}
}

func TestQuotaRateLimitCooldownIsScopedByEgress(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	directA := pluginapi.HostAuthFileEntry{AuthIndex: "direct-a", Provider: "claude", EgressID: "direct"}
	directB := pluginapi.HostAuthFileEntry{AuthIndex: "direct-b", Provider: "claude", EgressID: "direct"}
	proxied := pluginapi.HostAuthFileEntry{AuthIndex: "proxy-a", Provider: "claude", EgressID: "proxy-safe-id"}
	for _, auth := range []pluginapi.HostAuthFileEntry{directA, directB, proxied} {
		installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{})
	}
	installQuotaRefreshTestConfig(t, func(cfg *pluginConfig) {
		cfg.QuotaRefreshProviderMinIntervalMS = 0
	})
	var calls atomic.Int64
	installQuotaRefreshFetch(t, func(_ string, auth pluginapi.HostAuthFileEntry, _ string) (credentialQuotaState, error) {
		calls.Add(1)
		if auth.EgressID == "direct" {
			return credentialQuotaState{}, &quotaRefreshFailure{
				Code: "rate_limited", StatusCode: 429, Retryable: true,
				RetryAt: now.Add(15 * time.Minute),
			}
		}
		return confirmedQuotaForTest(now), nil
	})
	installQuotaRefreshClock(t, now)

	refreshQuotaResourceNow("callback", directA, quotaRefreshResourceUsage, true)
	refreshQuotaResourceNow("callback", directB, quotaRefreshResourceUsage, true)
	refreshQuotaResourceNow("callback", proxied, quotaRefreshResourceUsage, true)
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want direct A plus independent proxy", got)
	}
	if got := quotaSnapshot(directB.AuthIndex).UsageRefresh.AttemptCount; got != 0 {
		t.Fatalf("shared direct cooldown counted a provider request: %d", got)
	}
}

func TestQuotaPollingRunsOutsideAllocatorAndDeduplicatesFreshCycle(t *testing.T) {
	resetQuotaPollingForTest()
	t.Cleanup(resetQuotaPollingForTest)
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{ID: "quota-poller-id", AuthIndex: "quota-poller", Provider: "claude"}
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{})
	installQuotaRefreshTestConfig(t, func(cfg *pluginConfig) {
		cfg.QuotaRefreshProviderMinIntervalMS = 0
		cfg.QuotaRefreshJitterPercent = 0
	})
	var calls atomic.Int64
	entered := make(chan struct{}, 2)
	installQuotaRefreshFetch(t, func(_ string, _ pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
		calls.Add(1)
		entered <- struct{}{}
		if resource == quotaRefreshResourceProfile {
			return credentialQuotaState{ProfileRefreshedAt: now}, nil
		}
		return confirmedQuotaForTest(now), nil
	})
	quotaPollingConfigured.Store(true)
	observeQuotaPolling("callback", []pluginapi.HostAuthFileEntry{auth})
	runQuotaPollingCycle()
	for count := 0; count < 2; count++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("polling worker did not start both resources; calls=%d", calls.Load())
		}
	}
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("initial polling calls = %d, want one usage and one profile", got)
	}

	// A later scheduler check consumes the cache and performs no provider I/O.
	runQuotaPollingCycle()
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("fresh polling cycle made %d calls, want 2 total", got)
	}

	removed := quotaSnapshot(auth.AuthIndex)
	removed.Dirty = true
	storeQuotaSnapshot(auth.AuthIndex, removed)
	observeQuotaPolling("callback", nil)
	runQuotaPollingCycle()
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("removed auth was still polled; calls=%d", got)
	}
}

func TestAllocatorConsumesQuotaCacheWithoutProviderCall(t *testing.T) {
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "quota-hot-path", Provider: "claude"}
	installQuotaRefreshTestState(t, auth.AuthIndex, credentialQuotaState{Confidence: "unknown"})
	installQuotaRefreshFetch(t, func(string, pluginapi.HostAuthFileEntry, string) (credentialQuotaState, error) {
		t.Fatal("allocator contacted quota provider")
		return credentialQuotaState{}, nil
	})
	cfg := defaultPluginConfig()
	project := smartKeyConfig{ID: "project", PrimaryAuthIDs: []string{auth.AuthIndex}}
	got := allocateCandidateAuths(
		rpcExecutorRequest{}, cfg, project,
		candidate{Provider: "claude", Model: "claude-sonnet-5"},
		[]pluginapi.HostAuthFileEntry{auth}, "sticky",
	)
	if len(got) != 1 || got[0].Auth.AuthIndex != auth.AuthIndex {
		t.Fatalf("cached primary allocation = %#v", got)
	}
}
