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

type reverseProviderStreamFixture struct {
	name           string
	protocol       string
	requestBody    []byte
	codexPrelude   []byte
	codexContent   []byte
	claudeFallback []pluginapi.HostModelStreamReadResponse
}

func TestBravoStreamCodexPreludeThenServerErrorFallsBackToClaude(t *testing.T) {
	for _, fixture := range reverseProviderStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runReverseProviderStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.codexPrelude},
					reverseProviderServerErrorChunk(),
				},
			)

			assertReverseProviderOrder(t, observation.calls, "codex", "claude")
			visible := joinCrossProtocolPayloads(observation.emitted)
			if strings.Contains(visible, "failed-prelude") {
				t.Fatalf("failed Codex prelude was committed before fallback: %q", visible)
			}
			if !strings.Contains(visible, "fallback-visible") ||
				!strings.Contains(visible, "bravo/fallback-probe") {
				t.Fatalf("coherent logical-model fallback was not emitted: %q", visible)
			}
			if observation.pluginClose.Error != "" {
				t.Fatalf("plugin stream close = %#v, want successful fallback", observation.pluginClose)
			}
			assertReverseProviderCooldown(t)
		})
	}
}

func TestBravoStreamCodexServerErrorAfterContentNeverSplicesButCoolsModel(t *testing.T) {
	for _, fixture := range reverseProviderStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runReverseProviderStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.codexPrelude},
					{Payload: fixture.codexContent},
					reverseProviderServerErrorChunk(),
				},
			)

			assertReverseProviderOrder(t, observation.calls, "codex")
			visible := joinCrossProtocolPayloads(observation.emitted)
			if !strings.Contains(visible, "primary-visible") {
				t.Fatalf("committed Codex content disappeared: %q", visible)
			}
			if strings.Contains(visible, "fallback-visible") {
				t.Fatalf("Claude fallback was spliced into committed Codex output: %q", visible)
			}
			if !strings.Contains(observation.pluginClose.Error, "model_execution_failed") {
				t.Fatalf("plugin stream close = %#v, want terminal provider failure", observation.pluginClose)
			}
			assertReverseProviderCooldown(t)
		})
	}
}

func TestBravoStreamCodexPreludeThenContextOverflowFailsClosed(t *testing.T) {
	for _, fixture := range reverseProviderStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runReverseProviderStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.codexPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "invalid_request_error",
							Message:    "Your input exceeds the context window of this model.",
							HTTPStatus: http.StatusBadRequest,
						},
						Done: true,
					},
				},
			)

			assertReverseProviderOrder(t, observation.calls, "codex")
			if len(observation.emitted) != 0 {
				t.Fatalf("request-scoped failure emitted Codex prelude: %q", observation.emitted)
			}
			if observation.pluginClose.ErrorStatus != http.StatusBadRequest ||
				observation.pluginClose.ErrorCode != "bravo_context_window_exceeded" {
				t.Fatalf("plugin stream close = %#v, want request-scoped context failure", observation.pluginClose)
			}
			assertReverseProviderNoCooldown(t)
		})
	}
}

func reverseProviderServerErrorChunk() pluginapi.HostModelStreamReadResponse {
	return pluginapi.HostModelStreamReadResponse{
		ErrorDetail: &pluginapi.HostModelExecutionError{
			Code:       "model_execution_failed",
			Message:    "An error occurred while processing the provider request.",
			HTTPStatus: http.StatusBadGateway,
			Retryable:  true,
		},
		Done: true,
	}
}

func runReverseProviderStreamScenario(
	t *testing.T,
	fixture reverseProviderStreamFixture,
	codexChunks []pluginapi.HostModelStreamReadResponse,
) crossProtocolStreamObservation {
	t.Helper()
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
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "codex-x20", Name: "codex-x20.json", Provider: "codex", Note: "Codex x20"},
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
	}
	observation := crossProtocolStreamObservation{}
	readIndexes := map[string]int{}
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			observation.calls = append(observation.calls, request.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   request.ForcedProvider + "-reverse-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			var request pluginapi.HostModelStreamReadRequest
			decodeBravoPayload(t, payload, &request)
			chunks := codexChunks
			if strings.HasPrefix(request.StreamID, "claude-") {
				chunks = fixture.claudeFallback
			}
			index := readIndexes[request.StreamID]
			if index >= len(chunks) {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
			}
			readIndexes[request.StreamID] = index + 1
			return mustBravoJSON(t, chunks[index]), nil
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &request)
			observation.emitted = append(
				observation.emitted,
				append([]byte(nil), request.Payload...),
			)
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &observation.pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          fixture.protocol,
			SourceFormat:    fixture.protocol,
			OriginalRequest: fixture.requestBody,
			Metadata:        map[string]any{"request_id": "reverse-provider-" + fixture.name},
		},
		HostCallbackID: "reverse-provider-" + fixture.name + "-callback",
	}, "reverse-provider-"+fixture.name+"-client-stream")
	return observation
}

