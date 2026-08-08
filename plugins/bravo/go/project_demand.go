package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// projectDemandTracker is a local, provider-I/O-free signal used both to order
// secondary credentials and to reserve near-term capacity for active primary
// owners. Its guard can only withhold secondary borrowing; it never authorizes
// spending through a tariff floor and primary traffic always receives zero guard.
type projectDemandTracker struct {
	mu               sync.Mutex
	halfLife         time.Duration
	projects         map[projectDemandLoanKey]*projectDemandSample
	loans            map[projectDemandLoanKey]*projectDemandSample
	projectOverflow  map[string]*projectDemandSample
	loanOverflow     map[string]*projectDemandSample
	projectBlocked   map[string]time.Time
	loanBlocked      map[string]time.Time
	projectSaturated bool
	loanSaturated    bool
	// Constant-size conservative counters cover leases that could not receive
	// either an exact or overflow sample after both bounded maps saturated.
	// Without them reconcile could clear fail-closed markers while such a
	// request was still executing.
	untrackedProjectInFlight int
	untrackedLoanInFlight    int
	lastPrune                time.Time
	pruneRuns                uint64
}

type projectDemandSample struct {
	activity     float64
	at           time.Time
	inFlight     int
	lastActivity time.Time
	buckets      [60]projectDemandMinuteBucket
}

type projectDemandMinuteBucket struct {
	minute   int64
	units    float64
	requests int
}

type projectDemandTempo struct {
	OneMinuteRequestsPerMinute   float64
	TenMinuteRequestsPerMinute   float64
	SixtyMinuteRequestsPerMinute float64
}

type projectDemandLoanKey struct {
	projectID string
	authIndex string
}

type projectDemandView struct {
	penalties map[string]float64
}

const (
	defaultProjectDemandHalfLife = 2 * time.Minute
	defaultDemandUnits           = 0.1
	ownerStandbyPenalty          = 0.5
	ownerActivityWeight          = 6.0
	ownerInFlightWeight          = 12.0
	borrowActivityWeight         = 2.0
	borrowInFlightWeight         = 4.0
	projectDemandStateTTL        = 2 * time.Hour
	projectDemandMaximumEntries  = 4096
	projectDemandMaximumOverflow = 4096
	projectDemandMaximumGuard    = 25.0
)

var bravoProjectDemand = newProjectDemandTracker(defaultProjectDemandHalfLife)

func newProjectDemandTracker(halfLife time.Duration) *projectDemandTracker {
	if halfLife <= 0 {
		halfLife = defaultProjectDemandHalfLife
	}
	return &projectDemandTracker{
		halfLife:        halfLife,
		projects:        make(map[projectDemandLoanKey]*projectDemandSample),
		loans:           make(map[projectDemandLoanKey]*projectDemandSample),
		projectOverflow: make(map[string]*projectDemandSample),
		loanOverflow:    make(map[string]*projectDemandSample),
		projectBlocked:  make(map[string]time.Time),
		loanBlocked:     make(map[string]time.Time),
	}
}

// begin is the lease-lifecycle integration hook. Call it only after the normal
// allocator lease was acquired, and invoke the returned closure exactly once
// when that attempt releases. The accepted flag is intentionally not used:
// successful and failed provider attempts both occupied capacity and therefore
// represent real demand.
func (tracker *projectDemandTracker) begin(attempt executionAttempt, now time.Time) func(bool, time.Time) {
	if tracker == nil {
		return func(bool, time.Time) {}
	}
	projectID := strings.TrimSpace(attempt.ProjectID)
	authIndex := strings.TrimSpace(stableAuthIndex(attempt.Auth))
	if projectID == "" || authIndex == "" {
		return func(bool, time.Time) {}
	}
	units := projectDemandUnits(attempt)
	now = normalizedDemandTime(now)

	tracker.mu.Lock()
	tracker.pruneIfDueLocked(now)
	key := projectDemandLoanKey{projectID: projectID, authIndex: authIndex}
	var projectSample, loanSample *projectDemandSample
	untrackedProject, untrackedLoan := false, false
	if attempt.Primary {
		projectSample = tracker.addProjectLocked(key, units, now)
		if projectSample == nil {
			tracker.untrackedProjectInFlight++
			untrackedProject = true
		}
	} else {
		loanSample = tracker.addLoanLocked(key, units, now)
		if loanSample == nil {
			tracker.untrackedLoanInFlight++
			untrackedLoan = true
		}
	}
	tracker.mu.Unlock()

	var once sync.Once
	return func(_ bool, releasedAt time.Time) {
		once.Do(func() {
			releasedAt = normalizedDemandTime(releasedAt)
			tracker.mu.Lock()
			tracker.releaseLocked(projectSample, releasedAt)
			tracker.releaseLocked(loanSample, releasedAt)
			if untrackedProject && tracker.untrackedProjectInFlight > 0 {
				tracker.untrackedProjectInFlight--
			}
			if untrackedLoan && tracker.untrackedLoanInFlight > 0 {
				tracker.untrackedLoanInFlight--
			}
			tracker.mu.Unlock()
		})
	}
}

