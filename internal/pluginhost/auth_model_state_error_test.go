package pluginhost

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestHostAuthModelStatesCarriesSafeErrorInputs(t *testing.T) {
	updatedAt := time.Date(2026, time.July, 27, 2, 11, 17, 0, time.UTC)
	auth := &coreauth.Auth{
		ModelStates: map[string]*coreauth.ModelState{
			"claude-fable-5": {
				Status:        coreauth.StatusError,
				StatusMessage: anthropicCreditsRequiredModelStatePayload,
				Unavailable:   true,
				UpdatedAt:     updatedAt,
				LastError: &coreauth.Error{
					Code:       "credits_required",
					Message:    anthropicCreditsRequiredModelStatePayload,
					Retryable:  true,
					HTTPStatus: 429,
				},
			},
		},
	}

	states := hostAuthModelStates(auth)
	raw, errMarshal := json.Marshal(states["claude-fable-5"])
	if errMarshal != nil {
		t.Fatalf("marshal host model state: %v", errMarshal)
	}
	payload := string(raw)
	var view map[string]any
	if errUnmarshal := json.Unmarshal(raw, &view); errUnmarshal != nil {
		t.Fatalf("unmarshal host model state: %v", errUnmarshal)
	}
	want := map[string]string{
		"error_code":                  "credits_required",
		"error_message":               "Usage credits are required for this model.",
		"provider_model":              "claude-fable-5",
		"provider_model_display_name": "Fable 5",
		"provider_notice_title":       "You've hit your monthly spend limit",
		"provider_notice_text":        "Ask your admin to raise your spend limit, or switch models to continue this chat.",
		"provider_disabled_reason":    "org_level_disabled_until",
		"scope":                       "model",
		"reason":                      "monthly_spend_limit",
		"updated_at":                  updatedAt.Format(time.RFC3339),
	}
	for field, expected := range want {
		if actual, _ := view[field].(string); actual != expected {
			t.Errorf("%s = %q, want %q (payload=%s)", field, actual, expected, payload)
		}
	}
	statusMessage, _ := view["status_message"].(string)
	if statusMessage == "" || strings.HasPrefix(strings.TrimSpace(statusMessage), "{") {
		t.Errorf("status_message = %q, want a safe non-JSON summary", statusMessage)
	}
	for _, forbidden := range []string{
		"req_should_not_cross_host_boundary",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		`"type":"error"`,
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("host model state leaked %q: %s", forbidden, payload)
		}
	}

	auth.ID = "palantir"
	auth.Provider = "claude"
	auth.StatusMessage = anthropicCreditsRequiredModelStatePayload
	auth.Attributes = map[string]string{"runtime_only": "true"}
	entry := (&Host{}).buildHostAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("buildHostAuthFileEntry returned nil for runtime auth")
	}
	entryRaw, errMarshal := json.Marshal(entry)
	if errMarshal != nil {
		t.Fatalf("marshal full host auth entry: %v", errMarshal)
	}
	if entry.StatusMessage != "Fable 5: You've hit your monthly spend limit" {
		t.Fatalf("top-level status_message = %q, want safe provider summary", entry.StatusMessage)
	}
	for _, forbidden := range []string{
		"req_should_not_cross_host_boundary",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		`"type":"error"`,
	} {
		if strings.Contains(string(entryRaw), forbidden) {
			t.Fatalf("full host auth entry leaked %q: %s", forbidden, entryRaw)
		}
	}
}

func TestHostAuthModelStatesFailsClosedForUnreviewedStructuredPayload(t *testing.T) {
	const unreviewed = `{"type":"error","error":{"message":"private diagnostic"},"request_id":"req_private","has_chargeable_saved_payment_method":true}`
	auth := &coreauth.Auth{
		ModelStates: map[string]*coreauth.ModelState{
			"claude-unknown": {
				Status:        coreauth.StatusError,
				StatusMessage: unreviewed,
				LastError: &coreauth.Error{
					Code:    "model_execution_failed",
					Message: unreviewed,
				},
			},
		},
	}

	state := hostAuthModelStates(auth)["claude-unknown"]
	if state.StatusMessage != "" || state.ErrorMessage != "" {
		t.Fatalf("unreviewed provider JSON crossed host boundary: %#v", state)
	}
	if state.ErrorCode != "model_execution_failed" {
		t.Fatalf("safe typed error code = %q, want model_execution_failed", state.ErrorCode)
	}
	raw, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{"req_private", "private diagnostic", "payment_method"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("unreviewed provider state leaked %q: %s", forbidden, raw)
		}
	}
}

