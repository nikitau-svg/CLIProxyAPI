package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// creditsRequiredMinimumProbeInterval prevents a provider-confirmed monthly
// model-spend restriction from being probed every generic 30-second cooldown.
// It is only a fallback when the upstream did not provide a valid Retry-After;
// explicit provider guidance, including a shorter interval, remains authoritative.
const creditsRequiredMinimumProbeInterval = 15 * time.Minute

type executionFailure struct {
	Code          string
	Message       string
	Status        int
	Retryable     bool
	RouteFallback bool
	AccountWide   bool
	Headers       http.Header
	RetryAfter    string
	Provider      *providererror.Detail
}

func execute(raw []byte) ([]byte, error) {
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
	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	routeTrace.setLogicalModel(logicalModelID)
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
		return failureEnvelopeWithRouteTrace(routeTrace, failure), nil
	}

	if len(plan) > 0 {
		routeTrace.preflight(plan[0].PreflightRejections)
	}
	var lastFailure executionFailure
	failureTraces := initialExecutionFailureTraces(plan)
	blockedModels := make(map[string]bool)
	contextRouting := newContextRoutingState(req.HostCallbackID)
	providerCalls := 0
	adaptiveLastChanceAdded := false
	for attemptIndex := 0; attemptIndex < len(plan); attemptIndex++ {
		attempt := plan[attemptIndex]
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
		if contextRouting.active() && !contextRouting.proveCandidate(
			req,
			attempt,
			protocol,
			physicalModel,
			candidateBody,
		) {
			routeTrace.failure(attempt, time.Now(), http.StatusUnprocessableEntity, executionFailure{
				Code:    "bravo_context_target_incompatible",
				Message: "Целевая модель не прошла доказательную проверку вместимости контекста.",
				Status:  http.StatusUnprocessableEntity,
			})
			continue
		}
		if providerCallBudgetExhausted(cfg.MaxAttempts, providerCalls) {
			break
		}
		releaseLease, acquired, leaseFailure := acquireExecutionAttemptLease(attempt)
		if leaseFailure != nil {
			if !adaptiveLastChanceAdded && adaptiveBreakerLastChanceEligible(attempt, *leaseFailure) {
				// Keep the exact, already validated attempt at the tail of the
				// request plan. This is a synchronous fail-open: it spends no
				// provider-call budget unless every ordinary neighbor failed.
				plan = append(plan, adaptiveBreakerLastChanceAttempt(attempt))
				adaptiveLastChanceAdded = true
			}
			lastFailure = *leaseFailure
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, *leaseFailure)
			routeTrace.failure(attempt, time.Now(), leaseFailure.Status, *leaseFailure)
			continue
		}
		if !acquired {
			continue
		}
		attempt.AdaptiveProviderDispatched = true
		markAllocatorBypassProbeDispatched(attempt, time.Now())
		providerCalls++
		started := time.Now()
		responseRaw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
			HostModelExecutionRequest: nestedHostModelRequest(req, attempt, protocol, physicalModel, candidateBody, false),
			HostCallbackID:            req.HostCallbackID,
		})
		if errCall != nil {
			releaseLease(false)
			failure := classifyExecutionError(errCall)
			contextRouting.observeFailure(attempt, failure)
			recordExecutionAttempt(attempt, started, failure.Status, false, failure)
			routeTrace.failure(attempt, started, failure.Status, failure)
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
		// A host response means the provider accepted the attempt. Keep the
		// reservation until the next confirmed quota snapshot, even when the
		// response itself is malformed or unsuccessful.
		attempt.AdaptiveProviderAccepted = true
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
			routeTrace.failure(attempt, started, failure.Status, failure)
			lastFailure = failure
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, failure)
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			failure := classifyHTTPFailure(response.StatusCode, response.Headers, "candidate returned an HTTP error", response.Body)
			contextRouting.observeFailure(attempt, failure)
			recordExecutionAttempt(attempt, started, response.StatusCode, false, failure)
			routeTrace.failure(attempt, started, response.StatusCode, failure)
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
		routeTrace.success(attempt, started, response.StatusCode)
		responseHeaders := cloneHeader(response.Headers)
		metadata := map[string]any{
			"bravo_logical_model":    logicalModelID,
			"bravo_physical_model":   physicalModel,
			"bravo_provider":         normalizeProvider(attempt.Candidate.Provider),
			"bravo_auth_id":          pinnedAuthID(attempt.Auth),
			"bravo_effort":           attempt.Candidate.Effort,
			"bravo_requested_effort": attempt.RequestedEffort,
			"bravo_effective_effort": attempt.EffectiveEffort,
		}
		compactBypassResponseWarning(responseHeaders, metadata, attempt)
		traceID := routeTrace.finish(true, response.StatusCode, executionFailure{})
		responseHeaders = attachRouteTraceHeader(responseHeaders, traceID)
		metadata["bravo_trace_id"] = traceID
		normalizedBody := normalizeOpenAIChatJSONModeResponse(response.Body, body, protocol)
		return okEnvelope(pluginapi.ExecutorResponse{
			Payload:  rewriteResponseModel(normalizedBody, physicalModel, logicalModelID),
			Headers:  responseHeaders,
			Metadata: metadata,
		})
	}
	if lastFailure.Code == "" {
		lastFailure = executionFailure{
			Code:    "bravo_contract_unavailable",
			Message: "No configured candidate can preserve this request contract.",
			Status:  http.StatusUnprocessableEntity,
		}
	}
	return failureEnvelopeWithRouteTrace(routeTrace, finalExecutionFailureForRequest(req, failureTraces, lastFailure)), nil
}

