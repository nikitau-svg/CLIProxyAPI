package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
)

func TestAdaptiveBreakerPublicTruthKeepsForecastShadow(t *testing.T) {
	cfg := adaptiveBreakerTestConfig(t)
	view := adaptiveShadowSummary(cfg, nil, time.Now().UTC())
	if view.Mode != "breaker" || view.Effect != "breaker_routing_enforced" ||
		!view.RoutingEnforced || view.ForecastRoutingEnforced ||
		!view.EdgeGate.RoutingEnforced || view.EdgeGate.QueuesRequests || view.AdditionalProviderRequests {
		t.Fatalf("breaker public view=%#v", view)
	}
	message := adaptiveBreakerForecastAuditMessage("needs_review", 100)
	if !strings.Contains(message, "не блокирует") || strings.Contains(message, "вернуть observe") {
		t.Fatalf("breaker audit message=%q", message)
	}
}

func adaptiveBreakerTestConfig(t *testing.T) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "breaker"
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatalf("breaker config: %v", err)
	}
	return cfg
}

func TestAdaptiveBreakerNeverGuardsConfirmedLowHeadroom(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	cfg := adaptiveBreakerTestConfig(t)

	const workers = 100
	start := make(chan struct{})
	decisions := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		attempt := adaptiveEdgeGateTestAttempt("edge-auth", "claude-fable-5", 1, 1, now)
		attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt,
			credentialQuotaState{Confidence: "confirmed"}, tariffConfig{}, now)
		wg.Add(1)
		go func(a executionAttempt) {
			defer wg.Done()
			<-start
			beginAdaptiveEdgeGateShadow(a, now)
			decisions <- a.AdaptiveEdgeGate.snapshot().Decision
		}(attempt)
	}
	close(start)
	wg.Wait()
	close(decisions)
	for decision := range decisions {
		if decision != adaptiveEdgeGateDecisionDispatch {
			t.Fatalf("proactive low-headroom decision=%q, want dispatch", decision)
		}
	}
	if view := adaptiveEdgeGateSummary(cfg, nil, now); view.InFlightGuards != 0 {
		t.Fatalf("proactive low-headroom acquired %d guards", view.InFlightGuards)
	}
}

func TestAdaptiveBreakerTripsOnlyTrustedTaxonomyOrBare429(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)
	cfg := adaptiveBreakerTestConfig(t)

	untrusted := adaptiveEdgeGateTestAttempt("auth", "claude-opus-5", 50, 50, now)
	untrusted.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, untrusted, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(untrusted, now)
	observeAdaptiveEdgeGateOutcome(untrusted, false, executionFailure{Status: http.StatusForbidden,
		Provider: &providererror.Detail{Class: providererror.ClassQuota, Scope: providererror.ScopeAccount, Reason: "quota"}}, now)
	next := adaptiveEdgeGateTestAttempt("auth", "claude-opus-5", 50, 50, now)
	next.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, next, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(next, now.Add(time.Second))
	if got := next.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("untrusted provider text tripped breaker: %q", got)
	}

	trusted := adaptiveEdgeGateTestAttempt("auth", "claude-opus-5", 50, 50, now)
	trusted.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, trusted, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(trusted, now.Add(2*time.Second))
	observeAdaptiveEdgeGateOutcome(trusted, false, executionFailure{Status: http.StatusForbidden, RetryAfter: "60",
		Provider: &providererror.Detail{TaxonomyVersion: providererror.FailureTaxonomyV1,
			Class: providererror.ClassRateLimit, Scope: providererror.ScopeAccount}}, now.Add(2*time.Second))
	blocked := adaptiveEdgeGateTestAttempt("auth", "claude-haiku-5", 50, 50, now)
	blocked.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, blocked, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(blocked, now.Add(3*time.Second))
	if got := blocked.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionSkipTripped || got.Reason != "account_breaker" {
		t.Fatalf("trusted account breaker=%#v", got)
	}

	bare := adaptiveEdgeGateTestAttempt("bare", "claude-opus-5", 50, 50, now)
	bare.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, bare, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(bare, now)
	observeAdaptiveEdgeGateOutcome(bare, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "60", AccountWide: true}, now)
	otherModel := adaptiveEdgeGateTestAttempt("bare", "claude-haiku-5", 50, 50, now)
	otherModel.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, otherModel, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(otherModel, now.Add(time.Second))
	if got := otherModel.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("bare 429 widened beyond model: %q", got)
	}
}

func TestAdaptiveBreakerLastChanceBypassesActiveBreaker(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)
	cfg := adaptiveBreakerTestConfig(t)
	attempt := adaptiveEdgeGateTestAttempt("last-auth", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[adaptiveEdgeGateBreakerKey("claude", "last-auth", "claude-opus-5")] = adaptiveEdgeGateBreaker{
		AuthIndex: "last-auth", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute),
	}
	adaptiveEdgeGateRuntime.Unlock()

	last := adaptiveBreakerLastChanceAttempt(attempt)
	if !isAdaptiveBreakerLastChanceAttempt(last) {
		t.Fatal("last chance marker missing")
	}
	state := last.AdaptiveEdgeGate
	state.mu.Lock()
	state.started = true
	state.decision = adaptiveEdgeGateDecisionDispatch
	state.reason = "breaker_last_chance"
	state.mu.Unlock()
	if got := state.snapshot(); got.Decision != adaptiveEdgeGateDecisionDispatch || got.Reason != "breaker_last_chance" {
		t.Fatalf("last chance snapshot=%#v", got)
	}
}