func (tracker *projectDemandTracker) addProjectLocked(key projectDemandLoanKey, units float64, now time.Time) *projectDemandSample {
	sample := tracker.projects[key]
	if sample == nil {
		if len(tracker.projects) >= projectDemandMaximumEntries && !tracker.evictOldestProjectLocked(now) {
			sample = tracker.overflowSampleLocked(tracker.projectOverflow, tracker.projectBlocked, &tracker.projectSaturated, key.authIndex, now)
			tracker.addSampleLocked(sample, units, now)
			return sample
		}
		sample = &projectDemandSample{}
		tracker.projects[key] = sample
	}
	tracker.addSampleLocked(sample, units, now)
	return sample
}

func (tracker *projectDemandTracker) addLoanLocked(key projectDemandLoanKey, units float64, now time.Time) *projectDemandSample {
	sample := tracker.loans[key]
	if sample == nil {
		if len(tracker.loans) >= projectDemandMaximumEntries && !tracker.evictOldestLoanLocked(now) {
			sample = tracker.overflowSampleLocked(tracker.loanOverflow, tracker.loanBlocked, &tracker.loanSaturated, key.authIndex, now)
			tracker.addSampleLocked(sample, units, now)
			return sample
		}
		sample = &projectDemandSample{}
		tracker.loans[key] = sample
	}
	tracker.addSampleLocked(sample, units, now)
	return sample
}

func (tracker *projectDemandTracker) addSampleLocked(sample *projectDemandSample, units float64, now time.Time) {
	if sample == nil {
		return
	}
	tracker.decayLocked(sample, now)
	sample.activity += units
	sample.inFlight++
	sample.lastActivity = now
	addProjectDemandMinute(sample, units, now)
}

func addProjectDemandMinute(sample *projectDemandSample, units float64, now time.Time) {
	if sample == nil || units <= 0 {
		return
	}
	minute := now.UTC().Unix() / 60
	index := int(minute % int64(len(sample.buckets)))
	if index < 0 {
		index = -index
	}
	if sample.buckets[index].minute != minute {
		sample.buckets[index] = projectDemandMinuteBucket{minute: minute}
	}
	sample.buckets[index].units += units
	sample.buckets[index].requests++
}

func projectDemandWindowRate(sample *projectDemandSample, now time.Time, minutes int) float64 {
	if sample == nil || minutes <= 0 {
		return 0
	}
	current := now.UTC().Unix() / 60
	oldest := current - int64(minutes) + 1
	total := 0.0
	for _, bucket := range sample.buckets {
		if bucket.minute >= oldest && bucket.minute <= current {
			total += bucket.units
		}
	}
	return total / float64(minutes)
}

func projectDemandWindowRequestRate(sample *projectDemandSample, now time.Time, minutes int) float64 {
	if sample == nil || minutes <= 0 {
		return 0
	}
	current := now.UTC().Unix() / 60
	oldest := current - int64(minutes) + 1
	total := 0
	for _, bucket := range sample.buckets {
		if bucket.minute >= oldest && bucket.minute <= current {
			total += bucket.requests
		}
	}
	return float64(total) / float64(minutes)
}

