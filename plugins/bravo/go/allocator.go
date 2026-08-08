package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type allocatorRuntimeState struct {
	sync.Mutex
	InFlightPercent       map[string]float64
	PendingPercent        map[string]float64
	OrphanPreparedPercent map[string]float64
	InFlightRequests      map[string]int
	PendingRequests       map[string]int
}

var allocatorRuntime = allocatorRuntimeState{
	InFlightPercent:       make(map[string]float64),
	PendingPercent:        make(map[string]float64),
	OrphanPreparedPercent: make(map[string]float64),
	InFlightRequests:      make(map[string]int),
	PendingRequests:       make(map[string]int),
}

type allocatorObserveShadowState struct {
	InFlight         float64
	Pending          float64
	Updated          time.Time
	QuotaConfirmedAt time.Time
	Commits          []allocatorObserveShadowCommit
}

type allocatorObserveShadowCommit struct {
	Percent float64
	At      time.Time
}

const (
	allocatorObserveMaximumAccounts = 4096
	allocatorObservePendingTTL      = 15 * time.Minute
	allocatorObserveMaximumCommits  = 64
)

var allocatorObserveRuntime = struct {
	sync.Mutex
	Accounts map[string]allocatorObserveShadowState
}{Accounts: make(map[string]allocatorObserveShadowState)}

var acquireAttemptLeaseBeforeAdmissionHook func()

const allocatorAmberWakeCooldown = 30 * time.Second

var allocatorAmberWakeLast atomic.Int64
var allocatorQuotaPollingWake = wakeQuotaPolling

func authenticatedExecutionProject(req rpcExecutorRequest, cfg pluginConfig) (smartKeyConfig, bool) {
	if project, ok := smartKeyFromMetadata(req.Metadata, cfg); ok {
		return project, true
	}
	if plaintext := requestCredential(req.Headers, req.Query); plaintext != "" {
		return matchSmartKey(cfg, plaintext)
	}
	return smartKeyConfig{}, false
}

func allocateCandidateAuths(
	req rpcExecutorRequest,
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	sticky string,
) []executionAttempt {
	requestShape := buildAdaptiveRequestShape(executionBodyView(req), item)
	return allocateCandidateAuthsForShape(cfg, project, item, auths, sticky, requestShape)
}

func allocateCandidateAuthsForShape(
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	sticky string,
	requestShape adaptiveRequestShape,
) []executionAttempt {
	now := time.Now()
	bravoProjectDemand.maintain(now)
	primaryIndexes := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	primary := make([]executionAttempt, 0, len(auths))
	secondary := make([]executionAttempt, 0, len(auths))
	secondarySurplus := make(map[string]float64, len(auths))
	amber := false
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		subscription := subscriptionPolicy(cfg, authIndex)
		if !subscriptionEnabled(subscription) {
			continue
		}
		quota := normalizedQuotaState(quotaSnapshot(authIndex))
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
		reservation := adaptiveReservationForShape(auth, tariff, requestShape, now)
		profileKey := adaptiveProfileKey(authIndex, requestShape)
		_, isPrimary := primaryIndexes[authIndex]
		attempt := executionAttempt{
			LogicalModel:            "",
			Candidate:               item,
			Auth:                    auth,
			ProjectID:               project.ID,
			Primary:                 isPrimary,
			AllocatorManaged:        cfg.AllocatorMode == "enforce",
			ReservationPercent:      reservation,
			AdaptiveReserveKey:      profileKey,
			AdaptiveRequestShape:    requestShape,
			AdaptiveBaselinePercent: tariff.ReservationPercent,
			TariffID:                tariff.ID,
		}
		if isPrimary {
			primary = append(primary, attempt)
			continue
		}
		attempt.DemandGuardPercent = projectDemandGuard(cfg, attempt, now)
		if secondaryQuotaEligibleWithKey(cfg, quota, item.Model, tariff, authIndex, profileKey, reservation, attempt.DemandGuardPercent, now) {
			secondary = append(secondary, attempt)
			surplus := allocatorSafeSurplus(cfg, attempt, now)
			secondarySurplus[authIndex] = surplus
			if surplus > 0 && surplus <= 5 {
				amber = true
			}
		}
	}
	if amber {
		maybeWakeQuotaPollingForAmber(now)
	}

	orderAuthAttempts(primary, sticky)
	demandView := captureProjectDemandView(cfg, project.ID, secondary, now)
	sort.SliceStable(secondary, func(i, j int) bool {
		leftSurplus := secondarySurplus[stableAuthIndex(secondary[i].Auth)]
		rightSurplus := secondarySurplus[stableAuthIndex(secondary[j].Auth)]
		if math.Abs(leftSurplus-rightSurplus) > 0.000001 {
			return leftSurplus > rightSurplus
		}
		left := allocatorStress(cfg, secondary[i]) + demandView.penalty(secondary[i])
		right := allocatorStress(cfg, secondary[j]) + demandView.penalty(secondary[j])
		if math.Abs(left-right) > 0.000001 {
			return left < right
		}
		leftTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(secondary[i].Auth))
		rightTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(secondary[j].Auth))
		if leftTie == rightTie {
			return stableAuthIndex(secondary[i].Auth) < stableAuthIndex(secondary[j].Auth)
		}
		return leftTie > rightTie
	})
	return append(primary, secondary...)
}

