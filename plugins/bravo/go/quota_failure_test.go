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

const anthropicLegacyExtraUsageMessage = `model_execution_failed: {"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Add more at claude.ai/admin-settings/usage and keep going."}}`

const anthropicThirdPartyExtraUsageMessage = `model_execution_failed: {"type":"error","error":{"type":"invalid_request_error","message":"Third-party apps now draw from your extra usage, not your plan limits. Add more at claude.ai/settings/usage and keep going."}}`

func TestClassifyExecutionErrorRetriesAnthropicExtraUsage400(t *testing.T) {
	for name, message := range map[string]string{
		"legacy exhaustion":       anthropicLegacyExtraUsageMessage,
		"third-party extra usage": anthropicThirdPartyExtraUsageMessage,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			failure := classifyExecutionError(&hostCallError{
				Code:       "model_execution_failed",
				Message:    message,
				HTTPStatus: http.StatusBadRequest,
			})
			if !failure.Retryable {
				t.Fatalf("failure = %#v, want retryable account quota exhaustion", failure)
			}
			if failure.Code != "bravo_subscription_quota_exhausted" {
				t.Fatalf("code = %q, want bravo_subscription_quota_exhausted", failure.Code)
			}
		})
	}
}

func TestClassifyExecutionErrorAggregatesAccountWideQuotaAcrossFields(t *testing.T) {
	t.Parallel()

	failure := classifyExecutionError(&hostCallError{
		Code:       "usage limit has been reached",
		Message:    anthropicThirdPartyExtraUsageMessage,
		HTTPStatus: http.StatusBadRequest,
	})
	if !failure.Retryable || failure.Code != "bravo_subscription_quota_exhausted" {
		t.Fatalf("failure = %#v, want retryable quota exhaustion", failure)
	}
	if !failure.AccountWide {
		t.Fatalf("failure = %#v, exact later Anthropic signal must widen the cooldown to the account", failure)
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

func TestClassifyExecutionErrorAllowsFallbackForAmbiguousProvider400(t *testing.T) {
	t.Parallel()

	detail := providererror.Detail{
		Type:    "invalid_request_error",
		Message: "The provider rejected this request.",
	}
	failure := classifyExecutionError(&hostCallError{
		Code:          "model_execution_failed",
		Message:       detail.Message,
		HTTPStatus:    http.StatusBadRequest,
		ProviderError: &detail,
	})
	if failure.Retryable {
		t.Fatalf("failure = %#v, ambiguous candidate failure must not create a provider cooldown", failure)
	}
	if !failure.RouteFallback || failure.Code != "bravo_provider_ambiguous_invalid_request" {
		t.Fatalf("failure = %#v, want candidate-local fallback", failure)
	}
}

func TestClassifyExecutionErrorKeepsPreciseProvider400Terminal(t *testing.T) {
	t.Parallel()

	detail := providererror.Detail{
		Type:      "invalid_request_error",
		Code:      "invalid_tool_parameters",
		Parameter: "tools[3].function.parameters",
		Message:   "Invalid tool schema.",
	}
	failure := classifyExecutionError(&hostCallError{
		Code:          detail.Code,
		Message:       detail.Message,
		HTTPStatus:    http.StatusBadRequest,
		ProviderError: &detail,
	})
	if failure.Retryable || failure.RouteFallback {
		t.Fatalf("failure = %#v, precise invalid parameter must remain terminal", failure)
	}
	if failure.Code != "invalid_tool_parameters" {
		t.Fatalf("code = %q, want precise provider code", failure.Code)
	}
}

func TestClassifyHTTPFailureAllowsFallbackForAmbiguousProvider400Body(t *testing.T) {
	t.Parallel()

	failure := classifyHTTPFailure(
		http.StatusBadRequest,
		nil,
		"provider returned an HTTP error",
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"The provider rejected this request."}}`),
	)
	if failure.Retryable {
		t.Fatalf("failure = %#v, ambiguous candidate failure must not create a provider cooldown", failure)
	}
	if !failure.RouteFallback || failure.Code != "bravo_provider_ambiguous_invalid_request" {
		t.Fatalf("failure = %#v, want candidate-local fallback from an ordinary HTTP response body", failure)
	}
}

func TestClassifyHTTPFailureKeepsPreciseProvider400BodyTerminal(t *testing.T) {
	t.Parallel()

	failure := classifyHTTPFailure(
		http.StatusBadRequest,
		nil,
		"provider returned an HTTP error",
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must not exceed 128000"}}`),
	)
	if failure.Retryable || failure.RouteFallback {
		t.Fatalf("failure = %#v, precise request failure must remain terminal", failure)
	}
	if failure.Code != "invalid_parameter" || failure.Provider == nil ||
		failure.Provider.Parameter != "max_tokens" {
		t.Fatalf("failure = %#v, want reviewed terminal max_tokens classification", failure)
	}
}

