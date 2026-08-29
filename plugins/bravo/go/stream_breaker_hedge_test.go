package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestScheduledBreakerProofPrimaryNeverSpawnsStreamHedge(t *testing.T) {
	req, auths := installScheduledProofStreamTest(t, true)
	proofStarted := make(chan struct{})
	releaseProof := make(chan struct{})
	var proofOnce sync.Once
	var neighborCalls atomic.Int32
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostCallbackFork:
			return mustBravoJSON(t, pluginapi.HostCallbackScopeResponse{HostCallbackID: "proof-primary-child"}), nil
		case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostCallbackCommit:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			if call.ForcedProvider == "claude" {
				proofOnce.Do(func() { close(proofStarted) })
				<-releaseProof
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "proof-primary"}), nil
			}
			neighborCalls.Add(1)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "unexpected-neighbor"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamClose, pluginabi.MethodHostStreamEmit:
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); runBravoStream(req, "scheduled-proof-primary") }()
	waitStreamHedgeSignal(t, proofStarted, time.Second, "scheduled proof did not start")
	time.Sleep(1200 * time.Millisecond)
	if got := neighborCalls.Load(); got != 0 {
		close(releaseProof)
		t.Fatalf("protected primary spawned %d hedge calls", got)
	}
	adaptiveEdgeGateRuntime.Lock()
	_, leaseHeld := adaptiveEdgeGateRuntime.InFlight["claude-last-chance"]
	adaptiveEdgeGateRuntime.Unlock()
	if !leaseHeld {
		close(releaseProof)
		t.Fatal("proof per-auth lease released before bootstrap returned")
	}
	close(releaseProof)
	waitStreamHedgeSignal(t, done, time.Second, "protected primary did not settle")
	adaptiveEdgeGateRuntime.Lock()
	_, leaseHeld = adaptiveEdgeGateRuntime.InFlight["claude-last-chance"]
	adaptiveEdgeGateRuntime.Unlock()
	if leaseHeld {
		t.Fatal("proof per-auth lease remained after its own result")
	}
}

func TestScheduledBreakerProofDeferredAsHedgeRemainsSequentiallyAvailable(t *testing.T) {
	req, auths := installScheduledProofStreamTest(t, false)
	primaryStarted := make(chan struct{})
	releasePrimary := make(chan struct{})
	proofStarted := make(chan struct{})
	var primaryOnce, proofOnce sync.Once
	var proofCalls atomic.Int32
	var sequentialLeaseHeld atomic.Bool
	var sequentialProbeMarked atomic.Bool
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostCallbackFork:
			return mustBravoJSON(t, pluginapi.HostCallbackScopeResponse{HostCallbackID: "proof-deferred-child"}), nil
		case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostCallbackCommit:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			if call.ForcedProvider == "codex" {
				primaryOnce.Do(func() { close(primaryStarted) })
				<-releasePrimary
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			proofCalls.Add(1)
			adaptiveEdgeGateRuntime.Lock()
			_, held := adaptiveEdgeGateRuntime.InFlight["claude-last-chance"]
			probeMarked := false
			for _, breaker := range adaptiveEdgeGateRuntime.Breakers {
				if breaker.AuthIndex == "claude-last-chance" && breaker.ProbeInFlight {
					probeMarked = true
					break
				}
			}
			adaptiveEdgeGateRuntime.Unlock()
			sequentialLeaseHeld.Store(held)
			sequentialProbeMarked.Store(probeMarked)
			proofOnce.Do(func() { close(proofStarted) })
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "sequential-proof"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamClose, pluginabi.MethodHostStreamEmit:
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); runBravoStream(req, "scheduled-proof-deferred") }()
	waitStreamHedgeSignal(t, primaryStarted, time.Second, "slow primary did not start")
	time.Sleep(1200 * time.Millisecond)
	if got := proofCalls.Load(); got != 0 {
		close(releasePrimary)
		t.Fatalf("protected proof launched as hedge %d times", got)
	}
	close(releasePrimary)
	waitStreamHedgeSignal(t, proofStarted, time.Second, "deferred proof was not retained for sequential routing")
	waitStreamHedgeSignal(t, done, time.Second, "sequential proof did not settle")
	if got := proofCalls.Load(); got != 1 {
		t.Fatalf("sequential proof calls=%d, want 1", got)
	}
	if !sequentialLeaseHeld.Load() {
		t.Fatal("deferred proof did not reacquire its per-auth lease sequentially")
	}
	if !sequentialProbeMarked.Load() {
		t.Fatal("deferred proof did not reacquire the breaker ProbeInFlight token")
	}
}

