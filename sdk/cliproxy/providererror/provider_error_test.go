package providererror

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const creditsRequiredPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_must_not_be_retained"}`

const anthropicPromptTooLongPayload = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1003466 tokens > 1000000 maximum"},"request_id":"req_context_private"}`

func TestParseAnthropicPromptTooLongReturnsSafeTypedContextFailure(t *testing.T) {
	t.Parallel()

	classification, ok := ParseAnthropicStandard(anthropicPromptTooLongPayload)
	if !ok {
		t.Fatal("ParseAnthropicStandard() did not recognize prompt-too-long")
	}
	want := Classification{
		Detail: Detail{
			Type:            "invalid_request_error",
			Code:            "context_window_exceeded",
			Message:         "Input requires 1003466 tokens and exceeds the model context limit of 1000000 tokens.",
			Scope:           ScopeRequest,
			Reason:          "prompt_too_long",
			TaxonomyVersion: FailureTaxonomyV1,
			Class:           ClassContextWindow,
			RequiredTokens:  1003466,
			LimitTokens:     1000000,
		},
		Status:    400,
		Retryable: false,
	}
	if classification != want {
		t.Fatalf("classification = %#v, want %#v", classification, want)
	}

	serialized, err := json.Marshal(classification)
	if err != nil {
		t.Fatalf("marshal classification: %v", err)
	}
	for _, forbidden := range []string{"req_context_private", "request_id", "prompt is too long"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("classification retained forbidden provider text %q: %s", forbidden, serialized)
		}
	}
}

func TestParseAnthropicPromptTooLongFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []string{
		"prompt is too long: 1000000 tokens > 1000000 maximum",
		"prompt is too long: 999999 tokens > 1000000 maximum",
		"prompt is too long: 0 tokens > 1000000 maximum",
		"prompt is too long: 1000000000001 tokens > 1 maximum",
		"prompt is too long: 1003466 tokens > 1000000 maximum request_id=req_private",
	}
	for _, message := range tests {
		payload := `{"type":"error","error":{"type":"invalid_request_error","message":` + string(mustJSON(t, message)) + `}}`
		classification, ok := ParseAnthropicStandard(payload)
		if !ok {
			t.Fatalf("generic invalid_request_error was not recognized for %q", message)
		}
		if classification.Detail.Class == ClassContextWindow ||
			classification.Detail.Code == "context_window_exceeded" ||
			classification.Detail.RequiredTokens != 0 ||
			classification.Detail.LimitTokens != 0 {
			t.Fatalf("unsafe prompt-too-long variant was classified as context: %#v", classification)
		}
	}
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseCreditsRequired(t *testing.T) {
	t.Parallel()

	detail, ok := Parse(creditsRequiredPayload)
	if !ok {
		t.Fatal("Parse() did not recognize the provider error")
	}

	want := Detail{
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
		TaxonomyVersion:  FailureTaxonomyV1,
		Class:            ClassQuota,
	}
	if detail != want {
		t.Fatalf("Parse() = %#v, want %#v", detail, want)
	}
	if got := detail.Summary(); got != "Fable 5: You've hit your monthly spend limit" {
		t.Fatalf("Summary() = %q", got)
	}

	serialized, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	for _, forbidden := range []string{
		"req_must_not_be_retained",
		"can_user_purchase_credits",
		"has_chargeable_saved_payment_method",
		"exhausted_included_allowance",
		"switch_model",
	} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("structured detail retained forbidden provider field %q: %s", forbidden, serialized)
		}
	}
}

func TestParseAcceptsStatusCodePrefix(t *testing.T) {
	t.Parallel()

	detail, ok := Parse("status code 429: " + creditsRequiredPayload)
	if !ok {
		t.Fatal("Parse() did not recognize a code-prefixed provider error")
	}
	if detail.Code != "credits_required" {
		t.Fatalf("Code = %q, want credits_required", detail.Code)
	}
}

