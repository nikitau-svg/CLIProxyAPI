package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type fakeHostModelExecutor struct {
	executeModel       func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage)
	executeModelStream func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage)
	countModelTokens   func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage)
}

func (e *fakeHostModelExecutor) ExecuteModel(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
	return e.executeModel(ctx, req)
}

func (e *fakeHostModelExecutor) ExecuteModelStream(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
	return e.executeModelStream(ctx, req)
}

func (e *fakeHostModelExecutor) CountModelTokens(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
	return e.countModelTokens(ctx, req)
}

func TestHostHTTPDoCallbackUsesHostHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		w.Header().Set("X-Test", "ok")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    server.URL,
		Body:   []byte(`{"request":true}`),
	}
	rawReq, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	rawResp, errCall := New().callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDo, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	resp, errDecode := decodeRPCEnvelope[pluginapi.HTTPResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("response = %#v, want status 200 body", resp)
	}
	if resp.Headers.Get("X-Test") != "ok" {
		t.Fatalf("X-Test = %q, want ok", resp.Headers.Get("X-Test"))
	}
}

func TestHostHTTPDoCallbackRestoresRegisteredRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	host := New()
	host.mu.Lock()
	host.runtimeConfig = &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	host.mu.Unlock()
	callbackID, closeCallback := host.openCallbackContext(ctx)
	defer closeCallback()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			t.Fatalf("request context error = %v", r.Context().Err())
		}
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte("upstream-body"))
	}))
	defer server.Close()

	rawReq, errMarshal := json.Marshal(rpcHostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            server.URL,
		Body:           []byte(`{"request":true}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDo, rawReq); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	rawAPIRequest, okRequest := ginCtx.Get("API_REQUEST")
	if !okRequest {
		t.Fatal("API_REQUEST was not captured on the original Gin context")
	}
	apiRequest, _ := rawAPIRequest.([]byte)
	if !bytes.Contains(apiRequest, []byte("=== API REQUEST 1 ===")) || !bytes.Contains(apiRequest, []byte(`{"request":true}`)) {
		t.Fatalf("API_REQUEST = %q, want upstream request details", apiRequest)
	}

	rawAPIResponse, okResponse := ginCtx.Get("API_RESPONSE")
	if !okResponse {
		t.Fatal("API_RESPONSE was not captured on the original Gin context")
	}
	apiResponse, _ := rawAPIResponse.([]byte)
	if !bytes.Contains(apiResponse, []byte("=== API RESPONSE 1 ===")) || !bytes.Contains(apiResponse, []byte("upstream-body")) {
		t.Fatalf("API_RESPONSE = %q, want upstream response details", apiResponse)
	}
}

func TestHostHTTPDoStreamCallbackReturnsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = w.Write([]byte("second"))
	}))
	defer server.Close()
	defer close(release)

	rawReq, errMarshal := json.Marshal(pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    server.URL,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	type callResult struct {
		raw []byte
		err error
	}
	done := make(chan callResult, 1)
	host := New()
	go func() {
		rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDoStream, rawReq)
		done <- callResult{raw: rawResp, err: errCall}
	}()

	var result callResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("host.http.do_stream waited for the whole upstream response")
	}
	if result.err != nil {
		t.Fatalf("callFromPlugin() error = %v", result.err)
	}

	resp, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamResponse](result.raw)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}
	readReq, errMarshal := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	rawRead, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("read callback error = %v", errRead)
	}
	chunk, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode read response: %v", errDecode)
	}
	if string(chunk.Payload) != "first" || chunk.Done || chunk.Error != "" {
		t.Fatalf("read chunk = %#v, want first payload", chunk)
	}

	closeReq, errMarshal := json.Marshal(rpcHostHTTPStreamCloseRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPStreamClose, closeReq); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
}

func TestHostStreamCallbacksEmitAndClose(t *testing.T) {
	host := New()
	streamID, chunks, cleanup := host.streams.open(context.Background())
	defer cleanup()

	emitReq, errMarshal := json.Marshal(rpcStreamEmitRequest{StreamID: streamID, Payload: []byte("chunk")})
	if errMarshal != nil {
		t.Fatalf("marshal emit request: %v", errMarshal)
	}
	if _, errEmit := host.callFromPlugin(context.Background(), pluginabi.MethodHostStreamEmit, emitReq); errEmit != nil {
		t.Fatalf("emit callback error = %v", errEmit)
	}

	closeReq, errMarshal := json.Marshal(rpcStreamCloseRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostStreamClose, closeReq); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}

	chunk, ok := <-chunks
	if !ok {
		t.Fatalf("stream closed before chunk")
	}
	if string(chunk.Payload) != "chunk" || chunk.Err != nil {
		t.Fatalf("chunk = %#v, want payload chunk", chunk)
	}
	if _, ok = <-chunks; ok {
		t.Fatalf("stream remains open after close")
	}
}

func TestHostStreamEmitWithCanceledCallbackReturnsRequestCanceled(t *testing.T) {
	host := New()
	parentCtx, cancelParent := context.WithCancel(context.Background())
	callbackID, closeCallback := host.openCallbackContextForPlugin(parentCtx, "bravo")
	defer closeCallback()
	streamID, _, cleanupStream := host.streams.open(parentCtx)

	cancelParent()
	cleanupStream()

	emitReq, errMarshal := json.Marshal(map[string]any{
		"stream_id":        streamID,
		"host_callback_id": callbackID,
		"payload":          []byte("must-not-be-emitted"),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errEmit := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostStreamEmit,
		emitReq,
	)
	assertHostCallbackScopeError(t, errEmit, "request_canceled", statusClientClosedRequest)
}

func TestHostModelExecuteCallback(t *testing.T) {
	host := New()
	var got handlers.ModelExecutionRequest
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModel: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
			got = req
			return handlers.ModelExecutionResponse{
				StatusCode: http.StatusAccepted,
				Headers:    http.Header{"X-Model": []string{"ok"}},
				Body:       []byte(`{"response":true}`),
			}, nil
		},
	})

	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol:   "openai",
			ExitProtocol:    "claude",
			ForcedProvider:  "claude",
			AuthID:          "claude-account-1",
			SingleAttempt:   true,
			AllowImageModel: true,
			Model:           "model-1",
			Body:            []byte(`{"request":true}`),
			Headers:         http.Header{"X-Request": []string{"yes"}},
			Query:           url.Values{"alt": []string{"sse"}},
			Alt:             "raw",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StatusCode != http.StatusAccepted || string(resp.Body) != `{"response":true}` {
		t.Fatalf("response = %#v, want accepted body", resp)
	}
	if resp.Headers.Get("X-Model") != "ok" {
		t.Fatalf("X-Model = %q, want ok", resp.Headers.Get("X-Model"))
	}
	if got.EntryProtocol != "openai" || got.ExitProtocol != "claude" || got.Model != "model-1" || got.Stream {
		t.Fatalf("request protocols/model/stream = %#v", got)
	}
	if got.ForcedProvider != "claude" || got.AuthID != "claude-account-1" || !got.SingleAttempt || !got.AllowImageModel {
		t.Fatalf("request execution controls = %#v", got)
	}
	if string(got.Body) != `{"request":true}` {
		t.Fatalf("request body = %q, want original body", got.Body)
	}
	if got.Headers.Get("X-Request") != "yes" {
		t.Fatalf("request header = %q, want yes", got.Headers.Get("X-Request"))
	}
	if got.Query.Get("alt") != "sse" {
		t.Fatalf("query alt = %q, want sse", got.Query.Get("alt"))
	}
	if got.Alt != "raw" {
		t.Fatalf("alt = %q, want raw", got.Alt)
	}
}

func TestHostModelExecuteCallbackPreservesTypedErrorMetadata(t *testing.T) {
	host := New()
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModel: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
			return handlers.ModelExecutionResponse{}, &interfaces.ErrorMessage{
				StatusCode: http.StatusTooManyRequests,
				Error: &coreauth.Error{
					Code:       "rate_limited",
					Message:    "quota exhausted",
					Retryable:  true,
					HTTPStatus: http.StatusTooManyRequests,
				},
				Addon: http.Header{
					"Retry-After":  []string{"17"},
					"X-Request-Id": []string{"req-typed"},
				},
			}
		},
	})
	rawReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawReq)
	if errCall == nil {
		t.Fatal("callFromPlugin() error = nil")
	}
	rawEnvelope := marshalHostCallbackError(errCall)
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawEnvelope, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal callback envelope: %v", errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("callback envelope = %#v", envelope)
	}
	if envelope.Error.Code != "rate_limited" ||
		envelope.Error.Message != "rate_limited: quota exhausted" ||
		envelope.Error.HTTPStatus != http.StatusTooManyRequests ||
		!envelope.Error.Retryable ||
		envelope.Error.Headers.Get("Retry-After") != "17" ||
		envelope.Error.Headers.Get("X-Request-Id") != "req-typed" ||
		envelope.Error.RetryAfter != "17" {
		t.Fatalf("callback error = %#v", envelope.Error)
	}
}

func TestHostModelCallbackErrorPreservesRetryAfterWithoutHeader(t *testing.T) {
	providerDetail := providererror.Detail{
		Type:             "rate_limit_error",
		Code:             "credits_required",
		Model:            "claude-fable-5",
		ModelDisplayName: "Fable 5",
		Scope:            providererror.ScopeModel,
		Reason:           "monthly_spend_limit",
	}
	errCall := newHostModelCallbackError(&interfaces.ErrorMessage{
		Error: rpcPluginError{
			code:          "rate_limited",
			message:       "try later",
			statusCode:    http.StatusTooManyRequests,
			retryable:     true,
			retryAfter:    "23",
			providerError: &providerDetail,
		},
	})
	rawEnvelope := marshalHostCallbackError(errCall)
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawEnvelope, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal callback envelope: %v", errUnmarshal)
	}
	if envelope.Error == nil ||
		envelope.Error.Code != "credits_required" ||
		envelope.Error.HTTPStatus != http.StatusTooManyRequests ||
		!envelope.Error.Retryable ||
		envelope.Error.RetryAfter != "23" ||
		envelope.Error.ProviderError == nil ||
		envelope.Error.ProviderError.Code != "credits_required" ||
		envelope.Error.ProviderError.Model != "claude-fable-5" {
		t.Fatalf("callback error = %#v", envelope.Error)
	}
}

func TestHostModelCountTokensCallback(t *testing.T) {
	host := New()
	var got handlers.ModelExecutionRequest
	host.SetModelExecutor(&fakeHostModelExecutor{
		countModelTokens: func(_ context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
			got = req
			return handlers.ModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"X-Count": []string{"ok"}},
				Body:       []byte(`{"input_tokens":42}`),
			}, nil
		},
	})
	rawReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol:  "claude",
		ExitProtocol:   "claude",
		ForcedProvider: "claude",
		AuthID:         "claude-account-1",
		SingleAttempt:  true,
		Model:          "claude-test",
		Body:           []byte(`{"messages":[]}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelCountTokens, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"input_tokens":42}` || resp.Headers.Get("X-Count") != "ok" {
		t.Fatalf("response = %#v", resp)
	}
	if got.ForcedProvider != "claude" || got.AuthID != "claude-account-1" || !got.SingleAttempt || got.Model != "claude-test" {
		t.Fatalf("count request = %#v", got)
	}
}

func TestHostModelExecuteCallbackCarriesCallerPluginSkipID(t *testing.T) {
	host := New()
	var got handlers.ModelExecutionRequest
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModel: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
			got = req
			return handlers.ModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
		},
	})
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "origin-plugin")
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Body:          []byte(`{"request":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawReq); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	if got.SkipInterceptorPluginID != "origin-plugin" {
		t.Fatalf("SkipInterceptorPluginID = %q, want origin-plugin", got.SkipInterceptorPluginID)
	}
	if got.SkipRouterPluginID != "origin-plugin" {
		t.Fatalf("SkipRouterPluginID = %q, want origin-plugin", got.SkipRouterPluginID)
	}
}

