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
	var failureTraces []executionFailureTrace
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
		releaseLease, acquired := acquireAttemptLease(attempt)
		if !acquired {
			continue
		}
		providerCalls++
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
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, failure)
			if executionFailureBlocksPhysicalModel(failure) {
				blockedModels[executionFailureModelKey(attempt)] = true
			}
			if executionFailureCanContinueRoute(failure) {
				continue
			}
			return failureEnvelope(finalExecutionFailure(failureTraces, failure)), nil
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
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, failure)
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			failure := classifyHTTPFailure(response.StatusCode, response.Headers, "candidate returned an HTTP error", response.Body)
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
			return failureEnvelope(finalExecutionFailure(failureTraces, failure)), nil
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
	return failureEnvelope(finalExecutionFailure(failureTraces, lastFailure)), nil
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
	failure = sanitizeExecutionFailure(failure)
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
		if terminalProviderStreamErrorCode(hostErr.Code) {
			failure.Code = "bravo_provider_stream_error"
			failure.Message = "Provider returned an unrecognized structured error before completing the response."
			failure.Retryable = false
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
	return classifyProviderFailureSignal(
		failure,
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
	failure = sanitizeExecutionFailure(failure)
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
		LatencyMS:       time.Since(started).Milliseconds(),
	}
	if failure.Provider != nil {
		detail := *failure.Provider
		if strings.TrimSpace(detail.Model) == "" {
			detail.Model = strings.TrimSpace(attempt.Candidate.Model)
		}
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
