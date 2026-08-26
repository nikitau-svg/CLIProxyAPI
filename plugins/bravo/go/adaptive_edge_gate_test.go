package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveEdgeGateGuardedNeverQueuesAndReleasesOnOutcome(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	first := adaptiveEdgeGateTestAttempt("shared-auth", "claude-fable-5", 5, 50, now)
	second := adaptiveEdgeGateTestAttempt("shared-auth", "claude-fable-5", 5, 50, now)

	beginAdaptiveEdgeGateShadow(first, now)
	beginAdaptiveEdgeGateShadow(second, now)
	firstSnapshot := first.AdaptiveEdgeGate.snapshot()
	secondSnapshot := second.AdaptiveEdgeGate.snapshot()
	if firstSnapshot.State != adaptiveEdgeGateStateGuarded ||
		firstSnapshot.Decision != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("first guarded decision=%#v", firstSnapshot)
	}
	if secondSnapshot.State != adaptiveEdgeGateStateGuarded ||
		secondSnapshot.Decision != adaptiveEdgeGateDecisionSkipBusy {
		t.Fatalf("second guarded decision=%#v", secondSnapshot)
	}

	// Releasing the allocator lease alone must not create a race window: the
	// simulated gate remains held until the classified provider outcome.
	released := false
	wrapped := wrapAdaptiveShadowLease(first, func(bool) { released = true })
	wrapped(true)
	if !released {
		t.Fatal("base allocator lease was not released")
	}
	thirdWhileUnsettled := adaptiveEdgeGateTestAttempt("shared-auth", "claude-fable-5", 5, 50, now)
	beginAdaptiveEdgeGateShadow(thirdWhileUnsettled, now)
	if got := thirdWhileUnsettled.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionSkipBusy {
		t.Fatalf("unsettled gate decision=%q, want skip busy", got)
	}

	observeAdaptiveEdgeGateOutcome(first, true, executionFailure{}, now.Add(time.Second))
	third := adaptiveEdgeGateTestAttempt("shared-auth", "claude-fable-5", 5, 50, now)
	beginAdaptiveEdgeGateShadow(third, now.Add(2*time.Second))
	if got := third.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("released gate decision=%q, want dispatch", got)
	}
}

func TestAdaptiveEdgeGateGreenAllowsConcurrentAttempts(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 8; index++ {
		attempt := adaptiveEdgeGateTestAttempt("green-auth", "gpt-5.6-sol", 80, 70, now)
		beginAdaptiveEdgeGateShadow(attempt, now)
		snapshot := attempt.AdaptiveEdgeGate.snapshot()
		if snapshot.State != adaptiveEdgeGateStateGreen || snapshot.Decision != adaptiveEdgeGateDecisionDispatch {
			t.Fatalf("attempt %d snapshot=%#v", index, snapshot)
		}
	}
	view := adaptiveEdgeGateSummary(defaultPluginConfig(), []string{"green-auth"}, now)
	if view.InFlightGuards != 0 || view.TrackedBreakers != 0 || view.QueuesRequests || view.RoutingEnforced {
		t.Fatalf("green view=%#v", view)
	}
}

