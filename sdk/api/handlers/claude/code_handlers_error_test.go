package claude

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	basehandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type codedClaudeHandlerError struct {
	code    string
	message string
}

func (e codedClaudeHandlerError) Error() string     { return e.message }
func (e codedClaudeHandlerError) ErrorCode() string { return e.code }

type typedClaudeContextError struct {
	detail providererror.Detail
}

func (e typedClaudeContextError) Error() string     { return e.detail.Message }
func (e typedClaudeContextError) ErrorCode() string { return e.detail.Code }
func (e typedClaudeContextError) ProviderErrorDetail() (providererror.Detail, bool) {
	return e.detail, true
}

func TestClaudeErrorUsesTypedPromptTooLongDiagnostic(t *testing.T) {
	detail := providererror.Detail{
		Type:            "invalid_request_error",
		Code:            "context_window_exceeded",
		Message:         "Input requires 1003466 tokens and exceeds the model context limit of 1000000 tokens.",
		Scope:           providererror.ScopeRequest,
		Reason:          "prompt_too_long",
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		Class:           providererror.ClassContextWindow,
		RequiredTokens:  1003466,
		LimitTokens:     1000000,
	}
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &basehandlers.BaseAPIHandler{}}
	got := handler.toClaudeError(&interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      typedClaudeContextError{detail: detail},
	})
	if got.Error.Type != "invalid_request_error" || got.Error.Code != detail.Code || got.Error.Message != detail.Message {
		t.Fatalf("Claude error = %#v", got)
	}
}

func TestClaudeErrorPreservesExecutorErrorCode(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusUnprocessableEntity,
		Error: codedClaudeHandlerError{
			code:    "bravo_contract_unverified",
			message: "manual thinking budgets are not contract-preserving",
		},
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Code != "bravo_contract_unverified" {
		t.Fatalf("error.code = %q, want bravo_contract_unverified", got.Error.Code)
	}
	if got.Error.Message != "manual thinking budgets are not contract-preserving" {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
	if got.Error.Code != "context_too_large" {
		t.Fatalf("error.code = %q, want context_too_large", got.Error.Code)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestWriteClaudeErrorResponseCapturesMetadataDiagnostic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	base := basehandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ErrorLogCapture: sdkconfig.ErrorLogCaptureConfig{
			Mode: sdkconfig.ErrorLogCaptureModeMetadata,
		},
	}, nil)
	handler := NewClaudeCodeAPIHandler(base)
	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error: codedClaudeHandlerError{
			code:    "bravo_route_temporarily_unavailable",
			message: "provider message",
		},
	}

	handler.WriteErrorResponse(c, errMsg)

	value, ok := c.Get("API_RESPONSE_ERROR")
	if !ok {
		t.Fatal("metadata diagnostic was not captured")
	}
	errorsForLog, ok := value.([]*interfaces.ErrorMessage)
	if !ok || len(errorsForLog) != 1 || errorsForLog[0] != errMsg {
		t.Fatalf("API_RESPONSE_ERROR = %#v, want the request-scoped error", value)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := pendingClaudeStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}

// bravoStreamFailure models the terminal error a Bravo stream close produces
// when the pool is exhausted before any bytes reach the client.
type bravoStreamFailure struct {
	message    string
	status     int
	code       string
	retryAfter string
}

func (e bravoStreamFailure) Error() string           { return e.message }
func (e bravoStreamFailure) StatusCode() int         { return e.status }
func (e bravoStreamFailure) ErrorCode() string       { return e.code }
func (e bravoStreamFailure) RetryAfterValue() string { return e.retryAfter }

// The Claude endpoint carries the whole failover contract: 503, the machine
// readable code, and the backoff hint. The status and code already survived;
// the header was silently dropped because passthrough-headers is off in prod.
func TestClaudeWriteErrorResponseEmitsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	handler := &ClaudeCodeAPIHandler{}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error: bravoStreamFailure{
			message:    "bravo_no_eligible_account: Bravo has no healthy account for logical model opus",
			status:     http.StatusServiceUnavailable,
			code:       "bravo_no_eligible_account",
			retryAfter: "30",
		},
	})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != "bravo_no_eligible_account" {
		t.Fatalf("error.code = %q, want %q", got, "bravo_no_eligible_account")
	}
}

func TestClaudeWriteErrorResponseEmitsBravoTraceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &basehandlers.BaseAPIHandler{}}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error: bravoStreamFailure{
			message: "route unavailable",
			status:  http.StatusServiceUnavailable,
			code:    "bravo_route_temporarily_unavailable",
		},
		Addon: http.Header{"X-Bravo-Trace-Id": {"trc_0123456789abcdef01234567"}},
	})

	if got := recorder.Header().Get("X-Bravo-Trace-Id"); got != "trc_0123456789abcdef01234567" {
		t.Fatalf("trace id = %q", got)
	}
}

func TestClaudeWriteErrorResponseEmitsTrustedBravoTraceForProviderInvalidRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &basehandlers.BaseAPIHandler{}}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode:       http.StatusBadRequest,
		Error:            codedClaudeHandlerError{message: "invalid tool", code: "invalid_function_parameters"},
		Addon:            http.Header{"X-Bravo-Trace-Id": {"trc_0123456789abcdef01234567"}},
		ExecutorPluginID: "bravo",
	})

	if got := recorder.Header().Get("X-Bravo-Trace-Id"); got != "trc_0123456789abcdef01234567" {
		t.Fatalf("trace id = %q", got)
	}
}

func TestClaudeWriteErrorResponseRejectsProviderControlledBravoTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &basehandlers.BaseAPIHandler{}}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      codedClaudeHandlerError{message: "invalid tool", code: "invalid_function_parameters"},
		Addon:      http.Header{"X-Bravo-Trace-Id": {"trc_0123456789abcdef01234567"}},
	})

	if got := recorder.Header().Get("X-Bravo-Trace-Id"); got != "" {
		t.Fatalf("untrusted trace id escaped: %q", got)
	}
}
