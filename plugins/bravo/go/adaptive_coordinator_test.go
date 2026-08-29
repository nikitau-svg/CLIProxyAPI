package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveBreakerLastChanceEligibilityIsNarrow(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "breaker"
	attempt := executionAttempt{AdaptiveAllocatorMode: cfg.AdaptiveAllocatorMode}
	for _, code := range []string{"bravo_adaptive_edge_tripped", "bravo_adaptive_edge_busy"} {
		if !adaptiveBreakerLastChanceEligible(attempt, executionFailure{Code: code}) {
			t.Fatalf("breaker local skip %q was not retained", code)
		}
	}
	if adaptiveBreakerLastChanceEligible(attempt, executionFailure{Code: "rate_limited"}) {
		t.Fatal("provider failure incorrectly became a breaker last chance")
	}
	cfg.AdaptiveAllocatorMode = "observe"
	attempt.AdaptiveAllocatorMode = cfg.AdaptiveAllocatorMode
	if adaptiveBreakerLastChanceEligible(attempt, executionFailure{Code: "bravo_adaptive_edge_tripped"}) {
		t.Fatal("observe mode changed coordinator routing")
	}
}

func TestAdaptiveBreakerLastChanceBypassesOnlyBreakerAndKeepsAccounting(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 80)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"last-chance-auth": quota})
	cfg := loadedConfig()
	cfg.AdaptiveAllocatorMode = "breaker"
	currentConfig.Store(cfg)
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)

	attempt := adaptiveEnforcementAttempt("last-chance-auth", true, 1, quota, now)
	attempt.AllocatorManaged = true
	attempt.ReservationPercent = 1
	beginAdaptiveEdgeGateShadow(attempt, now)
	observeAdaptiveEdgeGateOutcome(attempt, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: 429, RetryAfter: "60", Retryable: true,
	}, now)
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(
		cfg, attempt, normalizedQuotaState(quota), tariffByID(cfg, "x1"), now.Add(time.Second),
	)
	if release, acquired, failure := acquireExecutionAttemptLease(attempt); acquired || failure == nil ||
		failure.Code != "bravo_adaptive_edge_tripped" {
		if acquired {
			release(false)
		}
		t.Fatalf("ordinary breaker attempt acquired=%v failure=%#v", acquired, failure)
	}

	lastChance := adaptiveBreakerLastChanceAttempt(attempt)
	release, acquired, failure := acquireExecutionAttemptLease(lastChance)
	if !acquired || failure != nil {
		t.Fatalf("last chance acquired=%v failure=%#v", acquired, failure)
	}
	allocatorRuntime.Lock()
	inFlight := allocatorRuntime.InFlightPercent["last-chance-auth"]
	allocatorRuntime.Unlock()
	if inFlight != 1 {
		t.Fatalf("ordinary allocator accounting=%v, want 1", inFlight)
	}
	release(false)
	allocatorRuntime.Lock()
	inFlight = allocatorRuntime.InFlightPercent["last-chance-auth"]
	allocatorRuntime.Unlock()
	if inFlight != 0 {
		t.Fatalf("last chance leaked allocator accounting=%v", inFlight)
	}
}

func TestAdaptiveBreakerLocalSkipNeverBecomesOutwardError(t *testing.T) {
	local := executionFailure{
		Code: "bravo_adaptive_edge_tripped", Message: "internal breaker detail", Status: 503,
		Retryable: true, RouteFallback: true,
	}
	traces, fallback := adaptiveBreakerOutwardFailures([]executionFailureTrace{
		{Provider: "claude", Model: "claude-fable-5", Failure: local},
	}, local)
	final := finalExecutionFailure(traces, fallback)
	if strings.Contains(final.Code, "bravo_adaptive_") || strings.Contains(final.Message, "bravo_adaptive_") ||
		strings.Contains(final.Message, "internal breaker detail") {
		t.Fatalf("local breaker escaped final envelope: %#v", final)
	}
	if final.Code != "bravo_route_temporarily_unavailable" || final.Status != 503 {
		t.Fatalf("budget-exhausted fallback=%#v", final)
	}
	// Sanitization is independent of the current config snapshot: a request
	// planned before a hot reload cannot expose any other experimental code.
	quotaWithheld := executionFailure{Code: "bravo_adaptive_quota_withheld", Message: "internal", Status: 429}
	traces, fallback = adaptiveBreakerOutwardFailures([]executionFailureTrace{{Failure: quotaWithheld}}, quotaWithheld)
	final = finalExecutionFailure(traces, fallback)
	if strings.Contains(final.Code, "bravo_adaptive_") || strings.Contains(final.Message, "bravo_adaptive_") {
		t.Fatalf("adaptive quota code escaped after config reload: %#v", final)
	}
}