func reverseProviderStreamFixtures() []reverseProviderStreamFixture {
	return []reverseProviderStreamFixture{
		{
			name:        "claude_messages",
			protocol:    protocolClaude,
			requestBody: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			codexPrelude: []byte(
				"event: message_start\n" +
					`data: {"type":"message_start","message":{"id":"failed-prelude","model":"gpt-5.6-sol","content":[]}}` +
					"\n\n",
			),
			codexContent: []byte(
				"event: content_block_delta\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"primary-visible"}}` +
					"\n\n",
			),
			claudeFallback: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						"event: message_start\n" +
							`data: {"type":"message_start","message":{"id":"fallback-start","model":"claude-fable-5","content":[]}}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: content_block_delta\n" +
							`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fallback-visible"}}` +
							"\n\n",
					),
					Done: true,
				},
			},
		},
		{
			name:        "openai_chat_completions",
			protocol:    protocolOpenAI,
			requestBody: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			codexPrelude: []byte(
				`{"id":"failed-prelude","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			),
			codexContent: []byte(
				`{"id":"primary-content","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"content":"primary-visible"},"finish_reason":null}]}`,
			),
			claudeFallback: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						`{"id":"fallback-content","object":"chat.completion.chunk","created":1,"model":"claude-fable-5","choices":[{"index":0,"delta":{"content":"fallback-visible"},"finish_reason":null}]}`,
					),
					Done: true,
				},
			},
		},
		{
			name:        "openai_responses",
			protocol:    protocolOpenAIResponse,
			requestBody: []byte(`{"model":"bravo/fallback-probe","input":"hello","stream":true}`),
			codexPrelude: []byte(
				"event: response.created\n" +
					`data: {"type":"response.created","sequence_number":1,"response":{"id":"failed-prelude","model":"gpt-5.6-sol","status":"in_progress","output":[]}}` +
					"\n\n",
			),
			codexContent: []byte(
				"event: response.output_text.delta\n" +
					`data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg-primary","output_index":0,"content_index":0,"delta":"primary-visible"}` +
					"\n\n",
			),
			claudeFallback: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						"event: response.created\n" +
							`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp-fallback","object":"response","created_at":1,"status":"in_progress","model":"claude-fable-5","output":[]}}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: response.output_text.delta\n" +
							`data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"msg-fallback","output_index":0,"content_index":0,"delta":"fallback-visible"}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: response.completed\n" +
							`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp-fallback","object":"response","created_at":1,"status":"completed","model":"claude-fable-5","output":[{"id":"msg-fallback","type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback-visible","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}` +
							"\n\n",
					),
					Done: true,
				},
			},
		},
	}
}

func assertReverseProviderOrder(
	t *testing.T,
	calls []pluginapi.HostModelExecutionRequest,
	providers ...string,
) {
	t.Helper()
	if len(calls) != len(providers) {
		t.Fatalf("provider calls = %#v, want providers %v", calls, providers)
	}
	for index, provider := range providers {
		call := calls[index]
		if call.ForcedProvider != provider || !call.SingleAttempt || call.AuthID == "" {
			t.Fatalf("provider call %d = %#v, want pinned single-attempt %s", index, call, provider)
		}
		if provider == "codex" &&
			(call.Model != "gpt-5.6-sol" || call.AuthID != "codex-x20") {
			t.Fatalf("Codex call = %#v, want x20 Sol", call)
		}
		if provider == "claude" &&
			(call.Model != "claude-fable-5" || call.AuthID != "palantir") {
			t.Fatalf("Claude call = %#v, want Palantir Fable", call)
		}
	}
}

func assertReverseProviderCooldown(t *testing.T) {
	t.Helper()
	now := time.Now()
	if !cooldownActive("codex", "codex-x20", "gpt-5.6-sol", now) {
		t.Fatal("retryable Codex model failure did not create a model-scoped cooldown")
	}
	if cooldownActive("codex", "codex-x20", "", now) {
		t.Fatal("retryable Codex model failure created an account-wide cooldown")
	}
	if cooldownActive("claude", "palantir", "claude-fable-5", now) {
		t.Fatal("successful Claude fallback received a cooldown")
	}
}

func assertReverseProviderNoCooldown(t *testing.T) {
	t.Helper()
	now := time.Now()
	for _, key := range []struct {
		provider string
		authID   string
		model    string
	}{
		{provider: "codex", authID: "codex-x20"},
		{provider: "codex", authID: "codex-x20", model: "gpt-5.6-sol"},
		{provider: "claude", authID: "palantir"},
		{provider: "claude", authID: "palantir", model: "claude-fable-5"},
	} {
		if cooldownActive(key.provider, key.authID, key.model, now) {
			t.Fatalf("unexpected cooldown for provider=%s auth=%s model=%s", key.provider, key.authID, key.model)
		}
	}
}
