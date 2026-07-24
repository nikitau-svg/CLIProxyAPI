package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type executionFailure struct {
	Code       string
	Message    string
	Status     int
	Retryable  bool
	Headers    http.Header
	RetryAfter string
}

func execute(raw []byte) ([]byte, error) {
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

	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	var lastFailure executionFailure
	for _, attempt := range plan {
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
		releaseLease, acquired := acquireAttemptLease(attempt)
		if !acquired {
			continue
		}
		started := time.Now()
		responseRaw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
			HostModelExecutionRequest: nestedHostModelRequest(req, attempt, protocol, physicalModel, candidateBody, false),
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
			return failureEnvelope(failure), nil
		}
		// A host response means the provider accepted the attempt. Keep the
		// reservation until the next confirmed quota snapshot, even when the
		// response itself is malformed or unsuccessful.
		releaseLease(true)

		var response pluginapi.HostModelExecutionResponse
		if errDecode := json.Unmarshal(responseRaw, &response); errDecode != nil {
			failure := executionFailure{
				Code:      "bravo_host_response_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
			recordExecutionAttempt(attempt, started, failure.Status, false, failure)
			lastFailure = failure
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			failure := classifyHTTPFailure(response.StatusCode, response.Headers, "candidate returned an HTTP error")
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
			Payload: rewriteResponseModel(response.Body, physicalModel, logicalModelID),
			Headers: cloneHeader(response.Headers),
			Metadata: map[string]any{
				"bravo_logical_model":    logicalModelID,
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
			Code:    "bravo_contract_unavailable",
			Message: "No configured candidate can preserve this request contract.",
			Status:  http.StatusUnprocessableEntity,
		}
	}
	return failureEnvelope(lastFailure), nil
}

func nestedHostModelRequest(req rpcExecutorRequest, attempt executionAttempt, protocol, physicalModel string, body []byte, stream bool) pluginapi.HostModelExecutionRequest {
	protocol = normalizeContractProtocol(protocol)
	return pluginapi.HostModelExecutionRequest{
		EntryProtocol:   protocol,
		ExitProtocol:    protocol,
		ForcedProvider:  normalizeProvider(attempt.Candidate.Provider),
		AuthID:          pinnedAuthID(attempt.Auth),
		SingleAttempt:   true,
		AllowImageModel: protocol == protocolOpenAIImage,
		Model:           physicalModel,
		Stream:          stream,
		Body:            body,
		Headers:         sanitizedNestedHeaders(req.Headers),
		Query:           sanitizedNestedQuery(req.Query),
		Alt:             req.Alt,
	}
}

func prepareBravoExecution(req rpcExecutorRequest) (string, logicalModel, pluginConfig, *executionFailure) {
	cfg := loadedConfig()
	if !cfg.Enabled {
		return "", logicalModel{}, cfg, &executionFailure{
			Code:    "bravo_disabled",
			Message: "Bravo is disabled.",
			Status:  http.StatusServiceUnavailable,
		}
	}
	logicalName, model, ok := resolveLogicalModel(cfg, req.Model)
	if !ok {
		logicalName, model, ok = resolveUnprefixedLogicalModel(cfg, req.Model)
	}
	if !ok {
		return "", logicalModel{}, cfg, &executionFailure{
			Code:    "bravo_model_unknown",
			Message: "Unknown Bravo logical model.",
			Status:  http.StatusNotFound,
		}
	}
	if cfg.RequireSmartKey {
		key, authenticated := smartKeyFromMetadata(req.Metadata, cfg)
		if !authenticated {
			if plaintext := requestCredential(req.Headers, req.Query); plaintext != "" {
				key, authenticated = matchSmartKey(cfg, plaintext)
			}
		}
		if !authenticated {
			return "", logicalModel{}, cfg, &executionFailure{
				Code:    "bravo_smart_key_required",
				Message: "This Bravo model requires a Bravo smart key.",
				Status:  http.StatusUnauthorized,
			}
		}
		if !smartKeyAllowsModel(key, logicalName) {
			return "", logicalModel{}, cfg, &executionFailure{
				Code:    "bravo_model_forbidden",
				Message: "This Bravo smart key cannot use the requested logical model.",
				Status:  http.StatusForbidden,
			}
		}
	}
	return logicalName, model, cfg, nil
}

func clientLogicalModelID(requested, fallback string) string {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested
	}
	return fallback
}