func failureEnvelopeWithRouteTrace(recorder *routeTraceRecorder, failure executionFailure) []byte {
	status := failure.Status
	if status <= 0 {
		status = http.StatusBadGateway
	}
	traceID := recorder.finish(false, status, failure)
	failure.Headers = attachRouteTraceHeader(cloneHeader(failure.Headers), traceID)
	return failureEnvelope(failure)
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
		UsageAlias:      nestedUsageAlias(req, attempt),
		Stream:          stream,
		Body:            body,
		Headers:         sanitizedNestedHeaders(req.Headers),
		Query:           sanitizedNestedQuery(req.Query),
		Alt:             req.Alt,
	}
}

func nestedUsageAlias(req rpcExecutorRequest, attempt executionAttempt) string {
	fallback := ""
	if logicalName := strings.TrimSpace(attempt.LogicalModel); logicalName != "" {
		fallback = loadedConfig().Prefix + logicalName
	}
	return clientLogicalModelID(req.Model, fallback)
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
	failure = clientExecutionFailureRU(failure)
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
	// Non-streaming failures need the same backoff hint the streaming path
	// already synthesizes in closePluginStreamFailure. Pool exhaustion carries
	// no upstream Retry-After of its own, so without this a 503 reaches the SDK
	// bare and it retries immediately into the same exhausted pool.
	retryAfter := strings.TrimSpace(failure.RetryAfter)
	if retryAfter == "" && status == http.StatusServiceUnavailable {
		retryAfter = strconv.Itoa(defaultRetryAfterSeconds(failure))
	}
	return detailedErrorEnvelope(envelopeError{
		Code:       code,
		Message:    message,
		Retryable:  failure.Retryable,
		HTTPStatus: status,
		Headers:    cloneHeader(failure.Headers),
		RetryAfter: retryAfter,
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
		failure := executionFailure{
			Code:       firstNonEmpty(hostErr.Code, "bravo_host_call_failed"),
			Message:    hostErr.Message,
			Status:     status,
			Retryable:  hostErr.Retryable || retryableHTTPStatus(status),
			Headers:    cloneHeader(hostErr.Headers),
			RetryAfter: firstNonEmpty(hostErr.RetryAfter, hostErr.Headers.Get("Retry-After")),
		}
		if failure.Code == "request_canceled" {
			// Cancellation belongs to the client request, not to a provider or
			// subscription. Never retry it and never park an otherwise healthy
			// credential in cooldown.
			failure.Status = 499
			failure.Retryable = false
			failure.AccountWide = false
			failure.RetryAfter = ""
			return failure
		}
		if localHostFailureCode(failure.Code) {
			// Callback/bridge failures are local request or ownership failures.
			// Another provider cannot repair them, and they say nothing about
			// provider health.
			failure.Retryable = false
			failure.AccountWide = false
			failure.RetryAfter = ""
			return failure
		}
		if hostErr.ProviderError != nil {
			return classifyProviderFailureDetail(failure, *hostErr.ProviderError)
		}
		if terminalProviderStreamErrorCode(hostErr.Code) {
			failure.Code = "bravo_provider_stream_error"
			failure.Message = "Provider returned an unrecognized structured error before completing the response."
			failure.Retryable = false
		}
		return classifyProviderFailureSignal(failure, hostErr.Code, hostErr.Message)
	}
	return executionFailure{
		Code:    "bravo_host_call_failed",
		Message: err.Error(),
		Status:  http.StatusBadGateway,
	}
}

func classifyProviderFailureDetail(failure executionFailure, detail providererror.Detail) executionFailure {
	detail = providererror.Sanitize(detail)
	if detail.TaxonomyVersion == providererror.FailureTaxonomyV1 {
		return classifyTaxonomyV1ProviderFailure(failure, detail)
	}
	if strings.EqualFold(strings.TrimSpace(detail.Class), "context_window") ||
		strings.EqualFold(strings.TrimSpace(detail.Code), "context_window_exceeded") ||
		providerContextWindowSignal(detail.Type, detail.Code, detail.Message, detail.Reason) {
		contextFailure := contextExecutionFailure(detail)
		contextFailure.Headers = cloneHeader(failure.Headers)
		contextFailure.RetryAfter = failure.RetryAfter
		return contextFailure
	}
	if strings.EqualFold(strings.TrimSpace(detail.Code), "credits_required") &&
		(strings.EqualFold(strings.TrimSpace(detail.Type), "rate_limit_error") ||
			strings.TrimSpace(detail.Type) == "") {
		failure.Code = "bravo_subscription_model_credits_exhausted"
		failure.Message = detail.Summary()
		failure.Retryable = true
		failure.AccountWide = false
		failure.Provider = &detail
		return sanitizeExecutionFailure(failure)
	}
	values := []string{
		detail.Type,
		detail.Code,
		detail.Message,
		detail.Model,
		detail.ModelDisplayName,
		detail.NoticeTitle,
		detail.NoticeText,
		detail.DisabledReason,
		detail.Reason,
	}
	if providerContextWindowSignal(values...) {
		return classifyProviderFailureSignal(failure, values...)
	}
	failure = classifyProviderQuotaSignal(failure, values...)
	if failure.Code == "bravo_subscription_quota_exhausted" {
		failure.Provider = &detail
		return sanitizeExecutionFailure(failure)
	}
	if (failure.Status == http.StatusBadRequest || failure.Status == http.StatusUnprocessableEntity) &&
		providerModelEntitlementSignal(values...) {
		detail.Scope = "model"
		failure.Code = "bravo_subscription_model_unavailable"
		failure.Retryable = true
		failure.AccountWide = false
		failure.Provider = &detail
		return sanitizeExecutionFailure(failure)
	}
	if classified, ok := classifyReviewedProviderFailureDetail(failure, detail); ok {
		return sanitizeExecutionFailure(classified)
	}
	return classifyProviderFailureSignal(
		failure,
		detail.Type,
		detail.Code,
		detail.Message,
		detail.Model,
		detail.ModelDisplayName,
		detail.NoticeTitle,
		detail.NoticeText,
		detail.DisabledReason,
		detail.Reason,
	)
}

func classifyTaxonomyV1ProviderFailure(failure executionFailure, detail providererror.Detail) executionFailure {
	failure.Provider = &detail
	failure.AccountWide = detail.Scope == providererror.ScopeAccount
	failure.Message = firstNonEmpty(detail.Summary(), failure.Message)
	switch detail.Class {
	case providererror.ClassContextWindow:
		classified := contextExecutionFailure(detail)
		classified.Headers = cloneHeader(failure.Headers)
		classified.RetryAfter = failure.RetryAfter
		return classified
	case providererror.ClassQuota, providererror.ClassRateLimit:
		if detail.Class == providererror.ClassQuota && detail.Scope == providererror.ScopeModel &&
			strings.EqualFold(strings.TrimSpace(detail.Code), "credits_required") {
			failure.Code = "bravo_subscription_model_credits_exhausted"
		} else {
			failure.Code = "bravo_subscription_quota_exhausted"
		}
		failure.Status = http.StatusTooManyRequests
		failure.Retryable = true
		failure.RouteFallback = true
	case providererror.ClassInvalidRequest:
		failure.Code = "bravo_provider_invalid_request"
		failure.Status = http.StatusBadRequest
		failure.Retryable = false
		failure.RouteFallback = ambiguousTaxonomyV1InvalidRequest(detail)
		if failure.RouteFallback {
			failure.Code = "bravo_provider_ambiguous_invalid_request"
		} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(detail.Code)), "invalid_") ||
			preciseProviderRequestSignal(detail.Code) {
			// Keep reviewed, safe parameter diagnostics such as
			// invalid_parameter/invalid_tool_parameters without allowing a
			// contradictory legacy class signal to steer classification.
			failure.Code = detail.Code
		}
		failure.AccountWide = false
		failure.RetryAfter = ""
	case providererror.ClassPayloadTooLarge:
		failure.Code, failure.Status = "bravo_provider_payload_too_large", http.StatusRequestEntityTooLarge
		failure.Retryable, failure.RouteFallback = false, false
	case providererror.ClassAuthentication:
		failure.Code, failure.Status = "bravo_provider_authentication_failed", http.StatusUnauthorized
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassPermission:
		failure.Code, failure.Status = "bravo_provider_permission_denied", http.StatusForbidden
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassBilling:
		failure.Code, failure.Status = "bravo_provider_billing_failed", http.StatusPaymentRequired
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassNotFound:
		failure.Code, failure.Status = "bravo_provider_not_found", http.StatusNotFound
		failure.Retryable, failure.RouteFallback = false, false
	case providererror.ClassConflict:
		failure.Code, failure.Status = "bravo_provider_conflict", http.StatusConflict
		failure.Retryable, failure.RouteFallback = false, false
	case providererror.ClassTimeout:
		failure.Code, failure.Status = "bravo_provider_timeout", http.StatusGatewayTimeout
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassOverloaded:
		failure.Code, failure.Status = "bravo_provider_overloaded", http.StatusServiceUnavailable
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassProviderInternal:
		failure.Code, failure.Status = "bravo_provider_internal", http.StatusBadGateway
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassTransport:
		failure.Code, failure.Status = "bravo_provider_transport", http.StatusBadGateway
		failure.Retryable, failure.RouteFallback = true, true
	case providererror.ClassCanceled:
		failure.Code, failure.Status = "request_canceled", 499
		failure.Retryable, failure.RouteFallback = false, false
		failure.AccountWide, failure.RetryAfter = false, ""
	}
	return sanitizeExecutionFailure(failure)
}