func TestAdaptiveEdgeGateTripsAndAllowsExactlyOneHalfOpenProbe(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	failed := adaptiveEdgeGateTestAttempt("trip-auth", "claude-fable-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(failed, now)
	failure := executionFailure{
		Code:       "bravo_subscription_quota_exhausted",
		Status:     http.StatusTooManyRequests,
		Retryable:  true,
		RetryAfter: "30",
		Provider:   &providererror.Detail{Scope: "model"},
	}
	observeAdaptiveEdgeGateOutcome(failed, false, failure, now)
	if got := failed.AdaptiveEdgeGate.snapshot().OutcomeTransition; got != "tripped_model" {
		t.Fatalf("transition=%q, want tripped_model", got)
	}

	tripped := adaptiveEdgeGateTestAttempt("trip-auth", "claude-fable-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(tripped, now.Add(time.Second))
	trippedSnapshot := tripped.AdaptiveEdgeGate.snapshot()
	if trippedSnapshot.State != adaptiveEdgeGateStateTripped ||
		trippedSnapshot.Decision != adaptiveEdgeGateDecisionSkipTripped ||
		trippedSnapshot.TripRemainingSeconds != 29 {
		t.Fatalf("tripped snapshot=%#v", trippedSnapshot)
	}

	probe := adaptiveEdgeGateTestAttempt("trip-auth", "claude-fable-5", 50, 50, now)
	competing := adaptiveEdgeGateTestAttempt("trip-auth", "claude-fable-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(probe, now.Add(31*time.Second))
	beginAdaptiveEdgeGateShadow(competing, now.Add(31*time.Second))
	if got := probe.AdaptiveEdgeGate.snapshot(); got.State != adaptiveEdgeGateStateHalfOpen ||
		got.Decision != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("probe=%#v", got)
	}
	if got := competing.AdaptiveEdgeGate.snapshot(); got.State != adaptiveEdgeGateStateHalfOpen ||
		got.Decision != adaptiveEdgeGateDecisionSkipBusy {
		t.Fatalf("competing probe=%#v", got)
	}

	observeAdaptiveEdgeGateOutcome(probe, true, executionFailure{}, now.Add(32*time.Second))
	if got := probe.AdaptiveEdgeGate.snapshot().OutcomeTransition; got != "reopened" {
		t.Fatalf("probe transition=%q, want reopened", got)
	}
	reopened := adaptiveEdgeGateTestAttempt("trip-auth", "claude-fable-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(reopened, now.Add(33*time.Second))
	if got := reopened.AdaptiveEdgeGate.snapshot(); got.State != adaptiveEdgeGateStateGreen ||
		got.Decision != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("reopened=%#v", got)
	}
}

func TestAdaptiveEdgeGateHalfOpenBurstSelectsOneProbeWithoutWaiting(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	failed := adaptiveEdgeGateTestAttempt("burst-auth", "claude-fable-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(failed, now)
	observeAdaptiveEdgeGateOutcome(failed, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: 429, RetryAfter: "1",
		Provider: &providererror.Detail{Scope: "model"},
	}, now)

	const workers = 64
	start := make(chan struct{})
	decisions := make(chan string, workers)
	for index := 0; index < workers; index++ {
		attempt := adaptiveEdgeGateTestAttempt("burst-auth", "claude-fable-5", 50, 50, now)
		go func() {
			<-start
			beginAdaptiveEdgeGateShadow(attempt, now.Add(2*time.Second))
			decisions <- attempt.AdaptiveEdgeGate.snapshot().Decision
		}()
	}
	close(start)
	probes, skipped := 0, 0
	for index := 0; index < workers; index++ {
		switch <-decisions {
		case adaptiveEdgeGateDecisionProbe:
			probes++
		case adaptiveEdgeGateDecisionSkipBusy:
			skipped++
		default:
			t.Fatal("half-open burst produced an unexpected decision")
		}
	}
	if probes != 1 || skipped != workers-1 {
		t.Fatalf("probes=%d skipped=%d, want 1/%d", probes, skipped, workers-1)
	}
}

func TestAdaptiveEdgeGateUsesModelAndAccountBreakerScopes(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	modelFailure := adaptiveEdgeGateTestAttempt("scoped-auth", "claude-opus-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(modelFailure, now)
	observeAdaptiveEdgeGateOutcome(modelFailure, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: 429, RetryAfter: "60",
		Provider: &providererror.Detail{Scope: "model"},
	}, now)
	otherModel := adaptiveEdgeGateTestAttempt("scoped-auth", "claude-haiku-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(otherModel, now.Add(time.Second))
	if got := otherModel.AdaptiveEdgeGate.snapshot().Decision; got != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("model-scoped breaker leaked to another model: %q", got)
	}
	observeAdaptiveEdgeGateOutcome(otherModel, true, executionFailure{}, now.Add(2*time.Second))

	accountFailure := adaptiveEdgeGateTestAttempt("scoped-auth", "claude-haiku-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(accountFailure, now.Add(2*time.Second))
	observeAdaptiveEdgeGateOutcome(accountFailure, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: http.StatusForbidden,
		AccountWide: true, RetryAfter: "60",
	}, now.Add(2*time.Second))
	allModels := adaptiveEdgeGateTestAttempt("scoped-auth", "claude-sonnet-5", 50, 50, now)
	beginAdaptiveEdgeGateShadow(allModels, now.Add(3*time.Second))
	if got := allModels.AdaptiveEdgeGate.snapshot(); got.Decision != adaptiveEdgeGateDecisionSkipTripped ||
		got.Reason != "account_breaker" {
		t.Fatalf("account breaker=%#v", got)
	}
}

func TestAdaptiveEdgeGateUnknownQuotaIsGuardedAndRuntimeSaturationFailsOpen(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	cfg := defaultPluginConfig()
	attempt := executionAttempt{
		Candidate:      candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth:           pluginapi.HostAuthFileEntry{AuthIndex: "unknown-auth", Provider: "claude"},
		Primary:        true,
		AdaptiveShadow: true,
	}
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(
		cfg, attempt, credentialQuotaState{Confidence: "unknown"}, tariffConfig{}, now,
	)
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.State != adaptiveEdgeGateStateGuarded ||
		snapshot.Reason != "quota_unconfirmed" || snapshot.QuotaConfirmed {
		t.Fatalf("unknown snapshot=%#v", snapshot)
	}

	adaptiveEdgeGateRuntime.Lock()
	for index := 0; index < adaptiveEdgeGateMaximumAccounts; index++ {
		adaptiveEdgeGateRuntime.InFlight[fmt.Sprintf("busy-%d", index)] = adaptiveEdgeGateLease{StartedAt: now}
	}
	adaptiveEdgeGateRuntime.Unlock()
	beginAdaptiveEdgeGateShadow(attempt, now)
	snapshot := attempt.AdaptiveEdgeGate.snapshot()
	if snapshot.Decision != adaptiveEdgeGateDecisionDispatch || snapshot.Reason != "runtime_saturated_fail_open" {
		t.Fatalf("saturated decision=%#v", snapshot)
	}
	view := adaptiveEdgeGateSummary(defaultPluginConfig(), nil, now)
	if !view.Saturated || view.DroppedLeases != 1 || view.RoutingEnforced || view.QueuesRequests {
		t.Fatalf("saturated view=%#v", view)
	}
}

func adaptiveEdgeGateTestAttempt(
	authIndex, model string,
	sessionRemaining, weeklyRemaining float64,
	now time.Time,
) executionAttempt {
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	attempt := executionAttempt{
		Candidate:                  candidate{Provider: "claude", Model: model},
		Auth:                       pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		Primary:                    true,
		AdaptiveShadow:             true,
		AdaptiveReservationPercent: 1,
	}
	quota := normalizedQuotaState(credentialQuotaState{
		Confidence:  "confirmed",
		ConfirmedAt: now,
		Session:     quotaWindowState{RemainingPercent: sessionRemaining},
		Weekly:      quotaWindowState{RemainingPercent: weeklyRemaining},
	})
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, quota, tariffConfig{}, now)
	return attempt
}
