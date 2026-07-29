package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBravoStreamPreservesCreditsThenContextFailureChain(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-sol",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
		{ID: "codex-one", Name: "codex-one.json", Provider: "codex"},
		{ID: "codex-two", Name: "codex-two.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	var emitted [][]byte
	var pluginClose rpcStreamCloseRequest
	streamReads := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.ForcedProvider == "claude" {
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
					StatusCode: http.StatusOK,
					StreamID:   "fable-credits-stream",
				}), nil
			}
			return nil, &hostCallError{
				Code:       "model_execution_failed",
				Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
				HTTPStatus: http.StatusBadRequest,
			}
		case pluginabi.MethodHostModelStreamRead:
			streamReads++
			if streamReads == 1 {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-fable-5\"}}\n\n"),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
				Payload: []byte("event: error\ndata: " + anthropicCreditsRequiredPayload + "\n\n"),
				Done:    true,
			}), nil
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &request)
			emitted = append(emitted, append([]byte(nil), request.Payload...))
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	runBravoStream(bravoContextStreamRequest("credits-context-chain"), "credits-context-client-stream")

	if len(calls) != 2 {
		t.Fatalf("stream provider calls = %#v, want Fable once then one Sol attempt", calls)
	}
	if calls[0].ForcedProvider != "claude" || calls[0].Model != "claude-fable-5" {
		t.Fatalf("first stream call = %#v, want Claude Fable", calls[0])
	}
	if calls[1].ForcedProvider != "codex" || calls[1].Model != "gpt-5.6-sol" {
		t.Fatalf("second stream call = %#v, want Codex Sol", calls[1])
	}
	if len(emitted) != 0 {
		t.Fatalf("provider error SSE was emitted to the client: %q", emitted)
	}
	for _, want := range []string{"Fable 5", "monthly spend", "gpt-5.6-sol", "context window"} {
		if !strings.Contains(pluginClose.Error, want) {
			t.Errorf("stream terminal error = %q, missing %q", pluginClose.Error, want)
		}
	}
	if strings.Contains(pluginClose.Error, "req_bravo_credits_private") ||
		strings.Contains(pluginClose.Error, `{"type":"error"`) {
		t.Fatalf("stream terminal error leaked raw provider data: %q", pluginClose.Error)
	}
	if !cooldownActive("claude", "palantir", "claude-fable-5", time.Now()) {
		t.Fatal("Fable credits failure did not receive its model-scoped cooldown")
	}
	for _, authID := range []string{"codex-one", "codex-two"} {
		if cooldownActive("codex", authID, "gpt-5.6-sol", time.Now()) {
			t.Fatalf("request-scoped context overflow cooled Codex auth %q", authID)
		}
	}
}

func TestBravoStreamUnknownStructuredErrorAfterPreludeFailsSafely(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	const unknownError = `{"type":"error","error":{"type":"billing_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_private"}`
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
	}
	var emitted [][]byte
	var pluginClose rpcStreamCloseRequest
	streamReads := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "unknown-error-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			streamReads++
			if streamReads == 1 {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-fable-5\"}}\n\n"),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
				Payload: []byte("event: error\ndata: " + unknownError + "\n\n"),
				Done:    true,
			}), nil
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &request)
			emitted = append(emitted, append([]byte(nil), request.Payload...))
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	runBravoStream(bravoContextStreamRequest("unknown-after-prelude"), "unknown-after-prelude-client-stream")

	if len(emitted) != 0 {
		t.Fatalf("structured provider error or prelude was emitted: %q", emitted)
	}
	if pluginClose.StreamID != "unknown-after-prelude-client-stream" ||
		!strings.Contains(pluginClose.Error, "bravo_provider_stream_error") {
		t.Fatalf("plugin stream close = %#v, want a safe terminal provider error", pluginClose)
	}
	for _, forbidden := range []string{"req_private", "pm_private", "private diagnostic", `{"type":"error"`} {
		if strings.Contains(pluginClose.Error, forbidden) {
			t.Fatalf("plugin stream close leaked %q: %#v", forbidden, pluginClose)
		}
	}
	if cooldownActive("claude", "palantir", "claude-fable-5", time.Now()) {
		t.Fatal("an unclassified structured error must not cool the subscription")
	}
}