func TestParseAnthropicStandardClassifiesSafeRoutingMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		errorType     string
		wantStatus    int
		wantRetryable bool
		wantScope     string
		wantClass     FailureClass
		wantMessage   string
	}{
		{
			name:          "invalid request",
			errorType:     "invalid_request_error",
			wantStatus:    400,
			wantRetryable: false,
			wantScope:     "request",
			wantClass:     ClassInvalidRequest,
			wantMessage:   "The provider rejected the request.",
		},
		{
			name:          "authentication",
			errorType:     "authentication_error",
			wantStatus:    401,
			wantRetryable: true,
			wantScope:     "account",
			wantClass:     ClassAuthentication,
			wantMessage:   "The provider rejected the subscription credentials.",
		},
		{
			name:          "billing",
			errorType:     "billing_error",
			wantStatus:    402,
			wantRetryable: true,
			wantScope:     "account",
			wantClass:     ClassBilling,
			wantMessage:   "The provider reported a billing restriction.",
		},
		{
			name:          "permission",
			errorType:     "permission_error",
			wantStatus:    403,
			wantRetryable: true,
			wantScope:     "account",
			wantClass:     ClassPermission,
			wantMessage:   "The provider denied this subscription access.",
		},
		{
			name:          "not found",
			errorType:     "not_found_error",
			wantStatus:    404,
			wantRetryable: false,
			wantScope:     "request",
			wantClass:     ClassNotFound,
			wantMessage:   "The provider could not find the requested resource.",
		},
		{
			name:          "conflict",
			errorType:     "conflict_error",
			wantStatus:    409,
			wantRetryable: false,
			wantScope:     "request",
			wantClass:     ClassConflict,
			wantMessage:   "The request conflicts with provider state.",
		},
		{
			name:          "request too large",
			errorType:     "request_too_large",
			wantStatus:    413,
			wantRetryable: false,
			wantScope:     "request",
			wantClass:     ClassPayloadTooLarge,
			wantMessage:   "The request exceeds the provider size limit.",
		},
		{
			name:          "rate limit",
			errorType:     "rate_limit_error",
			wantStatus:    429,
			wantRetryable: true,
			wantScope:     "model",
			wantClass:     ClassRateLimit,
			wantMessage:   "The provider rate limit was reached.",
		},
		{
			name:          "api error",
			errorType:     "api_error",
			wantStatus:    500,
			wantRetryable: true,
			wantScope:     "model",
			wantClass:     ClassProviderInternal,
			wantMessage:   "The provider encountered an internal error.",
		},
		{
			name:          "timeout",
			errorType:     "timeout_error",
			wantStatus:    504,
			wantRetryable: true,
			wantScope:     "model",
			wantClass:     ClassTimeout,
			wantMessage:   "The provider timed out while processing the request.",
		},
		{
			name:          "overloaded",
			errorType:     "overloaded_error",
			wantStatus:    529,
			wantRetryable: true,
			wantScope:     "model",
			wantClass:     ClassOverloaded,
			wantMessage:   "The provider is temporarily overloaded.",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload := `{"type":"error","error":{"type":"` + test.errorType + `","message":"private diagnostic payment_method=pm_private","details":{"access_token":"private"}},"request_id":"req_private"}`
			classification, ok := ParseAnthropicStandard(payload)
			if !ok {
				t.Fatal("ParseAnthropicStandard() did not recognize the documented error type")
			}
			if classification.Status != test.wantStatus ||
				classification.Retryable != test.wantRetryable {
				t.Fatalf("classification = %#v, want status=%d retryable=%t",
					classification, test.wantStatus, test.wantRetryable)
			}
			wantDetail := Detail{
				Type:            test.errorType,
				Code:            test.errorType,
				Message:         test.wantMessage,
				Scope:           test.wantScope,
				TaxonomyVersion: FailureTaxonomyV1,
				Class:           test.wantClass,
			}
			if classification.Detail != wantDetail {
				t.Fatalf("detail = %#v, want %#v", classification.Detail, wantDetail)
			}

			serialized, errMarshal := json.Marshal(classification)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			for _, forbidden := range []string{
				"req_private",
				"request_id",
				"private diagnostic",
				"payment_method",
				"pm_private",
				"access_token",
			} {
				if strings.Contains(string(serialized), forbidden) {
					t.Fatalf("classification leaks %q: %s", forbidden, serialized)
				}
			}
		})
	}
}

