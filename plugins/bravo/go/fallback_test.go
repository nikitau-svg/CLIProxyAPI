package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBravoExecuteExhaustsPrimaryAuthsBeforeNextCandidate(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "primary-model",
				Priority:     100,
				Capabilities: []string{capabilityText},
			},
			{
				Provider:     "codex",
				Model:        "fallback-model",
				Priority:     90,
				Capabilities: []string{capabilityText},
			},
		},
	})
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Name: "claude-a.json", Provider: "claude"},
		{ID: "claude-b", Name: "claude-b.json", Provider: "claude"},
		{ID: "codex-a", Name: "codex-a.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			if !req.SingleAttempt {
				t.Fatal("nested host execution did not set single_attempt")
			}
			if strings.TrimSpace(req.AuthID) == "" {
				t.Fatal("nested host execution did not pin auth_id")
			}
			if req.ForcedProvider == "claude" {
				return nil, &hostCallError{
					Code:       "rate_limited",
					Message:    "forced primary failure",
					Retryable:  true,
					HTTPStatus: http.StatusTooManyRequests,
				}
			}
			if req.ForcedProvider != "codex" {
				t.Fatalf("unexpected provider call %q", req.ForcedProvider)
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"id":"fallback-ok","model":"fallback-model","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
			Metadata:        map[string]any{"request_id": "fallback-proof"},
		},
		HostCallbackID: "fallback-proof-callback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo execution failed: %#v", env.Error)
	}
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !strings.Contains(string(response.Payload), `"model":"bravo/fallback-probe"`) {
		t.Fatalf("logical model was not preserved in fallback response: %s", response.Payload)
	}

	if len(calls) != 3 {
		t.Fatalf("host model calls = %d, want two primary auths then one fallback: %#v", len(calls), calls)
	}
	for index := 0; index < 2; index++ {
		if calls[index].ForcedProvider != "claude" || calls[index].Model != "primary-model" {
			t.Fatalf("call %d = %#v, want primary Claude candidate", index, calls[index])
		}
	}
	gotPrimaryAuths := []string{calls[0].AuthID, calls[1].AuthID}
	sort.Strings(gotPrimaryAuths)
	if strings.Join(gotPrimaryAuths, ",") != "claude-a,claude-b" {
		t.Fatalf("primary auths = %v, want every eligible primary auth", gotPrimaryAuths)
	}
	last := calls[2]
	if last.ForcedProvider != "codex" || last.AuthID != "codex-a" || last.Model != "fallback-model" {
		t.Fatalf("fallback call = %#v, want pinned Codex fallback candidate", last)
	}
}

func TestBravoExecutePreservesOpenAIChatVisionAcrossClaudeToCodexFallback(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-sonnet-5",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityVision},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-terra",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityVision},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Provider: "claude"},
		{ID: "codex-a", Provider: "codex"},
	}
	const imageURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if !strings.Contains(string(request.Body), imageURL) ||
				!strings.Contains(string(request.Body), `"type":"image_url"`) ||
				!strings.Contains(string(request.Body), `"detail":"high"`) {
				t.Fatalf("vision content was not preserved for %s: %s", request.ForcedProvider, request.Body)
			}
			if request.ForcedProvider == "claude" {
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"opaque provider rejection"}}`),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.6-terra","choices":[{"message":{"role":"assistant","content":"image received"}}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	body := []byte(`{
		"model":"bravo/sonnet",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is shown?"},
			{"type":"image_url","image_url":{"url":"` + imageURL + `","detail":"high"}}
		]}]
	}`)
	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			OriginalRequest: body,
		},
		HostCallbackID: "openai-vision-fallback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo vision execution failed: %#v", env.Error)
	}
	if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("calls = %#v, want Claude then Codex", calls)
	}
}

func TestBravoExecuteAcceptsJSONObjectAcrossClaudeToCodexFallback(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 90, Capabilities: []string{capabilityText}},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Provider: "claude"},
		{ID: "codex-a", Provider: "codex"},
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
			if !strings.Contains(string(request.Body), `"response_format":{"type":"json_object"}`) {
				t.Fatalf("json_object hint was not preserved for %s: %s", request.ForcedProvider, request.Body)
			}
			if request.ForcedProvider == "claude" {
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"opaque provider rejection"}}`),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.6-terra","choices":[{"message":{"role":"assistant","content":"{\\"synced\\":true}"}}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	body := []byte(`{"model":"bravo/sonnet","messages":[{"role":"user","content":"sync"}],"response_format":{"type":"json_object"}}`)
	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			OriginalRequest: body,
		},
		HostCallbackID: "json-object-fallback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo json_object execution failed: %#v", env.Error)
	}
	if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("calls = %#v, want Claude then Codex", calls)
	}
}

