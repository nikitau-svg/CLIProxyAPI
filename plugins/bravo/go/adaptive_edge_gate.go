package main

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
)

// The edge gate is deliberately simpler than the quota-cost estimator. It
// never predicts the cost of a request. Confirmed cached headroom only decides
// whether an account needs single-flight protection; an actual quota failure
// is the only signal allowed to trip its circuit breaker.
const (
	adaptiveEdgeGateStateGreen    = "green"
	adaptiveEdgeGateStateGuarded  = "guarded"
	adaptiveEdgeGateStateTripped  = "tripped"
	adaptiveEdgeGateStateHalfOpen = "half_open"

	adaptiveEdgeGateDecisionDispatch    = "would_dispatch"
	adaptiveEdgeGateDecisionSkipBusy    = "would_skip_busy"
	adaptiveEdgeGateDecisionSkipTripped = "would_skip_tripped"
	adaptiveEdgeGateDecisionProbe       = "would_probe"

	adaptiveEdgeGateSessionGuardPercent = 8.0
	adaptiveEdgeGateWeeklyGuardPercent  = 2.0
	adaptiveEdgeGateMaximumAccounts     = 4096
	adaptiveEdgeGateMaximumBreakers     = 4096
	adaptiveEdgeGateMaximumLeaseAge     = 2 * time.Hour
)

type adaptiveEdgeGateAttemptState struct {
	mu sync.RWMutex

	authIndex string
	provider  string
	model     string

	staticState  string
	staticReason string
	state        string
	decision     string
	reason       string

	quotaConfirmed             bool
	sessionHeadroomPercent     float64
	weeklyHeadroomPercent      float64
	tripRemainingSeconds       int64
	outcomeTransition          string
	started                    bool
	outcomeObserved            bool
	guardHeld                  bool
	guardLeaseAt               time.Time
	probeBreakerKey            string
	probeBreakerGeneration     uint64
	probeEvidenceRevision      uint64
	probeLeaseID               uint64
	breakerCandidateKey        string
	breakerCandidateGeneration uint64
	breakerCandidateRevision   uint64
	recoveryBreakerKey         string
	recoveryBreakerGeneration  uint64
	recoveryEvidenceRevision   uint64
	recoveryLeaseID            uint64
	enforce                    bool
	quotaFresh                 bool
	compactBypass              bool
	breakerOnly                bool
}

type adaptiveEdgeGateSnapshot struct {
	State                  string
	Decision               string
	Reason                 string
	Enforce                bool
	QuotaConfirmed         bool
	SessionHeadroomPercent float64
	WeeklyHeadroomPercent  float64
	TripRemainingSeconds   int64
	OutcomeTransition      string
}

type adaptiveEdgeGateBreaker struct {
	AuthIndex         string
	Provider          string
	Model             string
	Until             time.Time
	Generation        uint64
	EvidenceRevision  uint64
	ProbeInFlight     bool
	ProbeStartedAt    time.Time
	ProbeLeaseID      uint64
	RecoveryInFlight  bool
	RecoveryStartedAt time.Time
	RecoveryLeaseID   uint64
	RecoveryUsed      bool
}

type adaptiveEdgeGateLease struct {
	StartedAt time.Time
}

var adaptiveEdgeGateRuntime = struct {
	sync.Mutex
	InFlight             map[string]adaptiveEdgeGateLease
	Breakers             map[string]adaptiveEdgeGateBreaker
	Saturated            bool
	DroppedLeases        uint64
	DroppedBreakers      uint64
	NextGeneration       uint64
	NextLeaseID          uint64
	NextEvidenceRevision uint64
}{
	InFlight: make(map[string]adaptiveEdgeGateLease),
	Breakers: make(map[string]adaptiveEdgeGateBreaker),
}

type adaptiveEdgeGatePublicView struct {
	Mode                       string  `json:"mode"`
	Effect                     string  `json:"effect"`
	RoutingEnforced            bool    `json:"routing_enforced"`
	QueuesRequests             bool    `json:"queues_requests"`
	AdditionalProviderRequests bool    `json:"additional_provider_requests"`
	SessionGuardPercent        float64 `json:"session_guard_percent"`
	WeeklyGuardPercent         float64 `json:"weekly_guard_percent"`
	InFlightGuards             int     `json:"in_flight_guards"`
	TrackedBreakers            int     `json:"tracked_breakers"`
	HalfOpenProbes             int     `json:"half_open_probes"`
	Saturated                  bool    `json:"saturated"`
	DroppedLeases              uint64  `json:"dropped_leases"`
	DroppedBreakers            uint64  `json:"dropped_breakers"`
	Note                       string  `json:"note"`
}