func TestAdaptiveBreakerStaleRecoveryCannotOpenNewGeneration(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "generation-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "generation-auth", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(time.Minute), Generation: 1,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 1
	adaptiveEdgeGateRuntime.Unlock()
	attempt := adaptiveEdgeGateTestAttempt("generation-auth", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveAllocatorMode = "breaker"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(attempt, now)
	stale := adaptiveBreakerLastChanceAttempt(attempt)

	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	breaker.Generation = 2
	breaker.Until = now.Add(2 * time.Minute)
	adaptiveEdgeGateRuntime.Breakers[key] = breaker
	adaptiveEdgeGateRuntime.NextGeneration = 2
	adaptiveEdgeGateRuntime.Unlock()
	_, acquired, failure := acquireAdaptiveBreakerEnforcementLease(stale, now.Add(time.Second))
	if acquired || failure == nil || failure.Code != "bravo_adaptive_edge_tripped" {
		t.Fatalf("stale recovery opened new generation: acquired=%v failure=%#v", acquired, failure)
	}
}

func TestAdaptiveBreakerScheduledHalfOpenDoesNotOverlapRecovery(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "overlap-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "overlap-auth", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(time.Second), Generation: 1,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 1
	adaptiveEdgeGateRuntime.Unlock()
	attempt := adaptiveEdgeGateTestAttempt("overlap-auth", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveAllocatorMode = "breaker"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(attempt, now)
	recovery := adaptiveBreakerLastChanceAttempt(attempt)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(recovery, now); !acquired || failure != nil {
		t.Fatalf("recovery acquisition=%v failure=%#v", acquired, failure)
	}

	probe := adaptiveEdgeGateTestAttempt("overlap-auth", "claude-opus-5", 50, 50, now)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(probe, now.Add(2*time.Second))
	if got := probe.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionSkipBusy || got.Reason != "half_open_probe_busy" {
		t.Fatalf("scheduled half-open overlapped recovery: %#v", got)
	}
}

func TestAdaptiveBreakerRetainedRecoveryDoesNotOverlapScheduledProbe(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "reverse-overlap", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "reverse-overlap", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(time.Second), Generation: 61,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 61
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("reverse-overlap", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	retained := adaptiveBreakerLastChanceAttempt(base)

	probeAt := now.Add(2 * time.Second)
	probe := adaptiveEdgeGateTestAttempt("reverse-overlap", "claude-opus-5", 50, 50, probeAt)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, probeAt)
	beginAdaptiveEdgeGateShadow(probe, probeAt)
	if got := probe.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("scheduled probe decision=%q", got)
	}
	_, acquired, failure := acquireAdaptiveBreakerEnforcementLease(retained, probeAt)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_edge_busy" {
		t.Fatalf("retained recovery overlapped scheduled probe: acquired=%v failure=%#v", acquired, failure)
	}
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if !breaker.ProbeInFlight || breaker.RecoveryInFlight {
		t.Fatalf("overlap state=%#v", breaker)
	}
}

func TestAdaptiveBreakerStaleRecoveryLeaseAllowsScheduledProbe(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	cfg := adaptiveBreakerTestConfig(t)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "stale-recovery", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "stale-recovery", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(-time.Second), Generation: 31, RecoveryUsed: true,
		RecoveryInFlight: true, RecoveryStartedAt: now.Add(-adaptiveEdgeGateMaximumLeaseAge - time.Second),
	}
	adaptiveEdgeGateRuntime.NextGeneration = 31
	adaptiveEdgeGateRuntime.Unlock()
	probe := adaptiveEdgeGateTestAttempt("stale-recovery", "claude-opus-5", 50, 50, now)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(probe, now)
	if got := probe.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("stale recovery lease blocked scheduled probe: %#v", got)
	}
}

func TestAdaptiveBreakerLateProbeOutcomeCannotClearNewProbeABA(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	cfg := adaptiveBreakerTestConfig(t)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "probe-aba", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "probe-aba", Provider: "claude", Model: "claude-opus-5", Until: now.Add(-time.Second), Generation: 41}
	adaptiveEdgeGateRuntime.NextGeneration = 41
	adaptiveEdgeGateRuntime.Unlock()
	first := adaptiveEdgeGateTestAttempt("probe-aba", "claude-opus-5", 50, 50, now)
	first.AdaptiveAllocatorMode = "breaker"
	first.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, first, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(first, now)
	firstID := first.AdaptiveEdgeGate.probeLeaseID
	secondAt := now.Add(adaptiveEdgeGateMaximumLeaseAge + time.Second)
	second := adaptiveEdgeGateTestAttempt("probe-aba", "claude-opus-5", 50, 50, secondAt)
	second.AdaptiveAllocatorMode = "breaker"
	second.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, second, credentialQuotaState{}, tariffConfig{}, secondAt)
	beginAdaptiveEdgeGateShadow(second, secondAt)
	secondID := second.AdaptiveEdgeGate.probeLeaseID
	if firstID == 0 || secondID == 0 || firstID == secondID {
		t.Fatalf("probe lease ids first=%d second=%d", firstID, secondID)
	}
	observeAdaptiveEdgeGateOutcome(first, true, executionFailure{}, secondAt)
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if !breaker.ProbeInFlight || breaker.ProbeLeaseID != secondID {
		t.Fatalf("late probe outcome cleared current probe: %#v", breaker)
	}
}

func TestAdaptiveBreakerLateRecoveryOutcomeCannotClearScheduledProbeABA(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "recovery-aba", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "recovery-aba", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Second), Generation: 51}
	adaptiveEdgeGateRuntime.NextGeneration = 51
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("recovery-aba", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	recovery := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(recovery, now); !acquired || failure != nil {
		t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
	}
	recoveryID := recovery.AdaptiveEdgeGate.recoveryLeaseID
	probeAt := now.Add(adaptiveEdgeGateMaximumLeaseAge + 2*time.Second)
	probe := adaptiveEdgeGateTestAttempt("recovery-aba", "claude-opus-5", 50, 50, probeAt)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, probeAt)
	beginAdaptiveEdgeGateShadow(probe, probeAt)
	probeID := probe.AdaptiveEdgeGate.probeLeaseID
	if recoveryID == 0 || probeID == 0 {
		t.Fatalf("lease ids recovery=%d probe=%d", recoveryID, probeID)
	}
	observeAdaptiveEdgeGateOutcome(recovery, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "60"}, probeAt)
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if !breaker.ProbeInFlight || breaker.ProbeLeaseID != probeID {
		t.Fatalf("late recovery outcome cleared current probe: %#v", breaker)
	}
}