func TestBravoExecuteClientEffortResolvesForEveryFallback(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-sonnet-4-6",
				Effort:       "low",
				Priority:     100,
				Capabilities: []string{capabilityText},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.5",
				Effort:       "medium",
				Priority:     90,
				Capabilities: []string{capabilityText},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Name: "claude-a.json", Provider: "claude"},
		{ID: "codex-a", Name: "codex-a.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			if strings.Contains(string(req.Body), "reasoning_effort") || strings.Contains(string(req.Body), `"reasoning"`) {
				t.Fatalf("client effort leaked beside suffix authority: %s", req.Body)
			}
			if req.ForcedProvider == "claude" {
				return nil, &hostCallError{
					Code:       "rate_limited",
					Message:    "force fallback",
					Retryable:  true,
					HTTPStatus: http.StatusTooManyRequests,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.5","choices":[{"message":{"content":"ok"}}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:        "bravo/fallback-probe",
			Format:       protocolOpenAI,
			SourceFormat: protocolOpenAI,
			OriginalRequest: []byte(`{
				"model":"bravo/fallback-probe",
				"messages":[{"role":"user","content":"hello"}],
				"reasoning_effort":"xhigh"
			}`),
		},
		HostCallbackID: "effort-fallback-callback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo execution failed: %#v", env.Error)
	}
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if got := response.Metadata["bravo_effort"]; got != "xhigh" {
		t.Fatalf("response Bravo effort = %#v, want xhigh", got)
	}
	if got := response.Metadata["bravo_requested_effort"]; got != "xhigh" {
		t.Fatalf("response requested effort = %#v, want xhigh", got)
	}
	if got := response.Metadata["bravo_effective_effort"]; got != "xhigh" {
		t.Fatalf("response effective effort = %#v, want xhigh", got)
	}
	if len(calls) != 2 {
		t.Fatalf("host model calls = %d, want primary and fallback: %#v", len(calls), calls)
	}
	if calls[0].Model != "claude-sonnet-4-6(high)" {
		t.Fatalf("primary model = %q, want physical xhigh floor to high", calls[0].Model)
	}
	if calls[1].Model != "gpt-5.5(xhigh)" {
		t.Fatalf("fallback model = %q, want supported xhigh", calls[1].Model)
	}
	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %#v, want primary and fallback", attempts)
	}
	if attempts[0].RequestedEffort != "xhigh" || attempts[0].EffectiveEffort != "high" || attempts[0].Effort != "high" {
		t.Fatalf("primary attempt effort metadata = %#v, want requested xhigh/effective high", attempts[0])
	}
	if attempts[1].RequestedEffort != "xhigh" || attempts[1].EffectiveEffort != "xhigh" || attempts[1].Effort != "xhigh" {
		t.Fatalf("fallback attempt effort metadata = %#v, want requested/effective xhigh", attempts[1])
	}
}

