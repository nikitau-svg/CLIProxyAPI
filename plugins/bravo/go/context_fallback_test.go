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
	for _, want := range []string{"Fable 5", "monthly spend", "gpt-5.6-sol", "context window"} {
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
