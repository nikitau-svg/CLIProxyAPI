package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		return failureEnvelope(*failure), nil
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, false)
	if errDetect != nil {
		return failureEnvelope(contractFailure(errDetect)), nil
	}
	if errPreflight := verifyLogicalModelContract(model, contract); errPreflight != nil {
		return failureEnvelope(contractFailure(errPreflight)), nil
	}
	candidateSourceBody, errStrip := stripRequestEffort(body, protocol, contract.Effort)
	if errStrip != nil {
		return failureEnvelope(executionFailure{
			Code:    "bravo_request_invalid",
			Message: errStrip.Error(),
			Status:  http.StatusBadRequest,
		}), nil
	}
	plan, errPlan := buildExecutionPlan(req, logicalName, model, contract)
	if errPlan != nil {
		return failureEnvelope(executionFailure{
			Code:      "bravo_no_eligible_account",
			Message:   errPlan.Error(),
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		}), nil
	}

	var lastFailure executionFailure
	providerCalls := 0
	for _, attempt := range plan {
		if skipCoolingExecutionAttempt(attempt, &lastFailure) {
			continue
		}
		if errPreflight := verifyCandidateContract(attempt.Candidate, contract); errPreflight != nil {
			lastFailure = contractFailure(errPreflight)
			continue
		}
		physicalModel := candidateModelName(attempt.Candidate)
		candidateBody, errRewrite := rewriteCandidateRequest(candidateSourceBody, protocol, physicalModel, false, req.Headers.Get("Content-Type"))
		if errRewrite != nil {
			return failureEnvelope(executionFailure{
				Code:    "bravo_request_invalid",
				Message: errRewrite.Error(),
				Status:  http.StatusBadRequest,
			}), nil
		}
		if providerCallBudgetExhausted(cfg.MaxAttempts, providerCalls) {
			break
		}
		providerCalls++
		started := time.Now()
		responseRaw, errCall := callHost(pluginabi.MethodHostModelCountTokens, hostModelExecutionRequest{
			HostModelExecutionRequest: nestedHostModelRequest(req, attempt, protocol, physicalModel, candidateBody, false),
			HostCallbackID:            req.HostCallbackID,
		})
		if errCall != nil {
			failure := classifyExecutionError(errCall)
			recordExecutionAttempt(attempt, started, failure.Status, false, failure)
			applyFailureCooldown(attempt, failure)
			lastFailure = failure
			if failure.Retryable {
				continue
			}
			return failureEnvelope(failure), nil
		}
		var response pluginapi.HostModelExecutionResponse
		if errDecode := json.Unmarshal(responseRaw, &response); errDecode != nil {
			lastFailure = executionFailure{
				Code:      "bravo_count_response_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
			recordExecutionAttempt(attempt, started, lastFailure.Status, false, lastFailure)
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			failure := classifyHTTPFailure(response.StatusCode, response.Headers, "token count returned an HTTP error", response.Body)
			recordExecutionAttempt(attempt, started, response.StatusCode, false, failure)
			applyFailureCooldown(attempt, failure)
			lastFailure = failure
			if failure.Retryable {
				continue
			}
			return failureEnvelope(failure), nil
		}
		response.Headers.Del("Content-Length")
		recordExecutionAttempt(attempt, started, response.StatusCode, true, executionFailure{})
		return okEnvelope(pluginapi.ExecutorResponse{
			Payload: response.Body,
			Headers: cloneHeader(response.Headers),
			Metadata: map[string]any{
				"bravo_logical_model":    clientLogicalModelID(req.Model, loadedConfig().Prefix+logicalName),
				"bravo_physical_model":   physicalModel,
				"bravo_provider":         normalizeProvider(attempt.Candidate.Provider),
				"bravo_auth_id":          pinnedAuthID(attempt.Auth),
				"bravo_effort":           attempt.Candidate.Effort,
				"bravo_requested_effort": attempt.RequestedEffort,
				"bravo_effective_effort": attempt.EffectiveEffort,
			},
		})
	}
	if lastFailure.Code == "" {
		lastFailure = executionFailure{
			Code:    "bravo_count_unavailable",
			Message: "No configured candidate can count this request.",
			Status:  http.StatusUnprocessableEntity,
		}
	}
	return failureEnvelope(lastFailure), nil
}