func (tracker *projectDemandTracker) pruneLocked(now time.Time) {
	tracker.pruneRuns++
	for key, sample := range tracker.projects {
		if sample != nil && sample.inFlight == 0 && !sample.lastActivity.IsZero() && now.Sub(sample.lastActivity) > projectDemandStateTTL {
			delete(tracker.projects, key)
		}
	}
	for key, sample := range tracker.loans {
		if sample != nil && sample.inFlight == 0 && !sample.lastActivity.IsZero() && now.Sub(sample.lastActivity) > projectDemandStateTTL {
			delete(tracker.loans, key)
		}
	}
	for key, sample := range tracker.projectOverflow {
		if sample != nil && sample.inFlight == 0 && !sample.lastActivity.IsZero() && now.Sub(sample.lastActivity) > projectDemandStateTTL {
			delete(tracker.projectOverflow, key)
		}
	}
	for key, sample := range tracker.loanOverflow {
		if sample != nil && sample.inFlight == 0 && !sample.lastActivity.IsZero() && now.Sub(sample.lastActivity) > projectDemandStateTTL {
			delete(tracker.loanOverflow, key)
		}
	}
	// Blocked markers represent demand that could not be tracked because the
	// bounded overflow maps were full. Their age says nothing about whether a
	// long-running owner/loan finished, so TTL must never reopen borrowing or
	// erase its fairness penalty. They remain bounded and sticky; catastrophic
	// marker exhaustion escalates to the global fail-closed flags.
	for len(tracker.projects) > projectDemandMaximumEntries {
		if !tracker.evictOldestProjectLocked(now) {
			break
		}
	}
	for len(tracker.loans) > projectDemandMaximumEntries {
		if !tracker.evictOldestLoanLocked(now) {
			break
		}
	}
}

func (tracker *projectDemandTracker) pruneIfDueLocked(now time.Time) {
	if !tracker.lastPrune.IsZero() && now.Sub(tracker.lastPrune) < time.Minute {
		return
	}
	tracker.pruneLocked(now)
	tracker.lastPrune = now
}

func (tracker *projectDemandTracker) prune(now time.Time) {
	if tracker == nil {
		return
	}
	now = normalizedDemandTime(now)
	tracker.mu.Lock()
	tracker.pruneLocked(now)
	tracker.lastPrune = now
	tracker.mu.Unlock()
}

func (tracker *projectDemandTracker) maintain(now time.Time) {
	if tracker == nil {
		return
	}
	now = normalizedDemandTime(now)
	tracker.mu.Lock()
	tracker.pruneIfDueLocked(now)
	tracker.mu.Unlock()
}

func (tracker *projectDemandTracker) readyForSaturationReset() error {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.readyForSaturationResetLocked()
}

func (tracker *projectDemandTracker) resetSaturationAfterReconciliation() error {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if errReady := tracker.readyForSaturationResetLocked(); errReady != nil {
		return errReady
	}
	tracker.projectBlocked = make(map[string]time.Time)
	tracker.loanBlocked = make(map[string]time.Time)
	tracker.projectSaturated = false
	tracker.loanSaturated = false
	return nil
}

func (tracker *projectDemandTracker) readyForSaturationResetLocked() error {
	if tracker.untrackedProjectInFlight > 0 || tracker.untrackedLoanInFlight > 0 {
		return fmt.Errorf("project demand still contains untracked in-flight work")
	}
	for _, values := range []map[projectDemandLoanKey]*projectDemandSample{tracker.projects, tracker.loans} {
		for _, sample := range values {
			if sample != nil && sample.inFlight > 0 {
				return fmt.Errorf("project demand still contains in-flight work")
			}
		}
	}
	for _, values := range []map[string]*projectDemandSample{tracker.projectOverflow, tracker.loanOverflow} {
		for _, sample := range values {
			if sample != nil && sample.inFlight > 0 {
				return fmt.Errorf("project demand overflow still contains in-flight work")
			}
		}
	}
	return nil
}

func (tracker *projectDemandTracker) evictOldestProjectLocked(now time.Time) bool {
	var oldestKey projectDemandLoanKey
	found := false
	oldestAt := time.Time{}
	for key, sample := range tracker.projects {
		if sample == nil || sample.inFlight > 0 {
			continue
		}
		if !found || sample.lastActivity.Before(oldestAt) {
			oldestKey, oldestAt, found = key, sample.lastActivity, true
		}
	}
	if !found {
		return false
	}
	overflow := tracker.overflowSampleLocked(tracker.projectOverflow, tracker.projectBlocked, &tracker.projectSaturated, oldestKey.authIndex, now)
	tracker.mergeOverflowLocked(overflow, tracker.projects[oldestKey], now)
	delete(tracker.projects, oldestKey)
	return true
}