func TestAdaptiveBreakerInconclusiveProbeAndRecoveryStayClosed(t *testing.T) {
	for _, kind := range []string{"probe", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			resetAdaptiveEdgeGateForTest()
			t.Cleanup(resetAdaptiveEdgeGateForTest)
			previous := loadedConfig()
			t.Cleanup(func() { currentConfig.Store(previous) })
			cfg := adaptiveBreakerTestConfig(t)
			currentConfig.Store(cfg)
			now := time.Now().UTC()
			auth := "inconclusive-" + kind
			key := adaptiveEdgeGateBreakerKey("claude", auth, "claude-opus-5")
			until := now.Add(-time.Second)
			if kind == "recovery" {
				until = now.Add(time.Minute)
			}
			adaptiveEdgeGateRuntime.Lock()
			adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: auth, Provider: "claude", Model: "claude-opus-5", Until: until, Generation: 71}
			adaptiveEdgeGateRuntime.NextGeneration = 71
			adaptiveEdgeGateRuntime.Unlock()
			attempt := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
			attempt.AdaptiveAllocatorMode = "breaker"
			attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(attempt, now)
			if kind == "recovery" {
				attempt = adaptiveBreakerLastChanceAttempt(attempt)
				if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(attempt, now); !acquired || failure != nil {
					t.Fatalf("recovery acquire=%v failure=%#v", acquired, failure)
				}
			}
			generation := uint64(71)
			observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{Code: "request_canceled", Status: 499}, now)
			adaptiveEdgeGateRuntime.Lock()
			breaker, exists := adaptiveEdgeGateRuntime.Breakers[key]
			adaptiveEdgeGateRuntime.Unlock()
			if !exists || breaker.Generation != generation || breaker.ProbeInFlight || breaker.RecoveryInFlight ||
				!breaker.RecoveryUsed || !breaker.Until.After(now) {
				t.Fatalf("inconclusive %s weakened breaker: %#v exists=%v", kind, breaker, exists)
			}
			next := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
			next.AdaptiveAllocatorMode = "breaker"
			next.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, next, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(next, now.Add(500*time.Millisecond))
			if got := next.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipTripped {
				t.Fatalf("next %s request decision=%q", kind, got)
			}
		})
	}
}

func TestAdaptiveBreakerConclusiveAcceptedOrReviewedNonQuotaReopens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted bool
		provider *providererror.Detail
	}{
		{name: "accepted", accepted: true},
		{name: "reviewed", provider: &providererror.Detail{TaxonomyVersion: providererror.FailureTaxonomyV1, Class: providererror.ClassAuthentication, Scope: providererror.ScopeModel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAdaptiveEdgeGateForTest()
			now := time.Now().UTC()
			cfg := adaptiveBreakerTestConfig(t)
			key := adaptiveEdgeGateBreakerKey("claude", "conclusive-"+tc.name, "claude-opus-5")
			adaptiveEdgeGateRuntime.Lock()
			adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "conclusive-" + tc.name, Provider: "claude", Model: "claude-opus-5", Until: now.Add(-time.Second), Generation: 81}
			adaptiveEdgeGateRuntime.NextGeneration = 81
			adaptiveEdgeGateRuntime.Unlock()
			attempt := adaptiveEdgeGateTestAttempt("conclusive-"+tc.name, "claude-opus-5", 50, 50, now)
			attempt.AdaptiveAllocatorMode = "breaker"
			attempt.AdaptiveProviderAccepted = tc.accepted
			attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(attempt, now)
			observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{Status: http.StatusBadGateway, Provider: tc.provider}, now)
			adaptiveEdgeGateRuntime.Lock()
			_, exists := adaptiveEdgeGateRuntime.Breakers[key]
			adaptiveEdgeGateRuntime.Unlock()
			if exists {
				t.Fatal("conclusive non-quota outcome did not reopen breaker")
			}
		})
	}
}

func TestReviewedQuotaClassifierPreservesAuthoritativeScopeForBreaker(t *testing.T) {
	for _, tc := range []struct {
		name, typ, code, scope string
	}{
		{name: "legacy_rate_limit_type_account", typ: "rate_limit_error", code: "rate_limit_error", scope: providererror.ScopeAccount},
		{name: "unrecognized_empty_type_model", typ: "", code: "opaque_quota", scope: providererror.ScopeModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAdaptiveEdgeGateForTest()
			now := time.Now().UTC()
			detail := providererror.Detail{
				Type: tc.typ, Code: tc.code, Message: "safe reviewed quota",
				TaxonomyVersion: providererror.FailureTaxonomyV1,
				Class:           providererror.ClassQuota, Scope: tc.scope,
			}
			failure := classifyExecutionError(&hostCallError{
				Code: "provider_error", HTTPStatus: http.StatusTooManyRequests, ProviderError: &detail,
			})
			if failure.Provider == nil || failure.Provider.Scope != tc.scope ||
				failure.Provider.Class != providererror.ClassQuota || !failure.Retryable ||
				failure.AccountWide != (tc.scope == providererror.ScopeAccount) {
				t.Fatalf("classified reviewed quota=%#v", failure)
			}
			cfg := adaptiveBreakerTestConfig(t)
			attempt := adaptiveEdgeGateTestAttempt("classifier-auth", "claude-opus-5", 50, 50, now)
			attempt.AdaptiveAllocatorMode = "breaker"
			attempt.AdaptiveProviderAccepted = true
			attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(attempt, now)
			observeAdaptiveEdgeGateOutcome(attempt, false, failure, now)
			other := adaptiveEdgeGateTestAttempt("classifier-auth", "claude-haiku-5", 50, 50, now)
			other.AdaptiveAllocatorMode = "breaker"
			other.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, other, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(other, now.Add(time.Second))
			want := adaptiveEdgeGateDecisionDispatch
			if tc.scope == providererror.ScopeAccount {
				want = adaptiveEdgeGateDecisionSkipTripped
			}
			if got := other.AdaptiveEdgeGate.snapshot().Decision; got != want {
				t.Fatalf("other-model decision=%q want=%q", got, want)
			}
		})
	}
}