func TestAdaptiveModeSnapshotObserveToBreakerCannotAddRouting(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	now := time.Now().UTC()

	planCfg := previous
	planCfg.AdaptiveAllocatorMode = "observe"
	attempt := adaptiveEdgeGateTestAttempt("reload-observe", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveAllocatorMode = "observe"
	attempt.AllocatorManaged = false
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(planCfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[adaptiveEdgeGateBreakerKey("claude", "reload-observe", "claude-opus-5")] = adaptiveEdgeGateBreaker{
		AuthIndex: "reload-observe", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute),
	}
	adaptiveEdgeGateRuntime.Unlock()

	reloaded := previous
	reloaded.AdaptiveAllocatorMode = "breaker"
	currentConfig.Store(reloaded)
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("observe plan gained breaker authority after reload: acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.Enforce {
		t.Fatalf("observe audit snapshot became enforced: %#v", snapshot)
	}
}

func TestAdaptiveModeSnapshotBreakerToObserveKeepsSkipAndLastChance(t *testing.T) {
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	now := time.Now().UTC()

	planCfg := previous
	planCfg.AdaptiveAllocatorMode = "breaker"
	attempt := adaptiveEdgeGateTestAttempt("reload-breaker", "claude-opus-5", 50, 50, now)
	attempt.AdaptiveAllocatorMode = "breaker"
	attempt.AllocatorManaged = false
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(planCfg, attempt, credentialQuotaState{}, tariffConfig{}, now)
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.Breakers[adaptiveEdgeGateBreakerKey("claude", "reload-breaker", "claude-opus-5")] = adaptiveEdgeGateBreaker{
		AuthIndex: "reload-breaker", Provider: "claude", Model: "claude-opus-5", Until: now.Add(time.Minute),
	}
	adaptiveEdgeGateRuntime.Unlock()

	reloaded := previous
	reloaded.AdaptiveAllocatorMode = "observe"
	currentConfig.Store(reloaded)
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_edge_tripped" {
		t.Fatalf("breaker plan lost authority after reload: acquired=%v failure=%#v", acquired, failure)
	}
	if !adaptiveBreakerLastChanceEligible(attempt, *failure) {
		t.Fatal("breaker plan lost last-chance eligibility after reload")
	}
	recorder := &routeTraceRecorder{}
	recorder.captureAdaptiveAuditAttempt(attempt, now, failure.Status, false, "failed", failure.Code)
	if !recorder.adaptiveAuditRoutingEnforced || recorder.adaptiveAuditRoutingChanges != 1 {
		t.Fatalf("breaker audit lost request snapshot: %#v", recorder)
	}
}

func TestAdaptiveBreakerHealthyNeighborPreventsLastChance(t *testing.T) {
	req, auths := installAdaptiveBreakerCoordinatorTest(t, false, 2)
	var providers []string
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK, Body: []byte(`{"model":"gpt-5.6-luna","choices":[{"message":{"content":"ok"}}]}`),
			}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	raw, errExecute := execute(mustJSONValue(t, req))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK || strings.Join(providers, ",") != "codex" {
		t.Fatalf("healthy neighbor result=%#v providers=%v", env.Error, providers)
	}
}

