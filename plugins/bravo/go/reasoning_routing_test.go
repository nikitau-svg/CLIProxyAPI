package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNativeClaudeReasoningReplayContractSkipsIncompatibleCandidates(t *testing.T) {
	body := reasoningReplayRequestBody(false)
	contract, errDetect := detectRequestContract(protocolClaude, body, false)
	if errDetect != nil {
		t.Fatal(errDetect)
	}
	assertCapabilities(t, contract, capabilityText, capabilityReasoning)
	assertRequestEffort(t, contract, "xhigh", true)

	compatible := candidate{
		Provider:     "claude",
		Model:        "claude-opus-4-8",
		Capabilities: []string{capabilityText, capabilityReasoning},
	}
	if _, errResolve := resolveCandidateContract(compatible, contract); errResolve != nil {
		t.Fatalf("native Claude candidate rejected signed thinking replay: %v", errResolve)
	}

	incompatible := candidate{
		Provider:     "codex",
		Model:        "gpt-5.5",
		Capabilities: []string{capabilityText},
	}
	_, errResolve := resolveCandidateContract(incompatible, contract)
	assertContractError(t, errResolve, "bravo_capability_undeclared", capabilityReasoning)

	noThinkingModel := candidate{
		Provider:     "claude",
		Model:        "claude-3-5-haiku-20241022",
		Capabilities: []string{capabilityText, capabilityReasoning},
	}
	_, errResolve = resolveCandidateContract(noThinkingModel, contract)
	assertContractError(t, errResolve, "bravo_contract_unverified", capabilityReasoning)
}

// Both providers declare reasoning in the defaults. Claude replays the signed
// block verbatim; Codex carries the reasoning text across as prior context.
// Codex used to be excluded here, which made a replayed-thinking request
// unroutable as soon as every Claude credential was out of weekly quota — a 503
// with a fully healthy Codex pool sitting idle. See the liveCapabilityMatrix
// comment in contract.go for what the degraded Codex path does and does not keep.
func TestDefaultTextCandidatesDeclareReasoning(t *testing.T) {
	cfg := defaultPluginConfig()
	for _, name := range []string{"opus", "sonnet", "frontier", "claude-opus-5", "gpt-5.6-sol"} {
		model := cfg.Models[name]
		if len(model.Candidates) == 0 {
			t.Fatalf("default %s policy has no candidates", name)
		}
		for _, item := range model.Candidates {
			capabilities := newCapabilitySet(item.Capabilities...)
			if _, hasReasoning := capabilities[capabilityReasoning]; !hasReasoning {
				t.Fatalf("default %s candidate %#v does not declare reasoning, so a replayed-thinking request cannot use it", name, item)
			}
		}
	}
	// Image candidates must stay out of it: they cannot carry reasoning at all.
	for _, name := range []string{"image", "gpt-image-2"} {
		for _, item := range cfg.Models[name].Candidates {
			capabilities := newCapabilitySet(item.Capabilities...)
			if _, hasReasoning := capabilities[capabilityReasoning]; hasReasoning {
				t.Fatalf("image candidate %#v unexpectedly declares reasoning", item)
			}
		}
	}
}

func TestBravoManualClaudeThinkingFailsBeforeNativeCandidateRouting(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installReasoningRoutingConfig(t, true)
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		t.Fatalf("manual-thinking preflight unexpectedly called host method %q", method)
		return nil, nil
	})

	req := reasoningReplayExecutorRequest(false)
	req.OriginalRequest = []byte(`{
		"model":"bravo/fallback-probe",
		"messages":[{"role":"user","content":"do not execute"}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"max_tokens":2048
	}`)
	raw, errExecute := execute(mustJSONValue(t, req))
	if errExecute != nil {
		t.Fatal(errExecute)
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("response = %s, want fail-closed manual-thinking error", raw)
	}
	if env.Error.HTTPStatus != http.StatusUnprocessableEntity ||
		env.Error.Code != "bravo_contract_unverified" {
		t.Fatalf("error = %#v, want manual-thinking contract 422", env.Error)
	}
}

func TestBravoExecuteSkipsReasoningIncompatibleCandidate(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installReasoningRoutingConfig(t, true)

	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return reasoningRoutingAuths(t), nil
		case pluginabi.MethodHostModelExecute:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			assertNativeClaudeReasoningAttempt(t, req.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"ok"}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, reasoningReplayExecutorRequest(false)))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	assertSuccessfulBravoEnvelope(t, raw)
	if len(calls) != 1 || calls[0].ForcedProvider != "claude" {
		t.Fatalf("host model calls = %#v, want exactly one compatible Claude call", calls)
	}
}

func TestBravoCountTokensSkipsReasoningIncompatibleCandidate(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installReasoningRoutingConfig(t, true)

	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return reasoningRoutingAuths(t), nil
		case pluginabi.MethodHostModelCountTokens:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			assertNativeClaudeReasoningAttempt(t, req.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"input_tokens":42}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errCount := countTokens(mustJSONValue(t, reasoningReplayExecutorRequest(false)))
	if errCount != nil {
		t.Fatal(errCount)
	}
	assertSuccessfulBravoEnvelope(t, raw)
	if len(calls) != 1 || calls[0].ForcedProvider != "claude" {
		t.Fatalf("host count calls = %#v, want exactly one compatible Claude call", calls)
	}
}

