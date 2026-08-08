package main

import (
	"math"
	"strings"
	"time"
)

type adaptiveAdmissionRejectionCause string

const (
	adaptiveRejectionNone                  adaptiveAdmissionRejectionCause = ""
	adaptiveRejectionLedgerSaturated       adaptiveAdmissionRejectionCause = "ledger_saturated"
	adaptiveRejectionEstimatorSaturated    adaptiveAdmissionRejectionCause = "estimator_saturated"
	adaptiveRejectionDemandSaturated       adaptiveAdmissionRejectionCause = "demand_saturated"
	adaptiveRejectionDurabilityUnavailable adaptiveAdmissionRejectionCause = "durability_unavailable"
	adaptiveRejectionFloor                 adaptiveAdmissionRejectionCause = "floor"
	adaptiveRejectionConcurrency           adaptiveAdmissionRejectionCause = "concurrency"
	adaptiveRejectionQuotaStale            adaptiveAdmissionRejectionCause = "quota_stale"
	adaptiveRejectionPrimaryZero           adaptiveAdmissionRejectionCause = "primary_zero"
)

// adaptiveRouteDecision is captured while the credential admission lock is
// held. Route tracing serializes this immutable value later and never tries to
// reconstruct a decision from quota or concurrency state that may have moved.
type adaptiveRouteDecision struct {
	capturedAt       time.Time
	quotaConfirmedAt time.Time
	reservation      float64
	role             string
	mode             string
	decision         string
	rejection        string
	rejectionCause   adaptiveAdmissionRejectionCause
	sessionBefore    float64
	sessionAfter     float64
	weeklyBefore     float64
	weeklyAfter      float64
	sessionExposure  float64
	weeklyExposure   float64
	demandGuard      float64
	pendingGuard     float64
	inFlightGuard    float64
}

func captureAdaptiveAdmissionDecision(
	attempt executionAttempt,
	cfg pluginConfig,
	quota credentialQuotaState,
	inFlight, pending, sessionExposure, weeklyExposure, demandGuard float64,
	confidence string,
	rejectionCause adaptiveAdmissionRejectionCause,
	admitted bool,
	now time.Time,
) adaptiveRouteDecision {
	decision := adaptiveRouteDecision{
		capturedAt:       now.UTC(),
		quotaConfirmedAt: quotaConfirmedAt(quota),
		reservation:      attempt.ReservationPercent,
		mode:             firstNonEmpty(strings.TrimSpace(cfg.AllocatorMode), "enforce"),
		pendingGuard:     pending,
		inFlightGuard:    inFlight,
		sessionExposure:  sessionExposure,
		weeklyExposure:   weeklyExposure,
		demandGuard:      demandGuard,
	}
	if attempt.Primary {
		decision.role = "primary"
	} else {
		decision.role = "secondary"
	}
	if attempt.CompactBypass {
		decision.mode = "compact_bypass"
	}
	tarriff := tariffByID(cfg, attempt.TariffID)
	sessionFloor, weeklyFloor := tarriff.SessionFloorPercent, tarriff.WeeklyFloorPercent
	if attempt.Primary || attempt.CompactBypass {
		sessionFloor, weeklyFloor = 0, 0
	}
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	reserved := pending + inFlight
	decision.sessionBefore = quotaWindowSafeSurplus(session, sessionFloor, reserved, 0, sessionExposure, demandGuard)
	decision.weeklyBefore = quotaWindowSafeSurplus(weekly, weeklyFloor, reserved, 0, weeklyExposure, demandGuard)
	decision.sessionAfter = quotaWindowSafeSurplus(session, sessionFloor, reserved, decision.reservation, sessionExposure, demandGuard)
	decision.weeklyAfter = quotaWindowSafeSurplus(weekly, weeklyFloor, reserved, decision.reservation, weeklyExposure, demandGuard)
	if admitted {
		if math.Min(decision.sessionAfter, decision.weeklyAfter) <= 5 {
			decision.decision = "adaptive_amber_admitted"
		} else {
			decision.decision = "adaptive_green_admitted"
		}
		return decision
	}
	decision.rejectionCause = rejectionCause
	if rejectionCause != adaptiveRejectionNone {
		decision.rejection = adaptiveRejectionTraceCode(rejectionCause)
		return decision
	}
	switch {
	case confidence != "confirmed" && !attempt.Primary:
		decision.rejectionCause = adaptiveRejectionQuotaStale
		decision.rejection = "adaptive_quota_stale_protected"
	case attempt.Primary && (quotaWindowExhausted(session) || quotaWindowExhausted(weekly)):
		decision.rejectionCause = adaptiveRejectionPrimaryZero
		decision.rejection = "adaptive_primary_zero"
	case decision.sessionBefore > decision.reservation && decision.weeklyBefore > decision.reservation:
		decision.rejectionCause = adaptiveRejectionConcurrency
		decision.rejection = "adaptive_concurrency_recheck_failed"
	default:
		decision.rejectionCause = adaptiveRejectionFloor
		decision.rejection = "adaptive_secondary_floor_protected"
	}
	return decision
}

