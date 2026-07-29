package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type staticEnvelopePluginClient struct {
	raw []byte
}

func (c staticEnvelopePluginClient) Call(context.Context, string, []byte) ([]byte, error) {
	return c.raw, nil
}

func (c staticEnvelopePluginClient) Shutdown() {}

func TestDecodeEnvelopeResultPreservesPluginHTTPStatus(t *testing.T) {
	providerDetail := providererror.Detail{
		Type:             "rate_limit_error",
		Code:             "credits_required",
		Model:            "claude-fable-5",
		ModelDisplayName: "Fable 5",
		Scope:            "model",
		Reason:           "monthly_spend_limit",
	}
	_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:          "plugin_error",
			Message:       "license required",
			Retryable:     true,
			HTTPStatus:    http.StatusForbidden,
			Headers:       http.Header{"Retry-After": []string{"7"}},
			RetryAfter:    "7",
			ProviderError: &providerDetail,
		},
	})
	if errDecode == nil {
		t.Fatal("decodeEnvelopeResult returned nil error")
	}
	if got := errDecode.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errDecode.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errDecode)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
	coded, ok := errDecode.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "plugin_error" {
		t.Fatalf("error code provider = %#v", errDecode)
	}
	retryable, ok := errDecode.(interface{ Retryable() bool })
	if !ok || !retryable.Retryable() {
		t.Fatalf("retryable provider = %#v", errDecode)
	}
	headerProvider, ok := errDecode.(interface{ Headers() http.Header })
	if !ok || headerProvider.Headers().Get("Retry-After") != "7" {
		t.Fatalf("headers provider = %#v", errDecode)
	}
	retryAfterProvider, ok := errDecode.(interface{ RetryAfterValue() string })
	if !ok || retryAfterProvider.RetryAfterValue() != "7" {
		t.Fatalf("retry-after provider = %#v", errDecode)
	}
	provider, ok := errDecode.(interface {
		ProviderErrorDetail() (providererror.Detail, bool)
	})
	if !ok {
		t.Fatalf("provider detail carrier = %#v", errDecode)
	}
	detail, ok := provider.ProviderErrorDetail()
	if !ok || detail.Code != "credits_required" || detail.Model != "claude-fable-5" {
		t.Fatalf("provider detail = %#v, ok=%t", detail, ok)
	}
}

func TestCallPluginReturnsPluginErrorWithoutMethodWrapper(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       "plugin_error",
			Message:    "license required",
			HTTPStatus: http.StatusForbidden,
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}
	_, errCall := callPlugin[rpcEmptyResponse](context.Background(), staticEnvelopePluginClient{raw: raw}, pluginabi.MethodExecutorExecuteStream, rpcEmptyResponse{})
	if errCall == nil {
		t.Fatal("callPlugin returned nil error")
	}
	if got := errCall.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errCall.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errCall)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestIsPluginErrorEnvelopeAcceptsNonzeroReturnEnvelope(t *testing.T) {
	raw := marshalRPCError("plugin_error", "upstream failed")
	if !isPluginErrorEnvelope(raw) {
		t.Fatalf("isPluginErrorEnvelope(%s) = false, want true", raw)
	}
	if isPluginErrorEnvelope([]byte(`not json`)) {
		t.Fatal("isPluginErrorEnvelope accepted invalid JSON")
	}
}