func TestClassifyReviewedAnthropicMaxTokensDetailRemainsTerminal(t *testing.T) {
	t.Parallel()

	classification, ok := providererror.ParseAnthropicStandard(
		`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must not exceed 128000"}}`,
	)
	if !ok {
		t.Fatal("provider parser did not recognize precise max_tokens failure")
	}
	failure := classifyProviderFailureDetail(executionFailure{
		Code:   "model_execution_failed",
		Status: http.StatusBadRequest,
	}, classification.Detail)
	if failure.Retryable || failure.RouteFallback {
		t.Fatalf("failure = %#v, reviewed max_tokens detail must remain terminal", failure)
	}
	if failure.Code != "invalid_parameter" || failure.Provider == nil ||
		failure.Provider.Parameter != "max_tokens" {
		t.Fatalf("failure = %#v, want safe precise provider metadata", failure)
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

func TestClassifyHTTPFailureRetriesAnthropicThirdPartyExtraUsageBody(t *testing.T) {
	t.Parallel()

	failure := classifyHTTPFailure(
		http.StatusBadRequest,
		nil,
		"candidate returned an HTTP error",
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Third-party apps now draw from your extra usage, not your plan limits. Add more at claude.ai/settings/usage and keep going."}}`),
	)
	if !failure.Retryable || failure.Code != "bravo_subscription_quota_exhausted" {
		t.Fatalf("failure = %#v, want retryable account quota exhaustion", failure)
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
			Message:    anthropicThirdPartyExtraUsageMessage,
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

func TestAnthropicExtraUsageCooldownIsAccountWide(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
		},
	})

	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-sonnet-5"},
		Auth:      pluginapi.HostAuthFileEntry{ID: "claude-extra-usage-exhausted", Provider: "claude"},
	}
	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    anthropicThirdPartyExtraUsageMessage,
		HTTPStatus: http.StatusBadRequest,
	})
	applyFailureCooldown(attempt, failure)

	now := time.Now()
	if !cooldownActive("claude", attempt.Auth.ID, "claude-haiku-4-5-20251001", now) {
		t.Fatal("extra-usage exhaustion must suppress the same Claude subscription across models")
	}
	if cooldownActive("claude", "another-claude-account", "claude-haiku-4-5-20251001", now) {
		t.Fatal("extra-usage exhaustion must not suppress a different Claude subscription")
	}
}