func TestBravoStreamSkipsReasoningIncompatibleCandidate(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installReasoningRoutingConfig(t, true)

	var calls []pluginapi.HostModelExecutionRequest
	var pluginClose rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return reasoningRoutingAuths(t), nil
		case pluginabi.MethodHostModelExecuteStream:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			assertNativeClaudeReasoningAttempt(t, req.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "native-claude-reasoning-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	req := reasoningReplayExecutorRequest(true)
	runBravoStream(req, "reasoning-client-stream")
	if len(calls) != 1 || calls[0].ForcedProvider != "claude" {
		t.Fatalf("host stream calls = %#v, want exactly one compatible Claude call", calls)
	}
	if pluginClose.StreamID != "reasoning-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin stream close = %#v, want successful close", pluginClose)
	}
}

func TestBravoReasoningReplayFailsClosedWithoutCompatibleCandidate(t *testing.T) {
	tests := []struct {
		name string
		call func(t *testing.T) []byte
	}{
		{
			name: "execute",
			call: func(t *testing.T) []byte {
				raw, errExecute := execute(mustJSONValue(t, reasoningReplayExecutorRequest(false)))
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				return raw
			},
		},
		{
			name: "count",
			call: func(t *testing.T) []byte {
				raw, errCount := countTokens(mustJSONValue(t, reasoningReplayExecutorRequest(false)))
				if errCount != nil {
					t.Fatal(errCount)
				}
				return raw
			},
		},
		{
			name: "stream",
			call: func(t *testing.T) []byte {
				req := reasoningReplayExecutorRequest(true)
				req.StreamID = "fail-closed-stream"
				raw, errStream := executeStream(mustJSONValue(t, req))
				if errStream != nil {
					t.Fatal(errStream)
				}
				return raw
			},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			installReasoningRoutingConfig(t, false)
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				t.Fatalf("fail-closed preflight unexpectedly called host method %q", method)
				return nil, nil
			})

			raw := testCase.call(t)
			var env envelope
			if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if env.OK || env.Error == nil {
				t.Fatalf("response = %s, want fail-closed error", raw)
			}
			if env.Error.HTTPStatus != http.StatusUnprocessableEntity ||
				env.Error.Code != "bravo_capability_undeclared" {
				t.Fatalf("error = %#v, want reasoning capability 422", env.Error)
			}
		})
	}
}

func installReasoningRoutingConfig(t *testing.T, includeCompatible bool) {
	t.Helper()
	candidates := []candidate{
		{
			Provider:     "codex",
			Model:        "gpt-5.5",
			Effort:       "high",
			Priority:     100,
			Capabilities: []string{capabilityText, capabilityStream},
		},
	}
	if includeCompatible {
		candidates = append(candidates, candidate{
			Provider:     "claude",
			Model:        "claude-opus-4-8",
			Effort:       "high",
			Priority:     90,
			Capabilities: []string{capabilityText, capabilityReasoning, capabilityStream},
		})
	}
	installBravoTestConfig(t, logicalModel{Candidates: candidates})
}

func reasoningRoutingAuths(t *testing.T) json.RawMessage {
	t.Helper()
	return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
		{ID: "codex-incompatible", Provider: "codex"},
		{ID: "claude-compatible", Provider: "claude"},
	}})
}

func reasoningReplayExecutorRequest(stream bool) rpcExecutorRequest {
	return rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: reasoningReplayRequestBody(stream),
		},
		HostCallbackID: "reasoning-routing-callback",
	}
}

func reasoningReplayRequestBody(stream bool) []byte {
	streamValue := ""
	if stream {
		streamValue = `,"stream":true`
	}
	return []byte(`{
		"model":"bravo/fallback-probe",
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"prior trace","signature":"signed"}]},
			{"role":"user","content":"continue"}
		],
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"}` + streamValue + `
	}`)
}

func assertNativeClaudeReasoningAttempt(t *testing.T, req pluginapi.HostModelExecutionRequest) {
	t.Helper()
	if req.ForcedProvider != "claude" || req.AuthID != "claude-compatible" {
		t.Fatalf("nested request = %#v, want pinned compatible Claude candidate", req)
	}
	if !strings.HasPrefix(req.Model, "claude-opus-4-8(") {
		t.Fatalf("nested model = %q, want effort-resolved Claude Opus", req.Model)
	}
	body := string(req.Body)
	if !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, `"signature":"signed"`) {
		t.Fatalf("nested body lost signed reasoning replay: %s", body)
	}
	if strings.Contains(body, `"output_config"`) {
		t.Fatalf("nested body retained duplicate named effort authority: %s", body)
	}
}

func assertSuccessfulBravoEnvelope(t *testing.T, raw []byte) {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo execution failed: %#v", env.Error)
	}
}