func TestAdaptiveBreakerCoordinatorRunsLastChanceAfterNeighbors(t *testing.T) {
	req, auths := installAdaptiveBreakerCoordinatorTest(t, false, 2)
	var providers []string
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			if call.ForcedProvider == "codex" {
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
					StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":{"message":"neighbor unavailable"}}`),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK, Body: []byte(`{"model":"claude-fable-5","choices":[{"message":{"content":"ok"}}]}`),
			}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	raw, errExecute := execute(mustJSONValue(t, req))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("last chance failed: %#v", env.Error)
	}
	if strings.Join(providers, ",") != "codex,claude" {
		t.Fatalf("provider calls=%v, want ordinary neighbor then one last chance", providers)
	}
}

func TestAdaptiveBreakerStreamCoordinatorRunsLastChanceAfterNeighbors(t *testing.T) {
	req, auths := installAdaptiveBreakerCoordinatorTest(t, true, 2)
	var providers []string
	var closed rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			if call.ForcedProvider == "codex" {
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "last-chance-upstream"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &closed)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	runBravoStream(req, "breaker-client-stream")
	if strings.Join(providers, ",") != "codex,claude" {
		t.Fatalf("stream provider calls=%v close=%#v, want ordinary neighbor then one last chance", providers, closed)
	}
	if closed.ErrorCode != "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_") ||
		strings.Contains(closed.Error, "bravo_adaptive_") {
		t.Fatalf("stream close leaked adaptive decision: %#v", closed)
	}
}

func TestAdaptiveBreakerBudgetExhaustionNeverLeaksLocalSkip(t *testing.T) {
	t.Run("nonstream", func(t *testing.T) {
		req, auths := installAdaptiveBreakerCoordinatorTest(t, false, 1)
		installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
			switch method {
			case pluginabi.MethodHostAuthList:
				return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
			case pluginabi.MethodHostModelExecute:
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
					StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":{"message":"neighbor unavailable"}}`),
				}), nil
			default:
				return mustBravoJSON(t, map[string]any{}), nil
			}
		})
		raw, errExecute := execute(mustJSONValue(t, req))
		if errExecute != nil {
			t.Fatal(errExecute)
		}
		var env envelope
		if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if env.Error == nil || strings.Contains(env.Error.Code, "bravo_adaptive_") ||
			strings.Contains(env.Error.Message, "bravo_adaptive_") {
			t.Fatalf("nonstream leaked local skip: %#v", env.Error)
		}
	})

	t.Run("stream", func(t *testing.T) {
		req, auths := installAdaptiveBreakerCoordinatorTest(t, true, 1)
		var closed rpcStreamCloseRequest
		installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
			switch method {
			case pluginabi.MethodHostAuthList:
				return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
			case pluginabi.MethodHostModelExecuteStream:
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
			case pluginabi.MethodHostStreamClose:
				decodeBravoPayload(t, payload, &closed)
				return mustBravoJSON(t, map[string]any{}), nil
			default:
				return mustBravoJSON(t, map[string]any{}), nil
			}
		})
		runBravoStream(req, "breaker-budget-stream")
		if closed.ErrorCode == "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_") ||
			strings.Contains(closed.Error, "bravo_adaptive_") {
			t.Fatalf("stream leaked local skip: %#v", closed)
		}
	})
}