// observeAllocatorAttempts keeps the legacy provider execution order while
// attaching the decision the adaptive allocator would have made. It installs
// no lease and never changes eligibility: observe mode is therefore suitable
// for canary comparison without altering provider traffic.
func observeAllocatorAttempts(
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	allocated []executionAttempt,
	requestShape adaptiveRequestShape,
) []executionAttempt {
	now := time.Now()
	byAuth := make(map[string]executionAttempt, len(allocated))
	for _, attempt := range allocated {
		byAuth[stableAuthIndex(attempt.Auth)] = attempt
	}
	primaryIndexes := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	attempts := make([]executionAttempt, 0, len(auths))
	for _, auth := range auths {
		authIndex := stableAuthIndex(auth)
		attempt, wouldAdmit := byAuth[authIndex]
		if !wouldAdmit {
			quota := normalizedQuotaState(quotaSnapshot(authIndex))
			subscription := subscriptionPolicy(cfg, authIndex)
			tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
			reservation := adaptiveReservationForShape(auth, tariff, requestShape, now)
			_, primary := primaryIndexes[authIndex]
			attempt = executionAttempt{
				Candidate: item, Auth: auth, ProjectID: project.ID, Primary: primary,
				ReservationPercent: reservation, AdaptiveReserveKey: adaptiveProfileKey(authIndex, requestShape),
				AdaptiveRequestShape: requestShape, AdaptiveBaselinePercent: tariff.ReservationPercent,
				TariffID: tariff.ID,
			}
		}
		attempt.AllocatorManaged = false
		attempt.AllocatorObserve = true
		attempt.AdaptiveTrace = captureObserveAdaptiveDecision(cfg, attempt, wouldAdmit, now)
		attempts = append(attempts, attempt)
	}
	return attempts
}

func captureObserveAdaptiveDecision(cfg pluginConfig, attempt executionAttempt, wouldAdmit bool, now time.Time) adaptiveRouteDecision {
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	confidence := quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now)
	allocatorRuntime.Lock()
	inFlight := allocatorRuntime.InFlightPercent[authIndex]
	pending := allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	sessionGuard, weeklyGuard, demandGuard := 0.0, 0.0, 0.0
	if !attempt.Primary {
		sessionGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowSession, cfg, now)
		weeklyGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowWeekly, cfg, now)
		demandGuard = projectDemandGuard(cfg, attempt, now)
	}
	decision := captureAdaptiveAdmissionDecision(
		attempt, cfg, quota, inFlight, pending, sessionGuard, weeklyGuard, demandGuard,
		confidence, adaptiveAdmissionRejectionForState(attempt, authIndex, confidence, !wouldAdmit), wouldAdmit, now,
	)
	decision.mode = "observe"
	if !wouldAdmit {
		switch {
		case adaptiveRoutingSaturated.Load():
			decision.rejectionCause = adaptiveRejectionLedgerSaturated
			decision.rejection = "adaptive_ledger_saturated"
		case adaptiveEstimatorIdentitySaturated(authIndex):
			decision.rejectionCause = adaptiveRejectionEstimatorSaturated
			decision.rejection = "adaptive_estimator_saturated"
		case !subscriptionEnabled(subscriptionPolicy(cfg, authIndex)):
			decision.rejection = "adaptive_subscription_disabled"
		}
	}
	return decision
}

