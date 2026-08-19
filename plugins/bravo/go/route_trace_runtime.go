package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

const bravoTraceIDHeader = "X-Bravo-Trace-Id"

type routeTraceRecorder struct {
	trace                          routeTrace
	finished                       bool
	adaptiveAuditDisabled          bool
	adaptiveAuditAttempts          []adaptiveShadowAuditAttempt
	adaptiveAuditExecutionAttempts int
	adaptiveAuditOmittedAttempts   int
}

func (recorder *routeTraceRecorder) disableAdaptiveAudit() {
	if recorder == nil || recorder.finished {
		return
	}
	recorder.adaptiveAuditDisabled = true
}

func (recorder *routeTraceRecorder) captureAdaptiveAuditAttempt(
	attempt executionAttempt,
	started time.Time,
	status int,
	success bool,
	outcome string,
	errorCode string,
) {
	if recorder == nil || recorder.finished || recorder.adaptiveAuditDisabled ||
		!attempt.AdaptiveShadow || !attempt.AdaptiveProviderDispatched {
		return
	}
	recorder.adaptiveAuditExecutionAttempts++
	if len(recorder.adaptiveAuditAttempts) >= adaptiveShadowAuditAttemptsPerRecord {
		recorder.adaptiveAuditOmittedAttempts++
		return
	}
	recorder.adaptiveAuditAttempts = append(recorder.adaptiveAuditAttempts, adaptiveShadowAuditAttempt{
		Provider:                      normalizeProvider(attempt.Candidate.Provider),
		Model:                         strings.TrimSpace(attempt.Candidate.Model),
		Primary:                       attempt.Primary,
		Decision:                      attempt.AdaptiveShadowDecision,
		EstimateConfidence:            attempt.AdaptiveEstimateConfidence,
		ReservationPercent:            attempt.AdaptiveReservationPercent,
		SessionReservationPercent:     attempt.AdaptiveSessionReservationPercent,
		WeeklyReservationPercent:      attempt.AdaptiveWeeklyReservationPercent,
		ModelWeeklyReservationPercent: attempt.AdaptiveModelWeeklyReservationPercent,
		ModelWeeklyName:               attempt.AdaptiveModelWeeklyName,
		PredictedTokens:               attempt.AdaptivePredictedTokens,
		PendingPercent:                attempt.AdaptiveShadowPendingPercent,
		SafeHeadroomBefore:            attempt.AdaptiveShadowHeadroomBefore,
		SafeHeadroomAfter:             attempt.AdaptiveShadowHeadroomAfter,
		Outcome:                       outcome,
		Status:                        status,
		Success:                       success,
		ProviderAcceptance:            adaptiveShadowProviderAcceptance(attempt),
		LatencyMilliseconds:           time.Since(started).Milliseconds(),
		ErrorCode:                     errorCode,
	})
}

func adaptiveShadowProviderAcceptance(attempt executionAttempt) string {
	if attempt.AdaptiveProviderAccepted {
		return "confirmed"
	}
	return "unknown"
}

func newRouteTraceRecorder(
	req rpcExecutorRequest,
	logicalModel, protocol string,
	stream bool,
) *routeTraceRecorder {
	projectID := ""
	if project, ok := authenticatedExecutionProject(req, loadedConfig()); ok {
		projectID = project.ID
	}
	return &routeTraceRecorder{trace: routeTrace{
		TraceID:        newRouteTraceID(),
		StartedAt:      time.Now().UTC(),
		ProjectID:      projectID,
		LogicalModel:   strings.TrimSpace(logicalModel),
		SourceProtocol: normalizeContractProtocol(protocol),
		Stream:         stream,
	}}
}

func (recorder *routeTraceRecorder) setLogicalModel(logicalModel string) {
	if recorder == nil || recorder.finished {
		return
	}
	recorder.trace.LogicalModel = strings.TrimSpace(logicalModel)
}

func (recorder *routeTraceRecorder) preflightFailure(
	stage string,
	failure executionFailure,
	source error,
) {
	if recorder == nil || recorder.finished {
		return
	}
	capability := ""
	var typed *capabilityContractError
	if errors.As(source, &typed) && typed != nil {
		capability = strings.TrimSpace(typed.Capability)
	}
	recorder.appendAttempt(routeTraceAttempt{
		At:                 time.Now().UTC(),
		Status:             failure.Status,
		Success:            false,
		Outcome:            "rejected",
		Decision:           "stop",
		Committed:          false,
		ErrorCode:          failure.Code,
		ErrorMessage:       failure.Message,
		DiagnosticStage:    stage,
		RequiredCapability: capability,
		ParameterPath:      capabilityParameterPath(capability),
	})
}