func TestAdaptiveBreakerLastChanceRespectsLearnedModelBlock(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "nonstream"},
		{name: "stream", stream: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, auths := installAdaptiveBreakerCoordinatorTest(t, test.stream, 3)
			neighbor := pluginapi.HostAuthFileEntry{
				ID: "claude-model-block-neighbor", AuthIndex: "claude-model-block-neighbor", Provider: "claude",
			}
			auths = append(auths, neighbor)
			cfg := loadedConfig()
			cfg.SmartKeys[0].AllowedAuthIDs = append(cfg.SmartKeys[0].AllowedAuthIDs, neighbor.AuthIndex)
			currentConfig.Store(cfg)
			storeQuotaSnapshot(neighbor.AuthIndex, adaptiveEnforcementQuota(time.Now().UTC(), 80))

			var calls []hostModelExecutionRequest
			var closed rpcStreamCloseRequest
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
					var call hostModelExecutionRequest
					decodeBravoPayload(t, payload, &call)
					calls = append(calls, call)
					if call.ForcedProvider == "claude" {
						detail := providererror.Detail{Type: "invalid_request_error", Message: "ambiguous model rejection"}
						return nil, &hostCallError{
							Code: "model_execution_failed", Message: detail.Message, HTTPStatus: http.StatusBadRequest,
							ProviderError: &detail,
						}
					}
					if test.stream {
						return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
					}
					return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
						StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":{"message":"neighbor unavailable"}}`),
					}), nil
				case pluginabi.MethodHostStreamClose:
					decodeBravoPayload(t, payload, &closed)
					return mustBravoJSON(t, map[string]any{}), nil
				default:
					return mustBravoJSON(t, map[string]any{}), nil
				}
			})

			if test.stream {
				runBravoStream(req, "breaker-model-block-stream")
				if closed.ErrorCode == "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_") {
					t.Fatalf("stream terminal failure=%#v", closed)
				}
			} else {
				raw, errExecute := execute(mustJSONValue(t, req))
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				var env envelope
				if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
					t.Fatal(errUnmarshal)
				}
				if env.Error == nil || strings.Contains(env.Error.Code, "bravo_adaptive_") {
					t.Fatalf("nonstream terminal failure=%#v", env.Error)
				}
			}
			if len(calls) != 2 || calls[0].AuthID == "claude-last-chance" || calls[1].AuthID == "claude-last-chance" {
				t.Fatalf("provider calls=%#v, retained blocked attempt was re-dispatched", calls)
			}
		})
	}
}

func TestAdaptiveBreakerRecoveryProbeIsGlobalSingleFlightAcrossNonstreamCoordinators(t *testing.T) {
	const workers = 100
	req, auths := installAdaptiveBreakerCoordinatorTest(t, false, 2)
	var neighborArrivals atomic.Int32
	var protectedCalls atomic.Int32
	releaseNeighbors := make(chan struct{})
	var releaseOnce sync.Once
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			if call.ForcedProvider == "codex" {
				if neighborArrivals.Add(1) == workers {
					releaseOnce.Do(func() { close(releaseNeighbors) })
				}
				<-releaseNeighbors
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			protectedCalls.Add(1)
			return nil, adaptiveRecoveryQuotaHostError()
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})

	start := make(chan struct{})
	results := make(chan envelope, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer group.Done()
			<-start
			raw, errExecute := execute(mustJSONValue(t, req))
			if errExecute != nil {
				results <- envelope{Error: &envelopeError{Code: errExecute.Error()}}
				return
			}
			var env envelope
			if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
				results <- envelope{Error: &envelopeError{Code: errUnmarshal.Error()}}
				return
			}
			results <- env
		}()
	}
	started := time.Now()
	close(start)
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		releaseOnce.Do(func() { close(releaseNeighbors) })
		<-done
		t.Fatal("nonstream recovery burst waited or queued")
	}
	if elapsed := time.Since(started); elapsed >= 30*time.Second {
		t.Fatalf("nonstream recovery burst elapsed=%s", elapsed)
	}
	if got := protectedCalls.Load(); got > 1 {
		t.Fatalf("protected provider calls=%d, want <=1", got)
	}
	for index := 0; index < workers; index++ {
		env := <-results
		if env.Error == nil || strings.Contains(env.Error.Code, "bravo_adaptive_") ||
			strings.Contains(env.Error.Message, "bravo_adaptive_") {
			t.Fatalf("result %d leaked adaptive failure: %#v", index, env)
		}
	}
}