func TestAdaptiveBreakerNewerQuotaEvidenceSupersedesOldProof(t *testing.T) {
	for _, kind := range []string{"probe", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			resetAdaptiveEdgeGateForTest()
			t.Cleanup(resetAdaptiveEdgeGateForTest)
			previous := loadedConfig()
			t.Cleanup(func() { currentConfig.Store(previous) })
			cfg := adaptiveBreakerTestConfig(t)
			currentConfig.Store(cfg)
			now := time.Now().UTC()
			auth := "new-evidence-" + kind
			key := adaptiveEdgeGateBreakerKey("claude", auth, "claude-opus-5")
			until := now.Add(-time.Second)
			if kind == "recovery" {
				until = now.Add(time.Minute)
			}
			adaptiveEdgeGateRuntime.Lock()
			adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: auth, Provider: "claude", Model: "claude-opus-5", Until: until, Generation: 91, EvidenceRevision: 91}
			adaptiveEdgeGateRuntime.NextGeneration = 91
			adaptiveEdgeGateRuntime.NextEvidenceRevision = 91
			adaptiveEdgeGateRuntime.Unlock()
			old := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
			old.AdaptiveAllocatorMode = "breaker"
			old.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, old, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(old, now)
			if kind == "recovery" {
				old = adaptiveBreakerLastChanceAttempt(old)
				if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(old, now); !acquired || failure != nil {
					t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
				}
			}
			ordinary := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
			ordinary.AdaptiveAllocatorMode = "breaker"
			ordinary.AdaptiveProviderDispatched = true
			ordinary.AdaptiveEdgeGate = &adaptiveEdgeGateAttemptState{
				authIndex: auth, provider: "claude", model: "claude-opus-5",
				started: true, decision: adaptiveEdgeGateDecisionDispatch, enforce: true, breakerOnly: true,
			}
			observeAdaptiveEdgeGateOutcome(ordinary, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "120"}, now.Add(time.Second))
			adaptiveEdgeGateRuntime.Lock()
			newer := adaptiveEdgeGateRuntime.Breakers[key]
			adaptiveEdgeGateRuntime.Unlock()
			wantProbeInFlight := kind == "probe"
			wantRecoveryInFlight := kind == "recovery"
			if newer.EvidenceRevision <= 91 || newer.ProbeInFlight != wantProbeInFlight ||
				newer.RecoveryInFlight != wantRecoveryInFlight || !newer.RecoveryUsed {
				t.Fatalf("new evidence did not supersede old proof: %#v", newer)
			}
			observeAdaptiveEdgeGateOutcome(old, true, executionFailure{}, now.Add(2*time.Second))
			adaptiveEdgeGateRuntime.Lock()
			after := adaptiveEdgeGateRuntime.Breakers[key]
			adaptiveEdgeGateRuntime.Unlock()
			if after.EvidenceRevision != newer.EvidenceRevision || after.Until.Before(newer.Until) ||
				after.ProbeInFlight || after.RecoveryInFlight {
				t.Fatalf("old success erased newer evidence: newer=%#v after=%#v", newer, after)
			}
			next := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
			next.AdaptiveAllocatorMode = "breaker"
			next.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, next, credentialQuotaState{}, tariffConfig{}, now)
			beginAdaptiveEdgeGateShadow(next, now.Add(3*time.Second))
			if got := next.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipTripped {
				t.Fatalf("newer evidence did not keep route closed: %q", got)
			}
		})
	}
}

func TestAdaptiveBreakerReviewedNonQuotaOverridesContradictory429(t *testing.T) {
	for _, class := range []string{providererror.ClassContextWindow, providererror.ClassAuthentication} {
		resetAdaptiveEdgeGateForTest()
		now := time.Now().UTC()
		cfg := adaptiveBreakerTestConfig(t)
		attempt := adaptiveEdgeGateTestAttempt("contradictory-429", "claude-opus-5", 50, 50, now)
		attempt.AdaptiveAllocatorMode = "breaker"
		attempt.AdaptiveProviderAccepted = true
		attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
		beginAdaptiveEdgeGateShadow(attempt, now)
		observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{Status: http.StatusTooManyRequests, Provider: &providererror.Detail{
			TaxonomyVersion: providererror.FailureTaxonomyV1, Class: class, Scope: providererror.ScopeRequest,
		}}, now)
		if view := adaptiveEdgeGateSummary(cfg, nil, now); view.TrackedBreakers != 0 {
			t.Fatalf("reviewed nonquota class %q tripped on contradictory 429", class)
		}
	}
	// With no reviewed taxonomy, a bare 429 remains a model-scoped trip.
	resetAdaptiveEdgeGateForTest()
	now := time.Now().UTC()
	cfg := adaptiveBreakerTestConfig(t)
	bare := adaptiveEdgeGateTestAttempt("bare-429-review", "claude-opus-5", 50, 50, now)
	bare.AdaptiveAllocatorMode = "breaker"
	bare.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, bare, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(bare, now)
	observeAdaptiveEdgeGateOutcome(bare, false, executionFailure{Status: http.StatusTooManyRequests}, now)
	if view := adaptiveEdgeGateSummary(cfg, nil, now); view.TrackedBreakers != 1 {
		t.Fatalf("bare 429 breaker count=%d", view.TrackedBreakers)
	}
}