func TestHostModelStreamClosesWithCallbackScope(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"X-Stream": []string{"ok"}},
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})
	callbackID, closeCallback := host.openCallbackContext(context.Background())
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"stream":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}

	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	closeCallback()
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after callback scope closed")
	}
}

func TestHostModelStreamReadAfterCallbackCloseReturnsDone(t *testing.T) {
	host := New()
	chunks := make(chan handlers.ModelExecutionChunk)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     chunks,
			}, nil
		},
	})
	callbackID, closeCallback := host.openCallbackContext(context.Background())

	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"stream":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("execute stream callback error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode stream response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}

	closeCallback()
	readReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	readDone := make(chan pluginapi.HostModelStreamReadResponse, 1)
	readErr := make(chan error, 1)
	go func() {
		rawRead, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
		if errRead != nil {
			readErr <- errRead
			return
		}
		doneResp, errDecodeRead := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](rawRead)
		if errDecodeRead != nil {
			readErr <- errDecodeRead
			return
		}
		readDone <- doneResp
	}()
	select {
	case errRead := <-readErr:
		t.Fatalf("read after callback close error = %v", errRead)
	case doneResp := <-readDone:
		if !doneResp.Done || len(doneResp.Payload) != 0 || doneResp.Error != "" {
			t.Fatalf("read after callback close = %#v, want done without payload/error", doneResp)
		}
	case <-time.After(time.Second):
		t.Fatal("read after callback close blocked")
	}
}

