package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (h *Host) callHostModelExecuteStream(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model execution stream request: %w", errUnmarshal)
	}
	if !req.Stream {
		return nil, fmt.Errorf("host.model.execute_stream requires stream=true")
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	callbackCtx, errContext := h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil {
		return nil, errContext
	}
	// The callback scope, rather than the parent context signal alone, owns the
	// nested stream lifetime. This preserves the existing bridge handoff while
	// allowing a forked child scope to cancel one losing Bravo attempt.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(callbackCtx))
	if req.HostCallbackID != "" && !h.addCallbackCleanup(req.HostCallbackID, cancel) {
		return nil, h.callbackScopeUnavailableError(ctx, req.HostCallbackID)
	}
	stream, errMsg := executor.ExecuteModelStream(streamCtx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID))
	if errMsg != nil {
		cancel()
		return nil, modelExecutionError(errMsg)
	}
	streamID := ""
	if h.modelStreams != nil {
		streamID = h.modelStreams.open(req.HostCallbackID, stream.Chunks, cancel)
	}
	if streamID == "" {
		cancel()
		return nil, fmt.Errorf("host model stream bridge is unavailable")
	}
	if req.HostCallbackID != "" {
		if !h.addCallbackCleanup(req.HostCallbackID, func() {
			h.modelStreams.close(streamID)
		}) {
			return nil, h.callbackScopeUnavailableError(ctx, req.HostCallbackID)
		}
	}
	return marshalRPCResult(pluginapi.HostModelStreamResponse{
		StatusCode: stream.StatusCode,
		Headers:    cloneHeader(stream.Headers),
		StreamID:   streamID,
	})
}

func (h *Host) callbackScopeUnavailableError(ctx context.Context, callbackID string) error {
	if _, errContext := h.requiredModelCallbackContext(ctx, callbackID); errContext != nil {
		return errContext
	}
	return invalidHostCallbackScopeError()
}

func (h *Host) callHostModelStreamRead(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamReadRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream read request: %w", errUnmarshal)
	}
	if h == nil || h.modelStreams == nil {
		return nil, fmt.Errorf("host model stream bridge is unavailable")
	}
	readCtx := ctx
	requestedCallbackID := strings.TrimSpace(req.HostCallbackID)
	ownerCallbackID, streamExists := h.modelStreams.owner(req.StreamID)
	if streamExists && strings.TrimSpace(ownerCallbackID) != "" {
		if requestedCallbackID != "" && requestedCallbackID != ownerCallbackID {
			return nil, forbiddenHostCallbackScopeError()
		}
		var errContext error
		readCtx, errContext = h.requiredModelCallbackContext(ctx, ownerCallbackID)
		if errContext != nil {
			return nil, errContext
		}
	} else if requestedCallbackID != "" {
		var errContext error
		readCtx, errContext = h.requiredModelCallbackContext(ctx, requestedCallbackID)
		if errContext != nil {
			return nil, errContext
		}
	}
	chunk, done, errRead := h.modelStreams.read(readCtx, req.StreamID)
	if readCtx != nil && readCtx.Err() != nil {
		return nil, canceledHostCallbackScopeError()
	}
	if errRead != nil {
		return nil, errRead
	}
	resp := pluginapi.HostModelStreamReadResponse{
		Payload: append([]byte(nil), chunk.Payload...),
		Done:    done,
	}
	if chunk.Err != nil {
		resp.Error = chunk.Err.Error()
		resp.ErrorDetail = &pluginapi.HostModelExecutionError{
			Code:       chunk.Err.Code,
			Message:    chunk.Err.Error(),
			HTTPStatus: chunk.Err.StatusCode,
			Retryable:  chunk.Err.Retryable,
			Headers:    cloneHeader(chunk.Err.Headers),
			RetryAfter: chunk.Err.RetryAfter,
		}
		resp.Done = true
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostModelStreamClose(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream close request: %w", errUnmarshal)
	}
	if h != nil && h.modelStreams != nil {
		requestedCallbackID := strings.TrimSpace(req.HostCallbackID)
		ownerCallbackID, streamExists := h.modelStreams.owner(req.StreamID)
		if streamExists && strings.TrimSpace(ownerCallbackID) != "" {
			if requestedCallbackID != "" && requestedCallbackID != ownerCallbackID {
				return nil, forbiddenHostCallbackScopeError()
			}
			if _, errContext := h.requiredModelCallbackContext(ctx, ownerCallbackID); errContext != nil {
				return nil, errContext
			}
		} else if requestedCallbackID != "" {
			if _, errContext := h.requiredModelCallbackContext(ctx, requestedCallbackID); errContext != nil {
				return nil, errContext
			}
		}
		h.modelStreams.close(req.StreamID)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}
