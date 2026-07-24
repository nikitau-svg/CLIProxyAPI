package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return failureEnvelope(executionFailure{
			Code:    "bravo_stream_id_required",
			Message: "stream_id is required for executor.execute_stream",
			Status:  http.StatusBadRequest,
		}), nil
	}
	_, model, _, failure := prepareBravoExecution(req)
	if failure != nil {
		return failureEnvelope(*failure), nil
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		return failureEnvelope(contractFailure(errDetect)), nil
	}
	if errPreflight := verifyLogicalModelContract(model, contract); errPreflight != nil {
		return failureEnvelope(contractFailure(errPreflight)), nil
	}
	go runBravoStream(req, streamID)
	return okEnvelope(map[string]any{
		"headers": http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
	})
}

func runBravoStream(req rpcExecutorRequest, pluginStreamID string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			closePluginStream(pluginStreamID, fmt.Sprintf("bravo_stream_panic: %v", recovered))
		}
	}()

	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		closePluginStream(pluginStreamID, failure.Code+": "+failure.Message)
		return
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		failure := contractFailure(errDetect)
		closePluginStream(pluginStreamID, failure.Code+": "+failure.Message)
		return
	}
	candidateSourceBody, errStrip := stripRequestEffort(body, protocol, contract.Effort)
	if errStrip != nil {
		closePluginStream(pluginStreamID, "bravo_request_invalid: "+errStrip.Error())
		return
	}
	plan, errPlan := buildExecutionPlan(req, logicalName, model, contract)
	if errPlan != nil {
		closePluginStream(pluginStreamID, "bravo_no_eligible_account: "+errPlan.Error())
		return
	}
	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	var lastFailure executionFailure

	for _, attempt := range plan {
		if errPreflight := verifyCandidateContract(attempt.Candidate, contract); errPreflight != nil {
			lastFailure = contractFailure(errPreflight)
			continue
		}
		physicalModel := candidateModelName(attempt.Candidate)
		candidateBody, errRewrite := rewriteCandidateRequest(candidateSourceBody, protocol, physicalModel, true, req.Headers.Get("Content-Type"))
		if errRewrite != nil {
			closePluginStream(pluginStreamID, "bravo_request_invalid: "+errRewrite.Error())
			return
		}
		releaseLease, acquired := acquireAttemptLease(attempt)
		if !acquired {
			continue
		}
		started := time.Now()
		rawResponse, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
			HostModelExecutionRequest: nestedHostModelRequest(req, attempt, protocol, physicalModel, candidateBody, true),
			HostCallbackID:            req.HostCallbackID,
		})
		if errCall != nil {
			releaseLease(false)
			failure := classifyExecutionError(errCall)
			recordExecutionAttempt(attempt, started, failure.Status, false, failure)
			applyFailureCooldown(attempt, failure)
			lastFailure = failure
			if failure.Retryable {
				continue
			}
			closePluginStream(pluginStreamID, failure.Code+": "+failure.Message)
			return
		}

		var response pluginapi.HostModelStreamResponse
		if errDecode := json.Unmarshal(rawResponse, &response); errDecode != nil {
			releaseLease(true)
			lastFailure = executionFailure{
				Code:      "bravo_host_stream_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
			recordExecutionAttempt(attempt, started, lastFailure.Status, false, lastFailure)
			continue
		}
		if strings.TrimSpace(response.StreamID) == "" {
			releaseLease(true)
			lastFailure = executionFailure{
				Code:      "bravo_host_stream_missing",
				Message:   "host returned an empty stream_id",
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
			recordExecutionAttempt(attempt, started, lastFailure.Status, false, lastFailure)
			continue
		}

		committed, streamFailure := forwardCandidateStream(
			context.Background(),
			response.StreamID,
			pluginStreamID,
			protocol,
			physicalModel,
			logicalModelID,
		)
		_ = closeHostModelStream(response.StreamID)
		releaseLease(true)
		if streamFailure == nil {
			recordExecutionAttempt(attempt, started, http.StatusOK, true, executionFailure{})
			closePluginStream(pluginStreamID, "")
			return
		}
		recordExecutionAttempt(attempt, started, streamFailure.Status, false, *streamFailure)
		applyFailureCooldown(attempt, *streamFailure)
		if committed {
			// Switching provider after the client observed bytes would splice two
			// unrelated streams. Fail closed and let the caller retry explicitly.
			closePluginStream(pluginStreamID, streamFailure.Code+": "+streamFailure.Message)
			return
		}
		lastFailure = *streamFailure
		if !streamFailure.Retryable {
			closePluginStream(pluginStreamID, streamFailure.Code+": "+streamFailure.Message)
			return
		}
	}

	if lastFailure.Code == "" {
		lastFailure = executionFailure{
			Code:    "bravo_contract_unavailable",
			Message: "No configured candidate can preserve this streaming request contract.",
			Status:  http.StatusUnprocessableEntity,
		}
	}
	closePluginStream(pluginStreamID, lastFailure.Code+": "+lastFailure.Message)
}

