package main

import (
	"net/http"
	"strings"
	"time"
)

const bravoTraceIDHeader = "X-Bravo-Trace-Id"

type routeTraceRecorder struct {
	trace    routeTrace
	finished bool
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

func (recorder *routeTraceRecorder) preflight(rejections []candidateRejection) {
	if recorder == nil {
		return
	}
	for _, rejection := range rejections {
		item := routeTraceAttempt{
			At:           recorder.trace.StartedAt,
			Provider:     rejection.Provider,
			Model:        rejection.Model,
			Status:       http.StatusServiceUnavailable,
			ErrorCode:    rejection.Code,
			ErrorMessage: rejection.Reason,
			Outcome:      "skipped",
			Decision:     "skip",
		}
		if strings.TrimSpace(rejection.AuthIndex) != "" {
			item.SubscriptionID = analyticsSubscriptionID(rejection.AuthIndex)
		}
		if rejection.AdaptiveTrace.mode != "" || rejection.AdaptiveTrace.rejectionCause != adaptiveRejectionNone {
			applyAdaptiveRouteDecision(&item, executionAttempt{AdaptiveTrace: rejection.AdaptiveTrace})
		}
		if rejection.Stage == "allocator" {
			if item.ProjectRole == "" {
				item.ProjectRole = "secondary"
			}
			if item.AllocatorMode == "" {
				item.AllocatorMode = firstNonEmpty(strings.TrimSpace(loadedConfig().AllocatorMode), "enforce")
			}
			if item.AdaptiveRejection == "" {
				switch rejection.Code {
				case "bravo_allocator_reserve_floor":
					item.AdaptiveRejection = "adaptive_secondary_floor_protected"
				default:
					item.AdaptiveRejection = "adaptive_no_compatible_fallback"
				}
			}
		}
		recorder.appendAttempt(item)
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
			previous.AdaptiveFallback = "adaptive_failover_selected"
			previous.FallbackProvider = normalizeProvider(attempt.Provider)
			previous.FallbackModel = strings.TrimSpace(attempt.Model)
		}
	}
	recorder.trace.AttemptSummary.Total++
	attempt.Ordinal = recorder.trace.AttemptSummary.Total
	if len(recorder.trace.Attempts) < maxPersistedRouteTraceAttempts {
		recorder.trace.Attempts = append(recorder.trace.Attempts, attempt)
	} else {
		// Preserve the beginning of the route and the true terminal attempt while
		// keeping per-request memory constant during pathological retry streams.
		recorder.trace.Attempts[maxPersistedRouteTraceAttempts-1] = attempt
	}
	recorder.trace.AttemptSummary.Persisted = len(recorder.trace.Attempts)
	recorder.trace.AttemptSummary.Omitted = recorder.trace.AttemptSummary.Total - recorder.trace.AttemptSummary.Persisted
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
	decision := ""
	if committed {
		decision = "stop_committed"
	}
	item := routeTraceAttempt{
		At:                         started.UTC(),
		Provider:                   normalizeProvider(attempt.Candidate.Provider),
		Model:                      strings.TrimSpace(attempt.Candidate.Model),
		SubscriptionID:             analyticsSubscriptionID(stableAuthIndex(attempt.Auth)),
		Status:                     status,
		Success:                    false,
		Outcome:                    "failed",
		Decision:                   decision,
		Committed:                  committed,
		RequestedEffort:            strings.TrimSpace(attempt.RequestedEffort),
		EffectiveEffort:            strings.TrimSpace(attempt.EffectiveEffort),
		LatencyMS:                  time.Since(started).Milliseconds(),
		ErrorCode:                  failure.Code,
		ErrorMessage:               failure.Message,
		RetryAfter:                 failure.RetryAfter,
		ProviderStarted:            cloneBoolPointer(failure.ProviderStarted),
		ProviderExecutionAmbiguous: failure.ProviderExecutionAmbiguous,
	}
	if failure.Provider != nil {
		item.ProviderErrorType = failure.Provider.Type
		item.ProviderErrorCode = failure.Provider.Code
		item.ProviderErrorScope = failure.Provider.Scope
		item.FailureClass = failure.Provider.Class
		item.RequiredInputTokens = failure.Provider.RequiredTokens
		item.SupportedInputTokens = failure.Provider.LimitTokens
	}
	applyAdaptiveRouteDecision(&item, attempt)
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
	item := routeTraceAttempt{
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
	}
	applyAdaptiveRouteDecision(&item, attempt)
	recorder.appendAttempt(item)
}

func (recorder *routeTraceRecorder) superseded(attempt executionAttempt, started time.Time) {
	if recorder == nil || recorder.finished {
		return
	}
	item := routeTraceAttempt{
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
	}
	applyAdaptiveRouteDecision(&item, attempt)
	recorder.appendAttempt(item)
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
	if success {
		_ = appendCurrentRouteTrace(recorder.trace, false)
	} else {
		// Terminal failures must survive an immediate process restart so the
		// management UI can explain the exact route that failed.
		_ = appendCurrentRouteTrace(recorder.trace, true)
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