func installScheduledProofStreamTest(t *testing.T, proofFirst bool) (rpcExecutorRequest, []pluginapi.HostAuthFileEntry) {
	t.Helper()
	req, auths := installAdaptiveBreakerCoordinatorTest(t, true, 2)
	cfg := loadedConfig()
	cfg.FallbackHedgeDelaySeconds = 1
	model := cfg.Models["breaker-coordinator"]
	if proofFirst {
		model.Candidates[0].Priority, model.Candidates[1].Priority = 100, 90
	} else {
		model.Candidates[0], model.Candidates[1] = model.Candidates[1], model.Candidates[0]
		model.Candidates[0].Priority, model.Candidates[1].Priority = 100, 90
	}
	cfg.Models["breaker-coordinator"] = model
	if cfg.BaseModels != nil {
		cfg.BaseModels["breaker-coordinator"] = model
	}
	currentConfig.Store(cfg)
	adaptiveEdgeGateRuntime.Lock()
	for key, breaker := range adaptiveEdgeGateRuntime.Breakers {
		breaker.Until = time.Now().UTC().Add(-time.Second)
		breaker.ProbeInFlight = false
		breaker.RecoveryInFlight = false
		breaker.RecoveryUsed = false
		adaptiveEdgeGateRuntime.Breakers[key] = breaker
	}
	adaptiveEdgeGateRuntime.Unlock()
	return req, auths
}

