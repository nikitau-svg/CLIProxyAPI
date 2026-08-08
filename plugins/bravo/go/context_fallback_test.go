package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestContextOverflowDoesNotCreateAdaptivePendingDebt(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		stream             bool
		hostResponse       bool
		streamReadResponse bool
	}{
		{name: "nonstream host error"},
		{name: "nonstream HTTP response", hostResponse: true},
		{name: "stream host error", stream: true},
		{name: "stream read HTTP response", stream: true, streamReadResponse: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restoreUsage := isolateBravoUsageState(t)
			defer restoreUsage()
			isolateBravoFallbackTestState(t)
			resetAdaptiveReserveForTest()
			defer resetAdaptiveReserveForTest()
			statePath := filepath.Join(t.TempDir(), "context-pending-state.json")
			if errConfigure := configureUsageState(statePath); errConfigure != nil {
				t.Fatal(errConfigure)
			}

			const authIndex = "context-pending-auth"
			cfg := defaultPluginConfig()
			cfg.Enabled = true
			cfg.AllocatorMode = "enforce"
			cfg.FallbackHedgeDelaySeconds = 0
			cfg.Tariffs = []tariffConfig{{
				ID: "x1", SessionFloorPercent: 0, WeeklyFloorPercent: 0,
				Multiplier: 1, ReservationPercent: 1,
			}}
			cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
			cfg.SmartKeys = []smartKeyConfig{{
				ID: "context-pending-project", Name: "Context pending project", SHA256: strings.Repeat("c", 64),
				Enabled: boolPointer(true), Status: projectStatusActive, Models: []string{"*"},
			}}
			cfg.Models = map[string]logicalModel{"context-pending": {Candidates: []candidate{{
				Provider: "codex", Model: "gpt-5.6-sol", Priority: 100,
				Capabilities: []string{capabilityText, capabilityStream},
			}}}}
			if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
				t.Fatal(errNormalize)
			}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			defer currentConfig.Store(previousConfig)
			setAdaptivePersistenceQuota(t, authIndex, 90)

			providerCalls := 0
			var streamClose rpcStreamCloseRequest
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
						ID: "context-pending-id", AuthIndex: authIndex, Provider: "codex",
					}}}), nil
				case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
					providerCalls++
					if testCase.streamReadResponse {
						return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
							StatusCode: http.StatusOK,
							StreamID:   "context-pending-provider-stream",
						}), nil
					}
					if testCase.hostResponse {
						return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
							StatusCode: http.StatusBadRequest,
							Body:       []byte("Your input exceeds the context window of this model. Please adjust your input and try again."),
						}), nil
					}
					return nil, &hostCallError{
						Code:       "model_execution_failed",
						Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
						HTTPStatus: http.StatusBadRequest,
					}
				case pluginabi.MethodHostModelStreamRead:
					return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "model_execution_failed",
							Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
							HTTPStatus: http.StatusBadRequest,
						},
					}), nil
				case pluginabi.MethodHostStreamClose:
					decodeBravoPayload(t, payload, &streamClose)
					return json.RawMessage(`{}`), nil
				case pluginabi.MethodHostLog:
					return json.RawMessage(`{}`), nil
				default:
					return json.RawMessage(`{}`), nil
				}
			})

			body := []byte(`{"model":"bravo/context-pending","messages":[{"role":"user","content":"large history"}]}`)
			request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "bravo/context-pending", Format: protocolClaude, SourceFormat: protocolClaude,
				OriginalRequest: body, Metadata: compactProjectMetadata("context-pending-project"),
			}, HostCallbackID: "context-pending-callback"}
			if testCase.stream {
				request.OriginalRequest = []byte(`{"model":"bravo/context-pending","messages":[{"role":"user","content":"large history"}],"stream":true}`)
				runBravoStream(request, "context-pending-stream")
				if streamClose.ErrorCode != "bravo_context_window_exceeded" {
					t.Fatalf("stream close = %#v", streamClose)
				}
			} else {
				raw, errExecute := execute(mustJSONValue(t, request))
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				var env envelope
				if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.Error == nil || env.Error.Code != "bravo_context_window_exceeded" {
					t.Fatalf("context response = %s error=%v", raw, errUnmarshal)
				}
			}
			if providerCalls != 1 {
				t.Fatalf("provider calls = %d, want 1", providerCalls)
			}
			if got := pendingReservationPercent(authIndex); got != 0 {
				t.Fatalf("context rejection created pending debt %.3f, want 0", got)
			}
			traces, _, errList := listCurrentRouteTraces(routeTraceQuery{ProjectID: "context-pending-project", ErrorsOnly: true, Limit: 2}, time.Now().UTC())
			if errList != nil || len(traces) != 1 || len(traces[0].Attempts) != 1 {
				t.Fatalf("context traces = %#v error=%v", traces, errList)
			}
			if traces[0].Attempts[0].Committed {
				t.Fatalf("context trace marked rejected inference committed: %#v", traces[0].Attempts[0])
			}

			resetAdaptiveReserveForTest()
			simulateFreshBravoProcess(t, statePath)
			if got := pendingReservationPercent(authIndex); got != 0 {
				t.Fatalf("context debt resurrected after restart: %.3f", got)
			}
		})
	}
}