func TestHostAuthModelStatesKeepsShortPlainDiagnostics(t *testing.T) {
	auth := &coreauth.Auth{
		ModelStates: map[string]*coreauth.ModelState{
			"claude-sonnet-5": {
				Status:        coreauth.StatusError,
				StatusMessage: "temporary provider connection failure",
				LastError: &coreauth.Error{
					Code:    "upstream_unavailable",
					Message: "temporary provider connection failure",
				},
			},
		},
	}

	state := hostAuthModelStates(auth)["claude-sonnet-5"]
	if state.StatusMessage != "temporary provider connection failure" ||
		state.ErrorMessage != "temporary provider connection failure" ||
		state.ErrorCode != "upstream_unavailable" {
		t.Fatalf("plain diagnostic state = %#v", state)
	}
}

func TestHostAuthDiagnosticsFailClosedForPlainCredentialMarkers(t *testing.T) {
	tests := []string{
		"session_key=session-secret",
		"SESSION-KEY: session-secret",
		"session key session-secret",
		"sessionKey=session-secret",
		"session_token=session-token",
		"client_secret=client-secret",
		"CLIENT-SECRET: client-secret",
		"client secret client-secret",
		"clientSecret=client-secret",
		"id-token=id-token-value",
		"private_key=private-key-value",
		"secret access key=secret-key-value",
		"password=hunter2",
		"PASSWORD: hunter2",
		"pass-word=hunter2",
		"pass_word=hunter2",
		"passphrase=correct-horse-battery-staple",
		"requestId=req_private",
		"apiKey=sk-private",
		"accessToken=access-private",
		"refreshToken=refresh-private",
		"authorization=Bearer private",
		"paymentMethod=pm_private",
	}

	for _, diagnostic := range tests {
		t.Run(diagnostic, func(t *testing.T) {
			auth := &coreauth.Auth{
				ID:            "sensitive-auth",
				Provider:      "claude",
				StatusMessage: diagnostic,
				Attributes:    map[string]string{"runtime_only": "true"},
				ModelStates: map[string]*coreauth.ModelState{
					"claude-sonnet-5": {
						Status:        coreauth.StatusError,
						StatusMessage: diagnostic,
						LastError: &coreauth.Error{
							Code:    "upstream_unavailable",
							Message: diagnostic,
						},
					},
				},
			}

			state := hostAuthModelStates(auth)["claude-sonnet-5"]
			if state.StatusMessage != "" || state.ErrorMessage != "" {
				t.Fatalf("plain credential diagnostic crossed model-state boundary: %#v", state)
			}

			entry := (&Host{}).buildHostAuthFileEntry(auth)
			if entry == nil {
				t.Fatal("buildHostAuthFileEntry returned nil for runtime auth")
			}
			if entry.StatusMessage != "" {
				t.Fatalf("plain credential diagnostic crossed top-level boundary: %q", entry.StatusMessage)
			}
		})
	}
}

func TestHostAuthDiagnosticsKeepBenignMonthlySpendSummary(t *testing.T) {
	const diagnostic = "Fable 5: You've hit your monthly spend limit"
	auth := &coreauth.Auth{
		ID:            "monthly-spend-auth",
		Provider:      "claude",
		StatusMessage: diagnostic,
		Attributes:    map[string]string{"runtime_only": "true"},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-fable-5": {
				Status:        coreauth.StatusError,
				StatusMessage: diagnostic,
				LastError: &coreauth.Error{
					Code:    "credits_required",
					Message: diagnostic,
				},
			},
		},
	}

	state := hostAuthModelStates(auth)["claude-fable-5"]
	if state.StatusMessage != diagnostic || state.ErrorMessage != diagnostic {
		t.Fatalf("benign monthly-spend diagnostic was redacted: %#v", state)
	}
	entry := (&Host{}).buildHostAuthFileEntry(auth)
	if entry == nil || entry.StatusMessage != diagnostic {
		t.Fatalf("top-level monthly-spend diagnostic = %#v, want %q", entry, diagnostic)
	}
}

const anthropicCreditsRequiredModelStatePayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_should_not_cross_host_boundary"}`
