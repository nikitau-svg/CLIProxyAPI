package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestStreamBootstrapPanicDoesNotAcceptProtectedRecoveryProbe(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	cfg := adaptiveBreakerTestConfig(t)
	currentConfig.Store(cfg)
	now := time.Now().UTC()
	key := adaptiveEdgeGateBreakerKey("claude", "panic-recovery-auth", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "panic-recovery-auth", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(time.Minute), Generation: 41,
	}
	adaptiveEdgeGateRuntime.NextGeneration = 41
	adaptiveEdgeGateRuntime.Unlock()

	base := adaptiveEdgeGateTestAttempt("panic-recovery-auth", "claude-opus-5", 50, 50, now)
	base.AdaptiveAllocatorMode = "breaker"
	base.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, base, credentialQuotaState{}, tariffConfig{}, now)
	beginAdaptiveEdgeGateShadow(base, now)
	recovery := adaptiveBreakerLastChanceAttempt(base)
	release, acquired, failure := acquireExecutionAttemptLease(recovery)
	if !acquired || failure != nil {
		t.Fatalf("recovery acquired=%v failure=%#v", acquired, failure)
	}

	var releaseMu sync.Mutex
	releaseCommits := make([]bool, 0, 1)
	trackedRelease := func(commit bool) {
		releaseMu.Lock()
		releaseCommits = append(releaseCommits, commit)
		releaseMu.Unlock()
		release(commit)
	}
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelExecuteStream {
			panic("deterministic bootstrap panic")
		}
		return mustBravoJSON(t, map[string]any{}), nil
	})
	run := launchBravoStreamAttempt(
		rpcExecutorRequest{}, recovery, protocolOpenAI, "claude-opus-5",
		[]byte(`{"model":"claude-opus-5","messages":[]}`), "", false,
		trackedRelease, nil,
	)
	var result bravoStreamBootstrapResult
	select {
	case result = <-run.results:
	case <-time.After(time.Second):
		t.Fatal("bootstrap panic did not settle")
	}
	if result.failure == nil || result.failure.Code != "bravo_stream_attempt_aborted" || result.accepted {
		t.Fatalf("panic result=%#v, want unaccepted local abort", result)
	}
	finishBravoStreamAttemptFailure(run, *result.failure, result.accepted)
	if run.attempt.AdaptiveProviderAccepted {
		t.Fatal("bootstrap panic marked provider accepted")
	}
	releaseMu.Lock()
	commits := append([]bool(nil), releaseCommits...)
	releaseMu.Unlock()
	if len(commits) != 1 || !commits[0] {
		t.Fatalf("allocator releases=%v, want one conservative commit", commits)
	}

	observeAdaptiveEdgeGateOutcome(run.attempt, false, *result.failure, now.Add(time.Second))
	adaptiveEdgeGateRuntime.Lock()
	breaker, exists := adaptiveEdgeGateRuntime.Breakers[key]
	adaptiveEdgeGateRuntime.Unlock()
	if !exists || breaker.Generation != 41 || breaker.RecoveryInFlight || !breaker.RecoveryUsed ||
		!breaker.Until.After(now.Add(time.Second)) {
		t.Fatalf("panic recovery breaker=%#v exists=%v", breaker, exists)
	}
	peer := adaptiveBreakerLastChanceAttempt(base)
	if _, peerAcquired, peerFailure := acquireAdaptiveBreakerEnforcementLease(peer, now.Add(2*time.Second)); peerAcquired || peerFailure == nil || peerFailure.Code != "bravo_adaptive_edge_tripped" {
		t.Fatalf("panic reopened protected route: acquired=%v failure=%#v", peerAcquired, peerFailure)
	}
}
