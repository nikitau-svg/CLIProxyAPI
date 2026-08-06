package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestClientExecutionFailuresAreActionableInRussian(t *testing.T) {
	tests := []struct {
		code   string
		status int
		want   string
	}{
		{"bravo_subscription_quota_exhausted", http.StatusTooManyRequests, "лимита провайдера"},
		{"bravo_subscription_model_credits_exhausted", http.StatusTooManyRequests, "лимита расходов"},
		{"bravo_subscription_auth_unavailable", http.StatusUnauthorized, "Авторизация"},
		{"bravo_subscription_access_denied", http.StatusForbidden, "запретил доступ"},
		{"bravo_context_window_exceeded", http.StatusBadRequest, "/compact"},
		{"bravo_route_temporarily_unavailable", http.StatusServiceUnavailable, "Retry-After"},
		{"bravo_request_invalid", http.StatusBadRequest, "формат запроса"},
		{"bravo_provider_stream_error", http.StatusBadGateway, "структурированной ошибкой"},
		{"unknown_failure", http.StatusBadGateway, "Маршрут временно недоступен"},
	}
	for _, testCase := range tests {
		t.Run(testCase.code, func(t *testing.T) {
			failure := clientExecutionFailureRU(executionFailure{
				Code:    testCase.code,
				Message: `{"error":{"request_id":"private"}}`,
				Status:  testCase.status,
			})
			if !containsCyrillic(failure.Message) || !strings.Contains(failure.Message, testCase.want) {
				t.Fatalf("localized failure = %#v, want Russian text containing %q", failure, testCase.want)
			}
			if strings.Contains(failure.Message, "private") || strings.Contains(failure.Message, "request_id") {
				t.Fatalf("localized failure leaked provider diagnostic: %q", failure.Message)
			}
		})
	}
}

func TestAllocatorFallbackThenContextFailureExplainsWholeRouteInRussian(t *testing.T) {
	failure := finalExecutionFailure([]executionFailureTrace{
		{
			Provider: "claude",
			Model:    "claude-fable-5",
			Failure: executionFailure{
				Code:          "bravo_allocator_reserve_floor",
				Message:       "internal floor",
				Status:        http.StatusServiceUnavailable,
				Retryable:     true,
				RouteFallback: true,
			},
		},
		{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
			Failure: executionFailure{
				Code:    "bravo_context_window_exceeded",
				Message: "context window",
				Status:  http.StatusBadRequest,
			},
		},
	}, executionFailure{
		Code:    "bravo_context_window_exceeded",
		Message: "context window",
		Status:  http.StatusBadRequest,
	})

	for _, want := range []string{
		"Подписки Claude",
		"внутренних резервных порогов CLIProxyAPI",
		"перенаправлен в Sol",
		"не может вместить весь контекст",
		"/compact",
		"новую сессию",
	} {
		if !strings.Contains(failure.Message, want) {
			t.Errorf("route message = %q, missing %q", failure.Message, want)
		}
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(failureEnvelope(failure), &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.Error == nil || env.Error.Code != "bravo_context_window_exceeded" || env.Error.Message != failure.Message {
		t.Fatalf("localized envelope = %#v, want stable code and actionable route message", env.Error)
	}
}

func TestCompactBypassWarningIsRussianAndMachineReadable(t *testing.T) {
	headers := make(http.Header)
	metadata := make(map[string]any)
	compactBypassResponseWarning(headers, metadata, executionAttempt{CompactBypass: true})
	if headers.Get("X-Bravo-Warning-Code") != "compact-bypass-consumed-claude-reserve" {
		t.Fatalf("warning code header = %q", headers.Get("X-Bravo-Warning-Code"))
	}
	if !containsCyrillic(headers.Get("X-Bravo-Warning")) || metadata["bravo_compact_bypass"] != true {
		t.Fatalf("warning headers/metadata = %#v %#v", headers, metadata)
	}
}

func TestManagementFailuresAreActionableInRussian(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"bravo_project_name_exists", "уже существует"},
		{"bravo_project_prompt_cache_invalid", "auto, 5m или 1h"},
		{"bravo_route_invalid", "Маршрут Bravo"},
		{"bravo_allowed_auth_not_found", "Пул проекта закрыт"},
		{"bravo_primary_auth_outside_allowed_pool", "разрешённый пул"},
	}
	for _, testCase := range tests {
		t.Run(testCase.code, func(t *testing.T) {
			failure := clientProjectFailureRU(projectFailure{
				Code:    testCase.code,
				Message: "original English diagnostic",
				Status:  http.StatusBadRequest,
			})
			if !containsCyrillic(failure.Message) || !strings.Contains(failure.Message, testCase.want) {
				t.Fatalf("localized management failure = %#v, want Russian text containing %q", failure, testCase.want)
			}
			if failure.Code != testCase.code || failure.Status != http.StatusBadRequest {
				t.Fatalf("localization changed machine contract: %#v", failure)
			}
		})
	}
}
