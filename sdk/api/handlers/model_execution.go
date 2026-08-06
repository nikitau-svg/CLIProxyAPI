package handlers

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"golang.org/x/net/context"
)

const (
	modelExecutionMetadataSourceKey = "source"
	modelExecutionInternalSource    = "plugin_host_model_callback"
	modelExecutionCanceledCode      = "request_canceled"
	modelExecutionCanceledStatus    = 499
)

type modelExecutionCanceledError struct {
	cause error
}

func (e *modelExecutionCanceledError) Error() string {
	if e == nil || e.cause == nil {
		return "client request was canceled"
	}
	return e.cause.Error()
}

func (e *modelExecutionCanceledError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *modelExecutionCanceledError) ErrorCode() string {
	return modelExecutionCanceledCode
}

func (e *modelExecutionCanceledError) StatusCode() int {
	return modelExecutionCanceledStatus
}

func (e *modelExecutionCanceledError) Retryable() bool {
	return false
}

func (e *modelExecutionCanceledError) IsRequestScoped() bool {
	return true
}

type modelExecutionOptions struct {
	Headers                 http.Header
	Query                   url.Values
	InternalSource          bool
	SkipInterceptorPluginID string
	SkipRouterPluginID      string
	ForcedProvider          string
	PinnedAuthID            string
	SingleAttempt           bool
	AuthSelectionModel      string
	UsageAlias              string
}

// ProtocolExecutionRequest describes a route-level model execution request with explicit protocols.
type ProtocolExecutionRequest struct {
	EntryProtocol      string
	ExitProtocol       string
	ForcedProvider     string
	AuthID             string
	SingleAttempt      bool
	AuthSelectionModel string
	Model              string
	Stream             bool
	Body               []byte
	Headers            http.Header
	Query              url.Values
	Alt                string
}

// ModelExecutionRequest describes an internal model execution request.
type ModelExecutionRequest struct {
	EntryProtocol           string
	ExitProtocol            string
	ForcedProvider          string
	AuthID                  string
	SingleAttempt           bool
	AllowImageModel         bool
	Model                   string
	UsageAlias              string
	Stream                  bool
	Body                    []byte
	Headers                 http.Header
	Query                   url.Values
	Alt                     string
	SkipInterceptorPluginID string
	SkipRouterPluginID      string
}

// ModelExecutionResponse describes a non-streaming internal model execution response.
type ModelExecutionResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ModelExecutionStream describes a streaming internal model execution response.
type ModelExecutionStream struct {
	StatusCode int
	Headers    http.Header
	Chunks     <-chan ModelExecutionChunk
}

// ModelExecutionChunk carries either a streaming payload or a terminal stream error.
type ModelExecutionChunk struct {
	Payload []byte
	Err     *ModelExecutionStreamError
}

// ModelExecutionStreamError carries a JSON-friendly terminal stream error.
type ModelExecutionStreamError struct {
	Code          string                `json:"code,omitempty"`
	StatusCode    int                   `json:"status_code"`
	Message       string                `json:"message"`
	Retryable     bool                  `json:"retryable,omitempty"`
	Headers       http.Header           `json:"headers"`
	RetryAfter    string                `json:"retry_after,omitempty"`
	ProviderError *providererror.Detail `json:"provider_error,omitempty"`
}

// Error returns the stream error message or the HTTP status text.
func (e *ModelExecutionStreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.StatusCode)
}

// ProviderErrorDetail preserves the reviewed provider diagnostic across the
// internal streaming bridge without retaining raw upstream JSON.
func (e *ModelExecutionStreamError) ProviderErrorDetail() (providererror.Detail, bool) {
	if e == nil || e.ProviderError == nil {
		return providererror.Detail{}, false
	}
	detail := providererror.Sanitize(*e.ProviderError)
	if detail.Code == "" && detail.Type == "" && detail.Message == "" {
		return providererror.Detail{}, false
	}
	return detail, true
}