func newAdaptiveEdgeGateAttemptState(
	cfg pluginConfig,
	attempt executionAttempt,
	quota credentialQuotaState,
	tariff tariffConfig,
	now time.Time,
) *adaptiveEdgeGateAttemptState {
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	if authIndex == "" {
		return nil
	}
	provider := normalizeProvider(firstNonEmpty(attempt.Candidate.Provider, attempt.Auth.Provider, attempt.Auth.Type))
	model := baseModelKey(strings.TrimSpace(attempt.Candidate.Model))
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	sessionFloor, weeklyFloor := tariff.SessionFloorPercent, tariff.WeeklyFloorPercent
	if attempt.Primary {
		sessionFloor, weeklyFloor = 0, 0
	}
	sessionHeadroom := session.RemainingPercent - sessionFloor
	weeklyHeadroom := weekly.RemainingPercent - weeklyFloor
	confirmed := quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now) == "confirmed"
	state, reason := adaptiveEdgeGateStateGreen, "confirmed_headroom"
	switch {
	case !confirmed:
		state, reason = adaptiveEdgeGateStateGuarded, "quota_unconfirmed"
	case quotaFreshnessAt(quota, attempt.Candidate.Model, cfg, now) != quotaFreshnessFresh:
		state, reason = adaptiveEdgeGateStateGuarded, "quota_stale"
	case sessionHeadroom <= adaptiveEdgeGateSessionGuardPercent && weeklyHeadroom <= adaptiveEdgeGateWeeklyGuardPercent:
		state, reason = adaptiveEdgeGateStateGuarded, "session_and_weekly_edge"
	case sessionHeadroom <= adaptiveEdgeGateSessionGuardPercent:
		state, reason = adaptiveEdgeGateStateGuarded, "session_edge"
	case weeklyHeadroom <= adaptiveEdgeGateWeeklyGuardPercent:
		state, reason = adaptiveEdgeGateStateGuarded, "weekly_edge"
	}
	return &adaptiveEdgeGateAttemptState{
		authIndex:              authIndex,
		provider:               provider,
		model:                  model,
		staticState:            state,
		staticReason:           reason,
		state:                  state,
		reason:                 reason,
		quotaConfirmed:         confirmed,
		sessionHeadroomPercent: adaptiveShadowRound(sessionHeadroom),
		weeklyHeadroomPercent:  adaptiveShadowRound(weeklyHeadroom),
		enforce:                adaptiveEdgeRoutingEnforced(cfg),
		quotaFresh:             quotaFreshnessAt(quota, attempt.Candidate.Model, cfg, now) == quotaFreshnessFresh,
		compactBypass:          attempt.CompactBypass,
		breakerOnly:            cfg.AdaptiveAllocatorMode == "breaker",
	}
}

// acquireAdaptiveBreakerEnforcementLease applies only an evidence-backed
// breaker. Forecast headroom remains shadow-only and cannot withhold an
// attempt in this mode.
func acquireAdaptiveBreakerEnforcementLease(
	attempt executionAttempt,
	now time.Time,
) (func(bool), bool, *executionFailure) {
	cfg := loadedConfig()
	cfg = adaptiveAttemptConfig(attempt, cfg)
	if cfg.AdaptiveAllocatorMode != "breaker" || !attempt.AdaptiveShadow || attempt.CompactBypass {
		return wrapAdaptiveShadowLease(attempt, func(bool) {}), true, nil
	}
	now = now.UTC()
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	subscription := subscriptionPolicy(cfg, authIndex)
	tariff := effectiveTariff(cfg, subscription, firstNonEmpty(attempt.Auth.Provider, attempt.Auth.Type), quota)
	refreshAdaptiveEdgeGateAttemptState(attempt, cfg, quota, tariff, now)
	if attempt.AdaptiveBreakerLastChance {
		return acquireAdaptiveBreakerRecoveryLease(attempt, now)
	}
	beginAdaptiveEdgeGateShadow(attempt, now)
	switch attempt.AdaptiveEdgeGate.snapshot().Decision {
	case adaptiveEdgeGateDecisionSkipBusy:
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_busy",
			"Адаптивный edge-турникет уже проверяет эту подписку у подтверждённой границы лимита; Bravo сразу продолжил соседний маршрут.",
		)
	case adaptiveEdgeGateDecisionSkipTripped:
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_tripped",
			"Подтверждённая ошибка квоты временно закрыла этот маршрут; Bravo сразу продолжил соседний маршрут.",
		)
	default:
		return adaptiveEnforcementFailOpenLease(attempt), true, nil
	}
}

