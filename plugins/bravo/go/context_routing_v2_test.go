package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegisteredLogicalModelUsesOneReviewedCapacityTuple(t *testing.T) {
	item := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-small", Priority: 90, Capabilities: []string{capabilityText}},
	}}
	hostModels := []pluginapi.HostModelListEntry{
		{Provider: "claude", ID: "claude-opus-5", InputTokenLimit: 900000, ContextLength: 1000000, MaxCompletionTokens: 100000, Catalog: true},
		{Provider: "codex", ID: "gpt-small", InputTokenLimit: 120000, ContextLength: 128000, MaxCompletionTokens: 64000, Catalog: true},
	}

	info := registeredLogicalModel(defaultPrefix, "opus", item, hostModels)
	if info.InputTokenLimit != 900000 || info.ContextLength != 1000000 ||
		info.MaxCompletionTokens != 100000 || info.OutputTokenLimit != 100000 {
		t.Fatalf("logical limits = %#v, want one complete Claude tuple", info)
	}
}

func TestRegisteredLogicalModelDoesNotInvent128KWithoutMetadata(t *testing.T) {
	info := registeredLogicalModel(defaultPrefix, "unknown", logicalModel{
		Candidates: []candidate{{Provider: "codex", Model: "future-model", Capabilities: []string{capabilityText}}},
	})
	if info.InputTokenLimit != 0 || info.OutputTokenLimit != 0 ||
		info.ContextLength != 0 || info.MaxCompletionTokens != 0 {
		t.Fatalf("logical model invented context limits: %#v", info)
	}
}

func TestBravoContextOverflowFallsBackOnlyToProvenCompatibleCandidate(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-small", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-large", Priority: 90, Capabilities: []string{capabilityText}},
	}})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-one", Name: "claude-one.json", Provider: "claude"},
		{ID: "codex-one", Name: "codex-one.json", Provider: "codex"},
		{ID: "codex-two", Name: "codex-two.json", Provider: "codex"},
	}
	models := []pluginapi.HostModelListEntry{
		{Provider: "claude", ID: "claude-small", InputTokenLimit: 100, ContextLength: 120, MaxCompletionTokens: 20, Catalog: true, Available: true},
		{Provider: "codex", ID: "gpt-large", InputTokenLimit: 300, ContextLength: 320, MaxCompletionTokens: 20, Catalog: true, Available: true},
	}
	var calls []pluginapi.HostModelExecutionRequest
	var countCalls []pluginapi.HostModelExecutionRequest
	codexExecutions := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelList:
			return mustBravoJSON(t, pluginapi.HostModelListResponse{Models: models}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.Model == "claude-small" {
				return nil, &hostCallError{
					Code:       "context_window_exceeded",
					Message:    "Input exceeds this model's context window.",
					HTTPStatus: http.StatusBadRequest,
					ProviderError: &providererror.Detail{
						Code:            "context_window_exceeded",
						Class:           "context_window",
						Scope:           "request",
						RequiredTokens:  200,
						LimitTokens:     100,
						TaxonomyVersion: 1,
					},
				}
			}
			codexExecutions++
			if codexExecutions == 1 {
				return nil, &hostCallError{
					Code:       "rate_limit_error",
					Message:    "The provider rate limit was reached.",
					HTTPStatus: http.StatusTooManyRequests,
					Retryable:  true,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}), nil
		case pluginabi.MethodHostModelCountTokens:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			countCalls = append(countCalls, request.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"input_tokens":200}`),
			}), nil
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
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","max_tokens":20,"messages":[{"role":"user","content":"large"}]}`),
		},
		HostCallbackID: "context-proven-fallback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("response = %s, want compatible fallback success", raw)
	}
	if len(calls) != 3 || calls[0].Model != "claude-small" ||
		calls[1].Model != "gpt-large" || calls[2].Model != "gpt-large" {
		t.Fatalf("provider calls = %#v, want Claude then two proven-compatible Codex credentials", calls)
	}
	if len(countCalls) != 1 || countCalls[0].Model != "gpt-large" || countCalls[0].ForcedProvider != "codex" {
		t.Fatalf("target proof calls = %#v, want one exact Codex count", countCalls)
	}
	for _, authID := range []string{"claude-one"} {
		if cooldownActive("claude", authID, "claude-small", time.Now()) {
			t.Fatalf("request-scoped context overflow cooled %q", authID)
		}
	}
}

func TestBravoContextOverflowWithoutComparableCountFailsClosed(t *testing.T) {
	requirement := contextRequirement{
		RequiredInputTokens: 200,
		CountKind:           contextCountExact,
		CountScope:          contextCountTargetModel,
		Provider:            "claude",
		Model:               "claude-small",
	}
	limits := newHostModelLimitIndex([]pluginapi.HostModelListEntry{{
		Provider: "codex", ID: "gpt-large", InputTokenLimit: 400, ContextLength: 500, MaxCompletionTokens: 50, Catalog: true,
	}})
	if contextCandidateCompatibility(candidate{Provider: "codex", Model: "gpt-large"}, requirement, 20, limits) != contextCompatibilityUnknown {
		t.Fatal("cross-provider token count was treated as comparable")
	}
}