// TaxonomyV1 already established the failure class, so contradictory legacy
// Type text cannot decide whether an invalid request is candidate-local. Only
// a genuinely generic rejection may fall through to a neighboring provider;
// a stable code, reviewed reason, parameter, or precise safe message is a
// terminal client-contract failure.
func ambiguousTaxonomyV1InvalidRequest(detail providererror.Detail) bool {
	if detail.TaxonomyVersion != providererror.FailureTaxonomyV1 ||
		detail.Class != providererror.ClassInvalidRequest ||
		strings.TrimSpace(detail.Parameter) != "" ||
		strings.TrimSpace(detail.Reason) != "" ||
		preciseProviderRequestSignal(detail.Message) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(detail.Code)) {
	case "", "invalid_request", "invalid_request_error", "bad_request_error":
		return true
	default:
		return false
	}
}

func classifyReviewedProviderFailureDetail(
	failure executionFailure,
	detail providererror.Detail,
) (executionFailure, bool) {
	errorType := strings.ToLower(strings.TrimSpace(detail.Type))
	if errorType == "" {
		errorType = strings.ToLower(strings.TrimSpace(detail.Code))
	}

	switch errorType {
	case "authentication_error":
		detail.Scope = "account"
		failure.Status = firstPositive(failure.Status, http.StatusUnauthorized)
		failure.Retryable = true
		failure.AccountWide = true
	case "billing_error":
		detail.Scope = "account"
		failure.Status = firstPositive(failure.Status, http.StatusPaymentRequired)
		failure.Retryable = true
		failure.AccountWide = true
	case "permission_error":
		detail.Scope = "account"
		failure.Status = firstPositive(failure.Status, http.StatusForbidden)
		failure.Retryable = true
		failure.AccountWide = true
	case "usage_limit_reached":
		detail.Scope = "account"
		failure.Status = firstPositive(failure.Status, http.StatusTooManyRequests)
		failure.Retryable = true
		failure.AccountWide = true
	case "rate_limit_error", "api_error", "server_error", "upstream_error",
		"timeout_error", "overloaded_error":
		detail.Scope = "model"
		failure.Retryable = true
		failure.AccountWide = false
	case "invalid_request_error", "bad_request_error", "not_found_error",
		"conflict_error", "request_too_large":
		if ambiguousProviderInvalidRequest(detail) {
			detail.Scope = "candidate"
			failure.Code = "bravo_provider_ambiguous_invalid_request"
			failure.Retryable = false
			failure.RouteFallback = true
			failure.AccountWide = false
			failure.RetryAfter = ""
			failure.Provider = &detail
			return failure, true
		}
		detail.Scope = "request"
		failure.Retryable = false
		failure.RouteFallback = false
		failure.AccountWide = false
		failure.RetryAfter = ""
	default:
		return failure, false
	}

	failure.Code = firstNonEmpty(detail.Code, detail.Type, failure.Code)
	failure.Message = firstNonEmpty(detail.Message, failure.Message)
	failure.Provider = &detail
	return failure, true
}