func acquireAdaptiveBreakerRecoveryLease(
	attempt executionAttempt,
	now time.Time,
) (func(bool), bool, *executionFailure) {
	state := attempt.AdaptiveEdgeGate
	if state == nil {
		return adaptiveEnforcementFailOpenLease(attempt), true, nil
	}
	key := strings.TrimSpace(attempt.AdaptiveBreakerRecoveryKey)
	generation := attempt.AdaptiveBreakerRecoveryGeneration
	revision := attempt.AdaptiveBreakerRecoveryRevision
	state.mu.Lock()
	state.started = true
	state.state = adaptiveEdgeGateStateHalfOpen
	state.enforce = true
	state.breakerOnly = true
	if key == "" || generation == 0 || revision == 0 {
		state.decision = adaptiveEdgeGateDecisionDispatch
		state.reason = "breaker_disappeared_last_chance"
		state.mu.Unlock()
		return adaptiveEnforcementFailOpenLease(attempt), true, nil
	}

	adaptiveEdgeGateRuntime.Lock()
	currentKey, breaker, relevant := adaptiveEdgeGateActiveBreakerLocked(state, now)
	if !relevant {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionDispatch
		state.reason = "breaker_disappeared_last_chance"
		state.mu.Unlock()
		return adaptiveEnforcementFailOpenLease(attempt), true, nil
	}
	if currentKey != key || breaker.Generation != generation || breaker.EvidenceRevision != revision {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipTripped
		state.reason = "breaker_generation_changed"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_tripped",
			"Маршрут уже закрыт новым поколением breaker; устаревшая recovery-проверка отменена без ожидания.",
		)
	}
	if breaker.ProbeInFlight {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipBusy
		state.reason = "scheduled_probe_busy"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_busy",
			"Scheduled half-open уже проверяет этот breaker; сохранённая recovery-попытка не запускается параллельно.",
		)
	}
	if breaker.RecoveryInFlight && !breaker.RecoveryStartedAt.IsZero() &&
		now.Sub(breaker.RecoveryStartedAt) >= adaptiveEdgeGateMaximumLeaseAge {
		breaker.RecoveryInFlight = false
		breaker.RecoveryStartedAt = time.Time{}
		breaker.RecoveryLeaseID = 0
		adaptiveEdgeGateRuntime.Breakers[key] = breaker
	}
	if breaker.RecoveryInFlight {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipBusy
		state.reason = "recovery_probe_busy"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_busy",
			"Другой запрос уже выполняет единственную recovery-проверку закрытого маршрута; Bravo не ждёт и завершает локальный fallback.",
		)
	}
	if breaker.RecoveryUsed {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipTripped
		state.reason = "recovery_already_used"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_tripped",
			"Recovery-проверка этого поколения breaker уже использована; Bravo не повторяет обращение к закрытому маршруту.",
		)
	}
	if adaptiveEdgeGateLeaseBusyLocked(state.authIndex, now) {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipBusy
		state.reason = "proof_auth_busy"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_busy",
			"Другая proof-попытка уже использует эту подписку; recovery не запускается параллельно.",
		)
	}
	if !adaptiveEdgeGateAcquireLeaseLocked(state, now) {
		adaptiveEdgeGateRuntime.Unlock()
		state.decision = adaptiveEdgeGateDecisionSkipBusy
		state.reason = "proof_coordination_saturated"
		state.mu.Unlock()
		return func(bool) {}, false, adaptiveEnforcementFailure(
			"bravo_adaptive_edge_busy",
			"Координация recovery временно насыщена; защищённая попытка не отправлена повторно.",
		)
	}
	breaker.RecoveryInFlight = true
	breaker.RecoveryStartedAt = now
	breaker.RecoveryLeaseID = adaptiveEdgeGateNextLeaseIDLocked()
	breaker.RecoveryUsed = true
	adaptiveEdgeGateRuntime.Breakers[key] = breaker
	adaptiveEdgeGateRuntime.Unlock()
	state.decision = adaptiveEdgeGateDecisionProbe
	state.reason = "breaker_recovery_probe"
	state.recoveryBreakerKey = key
	state.recoveryBreakerGeneration = generation
	state.recoveryEvidenceRevision = revision
	state.recoveryLeaseID = breaker.RecoveryLeaseID
	state.mu.Unlock()
	return adaptiveEnforcementFailOpenLease(attempt), true, nil
}