func TestAdaptiveBreakerPredispatchCancelClearsOwnedLeaseAfterRevisionAdvance(t *testing.T) {
	for _, kind := range []string{"probe", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			resetAdaptiveEdgeGateForTest()
			key := adaptiveEdgeGateBreakerKey("claude", "cancel-revision-"+kind, "claude-opus-5")
			breaker := adaptiveEdgeGateBreaker{
				AuthIndex: "cancel-revision-" + kind, Provider: "claude", Model: "claude-opus-5",
				Until: time.Now().UTC().Add(time.Minute), Generation: 101, EvidenceRevision: 102,
				RecoveryUsed: true,
			}
			state := &adaptiveEdgeGateAttemptState{
				started: true, authIndex: breaker.AuthIndex, provider: "claude", model: "claude-opus-5",
			}
			if kind == "probe" {
				breaker.ProbeInFlight, breaker.ProbeLeaseID = true, 501
				state.probeBreakerKey, state.probeBreakerGeneration = key, 101
				state.probeEvidenceRevision, state.probeLeaseID = 101, 501
			} else {
				breaker.RecoveryInFlight, breaker.RecoveryLeaseID = true, 502
				state.recoveryBreakerKey, state.recoveryBreakerGeneration = key, 101
				state.recoveryEvidenceRevision, state.recoveryLeaseID = 101, 502
			}
			adaptiveEdgeGateRuntime.Lock()
			adaptiveEdgeGateRuntime.Breakers[key] = breaker
			adaptiveEdgeGateRuntime.Unlock()
			cancelAdaptiveEdgeGateAttempt(executionAttempt{AdaptiveShadow: true, AdaptiveEdgeGate: state})
			adaptiveEdgeGateRuntime.Lock()
			after := adaptiveEdgeGateRuntime.Breakers[key]
			adaptiveEdgeGateRuntime.Unlock()
			if after.ProbeInFlight || after.RecoveryInFlight || after.EvidenceRevision != 102 || !after.RecoveryUsed {
				t.Fatalf("cancel after revision advance weakened breaker: %#v", after)
			}
		})
	}
}

func TestAdaptiveBreakerPerAuthProofLeaseSurvivesAccountRescope(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	modelKey := adaptiveEdgeGateBreakerKey("claude", "proof-rescope", "claude-opus-5")
	accountKey := adaptiveEdgeGateBreakerKey("claude", "proof-rescope", "")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[modelKey] = adaptiveEdgeGateBreaker{AuthIndex: "proof-rescope", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute), Generation: 111, EvidenceRevision: 111}
	adaptiveEdgeGateRuntime.NextGeneration = 111
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 111
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("proof-rescope", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	recovery := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(recovery, now); !acquired || failure != nil {
		t.Fatalf("model recovery acquired=%v failure=%#v", acquired, failure)
	}
	ordinary := adaptiveEdgeGateTestAttempt("proof-rescope", "claude-opus-5", 50, 50, now)
	ordinary.AdaptiveAllocatorMode = "breaker"
	ordinary.AdaptiveEdgeGate = &adaptiveEdgeGateAttemptState{authIndex: "proof-rescope", provider: "claude", model: "claude-opus-5", started: true, decision: adaptiveEdgeGateDecisionDispatch}
	observeAdaptiveEdgeGateOutcome(ordinary, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "120", Provider: &providererror.Detail{
		TaxonomyVersion: providererror.FailureTaxonomyV1, Class: providererror.ClassQuota, Scope: providererror.ScopeAccount,
	}}, now.Add(time.Second))
	adaptiveEdgeGateRuntime.Lock()
	account := adaptiveEdgeGateRuntime.Breakers[accountKey]
	account.Until = now.Add(-time.Second)
	adaptiveEdgeGateRuntime.Breakers[accountKey] = account
	adaptiveEdgeGateRuntime.Unlock()
	peer := adaptiveEdgeGateTestAttempt("proof-rescope", "claude-haiku-5", 50, 50, now)
	peer.AdaptiveAllocatorMode = "breaker"
	peer.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, peer, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(peer, now.Add(2*time.Second))
	if got := peer.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipBusy {
		t.Fatalf("account rescope ignored physical model recovery: %q", got)
	}
	adaptiveEdgeGateRuntime.Lock()
	account = adaptiveEdgeGateRuntime.Breakers[accountKey]
	account.Until = now.Add(2 * time.Minute)
	adaptiveEdgeGateRuntime.Breakers[accountKey] = account
	adaptiveEdgeGateRuntime.Unlock()
	observeAdaptiveEdgeGateOutcome(recovery, true, executionFailure{}, now.Add(3*time.Second))
	adaptiveEdgeGateRuntime.Lock()
	_, accountExists := adaptiveEdgeGateRuntime.Breakers[accountKey]
	_, authBusy := adaptiveEdgeGateRuntime.InFlight["proof-rescope"]
	adaptiveEdgeGateRuntime.Unlock()
	if !accountExists || authBusy {
		t.Fatalf("old model success accountExists=%v authBusy=%v", accountExists, authBusy)
	}
}

