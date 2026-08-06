package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type claudeStreamTypedTerminalError interface {
	error
	StatusCode() int
	ErrorCode() string
	Retryable() bool
}

type claudeStreamProviderErrorDetailCarrier interface {
	ProviderErrorDetail() (providererror.Detail, bool)
}

func TestClaudePromptTooLongParityAcrossHTTPAndSSE(t *testing.T) {
	const payload = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1003466 tokens > 1000000 maximum"},"request_id":"req_context_private"}`
	want := providererror.Detail{
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

	httpErr := claudeHTTPStatusError(http.StatusBadRequest, []byte(payload))
	sseErr := claudeProviderStreamError([]byte("data: " + payload))
	if sseErr == nil {
		t.Fatal("claudeProviderStreamError() = nil")
	}

	for name, err := range map[string]error{"http": httpErr, "sse": sseErr} {
		detail, ok := providererror.FromError(err)
		if !ok || !reflect.DeepEqual(detail, want) {
			t.Errorf("%s ProviderErrorDetail = %#v, %t; want %#v, true", name, detail, ok, want)
		}
		coded, ok := err.(interface{ ErrorCode() string })
		if !ok {
			t.Errorf("%s error %T has no ErrorCode", name, err)
		} else if got := coded.ErrorCode(); got != "context_window_exceeded" {
			t.Errorf("%s ErrorCode = %q, want context_window_exceeded", name, got)
		}
		status, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Errorf("%s error %T has no StatusCode", name, err)
		} else if got := status.StatusCode(); got != http.StatusBadRequest {
			t.Errorf("%s StatusCode = %d, want %d", name, got, http.StatusBadRequest)
		}
		if got := err.Error(); got != want.Message {
			t.Errorf("%s Error() = %q, want %q", name, got, want.Message)
		}
		for _, forbidden := range []string{"req_context_private", "request_id", "prompt is too long"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Errorf("%s error leaks %q: %v", name, forbidden, err)
			}
		}
	}

	typedSSE, ok := sseErr.(claudeStreamTypedTerminalError)
	if !ok {
		t.Fatalf("SSE error %T does not expose retryability", sseErr)
	}
	if typedSSE.Retryable() {
		t.Fatal("SSE Retryable() = true, want false")
	}
}

