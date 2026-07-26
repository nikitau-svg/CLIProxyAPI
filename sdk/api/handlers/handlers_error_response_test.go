package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type codedHandlerResponseError struct {
	code    string
	message string
}

func (e codedHandlerResponseError) Error() string     { return e.message }
func (e codedHandlerResponseError) ErrorCode() string { return e.code }

func TestWriteErrorResponsePreservesExecutorErrorCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusUnprocessableEntity,
		Error: codedHandlerResponseError{
			code:    "bravo_effort_invalid",
			message: `reasoning_effort has unsupported effort "turbo"`,
		},
	})

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnprocessableEntity)
	}
	var response ErrorResponse
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.Error.Code != "bravo_effort_invalid" {
		t.Fatalf("error.code = %q, want bravo_effort_invalid; body=%s", response.Error.Code, recorder.Body.Bytes())
	}
	if response.Error.Message != `reasoning_effort has unsupported effort "turbo"` {
		t.Fatalf("error.message = %q", response.Error.Message)
	}
}

func TestWriteErrorResponseKeepsLegacyStatusCodesForGenericAuthErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		sourceCode string
		wantCode   string
	}{
		{
			name:       "unauthorized",
			status:     http.StatusUnauthorized,
			sourceCode: "unauthorized",
			wantCode:   "invalid_api_key",
		},
		{
			name:       "rate limited",
			status:     http.StatusTooManyRequests,
			sourceCode: "rate_limited",
			wantCode:   "rate_limit_exceeded",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			handler := NewBaseAPIHandlers(nil, nil)
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: testCase.status,
				Error: &coreauth.Error{
					Code:       testCase.sourceCode,
					Message:    "generic auth failure",
					HTTPStatus: testCase.status,
				},
			})

			var response ErrorResponse
			if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if response.Error.Code != testCase.wantCode {
				t.Fatalf("error.code = %q, want %q; body=%s", response.Error.Code, testCase.wantCode, recorder.Body.Bytes())
			}
		})
	}
}

func TestPreservedErrorCodeDoesNotExposeRequestScopeMarker(t *testing.T) {
	got := PreservedErrorCode(http.StatusUnprocessableEntity, codedHandlerResponseError{
		code:    "request_scoped",
		message: "unprocessable request",
	})
	if got != "" {
		t.Fatalf("PreservedErrorCode() = %q, want empty code", got)
	}
}

func TestWriteErrorResponse_AddonHeadersDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"req-1"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After should be empty when passthrough is disabled, got %q", got)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id should be empty when passthrough is disabled, got %q", got)
	}
}

func TestWriteErrorResponse_AddonHeadersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Writer.Header().Set("X-Request-Id", "old-value")
	c.Writer.Header().Set("x-cpa-trace-id", "local-trace")
	c.Writer.Header().Set("Access-Control-Expose-Headers", "x-cpa-trace-id")

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":                   {"30"},
			"X-Request-Id":                  {"new-1", "new-2"},
			"x-cpa-trace-id":                {"upstream-trace"},
			"Access-Control-Expose-Headers": {"upstream-header"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := recorder.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"new-1", "new-2"}) {
		t.Fatalf("X-Request-Id = %#v, want %#v", got, []string{"new-1", "new-2"})
	}
	if got := recorder.Header().Get("x-cpa-trace-id"); got != "local-trace" {
		t.Fatalf("x-cpa-trace-id = %q, want local trace", got)
	}
	if got := recorder.Header().Get("Access-Control-Expose-Headers"); got != "x-cpa-trace-id" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want CPA value", got)
	}
}

func TestEnrichAuthSelectionError_DefaultsTo503WithContext(t *testing.T) {
	in := &coreauth.Error{Code: "auth_not_found", Message: "no auth available"}
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusServiceUnavailable)
	}
	if !strings.Contains(got.Message, "providers=claude") {
		t.Fatalf("message missing provider context: %q", got.Message)
	}
	if !strings.Contains(got.Message, "model=claude-sonnet-4-6") {
		t.Fatalf("message missing model context: %q", got.Message)
	}
	if !strings.Contains(got.Message, "/v0/management/auth-files") {
		t.Fatalf("message missing management hint: %q", got.Message)
	}
}

func TestEnrichAuthSelectionError_PreservesExplicitStatus(t *testing.T) {
	in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusTooManyRequests}
	out := enrichAuthSelectionError(in, []string{"gemini"}, "gemini-2.5-pro")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestEnrichAuthSelectionError_IgnoresOtherErrors(t *testing.T) {
	in := errors.New("boom")
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")
	if out != in {
		t.Fatalf("expected original error to be returned unchanged")
	}
}

// retryHintedError models a proxy-authored failure: it computed a backoff hint
// itself without ever seeing an upstream HTTP response to copy a header from.
type retryHintedError struct {
	message    string
	status     int
	retryAfter string
}

func (e retryHintedError) Error() string           { return e.message }
func (e retryHintedError) StatusCode() int         { return e.status }
func (e retryHintedError) RetryAfterValue() string { return e.retryAfter }

// A proxy-authored backoff hint must reach the client even with
// passthrough-headers off. That switch governs forwarding upstream headers
// verbatim; Retry-After here is the proxy's own RFC 9110 contract. Without it a
// 503 reads as permanent and SDK clients retry straight back into the pool that
// just asked them to wait.
func TestWriteErrorResponse_RetryAfterIgnoresPassthroughSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error: retryHintedError{
			message:    "bravo_no_eligible_account: no healthy account",
			status:     http.StatusServiceUnavailable,
			retryAfter: "30",
		},
	})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
}

// An upstream Retry-After stays gated behind passthrough-headers: it is copied
// upstream data, not something the proxy authored.
func TestWriteErrorResponse_UpstreamRetryAfterStaysGated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon:      http.Header{"Retry-After": {"120"}},
	})

	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("upstream Retry-After leaked with passthrough off: %q", got)
	}
}

// A duration-shaped hint must be rendered as whole seconds, rounded up so the
// client never comes back a fraction of a second early.
func TestRetryAfterHintFromErrorRoundsDurationsUp(t *testing.T) {
	if got := retryAfterHintFromError(retryHintedError{retryAfter: "45"}); got != "45" {
		t.Fatalf("verbatim hint = %q, want %q", got, "45")
	}
	if got := retryAfterHintFromError(errors.New("boom")); got != "" {
		t.Fatalf("plain error produced a hint: %q", got)
	}
}

// The addon must carry only real upstream headers. Synthesising a Retry-After
// into it would smuggle a proxy-authored value through the passthrough gate.
func TestErrorResponseAddonCarriesOnlyUpstreamHeaders(t *testing.T) {
	if addon := ErrorResponseAddon(retryHintedError{retryAfter: "30"}); addon != nil {
		t.Fatalf("addon = %v, want nil for an error with no upstream headers", addon)
	}
	if addon := ErrorResponseAddon(nil); addon != nil {
		t.Fatalf("addon = %v, want nil for a nil error", addon)
	}
}