func TestAdaptiveBreakerRejectsUnreviewedRawTaxonomyAsConclusive(t *testing.T) {
	invalidDetails := []providererror.Detail{
		{TaxonomyVersion: 1, Class: "unknown_class", Scope: providererror.ScopeModel},
		{TaxonomyVersion: 1, Class: providererror.ClassQuota, Scope: "unknown_scope"},
	}
	for _, kind := range []string{"probe", "recovery"} {
		for index, raw := range invalidDetails {
			t.Run(fmt.Sprintf("%s_%d", kind, index), func(t *testing.T) {
				resetAdaptiveEdgeGateForTest()
				t.Cleanup(resetAdaptiveEdgeGateForTest)
				previous := loadedConfig()
				t.Cleanup(func() { currentConfig.Store(previous) })
				cfg := adaptiveBreakerTestConfig(t)
				currentConfig.Store(cfg)
				now := time.Now().UTC()
				auth := fmt.Sprintf("raw-taxonomy-%s-%d", kind, index)
				key := adaptiveEdgeGateBreakerKey("claude", auth, "claude-opus-5")
				until := now.Add(-time.Second)
				if kind == "recovery" {
					until = now.Add(time.Minute)
				}
				adaptiveEdgeGateRuntime.Lock()
				adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: auth, Provider: "claude", Model: "claude-opus-5", Until: until, Generation: 121, EvidenceRevision: 121}
				adaptiveEdgeGateRuntime.NextGeneration = 121
				adaptiveEdgeGateRuntime.NextEvidenceRevision = 121
				adaptiveEdgeGateRuntime.Unlock()
				attempt := adaptiveEdgeGateTestAttempt(auth, "claude-opus-5", 50, 50, now)
				attempt.AdaptiveAllocatorMode = "breaker"
				attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
				beginAdaptiveEdgeGateShadow(attempt, now)
				if kind == "recovery" {
					attempt = adaptiveBreakerLastChanceAttempt(attempt)
					if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(attempt, now); !acquired || failure != nil {
						t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
					}
				}
				observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{Status: http.StatusBadGateway, Provider: &raw}, now)
				adaptiveEdgeGateRuntime.Lock()
				breaker, exists := adaptiveEdgeGateRuntime.Breakers[key]
				adaptiveEdgeGateRuntime.Unlock()
				if !exists || breaker.Generation != 121 || !breaker.RecoveryUsed ||
					breaker.ProbeInFlight || breaker.RecoveryInFlight || !breaker.Until.After(now) {
					t.Fatalf("raw taxonomy was trusted as conclusive: exists=%v breaker=%#v", exists, breaker)
				}
			})
		}
	}
}

func TestAdaptiveBreakerInvalidRawTaxonomy429FallsBackToBare429(t *testing.T) {
	raw := providererror.Detail{TaxonomyVersion: 1, Class: "unknown_class", Scope: "unknown_scope"}
	failure := executionFailure{Status: http.StatusTooManyRequests, Provider: &raw}
	if _, reviewed := adaptiveEdgeGateReviewedProviderDetail(failure); reviewed {
		t.Fatal("invalid raw taxonomy passed review")
	}
	if !adaptiveEdgeGateQuotaFailure(failure) || adaptiveEdgeGateAccountWideQuotaFailure(failure) {
		t.Fatal("invalid raw taxonomy + 429 must use bare model-scoped 429 semantics")
	}
}