// acquireObserveShadowLease performs the same local admission arithmetic as
// enforce mode in an isolated, bounded ledger. The returned execution lease is
// always acquired: shadow rejections affect only trace/canary evidence and
// never change legacy provider order or traffic.
func acquireObserveShadowLease(attempt executionAttempt) (func(bool), executionAttempt) {
	now := time.Now()
	cfg := loadedConfig()
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	confidence := quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now)
	tariff := tariffByID(cfg, attempt.TariffID)
	if attempt.AdaptiveRequestShape.Multiplier > 0 {
		baseline := tariff
		if attempt.AdaptiveBaselinePercent > 0 {
			baseline.ReservationPercent = attempt.AdaptiveBaselinePercent
		}
		attempt.ReservationPercent = math.Max(attempt.ReservationPercent,
			adaptiveReservationForShape(attempt.Auth, baseline, attempt.AdaptiveRequestShape, now))
	}
	allocatorRuntime.Lock()
	realInFlight := allocatorRuntime.InFlightPercent[authIndex]
	realPending := allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	sessionGuard, weeklyGuard, demandGuard := 0.0, 0.0, 0.0
	if !attempt.Primary {
		sessionGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowSession, cfg, now)
		weeklyGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowWeekly, cfg, now)
		demandGuard = projectDemandGuard(cfg, attempt, now)
	}

	allocatorObserveRuntime.Lock()
	shadow, tracked := allocatorObserveRuntime.Accounts[authIndex]
	if tracked && now.Sub(shadow.Updated) >= allocatorObservePendingTTL && shadow.InFlight <= 0 && shadow.Pending <= 0 {
		delete(allocatorObserveRuntime.Accounts, authIndex)
		shadow, tracked = allocatorObserveShadowState{}, false
	}
	confirmedAt := quotaConfirmedAt(quota)
	trackable := tracked || len(allocatorObserveRuntime.Accounts) < allocatorObserveMaximumAccounts
	inFlight := realInFlight + shadow.InFlight
	pending := realPending + shadow.Pending
	wouldAdmit := trackable
	if !attempt.Primary && (adaptiveRoutingSaturated.Load() || adaptiveEstimatorIdentitySaturated(authIndex)) {
		wouldAdmit = false
	}
	if confidence != "confirmed" {
		// Mirror enforce mode exactly: a pinned primary remains available while
		// discovery catches up; unknown secondaries fail closed.
		wouldAdmit = wouldAdmit && attempt.Primary
	} else if wouldAdmit {
		session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
		sessionFloor, weeklyFloor := tariff.SessionFloorPercent, tariff.WeeklyFloorPercent
		if attempt.Primary {
			sessionFloor, weeklyFloor = 0, 0
		}
		reserved := inFlight + pending
		wouldAdmit = quotaWindowSafeSurplus(session, sessionFloor, reserved, attempt.ReservationPercent, sessionGuard, demandGuard) > 0 &&
			quotaWindowSafeSurplus(weekly, weeklyFloor, reserved, attempt.ReservationPercent, weeklyGuard, demandGuard) > 0
	}
	cause := adaptiveAdmissionRejectionForState(attempt, authIndex, confidence, !wouldAdmit)
	if !trackable {
		cause = adaptiveRejectionDemandSaturated
	}
	attempt.AdaptiveTrace = captureAdaptiveAdmissionDecision(
		attempt, cfg, quota, inFlight, pending, sessionGuard, weeklyGuard, demandGuard,
		confidence, cause, wouldAdmit, now,
	)
	attempt.AdaptiveTrace.mode = "observe"
	if wouldAdmit {
		shadow.InFlight += attempt.ReservationPercent
		shadow.Updated = now.UTC()
		if shadow.QuotaConfirmedAt.IsZero() || confirmedAt.After(shadow.QuotaConfirmedAt) {
			shadow.QuotaConfirmedAt = confirmedAt
		}
		allocatorObserveRuntime.Accounts[authIndex] = shadow
	}
	allocatorObserveRuntime.Unlock()

	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			if !wouldAdmit {
				return
			}
			allocatorObserveRuntime.Lock()
			current := allocatorObserveRuntime.Accounts[authIndex]
			current.InFlight -= attempt.ReservationPercent
			if current.InFlight < 0 {
				current.InFlight = 0
			}
			if commit {
				current.Pending += attempt.ReservationPercent
				current.Commits = append(current.Commits, allocatorObserveShadowCommit{
					Percent: attempt.ReservationPercent,
					At:      time.Now().UTC(),
				})
				if len(current.Commits) > allocatorObserveMaximumCommits {
					mergeCount := len(current.Commits) - allocatorObserveMaximumCommits + 1
					merged := allocatorObserveShadowCommit{}
					for _, item := range current.Commits[:mergeCount] {
						merged.Percent += item.Percent
						if item.At.After(merged.At) {
							merged.At = item.At
						}
					}
					current.Commits = append([]allocatorObserveShadowCommit{merged}, current.Commits[mergeCount:]...)
				}
			}
			current.Updated = time.Now().UTC()
			allocatorObserveRuntime.Accounts[authIndex] = current
			allocatorObserveRuntime.Unlock()
		})
	}, attempt
}