func TestNonAccountQuotaCooldownsRemainModelScoped(t *testing.T) {
	tests := []struct {
		name    string
		failure hostCallError
	}{
		{
			name: "ambiguous provider usage limit",
			failure: hostCallError{
				Code:       "model_execution_failed",
				Message:    "You have reached your usage limit for this model.",
				HTTPStatus: http.StatusBadRequest,
			},
		},
		{
			name: "rate limit",
			failure: hostCallError{
				Code:       "rate_limit_error",
				Message:    "The selected model is temporarily rate limited.",
				HTTPStatus: http.StatusTooManyRequests,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			installBravoTestConfig(t, logicalModel{
				Candidates: []candidate{
					{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
				},
			})

			attempt := executionAttempt{
				Candidate: candidate{Provider: "claude", Model: "claude-sonnet-5"},
				Auth:      pluginapi.HostAuthFileEntry{ID: "claude-model-scoped", Provider: "claude"},
			}
			failure := classifyExecutionError(&test.failure)
			applyFailureCooldown(attempt, failure)

			now := time.Now()
			if !cooldownActive("claude", attempt.Auth.ID, attempt.Candidate.Model, now) {
				t.Fatal("the physical model that failed must be cooling down")
			}
			if cooldownActive("claude", attempt.Auth.ID, "claude-haiku-4-5-20251001", now) {
				t.Fatal("a model-scoped quota or rate limit must not suppress a sibling model")
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
					Message:    anthropicThirdPartyExtraUsageMessage,
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

func TestBravoExecuteSkipsSameAccountAfterAccountWideQuotaFailure(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
			{Provider: "claude", Model: "claude-haiku-4-5-20251001", Priority: 90, Capabilities: []string{capabilityText}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 80, Capabilities: []string{capabilityText}},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 2
	currentConfig.Store(cfg)

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-extra-usage-exhausted", Name: "claude-extra-usage-exhausted.json", Provider: "claude"},
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
					Message:    anthropicThirdPartyExtraUsageMessage,
					HTTPStatus: http.StatusBadRequest,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.6-terra","content":[{"type":"text","text":"ok"}]}`),
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
		HostCallbackID: "account-wide-extra-usage-fallback",
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
		t.Fatalf("host model calls = %d, want exhausted Claude once then Codex: %#v", len(calls), calls)
	}
	if calls[0].ForcedProvider != "claude" || calls[0].Model != "claude-sonnet-5" {
		t.Fatalf("first call = %#v, want Claude Sonnet", calls[0])
	}
	if calls[1].ForcedProvider != "codex" || calls[1].Model != "gpt-5.6-terra" {
		t.Fatalf("second call = %#v, want Codex Terra", calls[1])
	}
}

func TestBravoCountSkipsSameAccountWithoutSpendingAttemptBudget(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
			{Provider: "claude", Model: "claude-haiku-4-5-20251001", Priority: 90, Capabilities: []string{capabilityText}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 80, Capabilities: []string{capabilityText}},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 2
	currentConfig.Store(cfg)

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-extra-usage-exhausted", Name: "claude-extra-usage-exhausted.json", Provider: "claude"},
		{ID: "codex-ready", Name: "codex-ready.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelCountTokens:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.ForcedProvider == "claude" {
				return nil, &hostCallError{
					Code:       "model_execution_failed",
					Message:    anthropicThirdPartyExtraUsageMessage,
					HTTPStatus: http.StatusBadRequest,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"input_tokens":42}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errCount := countTokens(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
		},
		HostCallbackID: "account-wide-count-fallback",
	}))
	if errCount != nil {
		t.Fatal(errCount)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo token count failed: %#v", env.Error)
	}
	if len(calls) != 2 || calls[0].Model != "claude-sonnet-5" ||
		calls[1].ForcedProvider != "codex" || calls[1].Model != "gpt-5.6-terra" {
		t.Fatalf("count calls = %#v, want Claude Sonnet once then Codex Terra", calls)
	}
}

func TestBravoStreamSkipsSameAccountWithoutSpendingAttemptBudget(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText, capabilityStream}},
			{Provider: "claude", Model: "claude-haiku-4-5-20251001", Priority: 90, Capabilities: []string{capabilityText, capabilityStream}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 80, Capabilities: []string{capabilityText, capabilityStream}},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 2
	currentConfig.Store(cfg)

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-extra-usage-exhausted", Name: "claude-extra-usage-exhausted.json", Provider: "claude"},
		{ID: "codex-ready", Name: "codex-ready.json", Provider: "codex"},
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
			streamID := "codex-success-stream"
			if request.ForcedProvider == "claude" {
				streamID = "claude-extra-usage-stream"
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   streamID,
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			var request pluginapi.HostModelStreamReadRequest
			decodeBravoPayload(t, payload, &request)
			if request.StreamID == "claude-extra-usage-stream" {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					ErrorDetail: &pluginapi.HostModelExecutionError{
						Code:       "model_execution_failed",
						Message:    anthropicThirdPartyExtraUsageMessage,
						HTTPStatus: http.StatusBadRequest,
					},
				}), nil
			}
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

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		},
		HostCallbackID: "account-wide-stream-fallback",
	}, "client-stream")

	if len(calls) != 2 || calls[0].Model != "claude-sonnet-5" ||
		calls[1].ForcedProvider != "codex" || calls[1].Model != "gpt-5.6-terra" {
		t.Fatalf("stream calls = %#v, want Claude Sonnet once then Codex Terra", calls)
	}
	if pluginClose.StreamID != "client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin stream close = %#v, want successful Codex close", pluginClose)
	}
}