func TestBravoFinalErrorPreservesCreditsAndContextFailures(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     100,
				Capabilities: []string{capabilityText},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-sol",
				Priority:     90,
				Capabilities: []string{capabilityText},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
		{ID: "codex-x20", Name: "codex-x20.json", Provider: "codex", Note: "Codex x20"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.ForcedProvider == "claude" {
				return nil, &hostCallError{
					Code:       "model_execution_failed",
					Message:    anthropicCreditsRequiredPayload,
					HTTPStatus: http.StatusTooManyRequests,
				}
			}
			return nil, &hostCallError{
				Code:       "model_execution_failed",
				Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
				HTTPStatus: http.StatusBadRequest,
			}
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"large history"}]}`),
		},
		HostCallbackID: "credits-then-context",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("response = %s, want a composite terminal failure", raw)
	}
	for _, want := range []string{"Fable 5", "лимит расходов", "gpt-5.6-sol", "контекст переписки"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("final message = %q, missing %q", env.Error.Message, want)
		}
	}
	if strings.Contains(env.Error.Message, "req_bravo_credits_private") ||
		strings.Contains(env.Error.Message, `{"type":"error"`) {
		t.Fatalf("final message leaked raw provider data: %q", env.Error.Message)
	}
	if len(calls) != 2 {
		t.Fatalf("host calls = %#v, want Claude then Codex", calls)
	}
	if !cooldownActive("claude", "palantir", "claude-fable-5", time.Now()) {
		t.Fatal("Fable credits failure did not receive its model-scoped cooldown")
	}
	if cooldownActive("codex", "codex-x20", "gpt-5.6-sol", time.Now()) {
		t.Fatal("request-scoped context overflow cooled the Codex model")
	}
}

func TestBravoContextOverflowFailsClosedWithoutTryingAnotherModel(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "codex",
				Model:        "gpt-5.6-terra",
				Priority:     100,
				Capabilities: []string{capabilityText},
			},
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     90,
				Capabilities: []string{capabilityText},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "codex-a", Name: "codex-a.json", Provider: "codex"},
		{ID: "codex-b", Name: "codex-b.json", Provider: "codex"},
		{ID: "claude-a", Name: "claude-a.json", Provider: "claude"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.ForcedProvider == "codex" {
				return nil, &hostCallError{
					Code:       "model_execution_failed",
					Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
					HTTPStatus: http.StatusBadRequest,
				}
			}
			t.Fatal("context overflow blindly advanced to an unverified context window")
			return nil, nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"large history"}]}`),
		},
		HostCallbackID: "context-next-route",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("response = %s, want a terminal context-window failure", raw)
	}
	if env.Error.Code != "bravo_context_window_exceeded" || env.Error.Retryable {
		t.Fatalf("error = %#v, want non-retryable context-window failure", env.Error)
	}
	if !strings.Contains(env.Error.Message, "gpt-5.6-terra") {
		t.Fatalf("terminal context error = %q, want the physical model name", env.Error.Message)
	}
	if len(calls) != 1 {
		t.Fatalf("host calls = %#v, want exactly one physical-model attempt", calls)
	}
	for _, authID := range []string{"codex-a", "codex-b"} {
		if cooldownActive("codex", authID, "gpt-5.6-terra", time.Now()) {
			t.Fatalf("request-scoped context overflow cooled %s", authID)
		}
	}
}
