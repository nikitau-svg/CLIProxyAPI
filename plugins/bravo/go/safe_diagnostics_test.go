package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSafeDiagnosticsPersistEarlyContractFailures(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(t *testing.T, req rpcExecutorRequest) []byte
	}{
		{
			name: "execute",
			invoke: func(t *testing.T, req rpcExecutorRequest) []byte {
				raw, err := execute(mustJSONValue(t, req))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
		},
		{
			name: "stream",
			invoke: func(t *testing.T, req rpcExecutorRequest) []byte {
				req.StreamID = "safe-diagnostic-stream"
				raw, err := executeStream(mustJSONValue(t, req))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
		},
		{
			name: "count_tokens",
			invoke: func(t *testing.T, req rpcExecutorRequest) []byte {
				raw, err := countTokens(mustJSONValue(t, req))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			installReasoningRoutingConfig(t, true)
			store := installSafeDiagnosticsTraceStore(t)
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				t.Fatalf("early contract failure unexpectedly called host method %q", method)
				return nil, nil
			})

			req := reasoningReplayExecutorRequest(testCase.name == "stream")
			req.OriginalRequest = []byte(`{
				"model":"bravo/fallback-probe",
				"messages":[{"role":"user","content":"do not execute"}],
				"thinking":{"type":"enabled","budget_tokens":1024},
				"max_tokens":2048
			}`)
			raw := testCase.invoke(t, req)
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusUnprocessableEntity {
				t.Fatalf("response = %s", raw)
			}
			traceID := env.Error.Headers.Get(bravoTraceIDHeader)
			if traceID == "" {
				t.Fatalf("422 response has no %s: %s", bravoTraceIDHeader, raw)
			}
			traces, errList := store.list(routeTraceQuery{TraceID: traceID}, time.Now().UTC())
			if errList != nil {
				t.Fatal(errList)
			}
			if len(traces) != 1 || len(traces[0].Attempts) != 1 {
				t.Fatalf("traces = %#v", traces)
			}
			attempt := traces[0].Attempts[0]
			if attempt.DiagnosticStage != "contract_detection" ||
				attempt.RequiredCapability != capabilityReasoning ||
				attempt.ParameterPath != "$.thinking" ||
				attempt.Decision != "stop" || attempt.Committed {
				t.Fatalf("preflight attempt = %#v", attempt)
			}
			if traces[0].ClientAction != "fix_request" || !strings.Contains(traces[0].FinalMessage, "до обращения к провайдеру") {
				t.Fatalf("trace action/message = %q/%q", traces[0].ClientAction, traces[0].FinalMessage)
			}
		})
	}
}

func TestSafeDiagnosticsPersistCodexInvalidToolParameterWithoutRawPayload(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider:     "codex",
		Model:        "gpt-5.6-sol",
		Priority:     100,
		Capabilities: []string{capabilityText, capabilityTools},
	}}})
	store := installSafeDiagnosticsTraceStore(t)
	classification, ok := providererror.ParseOpenAIStandard(http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","code":"invalid_tool_parameters","param":"tools[3].function.parameters","message":"echoed schema contains context window and sk-private"},"request_id":"req_private"}`)
	if !ok {
		t.Fatal("fixture did not parse")
	}
	providerCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID: "codex-safe-diagnostic", AuthIndex: "codex-safe-diagnostic", Provider: "codex",
			}}}), nil
		case pluginabi.MethodHostModelExecute:
			providerCalls++
			detail := classification.Detail
			return nil, &hostCallError{
				Code:          detail.Code,
				Message:       detail.Message,
				HTTPStatus:    http.StatusBadRequest,
				ProviderError: &detail,
			}
		default:
			t.Fatalf("unexpected host method %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:        "bravo/fallback-probe",
			Format:       protocolClaude,
			SourceFormat: protocolClaude,
			OriginalRequest: []byte(`{
				"model":"bravo/fallback-probe",
				"max_tokens":64,
				"tools":[{"name":"private_tool","description":"sk-request-secret","input_schema":{"type":"object","properties":{}}}],
				"messages":[{"role":"user","content":"private prompt"}]
			}`),
		},
		HostCallbackID: "safe-diagnostic-callback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "invalid_tool_parameters" {
		t.Fatalf("response = %s", raw)
	}
	traceID := env.Error.Headers.Get(bravoTraceIDHeader)
	traces, errList := store.list(routeTraceQuery{TraceID: traceID}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 1 || len(traces[0].Attempts) != 1 {
		t.Fatalf("traces = %#v", traces)
	}
	attempt := traces[0].Attempts[0]
	if attempt.ErrorCode == "bravo_context_window_exceeded" ||
		attempt.ProviderErrorCode != "invalid_tool_parameters" ||
		attempt.ProviderErrorParam != "tools[3].function.parameters" ||
		attempt.FailureClass != providererror.ClassInvalidRequest ||
		traces[0].ClientAction != "fix_request" {
		t.Fatalf("invalid-tool trace = %#v / %#v", attempt, traces[0])
	}
	encoded, errMarshal := json.Marshal(traces)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{"private prompt", "private_tool", "sk-request-secret", "req_private", "echoed schema"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("trace leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSafeRouteTraceParameterRejectsSchemaOrSecrets(t *testing.T) {
	for _, value := range []string{
		`tools[0].parameters;drop`,
		`tools[0].parameters bearer_secret`,
		`{"type":"object"}`,
		strings.Repeat("a", 257),
	} {
		if got := safeRouteTraceParameter(value); got != "" {
			t.Fatalf("safeRouteTraceParameter(%q) = %q", value, got)
		}
	}
	if got := safeRouteTraceParameter("tools[12].function.parameters"); got != "tools[12].function.parameters" {
		t.Fatalf("safe parameter = %q", got)
	}
}

func installSafeDiagnosticsTraceStore(t *testing.T) *routeTraceStore {
	t.Helper()
	previous := bravoRouteTraces
	store := newRouteTraceStore(filepath.Join(t.TempDir(), "bravo-state.json"))
	bravoRouteTraces = store
	t.Cleanup(func() {
		_ = store.flush()
		bravoRouteTraces = previous
	})
	return store
}
