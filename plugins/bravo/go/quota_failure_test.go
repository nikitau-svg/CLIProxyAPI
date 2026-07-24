package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const anthropicExtraUsageMessage = `model_execution_failed: {"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Add more at claude.ai/admin-settings/usage and keep going."}}`

func TestClassifyExecutionErrorRetriesAnthropicExtraUsage400(t *testing.T) {
	t.Parallel()

	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    anthropicExtraUsageMessage,
		HTTPStatus: http.StatusBadRequest,
	})
	if !failure.Retryable {
		t.Fatalf("failure = %#v, want retryable account quota exhaustion", failure)
	}
	if failure.Code != "bravo_subscription_quota_exhausted" {
		t.Fatalf("code = %q, want bravo_subscription_quota_exhausted", failure.Code)
	}
}

func TestClassifyExecutionErrorKeepsOrdinaryInvalidRequestTerminal(t *testing.T) {
	t.Parallel()

	for _, message := range []string{
		`invalid_request_error: max_tokens must be greater than zero`,
		`invalid_request_error: response_format JSON schema is invalid`,
		`invalid_request_error: tool_choice references an unknown tool`,
	} {
		failure := classifyExecutionError(&hostCallError{
			Code:       "model_execution_failed",
			Message:    message,
			HTTPStatus: http.StatusBadRequest,
		})
		if failure.Retryable {
			t.Fatalf("failure = %#v, malformed request must remain terminal", failure)
		}
		if failure.Code != "model_execution_failed" {
			t.Fatalf("code = %q, want original host error code", failure.Code)
		}
	}
}

func TestClassifyExecutionErrorRetriesAccountScopedAuthFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantCode string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantCode: "bravo_subscription_auth_unavailable"},
		{name: "forbidden", status: http.StatusForbidden, wantCode: "bravo_subscription_access_denied"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyExecutionError(&hostCallError{
				Code:       "provider_access_error",
				Message:    "the pinned subscription cannot access the provider",
				HTTPStatus: test.status,
			})
			if !failure.Retryable || failure.Code != test.wantCode {
				t.Fatalf("failure = %#v, want retryable %s", failure, test.wantCode)
			}
		})
	}
}

func TestClassifyExecutionErrorRetriesReviewedModelEntitlement400(t *testing.T) {
	t.Parallel()

	signals := []string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, signal := range signals {
		t.Run(signal, func(t *testing.T) {
			failure := classifyExecutionError(&hostCallError{
				Code:       "model_execution_failed",
				Message:    `invalid_request_error: ` + signal,
				HTTPStatus: http.StatusBadRequest,
			})
			if !failure.Retryable || failure.Code != "bravo_subscription_model_unavailable" {
				t.Fatalf("failure = %#v, want retryable model entitlement failure", failure)
			}
		})
	}
}

func TestClassifyExecutionErrorRetriesReviewedModelEntitlement422(t *testing.T) {
	t.Parallel()

	failure := classifyExecutionError(&hostCallError{
		Code:       "model_not_supported",
		Message:    "The requested model is unavailable for this account",
		HTTPStatus: http.StatusUnprocessableEntity,
	})
	if !failure.Retryable || failure.Code != "bravo_subscription_model_unavailable" {
		t.Fatalf("failure = %#v, want retryable upstream model entitlement failure", failure)
	}
}

func TestClassifyHTTPFailureRetriesReviewedModelEntitlementBody(t *testing.T) {
	t.Parallel()

	failure := classifyHTTPFailure(
		http.StatusBadRequest,
		nil,
		"candidate returned an HTTP error",
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"The requested model is not available for your plan"}}`),
	)
	if !failure.Retryable || failure.Code != "bravo_subscription_model_unavailable" {
		t.Fatalf("failure = %#v, want retryable response-body model entitlement failure", failure)
	}
}

func TestContractFailureRemainsTerminal(t *testing.T) {
	t.Parallel()

	failure := contractFailure(&capabilityContractError{
		Code:       "bravo_capability_unsupported",
		Provider:   "claude",
		Protocol:   protocolClaude,
		Capability: capabilityStructuredOutput,
		Message:    "structured output contract is not verified",
	})
	if failure.Retryable || failure.Status != http.StatusUnprocessableEntity {
		t.Fatalf("failure = %#v, local contract failures must remain terminal", failure)
	}
}

func TestStreamChunkFailureRetriesAnthropicExtraUsageBeforeCommit(t *testing.T) {
	t.Parallel()

	failure := streamChunkFailure(pluginapi.HostModelStreamReadResponse{
		ErrorDetail: &pluginapi.HostModelExecutionError{
			Code:       "model_execution_failed",
			Message:    anthropicExtraUsageMessage,
			HTTPStatus: http.StatusBadRequest,
		},
	})
	if !failure.Retryable || failure.Code != "bravo_subscription_quota_exhausted" {
		t.Fatalf("stream failure = %#v, want retryable account quota exhaustion", failure)
	}
}

func TestStreamChunkFailureRetriesAccountAndModelAvailabilityBeforeCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		message  string
		wantCode string
	}{
		{
			name:     "unauthorized subscription",
			status:   http.StatusUnauthorized,
			message:  "OAuth token expired",
			wantCode: "bravo_subscription_auth_unavailable",
		},
		{
			name:     "forbidden subscription",
			status:   http.StatusForbidden,
			message:  "subscription cannot access this provider",
			wantCode: "bravo_subscription_access_denied",
		},
		{
			name:     "model entitlement",
			status:   http.StatusBadRequest,
			message:  "requested model is not available for your plan",
			wantCode: "bravo_subscription_model_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := streamChunkFailure(pluginapi.HostModelStreamReadResponse{
				ErrorDetail: &pluginapi.HostModelExecutionError{
					Code:       "model_execution_failed",
					Message:    test.message,
					HTTPStatus: test.status,
				},
			})
			if !failure.Retryable || failure.Code != test.wantCode {
				t.Fatalf("stream failure = %#v, want retryable %s", failure, test.wantCode)
			}
		})
	}
}

func TestBravoExecuteFallsBackToCodexOnAnthropicExtraUsage400(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-opus-4-8",
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
		{ID: "claude-exhausted", Name: "claude-exhausted.json", Provider: "claude"},
		{ID: "codex-ready", Name: "codex-ready.json", Provider: "codex"},
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
					Message:    anthropicExtraUsageMessage,
					HTTPStatus: http.StatusBadRequest,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.6-sol","content":[{"type":"text","text":"ok"}]}`),
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
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
		},
		HostCallbackID: "anthropic-extra-usage-fallback",
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
	if len(calls) != 2 {
		t.Fatalf("host model calls = %d, want Claude then Codex: %#v", len(calls), calls)
	}
	if calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("provider order = %q, %q, want claude, codex", calls[0].ForcedProvider, calls[1].ForcedProvider)
	}

	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(attempts) != 2 || attempts[0].ErrorCode != "bravo_subscription_quota_exhausted" || !attempts[0].Retryable {
		t.Fatalf("attempt diagnostics = %#v", attempts)
	}
	if !strings.Contains(string(raw), "bravo/fallback-probe") {
		t.Fatalf("logical model was not restored in response: %s", raw)
	}
}