func TestClaudeExecutorStreamProviderErrorsAreTerminalAcrossProtocols(t *testing.T) {
	protocols := []struct {
		name    string
		format  sdktranslator.Format
		request []byte
	}{
		{
			name:   "claude",
			format: sdktranslator.FormatClaude,
			request: []byte(`{
				"model":"claude-fable-5",
				"max_tokens":32,
				"messages":[{"role":"user","content":"hi"}],
				"stream":true
			}`),
		},
		{
			name:   "openai",
			format: sdktranslator.FormatOpenAI,
			request: []byte(`{
				"model":"claude-fable-5",
				"messages":[{"role":"user","content":"hi"}],
				"stream":true
			}`),
		},
		{
			name:   "openai-response",
			format: sdktranslator.FormatOpenAIResponse,
			request: []byte(`{
				"model":"claude-fable-5",
				"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
				"stream":true
			}`),
		},
	}

	creditsDetail := providererror.Detail{
		Type:             "rate_limit_error",
		Code:             "credits_required",
		Message:          "Usage credits are required for this model.",
		Model:            "claude-fable-5",
		ModelDisplayName: "Fable 5",
		NoticeTitle:      "You've hit your monthly spend limit",
		NoticeText:       "Ask your admin to raise your spend limit, or switch models to continue this chat.",
		DisabledReason:   "org_level_disabled_until",
		Scope:            "model",
		Reason:           "monthly_spend_limit",
		TaxonomyVersion:  providererror.FailureTaxonomyV1,
		Class:            providererror.ClassQuota,
	}
	billingDetail := providererror.Detail{
		Type:            "billing_error",
		Code:            "billing_error",
		Message:         "The provider reported a billing restriction.",
		Scope:           "account",
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		Class:           providererror.ClassBilling,
	}
	overloadedDetail := providererror.Detail{
		Type:            "overloaded_error",
		Code:            "overloaded_error",
		Message:         "The provider is temporarily overloaded.",
		Scope:           "model",
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		Class:           providererror.ClassOverloaded,
	}
	contextDetail := providererror.Detail{
		Type:            "invalid_request_error",
		Code:            "context_window_exceeded",
		Message:         "Input exceeds the model context window.",
		Scope:           providererror.ScopeRequest,
		Reason:          "context_window_exceeded",
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		Class:           providererror.ClassContextWindow,
	}
	signals := []struct {
		name                    string
		event                   string
		wantStatus              int
		wantRetryable           bool
		wantCode                string
		wantErrorContains       string
		wantGenericError        bool
		wantProviderErrorDetail *providererror.Detail
		forbiddenPayload        []string
		forbiddenError          []string
	}{
		{
			name:                    "credits_required",
			event:                   `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_stream_credits_private"}`,
			wantStatus:              http.StatusTooManyRequests,
			wantRetryable:           true,
			wantCode:                "credits_required",
			wantProviderErrorDetail: &creditsDetail,
			forbiddenPayload: []string{
				"event: error",
				"credits_required",
				"usage credits are required",
				"rate_limit_error",
				"req_stream_credits_private",
				"has_chargeable_saved_payment_method",
			},
			forbiddenError: []string{
				`{"type"`,
				"req_stream_credits_private",
				"request_id",
				"has_chargeable_saved_payment_method",
				"can_user_purchase_credits",
				"payment_method",
			},
		},
		{
			name:                    "context_invalid_request",
			event:                   `{"type":"error","error":{"type":"invalid_request_error","message":"Your input exceeds the context window of this model. Please adjust your input and try again."},"request_id":"req_stream_context_private"}`,
			wantStatus:              http.StatusBadRequest,
			wantRetryable:           false,
			wantCode:                "context_window_exceeded",
			wantErrorContains:       "context window",
			wantProviderErrorDetail: &contextDetail,
			forbiddenPayload: []string{
				"event: error",
				"invalid_request_error",
				"input exceeds the context window",
				"req_stream_context_private",
			},
			forbiddenError: []string{
				`{"type"`,
				"req_stream_context_private",
				"request_id",
			},
		},
		{
			name:                    "billing_error",
			event:                   `{"type":"error","error":{"type":"billing_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_stream_billing_private"}`,
			wantStatus:              http.StatusPaymentRequired,
			wantRetryable:           true,
			wantCode:                "billing_error",
			wantErrorContains:       "billing restriction",
			wantProviderErrorDetail: &billingDetail,
			forbiddenPayload: []string{
				"event: error",
				"billing_error",
				"private diagnostic",
				"pm_private",
				"req_stream_billing_private",
			},
			forbiddenError: []string{
				`{"type"`,
				"private diagnostic",
				"pm_private",
				"req_stream_billing_private",
				"request_id",
				"payment_method",
			},
		},
		{
			name:                    "overloaded_error",
			event:                   `{"type":"error","error":{"type":"overloaded_error","message":"private diagnostic"},"request_id":"req_stream_overloaded_private"}`,
			wantStatus:              529,
			wantRetryable:           true,
			wantCode:                "overloaded_error",
			wantErrorContains:       "temporarily overloaded",
			wantProviderErrorDetail: &overloadedDetail,
			forbiddenPayload: []string{
				"event: error",
				"overloaded_error",
				"private diagnostic",
				"req_stream_overloaded_private",
			},
			forbiddenError: []string{
				`{"type"`,
				"private diagnostic",
				"req_stream_overloaded_private",
				"request_id",
			},
		},
		{
			name:             "unknown_structured_error",
			event:            `{"type":"error","error":{"type":"future_provider_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_stream_unknown_private"}`,
			wantStatus:       http.StatusBadGateway,
			wantRetryable:    false,
			wantGenericError: true,
			forbiddenPayload: []string{
				"event: error",
				"future_provider_error",
				"private diagnostic",
				"pm_private",
				"req_stream_unknown_private",
			},
			forbiddenError: []string{
				`{"type"`,
				"future_provider_error",
				"private diagnostic",
				"pm_private",
				"req_stream_unknown_private",
				"request_id",
				"payment_method",
			},
		},
	}

	for _, protocol := range protocols {
		for _, signal := range signals {
			t.Run(protocol.name+"/"+signal.name, func(t *testing.T) {
				terminalErr, payload := executeClaudeProviderErrorStream(
					t,
					protocol.format,
					protocol.request,
					signal.event,
				)

				assertClaudeProviderErrorStreamPayloadIsSafe(t, payload, signal.forbiddenPayload)
				if terminalErr == nil {
					t.Fatal("terminal stream error = nil")
				}

				typed, ok := terminalErr.(claudeStreamTypedTerminalError)
				if !ok {
					t.Fatalf("terminal stream error %T does not expose StatusCode, ErrorCode, and Retryable: %v", terminalErr, terminalErr)
				}
				if got := typed.StatusCode(); got != signal.wantStatus {
					t.Errorf("StatusCode() = %d, want %d", got, signal.wantStatus)
				}
				if got := typed.Retryable(); got != signal.wantRetryable {
					t.Errorf("Retryable() = %t, want %t", got, signal.wantRetryable)
				}
				if signal.wantCode != "" {
					if got := typed.ErrorCode(); got != signal.wantCode {
						t.Errorf("ErrorCode() = %q, want %q", got, signal.wantCode)
					}
				}

				errorText := strings.ToLower(terminalErr.Error())
				for _, forbidden := range signal.forbiddenError {
					if strings.Contains(errorText, strings.ToLower(forbidden)) {
						t.Errorf("terminal error leaks forbidden provider data %q: %v", forbidden, terminalErr)
					}
				}
				if signal.wantErrorContains != "" &&
					!strings.Contains(errorText, strings.ToLower(signal.wantErrorContains)) {
					t.Errorf("terminal error = %q, want identifiable text containing %q", terminalErr, signal.wantErrorContains)
				}
				if signal.wantGenericError &&
					!strings.Contains(errorText, "provider") &&
					!strings.Contains(errorText, "upstream") {
					t.Errorf("terminal error = %q, want a safe generic provider/upstream error", terminalErr)
				}

				if signal.wantProviderErrorDetail != nil {
					carrier, ok := terminalErr.(claudeStreamProviderErrorDetailCarrier)
					if !ok {
						t.Fatalf("terminal stream error %T does not expose ProviderErrorDetail: %v", terminalErr, terminalErr)
					}
					detail, ok := carrier.ProviderErrorDetail()
					if !ok {
						t.Fatal("ProviderErrorDetail() ok = false, want true")
					}
					detail = providererror.Sanitize(detail)
					if !reflect.DeepEqual(detail, *signal.wantProviderErrorDetail) {
						t.Errorf("ProviderErrorDetail() = %+v, want %+v", detail, *signal.wantProviderErrorDetail)
					}
					serialized, errMarshal := json.Marshal(detail)
					if errMarshal != nil {
						t.Fatalf("marshal ProviderErrorDetail: %v", errMarshal)
					}
					for _, forbidden := range []string{
						"request_id",
						"req_stream_credits_private",
						"payment_method",
						"has_chargeable_saved_payment_method",
						"can_user_purchase_credits",
					} {
						if strings.Contains(strings.ToLower(string(serialized)), forbidden) {
							t.Errorf("ProviderErrorDetail leaks forbidden provider data %q: %s", forbidden, serialized)
						}
					}
				}
			})
		}
	}
}