func TestContextRoutingUnknownCapacityFailsClosedBeforeCountProbe(t *testing.T) {
	isolateBravoFallbackTestState(t)
	countCalls := 0
	installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelList:
			return mustBravoJSON(t, pluginapi.HostModelListResponse{}), nil
		case pluginabi.MethodHostModelCountTokens:
			countCalls++
			t.Fatal("unknown target capacity must fail before token-count probe")
			return nil, nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})
	state := newContextRoutingState("unknown-capacity")
	state.requirement = contextRequirement{
		RequiredInputTokens: 200,
		CountKind:           contextCountExact,
		CountScope:          contextCountTargetModel,
		Provider:            "claude",
		Model:               "claude-small",
	}
	attempt := executionAttempt{Candidate: candidate{Provider: "codex", Model: "future-model"}}
	if state.proveCandidate(rpcExecutorRequest{}, attempt, protocolClaude, "future-model", []byte(`{"max_tokens":20}`)) {
		t.Fatal("unknown-capacity target was accepted")
	}
	if countCalls != 0 {
		t.Fatalf("count calls = %d, want zero", countCalls)
	}
}

func TestClaudeEmptyContentBlockStartDoesNotCommitStream(t *testing.T) {
	payload := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	if claudeStreamPayloadContainsContent(payload) {
		t.Fatal("empty Claude content block start committed the stream")
	}

	toolPayload := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"Edit\"}}\n\n")
	if !claudeStreamPayloadContainsContent(toolPayload) {
		t.Fatal("Claude tool-use start did not commit the stream")
	}
}

func TestBravoStreamContextFallbackUsesTargetCountBeforeCommit(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-small", Priority: 100, Capabilities: []string{capabilityText, capabilityStream}},
		{Provider: "codex", Model: "gpt-large", Priority: 90, Capabilities: []string{capabilityText, capabilityStream}},
	}})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-one", Name: "claude-one.json", Provider: "claude"},
		{ID: "codex-one", Name: "codex-one.json", Provider: "codex"},
	}
	models := []pluginapi.HostModelListEntry{
		{Provider: "claude", ID: "claude-small", InputTokenLimit: 100, ContextLength: 120, MaxCompletionTokens: 20, Catalog: true, Available: true},
		{Provider: "codex", ID: "gpt-large", InputTokenLimit: 300, ContextLength: 320, MaxCompletionTokens: 20, Catalog: true, Available: true},
	}
	var executions, counts int
	var emitted [][]byte
	var pluginClose rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelList:
			return mustBravoJSON(t, pluginapi.HostModelListResponse{Models: models}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			executions++
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   request.Model + "-stream",
			}), nil
		case pluginabi.MethodHostModelCountTokens:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			if request.Stream || !strings.Contains(string(request.Body), `"stream":false`) {
				t.Fatalf("stream proof request was not normalized for count: %#v body=%s", request.HostModelExecutionRequest, request.Body)
			}
			counts++
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"input_tokens":200}`),
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			var request pluginapi.HostModelStreamReadRequest
			decodeBravoPayload(t, payload, &request)
			if request.StreamID == "claude-small-stream" {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"prompt is too long: 200 tokens > 100 maximum\"}}\n\n"),
					Done:    true,
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
				Payload: []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"),
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

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","max_tokens":20,"messages":[{"role":"user","content":"large"}],"stream":true}`),
		},
		HostCallbackID: "stream-context-proof",
	}, "stream-context-client")

	if executions != 2 || counts != 1 {
		t.Fatalf("stream executions/counts = %d/%d, want 2/1", executions, counts)
	}
	if pluginClose.Error != "" {
		t.Fatalf("stream close = %#v, want success", pluginClose)
	}
	if !strings.Contains(string(joinByteSlices(emitted)), "ok") {
		t.Fatalf("winning stream payloads = %q, want Codex content", emitted)
	}
}

func TestContextFailureMessageIncludesKnownCounts(t *testing.T) {
	failure := contextExecutionFailure(providererror.Detail{
		Code:            "context_window_exceeded",
		Class:           "context_window",
		Scope:           "request",
		RequiredTokens:  1003466,
		LimitTokens:     1000000,
		TaxonomyVersion: 1,
	})
	if !strings.Contains(failure.Message, "1 003 466") || !strings.Contains(failure.Message, "1 000 000") {
		t.Fatalf("localized context message = %q", failure.Message)
	}
}

func joinByteSlices(values [][]byte) []byte {
	var joined []byte
	for _, value := range values {
		joined = append(joined, value...)
	}
	return joined
}
