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

type typedContextHostCallbackError struct {
	detail providererror.Detail
}

func (e typedContextHostCallbackError) Error() string     { return e.detail.Message }
func (e typedContextHostCallbackError) ErrorCode() string { return e.detail.Code }
func (e typedContextHostCallbackError) StatusCode() int   { return http.StatusBadRequest }
func (e typedContextHostCallbackError) Retryable() bool   { return false }
func (e typedContextHostCallbackError) ProviderErrorDetail() (providererror.Detail, bool) {
	return e.detail, true
}

func TestHostModelCallbackPreservesTypedContextFailure(t *testing.T) {
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
	callbackErr := newHostModelCallbackError(&interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      typedContextHostCallbackError{detail: want},
	})
	rawEnvelope := marshalHostCallbackError(callbackErr)

	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawEnvelope, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal callback envelope: %v", errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("callback envelope = %#v, want structured error", envelope)
	}
	if envelope.Error.Code != want.Code || envelope.Error.HTTPStatus != http.StatusBadRequest || envelope.Error.Retryable {
		t.Fatalf("callback error = %#v", envelope.Error)
	}
	if envelope.Error.ProviderError == nil || *envelope.Error.ProviderError != want {
		t.Fatalf("callback provider_error = %#v, want %#v", envelope.Error.ProviderError, want)
	}
}

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
