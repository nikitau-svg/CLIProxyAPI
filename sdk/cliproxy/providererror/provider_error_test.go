package providererror

import (
	"encoding/json"
	"strings"
	"testing"
)

const creditsRequiredPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_must_not_be_retained"}`

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