func TestHostModelExecuteStreamStartupErrorCleansUp(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			return handlers.ModelExecutionStream{}, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadGateway,
			}
		},
	})

	rawReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
		Stream:        true,
		Body:          []byte(`{"stream":true}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall == nil {
		t.Fatalf("execute stream callback error is nil, raw response = %q", rawResp)
	}
	if rawResp != nil {
		t.Fatalf("raw response = %q, want nil on startup error", rawResp)
	}
	if !strings.Contains(errCall.Error(), "status 502") {
		t.Fatalf("execute stream callback error = %v, want status 502", errCall)
	}

	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after startup error")
	}
	gotCount := hostModelStreamCountForTest(t, host)
	if gotCount != 0 {
		t.Fatalf("model stream count = %d, want 0", gotCount)
	}
}

func TestHostModelCallbacksValidateStreamMode(t *testing.T) {
	host := New()

	rawExecuteReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
		Stream:        true,
	})
	if errMarshal != nil {
		t.Fatalf("marshal execute request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawExecuteReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host.model.execute requires stream=false") {
		t.Fatalf("execute callback error = %v, want stream=false validation error", errCall)
	}

	rawStreamReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
		Stream:        false,
	})
	if errMarshal != nil {
		t.Fatalf("marshal execute stream request: %v", errMarshal)
	}
	_, errCall = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawStreamReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host.model.execute_stream requires stream=true") {
		t.Fatalf("execute stream callback error = %v, want stream=true validation error", errCall)
	}
}

func TestHostModelCallbacksRequireExecutor(t *testing.T) {
	host := New()

	rawExecuteReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
	})
	if errMarshal != nil {
		t.Fatalf("marshal execute request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawExecuteReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host model executor is unavailable") {
		t.Fatalf("execute callback error = %v, want unavailable executor error", errCall)
	}

	rawStreamReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
		Stream:        true,
	})
	if errMarshal != nil {
		t.Fatalf("marshal execute stream request: %v", errMarshal)
	}
	_, errCall = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawStreamReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host model executor is unavailable") {
		t.Fatalf("execute stream callback error = %v, want unavailable executor error", errCall)
	}
}

func TestHostModelStreamReadAndCloseValidateStreamID(t *testing.T) {
	host := New()

	rawReadReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	_, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, rawReadReq)
	if errRead == nil || !strings.Contains(errRead.Error(), "model stream id is required") {
		t.Fatalf("read callback error = %v, want required stream id error", errRead)
	}

	rawCloseReq, errMarshal := json.Marshal(pluginapi.HostModelStreamCloseRequest{})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	rawClose, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamClose, rawCloseReq)
	if errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
	_, errDecode := decodeRPCEnvelope[rpcEmptyResponse](rawClose)
	if errDecode != nil {
		t.Fatalf("decode close response: %v", errDecode)
	}
}

func TestHostModelStreamReadReturnsPayloadAndTerminalError(t *testing.T) {
	host := New()
	providerDetail := providererror.Detail{
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
	chunks := make(chan handlers.ModelExecutionChunk, 2)
	chunks <- handlers.ModelExecutionChunk{Payload: []byte("first")}
	chunks <- handlers.ModelExecutionChunk{Err: &handlers.ModelExecutionStreamError{
		Code:          "context_window_exceeded",
		StatusCode:    http.StatusBadRequest,
		Message:       providerDetail.Message,
		Retryable:     false,
		ProviderError: &providerDetail,
	}}
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"X-Stream": []string{"ok"}},
				Chunks:     chunks,
			}, nil
		},
	})

	streamID := openHostModelStreamForTest(t, host)
	readReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	rawRead, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("read callback error = %v", errRead)
	}
	first, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode read response: %v", errDecode)
	}
	if string(first.Payload) != "first" || first.Done || first.Error != "" {
		t.Fatalf("first read = %#v, want payload without done", first)
	}

	rawRead, errRead = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("terminal read callback error = %v", errRead)
	}
	terminal, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode terminal response: %v", errDecode)
	}
	if !terminal.Done || terminal.Error != providerDetail.Message || len(terminal.Payload) != 0 {
		t.Fatalf("terminal read = %#v, want done terminal error", terminal)
	}
	if terminal.ErrorDetail == nil ||
		terminal.ErrorDetail.Code != "context_window_exceeded" ||
		terminal.ErrorDetail.HTTPStatus != http.StatusBadRequest ||
		terminal.ErrorDetail.Retryable ||
		terminal.ErrorDetail.RetryAfter != "" ||
		terminal.ErrorDetail.ProviderError == nil ||
		terminal.ErrorDetail.ProviderError.Code != "context_window_exceeded" ||
		terminal.ErrorDetail.ProviderError.Class != providererror.ClassContextWindow ||
		terminal.ErrorDetail.ProviderError.Scope != providererror.ScopeRequest ||
		terminal.ErrorDetail.ProviderError.RequiredTokens != 1003466 ||
		terminal.ErrorDetail.ProviderError.LimitTokens != 1000000 {
		t.Fatalf("terminal error detail = %#v", terminal.ErrorDetail)
	}
}

func TestHostModelStreamExplicitCloseCancelsStream(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})

	streamID := openHostModelStreamForTest(t, host)
	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	closeReq, errMarshal := json.Marshal(pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamClose, closeReq); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after explicit close")
	}
	if _, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamClose, closeReq); errClose != nil {
		t.Fatalf("second close callback error = %v", errClose)
	}
}

func openHostModelStreamForTest(t *testing.T, host *Host) string {
	t.Helper()
	rawReq, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "model-1",
		Stream:        true,
		Body:          []byte(`{"stream":true}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("execute stream callback error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode stream response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}
	return resp.StreamID
}

func hostModelStreamCountForTest(t *testing.T, host *Host) int {
	t.Helper()
	host.modelStreams.mu.Lock()
	defer host.modelStreams.mu.Unlock()
	return len(host.modelStreams.streams)
}

func TestHostLogCallbackRestoresRegisteredRequestContext(t *testing.T) {
	host := New()
	ctx := logging.WithRequestID(context.Background(), "request-123")
	callbackID, closeCallback := host.openCallbackContext(ctx)
	defer closeCallback()

	var out bytes.Buffer
	logger := log.StandardLogger()
	originalOut := logger.Out
	originalFormatter := logger.Formatter
	originalLevel := logger.Level
	log.SetOutput(&out)
	log.SetFormatter(&log.TextFormatter{
		DisableColors:    true,
		DisableTimestamp: true,
	})
	log.SetLevel(log.InfoLevel)
	defer func() {
		log.SetOutput(originalOut)
		log.SetFormatter(originalFormatter)
		log.SetLevel(originalLevel)
	}()

	rawReq, errMarshal := json.Marshal(rpcHostLogRequest{
		HostCallbackID: callbackID,
		Level:          "info",
		Message:        "plugin callback message",
	})
	if errMarshal != nil {
		t.Fatalf("marshal log request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostLog, rawReq); errCall != nil {
		t.Fatalf("log callback error = %v", errCall)
	}

	got := out.String()
	if !strings.Contains(got, "plugin callback message") || !strings.Contains(got, "request_id=request-123") {
		t.Fatalf("log output = %q, want message and request_id field", got)
	}
}