func forwardCandidateStream(ctx context.Context, hostStreamID, pluginStreamID, protocol, physicalModel, logicalModel string) (bool, *executionFailure) {
	rewriter := streamModelRewriter{
		physical: physicalModel,
		logical:  logicalModel,
		protocol: protocol,
	}
	committed := false
	for {
		rawChunk, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{
			StreamID: hostStreamID,
		})
		if errRead != nil {
			failure := classifyExecutionError(errRead)
			return committed, &failure
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if errDecode := json.Unmarshal(rawChunk, &chunk); errDecode != nil {
			return committed, &executionFailure{
				Code:      "bravo_host_stream_chunk_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: !committed,
			}
		}
		if chunk.ErrorDetail != nil || strings.TrimSpace(chunk.Error) != "" {
			failure := streamChunkFailure(chunk)
			if committed {
				failure.Retryable = false
			}
			return committed, &failure
		}
		for _, payload := range rewriter.Push(chunk.Payload) {
			if len(payload) == 0 {
				continue
			}
			if errEmit := emitPluginStreamChunk(ctx, pluginStreamID, payload); errEmit != nil {
				failure := classifyExecutionError(errEmit)
				failure.Retryable = false
				return committed, &failure
			}
			committed = true
		}
		if chunk.Done {
			if tail := rewriter.Flush(); len(tail) > 0 {
				if errEmit := emitPluginStreamChunk(ctx, pluginStreamID, tail); errEmit != nil {
					failure := classifyExecutionError(errEmit)
					failure.Retryable = false
					return committed, &failure
				}
				committed = true
			}
			return committed, nil
		}
	}
}

func streamChunkFailure(chunk pluginapi.HostModelStreamReadResponse) executionFailure {
	if detail := chunk.ErrorDetail; detail != nil {
		failure := executionFailure{
			Code:       firstNonEmpty(detail.Code, "bravo_host_stream_error"),
			Message:    firstNonEmpty(detail.Message, chunk.Error),
			Status:     firstPositive(detail.HTTPStatus, http.StatusBadGateway),
			Retryable:  detail.Retryable || retryableHTTPStatus(detail.HTTPStatus),
			Headers:    cloneHeader(detail.Headers),
			RetryAfter: firstNonEmpty(detail.RetryAfter, detail.Headers.Get("Retry-After")),
		}
		return classifyProviderFailureSignal(failure, detail.Code, detail.Message, chunk.Error)
	}
	return classifyProviderFailureSignal(executionFailure{
		Code:      "bravo_host_stream_error",
		Message:   chunk.Error,
		Status:    http.StatusBadGateway,
		Retryable: true,
	}, chunk.Error)
}

func emitPluginStreamChunk(ctx context.Context, streamID string, payload []byte) error {
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errorMessage string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errorMessage),
	})
}

func closeHostModelStream(streamID string) error {
	_, errCall := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{
		StreamID: streamID,
	})
	return errCall
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
