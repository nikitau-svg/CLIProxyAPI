package main

import (
	"fmt"
	"strings"
)

type executionFailureTrace struct {
	Provider string
	Model    string
	Failure  executionFailure
}

func appendExecutionFailureTrace(
	traces []executionFailureTrace,
	attempt executionAttempt,
	failure executionFailure,
) []executionFailureTrace {
	return append(traces, executionFailureTrace{
		Provider: normalizeProvider(attempt.Candidate.Provider),
		Model:    strings.TrimSpace(attempt.Candidate.Model),
		Failure:  failure,
	})
}

func executionFailureCanContinueRoute(failure executionFailure) bool {
	return failure.Retryable || failure.RouteFallback
}

func executionFailureBlocksPhysicalModel(failure executionFailure) bool {
	return failure.Code == "bravo_context_window_exceeded"
}

func executionFailureModelKey(attempt executionAttempt) string {
	return normalizeProvider(attempt.Candidate.Provider) + "\x00" +
		strings.TrimSpace(attempt.Candidate.Model)
}

func finalExecutionFailure(
	traces []executionFailureTrace,
	fallback executionFailure,
) executionFailure {
	if len(traces) == 0 {
		return fallback
	}
	if len(traces) == 1 {
		trace := traces[0]
		if summary := strings.TrimSpace(safeExecutionFailureSummary(trace)); summary != "" {
			fallback.Message = strings.TrimSuffix(summary, ".") + "."
		}
		return normalizeExhaustedRouteFailure(traces, fallback)
	}
	parts := make([]string, 0, len(traces))
	seen := make(map[string]bool, len(traces))
	for _, trace := range traces {
		summary := safeExecutionFailureSummary(trace)
		key := strings.ToLower(strings.TrimSpace(summary))
		if key != "" && !seen[key] {
			parts = append(parts, summary)
			seen[key] = true
		}
		if len(parts) == 4 {
			break
		}
	}
	if len(parts) < 2 {
		return normalizeExhaustedRouteFailure(traces, fallback)
	}
	fallback.Message = "Bravo exhausted the route: " + strings.Join(parts, "; ") + "."
	return normalizeExhaustedRouteFailure(traces, fallback)
}

func normalizeExhaustedRouteFailure(
	traces []executionFailureTrace,
	failure executionFailure,
) executionFailure {
	for _, trace := range traces {
		if !executionFailureCanContinueRoute(trace.Failure) {
			return failure
		}
	}

	// Every provider attempt failed for a reviewed transient reason. Report the
	// exhausted pool as temporarily unavailable so SDKs honor Retry-After
	// instead of immediately replaying the entire route.
	failure.Code = "bravo_route_temporarily_unavailable"
	failure.Status = 503
	failure.Retryable = true
	failure.RouteFallback = false
	failure.AccountWide = false
	failure.Provider = nil
	return failure
}

func safeExecutionFailureSummary(trace executionFailureTrace) string {
	model := strings.TrimSpace(trace.Model)
	if trace.Failure.Provider != nil {
		detail := *trace.Failure.Provider
		if strings.TrimSpace(detail.ModelDisplayName) != "" {
			model = strings.TrimSpace(detail.ModelDisplayName)
		} else if strings.TrimSpace(detail.Model) != "" {
			model = strings.TrimSpace(detail.Model)
		}
		if summary := strings.TrimSpace(detail.Summary()); summary != "" {
			if model != "" && strings.Contains(strings.ToLower(summary), strings.ToLower(model)) {
				return summary
			}
			return failureModelSummary(model, summary)
		}
	}
	switch trace.Failure.Code {
	case "bravo_context_window_exceeded":
		return failureModelSummary(model, "input exceeds the context window")
	case "bravo_subscription_model_credits_exhausted":
		return failureModelSummary(model, "model credits are exhausted")
	}
	code := strings.TrimSpace(trace.Failure.Code)
	if code == "" {
		return ""
	}
	return failureModelSummary(model, code)
}

func failureModelSummary(model, summary string) string {
	model = strings.TrimSpace(model)
	summary = strings.TrimSpace(summary)
	switch {
	case model == "":
		return summary
	case summary == "":
		return model
	default:
		return fmt.Sprintf("%s — %s", model, summary)
	}
}
