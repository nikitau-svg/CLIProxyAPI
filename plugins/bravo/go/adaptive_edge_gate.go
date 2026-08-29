package main

import (
	"math"
	"strings"
	"sync"
	"time"
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

	quotaConfirmed         bool
	sessionHeadroomPercent float64
	weeklyHeadroomPercent  float64
	tripRemainingSeconds   int64
	outcomeTransition      string
	started                bool
	outcomeObserved        bool
	guardHeld              bool
	guardLeaseAt           time.Time
	probeBreakerKey        string
	enforce                bool
	quotaFresh             bool
	compactBypass          bool
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
	AuthIndex      string
	Provider       string
	Model          string
	Until          time.Time
	ProbeInFlight  bool
	ProbeStartedAt time.Time
}

type adaptiveEdgeGateLease struct {
	StartedAt time.Time
}

var adaptiveEdgeGateRuntime = struct {
	sync.Mutex
	InFlight        map[string]adaptiveEdgeGateLease
	Breakers        map[string]adaptiveEdgeGateBreaker
	Saturated       bool
	DroppedLeases   uint64
	DroppedBreakers uint64
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
		enforce:                cfg.AdaptiveAllocatorMode == "enforce",
		quotaFresh:             quotaFreshnessAt(quota, attempt.Candidate.Model, cfg, now) == quotaFreshnessFresh,
		compactBypass:          attempt.CompactBypass,
	}
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
		if breaker.Until.After(now) {
			state.state = adaptiveEdgeGateStateTripped
			state.decision = adaptiveEdgeGateDecisionSkipTripped
			state.reason = adaptiveEdgeGateBreakerReason(breaker)
			state.tripRemainingSeconds = adaptiveEdgeGateRemainingSeconds(breaker.Until, now)
			return
		}
		if adaptiveEdgeGateLeaseBusyLocked(state.authIndex, now) || breaker.ProbeInFlight {
			state.state = adaptiveEdgeGateStateHalfOpen
			state.decision = adaptiveEdgeGateDecisionSkipBusy
			state.reason = "half_open_probe_busy"
			return
		}
		if !adaptiveEdgeGateAcquireLeaseLocked(state, now) {
			state.state = adaptiveEdgeGateStateHalfOpen
			state.decision = adaptiveEdgeGateDecisionDispatch
			state.reason = "runtime_saturated_fail_open"
			return
		}
		breaker.ProbeInFlight = true
		breaker.ProbeStartedAt = now
		adaptiveEdgeGateRuntime.Breakers[breakerKey] = breaker
		state.state = adaptiveEdgeGateStateHalfOpen
		state.decision = adaptiveEdgeGateDecisionProbe
		state.reason = adaptiveEdgeGateBreakerReason(breaker)
		state.probeBreakerKey = breakerKey
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
		if breaker, ok := adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey]; ok {
			breaker.ProbeInFlight = false
			breaker.ProbeStartedAt = time.Time{}
			adaptiveEdgeGateRuntime.Breakers[state.probeBreakerKey] = breaker
		}
		state.probeBreakerKey = ""
	}
}

func adaptiveEdgeGateActiveBreakerLocked(
	state *adaptiveEdgeGateAttemptState,
	now time.Time,
) (string, adaptiveEdgeGateBreaker, bool) {
	for _, key := range []string{
		adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, ""),
		adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, state.model),
	} {
		breaker, ok := adaptiveEdgeGateRuntime.Breakers[key]
		if !ok {
			continue
		}
		if breaker.ProbeInFlight && !breaker.ProbeStartedAt.IsZero() &&
			now.Sub(breaker.ProbeStartedAt) >= adaptiveEdgeGateMaximumLeaseAge {
			breaker.ProbeInFlight = false
			breaker.ProbeStartedAt = time.Time{}
			adaptiveEdgeGateRuntime.Breakers[key] = breaker
		}
		return key, breaker, true
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
	if state.probeBreakerKey != "" {
		delete(adaptiveEdgeGateRuntime.Breakers, state.probeBreakerKey)
		if !quotaFailure {
			state.outcomeTransition = "reopened"
			return
		}
	}
	if !quotaFailure {
		state.outcomeTransition = "unchanged"
		return
	}

	until := failureCooldownUntil(failure, now)
	model := state.model
	explicitModelScope := failure.Provider != nil &&
		strings.EqualFold(strings.TrimSpace(failure.Provider.Scope), "model")
	if !explicitModelScope && (failure.AccountWide || accountWideCooldownStatus(failure.Status)) {
		model = ""
	}
	breaker := adaptiveEdgeGateBreaker{
		AuthIndex: state.authIndex,
		Provider:  state.provider,
		Model:     model,
		Until:     until.UTC(),
	}
	key := adaptiveEdgeGateBreakerKey(state.provider, state.authIndex, model)
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
	if failure.Status == 429 || adaptiveShadowAuditQuotaFailure(failure.Code) {
		return true
	}
	if failure.Provider == nil {
		return false
	}
	for _, value := range []string{
		failure.Provider.Code,
		failure.Provider.Type,
		failure.Provider.Reason,
		failure.Provider.Class,
	} {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.Contains(value, "quota") || strings.Contains(value, "rate_limit") ||
			strings.Contains(value, "usage_limit") || strings.Contains(value, "credits_exhausted") {
			return true
		}
	}
	return false
}

func adaptiveEdgeGateBreakerKey(provider, authIndex, model string) string {
	return normalizeProvider(provider) + "\x00" + strings.TrimSpace(authIndex) + "\x00" + baseModelKey(strings.TrimSpace(model))
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
		RoutingEnforced:            cfg.AdaptiveAllocatorMode == "enforce",
		QueuesRequests:             false,
		AdditionalProviderRequests: false,
		SessionGuardPercent:        adaptiveEdgeGateSessionGuardPercent,
		WeeklyGuardPercent:         adaptiveEdgeGateWeeklyGuardPercent,
		Note:                       "Турникет моделирует мгновенный переход к следующему маршруту; он не ждёт, не ставит запросы в очередь и не добавляет обращений к провайдеру.",
	}
	if cfg.AdaptiveAllocatorMode == "off" {
		view.Note = "Shadow-турникет отключён вместе с адаптивным наблюдением."
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
		if breaker.ProbeInFlight {
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
	adaptiveEdgeGateRuntime.Unlock()
}