func TestBravoExecuteFallsBackForAccountAndModelAvailabilityFailures(t *testing.T) {
	tests := []struct {
		name     string
		failure  hostCallError
		wantCode string
	}{
		{
			name: "unauthorized subscription",
			failure: hostCallError{
				Code:       "authentication_error",
				Message:    `{"type":"error","error":{"type":"authentication_error","message":"OAuth token expired"}}`,
				HTTPStatus: http.StatusUnauthorized,
			},
			wantCode: "bravo_subscription_auth_unavailable",
		},
		{
			name: "forbidden subscription",
			failure: hostCallError{
				Code:       "permission_error",
				Message:    `{"type":"error","error":{"type":"permission_error","message":"This subscription cannot access the model"}}`,
				HTTPStatus: http.StatusForbidden,
			},
			wantCode: "bravo_subscription_access_denied",
		},
		{
			name: "model unavailable for plan",
			failure: hostCallError{
				Code:       "model_execution_failed",
				Message:    `{"type":"error","error":{"type":"invalid_request_error","message":"The requested model is not available for your plan"}}`,
				HTTPStatus: http.StatusBadRequest,
			},
			wantCode: "bravo_subscription_model_unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			installBravoTestConfig(t, logicalModel{
				Candidates: []candidate{
					{
						Provider:     "claude",
						Model:        "claude-opus-5",
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
				{ID: "claude-unavailable", Name: "claude-unavailable.json", Provider: "claude"},
				{ID: "codex-ready", Name: "codex-ready.json", Provider: "codex"},
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
						failure := test.failure
						return nil, &failure
					}
					return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
						StatusCode: http.StatusOK,
						Headers:    http.Header{"Content-Type": []string{"application/json"}},
						Body:       []byte(`{"model":"gpt-5.6-sol","content":[{"type":"text","text":"ok"}]}`),
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
					OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
				},
				HostCallbackID: "account-model-availability-fallback",
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
			if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
				t.Fatalf("calls = %#v, want Claude then Codex", calls)
			}

			runtimeState.RLock()
			attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
			runtimeState.RUnlock()
			if len(attempts) != 2 || attempts[0].ErrorCode != test.wantCode || !attempts[0].Retryable {
				t.Fatalf("attempt diagnostics = %#v, want retryable %s", attempts, test.wantCode)
			}
		})
	}
}

func TestBravoExecuteDoesNotFallbackOnMalformedOrSchema400(t *testing.T) {
	for _, message := range []string{
		`invalid_request_error: max_tokens must be greater than zero`,
		`invalid_request_error: response_format JSON schema is invalid`,
	} {
		t.Run(message, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			installBravoTestConfig(t, logicalModel{
				Candidates: []candidate{
					{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
					{Provider: "codex", Model: "gpt-5.6-sol", Priority: 90, Capabilities: []string{capabilityText}},
				},
			})
			auths := []pluginapi.HostAuthFileEntry{
				{ID: "claude-ready", Name: "claude-ready.json", Provider: "claude"},
				{ID: "codex-ready", Name: "codex-ready.json", Provider: "codex"},
			}
			calls := 0
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostModelExecute:
					var request hostModelExecutionRequest
					decodeBravoPayload(t, payload, &request)
					calls++
					if request.ForcedProvider != "claude" {
						t.Fatalf("malformed request reached fallback provider: %#v", request)
					}
					return nil, &hostCallError{
						Code:       "model_execution_failed",
						Message:    message,
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
					OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
				},
				HostCallbackID: "terminal-invalid-request",
			}))
			if errExecute != nil {
				t.Fatal(errExecute)
			}
			var env envelope
			if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if env.OK || env.Error == nil || env.Error.Retryable || calls != 1 {
				t.Fatalf("response=%s calls=%d, malformed/schema 400 must remain terminal", raw, calls)
			}
		})
	}
}