func (state *adaptiveEdgeGateAttemptState) snapshot() adaptiveEdgeGateSnapshot {
	if state == nil {
		return adaptiveEdgeGateSnapshot{}
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return adaptiveEdgeGateSnapshot{
		State:                  state.state,
		Decision:               state.decision,
		Reason:                 state.reason,
		Enforce:                state.enforce,
		QuotaConfirmed:         state.quotaConfirmed,
		SessionHeadroomPercent: state.sessionHeadroomPercent,
		WeeklyHeadroomPercent:  state.weeklyHeadroomPercent,
		TripRemainingSeconds:   state.tripRemainingSeconds,
		OutcomeTransition:      state.outcomeTransition,
	}
}

// refreshAdaptiveEdgeGateAttemptState updates the plan-time snapshot just
// before acquisition. Quota may have become stale between planning and the
// provider call; enforcement must not inherit a stale guarded verdict and turn
// it into a real skip. Existing started state is never rewritten.
func refreshAdaptiveEdgeGateAttemptState(
	attempt executionAttempt,
	cfg pluginConfig,
	quota credentialQuotaState,
	tariff tariffConfig,
	now time.Time,
) {
	state := attempt.AdaptiveEdgeGate
	if state == nil {
		return
	}
	refreshed := newAdaptiveEdgeGateAttemptState(cfg, attempt, quota, tariff, now)
	if refreshed == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started {
		return
	}
	state.authIndex = refreshed.authIndex
	state.provider = refreshed.provider
	state.model = refreshed.model
	state.staticState = refreshed.staticState
	state.staticReason = refreshed.staticReason
	state.state = refreshed.state
	state.decision = ""
	state.reason = refreshed.reason
	state.quotaConfirmed = refreshed.quotaConfirmed
	state.sessionHeadroomPercent = refreshed.sessionHeadroomPercent
	state.weeklyHeadroomPercent = refreshed.weeklyHeadroomPercent
	state.tripRemainingSeconds = 0
	state.enforce = refreshed.enforce
	state.quotaFresh = refreshed.quotaFresh
	state.compactBypass = refreshed.compactBypass
	state.breakerOnly = refreshed.breakerOnly
}

func beginAdaptiveEdgeGateShadow(attempt executionAttempt, now time.Time) {
	state := attempt.AdaptiveEdgeGate
	if !attempt.AdaptiveShadow || state == nil {
		return
	}
	now = now.UTC()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started {
		return
	}
	state.started = true
	if state.enforce && state.compactBypass {
		// /compact is the explicit, rate-limited escape hatch for the ordinary
		// reserve floor. Adaptive enforcement may observe it, but must not add a
		// second gate that changes the bypass contract.
		state.state = state.staticState
		state.decision = adaptiveEdgeGateDecisionDispatch
		state.reason = "compact_bypass_fail_open"
		return
	}

	adaptiveEdgeGateRuntime.Lock()
	defer adaptiveEdgeGateRuntime.Unlock()
	adaptiveEdgeGatePruneStaleLeaseLocked(state.authIndex, now)

	breakerKey, breaker, found := adaptiveEdgeGateActiveBreakerLocked(state, now)
	if found {
		if breaker.Generation == 0 {
			breaker.Generation = adaptiveEdgeGateNextGenerationLocked()
			adaptiveEdgeGateRuntime.Breakers[breakerKey] = breaker
		}
		if breaker.EvidenceRevision == 0 {
			breaker.EvidenceRevision = adaptiveEdgeGateNextEvidenceRevisionLocked()
			adaptiveEdgeGateRuntime.Breakers[breakerKey] = breaker
		}
		state.breakerCandidateKey = breakerKey
		state.breakerCandidateGeneration = breaker.Generation
		state.breakerCandidateRevision = breaker.EvidenceRevision
		if breaker.Until.After(now) {
			state.state = adaptiveEdgeGateStateTripped
			state.decision = adaptiveEdgeGateDecisionSkipTripped
			state.reason = adaptiveEdgeGateBreakerReason(breaker)
			state.tripRemainingSeconds = adaptiveEdgeGateRemainingSeconds(breaker.Until, now)
			return
		}
		if adaptiveEdgeGateLeaseBusyLocked(state.authIndex, now) || breaker.ProbeInFlight ||
			breaker.RecoveryInFlight {
			state.state = adaptiveEdgeGateStateHalfOpen
			state.decision = adaptiveEdgeGateDecisionSkipBusy
			state.reason = "half_open_probe_busy"
			return
		}
		if !adaptiveEdgeGateAcquireLeaseLocked(state, now) {
			state.state = adaptiveEdgeGateStateHalfOpen
			if state.breakerOnly {
				state.decision = adaptiveEdgeGateDecisionSkipBusy
				state.reason = "proof_coordination_saturated"
			} else {
				state.decision = adaptiveEdgeGateDecisionDispatch
				state.reason = "runtime_saturated_fail_open"
			}
			return
		}
		breaker.ProbeInFlight = true
		breaker.ProbeStartedAt = now
		breaker.ProbeLeaseID = adaptiveEdgeGateNextLeaseIDLocked()
		adaptiveEdgeGateRuntime.Breakers[breakerKey] = breaker
		state.state = adaptiveEdgeGateStateHalfOpen
		state.decision = adaptiveEdgeGateDecisionProbe
		state.reason = adaptiveEdgeGateBreakerReason(breaker)
		state.probeBreakerKey = breakerKey
		state.probeBreakerGeneration = breaker.Generation
		state.probeEvidenceRevision = breaker.EvidenceRevision
		state.probeLeaseID = breaker.ProbeLeaseID
		return
	}
	if state.breakerOnly {
		state.state = state.staticState
		state.decision = adaptiveEdgeGateDecisionDispatch
		switch {
		case !state.quotaConfirmed:
			state.reason = "quota_unconfirmed_fail_open"
		case !state.quotaFresh:
			state.reason = "quota_stale_fail_open"
		default:
			state.reason = "breaker_clear"
		}
		return
	}

	state.state = state.staticState
	state.reason = state.staticReason
	if state.enforce && (!state.quotaConfirmed || !state.quotaFresh) {
		state.decision = adaptiveEdgeGateDecisionDispatch
		if state.quotaConfirmed {
			state.reason = "quota_stale_fail_open"
		} else {
			state.reason = "quota_unconfirmed_fail_open"
		}
		return
	}
	if state.staticState != adaptiveEdgeGateStateGuarded {
		state.decision = adaptiveEdgeGateDecisionDispatch
		return
	}
	if adaptiveEdgeGateLeaseBusyLocked(state.authIndex, now) {
		state.decision = adaptiveEdgeGateDecisionSkipBusy
		state.reason = "guarded_request_busy"
		return
	}
	if !adaptiveEdgeGateAcquireLeaseLocked(state, now) {
		state.decision = adaptiveEdgeGateDecisionDispatch
		state.reason = "runtime_saturated_fail_open"
		return
	}
	state.decision = adaptiveEdgeGateDecisionDispatch
}

// cancelAdaptiveEdgeGateAttempt releases a simulated/enforced guard when the
// request never reached the provider (for example the ordinary allocator lost
// a concurrent reservation race). It does not create a breaker.
func cancelAdaptiveEdgeGateAttempt(attempt executionAttempt) {
	state := attempt.AdaptiveEdgeGate
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.started || state.outcomeObserved {
		return
	}
	releaseAdaptiveEdgeGateStateLocked(state)
	state.outcomeObserved = true
	state.outcomeTransition = "not_dispatched"
}

// failOpenAdaptiveEdgeGateAttempt removes any guard acquired by this attempt
// while still allowing its later provider outcome to trip a real breaker.
func failOpenAdaptiveEdgeGateAttempt(attempt executionAttempt, reason string) {
	state := attempt.AdaptiveEdgeGate
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.started || state.outcomeObserved {
		return
	}
	releaseAdaptiveEdgeGateStateLocked(state)
	state.decision = adaptiveEdgeGateDecisionDispatch
	state.reason = reason
}

func releaseAdaptiveEdgeGateStateLocked(state *adaptiveEdgeGateAttemptState) {
	if state == nil {
		return
	}
	adaptiveEdgeGateRuntime.Lock()
	defer adaptiveEdgeGateRuntime.Unlock()
	if state.guardHeld {
		if lease, ok := adaptiveEdgeGateRuntime.InFlight[state.authIndex]; ok &&
			lease.StartedAt.Equal(state.guardLeaseAt) {
			delete(adaptiveEdgeGateRuntime.InFlight, state.authIndex)
		}
		state.guardHeld = false
	}
	if state.probeBreakerKey != "" {
		if breaker, ok := adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey]; ok &&
			breaker.Generation == state.probeBreakerGeneration && breaker.ProbeInFlight &&
			breaker.ProbeLeaseID == state.probeLeaseID {
			breaker.ProbeInFlight = false
			breaker.ProbeStartedAt = time.Time{}
			breaker.ProbeLeaseID = 0
			adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey] = breaker
		}
		state.probeBreakerKey = ""
	}
	if state.recoveryBreakerKey != "" {
		if breaker, ok := adaptiveEdgeGateRuntime.Breakers[state.recoveryBreakerKey]; ok &&
			breaker.Generation == state.recoveryBreakerGeneration && breaker.RecoveryInFlight &&
			breaker.RecoveryLeaseID == state.recoveryLeaseID {
			breaker.RecoveryInFlight = false
			breaker.RecoveryStartedAt = time.Time{}
			breaker.RecoveryLeaseID = 0
			if breaker.EvidenceRevision == state.recoveryEvidenceRevision {
				breaker.RecoveryUsed = false
			}
			adaptiveEdgeGateRuntime.Breakers[state.recoveryBreakerKey] = breaker
		}
		state.recoveryBreakerKey = ""
	}
}