func installHardProviderCallBudgetTest(t *testing.T) []pluginapi.HostAuthFileEntry {
	t.Helper()
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-budget-probe", Priority: 100, Capabilities: []string{capabilityText, capabilityStream}},
			{Provider: "codex", Model: "codex-budget-probe", Priority: 90, Capabilities: []string{capabilityText, capabilityStream}},
			{Provider: "gemini", Model: "gemini-budget-probe", Priority: 80, Capabilities: []string{capabilityText, capabilityStream}},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 2
	currentConfig.Store(cfg)
	return []pluginapi.HostAuthFileEntry{
		{ID: "claude-budget", Name: "claude-budget.json", Provider: "claude"},
		{ID: "codex-budget", Name: "codex-budget.json", Provider: "codex"},
		{ID: "gemini-budget", Name: "gemini-budget.json", Provider: "gemini"},
	}
}

func hardProviderCallBudgetFailure() error {
	return &hostCallError{
		Code:       "rate_limited",
		Message:    "provider-call budget probe",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

func assertHardProviderCallBudget(t *testing.T, calls []pluginapi.HostModelExecutionRequest) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("host model calls = %#v, want exactly two real provider calls", calls)
	}
	if calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("providers = %q, %q, want claude then codex with gemini blocked by max_attempts", calls[0].ForcedProvider, calls[1].ForcedProvider)
	}
}

func TestBravoExecuteMaxAttemptsCapsRealProviderCalls(t *testing.T) {
	auths := installHardProviderCallBudgetTest(t)
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecute:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			return nil, hardProviderCallBudgetFailure()
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
		},
		HostCallbackID: "hard-provider-call-budget-execute",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("response = %s, want exhausted provider-call budget failure", raw)
	}
	assertHardProviderCallBudget(t, calls)
}