func TestClaudeExecutorNonStreamProviderErrorsAreTypedAndSafeAcrossTranslatedProtocols(t *testing.T) {
	protocols := []struct {
		name    string
		format  sdktranslator.Format
		request []byte
	}{
		{
			name:   "openai",
			format: sdktranslator.FormatOpenAI,
			request: []byte(`{
				"model":"claude-fable-5",
				"messages":[{"role":"user","content":"hi"}]
			}`),
		},
		{
			name:   "openai-response",
			format: sdktranslator.FormatOpenAIResponse,
			request: []byte(`{
				"model":"claude-fable-5",
				"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]
			}`),
		},
	}

	creditsDetail, ok := providererror.Parse(anthropicCreditsRequiredPayload)
	if !ok {
		t.Fatal("credits fixture is not a reviewed provider error")
	}
	billingClassification, ok := providererror.ParseAnthropicStandard(
		`{"type":"error","error":{"type":"billing_error","message":"private diagnostic"}}`,
	)
	if !ok {
		t.Fatal("billing fixture is not a documented provider error")
	}
	billingDetail := billingClassification.Detail
	signals := []struct {
		name                    string
		event                   string
		wantStatus              int
		wantRetryable           bool
		wantCode                string
		wantErrorContains       string
		wantProviderErrorDetail *providererror.Detail
		forbiddenError          []string
	}{
		{
			name:                    "credits_required",
			event:                   anthropicCreditsRequiredPayload,
			wantStatus:              http.StatusTooManyRequests,
			wantRetryable:           true,
			wantCode:                "credits_required",
			wantErrorContains:       "monthly spend limit",
			wantProviderErrorDetail: &creditsDetail,
			forbiddenError: []string{
				`{"type"`,
				"req_redacted",
				"request_id",
				"has_chargeable_saved_payment_method",
				"can_user_purchase_credits",
				"exhausted_included_allowance",
				"redirect_hint",
			},
		},
		{
			name:              "context_window",
			event:             `{"type":"error","error":{"type":"invalid_request_error","message":"Your input exceeds the context window of this model. Please adjust your input and try again."},"request_id":"req_nonstream_context_private"}`,
			wantStatus:        http.StatusBadRequest,
			wantRetryable:     false,
			wantCode:          "context_window_exceeded",
			wantErrorContains: "context window",
			wantProviderErrorDetail: &providererror.Detail{
				Type:            "invalid_request_error",
				Code:            "context_window_exceeded",
				Message:         "Input exceeds the model context window.",
				Scope:           providererror.ScopeRequest,
				Reason:          "context_window_exceeded",
				TaxonomyVersion: providererror.FailureTaxonomyV1,
				Class:           providererror.ClassContextWindow,
			},
			forbiddenError: []string{
				`{"type"`,
				"req_nonstream_context_private",
				"request_id",
			},
		},
		{
			name:                    "billing_error",
			event:                   `{"type":"error","error":{"type":"billing_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_nonstream_billing_private"}`,
			wantStatus:              http.StatusPaymentRequired,
			wantRetryable:           true,
			wantCode:                "billing_error",
			wantErrorContains:       "billing restriction",
			wantProviderErrorDetail: &billingDetail,
			forbiddenError: []string{
				`{"type"`,
				"private diagnostic",
				"payment_method",
				"pm_private",
				"req_nonstream_billing_private",
				"request_id",
			},
		},
		{
			name:          "unknown_structured_error",
			event:         `{"type":"error","error":{"type":"future_provider_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_nonstream_unknown_private"}`,
			wantStatus:    http.StatusBadGateway,
			wantRetryable: false,
			wantCode:      "provider_stream_error",
			forbiddenError: []string{
				`{"type"`,
				"future_provider_error",
				"private diagnostic",
				"payment_method",
				"pm_private",
				"req_nonstream_unknown_private",
				"request_id",
			},
		},
	}

	for _, protocol := range protocols {
		for _, signal := range signals {
			t.Run(protocol.name+"/"+signal.name, func(t *testing.T) {
				err := executeClaudeNonStreamSSEFixture(
					t,
					protocol.format,
					protocol.request,
					"event: error\n"+"data: "+signal.event+"\n\n",
				)
				if err == nil {
					t.Fatal("Execute() error = nil, want terminal upstream SSE error")
				}
				typed, okTyped := err.(claudeStreamTypedTerminalError)
				if !okTyped {
					t.Fatalf("Execute() error %T does not expose typed stream status: %v", err, err)
				}
				if got := typed.StatusCode(); got != signal.wantStatus {
					t.Errorf("StatusCode() = %d, want %d", got, signal.wantStatus)
				}
				if got := typed.Retryable(); got != signal.wantRetryable {
					t.Errorf("Retryable() = %t, want %t", got, signal.wantRetryable)
				}
				if got := typed.ErrorCode(); got != signal.wantCode {
					t.Errorf("ErrorCode() = %q, want %q", got, signal.wantCode)
				}
				if signal.wantErrorContains != "" &&
					!strings.Contains(strings.ToLower(err.Error()), signal.wantErrorContains) {
					t.Errorf("Execute() error = %q, want text containing %q", err, signal.wantErrorContains)
				}
				for _, forbidden := range signal.forbiddenError {
					if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(forbidden)) {
						t.Errorf("Execute() error leaks provider data %q: %v", forbidden, err)
					}
				}

				detail, hasDetail := providererror.FromError(err)
				if signal.wantProviderErrorDetail == nil {
					if hasDetail {
						t.Errorf("unexpected ProviderErrorDetail = %+v", detail)
					}
					return
				}
				if !hasDetail || !reflect.DeepEqual(detail, *signal.wantProviderErrorDetail) {
					t.Errorf(
						"ProviderErrorDetail = %+v, %t; want %+v, true",
						detail,
						hasDetail,
						*signal.wantProviderErrorDetail,
					)
				}
			})
		}
	}
}