func resetAllocatorObserveRuntime() {
	allocatorObserveRuntime.Lock()
	allocatorObserveRuntime.Accounts = make(map[string]allocatorObserveShadowState)
	allocatorObserveRuntime.Unlock()
}

// reconcileAllocatorObservePending clears only the shadow debt captured before
// a provider refresh began. Commits made while the fetch was in flight stay in
// the bounded FIFO, matching the production allocator's acquisition watermark.
func reconcileAllocatorObservePending(authIndex string, captured float64, confirmedAt time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || captured <= 0 {
		return
	}
	allocatorObserveRuntime.Lock()
	state, exists := allocatorObserveRuntime.Accounts[authIndex]
	if !exists {
		allocatorObserveRuntime.Unlock()
		return
	}
	remaining := math.Min(captured, state.Pending)
	for remaining > 0 && len(state.Commits) > 0 {
		if state.Commits[0].Percent <= remaining+1e-12 {
			remaining -= state.Commits[0].Percent
			state.Pending -= state.Commits[0].Percent
			state.Commits = state.Commits[1:]
			continue
		}
		state.Commits[0].Percent -= remaining
		state.Pending -= remaining
		remaining = 0
	}
	if state.Pending < 1e-12 {
		state.Pending = 0
	}
	if confirmedAt.After(state.QuotaConfirmedAt) {
		state.QuotaConfirmedAt = confirmedAt.UTC()
	}
	state.Updated = time.Now().UTC()
	allocatorObserveRuntime.Accounts[authIndex] = state
	allocatorObserveRuntime.Unlock()
}

func allocatorSafeSurplus(cfg pluginConfig, attempt executionAttempt, now time.Time) float64 {
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	quota := quotaSnapshot(authIndex)
	if quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now) != "confirmed" {
		return -100
	}
	tariff := tariffByID(cfg, attempt.TariffID)
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[authIndex] + allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	sessionGuard := adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowSession, cfg, now)
	weeklyGuard := adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowWeekly, cfg, now)
	sessionSurplus := quotaWindowSafeSurplus(session, tariff.SessionFloorPercent, reserved, attempt.ReservationPercent, sessionGuard, attempt.DemandGuardPercent)
	weeklySurplus := quotaWindowSafeSurplus(weekly, tariff.WeeklyFloorPercent, reserved, attempt.ReservationPercent, weeklyGuard, attempt.DemandGuardPercent)
	return math.Min(sessionSurplus, weeklySurplus)
}

func maybeWakeQuotaPollingForAmber(now time.Time) {
	nowNanos := now.UTC().UnixNano()
	for {
		previous := allocatorAmberWakeLast.Load()
		if previous != 0 && nowNanos-previous < int64(allocatorAmberWakeCooldown) {
			return
		}
		if allocatorAmberWakeLast.CompareAndSwap(previous, nowNanos) {
			allocatorQuotaPollingWake()
			return
		}
	}
}

func resolvedPrimaryAuthIndexes(configured []string, auths []pluginapi.HostAuthFileEntry) map[string]struct{} {
	resolved := make(map[string]struct{}, len(configured))
	for _, raw := range configured {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.AuthIndex) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.ID) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.Name) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
	nextPrimary:
	}
	return resolved
}