func TestBravoCountMaxAttemptsCapsRealProviderCalls(t *testing.T) {
	auths := installHardProviderCallBudgetTest(t)
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelCountTokens:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			return nil, hardProviderCallBudgetFailure()
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errCount := countTokens(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}]}`),
		},
		HostCallbackID: "hard-provider-call-budget-count",
	}))
	if errCount != nil {
		t.Fatal(errCount)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("response = %s, want exhausted provider-call budget failure", raw)
	}
	assertHardProviderCallBudget(t, calls)
}

func TestBravoStreamMaxAttemptsCapsRealProviderCalls(t *testing.T) {
	auths := installHardProviderCallBudgetTest(t)
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
			return nil, hardProviderCallBudgetFailure()
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
		},
		HostCallbackID: "hard-provider-call-budget-stream",
	}, "hard-provider-call-budget-client-stream")

	assertHardProviderCallBudget(t, calls)
	if pluginClose.StreamID != "hard-provider-call-budget-client-stream" || pluginClose.Error == "" {
		t.Fatalf("plugin stream close = %#v, want provider-call budget failure", pluginClose)
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

func TestBravoExecuteFallsBackFromAmbiguousClaude400ToCodex(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText, capabilityTools}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 90, Capabilities: []string{capabilityText, capabilityTools}},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-maria", AuthIndex: "claude-maria", Name: "claude-maria.json", Provider: "claude"},
		{ID: "codex-maria", AuthIndex: "codex-maria", Name: "codex-maria.json", Provider: "codex"},
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
				detail := providererror.Detail{
					Type:    "invalid_request_error",
					Message: "The provider rejected this request.",
				}
				return nil, &hostCallError{
					Code:          "model_execution_failed",
					Message:       detail.Message,
					HTTPStatus:    http.StatusBadRequest,
					ProviderError: &detail,
				}
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"model":"gpt-5.6-terra","content":[{"type":"text","text":"ok"}]}`),
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
				"max_tokens":65536,
				"tools":[
					{"name":"memory_get","description":"d","input_schema":{"type":"object","properties":{}}},
					{"name":"memory_search","description":"d","input_schema":{"type":"object","properties":{}}}
				],
				"messages":[{"role":"user","content":"ok"}]
			}`),
		},
		HostCallbackID: "maria-ambiguous-400-fallback",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo execution failed instead of protecting Maria: %#v", env.Error)
	}
	if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("calls = %#v, want Claude Sonnet then Codex Terra", calls)
	}

	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(attempts) != 2 || attempts[0].ErrorCode != "bravo_provider_ambiguous_invalid_request" ||
		attempts[0].Retryable || !attempts[1].Success {
		t.Fatalf("attempt diagnostics = %#v", attempts)
	}
}

func TestBravoStreamFallsBackFromAmbiguousClaude400WithoutRepeatingPhysicalModel(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText, capabilityTools, capabilityStream}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 90, Capabilities: []string{capabilityText, capabilityTools, capabilityStream}},
		},
	})
	cfg := loadedConfig()
	cfg.MaxAttempts = 2
	currentConfig.Store(cfg)

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-maria-a", AuthIndex: "claude-maria-a", Name: "claude-maria-a.json", Provider: "claude"},
		{ID: "claude-maria-b", AuthIndex: "claude-maria-b", Name: "claude-maria-b.json", Provider: "claude"},
		{ID: "codex-maria", AuthIndex: "codex-maria", Name: "codex-maria.json", Provider: "codex"},
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
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   request.ForcedProvider + "-ambiguous-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			var request pluginapi.HostModelStreamReadRequest
			decodeBravoPayload(t, payload, &request)
			if strings.HasPrefix(request.StreamID, "claude-") {
				detail := providererror.Detail{
					Type:    "invalid_request_error",
					Message: "The provider rejected this request.",
				}
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
					ErrorDetail: &pluginapi.HostModelExecutionError{
						Code:          "model_execution_failed",
						Message:       detail.Message,
						HTTPStatus:    http.StatusBadRequest,
						ProviderError: &detail,
					},
				}), nil
			}
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

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","max_tokens":65536,"messages":[{"role":"user","content":"ok"}],"stream":true}`),
		},
		HostCallbackID: "maria-ambiguous-400-stream-fallback",
	}, "maria-client-stream")

	if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("stream calls = %#v, want one Claude Sonnet call then Codex Terra", calls)
	}
	if pluginClose.StreamID != "maria-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin stream close = %#v, want successful Codex close", pluginClose)
	}
}

func TestBravoCountFallsBackFromAmbiguousClaude400ResponseBody(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{Provider: "claude", Model: "claude-sonnet-5", Priority: 100, Capabilities: []string{capabilityText}},
			{Provider: "codex", Model: "gpt-5.6-terra", Priority: 90, Capabilities: []string{capabilityText}},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-maria", AuthIndex: "claude-maria", Name: "claude-maria.json", Provider: "claude"},
		{ID: "codex-maria", AuthIndex: "codex-maria", Name: "codex-maria.json", Provider: "codex"},
	}
	var calls []pluginapi.HostModelExecutionRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelCountTokens:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			calls = append(calls, request.HostModelExecutionRequest)
			if request.ForcedProvider == "claude" {
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
					StatusCode: http.StatusBadRequest,
					Body:       []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"The provider rejected this request."}}`),
				}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"input_tokens":26140}`),
			}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	raw, errCount := countTokens(mustJSONValue(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolClaude,
			SourceFormat:    protocolClaude,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","max_tokens":65536,"messages":[{"role":"user","content":"ok"}]}`),
		},
		HostCallbackID: "maria-ambiguous-400-count-fallback",
	}))
	if errCount != nil {
		t.Fatal(errCount)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("Bravo token count failed instead of using Codex: %#v", env.Error)
	}
	if len(calls) != 2 || calls[0].ForcedProvider != "claude" || calls[1].ForcedProvider != "codex" {
		t.Fatalf("count calls = %#v, want Claude Sonnet then Codex Terra", calls)
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
