package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	routeTrace := newRouteTraceRecorder(req, strings.TrimSpace(req.Model), protocol, false)
	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		routeTrace.preflightFailure("routing_preflight", *failure, nil)
		return failureEnvelopeWithRouteTrace(routeTrace, *failure), nil
	}
	routeTrace.setLogicalModel(clientLogicalModelID(req.Model, cfg.Prefix+logicalName))
	contract, errDetect := detectRequestContract(protocol, body, false)
	if errDetect != nil {
		failure := contractFailure(errDetect)
		routeTrace.preflightFailure("contract_detection", failure, errDetect)
		return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
	}
	if errPreflight := verifyLogicalModelContract(model, contract); errPreflight != nil {
		failure := contractFailure(errPreflight)
		routeTrace.preflightFailure("logical_contract", failure, errPreflight)
		return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
	}
	candidateSourceBody, errStrip := stripRequestEffort(body, protocol, contract.Effort)
	if errStrip != nil {
		failure := executionFailure{
			Code:    "bravo_request_invalid",
			Message: errStrip.Error(),
			Status:  http.StatusBadRequest,
		}
		routeTrace.preflightFailure("request_rewrite", failure, errStrip)
		return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
	}
	plan, errPlan := buildExecutionPlan(req, logicalName, model, contract)
	if errPlan != nil {
		failure := executionFailure{
			Code:      "bravo_no_eligible_account",
			Message:   errPlan.Error(),
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		}
		routeTrace.preflightFailure("route_planning", failure, errPlan)
		return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
	}

	var lastFailure executionFailure
	failureTraces := initialExecutionFailureTraces(plan)
	blockedModels := make(map[string]bool)
	providerCalls := 0
	for _, attempt := range plan {
		if blockedModels[executionFailureModelKey(attempt)] {
			continue
		}
		if skipCoolingExecutionAttempt(attempt, &lastFailure) {
			continue
		}
		if errPreflight := verifyCandidateContract(attempt.Candidate, contract); errPreflight != nil {
			lastFailure = contractFailure(errPreflight)
			routeTrace.failure(attempt, time.Now(), lastFailure.Status, lastFailure)
			continue
		}
		physicalModel := candidateModelName(attempt.Candidate)
		candidateBody, errRewrite := rewriteCandidateRequest(candidateSourceBody, protocol, physicalModel, false, req.Headers.Get("Content-Type"))
		if errRewrite != nil {
			failure := executionFailure{
				Code:    "bravo_request_invalid",
				Message: errRewrite.Error(),
				Status:  http.StatusBadRequest,
			}
			routeTrace.failure(attempt, time.Now(), failure.Status, failure)
			return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
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
			routeTrace.failure(attempt, started, failure.Status, failure)
			recordExecutionAttempt(attempt, started, failure.Status, false, failure)
			applyFailureCooldown(attempt, failure)
			lastFailure = failure
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, failure)
			if executionFailureBlocksPhysicalModel(failure) {
				blockedModels[executionFailureModelKey(attempt)] = true
			}
			if executionFailureCanContinueRoute(failure) {
				continue
			}
			return failureEnvelopeWithRouteTrace(routeTrace, finalExecutionFailureForRequest(req, failureTraces, failure)), nil
		}
		var response pluginapi.HostModelExecutionResponse
		if errDecode := json.Unmarshal(responseRaw, &response); errDecode != nil {
			lastFailure = executionFailure{
				Code:      "bravo_count_response_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
			routeTrace.failure(attempt, started, lastFailure.Status, lastFailure)
			recordExecutionAttempt(attempt, started, lastFailure.Status, false, lastFailure)
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, lastFailure)
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			failure := classifyHTTPFailure(response.StatusCode, response.Headers, "token count returned an HTTP error", response.Body)
			routeTrace.failure(attempt, started, response.StatusCode, failure)
			recordExecutionAttempt(attempt, started, response.StatusCode, false, failure)
			applyFailureCooldown(attempt, failure)
			lastFailure = failure
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, failure)
			if executionFailureBlocksPhysicalModel(failure) {
				blockedModels[executionFailureModelKey(attempt)] = true
			}
			if executionFailureCanContinueRoute(failure) {
				continue
			}
			return failureEnvelopeWithRouteTrace(routeTrace, finalExecutionFailureForRequest(req, failureTraces, failure)), nil
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
	return failureEnvelopeWithRouteTrace(routeTrace, finalExecutionFailureForRequest(req, failureTraces, lastFailure)), nil
}