func TestParseAnthropicStandardPreservesReviewedMaxTokensParameter(t *testing.T) {
	t.Parallel()

	payload := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must not exceed 128000; request_id=req_private sk-private"}}`
	classification, ok := ParseAnthropicStandard(payload)
	if !ok {
		t.Fatal("ParseAnthropicStandard() did not recognize max_tokens rejection")
	}
	detail := classification.Detail
	if detail.Type != "invalid_request_error" || detail.Code != "invalid_parameter" ||
		detail.Parameter != "max_tokens" || detail.Reason != "invalid_max_tokens" ||
		detail.Class != ClassInvalidRequest || detail.Scope != ScopeRequest {
		t.Fatalf("detail = %#v, want reviewed max_tokens metadata", detail)
	}
	serialized, errMarshal := json.Marshal(classification)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{"128000", "req_private", "sk-private"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("classification leaks %q: %s", forbidden, serialized)
		}
	}
}

func TestParseAnthropicStandardKeepsOpaqueInvalidRequestGeneric(t *testing.T) {
	t.Parallel()

	classification, ok := ParseAnthropicStandard(
		`{"type":"error","error":{"type":"invalid_request_error","message":"The provider rejected this request."}}`,
	)
	if !ok {
		t.Fatal("ParseAnthropicStandard() did not recognize generic invalid request")
	}
	detail := classification.Detail
	if detail.Code != "invalid_request_error" || detail.Parameter != "" || detail.Reason != "" {
		t.Fatalf("detail = %#v, opaque rejection must remain generic", detail)
	}
}

func TestParseAnthropicStandardRejectsUnknownOrUnsafeEnvelopes(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		``,
		`{"type":"error"`,
		`{"error":{"type":"api_error"}}`,
		`{"type":"message","error":{"type":"api_error"}}`,
		`{"type":"error","error":{"type":"future_provider_error"}}`,
		`{"type":"error","error":{"type":"bravo_no_eligible_account"}}`,
		strings.Repeat("x", maxProviderErrorPayloadBytes+1),
	} {
		if classification, ok := ParseAnthropicStandard(payload); ok {
			t.Fatalf("ParseAnthropicStandard(%q) = %#v, true; want false", payload, classification)
		}
	}
}

func TestParseOpenAIStandardPreservesOnlySafeInvalidToolMetadata(t *testing.T) {
	payload := `{"error":{"type":"invalid_request_error","code":"invalid_tool_parameters","param":"tools[12].function.parameters","message":"schema contains context window and secret sk-private"},"request_id":"req_private"}`
	classification, ok := ParseOpenAIStandard(http.StatusBadRequest, payload)
	if !ok {
		t.Fatal("ParseOpenAIStandard() did not recognize invalid tool parameters")
	}
	detail := classification.Detail
	if detail.Type != "invalid_request_error" || detail.Code != "invalid_tool_parameters" ||
		detail.Parameter != "tools[12].function.parameters" ||
		detail.Class != ClassInvalidRequest || detail.Scope != ScopeRequest {
		t.Fatalf("detail = %#v", detail)
	}
	serialized, errMarshal := json.Marshal(detail)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{"req_private", "sk-private", "schema contains", "context window and secret"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("safe detail leaked %q: %s", forbidden, serialized)
		}
	}
}

func TestParseOpenAIStandardRejectsUnsafeParameterPath(t *testing.T) {
	payload := `{"error":{"type":"invalid_request_error","code":"invalid_tool_parameters","param":"tools[0].parameters; bearer secret","message":"invalid"}}`
	classification, ok := ParseOpenAIStandard(http.StatusUnprocessableEntity, payload)
	if !ok {
		t.Fatal("ParseOpenAIStandard() did not recognize envelope")
	}
	if classification.Detail.Parameter != "" {
		t.Fatalf("unsafe parameter = %q", classification.Detail.Parameter)
	}
}