// An upstream can reject a provider-specific representation even after Bravo
// has validated the logical request. A generic invalid_request_error without a
// stable code or parameter therefore belongs to this candidate, not to the
// whole logical route. Continue to another compatible provider without putting
// the account into cooldown. Precise parameter/schema failures remain terminal.
func ambiguousProviderInvalidRequest(detail providererror.Detail) bool {
	errorType := strings.ToLower(strings.TrimSpace(detail.Type))
	if errorType == "" {
		errorType = strings.ToLower(strings.TrimSpace(detail.Code))
	}
	switch errorType {
	case "invalid_request_error", "bad_request_error":
	default:
		return false
	}
	if strings.TrimSpace(detail.Parameter) != "" {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(detail.Code))
	if code != "" && code != "invalid_request_error" && code != "bad_request_error" {
		return false
	}
	return !preciseProviderRequestSignal(detail.Message, detail.Reason)
}

func preciseProviderRequestSignal(values ...string) bool {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		for _, signal := range []string{
			"max_tokens",
			"max output tokens",
			"response_format",
			"json schema",
			"tool_choice",
			"unknown tool",
			"tool_use_id",
			"invalid json",
			"malformed",
			"missing required",
			"must be greater than",
			"must be less than",
			"must not exceed",
		} {
			if strings.Contains(value, signal) {
				return true
			}
		}
	}
	return false
}