// ExecuteModel executes an internal non-streaming model request.
// Host model callbacks are non-recursive for their caller: when
// skip plugin IDs are set, that plugin's interceptors and router are skipped
// for the nested model execution while other plugins may still run.
func (h *BaseAPIHandler) ExecuteModel(ctx context.Context, req ModelExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage) {
	if req.Stream {
		return ModelExecutionResponse{}, modelExecutionModeError("ExecuteModel requires Stream=false")
	}
	if errMsg := canceledModelExecutionError(ctx); errMsg != nil {
		return ModelExecutionResponse{}, errMsg
	}
	body, headers, errMsg := h.executeWithAuthManagerFormats(ctx, req.EntryProtocol, req.ExitProtocol, req.Model, cloneBytes(req.Body), req.Alt, req.AllowImageModel, modelExecutionOptions{
		Headers:                 req.Headers,
		Query:                   req.Query,
		InternalSource:          true,
		ForcedProvider:          req.ForcedProvider,
		PinnedAuthID:            req.AuthID,
		SingleAttempt:           req.SingleAttempt,
		UsageAlias:              req.UsageAlias,
		SkipInterceptorPluginID: req.SkipInterceptorPluginID,
		SkipRouterPluginID:      req.SkipRouterPluginID,
	})
	if errMsg != nil {
		return ModelExecutionResponse{}, normalizeCanceledModelExecutionError(ctx, errMsg)
	}
	return ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Body:       cloneBytes(body),
	}, nil
}

// CountModelTokens counts tokens for an internal model request using the same
// provider and auth execution controls as ExecuteModel.
func (h *BaseAPIHandler) CountModelTokens(ctx context.Context, req ModelExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage) {
	if req.Stream {
		return ModelExecutionResponse{}, modelExecutionModeError("CountModelTokens requires Stream=false")
	}
	if errMsg := canceledModelExecutionError(ctx); errMsg != nil {
		return ModelExecutionResponse{}, errMsg
	}
	body, headers, errMsg := h.executeCountWithAuthManager(ctx, req.EntryProtocol, req.Model, cloneBytes(req.Body), req.Alt, modelExecutionOptions{
		Headers:                 req.Headers,
		Query:                   req.Query,
		InternalSource:          true,
		ForcedProvider:          req.ForcedProvider,
		PinnedAuthID:            req.AuthID,
		SingleAttempt:           req.SingleAttempt,
		UsageAlias:              req.UsageAlias,
		SkipInterceptorPluginID: req.SkipInterceptorPluginID,
		SkipRouterPluginID:      req.SkipRouterPluginID,
	})
	if errMsg != nil {
		return ModelExecutionResponse{}, normalizeCanceledModelExecutionError(ctx, errMsg)
	}
	return ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Body:       cloneBytes(body),
	}, nil
}

// ExecuteModelStream executes an internal streaming model request.
// Host model callbacks are non-recursive for their caller: when
// skip plugin IDs are set, that plugin's interceptors and router are skipped
// for the nested model execution while other plugins may still run.
func (h *BaseAPIHandler) ExecuteModelStream(ctx context.Context, req ModelExecutionRequest) (ModelExecutionStream, *interfaces.ErrorMessage) {
	if !req.Stream {
		return ModelExecutionStream{}, modelExecutionModeError("ExecuteModelStream requires Stream=true")
	}
	if errMsg := canceledModelExecutionError(ctx); errMsg != nil {
		return ModelExecutionStream{}, errMsg
	}
	dataChan, headers, errChan := h.executeStreamWithAuthManagerFormats(ctx, req.EntryProtocol, req.ExitProtocol, req.Model, cloneBytes(req.Body), req.Alt, req.AllowImageModel, modelExecutionOptions{
		Headers:                 req.Headers,
		Query:                   req.Query,
		InternalSource:          true,
		ForcedProvider:          req.ForcedProvider,
		PinnedAuthID:            req.AuthID,
		SingleAttempt:           req.SingleAttempt,
		UsageAlias:              req.UsageAlias,
		SkipInterceptorPluginID: req.SkipInterceptorPluginID,
		SkipRouterPluginID:      req.SkipRouterPluginID,
	})
	chunks, errMsg := prepareModelExecutionStream(ctx, dataChan, errChan)
	if errMsg != nil {
		return ModelExecutionStream{}, normalizeCanceledModelExecutionError(ctx, errMsg)
	}
	return ModelExecutionStream{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Chunks:     chunks,
	}, nil
}