func TestBravoStreamDoesNotFallbackAfterClaudeContent(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-sol",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
		{ID: "codex-one", Name: "codex-one.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	var emitted [][]byte
	var pluginClose rpcStreamCloseRequest
	streamReads := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "claude-content-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			streamReads++
			switch streamReads {
			case 1:
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-fable-5\"}}\n\n"),
				}), nil
			case 2:
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
				}), nil
			case 3:
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"visible\"}}\n\n"),
				}), nil
			default:
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: error\ndata: " + anthropicCreditsRequiredPayload + "\n\n"),
					Done:    true,
				}), nil
			}
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &request)
			emitted = append(emitted, append([]byte(nil), request.Payload...))
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	runBravoStream(bravoContextStreamRequest("claude-content-commit"), "claude-content-client-stream")

	if len(calls) != 1 || calls[0].ForcedProvider != "claude" {
		t.Fatalf("stream provider calls = %#v, want no fallback after Claude content", calls)
	}
	if len(emitted) != 3 {
		t.Fatalf("emitted chunks = %d, want buffered prelude plus two content events", len(emitted))
	}
	visible := strings.Join([]string{string(emitted[0]), string(emitted[1]), string(emitted[2])}, "")
	if !strings.Contains(visible, "visible") {
		t.Fatalf("content delta was not emitted: %q", emitted)
	}
	for _, forbidden := range []string{"req_bravo_credits_private", `event: error`, `{"type":"error"`} {
		if strings.Contains(visible, forbidden) || strings.Contains(pluginClose.Error, forbidden) {
			t.Fatalf("post-content provider error leaked %q: emitted=%q close=%#v", forbidden, emitted, pluginClose)
		}
	}
	if !strings.Contains(pluginClose.Error, "bravo_subscription_model_credits_exhausted") {
		t.Fatalf("plugin stream close = %#v, want safe terminal credits error", pluginClose)
	}
}

func TestBravoStreamContextOverflowDoesNotBlindlyFallback(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "codex",
				Model:        "gpt-5.6-sol",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-terra",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "codex-one", Name: "codex-one.json", Provider: "codex"},
		{ID: "codex-two", Name: "codex-two.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	var pluginClose rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.Model == "gpt-5.6-sol" {
				return nil, &hostCallError{
					Code:       "model_execution_failed",
					Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
					HTTPStatus: http.StatusBadRequest,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "terra-success-stream",
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

	runBravoStream(bravoContextStreamRequest("context-fail-closed"), "context-fail-closed-client-stream")

	if len(calls) != 1 {
		t.Fatalf("stream provider calls = %#v, want only the overflowing Sol attempt", calls)
	}
	if calls[0].Model != "gpt-5.6-sol" {
		t.Fatalf("stream model = %#v, want Sol with unverified Terra left untouched", calls)
	}
	if pluginClose.StreamID != "context-fail-closed-client-stream" ||
		!strings.Contains(pluginClose.Error, "context window") {
		t.Fatalf("plugin stream close = %#v, want a safe context-window terminal error", pluginClose)
	}
	for _, authID := range []string{"codex-one", "codex-two"} {
		if cooldownActive("codex", authID, "gpt-5.6-sol", time.Now()) {
			t.Fatalf("request-scoped context overflow cooled Codex auth %q", authID)
		}
	}
}

func bravoContextStreamRequest(requestID string) rpcExecutorRequest {
	return rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"large history"}],"stream":true}`),
			Metadata:        map[string]any{"request_id": requestID},
		},
		HostCallbackID: requestID + "-callback",
	}
}