func orderAuthAttempts(attempts []executionAttempt, sticky string) {
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Auth.Priority != attempts[j].Auth.Priority {
			return attempts[i].Auth.Priority > attempts[j].Auth.Priority
		}
		left := rendezvousScore(sticky, attempts[i].Candidate.Provider, attempts[i].Candidate.Model, stableAuthIndex(attempts[i].Auth))
		right := rendezvousScore(sticky, attempts[j].Candidate.Provider, attempts[j].Candidate.Model, stableAuthIndex(attempts[j].Auth))
		if left == right {
			return stableAuthIndex(attempts[i].Auth) < stableAuthIndex(attempts[j].Auth)
		}
		return left > right
	})
}

func stableAuthIndex(auth pluginapi.HostAuthFileEntry) string {
	if value := strings.TrimSpace(auth.AuthIndex); value != "" {
		return value
	}
	return authIdentifier(auth)
}

func secondaryQuotaEligible(
	cfg pluginConfig,
	quota credentialQuotaState,
	model string,
	tariff tariffConfig,
	authIndex string,
	reservation float64,
) bool {
	return secondaryQuotaEligibleWithKey(cfg, quota, model, tariff, authIndex, "", reservation, 0, time.Now())
}

func secondaryQuotaEligibleAt(
	cfg pluginConfig,
	quota credentialQuotaState,
	model string,
	tariff tariffConfig,
	authIndex string,
	reservation float64,
	now time.Time,
) bool {
	return secondaryQuotaEligibleWithKey(cfg, quota, model, tariff, authIndex, "", reservation, 0, now)
}

func secondaryQuotaEligibleWithKey(
	cfg pluginConfig,
	quota credentialQuotaState,
	model string,
	tariff tariffConfig,
	authIndex, profileKey string,
	reservation, demandGuard float64,
	now time.Time,
) bool {
	if adaptiveRoutingSaturated.Load() {
		return false
	}
	if quotaRoutingConfidenceAt(quota, model, cfg, now) != "confirmed" {
		return cfg.AllocatorMode != "enforce" && cfg.UnknownSecondaryPolicy == "allow"
	}
	session, weekly := effectiveQuotaWindows(quota, model)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[strings.TrimSpace(authIndex)] +
		allocatorRuntime.PendingPercent[strings.TrimSpace(authIndex)]
	allocatorRuntime.Unlock()
	sessionGuard := adaptiveExposureGuard(authIndex, profileKey, quota, adaptiveWindowSession, cfg, now)
	weeklyGuard := adaptiveExposureGuard(authIndex, profileKey, quota, adaptiveWindowWeekly, cfg, now)
	return quotaWindowSafeSurplus(session, tariff.SessionFloorPercent, reserved, reservation, sessionGuard, demandGuard) > 0 &&
		quotaWindowSafeSurplus(weekly, tariff.WeeklyFloorPercent, reserved, reservation, weeklyGuard, demandGuard) > 0
}

func allocatorStress(cfg pluginConfig, attempt executionAttempt) float64 {
	quota := quotaSnapshot(attempt.Auth.AuthIndex)
	tariff := tariffByID(cfg, attempt.TariffID)
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	minHeadroom := 1.0
	if quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, time.Now()) == "confirmed" {
		sessionHeadroom := 1.0
		if quotaWindowApplicable(session) {
			sessionHeadroom = normalizedHeadroom(session.RemainingPercent, tariff.SessionFloorPercent)
		}
		weeklyHeadroom := 1.0
		if quotaWindowApplicable(weekly) {
			weeklyHeadroom = normalizedHeadroom(weekly.RemainingPercent, tariff.WeeklyFloorPercent)
		}
		minHeadroom = math.Min(sessionHeadroom, weeklyHeadroom)
	}
	usage := authUsageSummary(attempt.Auth.AuthIndex, time.Now())
	usagePressure := math.Log1p(float64(usage.Weekly.TotalTokens)) / math.Max(tariff.Multiplier, 1)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[strings.TrimSpace(attempt.Auth.AuthIndex)] +
		allocatorRuntime.PendingPercent[strings.TrimSpace(attempt.Auth.AuthIndex)]
	allocatorRuntime.Unlock()
	return (1-minHeadroom)*100 + usagePressure + reserved
}

func normalizedHeadroom(remaining, floor float64) float64 {
	if remaining <= floor {
		return 0
	}
	if floor >= 100 {
		return 0
	}
	return math.Min((remaining-floor)/(100-floor), 1)
}