func adaptiveEdgeGateActiveBreakerLocked(
	state *adaptiveEdgeGateAttemptState,
	now time.Time,
) (string, adaptiveEdgeGateBreaker, bool) {
	type candidateBreaker struct {
		key     string
		breaker adaptiveEdgeGateBreaker
	}
	candidates := make([]candidateBreaker, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, key := range []string{
		adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, ""),
		adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, state.model),
	} {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		breaker, ok := adaptiveEdgeGateRuntime.Breakers[key]
		if !ok {
			continue
		}
		if breaker.ProbeInFlight && !breaker.ProbeStartedAt.IsZero() &&
			now.Sub(breaker.ProbeStartedAt) >= adaptiveEdgeGateMaximumLeaseAge {
			breaker.ProbeInFlight = false
			breaker.ProbeStartedAt = time.Time{}
			breaker.ProbeLeaseID = 0
			adaptiveEdgeGateRuntime.Breakers[key] = breaker
		}
		if breaker.RecoveryInFlight && !breaker.RecoveryStartedAt.IsZero() &&
			now.Sub(breaker.RecoveryStartedAt) >= adaptiveEdgeGateMaximumLeaseAge {
			breaker.RecoveryInFlight = false
			breaker.RecoveryStartedAt = time.Time{}
			breaker.RecoveryLeaseID = 0
			adaptiveEdgeGateRuntime.Breakers[key] = breaker
		}
		candidates = append(candidates, candidateBreaker{key: key, breaker: breaker})
	}
	// Candidate order is account then current model. Prefer an actually active
	// proof in that order; an expired account breaker must never hide a live
	// model breaker. Only when neither is active do we choose the deterministic
	// expired proof candidate for half-open.
	for _, candidate := range candidates {
		breaker := candidate.breaker
		if breaker.Until.After(now) || breaker.ProbeInFlight || breaker.RecoveryInFlight {
			return candidate.key, breaker, true
		}
	}
	if len(candidates) > 0 {
		return candidates[0].key, candidates[0].breaker, true
	}
	return "", adaptiveEdgeGateBreaker{}, false
}

func adaptiveEdgeGateAcquireLeaseLocked(state *adaptiveEdgeGateAttemptState, now time.Time) bool {
	if state == nil || state.authIndex == "" {
		return false
	}
	if _, exists := adaptiveEdgeGateRuntime.InFlight[state.authIndex]; !exists &&
		len(adaptiveEdgeGateRuntime.InFlight) >= adaptiveEdgeGateMaximumAccounts {
		adaptiveEdgeGateRuntime.Saturated = true
		adaptiveEdgeGateRuntime.DroppedLeases++
		return false
	}
	adaptiveEdgeGateRuntime.InFlight[state.authIndex] = adaptiveEdgeGateLease{StartedAt: now}
	state.guardHeld = true
	state.guardLeaseAt = now
	return true
}

func adaptiveEdgeGateLeaseBusyLocked(authIndex string, now time.Time) bool {
	lease, ok := adaptiveEdgeGateRuntime.InFlight[authIndex]
	if !ok {
		return false
	}
	if lease.StartedAt.IsZero() || now.Sub(lease.StartedAt) < adaptiveEdgeGateMaximumLeaseAge {
		return true
	}
	delete(adaptiveEdgeGateRuntime.InFlight, authIndex)
	return false
}

func adaptiveEdgeGatePruneStaleLeaseLocked(authIndex string, now time.Time) {
	_ = adaptiveEdgeGateLeaseBusyLocked(authIndex, now)
}