func markAdaptiveAdmissionRejected(attempt executionAttempt, cause adaptiveAdmissionRejectionCause) executionAttempt {
	attempt.AdaptiveTrace.decision = ""
	attempt.AdaptiveTrace.rejectionCause = cause
	attempt.AdaptiveTrace.rejection = adaptiveRejectionTraceCode(cause)
	return attempt
}

func adaptiveRejectionTraceCode(cause adaptiveAdmissionRejectionCause) string {
	switch cause {
	case adaptiveRejectionLedgerSaturated:
		return "adaptive_ledger_saturated"
	case adaptiveRejectionEstimatorSaturated:
		return "adaptive_estimator_saturated"
	case adaptiveRejectionDemandSaturated:
		return "adaptive_demand_saturated"
	case adaptiveRejectionDurabilityUnavailable:
		return "adaptive_durability_unavailable"
	case adaptiveRejectionFloor:
		return "adaptive_secondary_floor_protected"
	case adaptiveRejectionConcurrency:
		return "adaptive_concurrency_recheck_failed"
	case adaptiveRejectionQuotaStale:
		return "adaptive_quota_stale_protected"
	case adaptiveRejectionPrimaryZero:
		return "adaptive_primary_zero"
	default:
		return ""
	}
}

func adaptiveEstimatorIsSaturated(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	adaptiveReserveRuntime.Lock()
	defer adaptiveReserveRuntime.Unlock()
	if adaptiveReserveRuntime.SaturationGlobal {
		return true
	}
	_, saturated := adaptiveReserveRuntime.Saturated[authIndex]
	return saturated
}

func adaptiveDemandIsSaturated(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	bravoProjectDemand.mu.Lock()
	defer bravoProjectDemand.mu.Unlock()
	if bravoProjectDemand.projectSaturated {
		return true
	}
	_, saturated := bravoProjectDemand.projectBlocked[authIndex]
	return saturated
}

func applyAdaptiveRouteDecision(item *routeTraceAttempt, attempt executionAttempt) {
	if item == nil {
		return
	}
	decision := attempt.AdaptiveTrace
	if decision.mode == "" {
		if attempt.Primary {
			decision.role = "primary"
		} else {
			decision.role = "secondary"
		}
		if attempt.CompactBypass {
			decision.mode = "compact_bypass"
		} else if attempt.AllocatorManaged {
			decision.mode = "enforce"
		} else {
			decision.mode = "off"
		}
		decision.reservation = attempt.ReservationPercent
	}
	item.ReservationPercent = decision.reservation
	item.ProjectRole = decision.role
	item.AllocatorMode = decision.mode
	item.AdaptiveDecision = decision.decision
	item.AdaptiveRejection = decision.rejection
	item.AdmissionRejectionCause = string(decision.rejectionCause)
	item.SessionHeadroomBefore = decision.sessionBefore
	item.SessionHeadroomAfter = decision.sessionAfter
	item.WeeklyHeadroomBefore = decision.weeklyBefore
	item.WeeklyHeadroomAfter = decision.weeklyAfter
	item.SessionExposureGuardPercent = decision.sessionExposure
	item.WeeklyExposureGuardPercent = decision.weeklyExposure
	item.DemandGuardPercent = decision.demandGuard
	item.PendingGuardPercent = decision.pendingGuard
	item.InFlightGuardPercent = decision.inFlightGuard
}