func acquireAttemptLease(attempt executionAttempt) (func(bool), bool) {
	release, acquired, _ := acquireAttemptLeaseDetailed(attempt)
	return release, acquired
}

func acquireAttemptLeaseDetailed(attempt executionAttempt) (func(bool), bool, executionAttempt) {
	if !attempt.AllocatorManaged || strings.TrimSpace(attempt.Auth.AuthIndex) == "" {
		return func(bool) {}, true, attempt
	}
	if acquireAttemptLeaseBeforeAdmissionHook != nil {
		acquireAttemptLeaseBeforeAdmissionHook()
	}
	adaptiveAdmissionMu.RLock()
	defer adaptiveAdmissionMu.RUnlock()
	cfg := loadedConfig()
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	tariff := tariffByID(cfg, attempt.TariffID)
	if attempt.AdaptiveRequestShape.Multiplier > 0 {
		baselineTariff := tariff
		if attempt.AdaptiveBaselinePercent > 0 {
			baselineTariff.ReservationPercent = attempt.AdaptiveBaselinePercent
		}
		attempt.ReservationPercent = math.Max(
			attempt.ReservationPercent,
			adaptiveReservationForShape(attempt.Auth, baselineTariff, attempt.AdaptiveRequestShape, time.Now()),
		)
	}
	allocatorRuntime.Lock()
	now := time.Now()
	quota := quotaSnapshot(authIndex)
	confidence := quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now)
	inFlight := allocatorRuntime.InFlightPercent[authIndex]
	pending := allocatorRuntime.PendingPercent[authIndex]
	sessionGuard, weeklyGuard, demandGuard := 0.0, 0.0, 0.0
	if !attempt.Primary {
		sessionGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowSession, cfg, now)
		weeklyGuard = adaptiveExposureGuard(authIndex, attempt.AdaptiveReserveKey, quota, adaptiveWindowWeekly, cfg, now)
		demandGuard = projectDemandGuard(cfg, attempt, now)
	}
	rejected := !attempt.Primary && (adaptiveRoutingSaturated.Load() || adaptiveEstimatorIdentitySaturated(authIndex))
	if confidence != "confirmed" {
		// An unknown snapshot only blocks secondaries; a pinned primary is
		// still trusted while quota discovery catches up.
		if !attempt.Primary {
			rejected = true
		}
	} else {
		session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
		reserved := inFlight + pending
		// Being primary grants priority and the right to spend the reserve
		// below the configured floor — it is not an exemption from the quota
		// itself. Without this a pinned credential keeps being retried at 0%
		// remaining, which only produces upstream rate limits.
		sessionFloor, weeklyFloor := tariff.SessionFloorPercent, tariff.WeeklyFloorPercent
		if attempt.Primary || attempt.CompactBypass {
			sessionFloor, weeklyFloor = 0, 0
		}
		if quotaWindowSafeSurplus(session, sessionFloor, reserved, attempt.ReservationPercent, sessionGuard, demandGuard) <= 0 ||
			quotaWindowSafeSurplus(weekly, weeklyFloor, reserved, attempt.ReservationPercent, weeklyGuard, demandGuard) <= 0 {
			rejected = true
		}
	}
	attempt.AdaptiveTrace = captureAdaptiveAdmissionDecision(
		attempt, cfg, quota, inFlight, pending, sessionGuard, weeklyGuard, demandGuard,
		confidence, adaptiveAdmissionRejectionForState(attempt, authIndex, confidence, rejected), !rejected, now,
	)
	if rejected {
		allocatorRuntime.Unlock()
		return func(bool) {}, false, attempt
	}
	allocatorRuntime.InFlightPercent[authIndex] += attempt.ReservationPercent
	allocatorRuntime.InFlightRequests[authIndex]++
	allocatorRuntime.Unlock()
	preparedAt := time.Now()
	prepareOK, prepareRejection := persistAdaptivePrepareDetailed(authIndex, attempt.ReservationPercent, preparedAt)
	if !prepareOK {
		allocatorRuntime.Lock()
		allocatorRuntime.InFlightPercent[authIndex] -= attempt.ReservationPercent
		if allocatorRuntime.InFlightPercent[authIndex] <= 0 {
			delete(allocatorRuntime.InFlightPercent, authIndex)
		}
		allocatorRuntime.InFlightRequests[authIndex]--
		if allocatorRuntime.InFlightRequests[authIndex] <= 0 {
			delete(allocatorRuntime.InFlightRequests, authIndex)
		}
		allocatorRuntime.Unlock()
		attempt = markAdaptiveAdmissionRejected(attempt, prepareRejection)
		return func(bool) {}, false, attempt
	}
	trackPending := adaptiveDurableLedgerTracksAuth(authIndex)
	demandRelease := beginProjectDemandLease(attempt, time.Now())

	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			adaptiveAdmissionMu.RLock()
			defer adaptiveAdmissionMu.RUnlock()
			allocatorRuntime.Lock()
			allocatorRuntime.InFlightPercent[authIndex] -= attempt.ReservationPercent
			if allocatorRuntime.InFlightPercent[authIndex] <= 0 {
				delete(allocatorRuntime.InFlightPercent, authIndex)
			}
			allocatorRuntime.InFlightRequests[authIndex]--
			if allocatorRuntime.InFlightRequests[authIndex] <= 0 {
				delete(allocatorRuntime.InFlightRequests, authIndex)
			}
			if commit && trackPending {
				allocatorRuntime.PendingPercent[authIndex] += attempt.ReservationPercent
				allocatorRuntime.PendingRequests[authIndex]++
			}
			allocatorRuntime.Unlock()
			finalizedAt := time.Now()
			if commit {
				committedAt := finalizedAt
				recordAdaptiveReservationCommitForKey(authIndex, attempt.AdaptiveReserveKey, attempt.ReservationPercent, committedAt)
			}
			persistAdaptiveFinalize(authIndex, attempt.ReservationPercent, commit, finalizedAt)
			demandRelease(commit, time.Now())
		})
	}, true, attempt
}