func observeAdaptiveEdgeGateOutcome(
	attempt executionAttempt,
	success bool,
	failure executionFailure,
	now time.Time,
) {
	state := attempt.AdaptiveEdgeGate
	if !attempt.AdaptiveShadow || state == nil {
		return
	}
	now = now.UTC()
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.started || state.outcomeObserved {
		return
	}
	state.outcomeObserved = true

	adaptiveEdgeGateRuntime.Lock()
	defer adaptiveEdgeGateRuntime.Unlock()
	if state.guardHeld {
		if lease, ok := adaptiveEdgeGateRuntime.InFlight[state.authIndex]; ok &&
			lease.StartedAt.Equal(state.guardLeaseAt) {
			delete(adaptiveEdgeGateRuntime.InFlight, state.authIndex)
		}
		state.guardHeld = false
	}

	if state.decision == adaptiveEdgeGateDecisionSkipBusy ||
		state.decision == adaptiveEdgeGateDecisionSkipTripped {
		state.outcomeTransition = "counterfactual_only"
		return
	}

	quotaFailure := !success && adaptiveEdgeGateQuotaFailure(failure)
	_, reviewedProviderOutcome := adaptiveEdgeGateReviewedProviderDetail(failure)
	conclusive := success || attempt.AdaptiveProviderAccepted || reviewedProviderOutcome
	if state.recoveryBreakerKey != "" {
		breaker, exists := adaptiveEdgeGateRuntime.Breakers[state.recoveryBreakerKey]
		if exists && breaker.Generation == state.recoveryBreakerGeneration && breaker.EvidenceRevision == state.recoveryEvidenceRevision &&
			breaker.RecoveryInFlight && breaker.RecoveryLeaseID == state.recoveryLeaseID {
			breaker.RecoveryInFlight = false
			breaker.RecoveryStartedAt = time.Time{}
			breaker.RecoveryLeaseID = 0
			if !quotaFailure && !conclusive {
				breaker.RecoveryUsed = true
				adaptiveEdgeGateExtendBreakerUntil(&breaker, failure, now)
				adaptiveEdgeGateRuntime.Breakers[state.recoveryBreakerKey] = breaker
				state.outcomeTransition = "recovery_inconclusive_retripped"
				return
			}
			if !quotaFailure {
				delete(adaptiveEdgeGateRuntime.Breakers, state.recoveryBreakerKey)
				state.outcomeTransition = "reopened_recovery"
				return
			}
			breaker.RecoveryUsed = true
			breaker.EvidenceRevision = adaptiveEdgeGateNextEvidenceRevisionLocked()
			adaptiveEdgeGateExtendBreakerUntil(&breaker, failure, now)
			targetKey := state.recoveryBreakerKey
			if adaptiveEdgeGateAccountWideQuotaFailure(failure) && breaker.Model != "" {
				delete(adaptiveEdgeGateRuntime.Breakers, state.recoveryBreakerKey)
				breaker.Model = ""
				targetKey = adaptiveEdgeGateBreakerKey(breaker.Provider, breaker.AuthIndex, "")
				if existing, ok := adaptiveEdgeGateRuntime.Breakers[targetKey]; ok {
					newRevision := breaker.EvidenceRevision
					if breaker.Until.After(existing.Until) {
						existing.Until = breaker.Until
					}
					existing.RecoveryUsed = existing.RecoveryUsed || breaker.RecoveryUsed ||
						existing.ProbeInFlight || existing.RecoveryInFlight
					existing.EvidenceRevision = newRevision
					breaker = existing
				}
			}
			adaptiveEdgeGateRuntime.Breakers[targetKey] = breaker
			if targetKey == state.recoveryBreakerKey {
				state.outcomeTransition = "retripped_recovery"
			} else {
				state.outcomeTransition = "retripped_recovery_account"
			}
			return
		}
		if exists && breaker.Generation == state.recoveryBreakerGeneration &&
			breaker.RecoveryInFlight && breaker.RecoveryLeaseID == state.recoveryLeaseID {
			breaker.RecoveryInFlight = false
			breaker.RecoveryStartedAt = time.Time{}
			breaker.RecoveryLeaseID = 0
			if quotaFailure {
				breaker.RecoveryUsed = true
				breaker.EvidenceRevision = adaptiveEdgeGateNextEvidenceRevisionLocked()
				adaptiveEdgeGateExtendBreakerUntil(&breaker, failure, now)
			}
			adaptiveEdgeGateStoreStaleQuotaEvidenceLocked(state.recoveryBreakerKey, breaker, failure)
			state.outcomeTransition = "stale_newer_evidence"
			return
		}
		state.outcomeTransition = "stale_recovery_outcome"
		return
	}
	probeRetrip := false
	probeGeneration := uint64(0)
	if state.probeBreakerKey != "" {
		probeGeneration = state.probeBreakerGeneration
		breaker, exists := adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey]
		if !exists || breaker.Generation != state.probeBreakerGeneration || breaker.EvidenceRevision != state.probeEvidenceRevision ||
			!breaker.ProbeInFlight || breaker.ProbeLeaseID != state.probeLeaseID {
			if exists && breaker.Generation == state.probeBreakerGeneration &&
				breaker.ProbeInFlight && breaker.ProbeLeaseID == state.probeLeaseID {
				breaker.ProbeInFlight = false
				breaker.ProbeStartedAt = time.Time{}
				breaker.ProbeLeaseID = 0
				if quotaFailure {
					breaker.RecoveryUsed = true
					breaker.EvidenceRevision = adaptiveEdgeGateNextEvidenceRevisionLocked()
					adaptiveEdgeGateExtendBreakerUntil(&breaker, failure, now)
				}
				adaptiveEdgeGateStoreStaleQuotaEvidenceLocked(state.probeBreakerKey, breaker, failure)
				state.outcomeTransition = "stale_newer_evidence"
				return
			}
			state.outcomeTransition = "stale_probe_outcome"
			return
		}
		if !quotaFailure && !conclusive {
			breaker.ProbeInFlight = false
			breaker.ProbeStartedAt = time.Time{}
			breaker.ProbeLeaseID = 0
			breaker.RecoveryUsed = true
			adaptiveEdgeGateExtendBreakerUntil(&breaker, failure, now)
			adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey] = breaker
			state.outcomeTransition = "probe_inconclusive_retripped"
			return
		}
		delete(adaptiveEdgeGateRuntime.Breakers, state.probeBreakerKey)
		if !quotaFailure {
			state.outcomeTransition = "reopened"
			return
		}
		probeRetrip = true
	}
	if !quotaFailure {
		state.outcomeTransition = "unchanged"
		return
	}

	until := failureCooldownUntil(failure, now)
	model := state.model
	if adaptiveEdgeGateAccountWideQuotaFailure(failure) {
		model = ""
	}
	breaker := adaptiveEdgeGateBreaker{
		AuthIndex:        state.authIndex,
		Provider:         state.provider,
		Model:            model,
		Until:            until.UTC(),
		Generation:       adaptiveEdgeGateNextGenerationLocked(),
		EvidenceRevision: adaptiveEdgeGateNextEvidenceRevisionLocked(),
	}
	if probeRetrip {
		breaker.Generation = probeGeneration
		if breaker.Generation == 0 {
			breaker.Generation = adaptiveEdgeGateNextGenerationLocked()
		}
		breaker.RecoveryUsed = true
	}
	key := adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, model)
	if existing, exists := adaptiveEdgeGateRuntime.Breakers[key]; exists {
		// Concurrent real failures belong to the same breaker lifecycle. Never
		// erase an active/used recovery token by replacing the struct.
		breaker.Generation = existing.Generation
		// New evidence advances the revision but cannot pretend an older
		// physical provider call stopped. Preserve its lease so peers remain
		// busy; the late owner may clear only its matching lease ID.
		breaker.ProbeInFlight = existing.ProbeInFlight
		breaker.ProbeStartedAt = existing.ProbeStartedAt
		breaker.ProbeLeaseID = existing.ProbeLeaseID
		breaker.RecoveryInFlight = existing.RecoveryInFlight
		breaker.RecoveryStartedAt = existing.RecoveryStartedAt
		breaker.RecoveryLeaseID = existing.RecoveryLeaseID
		breaker.RecoveryUsed = existing.RecoveryUsed || breaker.RecoveryUsed ||
			existing.ProbeInFlight || existing.RecoveryInFlight
		if existing.Until.After(breaker.Until) {
			breaker.Until = existing.Until
		}
	}
	if _, exists := adaptiveEdgeGateRuntime.Breakers[key]; !exists &&
		len(adaptiveEdgeGateRuntime.Breakers) >= adaptiveEdgeGateMaximumBreakers {
		adaptiveEdgeGateRuntime.Saturated = true
		adaptiveEdgeGateRuntime.DroppedBreakers++
		state.outcomeTransition = "trip_dropped_fail_open"
		return
	}
	adaptiveEdgeGateRuntime.Breakers[key] = breaker
	state.tripRemainingSeconds = adaptiveEdgeGateRemainingSeconds(until, now)
	if model == "" {
		state.outcomeTransition = "tripped_account"
	} else {
		state.outcomeTransition = "tripped_model"
	}
}