func TestClaudeExecutorIncompleteStreamIsTerminalAcrossProtocols(t *testing.T) {
	protocols := []struct {
		name    string
		format  sdktranslator.Format
		request []byte
	}{
		{
			name:    "claude",
			format:  sdktranslator.FormatClaude,
			request: []byte(`{"model":"claude-fable-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		},
		{
			name:    "openai",
			format:  sdktranslator.FormatOpenAI,
			request: []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		},
		{
			name:    "openai-response",
			format:  sdktranslator.FormatOpenAIResponse,
			request: []byte(`{"model":"claude-fable-5","input":"hi","stream":true}`),
		},
	}
	messageStart := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_incomplete","type":"message","role":"assistant","model":"claude-fable-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` +
		"\n\n"
	content := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial-visible"}}` +
		"\n\n"

	for _, protocol := range protocols {
		for _, phase := range []struct {
			name     string
			upstream string
		}{
			{name: "before-content", upstream: messageStart},
			{name: "after-content", upstream: messageStart + content},
		} {
			t.Run(protocol.name+"/"+phase.name, func(t *testing.T) {
				terminalErr, _ := executeClaudeStreamFixture(
					t,
					protocol.format,
					protocol.request,
					phase.upstream,
				)
				typed, ok := terminalErr.(claudeStreamTypedTerminalError)
				if !ok {
					t.Fatalf("terminal stream error = %T %v, want typed incomplete error", terminalErr, terminalErr)
				}
				if typed.StatusCode() != http.StatusBadGateway ||
					typed.ErrorCode() != "provider_stream_incomplete" ||
					!typed.Retryable() {
					t.Fatalf("terminal stream error = %#v, want retryable provider_stream_incomplete/502", typed)
				}
				if strings.Contains(strings.ToLower(typed.Error()), "msg_incomplete") ||
					!strings.Contains(strings.ToLower(typed.Error()), "incomplete") {
					t.Fatalf("terminal stream error is unsafe or unclear: %q", typed.Error())
				}
			})
		}
	}
}

func TestClaudeExecutorCleanEOFBeforeMessageStartIsTerminalAcrossProtocols(t *testing.T) {
	protocols := []struct {
		name    string
		format  sdktranslator.Format
		request []byte
	}{
		{
			name:    "claude",
			format:  sdktranslator.FormatClaude,
			request: []byte(`{"model":"claude-fable-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		},
		{
			name:    "openai",
			format:  sdktranslator.FormatOpenAI,
			request: []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		},
		{
			name:    "openai-response",
			format:  sdktranslator.FormatOpenAIResponse,
			request: []byte(`{"model":"claude-fable-5","input":"hi","stream":true}`),
		},
	}
	phases := []struct {
		name     string
		upstream string
	}{
		{name: "empty"},
		{
			name: "ping-only",
			upstream: "event: ping\n" +
				`data: {"type":"ping"}` +
				"\n\n",
		},
	}

	for _, protocol := range protocols {
		for _, phase := range phases {
			t.Run(protocol.name+"/"+phase.name, func(t *testing.T) {
				terminalErr, payload := executeClaudeStreamFixture(
					t,
					protocol.format,
					protocol.request,
					phase.upstream,
				)
				typed, ok := terminalErr.(claudeStreamTypedTerminalError)
				if !ok {
					t.Fatalf("terminal stream error = %T %v, want typed incomplete error", terminalErr, terminalErr)
				}
				if typed.StatusCode() != http.StatusBadGateway ||
					typed.ErrorCode() != "provider_stream_incomplete" ||
					!typed.Retryable() {
					t.Fatalf("terminal stream error = %#v, want retryable provider_stream_incomplete/502", typed)
				}
				if !strings.Contains(strings.ToLower(typed.Error()), "incomplete") {
					t.Errorf("terminal stream error = %q, want safe incomplete diagnostic", typed.Error())
				}
				if strings.Contains(payload, "message_start") {
					t.Errorf("pre-start EOF emitted an assistant start: %s", payload)
				}
			})
		}
	}
}

func executeClaudeNonStreamSSEFixture(
	t *testing.T,
	format sdktranslator.Format,
	request []byte,
	upstreamStream string,
) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamStream))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-fable-5",
		Payload: request,
	}, cliproxyexecutor.Options{
		SourceFormat:    format,
		ResponseFormat:  format,
		OriginalRequest: request,
	})
	return err
}

func executeClaudeProviderErrorStream(
	t *testing.T,
	format sdktranslator.Format,
	request []byte,
	providerErrorEvent string,
) (error, string) {
	t.Helper()

	messageStart := `{"type":"message_start","message":{"id":"msg_stream_error","type":"message","role":"assistant","model":"claude-fable-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":0}}}`
	upstreamStream := "event: message_start\n" +
		"data: " + messageStart + "\n\n" +
		"event: error\n" +
		"data: " + providerErrorEvent + "\n\n"
	return executeClaudeStreamFixture(t, format, request, upstreamStream)
}

func executeClaudeStreamFixture(
	t *testing.T,
	format sdktranslator.Format,
	request []byte,
	upstreamStream string,
) (error, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamStream))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-fable-5",
		Payload: request,
	}, cliproxyexecutor.Options{
		SourceFormat:    format,
		ResponseFormat:  format,
		OriginalRequest: request,
		Stream:          true,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() setup error = %v", errExecute)
	}

	var terminalErr error
	var payload strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			if terminalErr != nil {
				t.Errorf("multiple terminal stream errors: first=%v second=%v", terminalErr, chunk.Err)
			}
			terminalErr = chunk.Err
			continue
		}
		payload.Write(chunk.Payload)
	}
	return terminalErr, payload.String()
}

func assertClaudeProviderErrorStreamPayloadIsSafe(t *testing.T, payload string, forbidden []string) {
	t.Helper()
	lowerPayload := strings.ToLower(payload)
	for _, value := range forbidden {
		if strings.Contains(lowerPayload, strings.ToLower(value)) {
			t.Errorf("stream payload leaks provider error data %q: %s", value, payload)
		}
	}
}