func localHostFailureCode(code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "host_call_failed",
		"host_callback_empty",
		"host_callback_code",
		"bravo_host_call_failed":
		return true
	default:
		return strings.HasPrefix(code, "host_callback_")
	}
}

func classifyHTTPFailure(status int, headers http.Header, message string, responseBody ...[]byte) executionFailure {
	failure := executionFailure{
		Code:       "bravo_candidate_http_error",
		Message:    fmt.Sprintf("%s (%d)", message, status),
		Status:     status,
		Retryable:  retryableHTTPStatus(status),
		Headers:    cloneHeader(headers),
		RetryAfter: headers.Get("Retry-After"),
	}
	for _, body := range responseBody {
		failure = classifyProviderFailureSignal(failure, string(body))
	}
	return failure
}

// Authentication and access failures returned by an upstream model call are
// scoped to the pinned subscription. Bravo can safely continue with the next
// credential/provider because the client request has already passed the local
// request-contract checks. A provider-side HTTP 400 or 422 remains terminal
// unless it carries one of the reviewed quota or model-entitlement signals
// below.
func classifyProviderFailureSignal(failure executionFailure, values ...string) (classified executionFailure) {
	defer func() {
		classified = sanitizeExecutionFailure(classified)
	}()
	if detail, ok := creditsRequiredProviderDetail(values...); ok {
		failure.Code = "bravo_subscription_model_credits_exhausted"
		failure.Message = detail.Summary()
		failure.Retryable = true
		failure.AccountWide = false
		failure.Provider = &detail
		return failure
	}
	if providerContextWindowSignal(values...) {
		failure.Code = "bravo_context_window_exceeded"
		failure.Message = "Input exceeds this model's context window."
		failure.Retryable = false
		// Context overflow is request-scoped, but a different credential cannot
		// repair it. Candidate configuration does not yet carry a verified
		// context-window size, so fail closed instead of blindly switching to an
		// equal or smaller physical model.
		failure.RouteFallback = false
		failure.AccountWide = false
		failure.Provider = nil
		return failure
	}

	switch failure.Status {
	case http.StatusUnauthorized:
		failure.Code = "bravo_subscription_auth_unavailable"
		failure.Retryable = true
		return failure
	case http.StatusForbidden:
		failure.Code = "bravo_subscription_access_denied"
		failure.Retryable = true
		return failure
	}

	failure = classifyProviderQuotaSignal(failure, values...)
	if failure.Code == "bravo_subscription_quota_exhausted" {
		return failure
	}
	if (failure.Status == http.StatusBadRequest || failure.Status == http.StatusUnprocessableEntity) &&
		providerModelEntitlementSignal(values...) {
		failure.Code = "bravo_subscription_model_unavailable"
		failure.Retryable = true
		return failure
	}

	// Some host implementations return the provider response as an ordinary
	// HTTP body instead of attaching ProviderError to the callback error. Apply
	// the same reviewed classification in both forms so routing does not depend
	// on which host boundary happened to carry the diagnostic.
	for _, value := range values {
		value = strings.TrimSpace(value)
		rawCarriesPreciseRequestSignal := preciseProviderRequestSignal(value)
		if classification, ok := providererror.ParseOpenAIStandard(failure.Status, value); ok {
			if !(rawCarriesPreciseRequestSignal && ambiguousProviderInvalidRequest(classification.Detail)) {
				classified := failure
				classified.Status = firstPositive(classification.Status, failure.Status)
				classified.Retryable = classification.Retryable
				if reviewed, accepted := classifyReviewedProviderFailureDetail(classified, classification.Detail); accepted {
					return reviewed
				}
			}
		}
		if classification, ok := providererror.ParseAnthropicStandard(value); ok {
			// Older host parsers discarded the provider-authored message without
			// retaining a reviewed parameter. Keep the raw-signal fail-closed guard
			// only while the safe classification is still ambiguous. A parser that
			// retained a reviewed code/parameter can cross the boundary directly.
			preserveRawPreciseSignal := classification.Detail.Class == providererror.ClassInvalidRequest &&
				rawCarriesPreciseRequestSignal &&
				ambiguousProviderInvalidRequest(classification.Detail)
			if !preserveRawPreciseSignal {
				classified := failure
				classified.Status = firstPositive(classification.Status, failure.Status)
				classified.Retryable = classification.Retryable
				if reviewed, accepted := classifyReviewedProviderFailureDetail(classified, classification.Detail); accepted {
					return reviewed
				}
			}
		}
	}
	return failure
}