func capabilityParameterPath(capability string) string {
	switch strings.TrimSpace(capability) {
	case capabilityTools, capabilityProviderTool, capabilityWebSearch,
		capabilityWebSearchFilters, capabilityImageGeneration:
		return "$.tools"
	case capabilityToolResult, capabilityVision, capabilityFileInput:
		return "$.messages"
	case capabilityReasoning:
		return "$.thinking"
	case capabilityStream:
		return "$.stream"
	case capabilityStructuredOutput:
		return "$.output_config"
	default:
		return "$"
	}
}

func (recorder *routeTraceRecorder) preflight(rejections []candidateRejection) {
	if recorder == nil {
		return
	}
	for _, rejection := range rejections {
		recorder.appendAttempt(routeTraceAttempt{
			At:           recorder.trace.StartedAt,
			Provider:     rejection.Provider,
			Model:        rejection.Model,
			Status:       http.StatusServiceUnavailable,
			ErrorCode:    rejection.Code,
			ErrorMessage: rejection.Reason,
			Outcome:      "skipped",
			Decision:     "skip",
		})
	}
}

func (recorder *routeTraceRecorder) appendAttempt(attempt routeTraceAttempt) {
	if recorder == nil || recorder.finished {
		return
	}
	if count := len(recorder.trace.Attempts); count > 0 {
		previous := &recorder.trace.Attempts[count-1]
		if previous.Outcome == "failed" && previous.Decision == "" && !previous.Committed {
			previous.Decision = "fallback"
		}
	}
	attempt.Ordinal = len(recorder.trace.Attempts) + 1
	recorder.trace.Attempts = append(recorder.trace.Attempts, attempt)
}

func (recorder *routeTraceRecorder) failure(
	attempt executionAttempt,
	started time.Time,
	status int,
	failure executionFailure,
) {
	recorder.failureWithCommit(attempt, started, status, failure, false)
}

func (recorder *routeTraceRecorder) failureWithCommit(
	attempt executionAttempt,
	started time.Time,
	status int,
	failure executionFailure,
	committed bool,
) {
	if recorder == nil || recorder.finished {
		return
	}
	recorder.captureAdaptiveAuditAttempt(attempt, started, status, false, "failed", failure.Code)
	decision := ""
	if committed {
		decision = "stop_committed"
	}
	item := routeTraceAttempt{
		At:              started.UTC(),
		Provider:        normalizeProvider(attempt.Candidate.Provider),
		Model:           strings.TrimSpace(attempt.Candidate.Model),
		SubscriptionID:  analyticsSubscriptionID(stableAuthIndex(attempt.Auth)),
		Status:          status,
		Success:         false,
		Outcome:         "failed",
		Decision:        decision,
		Committed:       committed,
		RequestedEffort: strings.TrimSpace(attempt.RequestedEffort),
		EffectiveEffort: strings.TrimSpace(attempt.EffectiveEffort),
		LatencyMS:       time.Since(started).Milliseconds(),
		ErrorCode:       failure.Code,
		ErrorMessage:    failure.Message,
		RetryAfter:      failure.RetryAfter,
	}
	if failure.Provider != nil {
		item.ProviderErrorType = failure.Provider.Type
		item.ProviderErrorCode = failure.Provider.Code
		item.ProviderErrorParam = failure.Provider.Parameter
		item.ProviderErrorScope = failure.Provider.Scope
		item.FailureClass = failure.Provider.Class
		item.RequiredInputTokens = failure.Provider.RequiredTokens
		item.SupportedInputTokens = failure.Provider.LimitTokens
	}
	recorder.appendAttempt(item)
}

