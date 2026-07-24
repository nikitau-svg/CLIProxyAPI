package auth

import (
	"net/http"
	"testing"
)

type codedResultTestError struct {
	code      string
	message   string
	status    int
	retryable bool
}

func (e codedResultTestError) Error() string     { return e.message }
func (e codedResultTestError) ErrorCode() string { return e.code }
func (e codedResultTestError) StatusCode() int   { return e.status }
func (e codedResultTestError) Retryable() bool   { return e.retryable }

type explicitlyScopedCodedResultTestError struct {
	codedResultTestError
}

func (explicitlyScopedCodedResultTestError) IsRequestScoped() bool { return true }

func TestResultErrorFromErrorPreservesPluginCodeForRequestScoped422(t *testing.T) {
	source := codedResultTestError{
		code:      "bravo_effort_invalid",
		message:   "reasoning_effort has unsupported effort",
		status:    http.StatusUnprocessableEntity,
		retryable: false,
	}

	got := resultErrorFromError(source)
	if got == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if got.Code != source.code {
		t.Fatalf("Code = %q, want %q", got.Code, source.code)
	}
	if got.Message != source.message {
		t.Fatalf("Message = %q, want %q", got.Message, source.message)
	}
	if got.HTTPStatus != source.status {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, source.status)
	}
	if got.Retryable {
		t.Fatal("Retryable = true, want false")
	}
	if !isRequestScopedResultError(got) {
		t.Fatal("422 plugin error must remain request-scoped")
	}
	if !got.IsRequestScoped() {
		t.Fatal("public Error.IsRequestScoped must preserve request-scoped semantics")
	}
}

func TestResultErrorFromErrorKeepsLegacyMarkerForUncoded422(t *testing.T) {
	source := codedResultTestError{
		message: "unprocessable request",
		status:  http.StatusUnprocessableEntity,
	}

	got := resultErrorFromError(source)
	if got == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if got.Code != requestScopedErrorCode {
		t.Fatalf("Code = %q, want %q", got.Code, requestScopedErrorCode)
	}
	if !isRequestScopedResultError(got) {
		t.Fatal("uncoded 422 error must remain request-scoped")
	}
}

func TestResultErrorFromErrorKeepsExplicitScopeWhenCodeCannotEncodeBoth(t *testing.T) {
	source := explicitlyScopedCodedResultTestError{
		codedResultTestError: codedResultTestError{
			code:    "stream_incomplete",
			message: "upstream closed before completion",
			status:  http.StatusRequestTimeout,
		},
	}

	got := resultErrorFromError(source)
	if got == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if got.Code != requestScopedErrorCode {
		t.Fatalf("Code = %q, want legacy request-scope marker %q", got.Code, requestScopedErrorCode)
	}
	if !got.IsRequestScoped() {
		t.Fatal("explicitly request-scoped error must remain request-scoped")
	}
}

func TestModelSupport422IsNotMarkedRequestScoped(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
	}{
		{
			name:    "message",
			code:    "upstream_rejected",
			message: "requested model is not supported",
		},
		{
			name:    "code",
			code:    "model_not_supported",
			message: "candidate rejected the request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := codedResultTestError{
				code:    tt.code,
				message: tt.message,
				status:  http.StatusUnprocessableEntity,
			}

			got := resultErrorFromError(source)
			if got == nil {
				t.Fatal("resultErrorFromError returned nil")
			}
			if got.Code != source.code {
				t.Fatalf("Code = %q, want %q", got.Code, source.code)
			}
			if isRequestScopedResultError(got) {
				t.Fatal("model-support failure must remain eligible for fallback")
			}
			if got.IsRequestScoped() {
				t.Fatal("public Error.IsRequestScoped must keep model-support failure eligible for fallback")
			}
		})
	}
}