func sanitizeExecutionFailure(failure executionFailure) executionFailure {
	if failure.Provider != nil {
		detail := providererror.Sanitize(*failure.Provider)
		failure.Provider = &detail
		if summary := strings.TrimSpace(detail.Summary()); summary != "" {
			failure.Message = summary
		}
	}
	message := strings.TrimSpace(failure.Message)
	safeMessage := providererror.Sanitize(providererror.Detail{Message: message}).Message
	if safeMessage == "" || structuredExecutionDiagnostic(message) {
		failure.Message = genericExecutionFailureMessage(failure.Code)
		return failure
	}
	failure.Message = safeMessage
	return failure
}

func structuredExecutionDiagnostic(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, "{}") || strings.HasPrefix(value, "[") {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"request_id",
		"request-id",
		"authorization",
		"bearer ",
		"api_key",
		"api-key",
		"access_token",
		"refresh_token",
		"payment_method",
		"payment-method",
		"cookie:",
		"set-cookie",
		"sk-",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func genericExecutionFailureMessage(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "bravo_subscription_quota_exhausted":
		return "The selected subscription has reached its provider usage limit."
	case "bravo_subscription_model_unavailable":
		return "The requested model is unavailable on the selected subscription."
	case "bravo_subscription_model_credits_exhausted":
		return "The selected model has reached its provider spending limit."
	case "bravo_context_window_exceeded":
		return "Input exceeds this model's context window."
	default:
		return "The provider returned a structured diagnostic that Bravo does not expose."
	}
}