func TestObserveCounterfactualProofDoesNotDisableStreamHedging(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	now := time.Now().UTC()
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	attempt := adaptiveEdgeGateTestAttempt("observe-proof", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveAllocatorMode = "observe"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	key := adaptiveEdgeGateBreakerKey("claude", "observe-proof", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[key] = adaptiveEdgeGateBreaker{
		AuthIndex: "observe-proof", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(-time.Second), Generation: 1, EvidenceRevision: 1,
	}
	adaptiveEdgeGateRuntime.Unlock()
	beginAdaptiveEdgeGateShadow(attempt, now)
	snapshot := attempt.AdaptiveEdgeGate.snapshot()
	if snapshot.Decision != adaptiveEdgeGateDecisionProbe || snapshot.Enforce {
		t.Fatalf("observe counterfactual snapshot=%#v", snapshot)
	}
	if streamAttemptIsProtectedBreakerProof(attempt) {
		t.Fatal("observe counterfactual proof disabled ordinary stream hedging")
	}

	enforceCfg := defaultPluginConfig()
	enforceCfg.AdaptiveAllocatorMode = "enforce"
	enforced := adaptiveEdgeGateTestAttempt("enforce-proof", "claude-opus-5", 50, 50, now)
	enforced.AdaptiveAllocatorMode = "enforce"
	enforced.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(enforceCfg, enforced, credentialQuotaState{}, tariffConfig{}, now)
	enforceKey := adaptiveEdgeGateBreakerKey("claude", "enforce-proof", "claude-opus-5")
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[enforceKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "enforce-proof", Provider: "claude", Model: "claude-opus-5",
		Until: now.Add(-time.Second), Generation: 2, EvidenceRevision: 2,
	}
	adaptiveEdgeGateRuntime.Unlock()
	beginAdaptiveEdgeGateShadow(enforced, now)
	enforcedSnapshot := enforced.AdaptiveEdgeGate.snapshot()
	if enforcedSnapshot.Decision != adaptiveEdgeGateDecisionProbe || !enforcedSnapshot.Enforce ||
		!streamAttemptIsProtectedBreakerProof(enforced) {
		t.Fatalf("enforced half-open proof was not protected: %#v", enforcedSnapshot)
	}
}

func TestDeferredCombinedResetAndBreakerProofReacquiresBothTokensSequentially(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"combined-proof": quota})
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	cfg := loadedConfig()
	cfg.AdaptiveAllocatorMode = "breaker"
	currentConfig.Store(cfg)

	attempt := allocatorBypassProbeTestAttempt("combined-proof", now)
	attempt.AdaptiveShadow = true
	attempt.AdaptiveAllocatorMode = "breaker"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, quota, tariffConfig{}, now)
	breakerKey := adaptiveEdgeGateBreakerKey("claude", "combined-proof", attempt.Candidate.Model)
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[breakerKey] = adaptiveEdgeGateBreaker{
		AuthIndex: "combined-proof", Provider: "claude", Model: attempt.Candidate.Model,
		Until: now.Add(-time.Second), Generation: 7, EvidenceRevision: 7,
	}
	adaptiveEdgeGateRuntime.Unlock()

	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil || !streamAttemptIsProtectedBreakerProof(attempt) {
		t.Fatalf("combined hedge acquire=%v failure=%#v snapshot=%#v", acquired, failure, attempt.AdaptiveEdgeGate.snapshot())
	}
	oldResetState := attempt.AllocatorBypassProbe
	release(false)
	cancelAdaptiveEdgeGateAttempt(attempt)
	fresh := freshStreamDeferredBreakerProofAttempt(attempt, now.Add(time.Millisecond))
	if fresh.AllocatorBypassProbe == oldResetState || fresh.AllocatorBypassProbe == nil {
		t.Fatal("deferred combined proof retained settled allocator reset state")
	}

	sequentialRelease, sequentialAcquired, sequentialFailure := acquireExecutionAttemptLease(fresh)
	if !sequentialAcquired || sequentialFailure != nil || !streamAttemptIsProtectedBreakerProof(fresh) {
		t.Fatalf("combined sequential acquire=%v failure=%#v snapshot=%#v", sequentialAcquired, sequentialFailure, fresh.AdaptiveEdgeGate.snapshot())
	}
	adaptiveEdgeGateRuntime.Lock()
	_, authLeaseHeld := adaptiveEdgeGateRuntime.InFlight["combined-proof"]
	breaker := adaptiveEdgeGateRuntime.Breakers[breakerKey]
	adaptiveEdgeGateRuntime.Unlock()
	allocatorBypassProbeRuntime.Lock()
	resetTokenHeld := false
	for _, entry := range allocatorBypassProbeRuntime.Entries {
		if entry.InFlight && !entry.Consumed {
			resetTokenHeld = true
			break
		}
	}
	allocatorBypassProbeRuntime.Unlock()
	if !authLeaseHeld || !breaker.ProbeInFlight || !resetTokenHeld {
		t.Fatalf("combined sequential tokens auth=%v breaker=%#v reset=%v", authLeaseHeld, breaker, resetTokenHeld)
	}
	var providerCalls atomic.Int32
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelExecuteStream {
			providerCalls.Add(1)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK, StreamID: "combined-sequential-proof",
			}), nil
		}
		return mustBravoJSON(t, map[string]any{}), nil
	})
	// This is the sole provider dispatch; the deferred hedge above never marked
	// or consumed either proof.
	markAllocatorBypassProbeDispatched(fresh, now.Add(2*time.Millisecond))
	run := launchBravoStreamAttempt(
		rpcExecutorRequest{}, fresh, protocolOpenAI, fresh.Candidate.Model,
		[]byte(`{"model":"claude-sonnet-5","messages":[]}`), "", false,
		sequentialRelease, nil,
	)
	select {
	case result := <-run.results:
		if result.response == nil || result.failure != nil {
			t.Fatalf("combined sequential provider result=%#v", result)
		}
		run.attempt.AdaptiveProviderAccepted = true
		run.release(true)
		observeAdaptiveEdgeGateOutcome(run.attempt, true, executionFailure{}, now.Add(3*time.Millisecond))
	case <-time.After(time.Second):
		t.Fatal("combined sequential provider did not return")
	}
	if got := providerCalls.Load(); got != 1 {
		t.Fatalf("combined sequential provider calls=%d, want exactly 1", got)
	}
}