func TestAdaptiveBreakerRecoveryQuotaNeverShortensExistingUntil(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	originalUntil := now.Add(time.Hour)
	key := adaptiveEdgeGateBreakerKey("claude", "until-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "until-auth", Provider: "claude", Model: "claude-opus-5", Until: originalUntil, Generation: 131, EvidenceRevision: 131}
	adaptiveEdgeGateRuntime.NextGeneration = 131
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 131
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("until-auth", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	recovery := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(recovery, now); !acquired || failure != nil {
		t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
	}
	observeAdaptiveEdgeGateOutcome(recovery, false, executionFailure{Status: http.StatusTooManyRequests}, now)
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if breaker.Until.Before(originalUntil) {
		t.Fatalf("recovery shortened Until: got=%v want>=%v", breaker.Until, originalUntil)
	}
	peer := adaptiveEdgeGateTestAttempt("until-auth", "claude-opus-5", 50, 50, now)
	peer.AdaptiveAllocatorMode = "breaker"
	peer.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, peer, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(peer, now.Add(31*time.Second))
	if got := peer.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipTripped {
		t.Fatalf("peer after 31s decision=%q", got)
	}
}

func TestAdaptiveBreakerStaleModelProofAccountQuotaRescopes(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Now().UTC()
	modelKey := adaptiveEdgeGateBreakerKey("claude", "stale-account", "claude-opus-5")
	accountKey := adaptiveEdgeGateBreakerKey("claude", "stale-account", "")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[modelKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "stale-account", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(time.Minute), Generation: 141, EvidenceRevision: 142,
		RecoveryInFlight: true, RecoveryLeaseID: 601, RecoveryUsed: true,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 141
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 142
	adaptiveEdgeGateRuntime.Unlock()
	state := &adaptiveEdgeGateAttemptState{
		authIndex: "stale-account", provider: "claude", model: "claude-opus-5",
		started: true, decision: adaptiveEdgeGateDecisionProbe,
		recoveryBreakerKey: modelKey, recoveryBreakerGeneration: 141,
		recoveryEvidenceRevision: 141, recoveryLeaseID: 601,
	}
	attempt := executionAttempt{AdaptiveShadow: true, AdaptiveAllocatorMode: "breaker", AdaptiveEdgeGate: state}
	observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "120", Provider: &providererror.Detail{
		TaxonomyVersion: providererror.FailureTaxonomyV1, Class: providererror.ClassQuota, Scope: providererror.ScopeAccount,
	}}, now)
	adaptiveEdgeGateRuntime.Lock()
	_, modelExists := adaptiveEdgeGateRuntime.Breakers[modelKey]
	account, accountExists := adaptiveEdgeGateRuntime.Breakers[accountKey]
	adaptiveEdgeGateRuntime.Unlock()
	if modelExists || !accountExists || !account.RecoveryUsed || account.EvidenceRevision <= 142 {
		t.Fatalf("stale account quota rescope model=%v account=%#v", modelExists, account)
	}
	cfg := adaptiveBreakerTestConfig(t)
	other := adaptiveEdgeGateTestAttempt("stale-account", "claude-haiku-5", 50, 50, now)
	other.AdaptiveAllocatorMode = "breaker"
	other.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, other, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(other, now.Add(time.Second))
	if got := other.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipTripped {
		t.Fatalf("account evidence did not block other model: %q", got)
	}
}

func TestAdaptiveBreakerSaturatedHalfOpenNeverFailOpensProtectedRoute(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Now().UTC()
	cfg := adaptiveBreakerTestConfig(t)
	key := adaptiveEdgeGateBreakerKey("claude", "saturated-proof", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	for index := 0; index < adaptiveEdgeGateMaximumAccounts; index++ {
		adaptiveEdgeGateRuntime.InFlight[fmt.Sprintf("other-%d", index)] = adaptiveEdgeGateLease{StartedAt: now}
	}
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "saturated-proof", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(-time.Second), Generation: 151, EvidenceRevision: 151,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 151
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 151
	adaptiveEdgeGateRuntime.Unlock()
	const workers = 20
	start := make(chan struct{})
	results := make(chan adaptiveEdgeGateSnapshot, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		attempt := adaptiveEdgeGateTestAttempt("saturated-proof", "claude-opus-5", 50, 50, now)
		attempt.AdaptiveAllocatorMode = "breaker"
		attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
		group.Add(1)
		go func(a executionAttempt) {
			defer group.Done()
			<-start
			beginAdaptiveEdgeGateShadow(a, now)
			results <- a.AdaptiveEdgeGate.snapshot()
		}(attempt)
	}
	close(start)
	group.Wait()
	close(results)
	for snapshot := range results {
		if snapshot.Decision != adaptiveEdgeGateDecisionSkipBusy || snapshot.Reason != "proof_coordination_saturated" {
			t.Fatalf("saturated half-open escaped protection: %#v", snapshot)
		}
	}
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	dropped := adaptiveEdgeGateRuntime.DroppedLeases
	saturated := adaptiveEdgeGateRuntime.Saturated
	_, protectedLease := adaptiveEdgeGateRuntime.InFlight["saturated-proof"]
	adaptiveEdgeGateRuntime.Unlock()
	if breaker.ProbeInFlight || protectedLease || !saturated || dropped != workers {
		t.Fatalf("saturation state breaker=%#v protectedLease=%v saturated=%v dropped=%d", breaker, protectedLease, saturated, dropped)
	}
	local := executionFailure{Code: "bravo_adaptive_edge_busy", Message: "proof_coordination_saturated", Status: 503, Retryable: true, RouteFallback: true}
	traces, fallback := adaptiveBreakerOutwardFailures([]executionFailureTrace{{Provider: "claude", Model: "claude-opus-5", Failure: local}}, local)
	final := finalExecutionFailure(traces, fallback)
	if strings.Contains(final.Code, "bravo_adaptive_") || strings.Contains(final.Message, "proof_coordination_saturated") {
		t.Fatalf("local saturation detail escaped outward: %#v", final)
	}
}

func TestAdaptiveBreakerResolverPrefersLiveModelOverExpiredAccount(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Now().UTC()
	cfg := adaptiveBreakerTestConfig(t)
	accountKey := adaptiveEdgeGateBreakerKey("claude", "resolver-auth", "")
	modelKey := adaptiveEdgeGateBreakerKey("claude", "resolver-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[accountKey] = adaptiveEdgeGateBreaker{AuthIndex: "resolver-auth", Provider: "claude", Until: now.Add(-time.Second), Generation: 161, EvidenceRevision: 161}
	adaptiveEdgeGateRuntime.Breakers[modelKey] = adaptiveEdgeGateBreaker{AuthIndex: "resolver-auth", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Hour), Generation: 162, EvidenceRevision: 162}
	adaptiveEdgeGateRuntime.NextGeneration = 162
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 162
	adaptiveEdgeGateRuntime.Unlock()
	current := adaptiveEdgeGateTestAttempt("resolver-auth", "claude-opus-5", 50, 50, now)
	current.AdaptiveAllocatorMode = "breaker"
	current.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, current, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(current, now)
	if got := current.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionSkipTripped || got.Reason != "model_breaker" {
		t.Fatalf("expired account hid live model breaker: %#v", got)
	}
	adaptiveEdgeGateRuntime.Lock()
	if adaptiveEdgeGateRuntime.Breakers[accountKey].ProbeInFlight {
		adaptiveEdgeGateRuntime.Unlock()
		t.Fatal("live model selection started expired account probe")
	}
	adaptiveEdgeGateRuntime.Unlock()
	other := adaptiveEdgeGateTestAttempt("resolver-auth", "claude-haiku-5", 50, 50, now)
	other.AdaptiveAllocatorMode = "breaker"
	other.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, other, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(other, now)
	if got := other.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("other model did not select expired account proof: %q", got)
	}
	cancelAdaptiveEdgeGateAttempt(other)
	afterExpiry := adaptiveEdgeGateTestAttempt("resolver-auth", "claude-opus-5", 50, 50, now)
	afterExpiry.AdaptiveAllocatorMode = "breaker"
	afterExpiry.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, afterExpiry, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(afterExpiry, now.Add(time.Hour+time.Second))
	if got := afterExpiry.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionProbe || got.Reason != "account_breaker" {
		t.Fatalf("expired proof selection was not deterministic account-first: %#v", got)
	}
}

func TestAdaptiveBreakerFailedScheduledProbeConsumesEarlyRecovery(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	cfg := adaptiveBreakerTestConfig(t)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "scheduled-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "scheduled-auth", Provider: "claude", Model: "claude-opus-5", Until: now.Add(-time.Second), Generation: 11}
	adaptiveEdgeGateRuntime.NextGeneration = 11
	adaptiveEdgeGateRuntime.Unlock()
	probe := adaptiveEdgeGateTestAttempt("scheduled-auth", "claude-opus-5", 50, 50, now)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(probe, now)
	if got := probe.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("scheduled probe decision=%q", got)
	}
	observeAdaptiveEdgeGateOutcome(probe, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "60"}, now)
	adaptiveEdgeGateRuntime.Lock()
	breaker := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if breaker.Generation != 11 || !breaker.RecoveryUsed {
		t.Fatalf("scheduled retrip reset recovery lifecycle: %#v", breaker)
	}
}