func (recorder *routeTraceRecorder) success(
	attempt executionAttempt,
	started time.Time,
	status int,
) {
	if recorder == nil || recorder.finished {
		return
	}
	recorder.captureAdaptiveAuditAttempt(attempt, started, status, true, "succeeded", "")
	recorder.appendAttempt(routeTraceAttempt{
		At:              started.UTC(),
		Provider:        normalizeProvider(attempt.Candidate.Provider),
		Model:           strings.TrimSpace(attempt.Candidate.Model),
		SubscriptionID:  analyticsSubscriptionID(stableAuthIndex(attempt.Auth)),
		Status:          status,
		Success:         true,
		Outcome:         "succeeded",
		Decision:        "winner",
		Committed:       true,
		RequestedEffort: strings.TrimSpace(attempt.RequestedEffort),
		EffectiveEffort: strings.TrimSpace(attempt.EffectiveEffort),
		LatencyMS:       time.Since(started).Milliseconds(),
	})
}

func (recorder *routeTraceRecorder) superseded(attempt executionAttempt, started time.Time) {
	if recorder == nil || recorder.finished {
		return
	}
	recorder.captureAdaptiveAuditAttempt(attempt, started, 499, false, "superseded", "bravo_attempt_superseded")
	recorder.appendAttempt(routeTraceAttempt{
		At:              started.UTC(),
		Provider:        normalizeProvider(attempt.Candidate.Provider),
		Model:           strings.TrimSpace(attempt.Candidate.Model),
		SubscriptionID:  analyticsSubscriptionID(stableAuthIndex(attempt.Auth)),
		Status:          499,
		Outcome:         "superseded",
		Decision:        "superseded",
		RequestedEffort: strings.TrimSpace(attempt.RequestedEffort),
		EffectiveEffort: strings.TrimSpace(attempt.EffectiveEffort),
		LatencyMS:       time.Since(started).Milliseconds(),
		ErrorCode:       "bravo_attempt_superseded",
	})
}

func (recorder *routeTraceRecorder) finish(success bool, status int, failure executionFailure) string {
	if recorder == nil {
		return ""
	}
	if recorder.finished {
		return recorder.trace.TraceID
	}
	recorder.finished = true
	recorder.trace.CompletedAt = time.Now().UTC()
	recorder.trace.TotalLatencyMS = recorder.trace.CompletedAt.Sub(recorder.trace.StartedAt).Milliseconds()
	recorder.trace.Success = success
	if success {
		recorder.trace.Outcome = "success"
	} else if failure.Code == "request_canceled" {
		recorder.trace.Outcome = "canceled"
	} else {
		recorder.trace.Outcome = "failed"
	}
	if status <= 0 {
		if success {
			status = http.StatusOK
		} else {
			status = http.StatusBadGateway
		}
	}
	recorder.trace.Status = status
	for index := range recorder.trace.Attempts {
		attempt := &recorder.trace.Attempts[index]
		if attempt.Outcome == "failed" && attempt.Decision == "" {
			attempt.Decision = "stop"
		}
	}
	if !success {
		localized := clientExecutionFailureRU(failure)
		recorder.trace.FinalCode = localized.Code
		recorder.trace.FinalMessage = localized.Message
	}
	if !recorder.adaptiveAuditDisabled && len(recorder.adaptiveAuditAttempts) > 0 {
		enqueueAdaptiveShadowAudit(adaptiveShadowAuditRecord{
			SchemaVersion:              adaptiveShadowAuditSchemaVersion,
			At:                         recorder.trace.CompletedAt,
			TraceID:                    recorder.trace.TraceID,
			LogicalModel:               recorder.trace.LogicalModel,
			Stream:                     recorder.trace.Stream,
			Success:                    success,
			Status:                     status,
			ActualExecutionAttempts:    recorder.adaptiveAuditExecutionAttempts,
			OmittedAttempts:            recorder.adaptiveAuditOmittedAttempts,
			FallbackUsed:               recorder.adaptiveAuditExecutionAttempts > 1,
			RoutingEnforced:            false,
			AdditionalProviderRequests: 0,
			Attempts:                   recorder.adaptiveAuditAttempts,
		})
	}
	if success {
		bravoRouteTraces.append(recorder.trace)
	} else {
		// Terminal failures must survive an immediate process restart so the
		// management UI can explain the exact route that failed.
		_ = bravoRouteTraces.appendDurable(recorder.trace)
	}
	return recorder.trace.TraceID
}

func attachRouteTraceHeader(headers http.Header, traceID string) http.Header {
	if headers == nil {
		headers = make(http.Header)
	}
	if traceID = strings.TrimSpace(traceID); traceID != "" {
		headers.Set(bravoTraceIDHeader, traceID)
	}
	return headers
}