func TestBravoForcedClaudeToolChoiceSkipsClaudeAndKeepsEffortOnCodex(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-model",
				Effort:       "medium",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityTools},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.5",
				Effort:       "medium",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityTools},
			},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 1
	currentConfig.Store(cfg)

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Provider: "claude"},
		{ID: "codex-a", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			calls = append(calls, req.HostModelExecutionRequest)
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"model":"gpt-5.5","content":[{"type":"text","text":"ok"}]}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
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
				"messages":[{"role":"user","content":"call the tool"}],
				"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
				"tool_choice":{"type":"any"},
				"thinking":{"type":"adaptive"},
				"output_config":{"effort":"high"}
			}`),
		},
		HostCallbackID: "forced-tool-effort-callback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo execution failed: %#v", env.Error)
	}
	if len(calls) != 1 {
		t.Fatalf("host model calls = %d, want only safe Codex fallback: %#v", len(calls), calls)
	}
	if calls[0].ForcedProvider != "codex" || calls[0].Model != "gpt-5.5(high)" {
		t.Fatalf("safe fallback call = %#v, want Codex at requested high effort", calls[0])
	}
}

func TestBravoStreamDoesNotFallbackAfterFirstPayload(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "primary-stream-model",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
			{
				Provider:     "codex",
				Model:        "fallback-stream-model",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", Name: "claude-a.json", Provider: "claude"},
		{ID: "claude-b", Name: "claude-b.json", Provider: "claude"},
		{ID: "codex-a", Name: "codex-a.json", Provider: "codex"},
	}
	var modelCalls []pluginapi.HostModelExecutionRequest
	var emitted [][]byte
	var pluginClose rpcStreamCloseRequest
	readCount := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var req hostModelExecutionRequest
			decodeBravoPayload(t, payload, &req)
			modelCalls = append(modelCalls, req.HostModelExecutionRequest)
			if !req.SingleAttempt || req.AuthID == "" {
				t.Fatalf("stream attempt is not pinned and single-shot: %#v", req.HostModelExecutionRequest)
			}
			if req.ForcedProvider != "claude" {
				t.Fatalf("stream fell back after a committed payload: %#v", req.HostModelExecutionRequest)
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "primary-upstream-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			readCount++
			if readCount == 1 {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					Payload: []byte(`{"model":"primary-stream-model","choices":[{"delta":{"content":"visible"}}]}`),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
				Error: "rate limited after bytes",
				ErrorDetail: &pluginapi.HostModelExecutionError{
					Code:       "rate_limited",
					Message:    "rate limited after bytes",
					HTTPStatus: http.StatusTooManyRequests,
					Retryable:  true,
				},
			}), nil
		case pluginabi.MethodHostStreamEmit:
			var req rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &req)
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			return mustBravoJSON(t, map[string]any{}), nil
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

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			Metadata:        map[string]any{"request_id": "committed-stream-proof"},
		},
		HostCallbackID: "committed-stream-callback",
	}, "client-stream")

	if len(modelCalls) != 1 {
		t.Fatalf("model stream calls = %d, want exactly one after first payload: %#v", len(modelCalls), modelCalls)
	}
	if modelCalls[0].ForcedProvider != "claude" {
		t.Fatalf("first stream provider = %q, want claude", modelCalls[0].ForcedProvider)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted chunks = %d, want one committed payload", len(emitted))
	}
	if text := string(emitted[0]); strings.Contains(text, "primary-stream-model") ||
		!strings.Contains(text, `"model":"bravo/fallback-probe"`) {
		t.Fatalf("unexpected emitted payload: %s", text)
	}
	if pluginClose.StreamID != "client-stream" || !strings.Contains(pluginClose.Error, "rate_limited") {
		t.Fatalf("plugin stream close = %#v, want explicit terminal primary error", pluginClose)
	}
}

func installBravoTestConfig(t *testing.T, model logicalModel) {
	t.Helper()
	previous := loadedConfig()
	cfg := pluginConfig{
		Enabled:         true,
		Prefix:          defaultPrefix,
		RequireSmartKey: false,
		CooldownSeconds: 30,
		Models: map[string]logicalModel{
			"fallback-probe": model,
		},
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previous)
	})
}

func installBravoHostCall(t *testing.T, callback hostCallFunc) {
	t.Helper()
	previous := swapHostCall(callback)
	t.Cleanup(func() {
		swapHostCall(previous)
	})
}

func isolateBravoFallbackTestState(t *testing.T) {
	t.Helper()
	runtimeState.Lock()
	previousCooldowns := runtimeState.Cooldowns
	previousAttempts := runtimeState.Attempts
	runtimeState.Cooldowns = make(map[string]cooldownEntry)
	runtimeState.Attempts = nil
	runtimeState.Unlock()
	t.Cleanup(func() {
		runtimeState.Lock()
		runtimeState.Cooldowns = previousCooldowns
		runtimeState.Attempts = previousAttempts
		runtimeState.Unlock()
	})
}

func mustBravoJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func decodeBravoPayload(t *testing.T, payload any, target any) {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
}