func TestParseDoesNotInferUnknownScopeOrReason(t *testing.T) {
	t.Parallel()

	payload := `{"type":"error","error":{"type":"rate_limit_error","message":"Credits are required.","details":{"error_code":"credits_required","notice":{"title":"Credits required"}}}}`
	detail, ok := Parse(payload)
	if !ok {
		t.Fatal("Parse() did not recognize the provider error")
	}
	if detail.Scope != "" {
		t.Fatalf("Scope = %q, want empty scope", detail.Scope)
	}
	if detail.Reason != "" {
		t.Fatalf("Reason = %q, want empty reason", detail.Reason)
	}
}

func TestParseRejectsNonProviderErrorJSON(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"",
		"status code 429",
		`{"error":{"type":"rate_limit_error","details":{"error_code":"credits_required"}}}`,
		`{"type":"message","error":{"type":"rate_limit_error","message":"no"}}`,
		`{"type":"error","request_id":"req_only"}`,
		`{"type":"error","error":{"details":{"model":"claude-fable-5"}}}`,
		`{"type":"error","error":{"type":"invalid_request_error","details":{"error_code":"credits_required"}}}`,
		`{"type":"error","error":{"type":"rate_limit_error","details":{"error_code":"rate_limit_exceeded"}}}`,
		`prefix {"type":"error","error":{"type":"rate_limit_error"}} trailing`,
	} {
		if detail, ok := Parse(value); ok {
			t.Errorf("Parse(%q) = %#v, true; want false", value, detail)
		}
	}
}

func TestParseRejectsInternalCodeInjection(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"request_scoped", "bravo_no_eligible_account"} {
		payload := `{"type":"error","error":{"type":"rate_limit_error","message":"injected","details":{"error_code":"` + code + `","model":"claude-fable-5"}}}`
		if detail, ok := Parse(payload); ok {
			t.Errorf("Parse() accepted injected code %q: %#v", code, detail)
		}
	}
}

func TestParseRedactsSensitiveOrOversizedAllowedFields(t *testing.T) {
	t.Parallel()

	payload := `{"type":"error","error":{"type":"rate_limit_error","message":"Authorization: Bearer secret","details":{"error_code":"credits_required","model":"claude-fable-5","model_display_name":"request_id=req_private","notice":{"title":"You've hit your monthly spend limit","text":"request-id: req_private"},"disabled_reason":"org_level_disabled_until"}}}`
	detail, ok := Parse(payload)
	if !ok {
		t.Fatal("Parse() rejected the reviewed error signature")
	}
	if detail.Message != "" || detail.ModelDisplayName != "" || detail.NoticeText != "" {
		t.Fatalf("sensitive provider fields survived redaction: %#v", detail)
	}
	if detail.Model != "claude-fable-5" || detail.NoticeTitle != monthlySpendLimitNoticeTitle {
		t.Fatalf("safe provider fields were lost: %#v", detail)
	}

	longText := strings.Repeat("x", 2000)
	payload = `{"type":"error","error":{"type":"rate_limit_error","details":{"error_code":"credits_required","model":"claude-fable-5","notice":{"text":"` + longText + `"}}}}`
	detail, ok = Parse(payload)
	if !ok {
		t.Fatal("Parse() rejected oversized optional text")
	}
	if len(detail.NoticeText) > 600 {
		t.Fatalf("notice text length = %d, want at most 600", len(detail.NoticeText))
	}
}

func TestSanitizeRedactsAdditionalCredentialMarkerVariants(t *testing.T) {
	t.Parallel()

	for _, marker := range []string{
		"client_secret",
		"clientSecret",
		"client secret",
		"session_token",
		"sessionToken",
		"session token",
		"id_token",
		"idToken",
		"id token",
		"private_key",
		"privateKey",
		"private key",
		"secret_key",
		"secretKey",
		"secret key",
		"secret_access_key",
		"secretAccessKey",
		"secret access key",
		"password",
		"pass_word",
		"passWord",
		"pass word",
		"passphrase",
		"pass_phrase",
		"passPhrase",
		"pass phrase",
	} {
		t.Run(marker, func(t *testing.T) {
			detail := Sanitize(Detail{
				Message:    "provider diagnostic " + marker + "=private-value",
				NoticeText: "provider notice " + marker + "=private-value",
			})
			if detail.Message != "" || detail.NoticeText != "" {
				t.Fatalf("credential marker %q survived sanitization: %#v", marker, detail)
			}
		})
	}
}