// ExecuteProtocolWithAuthManager executes a route-level non-streaming request with explicit protocols.
func (h *BaseAPIHandler) ExecuteProtocolWithAuthManager(ctx context.Context, req ProtocolExecutionRequest) (ModelExecutionResponse, *interfaces.ErrorMessage) {
	if req.Stream {
		return ModelExecutionResponse{}, modelExecutionModeError("ExecuteProtocolWithAuthManager requires Stream=false")
	}
	body, headers, errMsg := h.executeWithAuthManagerFormats(ctx, req.EntryProtocol, req.ExitProtocol, req.Model, cloneBytes(req.Body), req.Alt, false, modelExecutionOptions{
		Headers:            req.Headers,
		Query:              req.Query,
		ForcedProvider:     req.ForcedProvider,
		PinnedAuthID:       req.AuthID,
		SingleAttempt:      req.SingleAttempt,
		AuthSelectionModel: req.AuthSelectionModel,
	})
	if errMsg != nil {
		return ModelExecutionResponse{}, errMsg
	}
	return ModelExecutionResponse{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Body:       cloneBytes(body),
	}, nil
}

// ExecuteProtocolStreamWithAuthManager executes a route-level streaming request with explicit protocols.
func (h *BaseAPIHandler) ExecuteProtocolStreamWithAuthManager(ctx context.Context, req ProtocolExecutionRequest) (ModelExecutionStream, *interfaces.ErrorMessage) {
	if !req.Stream {
		return ModelExecutionStream{}, modelExecutionModeError("ExecuteProtocolStreamWithAuthManager requires Stream=true")
	}
	dataChan, headers, errChan := h.executeStreamWithAuthManagerFormats(ctx, req.EntryProtocol, req.ExitProtocol, req.Model, cloneBytes(req.Body), req.Alt, false, modelExecutionOptions{
		Headers:            req.Headers,
		Query:              req.Query,
		ForcedProvider:     req.ForcedProvider,
		PinnedAuthID:       req.AuthID,
		SingleAttempt:      req.SingleAttempt,
		AuthSelectionModel: req.AuthSelectionModel,
	})
	chunks, errMsg := prepareModelExecutionStream(ctx, dataChan, errChan)
	if errMsg != nil {
		return ModelExecutionStream{}, errMsg
	}
	return ModelExecutionStream{
		StatusCode: http.StatusOK,
		Headers:    cloneHeader(headers),
		Chunks:     chunks,
	}, nil
}

func modelExecutionModeError(message string) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(message)}
}

func canceledModelExecutionError(ctx context.Context) *interfaces.ErrorMessage {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	return &interfaces.ErrorMessage{
		StatusCode: modelExecutionCanceledStatus,
		Error:      &modelExecutionCanceledError{cause: ctx.Err()},
	}
}

func normalizeCanceledModelExecutionError(ctx context.Context, errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if canceled := canceledModelExecutionError(ctx); canceled != nil {
		return canceled
	}
	return errMsg
}

func modelExecutionResponseProtocol(entryProtocol, exitProtocol string) string {
	if exitProtocol == "" {
		return entryProtocol
	}
	return exitProtocol
}