func TestAdaptiveBreakerScheduledProbeAccountRescopeMergesRecoveryUsed(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	modelKey := adaptiveEdgeGateBreakerKey("claude", "scheduled-rescope", "claude-opus-5")
	accountKey := adaptiveEdgeGateBreakerKey("claude", "scheduled-rescope", "")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[modelKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "scheduled-rescope", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(-time.Second), Generation: 21,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 21
	adaptiveEdgeGateRuntime.Unlock()
	probe := adaptiveEdgeGateTestAttempt("scheduled-rescope", "claude-opus-5", 50, 50, now)
	probe.AdaptiveAllocatorMode = "breaker"
	probe.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, probe, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(probe, now)
	if got := probe.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("scheduled model probe decision=%q", got)
	}
	stalePeer := adaptiveBreakerLastChanceAttempt(probe)

	// A concurrent account-scoped failure lands after the model probe began.
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[accountKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "scheduled-rescope", Provider: "claude", Until: now.Add(time.Minute),
		Generation: 22, RecoveryUsed: false,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 22
	adaptiveEdgeGateRuntime.Unlock()
	observeAdaptiveEdgeGateOutcome(probe, false, executionFailure{Status: http.StatusForbidden, RetryAfter: "60", Provider: &providererror.Detail{
		TaxonomyVersion: providererror.FailureTaxonomyV1, Class: providererror.ClassQuota, Scope: providererror.ScopeAccount,
	}}, now)

	adaptiveEdgeGateRuntime.Lock()
	account := adaptiveEdgeGateRuntime.Breakers[accountKey]
	_, modelExists := adaptiveEdgeGateRuntime.Breakers[modelKey]
	adaptiveEdgeGateRuntime.Unlock()
	if modelExists || account.Generation != 22 || !account.RecoveryUsed {
		t.Fatalf("scheduled account merge lost recovery evidence: modelExists=%v account=%#v", modelExists, account)
	}
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(stalePeer, now.Add(time.Second)); acquired || failure == nil || failure.Code != "bravo_adaptive_edge_tripped" {
		t.Fatalf("stale model peer crossed merged account breaker: acquired=%v failure=%#v", acquired, failure)
	}
}

func TestAdaptiveBreakerPredispatchCancelReleasesRecoveryGeneration(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "cancel-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{AuthIndex: "cancel-auth", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute), Generation: 1}
	adaptiveEdgeGateRuntime.NextGeneration = 1
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("cancel-auth", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	first := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(first, now); !acquired || failure != nil {
		t.Fatalf("first recovery acquired=%v failure=%#v", acquired, failure)
	}
	cancelAdaptiveEdgeGateAttempt(first)
	second := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(second, now.Add(time.Second)); !acquired || failure != nil {
		t.Fatalf("predispatch cancel did not release recovery: acquired=%v failure=%#v", acquired, failure)
	}
}

func TestAdaptiveBreakerRecoveryAccountFailureRescopesWithoutReset(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	modelKey := adaptiveEdgeGateBreakerKey("claude", "rescope-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[modelKey] = adaptiveEdgeGateBreaker{AuthIndex: "rescope-auth", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute), Generation: 7}
	adaptiveEdgeGateRuntime.NextGeneration = 7
	adaptiveEdgeGateRuntime.Unlock()
	base := adaptiveEdgeGateTestAttempt("rescope-auth", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	recovery := adaptiveBreakerLastChanceAttempt(base)
	peer := adaptiveBreakerLastChanceAttempt(base)
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(recovery, now); !acquired || failure != nil {
		t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
	}
	accountKey := adaptiveEdgeGateBreakerKey("claude", "rescope-auth", "")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[accountKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "rescope-auth", Provider: "claude", Until: now.Add(2 * time.Minute),
		Generation: 9, RecoveryInFlight: true, RecoveryStartedAt: now, RecoveryUsed: true,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 9
	adaptiveEdgeGateRuntime.Unlock()
	observeAdaptiveEdgeGateOutcome(recovery, false, executionFailure{Status: http.StatusForbidden, RetryAfter: "60", Provider: &providererror.Detail{
		TaxonomyVersion: providererror.FailureTaxonomyV1, Class: providererror.ClassQuota, Scope: providererror.ScopeAccount,
	}}, now.Add(time.Second))
	adaptiveEdgeGateRuntime.Lock()
	_, modelExists := adaptiveEdgeGateRuntime.Breakers[modelKey]
	account, accountExists := adaptiveEdgeGateRuntime.Breakers[accountKey]
	adaptiveEdgeGateRuntime.Unlock()
	if modelExists || !accountExists || account.Generation != 9 || !account.RecoveryUsed || !account.RecoveryInFlight {
		t.Fatalf("account rescope modelExists=%v account=%#v", modelExists, account)
	}
	if _, acquired, failure := acquireAdaptiveBreakerEnforcementLease(peer, now.Add(2*time.Second)); acquired || failure == nil || failure.Code != "bravo_adaptive_edge_tripped" {
		t.Fatalf("stale model recovery crossed account rescope: acquired=%v failure=%#v", acquired, failure)
	}
}