func providerContextWindowSignal(values ...string) bool {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		for _, signal := range []string{
			"prompt is too long",
			"context_length_exceeded",
			"context_window_exceeded",
			"context_too_large",
			"input exceeds the context window",
			"exceeds the context window of this model",
			"maximum context length",
		} {
			if strings.Contains(value, signal) {
				return true
			}
		}
	}
	return false
}

func creditsRequiredProviderDetail(values ...string) (providererror.Detail, bool) {
	for _, value := range values {
		if detail, ok := providererror.Parse(value); ok {
			if strings.EqualFold(strings.TrimSpace(detail.Code), "credits_required") &&
				(strings.EqualFold(strings.TrimSpace(detail.Type), "rate_limit_error") ||
					strings.TrimSpace(detail.Type) == "") {
				return detail, true
			}
		}
	}
	return providererror.Detail{}, false
}

// Some subscription-backed providers report account exhaustion as a generic
// HTTP 400 invalid_request_error. These exact account-level signals are safe to
// retry on another credential or provider; ordinary malformed-request 400s
// remain terminal so Bravo never hides a client contract error.
func classifyProviderQuotaSignal(failure executionFailure, values ...string) executionFailure {
	matched := false
	for _, value := range values {
		accountWide := providerAccountWideQuotaSignal(value)
		if accountWide {
			failure.AccountWide = true
		}
		matched = matched || accountWide || providerQuotaSignal(value)
	}
	if matched {
		failure.Code = "bravo_subscription_quota_exhausted"
		failure.Retryable = true
	}
	return failure
}