func TestAdaptiveBreakerRecoveryProbeIsGlobalSingleFlightAcrossStreamCoordinators(t *testing.T) {
	const workers = 20
	req, auths := installAdaptiveBreakerCoordinatorTest(t, true, 2)
	var neighborArrivals atomic.Int32
	var protectedCalls atomic.Int32
	releaseNeighbors := make(chan struct{})
	var releaseOnce sync.Once
	closes := make([]rpcStreamCloseRequest, 0, workers)
	var closesMu sync.Mutex
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			if call.ForcedProvider == "codex" {
				if neighborArrivals.Add(1) == workers {
					releaseOnce.Do(func() { close(releaseNeighbors) })
				}
				<-releaseNeighbors
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			protectedCalls.Add(1)
			return nil, adaptiveRecoveryQuotaHostError()
		case pluginabi.MethodHostStreamClose:
			var closed rpcStreamCloseRequest
			decodeBravoPayload(t, payload, &closed)
			closesMu.Lock()
			closes = append(closes, closed)
			closesMu.Unlock()
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})

	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		index := index
		go func() {
			defer group.Done()
			<-start
			runBravoStream(req, fmt.Sprintf("breaker-recovery-stream-%d", index))
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		releaseOnce.Do(func() { close(releaseNeighbors) })
		<-done
		t.Fatal("stream recovery burst waited or queued")
	}
	if got := protectedCalls.Load(); got > 1 {
		t.Fatalf("protected stream provider calls=%d, want <=1", got)
	}
	closesMu.Lock()
	defer closesMu.Unlock()
	if len(closes) != workers {
		t.Fatalf("stream closes=%d, want %d", len(closes), workers)
	}
	for _, closed := range closes {
		if closed.ErrorCode == "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_") ||
			strings.Contains(closed.Error, "bravo_adaptive_") {
			t.Fatalf("stream leaked adaptive failure: %#v", closed)
		}
	}
}

func adaptiveRecoveryQuotaHostError() error {
	return &hostCallError{
		Code: "rate_limit_error", Message: "trusted quota exhaustion", HTTPStatus: http.StatusTooManyRequests,
		ProviderError: &providererror.Detail{
			TaxonomyVersion: providererror.FailureTaxonomyV1,
			Class:           providererror.ClassQuota, Scope: providererror.ScopeAccount,
		},
	}
}

func installAdaptiveBreakerCoordinatorTest(t *testing.T, stream bool, maxAttempts int) (rpcExecutorRequest, []pluginapi.HostAuthFileEntry) {
	t.Helper()
	isolateBravoFallbackTestState(t)
	restoreUsage := isolateBravoUsageState(t)
	t.Cleanup(restoreUsage)
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)

	const plaintext = "brv_breaker_coordinator"
	sum := sha256.Sum256([]byte(plaintext))
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-last-chance", AuthIndex: "claude-last-chance", Provider: "claude"},
		{ID: "codex-neighbor", AuthIndex: "codex-neighbor", Provider: "codex"},
	}
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "off"
	cfg.AdaptiveAllocatorMode = "breaker"
	cfg.MaxAttempts = maxAttempts
	cfg.FallbackHedgeDelaySeconds = 0
	capabilities := []string{capabilityText}
	if stream {
		capabilities = append(capabilities, capabilityStream)
	}
	cfg.Models = map[string]logicalModel{"breaker-coordinator": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: capabilities},
		{Provider: "codex", Model: "gpt-5.6-luna", Priority: 90, Capabilities: capabilities},
	}}}
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "breaker-project", Name: "Breaker", SHA256: hex.EncodeToString(sum[:]), Models: []string{"*"},
		AllowedAuthIDs: []string{"claude-last-chance", "codex-neighbor"}, PrimaryAuthIDs: []string{"claude-last-chance"},
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previous := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 80)
	storeQuotaSnapshot("claude-last-chance", quota)
	seed := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth:      pluginapi.HostAuthFileEntry{AuthIndex: "claude-last-chance", Provider: "claude"},
		Primary:   true, AdaptiveShadow: true,
	}
	seed.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, seed, quota, tariffByID(cfg, "x1"), now)
	beginAdaptiveEdgeGateShadow(seed, now)
	observeAdaptiveEdgeGateOutcome(seed, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: 429, RetryAfter: "60", Retryable: true,
		AccountWide: true, Provider: &providererror.Detail{Scope: providererror.ScopeAccount},
	}, now)
	body := []byte(`{"model":"bravo/breaker-coordinator","messages":[{"role":"user","content":"ok"}]}`)
	if stream {
		body = []byte(`{"model":"bravo/breaker-coordinator","messages":[{"role":"user","content":"ok"}],"stream":true}`)
	}
	return rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/breaker-coordinator", Format: protocolOpenAI, SourceFormat: protocolOpenAI,
		Headers: http.Header{"Authorization": []string{"Bearer " + plaintext}}, OriginalRequest: body,
	}, HostCallbackID: "breaker-coordinator-callback"}, auths
}