func (tracker *projectDemandTracker) evictOldestLoanLocked(now time.Time) bool {
	var oldestKey projectDemandLoanKey
	found := false
	oldestAt := time.Time{}
	for key, sample := range tracker.loans {
		if sample == nil || sample.inFlight > 0 {
			continue
		}
		if !found || sample.lastActivity.Before(oldestAt) {
			oldestKey, oldestAt, found = key, sample.lastActivity, true
		}
	}
	if found {
		overflow := tracker.overflowSampleLocked(tracker.loanOverflow, tracker.loanBlocked, &tracker.loanSaturated, oldestKey.authIndex, now)
		tracker.mergeOverflowLocked(overflow, tracker.loans[oldestKey], now)
		delete(tracker.loans, oldestKey)
	}
	return found
}

func (tracker *projectDemandTracker) overflowSampleLocked(
	values map[string]*projectDemandSample,
	blocked map[string]time.Time,
	saturated *bool,
	authIndex string,
	now time.Time,
) *projectDemandSample {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil
	}
	if sample := values[authIndex]; sample != nil {
		delete(blocked, authIndex)
		return sample
	}
	if len(values) >= projectDemandMaximumOverflow {
		if markDemandBlockedLocked(blocked, authIndex, now) && saturated != nil {
			*saturated = true
		}
		return nil
	}
	sample := &projectDemandSample{}
	values[authIndex] = sample
	delete(blocked, authIndex)
	return sample
}

func markDemandBlockedLocked(values map[string]time.Time, authIndex string, now time.Time) bool {
	if _, exists := values[authIndex]; exists || len(values) < projectDemandMaximumOverflow {
		values[authIndex] = now
		return false
	}
	// The bounded exact-marker set is exhausted. Evicting an older marker
	// would silently reopen that credential, so enter a sticky global
	// fail-closed state for secondary borrowing. Primary traffic never reads
	// this guard and remains available.
	return true
}

func (tracker *projectDemandTracker) mergeOverflowLocked(target, source *projectDemandSample, now time.Time) {
	if source == nil {
		return
	}
	if target == nil {
		return
	}
	tracker.decayLocked(target, now)
	activity, _ := tracker.sampleAt(source, now)
	target.activity += activity
	if source.lastActivity.After(target.lastActivity) {
		target.lastActivity = source.lastActivity
	}
	for _, bucket := range source.buckets {
		if bucket.minute == 0 || bucket.units <= 0 {
			continue
		}
		index := int(bucket.minute % int64(len(target.buckets)))
		if index < 0 {
			index = -index
		}
		if target.buckets[index].minute != bucket.minute {
			target.buckets[index] = bucket
		} else {
			target.buckets[index].units += bucket.units
			target.buckets[index].requests += bucket.requests
		}
	}
}

func (tracker *projectDemandTracker) tempo(authIndex string, now time.Time) projectDemandTempo {
	if tracker == nil {
		return projectDemandTempo{}
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return projectDemandTempo{}
	}
	now = normalizedDemandTime(now)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.pruneIfDueLocked(now)
	var tempo projectDemandTempo
	add := func(sample *projectDemandSample) {
		tempo.OneMinuteRequestsPerMinute += projectDemandWindowRequestRate(sample, now, 1)
		tempo.TenMinuteRequestsPerMinute += projectDemandWindowRequestRate(sample, now, 10)
		tempo.SixtyMinuteRequestsPerMinute += projectDemandWindowRequestRate(sample, now, 60)
	}
	for key, sample := range tracker.projects {
		if key.authIndex == authIndex {
			add(sample)
		}
	}
	for key, sample := range tracker.loans {
		if key.authIndex == authIndex {
			add(sample)
		}
	}
	add(tracker.projectOverflow[authIndex])
	add(tracker.loanOverflow[authIndex])
	return tempo
}

