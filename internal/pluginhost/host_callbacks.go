package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type rpcHostHTTPRequest struct {
	HTTPClientID   string       `json:"http_client_id,omitempty"`
	HostCallbackID string       `json:"host_callback_id,omitempty"`
	Method         string       `json:"method,omitempty"`
	URL            string       `json:"url,omitempty"`
	Headers        httpHeader   `json:"headers,omitempty"`
	Body           []byte       `json:"body,omitempty"`
	Request        *httpRequest `json:"request,omitempty"`
}

type httpHeader map[string][]string

type httpRequest struct {
	Method  string     `json:"method,omitempty"`
	URL     string     `json:"url,omitempty"`
	Headers httpHeader `json:"headers,omitempty"`
	Body    []byte     `json:"body,omitempty"`
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int                         `json:"status_code"`
	Headers    httpHeader                  `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type dynamicHostCallbackEntry struct {
	host     *Host
	pluginID string
}

type hostCallbackPluginIDKey struct{}

const statusClientClosedRequest = 499

type hostCallbackScopeError struct {
	code                       string
	message                    string
	status                     int
	providerStarted            bool
	providerExecutionKnown     bool
	providerExecutionAmbiguous bool
}

func (e *hostCallbackScopeError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *hostCallbackScopeError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *hostCallbackScopeError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *hostCallbackScopeError) Retryable() bool {
	return false
}

// ProviderExecutionState lets the host callback ABI distinguish a callback
// scope rejected before the model executor from cancellation after dispatch.
// Only scope errors with explicit evidence set providerExecutionKnown.
func (e *hostCallbackScopeError) ProviderExecutionState() (started, known, ambiguous bool) {
	if e == nil {
		return false, false, false
	}
	return e.providerStarted, e.providerExecutionKnown, e.providerExecutionAmbiguous
}

func withHostCallbackPluginID(ctx context.Context, pluginID string) context.Context {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		if ctx == nil {
			return context.Background()
		}
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, hostCallbackPluginIDKey{}, pluginID)
}

func hostCallbackPluginIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	pluginID, _ := ctx.Value(hostCallbackPluginIDKey{}).(string)
	return strings.TrimSpace(pluginID)
}

func (h *Host) callFromPlugin(ctx context.Context, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostCallbackFork:
		return h.callHostCallbackFork(ctx, request)
	case pluginabi.MethodHostCallbackCommit:
		return h.callHostCallbackCommit(ctx, request)
	case pluginabi.MethodHostCallbackClose:
		return h.callHostCallbackClose(ctx, request)
	case pluginabi.MethodHostModelExecute:
		return h.callHostModelExecute(ctx, request)
	case pluginabi.MethodHostModelCountTokens:
		return h.callHostModelCountTokens(ctx, request)
	case pluginabi.MethodHostModelExecuteStream:
		return h.callHostModelExecuteStream(ctx, request)
	case pluginabi.MethodHostModelStreamRead:
		return h.callHostModelStreamRead(ctx, request)
	case pluginabi.MethodHostModelStreamClose:
		return h.callHostModelStreamClose(ctx, request)
	case pluginabi.MethodHostModelList:
		return h.callHostModelList(ctx, request)
	case pluginabi.MethodHostHTTPDo:
		return h.callHostHTTPDo(ctx, request)
	case pluginabi.MethodHostHTTPDoStream:
		return h.callHostHTTPDoStream(ctx, request)
	case pluginabi.MethodHostHTTPStreamRead:
		return h.callHostHTTPStreamRead(ctx, request)
	case pluginabi.MethodHostHTTPStreamClose:
		return h.callHostHTTPStreamClose(request)
	case pluginabi.MethodHostStreamEmit:
		return h.callHostStreamEmit(ctx, request)
	case pluginabi.MethodHostStreamClose:
		return h.callHostStreamClose(request)
	case pluginabi.MethodHostLog:
		return h.callHostLog(ctx, request)
	case pluginabi.MethodHostAuthList:
		return h.callHostAuthList(ctx, request)
	case pluginabi.MethodHostAuthGet:
		return h.callHostAuthGet(ctx, request)
	case pluginabi.MethodHostAuthGetRuntime:
		return h.callHostAuthGetRuntime(ctx, request)
	case pluginabi.MethodHostAuthQuotaGet:
		return h.callHostAuthQuotaGet(ctx, request)
	case pluginabi.MethodHostAuthSave:
		return h.callHostAuthSave(ctx, request)
	case pluginabi.MethodHostPluginConfigListMutate:
		return h.callHostPluginConfigListMutate(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported host callback %s", method)
	}
}

func (h *Host) callHostCallbackCommit(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostCallbackScopeRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback commit request: %w", errUnmarshal)
	}
	callbackID := strings.TrimSpace(req.HostCallbackID)
	if callbackID == "" {
		return nil, invalidHostCallbackScopeError()
	}
	ok, forbidden, canceled := h.commitOwnedCallbackContext(callbackID, hostCallbackPluginIDFromContext(ctx))
	if forbidden {
		return nil, forbiddenHostCallbackScopeError()
	}
	if canceled {
		return nil, canceledHostCallbackScopeError()
	}
	if !ok {
		return nil, invalidHostCallbackScopeError()
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostCallbackFork(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostCallbackScopeRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback fork request: %w", errUnmarshal)
	}
	callbackID := strings.TrimSpace(req.HostCallbackID)
	if callbackID == "" {
		return nil, invalidHostCallbackScopeError()
	}
	childID, ok, forbidden, canceled := h.forkCallbackContext(callbackID, hostCallbackPluginIDFromContext(ctx))
	if forbidden {
		return nil, forbiddenHostCallbackScopeError()
	}
	if canceled {
		return nil, canceledHostCallbackScopeError()
	}
	if !ok || childID == "" {
		return nil, invalidHostCallbackScopeError()
	}
	return marshalRPCResult(pluginapi.HostCallbackScopeResponse{HostCallbackID: childID})
}

func (h *Host) callHostCallbackClose(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostCallbackScopeRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback close request: %w", errUnmarshal)
	}
	callbackID := strings.TrimSpace(req.HostCallbackID)
	if callbackID == "" {
		return nil, invalidHostCallbackScopeError()
	}
	ok, forbidden := h.closeOwnedCallbackContext(callbackID, hostCallbackPluginIDFromContext(ctx))
	if forbidden {
		return nil, forbiddenHostCallbackScopeError()
	}
	if !ok {
		return nil, invalidHostCallbackScopeError()
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func invalidHostCallbackScopeError() error {
	return &hostCallbackScopeError{
		code:    "host_callback_invalid",
		message: "host callback context is unknown or closed",
		status:  http.StatusBadRequest,
	}
}

func forbiddenHostCallbackScopeError() error {
	return &hostCallbackScopeError{
		code:    "host_callback_forbidden",
		message: "host callback context belongs to another plugin",
		status:  http.StatusForbidden,
	}
}

func canceledHostCallbackScopeError() error {
	return &hostCallbackScopeError{
		code:                   "request_canceled",
		message:                "client request was canceled",
		status:                 statusClientClosedRequest,
		providerExecutionKnown: true,
	}
}

func (h *Host) requiredModelCallbackContext(ctx context.Context, callbackID string) (context.Context, error) {
	callbackID = strings.TrimSpace(callbackID)
	if callbackID == "" {
		if ctx == nil {
			return context.Background(), nil
		}
		return ctx, nil
	}
	callerPluginID := strings.TrimSpace(hostCallbackPluginIDFromContext(ctx))
	callbackCtx, ownerPluginID, active, canceled := h.resolveRequiredCallbackContext(callbackID)
	if ownerPluginID != "" && callerPluginID != "" && ownerPluginID != callerPluginID {
		return nil, forbiddenHostCallbackScopeError()
	}
	if canceled {
		return nil, canceledHostCallbackScopeError()
	}
	if !active || callbackCtx == nil {
		return nil, invalidHostCallbackScopeError()
	}
	return callbackCtx, nil
}

func (h *Host) callbackCallerPluginID(ctx context.Context, callbackID string) string {
	if pluginID := hostCallbackPluginIDFromContext(ctx); pluginID != "" {
		return pluginID
	}
	return h.callbackContextPluginID(callbackID)
}

func (h *Host) callHostHTTPDo(ctx context.Context, request []byte) ([]byte, error) {
	httpReq, callbackID, errDecode := decodeHostHTTPRequestWithCallbackID(request)
	if errDecode != nil {
		return nil, errDecode
	}
	ctx, errDecode = h.requiredModelCallbackContext(ctx, callbackID)
	if errDecode != nil {
		return nil, errDecode
	}
	resp, errDo := h.newHTTPClient(nil).Do(ctx, httpReq)
	if errDo != nil {
		return nil, errDo
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostHTTPDoStream(ctx context.Context, request []byte) ([]byte, error) {
	httpReq, callbackID, errDecode := decodeHostHTTPRequestWithCallbackID(request)
	if errDecode != nil {
		return nil, errDecode
	}
	ctx, errDecode = h.requiredModelCallbackContext(ctx, callbackID)
	if errDecode != nil {
		return nil, errDecode
	}
	streamCtx, cancel := context.WithCancel(ctx)
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	resp, errDo := h.newHTTPClient(nil).DoStream(streamCtx, httpReq)
	if errDo != nil {
		return nil, errDo
	}
	streamID := ""
	if h != nil && h.httpStreams != nil {
		streamID = h.httpStreams.open(resp.Chunks, cancel)
	}
	if streamID == "" {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	rawResponse, errMarshal := marshalRPCResult(rpcHostHTTPStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    httpHeader(resp.Headers),
		StreamID:   streamID,
	})
	if errMarshal != nil {
		h.httpStreams.close(streamID)
		return nil, errMarshal
	}
	transferred = true
	return rawResponse, nil
}

func (h *Host) callHostHTTPStreamRead(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostHTTPStreamReadRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream read request: %w", errUnmarshal)
	}
	if h == nil || h.httpStreams == nil {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	chunk, done, errRead := h.httpStreams.read(ctx, req.StreamID)
	if errRead != nil {
		return nil, errRead
	}
	resp := rpcHostHTTPStreamReadResponse{
		Payload: append([]byte(nil), chunk.Payload...),
		Done:    done,
	}
	if chunk.Err != nil {
		resp.Error = chunk.Err.Error()
		resp.Done = true
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostHTTPStreamClose(request []byte) ([]byte, error) {
	var req rpcHostHTTPStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream close request: %w", errUnmarshal)
	}
	if h != nil && h.httpStreams != nil {
		h.httpStreams.close(req.StreamID)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func decodeHostHTTPRequest(raw []byte) (pluginapi.HTTPRequest, error) {
	httpReq, _, errDecode := decodeHostHTTPRequestWithCallbackID(raw)
	return httpReq, errDecode
}

func decodeHostHTTPRequestWithCallbackID(raw []byte) (pluginapi.HTTPRequest, string, error) {
	var req rpcHostHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return pluginapi.HTTPRequest{}, "", fmt.Errorf("decode host http request: %w", errUnmarshal)
	}
	if req.Request != nil {
		return pluginapi.HTTPRequest{
			Method:  req.Request.Method,
			URL:     req.Request.URL,
			Headers: map[string][]string(req.Request.Headers),
			Body:    append([]byte(nil), req.Request.Body...),
		}, req.HostCallbackID, nil
	}
	return pluginapi.HTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: map[string][]string(req.Headers),
		Body:    append([]byte(nil), req.Body...),
	}, req.HostCallbackID, nil
}

func (h *Host) callHostStreamEmit(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcStreamEmitRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode stream emit request: %w", errUnmarshal)
	}
	emitCtx, errContext := h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil {
		return nil, errContext
	}
	chunk := pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), req.Payload...)}
	if req.Error != "" {
		chunk.Err = fmt.Errorf("%s", req.Error)
	}
	errEmit := h.streams.emit(emitCtx, req.StreamID, chunk)
	if emitCtx != nil && emitCtx.Err() != nil {
		return nil, canceledHostCallbackScopeError()
	}
	if errEmit != nil {
		return nil, errEmit
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostStreamClose(request []byte) ([]byte, error) {
	var req rpcStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode stream close request: %w", errUnmarshal)
	}
	h.streams.close(req.StreamID, req.Error, req.ErrorStatus, req.ErrorCode, req.RetryAfter)
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostModelExecute(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model execution request: %w", errUnmarshal)
	}
	if req.Stream {
		return nil, fmt.Errorf("host.model.execute requires stream=false")
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	var errContext error
	ctx, errContext = h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil {
		return nil, errContext
	}
	resp, errMsg := executor.ExecuteModel(ctx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID))
	if errMsg != nil {
		return nil, modelExecutionError(errMsg)
	}
	return marshalRPCResult(pluginapi.HostModelExecutionResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Headers),
		Body:       append([]byte(nil), resp.Body...),
	})
}

func (h *Host) callHostModelCountTokens(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model count tokens request: %w", errUnmarshal)
	}
	if req.Stream {
		return nil, fmt.Errorf("host.model.count_tokens requires stream=false")
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	counter, ok := executor.(modelTokenCounter)
	if !ok || counter == nil {
		return nil, fmt.Errorf("host model token counter is unavailable")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	var errContext error
	ctx, errContext = h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil {
		return nil, errContext
	}
	resp, errMsg := counter.CountModelTokens(ctx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID))
	if errMsg != nil {
		return nil, modelExecutionError(errMsg)
	}
	return marshalRPCResult(pluginapi.HostModelExecutionResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Headers),
		Body:       append([]byte(nil), resp.Body...),
	})
}

func modelExecutionRequestFromPlugin(req pluginapi.HostModelExecutionRequest, skipPluginID string) handlers.ModelExecutionRequest {
	return handlers.ModelExecutionRequest{
		EntryProtocol:           req.EntryProtocol,
		ExitProtocol:            req.ExitProtocol,
		ForcedProvider:          req.ForcedProvider,
		AuthID:                  req.AuthID,
		SingleAttempt:           req.SingleAttempt,
		AllowImageModel:         req.AllowImageModel,
		Model:                   req.Model,
		UsageAlias:              req.UsageAlias,
		Stream:                  req.Stream,
		Body:                    append([]byte(nil), req.Body...),
		Headers:                 cloneHeader(req.Headers),
		Query:                   cloneValues(req.Query),
		Alt:                     req.Alt,
		SkipInterceptorPluginID: skipPluginID,
		SkipRouterPluginID:      skipPluginID,
	}
}

func modelExecutionError(errMsg *interfaces.ErrorMessage) error {
	return newHostModelCallbackError(errMsg)
}

func (h *Host) callHostLog(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostLogRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host log request: %w", errUnmarshal)
	}
	var errContext error
	ctx, errContext = h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil {
		return nil, errContext
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		message = "plugin log"
	}
	fields := log.Fields{}
	for key, value := range req.Fields {
		key = strings.TrimSpace(key)
		if key != "" {
			fields[key] = value
		}
	}
	if requestID := logging.GetRequestID(ctx); requestID != "" {
		fields["request_id"] = requestID
	}
	entry := log.WithFields(fields)
	switch strings.ToLower(strings.TrimSpace(req.Level)) {
	case "trace":
		entry.Trace(message)
	case "info":
		entry.Info(message)
	case "warn", "warning":
		entry.Warn(message)
	case "error":
		entry.Error(message)
	default:
		entry.Debug(message)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}
