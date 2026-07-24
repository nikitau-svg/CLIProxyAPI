package pluginabi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"name":"example"}`)
	env := Envelope{
		OK:     true,
		Result: payload,
	}

	raw, errMarshal := json.Marshal(env)
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}

	var decoded Envelope
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	if !decoded.OK || string(decoded.Result) != string(payload) {
		t.Fatalf("decoded envelope = %#v, want ok payload", decoded)
	}
}

func TestErrorEnvelopePreservesRetryMetadata(t *testing.T) {
	env := Envelope{
		OK: false,
		Error: &Error{
			Code:       "rate_limited",
			Message:    "try later",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"11"}, "X-Request-Id": []string{"req-1"}},
			RetryAfter: "11",
		},
	}
	raw, errMarshal := json.Marshal(env)
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}
	var decoded Envelope
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	if decoded.Error == nil ||
		decoded.Error.Code != "rate_limited" ||
		!decoded.Error.Retryable ||
		decoded.Error.HTTPStatus != http.StatusTooManyRequests ||
		decoded.Error.Headers.Get("Retry-After") != "11" ||
		decoded.Error.Headers.Get("X-Request-Id") != "req-1" ||
		decoded.Error.RetryAfter != "11" {
		t.Fatalf("decoded error = %#v", decoded.Error)
	}
}

func TestMethodNamesAreStable(t *testing.T) {
	if MethodPluginRegister != "plugin.register" {
		t.Fatalf("MethodPluginRegister = %q", MethodPluginRegister)
	}
	if MethodRequestInterceptBefore != "request.intercept_before" {
		t.Fatalf("MethodRequestInterceptBefore = %q", MethodRequestInterceptBefore)
	}
	if MethodRequestInterceptAfter != "request.intercept_after" {
		t.Fatalf("MethodRequestInterceptAfter = %q", MethodRequestInterceptAfter)
	}
	if MethodResponseInterceptAfter != "response.intercept_after" {
		t.Fatalf("MethodResponseInterceptAfter = %q", MethodResponseInterceptAfter)
	}
	if MethodResponseInterceptStreamChunk != "response.intercept_stream_chunk" {
		t.Fatalf("MethodResponseInterceptStreamChunk = %q", MethodResponseInterceptStreamChunk)
	}
	if MethodHostHTTPDo != "host.http.do" {
		t.Fatalf("MethodHostHTTPDo = %q", MethodHostHTTPDo)
	}
	if MethodHostHTTPStreamRead != "host.http.stream_read" {
		t.Fatalf("MethodHostHTTPStreamRead = %q", MethodHostHTTPStreamRead)
	}
	if MethodHostModelExecute != "host.model.execute" {
		t.Fatalf("MethodHostModelExecute = %q", MethodHostModelExecute)
	}
	if MethodHostModelCountTokens != "host.model.count_tokens" {
		t.Fatalf("MethodHostModelCountTokens = %q", MethodHostModelCountTokens)
	}
	if MethodHostModelExecuteStream != "host.model.execute_stream" {
		t.Fatalf("MethodHostModelExecuteStream = %q", MethodHostModelExecuteStream)
	}
	if MethodHostModelStreamRead != "host.model.stream_read" {
		t.Fatalf("MethodHostModelStreamRead = %q", MethodHostModelStreamRead)
	}
	if MethodHostModelStreamClose != "host.model.stream_close" {
		t.Fatalf("MethodHostModelStreamClose = %q", MethodHostModelStreamClose)
	}
	if MethodHostModelList != "host.model.list" {
		t.Fatalf("MethodHostModelList = %q", MethodHostModelList)
	}
	if MethodHostAuthList != "host.auth.list" {
		t.Fatalf("MethodHostAuthList = %q", MethodHostAuthList)
	}
	if MethodHostAuthGet != "host.auth.get" {
		t.Fatalf("MethodHostAuthGet = %q", MethodHostAuthGet)
	}
	if MethodHostAuthGetRuntime != "host.auth.get_runtime" {
		t.Fatalf("MethodHostAuthGetRuntime = %q", MethodHostAuthGetRuntime)
	}
	if MethodHostAuthQuotaGet != "host.auth.quota_get" {
		t.Fatalf("MethodHostAuthQuotaGet = %q", MethodHostAuthQuotaGet)
	}
	if MethodHostAuthSave != "host.auth.save" {
		t.Fatalf("MethodHostAuthSave = %q", MethodHostAuthSave)
	}
	if MethodHostPluginConfigListMutate != "host.plugin.config.list_mutate" {
		t.Fatalf("MethodHostPluginConfigListMutate = %q", MethodHostPluginConfigListMutate)
	}
	if MethodExecutorExecuteStream != "executor.execute_stream" {
		t.Fatalf("MethodExecutorExecuteStream = %q", MethodExecutorExecuteStream)
	}
}

func TestSchedulerPickMethodName(t *testing.T) {
	if MethodSchedulerPick != "scheduler.pick" {
		t.Fatalf("MethodSchedulerPick = %q", MethodSchedulerPick)
	}
	if MethodModelRoute != "model.route" {
		t.Fatalf("MethodModelRoute = %q", MethodModelRoute)
	}
}