func modelExecutionHeaders(ctx context.Context, headers http.Header) http.Header {
	if len(headers) > 0 {
		return cloneHeader(headers)
	}
	return headersFromContext(ctx)
}

// modelExecutionQuery prefers an explicitly provided query and otherwise falls
// back to the inbound query embedded in the request context. This lets model
// routers observe query parameters for plain HTTP requests even when callers
// do not populate execOptions.Query (mirrors modelExecutionHeaders).
func modelExecutionQuery(ctx context.Context, query url.Values) url.Values {
	if len(query) > 0 {
		return cloneURLValues(query)
	}
	return queryFromContext(ctx)
}

func cloneURLValues(src url.Values) url.Values {
	if src == nil {
		return nil
	}
	dst := make(url.Values, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func addModelExecutionSourceMetadata(meta map[string]any, internalSource bool) {
	if !internalSource || meta == nil {
		return
	}
	meta[modelExecutionMetadataSourceKey] = modelExecutionInternalSource
}

func modelExecutionUsageAlias(originalRequestedModel string, execOptions modelExecutionOptions) string {
	if usageAlias := strings.TrimSpace(execOptions.UsageAlias); usageAlias != "" {
		return usageAlias
	}
	return originalRequestedModel
}

func prepareModelExecutionStream(ctx context.Context, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) (<-chan ModelExecutionChunk, *interfaces.ErrorMessage) {
	pending, nextDataChan, nextErrChan, errMsg := receiveInitialModelExecutionChunk(ctx, dataChan, errChan)
	if errMsg != nil {
		return nil, errMsg
	}
	return wrapModelExecutionChunks(ctx, nextDataChan, nextErrChan, pending), nil
}

func receiveInitialModelExecutionChunk(ctx context.Context, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage) ([]ModelExecutionChunk, <-chan []byte, <-chan *interfaces.ErrorMessage, *interfaces.ErrorMessage) {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	for dataChan != nil || errChan != nil {
		select {
		case payload, ok := <-dataChan:
			if !ok {
				dataChan = nil
				if dataChan == nil && errChan == nil {
					return nil, dataChan, errChan, canceledModelExecutionError(ctx)
				}
				continue
			}
			return []ModelExecutionChunk{{Payload: cloneBytes(payload)}}, dataChan, errChan, nil
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				if dataChan == nil && errChan == nil {
					return nil, dataChan, errChan, canceledModelExecutionError(ctx)
				}
				continue
			}
			if errMsg != nil {
				return nil, dataChan, errChan, errMsg
			}
		case <-done:
			return nil, dataChan, errChan, canceledModelExecutionError(ctx)
		}
	}
	if canceled := canceledModelExecutionError(ctx); canceled != nil {
		return nil, dataChan, errChan, canceled
	}
	return nil, dataChan, errChan, nil
}

func wrapModelExecutionChunks(ctx context.Context, dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage, pending []ModelExecutionChunk) <-chan ModelExecutionChunk {
	chunks := make(chan ModelExecutionChunk)
	go func() {
		defer close(chunks)
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		for _, chunk := range pending {
			if !sendModelExecutionChunk(ctx, chunks, chunk) {
				return
			}
		}
		for dataChan != nil || errChan != nil {
			select {
			case <-done:
				return
			case payload, ok := <-dataChan:
				if !ok {
					dataChan = nil
					continue
				}
				if !sendModelExecutionChunk(ctx, chunks, ModelExecutionChunk{Payload: cloneBytes(payload)}) {
					return
				}
			case errMsg, ok := <-errChan:
				if !ok {
					errChan = nil
					continue
				}
				if errMsg != nil {
					_ = sendModelExecutionChunk(ctx, chunks, ModelExecutionChunk{Err: modelExecutionStreamErrorFromMessage(errMsg)})
					return
				}
			}
		}
	}()
	return chunks
}

func modelExecutionStreamErrorFromMessage(errMsg *interfaces.ErrorMessage) *ModelExecutionStreamError {
	if errMsg == nil {
		return nil
	}
	message := ""
	if errMsg.Error != nil {
		message = errMsg.Error.Error()
	}
	statusCode := errMsg.StatusCode
	headers := cloneHeader(errMsg.Addon)
	code := ""
	retryable := false
	retryableKnown := false

	var authErr *coreauth.Error
	if errors.As(errMsg.Error, &authErr) && authErr != nil {
		code = strings.TrimSpace(authErr.Code)
		retryable = authErr.Retryable
		retryableKnown = true
		if statusCode == 0 {
			statusCode = authErr.HTTPStatus
		}
	}
	var coded interface{ ErrorCode() string }
	if code == "" && errors.As(errMsg.Error, &coded) && coded != nil {
		code = strings.TrimSpace(coded.ErrorCode())
	}
	var statusProvider interface{ StatusCode() int }
	if statusCode == 0 && errors.As(errMsg.Error, &statusProvider) && statusProvider != nil {
		statusCode = statusProvider.StatusCode()
	}
	var retryableProvider interface{ Retryable() bool }
	if !retryableKnown && errors.As(errMsg.Error, &retryableProvider) && retryableProvider != nil {
		retryable = retryableProvider.Retryable()
		retryableKnown = true
	}
	var headerProvider interface{ Headers() http.Header }
	if errors.As(errMsg.Error, &headerProvider) && headerProvider != nil {
		headers = mergeModelExecutionErrorHeaders(headers, headerProvider.Headers())
	}
	if !retryableKnown {
		retryable = isRetryableModelExecutionStatus(statusCode)
	}
	retryAfter := strings.TrimSpace(headers.Get("Retry-After"))
	if retryAfter == "" {
		var retryAfterValueProvider interface{ RetryAfterValue() string }
		if errors.As(errMsg.Error, &retryAfterValueProvider) && retryAfterValueProvider != nil {
			retryAfter = strings.TrimSpace(retryAfterValueProvider.RetryAfterValue())
		}
	}
	if retryAfter == "" {
		var retryAfterDurationProvider interface{ RetryAfter() *time.Duration }
		if errors.As(errMsg.Error, &retryAfterDurationProvider) && retryAfterDurationProvider != nil {
			if duration := retryAfterDurationProvider.RetryAfter(); duration != nil {
				seconds := int64(math.Ceil(duration.Seconds()))
				if seconds < 0 {
					seconds = 0
				}
				retryAfter = strconv.FormatInt(seconds, 10)
			}
		}
	}
	providerDetail, hasProviderDetail := providererror.FromError(errMsg.Error)
	if hasProviderDetail {
		if providerCode := strings.TrimSpace(providerDetail.Code); providerCode != "" {
			code = providerCode
		}
		if summary := strings.TrimSpace(providerDetail.Summary()); summary != "" {
			message = summary
		}
	}
	if code == "" {
		code = "model_execution_failed"
	}
	streamErr := &ModelExecutionStreamError{
		Code:       code,
		StatusCode: statusCode,
		Message:    message,
		Retryable:  retryable,
		Headers:    headers,
		RetryAfter: retryAfter,
	}
	if hasProviderDetail {
		streamErr.ProviderError = &providerDetail
	}
	return streamErr
}

func mergeModelExecutionErrorHeaders(base, extra http.Header) http.Header {
	if len(extra) == 0 {
		return base
	}
	if base == nil {
		base = make(http.Header, len(extra))
	}
	for key, values := range extra {
		base.Del(key)
		for _, value := range values {
			base.Add(key, value)
		}
	}
	return base
}

func isRetryableModelExecutionStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func sendModelExecutionChunk(ctx context.Context, chunks chan<- ModelExecutionChunk, chunk ModelExecutionChunk) bool {
	if ctx == nil {
		chunks <- chunk
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case chunks <- chunk:
		return true
	}
}