func requestProtocol(req pluginapi.ExecutorRequest) string {
	for _, value := range []string{req.SourceFormat, req.Format} {
		if normalized := normalizeContractProtocol(value); normalized != "" {
			return normalized
		}
	}
	return strings.ToLower(strings.TrimSpace(req.SourceFormat))
}

func failureEnvelope(failure executionFailure) []byte {
	status := failure.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	code := strings.TrimSpace(failure.Code)
	if code == "" {
		code = "bravo_execution_failed"
	}
	message := strings.TrimSpace(failure.Message)
	if message == "" {
		message = http.StatusText(status)
	}
	return detailedErrorEnvelope(envelopeError{
		Code:       code,
		Message:    message,
		Retryable:  failure.Retryable,
		HTTPStatus: status,
		Headers:    cloneHeader(failure.Headers),
		RetryAfter: strings.TrimSpace(failure.RetryAfter),
	})
}

func contractFailure(err error) executionFailure {
	var typed *capabilityContractError
	if errors.As(err, &typed) {
		return executionFailure{
			Code:    typed.Code,
			Message: typed.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}
	return executionFailure{
		Code:    "bravo_contract_unavailable",
		Message: err.Error(),
		Status:  http.StatusUnprocessableEntity,
	}
}

func classifyExecutionError(err error) executionFailure {
	var hostErr *hostCallError
	if errors.As(err, &hostErr) {
		status := hostErr.HTTPStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		retryable := hostErr.Retryable || retryableHTTPStatus(status)
		return executionFailure{
			Code:       firstNonEmpty(hostErr.Code, "bravo_host_call_failed"),
			Message:    hostErr.Message,
			Status:     status,
			Retryable:  retryable,
			Headers:    cloneHeader(hostErr.Headers),
			RetryAfter: firstNonEmpty(hostErr.RetryAfter, hostErr.Headers.Get("Retry-After")),
		}
	}
	return executionFailure{
		Code:      "bravo_host_call_failed",
		Message:   err.Error(),
		Status:    http.StatusBadGateway,
		Retryable: true,
	}
}

func classifyHTTPFailure(status int, headers http.Header, message string) executionFailure {
	return executionFailure{
		Code:       "bravo_candidate_http_error",
		Message:    fmt.Sprintf("%s (%d)", message, status),
		Status:     status,
		Retryable:  retryableHTTPStatus(status),
		Headers:    cloneHeader(headers),
		RetryAfter: headers.Get("Retry-After"),
	}
}

func retryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusConflict,
		http.StatusTooEarly,
		http.StatusTooManyRequests:
		return true
	default:
		return status >= http.StatusInternalServerError
	}
}

func applyFailureCooldown(attempt executionAttempt, failure executionFailure) {
	if !failure.Retryable {
		return
	}
	until := retryAfterTime(failure.RetryAfter, time.Now())
	if until.IsZero() {
		until = time.Now().Add(time.Duration(loadedConfig().CooldownSeconds) * time.Second)
	}
	setCooldown(attempt.Candidate.Provider, pinnedAuthID(attempt.Auth), failure.Code, until)
}

func retryAfterTime(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if seconds, errParse := strconv.Atoi(value); errParse == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if parsed, errParse := http.ParseTime(value); errParse == nil {
		return parsed
	}
	return time.Time{}
}

func recordExecutionAttempt(attempt executionAttempt, started time.Time, status int, success bool, failure executionFailure) {
	appendAttempt(attemptRecord{
		At:              started.UTC(),
		LogicalModel:    attempt.LogicalModel,
		Provider:        normalizeProvider(attempt.Candidate.Provider),
		Model:           attempt.Candidate.Model,
		Effort:          attempt.Candidate.Effort,
		RequestedEffort: attempt.RequestedEffort,
		EffectiveEffort: attempt.EffectiveEffort,
		AuthID:          pinnedAuthID(attempt.Auth),
		AuthLabel:       firstNonEmpty(attempt.Auth.Label, attempt.Auth.Email, attempt.Auth.Name),
		Status:          status,
		Success:         success,
		Retryable:       failure.Retryable,
		ErrorCode:       failure.Code,
		Error:           failure.Message,
		LatencyMS:       time.Since(started).Milliseconds(),
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
