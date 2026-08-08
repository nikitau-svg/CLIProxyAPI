package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecuteAmbiguousHostErrorCommitsFirstLeaseBeforeCredentialFallback(t *testing.T) {
	isolateBravoFallbackTestState(t)
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	if err := configureUsageState(filepath.Join(t.TempDir(), "ambiguous.json")); err != nil {
		t.Fatal(err)
	}
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText},
	}}})
	cfg := loadedConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Subscriptions = []subscriptionConfig{
		{AuthIndex: "1111111111111111", Tariff: "x1"},
		{AuthIndex: "2222222222222222", Tariff: "x1"},
	}
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "ambiguous-project", Name: "Ambiguous fallback", SHA256: strings.Repeat("a", 64), Enabled: boolPointer(true), Status: projectStatusActive, Models: []string{"*"},
	}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	currentConfig.Store(cfg)
	setAdaptivePersistenceQuota(t, "1111111111111111", 90)
	setAdaptivePersistenceQuota(t, "2222222222222222", 90)
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "auth-a", AuthIndex: "1111111111111111", Provider: "claude"},
		{ID: "auth-b", AuthIndex: "2222222222222222", Provider: "claude"},
	}
	calledAuth := make([]string, 0, 2)
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calledAuth = append(calledAuth, request.AuthID)
			if len(calledAuth) == 1 {
				return nil, &hostCallError{
					Code: "model_execution_failed", Message: "upstream connection closed after dispatch", HTTPStatus: http.StatusBadGateway, Retryable: true,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"claude-fable-5","content":[{"type":"text","text":"ok"}]}`),
			}), nil
		default:
			return json.RawMessage(`{}`), nil
		}
	})

	executorRequest := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"continue"}]}`),
		},
		HostCallbackID: "ambiguous-fallback",
	}
	executorRequest.Metadata = compactProjectMetadata("ambiguous-project")
	contract, errContract := detectRequestContract(protocolClaude, executionBody(executorRequest), false)
	if errContract != nil {
		t.Fatal(errContract)
	}
	direct := allocateCandidateAuthsForShape(cfg, cfg.SmartKeys[0], cfg.Models["fallback-probe"].Candidates[0], auths, "", adaptiveRequestShape{})
	if len(direct) != 2 {
		t.Fatalf("direct allocator attempts = %#v quotas=%#v/%#v cfg=%#v", direct, quotaSnapshot("1111111111111111"), quotaSnapshot("2222222222222222"), cfg.AllocatorMode)
	}
	plan, errPlan := buildExecutionPlan(executorRequest, "fallback-probe", cfg.Models["fallback-probe"], contract)
	if errPlan != nil || len(plan) != 2 || !plan[0].AllocatorManaged || !plan[1].AllocatorManaged {
		t.Fatalf("allocator plan = %#v error=%v", plan, errPlan)
	}
	raw, errExecute := execute(mustJSONValue(t, executorRequest))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("execution response = %s error=%v", raw, err)
	}
	if len(calledAuth) != 2 || calledAuth[0] == calledAuth[1] {
		t.Fatalf("credential fallback calls = %v, want two independent credentials", calledAuth)
	}
	indexByID := map[string]string{"auth-a": "1111111111111111", "auth-b": "2222222222222222"}
	for _, authID := range calledAuth {
		authIndex := indexByID[authID]
		if got := pendingReservationPercent(authIndex); got <= 0 {
			allocatorRuntime.Lock()
			allPending := make(map[string]float64, len(allocatorRuntime.PendingPercent))
			for key, value := range allocatorRuntime.PendingPercent {
				allPending[key] = value
			}
			allocatorRuntime.Unlock()
			t.Fatalf("credential %s/%s pending = %.3f all=%v, ambiguous/successful work was not committed", authID, authIndex, got, allPending)
		}
	}
}

func TestExecuteInvalidPreflightCreatesNoLeaseAndNoProviderCall(t *testing.T) {
	isolateBravoFallbackTestState(t)
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	if err := configureUsageState(filepath.Join(t.TempDir(), "invalid-preflight.json")); err != nil {
		t.Fatal(err)
	}
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText},
	}}})
	providerCalls := 0
	installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelExecute || method == pluginabi.MethodHostModelExecuteStream {
			providerCalls++
		}
		return json.RawMessage(`{}`), nil
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/fallback-probe", Format: protocolClaude, SourceFormat: protocolClaude,
		OriginalRequest: []byte(`{`),
	}}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil || env.Error.Code != "bravo_request_invalid" {
		t.Fatalf("invalid preflight response = %s error=%v", raw, err)
	}
	if providerCalls != 0 {
		t.Fatalf("invalid preflight made %d provider calls", providerCalls)
	}
	allocatorRuntime.Lock()
	pendingCount, inFlightCount := len(allocatorRuntime.PendingPercent), len(allocatorRuntime.InFlightPercent)
	allocatorRuntime.Unlock()
	if pendingCount != 0 || inFlightCount != 0 {
		t.Fatalf("invalid preflight created pending=%d in-flight=%d ledger entries", pendingCount, inFlightCount)
	}
	bravoUsageState.mu.RLock()
	durablePending, durablePrepared := len(bravoUsageState.state.AdaptiveQuota.Pending), len(bravoUsageState.state.AdaptiveQuota.Prepared)
	bravoUsageState.mu.RUnlock()
	if durablePending != 0 || durablePrepared != 0 {
		t.Fatalf("invalid preflight created durable pending=%d prepared=%d entries", durablePending, durablePrepared)
	}
}
