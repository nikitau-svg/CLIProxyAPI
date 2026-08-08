package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const anthropicCreditsRequiredPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_redacted"}`

func TestStatusErrExtractsStructuredProviderErrorCode(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "raw body", message: anthropicCreditsRequiredPayload, want: "credits_required"},
		{name: "status-prefixed body", message: "429: " + anthropicCreditsRequiredPayload, want: "credits_required"},
		{name: "unstructured body", message: "rate limited", want: ""},
		{name: "malformed body", message: `{"type":"error"`, want: ""},
		{name: "internal code injection", message: `{"type":"error","error":{"type":"rate_limit_error","details":{"error_code":"request_scoped"}}}`, want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errStatus := statusErr{code: 429, msg: test.message}
			coded, ok := any(errStatus).(interface{ ErrorCode() string })
			if !ok {
				t.Fatal("statusErr does not expose a structured provider error code")
			}
			if got := coded.ErrorCode(); got != test.want {
				t.Fatalf("ErrorCode() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusErrCreditsRequiredIsSafeStructuredProviderError(t *testing.T) {
	errStatus := statusErr{
		code: http.StatusTooManyRequests,
		msg:  anthropicCreditsRequiredPayload,
	}

	want, ok := providererror.Parse(anthropicCreditsRequiredPayload)
	if !ok {
		t.Fatal("test fixture is not a reviewed provider error")
	}
	if got := errStatus.Error(); got != want.Summary() {
		t.Errorf("Error() = %q, want safe summary %q", got, want.Summary())
	}
	for _, forbidden := range []string{
		"req_redacted",
		"request_id",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		"exhausted_included_allowance",
		`"cta"`,
		"switch_model",
		"redirect_hint",
		`{"type":"error"`,
	} {
		if strings.Contains(strings.ToLower(errStatus.Error()), strings.ToLower(forbidden)) {
			t.Errorf("Error() leaks forbidden provider field %q: %s", forbidden, errStatus.Error())
		}
	}

	got, ok := providererror.FromError(errStatus)
	if !ok {
		t.Fatal("statusErr does not expose ProviderErrorDetail")
	}
	if got != want {
		t.Fatalf("ProviderErrorDetail = %#v, want %#v", got, want)
	}
}

func TestStatusErrCarriesSafeCodexServerError(t *testing.T) {
	errStatus := newCodexStatusErr(
		http.StatusBadGateway,
		[]byte(`{"error":{"type":"server_error","code":"server_error","message":"private diagnostic request_id=req_private","param":null}}`),
	)

	if got := errStatus.ErrorCode(); got != "server_error" {
		t.Fatalf("ErrorCode() = %q, want server_error", got)
	}
	detail, ok := providererror.FromError(errStatus)
	if !ok {
		t.Fatal("statusErr does not expose safe Codex server detail")
	}
	if detail.Type != "server_error" ||
		detail.Code != "server_error" ||
		detail.Scope != providererror.ScopeModel ||
		detail.Class != providererror.ClassProviderInternal ||
		detail.TaxonomyVersion != providererror.FailureTaxonomyV1 ||
		detail.Message != "The provider encountered an internal error." {
		t.Fatalf("ProviderErrorDetail = %#v, want safe model-scoped server_error", detail)
	}
	for _, forbidden := range []string{
		"private diagnostic",
		"request_id",
		"req_private",
		`{"error"`,
	} {
		if strings.Contains(strings.ToLower(errStatus.Error()), forbidden) {
			t.Fatalf("Error() leaks %q: %s", forbidden, errStatus.Error())
		}
	}
}

func TestStatusErrCarriesSafeCodexInvalidToolParameter(t *testing.T) {
	errStatus := newCodexStatusErr(
		http.StatusBadRequest,
		[]byte(`{"error":{"type":"invalid_request_error","code":"invalid_tool_parameters","param":"tools[7].function.parameters","message":"echoed schema says context window and request_id=req_private"},"request_id":"req_private"}`),
	)

	detail, ok := providererror.FromError(errStatus)
	if !ok {
		t.Fatal("statusErr does not expose safe Codex invalid-tool detail")
	}
	if detail.Type != "invalid_request_error" ||
		detail.Code != "invalid_tool_parameters" ||
		detail.Parameter != "tools[7].function.parameters" ||
		detail.Scope != providererror.ScopeRequest ||
		detail.Class != providererror.ClassInvalidRequest {
		t.Fatalf("ProviderErrorDetail = %#v", detail)
	}
	for _, forbidden := range []string{"echoed schema", "context window and request_id", "req_private"} {
		if strings.Contains(strings.ToLower(errStatus.Error()), strings.ToLower(forbidden)) {
			t.Fatalf("Error() leaks %q: %s", forbidden, errStatus.Error())
		}
	}
}

func TestClaudeHTTPStatusCarriesSafeRetryableStandardErrors(t *testing.T) {
	tests := []struct {
		name       string
		errorType  string
		httpStatus int
		wantScope  string
	}{
		{
			name:       "billing",
			errorType:  "billing_error",
			httpStatus: http.StatusPaymentRequired,
			wantScope:  "account",
		},
		{
			name:       "overloaded",
			errorType:  "overloaded_error",
			httpStatus: 529,
			wantScope:  "model",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"type":"error","error":{"type":"` + test.errorType +
				`","message":"private diagnostic payment_method=pm_private"},"request_id":"req_private"}`)
			errStatus := claudeHTTPStatusError(test.httpStatus, body)
			if errStatus.StatusCode() != test.httpStatus ||
				errStatus.ErrorCode() != test.errorType {
				t.Fatalf("status error = %#v, want status=%d code=%s",
					errStatus, test.httpStatus, test.errorType)
			}
			detail, ok := providererror.FromError(errStatus)
			if !ok ||
				detail.Type != test.errorType ||
				detail.Code != test.errorType ||
				detail.Scope != test.wantScope {
				t.Fatalf("ProviderErrorDetail = %#v, %t; want %s/%s",
					detail, ok, test.errorType, test.wantScope)
			}
			for _, forbidden := range []string{
				"private diagnostic",
				"payment_method",
				"pm_private",
				"request_id",
				"req_private",
			} {
				if strings.Contains(strings.ToLower(errStatus.Error()), forbidden) {
					t.Fatalf("Error() leaks %q: %s", forbidden, errStatus.Error())
				}
			}
		})
	}
}

func TestClaudeHTTPStatusCreditsRequiredIsSafeAcrossEntryPoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(anthropicCreditsRequiredPayload))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "claude-fable-5",
		Payload: []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
	}
	options := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		ResponseFormat:  sdktranslator.FromString("claude"),
		OriginalRequest: request.Payload,
	}
	tests := []struct {
		name   string
		invoke func() error
	}{
		{
			name: "execute",
			invoke: func() error {
				_, err := executor.Execute(context.Background(), auth, request, options)
				return err
			},
		},
		{
			name: "execute stream bootstrap",
			invoke: func() error {
				streamOptions := options
				streamOptions.Stream = true
				_, err := executor.ExecuteStream(context.Background(), auth, request, streamOptions)
				return err
			},
		},
		{
			name: "count tokens",
			invoke: func() error {
				_, err := executor.CountTokens(context.Background(), auth, request, options)
				return err
			},
		},
	}

	want, ok := providererror.Parse(anthropicCreditsRequiredPayload)
	if !ok {
		t.Fatal("test fixture is not a reviewed provider error")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.invoke()
			if err == nil {
				t.Fatal("entry point returned nil error")
			}
			if got := err.Error(); got != want.Summary() {
				t.Errorf("Error() = %q, want safe summary %q", got, want.Summary())
			}
			if status, okStatus := err.(interface{ StatusCode() int }); !okStatus ||
				status.StatusCode() != http.StatusTooManyRequests {
				t.Errorf("StatusCode() = %#v, want 429", status)
			}
			got, okDetail := providererror.FromError(err)
			if !okDetail || got != want {
				t.Errorf("ProviderErrorDetail = %#v, %t; want %#v, true", got, okDetail, want)
			}
			for _, forbidden := range []string{
				"req_redacted",
				"request_id",
				"has_chargeable_saved_payment_method",
				"can_user_purchase_credits",
				"exhausted_included_allowance",
				`"cta"`,
				"switch_model",
				"redirect_hint",
				`{"type":"error"`,
			} {
				if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(forbidden)) {
					t.Errorf("Error() leaks forbidden provider field %q: %s", forbidden, err)
				}
			}
		})
	}
}