func adaptiveEdgeGateQuotaFailure(failure executionFailure) bool {
	if detail, reviewed := adaptiveEdgeGateReviewedProviderDetail(failure); reviewed {
		return (detail.Class == providererror.ClassQuota || detail.Class == providererror.ClassRateLimit) &&
			(detail.Scope == providererror.ScopeModel || detail.Scope == providererror.ScopeAccount)
	}
	return failure.Status == 429
}

func adaptiveEdgeGateAccountWideQuotaFailure(failure executionFailure) bool {
	detail, reviewed := adaptiveEdgeGateReviewedProviderDetail(failure)
	return reviewed &&
		(detail.Class == providererror.ClassQuota || detail.Class == providererror.ClassRateLimit) &&
		detail.Scope == providererror.ScopeAccount
}

func adaptiveEdgeGateReviewedProviderDetail(failure executionFailure) (providererror.Detail, bool) {
	if failure.Provider == nil {
		return providererror.Detail{}, false
	}
	detail := providererror.Sanitize(*failure.Provider)
	return detail, detail.TaxonomyVersion == providererror.FailureTaxonomyV1
}

func adaptiveEdgeGateBreakerKey(provider, authIndex, model string) string {
	return normalizeProvider(provider) + "\x00" + strings.TrimSpace(authIndex) + "\x00" + baseModelKey(strings.TrimSpace(model))
}

func adaptiveEdgeGateNextGenerationLocked() uint64 {
	adaptiveEdgeGateRuntime.NextGeneration++
	if adaptiveEdgeGateRuntime.NextGeneration == 0 {
		adaptiveEdgeGateRuntime.NextGeneration++
	}
	return adaptiveEdgeGateRuntime.NextGeneration
}

func adaptiveEdgeGateNextLeaseIDLocked() uint64 {
	adaptiveEdgeGateRuntime.NextLeaseID++
	if adaptiveEdgeGateRuntime.NextLeaseID == 0 {
		adaptiveEdgeGateRuntime.NextLeaseID++
	}
	return adaptiveEdgeGateRuntime.NextLeaseID
}

func adaptiveEdgeGateNextEvidenceRevisionLocked() uint64 {
	adaptiveEdgeGateRuntime.NextEvidenceRevision++
	if adaptiveEdgeGateRuntime.NextEvidenceRevision == 0 {
		adaptiveEdgeGateRuntime.NextEvidenceRevision++
	}
	return adaptiveEdgeGateRuntime.NextEvidenceRevision
}