func providerQuotaSignal(value string) bool {
	if providerAccountWideQuotaSignal(value) {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, signal := range []string{
		"reached your usage limit",
		"usage limit has been reached",
	} {
		if strings.Contains(value, signal) {
			return true
		}
	}
	return false
}

func providerAccountWideQuotaSignal(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, signal := range []string{
		"out of extra usage",
		"third-party apps now draw from your extra usage",
		"extra usage is disabled",
		"extra usage limit",
	} {
		if strings.Contains(value, signal) {
			return true
		}
	}
	return false
}

// Keep this list aligned with the host's reviewed model-support classification.
// Do not add broad words such as "invalid model": malformed model fields are
// request-scoped and must remain terminal.
func providerModelEntitlementSignal(values ...string) bool {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		for _, signal := range []string{
			"model_not_supported",
			"requested model is not supported",
			"requested model is unsupported",
			"requested model is unavailable",
			"model is not supported",
			"model not supported",
			"unsupported model",
			"model unavailable",
			"not available for your plan",
			"not available for your account",
		} {
			if strings.Contains(value, signal) {
				return true
			}
		}
	}
	return false
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
	until := failureCooldownUntil(failure, time.Now())
	// Credential-level rejections and reviewed account-quota exhaustion disable
	// the account everywhere. A model rate limit or upstream fault only disables
	// the physical model that hit it.
	model := attempt.Candidate.Model
	explicitModelScope := failure.Provider != nil &&
		strings.EqualFold(strings.TrimSpace(failure.Provider.Scope), "model")
	if !explicitModelScope && (failure.AccountWide || accountWideCooldownStatus(failure.Status)) {
		model = ""
	}
	setCooldownWithProviderError(
		attempt.Candidate.Provider,
		pinnedAuthID(attempt.Auth),
		model,
		failure.Code,
		until,
		failure.Provider,
	)
}

func failureCooldownUntil(failure executionFailure, now time.Time) time.Time {
	if explicit := retryAfterTime(failure.RetryAfter, now); !explicit.IsZero() {
		return explicit
	}
	duration := time.Duration(loadedConfig().CooldownSeconds) * time.Second
	if failure.Code == "bravo_subscription_model_credits_exhausted" &&
		duration < creditsRequiredMinimumProbeInterval {
		duration = creditsRequiredMinimumProbeInterval
	}
	return now.Add(duration)
}

func skipCoolingExecutionAttempt(attempt executionAttempt, lastFailure *executionFailure) bool {
	if !cooldownActive(
		attempt.Candidate.Provider,
		pinnedAuthID(attempt.Auth),
		attempt.Candidate.Model,
		time.Now(),
	) {
		return false
	}
	if lastFailure != nil && lastFailure.Code == "" {
		*lastFailure = executionFailure{
			Code:      "bravo_subscription_cooling_down",
			Message:   "A candidate subscription entered cooldown after the execution plan was built.",
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		}
	}
	return true
}

func providerCallBudgetExhausted(maxAttempts, providerCalls int) bool {
	return maxAttempts > 0 && providerCalls >= maxAttempts
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
	observeAdaptiveEdgeGateOutcome(attempt, success, failure, adaptiveShadowNow())
	if success {
		failure = executionFailure{}
	} else {
		failure = clientExecutionFailureRU(failure)
	}
	record := attemptRecord{
		At:              started.UTC(),
		LogicalModel:    attempt.LogicalModel,
		Provider:        normalizeProvider(attempt.Candidate.Provider),
		Model:           attempt.Candidate.Model,
		Effort:          attempt.Candidate.Effort,
		RequestedEffort: attempt.RequestedEffort,
		EffectiveEffort: attempt.EffectiveEffort,
		AuthID:          pinnedAuthID(attempt.Auth),
		AuthLabel:       firstNonEmpty(attempt.Auth.Note, attempt.Auth.Label, attempt.Auth.Email, attempt.Auth.Name),
		Status:          status,
		Success:         success,
		Retryable:       failure.Retryable,
		ErrorCode:       failure.Code,
		Error:           failure.Message,
		CompactBypass:   attempt.CompactBypass,
		LatencyMS:       time.Since(started).Milliseconds(),
	}
	if failure.Provider != nil {
		detail := *failure.Provider
		if strings.TrimSpace(detail.Model) == "" {
			detail.Model = strings.TrimSpace(attempt.Candidate.Model)
		}
		record.ProviderErrorType = detail.Type
		record.ProviderErrorCode = detail.Code
		record.ProviderModel = detail.Model
		record.ProviderModelDisplayName = detail.ModelDisplayName
		record.ProviderNoticeTitle = detail.NoticeTitle
		record.ProviderNoticeText = detail.NoticeText
		record.ProviderDisabledReason = detail.DisabledReason
		record.ProviderErrorReason = detail.Reason
		record.Scope = detail.Scope
	}
	appendAttempt(record)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