func adaptiveAdmissionRejectionForState(
	attempt executionAttempt,
	authIndex, confidence string,
	rejected bool,
) adaptiveAdmissionRejectionCause {
	if !rejected {
		return adaptiveRejectionNone
	}
	if adaptiveRoutingSaturated.Load() {
		return adaptiveRejectionLedgerSaturated
	}
	if confidence != "confirmed" && !attempt.Primary {
		return adaptiveRejectionQuotaStale
	}
	if adaptiveEstimatorIsSaturated(authIndex) {
		return adaptiveRejectionEstimatorSaturated
	}
	if adaptiveDemandIsSaturated(authIndex) {
		return adaptiveRejectionDemandSaturated
	}
	return adaptiveRejectionNone
}

func pendingReservationPercent(authIndex string) float64 {
	allocatorRuntime.Lock()
	defer allocatorRuntime.Unlock()
	return allocatorRuntime.PendingPercent[strings.TrimSpace(authIndex)]
}

func clearPendingReservation(authIndex string, amount float64, captured ...adaptiveRefreshWatermark) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || amount <= 0 {
		return
	}
	allocatorRuntime.Lock()
	actual := math.Min(amount, allocatorRuntime.PendingPercent[authIndex])
	pendingBefore := allocatorRuntime.PendingPercent[authIndex]
	orphanAtStart := 0.0
	if len(captured) > 0 {
		orphanAtStart = captured[0].OrphanPreparedPercent
	}
	orphanClear := math.Min(actual, math.Min(orphanAtStart, allocatorRuntime.OrphanPreparedPercent[authIndex]))
	pendingClear := actual - orphanClear
	allocatorRuntime.PendingPercent[authIndex] -= actual
	if allocatorRuntime.PendingPercent[authIndex] <= 0 {
		delete(allocatorRuntime.PendingPercent, authIndex)
		delete(allocatorRuntime.PendingRequests, authIndex)
	} else if actual > 0 && pendingBefore > 0 && allocatorRuntime.PendingRequests[authIndex] > 0 {
		remainingRatio := allocatorRuntime.PendingPercent[authIndex] / pendingBefore
		allocatorRuntime.PendingRequests[authIndex] = int(math.Ceil(float64(allocatorRuntime.PendingRequests[authIndex]) * remainingRatio))
	}
	allocatorRuntime.OrphanPreparedPercent[authIndex] -= orphanClear
	if allocatorRuntime.OrphanPreparedPercent[authIndex] <= 0 {
		delete(allocatorRuntime.OrphanPreparedPercent, authIndex)
	}
	allocatorRuntime.Unlock()
	persistAdaptivePendingClear(authIndex, pendingClear, orphanClear, time.Now())
}
