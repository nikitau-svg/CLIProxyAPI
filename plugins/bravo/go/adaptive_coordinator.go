package main

import "strings"

// adaptiveBreakerLastChanceEligible deliberately recognizes only local
// breaker decisions. Provider failures must keep their normal routing and
// retry semantics, and older allocator modes must remain behaviorally stable.
func adaptiveBreakerLastChanceEligible(attempt executionAttempt, failure executionFailure) bool {
	cfg := adaptiveAttemptConfig(attempt, loadedConfig())
	if cfg.AdaptiveAllocatorMode != "breaker" && cfg.AdaptiveAllocatorMode != "assist" {
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

func adaptiveAssistDeferredEligible(attempt executionAttempt, failure executionFailure) bool {
	return attempt.AdaptiveAllocatorMode == "assist" && !attempt.Primary && !attempt.AdaptiveAssistTail &&
		!attempt.CompactBypass && !attempt.AllocatorBypass && !attempt.AdaptiveBreakerLastChance &&
		failure.Code == "bravo_adaptive_quota_withheld"
}

func adaptiveAssistTailAttempt(attempt executionAttempt) executionAttempt {
	attempt.AdaptiveAssistTail = true
	if attempt.AdaptiveEdgeGate != nil {
		attempt.AdaptiveEdgeGate = &adaptiveEdgeGateAttemptState{}
	}
	return attempt
}

func insertExecutionAttemptBefore(plan []executionAttempt, index int, attempt executionAttempt) []executionAttempt {
	if index < 0 || index >= len(plan) {
		return append(plan, attempt)
	}
	plan = append(plan, executionAttempt{})
	copy(plan[index+1:], plan[index:])
	plan[index] = attempt
	return plan
}

func nextAdaptiveAssistTailIndex(plan []executionAttempt, start int, attempted map[int]bool) int {
	if start < 0 {
		start = 0
	}
	for index := start; index < len(plan); index++ {
		if plan[index].AdaptiveAssistTail && (attempted == nil || !attempted[index]) {
			return index
		}
	}
	return -1
}

func adaptiveAssistCanReachTailAfter(failure executionFailure) bool {
	return strings.TrimSpace(failure.Code) != "request_canceled"
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