func (tracker *projectDemandTracker) releaseLocked(sample *projectDemandSample, now time.Time) {
	if sample == nil {
		return
	}
	tracker.decayLocked(sample, now)
	if sample.inFlight > 0 {
		sample.inFlight--
	}
}

func (tracker *projectDemandTracker) decayLocked(sample *projectDemandSample, now time.Time) {
	if sample == nil {
		return
	}
	if sample.at.IsZero() {
		sample.at = now
		return
	}
	if !now.After(sample.at) {
		return
	}
	elapsed := now.Sub(sample.at)
	sample.activity *= math.Exp(-math.Ln2 * float64(elapsed) / float64(tracker.halfLife))
	if sample.activity < 0.000001 {
		sample.activity = 0
	}
	sample.at = now
}

func (tracker *projectDemandTracker) sampleAt(sample *projectDemandSample, now time.Time) (float64, int) {
	if sample == nil {
		return 0, 0
	}
	activity := sample.activity
	if !sample.at.IsZero() && now.After(sample.at) {
		activity *= math.Exp(-math.Ln2 * float64(now.Sub(sample.at)) / float64(tracker.halfLife))
	}
	return activity, sample.inFlight
}

// view takes one consistent snapshot for an allocator sort. It reads only
// local counters and configuration; it performs no provider calls, sleeps, or
// quota refreshes. Primary attempts are always assigned zero demand penalty.
func (tracker *projectDemandTracker) view(
	cfg pluginConfig,
	requesterID string,
	attempts []executionAttempt,
	now time.Time,
) projectDemandView {
	view := projectDemandView{penalties: make(map[string]float64, len(attempts))}
	if tracker == nil || len(attempts) == 0 {
		return view
	}
	requesterID = strings.TrimSpace(requesterID)
	now = normalizedDemandTime(now)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.pruneIfDueLocked(now)
	for _, attempt := range attempts {
		authIndex := strings.TrimSpace(stableAuthIndex(attempt.Auth))
		if authIndex == "" || attempt.Primary {
			continue
		}

		penalty := 0.0
		owners := projectDemandPrimaryOwners(cfg, attempt.Auth)
		if len(owners) > 0 {
			penalty += ownerStandbyPenalty
		}
		for _, ownerID := range owners {
			if ownerID == requesterID {
				continue
			}
			activity, inFlight := tracker.sampleAt(tracker.projects[projectDemandLoanKey{
				projectID: ownerID, authIndex: authIndex,
			}], now)
			penalty += activity*ownerActivityWeight + float64(inFlight)*ownerInFlightWeight
		}
		overflowActivity, overflowInFlight := tracker.sampleAt(tracker.projectOverflow[authIndex], now)
		penalty += overflowActivity*ownerActivityWeight + float64(overflowInFlight)*ownerInFlightWeight
		borrowed, borrowedInFlight := tracker.sampleAt(tracker.loans[projectDemandLoanKey{
			projectID: requesterID,
			authIndex: authIndex,
		}], now)
		penalty += borrowed*borrowActivityWeight + float64(borrowedInFlight)*borrowInFlightWeight
		overflowBorrowed, overflowBorrowedInFlight := tracker.sampleAt(tracker.loanOverflow[authIndex], now)
		penalty += overflowBorrowed*borrowActivityWeight + float64(overflowBorrowedInFlight)*borrowInFlightWeight
		if tracker.projectSaturated {
			penalty += projectDemandMaximumGuard * ownerActivityWeight
		} else if _, blocked := tracker.projectBlocked[authIndex]; blocked {
			penalty += projectDemandMaximumGuard * ownerActivityWeight
		}
		if tracker.loanSaturated {
			penalty += projectDemandMaximumGuard * borrowActivityWeight
		} else if _, blocked := tracker.loanBlocked[authIndex]; blocked {
			penalty += projectDemandMaximumGuard * borrowActivityWeight
		}
		view.penalties[authIndex] = penalty
	}
	return view
}

func (view projectDemandView) penalty(attempt executionAttempt) float64 {
	if attempt.Primary {
		return 0
	}
	return view.penalties[strings.TrimSpace(stableAuthIndex(attempt.Auth))]
}

