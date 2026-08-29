package main

import "strings"

// adaptiveBreakerLastChanceEligible deliberately recognizes only local
// breaker decisions. Provider failures must keep their normal routing and
// retry semantics, and older allocator modes must remain behaviorally stable.
func adaptiveBreakerLastChanceEligible(attempt executionAttempt, failure executionFailure) bool {
	cfg := adaptiveAttemptConfig(attempt, loadedConfig())
	if cfg.AdaptiveAllocatorMode != "breaker" {
		return false
	}
	switch failure.Code {
	case "bravo_adaptive_edge_tripped", "bravo_adaptive_edge_busy":
		return true
	default:
		return false
	}
}

// adaptiveBreakerOutwardFailures keeps a coordinator-local admission decision
// out of the public provider error. In particular, when the real-call budget
// was consumed by neighboring attempts, the skipped attempt cannot be retried
// inside this request, but it still must not masquerade as an upstream error.
func adaptiveBreakerOutwardFailures(
	traces []executionFailureTrace,
	fallback executionFailure,
) ([]executionFailureTrace, executionFailure) {
	filtered := make([]executionFailureTrace, 0, len(traces))
	for _, trace := range traces {
		if isAdaptiveBreakerLocalFailure(trace.Failure) {
			continue
		}
		filtered = append(filtered, trace)
	}
	if isAdaptiveBreakerLocalFailure(fallback) {
		fallback = executionFailure{
			Code:      "bravo_route_temporarily_unavailable",
			Message:   "Bravo исчерпал доступный маршрут; локально пропущенный кандидат не удалось выполнить в пределах лимита попыток.",
			Status:    503,
			Retryable: true,
		}
	}
	return filtered, fallback
}

func isAdaptiveBreakerLocalFailure(failure executionFailure) bool {
	return strings.HasPrefix(strings.TrimSpace(failure.Code), "bravo_adaptive_")
}

func adaptiveBreakerLastChanceAttempt(attempt executionAttempt) executionAttempt {
	if state := attempt.AdaptiveEdgeGate; state != nil {
		state.mu.RLock()
		attempt.AdaptiveBreakerRecoveryKey = state.breakerCandidateKey
		attempt.AdaptiveBreakerRecoveryGeneration = state.breakerCandidateGeneration
		attempt.AdaptiveBreakerRecoveryRevision = state.breakerCandidateRevision
		state.mu.RUnlock()
	}
	attempt.AdaptiveBreakerLastChance = true
	if attempt.AdaptiveEdgeGate != nil {
		// Do not share or read mutable state from the breaker-skipped attempt.
		// Acquisition refreshes this private state from the immutable attempt
		// and current quota snapshot before marking the baseline dispatch.
		attempt.AdaptiveEdgeGate = &adaptiveEdgeGateAttemptState{}
	}
	return attempt
}

func isAdaptiveBreakerLastChanceAttempt(attempt executionAttempt) bool {
	return attempt.AdaptiveBreakerLastChance
}