func adaptiveEdgeGateExtendBreakerUntil(breaker *adaptiveEdgeGateBreaker, failure executionFailure, now time.Time) {
	if breaker == nil {
		return
	}
	until := failureCooldownUntil(failure, now).UTC()
	if !until.After(now) {
		until = now.Add(time.Second)
	}
	if until.After(breaker.Until) {
		breaker.Until = until
	}
}

func adaptiveEdgeGateStoreStaleQuotaEvidenceLocked(oldKey string, breaker adaptiveEdgeGateBreaker, failure executionFailure) {
	if !adaptiveEdgeGateAccountWideQuotaFailure(failure) || breaker.Model == "" {
		adaptiveEdgeGateRuntime.Breakers[oldKey] = breaker
		return
	}
	delete(adaptiveEdgeGateRuntime.Breakers, oldKey)
	breaker.Model = ""
	breaker.RecoveryUsed = true
	targetKey := adaptiveEdgeGateBreakerKey(breaker.Provider, breaker.AuthIndex, "")
	if existing, ok := adaptiveEdgeGateRuntime.Breakers[targetKey]; ok {
		newRevision := breaker.EvidenceRevision
		if breaker.Until.After(existing.Until) {
			existing.Until = breaker.Until
		}
		existing.EvidenceRevision = newRevision
		existing.RecoveryUsed = true
		breaker = existing
	}
	adaptiveEdgeGateRuntime.Breakers[targetKey] = breaker
}

func adaptiveEdgeGateBreakerReason(breaker adaptiveEdgeGateBreaker) string {
	if strings.TrimSpace(breaker.Model) == "" {
		return "account_breaker"
	}
	return "model_breaker"
}

func adaptiveEdgeGateRemainingSeconds(until, now time.Time) int64 {
	if !until.After(now) {
		return 0
	}
	return int64(math.Ceil(until.Sub(now).Seconds()))
}

func adaptiveEdgeGateSummary(cfg pluginConfig, authIndexes []string, now time.Time) adaptiveEdgeGatePublicView {
	allowed := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
			allowed[authIndex] = struct{}{}
		}
	}
	filter := authIndexes != nil
	view := adaptiveEdgeGatePublicView{
		Mode:                       cfg.AdaptiveAllocatorMode,
		Effect:                     adaptiveShadowEffect(cfg),
		RoutingEnforced:            adaptiveEdgeRoutingEnforced(cfg),
		QueuesRequests:             false,
		AdditionalProviderRequests: false,
		SessionGuardPercent:        adaptiveEdgeGateSessionGuardPercent,
		WeeklyGuardPercent:         adaptiveEdgeGateWeeklyGuardPercent,
		Note:                       "Турникет моделирует мгновенный переход к следующему маршруту; он не ждёт, не ставит запросы в очередь и не добавляет обращений к провайдеру.",
	}
	if cfg.AdaptiveAllocatorMode == "off" {
		view.Note = "Shadow-турникет отключён вместе с адаптивным наблюдением."
	} else if cfg.AdaptiveAllocatorMode == "breaker" {
		view.Note = "Боевой breaker закрывает маршрут только после фактической доверенной ошибки квоты/rate-limit; прогноз headroom остаётся теневым, очередей нет."
	} else if cfg.AdaptiveAllocatorMode == "enforce" {
		view.Note = "Турникет мгновенно пропускает подтверждённо опасную попытку и продолжает соседний маршрут; неизвестные и устаревшие квоты fail-open, очередей нет."
	}
	now = now.UTC()
	adaptiveEdgeGateRuntime.Lock()
	defer adaptiveEdgeGateRuntime.Unlock()
	for authIndex, lease := range adaptiveEdgeGateRuntime.InFlight {
		if !lease.StartedAt.IsZero() && now.Sub(lease.StartedAt) >= adaptiveEdgeGateMaximumLeaseAge {
			delete(adaptiveEdgeGateRuntime.InFlight, authIndex)
			continue
		}
		if filter {
			if _, ok := allowed[authIndex]; !ok {
				continue
			}
		}
		view.InFlightGuards++
	}
	for _, breaker := range adaptiveEdgeGateRuntime.Breakers {
		if filter {
			if _, ok := allowed[breaker.AuthIndex]; !ok {
				continue
			}
		}
		view.TrackedBreakers++
		if breaker.ProbeInFlight || breaker.RecoveryInFlight {
			view.HalfOpenProbes++
		}
	}
	view.Saturated = adaptiveEdgeGateRuntime.Saturated
	view.DroppedLeases = adaptiveEdgeGateRuntime.DroppedLeases
	view.DroppedBreakers = adaptiveEdgeGateRuntime.DroppedBreakers
	return view
}

func resetAdaptiveEdgeGateForTest() {
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.InFlight = make(map[string]adaptiveEdgeGateLease)
	adaptiveEdgeGateRuntime.Breakers = make(map[string]adaptiveEdgeGateBreaker)
	adaptiveEdgeGateRuntime.Saturated = false
	adaptiveEdgeGateRuntime.DroppedLeases = 0
	adaptiveEdgeGateRuntime.DroppedBreakers = 0
	adaptiveEdgeGateRuntime.NextGeneration = 0
	adaptiveEdgeGateRuntime.NextLeaseID = 0
	adaptiveEdgeGateRuntime.NextEvidenceRevision = 0
	adaptiveEdgeGateRuntime.Unlock()
}