func (tracker *projectDemandTracker) inFlight(projectID string) int {
	if tracker == nil {
		return 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	projectID = strings.TrimSpace(projectID)
	inFlight := 0
	for key, sample := range tracker.projects {
		if key.projectID == projectID {
			_, current := tracker.sampleAt(sample, time.Now().UTC())
			inFlight += current
		}
	}
	return inFlight
}

func (tracker *projectDemandTracker) guard(
	cfg pluginConfig,
	requesterID string,
	auth pluginapi.HostAuthFileEntry,
	now time.Time,
) float64 {
	if tracker == nil {
		return 0
	}
	requesterID = strings.TrimSpace(requesterID)
	now = normalizedDemandTime(now)
	horizon := time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	if horizon <= 0 {
		horizon = time.Duration(defaultQuotaUsageRefreshSeconds) * time.Second
	}
	if horizon > 15*time.Minute {
		horizon = 15 * time.Minute
	}
	if horizon < time.Minute {
		horizon = time.Minute
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	authIndex := strings.TrimSpace(stableAuthIndex(auth))
	if tracker.projectSaturated {
		return projectDemandMaximumGuard
	}
	if _, blocked := tracker.projectBlocked[authIndex]; blocked {
		return projectDemandMaximumGuard
	}
	guard := 0.0
	for _, ownerID := range projectDemandPrimaryOwners(cfg, auth) {
		if ownerID == requesterID {
			continue
		}
		sample := tracker.projects[projectDemandLoanKey{projectID: ownerID, authIndex: authIndex}]
		if sample == nil {
			continue
		}
		rate := math.Max(
			projectDemandWindowRate(sample, now, 1),
			math.Max(projectDemandWindowRate(sample, now, 10), projectDemandWindowRate(sample, now, 60)),
		)
		guard += rate * horizon.Minutes()
	}
	if overflow := tracker.projectOverflow[authIndex]; overflow != nil {
		rate := math.Max(
			projectDemandWindowRate(overflow, now, 1),
			math.Max(projectDemandWindowRate(overflow, now, 10), projectDemandWindowRate(overflow, now, 60)),
		)
		guard += rate * horizon.Minutes()
	}
	return math.Min(math.Max(guard, 0), projectDemandMaximumGuard)
}

func projectDemandUnits(attempt executionAttempt) float64 {
	if attempt.ReservationPercent > 0 && !math.IsNaN(attempt.ReservationPercent) && !math.IsInf(attempt.ReservationPercent, 0) {
		return attempt.ReservationPercent
	}
	return defaultDemandUnits
}

func projectDemandPrimaryOwners(cfg pluginConfig, auth pluginapi.HostAuthFileEntry) []string {
	owners := make([]string, 0, 1)
	for _, project := range cfg.SmartKeys {
		projectID := strings.TrimSpace(project.ID)
		if projectID == "" || !smartKeyActive(project) || !projectReferencesAuth(project.PrimaryAuthIDs, auth) {
			continue
		}
		owners = append(owners, projectID)
	}
	return owners
}

func projectReferencesAuth(references []string, auth pluginapi.HostAuthFileEntry) bool {
	values := []string{
		strings.TrimSpace(auth.AuthIndex),
		strings.TrimSpace(auth.ID),
		strings.TrimSpace(auth.Name),
	}
	for _, raw := range references {
		reference := strings.TrimSpace(raw)
		if reference == "" {
			continue
		}
		for _, value := range values {
			if value != "" && reference == value {
				return true
			}
		}
	}
	return false
}

func normalizedDemandTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

// captureProjectDemandView and beginProjectDemandLease are intentionally tiny
// integration surfaces. The allocator captures the view after admission for
// sorting, while projectDemandGuard participates in both preflight and atomic
// lease admission. Only successfully acquired leases enter the lifecycle hook.
func captureProjectDemandView(cfg pluginConfig, projectID string, attempts []executionAttempt, now time.Time) projectDemandView {
	return bravoProjectDemand.view(cfg, projectID, attempts, now)
}

func beginProjectDemandLease(attempt executionAttempt, now time.Time) func(bool, time.Time) {
	return bravoProjectDemand.begin(attempt, now)
}

func projectDemandGuard(cfg pluginConfig, attempt executionAttempt, now time.Time) float64 {
	if attempt.Primary {
		return 0
	}
	return bravoProjectDemand.guard(cfg, attempt.ProjectID, attempt.Auth, now)
}
