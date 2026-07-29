package pluginhost

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const hostCallbackPalantirCreditsPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_host_callback_private"}`

type rawPalantirHostCallbackError struct{}

func (rawPalantirHostCallbackError) Error() string     { return hostCallbackPalantirCreditsPayload }
func (rawPalantirHostCallbackError) ErrorCode() string { return "credits_required" }
func (rawPalantirHostCallbackError) StatusCode() int   { return http.StatusTooManyRequests }
func (rawPalantirHostCallbackError) Retryable() bool   { return true }

func TestHostModelCallbackSanitizesExactPalantirCreditsError(t *testing.T) {
	callbackErr := newHostModelCallbackError(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      rawPalantirHostCallbackError{},
	})
	rawEnvelope := marshalHostCallbackError(callbackErr)

	for _, forbidden := range []string{
		"req_host_callback_private",
		"request_id",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		"exhausted_included_allowance",
		`"cta"`,
		"switch_model",
		"redirect_hint",
		`\"type\":\"error\"`,
	} {
		if strings.Contains(strings.ToLower(string(rawEnvelope)), strings.ToLower(forbidden)) {
			t.Errorf("host callback leaks forbidden provider field %q: %s", forbidden, rawEnvelope)
		}
	}

	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawEnvelope, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal callback envelope: %v", errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("callback envelope = %#v, want structured error", envelope)
	}
	want, ok := providererror.Parse(hostCallbackPalantirCreditsPayload)
	if !ok {
		t.Fatal("test fixture is not a reviewed provider error")
	}
	if envelope.Error.Message != want.Summary() {
		t.Errorf("callback message = %q, want safe summary %q", envelope.Error.Message, want.Summary())
	}
	if envelope.Error.ProviderError == nil {
		t.Fatal("callback provider_error = nil")
	}
	if got := providererror.Sanitize(*envelope.Error.ProviderError); got != want {
		t.Fatalf("callback provider_error = %#v, want %#v", got, want)
	}
}
