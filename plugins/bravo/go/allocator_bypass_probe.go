package main

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	allocatorBypassProbeMaximumEntries  = 4096
	allocatorBypassProbeMaximumLeaseAge = 2 * time.Hour
)

// allocatorBypassDecision distinguishes ordinary availability from the narrow
// stale-after-scheduled-reset escape hatch. Only the latter needs a probe.
type allocatorBypassDecision struct {
	Eligible        bool
	Confirmed       bool
	ProbeGeneration string
}

type allocatorBypassProbeAttemptState struct {
	sync.Mutex
	plannedGeneration string
	activeKey         string
	leaseID           uint64
	dispatched        bool
	settled           bool
}

type allocatorBypassProbeEntry struct {
	LeaseID   uint64
	InFlight  bool
	Consumed  bool
	UpdatedAt time.Time
}

var allocatorBypassProbeRuntime = struct {
	sync.Mutex
	Entries     map[string]allocatorBypassProbeEntry
	NextLeaseID uint64
	Saturated   bool
	Dropped     uint64
}{Entries: make(map[string]allocatorBypassProbeEntry)}

func newAllocatorBypassProbeAttemptState(decision allocatorBypassDecision) *allocatorBypassProbeAttemptState {
	return &allocatorBypassProbeAttemptState{plannedGeneration: decision.ProbeGeneration}
}

func allocatorBypassAuthDecision(
	cfg pluginConfig,
	item candidate,
	auth pluginapi.HostAuthFileEntry,
	now time.Time,
) allocatorBypassDecision {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	subscription := subscriptionPolicy(cfg, authIndex)
	// Explicit disable and owner reserve floors remain stronger than the
	// availability fallback.
	if authIndex == "" || !subscriptionEnabled(subscription) {
		return allocatorBypassDecision{}
	}
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	if quotaConfidence(quota) != "confirmed" {
		// Preserve the historical unknown-snapshot fail-open contract. Unknown
		// data never enters the reset probe gate.
		return allocatorBypassDecision{Eligible: true}
	}
	tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
	session, weekly := effectiveQuotaWindows(quota, item.Model)
	weeklyLabel := allocatorBypassWeeklyWindowLabel(quota, item.Model)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[authIndex] + allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	observedAt := quotaConfirmedAt(quota)
	sessionDecision := allocatorBypassWindowDecision(
		"session", session, tariff.SessionFloorPercent, tariff.ReservationPercent, reserved, observedAt, now,
	)
	weeklyDecision := allocatorBypassWindowDecision(
		weeklyLabel, weekly, tariff.WeeklyFloorPercent, tariff.ReservationPercent, reserved, observedAt, now,
	)
	if !sessionDecision.Eligible || !weeklyDecision.Eligible {
		return allocatorBypassDecision{Confirmed: true}
	}
	generations := make([]string, 0, 2)
	if sessionDecision.ProbeGeneration != "" {
		generations = append(generations, sessionDecision.ProbeGeneration)
	}
	if weeklyDecision.ProbeGeneration != "" {
		generations = append(generations, weeklyDecision.ProbeGeneration)
	}
	return allocatorBypassDecision{Eligible: true, Confirmed: true, ProbeGeneration: strings.Join(generations, "|")}
}

func allocatorBypassWindowDecision(
	label string,
	window quotaWindowState,
	floor, reservation, reserved float64,
	observedAt, now time.Time,
) allocatorBypassDecision {
	window = normalizeQuotaWindow(window)
	if window.RemainingPercent-reserved-reservation > floor {
		return allocatorBypassDecision{Eligible: true}
	}
	if window.ResetMode != pluginapi.HostAuthQuotaResetModeScheduled ||
		window.ResetAt.IsZero() || now.Before(window.ResetAt) {
		return allocatorBypassDecision{}
	}
	// A quota observation made after the scheduled reset is fresh evidence, so
	// its remaining=0/floor verdict must not be bypassed. Only a snapshot from
	// the previous generation earns one availability probe.
	if !observedAt.IsZero() && !observedAt.Before(window.ResetAt) {
		return allocatorBypassDecision{}
	}
	return allocatorBypassDecision{
		Eligible:        true,
		ProbeGeneration: strings.TrimSpace(label) + "@" + window.ResetAt.UTC().Format(time.RFC3339Nano),
	}
}

func allocatorBypassWeeklyWindowLabel(quota credentialQuotaState, model string) string {
	weekly := normalizeQuotaWindow(quota.Weekly)
	label := "weekly"
	for _, candidate := range quota.ModelWeekly {
		if !quotaModelMatches(model, candidate.Model) {
			continue
		}
		window := normalizeQuotaWindow(candidate.quotaWindowState)
		if window.RemainingPercent < weekly.RemainingPercent {
			weekly = window
			label = "model_weekly:" + strings.ToLower(strings.TrimSpace(candidate.Model))
		}
	}
	return label
}

// acquireAllocatorBypassProbeLease never waits. A busy or consumed generation
// simply makes this attempt unavailable so the existing executor continues the
// neighboring auth/model route without spending provider-call budget.
func acquireAllocatorBypassProbeLease(
	attempt executionAttempt,
	now time.Time,
) (func(bool), bool) {
	if !attempt.AllocatorBypass {
		return func(bool) {}, true
	}
	now = now.UTC()
	decision := allocatorBypassAuthDecision(loadedConfig(), attempt.Candidate, attempt.Auth, now)
	reconcileAllocatorBypassProbeGenerations(attempt.Auth.AuthIndex, decision, now)
	if !decision.Eligible {
		return func(bool) {}, false
	}
	state := attempt.AllocatorBypassProbe
	if decision.ProbeGeneration == "" {
		if state != nil {
			clearAllocatorBypassPlannedGeneration(attempt.Auth.AuthIndex, state)
		}
		return func(bool) {}, true
	}
	// Production plans always carry shared state. Fail closed for a manually
	// constructed reset-bypass attempt rather than creating an unmarkable lease
	// that release(false) could mistake for a pre-dispatch failure.
	if state == nil {
		return func(bool) {}, false
	}

	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	key := allocatorBypassProbeKey(authIndex, decision.ProbeGeneration)
	state.Lock()
	if state.settled || state.leaseID != 0 {
		state.Unlock()
		return func(bool) {}, false
	}
	allocatorBypassProbeRuntime.Lock()
	pruneAllocatorBypassProbeRuntimeLocked(now)
	if _, exists := allocatorBypassProbeRuntime.Entries[key]; exists {
		allocatorBypassProbeRuntime.Unlock()
		state.Unlock()
		return func(bool) {}, false
	}
	if len(allocatorBypassProbeRuntime.Entries) >= allocatorBypassProbeMaximumEntries {
		allocatorBypassProbeRuntime.Saturated = true
		allocatorBypassProbeRuntime.Dropped++
		allocatorBypassProbeRuntime.Unlock()
		state.Unlock()
		return func(bool) {}, false
	}
	allocatorBypassProbeRuntime.NextLeaseID++
	leaseID := allocatorBypassProbeRuntime.NextLeaseID
	if leaseID == 0 {
		allocatorBypassProbeRuntime.NextLeaseID++
		leaseID = allocatorBypassProbeRuntime.NextLeaseID
	}
	allocatorBypassProbeRuntime.Entries[key] = allocatorBypassProbeEntry{
		LeaseID: leaseID, InFlight: true, UpdatedAt: now,
	}
	allocatorBypassProbeRuntime.Unlock()
	state.plannedGeneration = decision.ProbeGeneration
	state.activeKey = key
	state.leaseID = leaseID
	state.Unlock()

	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			settleAllocatorBypassProbe(state, commit, time.Now().UTC())
		})
	}, true
}

// markAllocatorBypassProbeDispatched is called immediately before the host
// provider method is launched. From this point even release(false) represents
// a real dispatch attempt, so the stale generation remains consumed.
func markAllocatorBypassProbeDispatched(attempt executionAttempt, now time.Time) {
	state := attempt.AllocatorBypassProbe
	if state == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	if state.settled || state.leaseID == 0 {
		return
	}
	state.dispatched = true
	allocatorBypassProbeRuntime.Lock()
	if entry, ok := allocatorBypassProbeRuntime.Entries[state.activeKey]; ok && entry.LeaseID == state.leaseID {
		entry.InFlight = false
		entry.Consumed = true
		entry.UpdatedAt = now.UTC()
		allocatorBypassProbeRuntime.Entries[state.activeKey] = entry
	}
	allocatorBypassProbeRuntime.Unlock()
}

func settleAllocatorBypassProbe(state *allocatorBypassProbeAttemptState, commit bool, now time.Time) {
	if state == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	if state.settled || state.leaseID == 0 {
		return
	}
	allocatorBypassProbeRuntime.Lock()
	if entry, ok := allocatorBypassProbeRuntime.Entries[state.activeKey]; ok && entry.LeaseID == state.leaseID {
		if commit || state.dispatched {
			entry.InFlight = false
			entry.Consumed = true
			entry.UpdatedAt = now.UTC()
			allocatorBypassProbeRuntime.Entries[state.activeKey] = entry
		} else {
			delete(allocatorBypassProbeRuntime.Entries, state.activeKey)
		}
	}
	allocatorBypassProbeRuntime.Unlock()
	state.settled = true
}

func clearAllocatorBypassPlannedGeneration(authIndex string, state *allocatorBypassProbeAttemptState) {
	if state == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	if state.plannedGeneration == "" || state.leaseID != 0 {
		return
	}
	key := allocatorBypassProbeKey(strings.TrimSpace(authIndex), state.plannedGeneration)
	allocatorBypassProbeRuntime.Lock()
	if entry, ok := allocatorBypassProbeRuntime.Entries[key]; ok && !entry.InFlight {
		delete(allocatorBypassProbeRuntime.Entries, key)
	}
	allocatorBypassProbeRuntime.Unlock()
	state.plannedGeneration = ""
}

func allocatorBypassProbeKey(authIndex, generation string) string {
	return strings.TrimSpace(authIndex) + "\x00" + strings.TrimSpace(generation)
}

func pruneAllocatorBypassProbeRuntimeLocked(now time.Time) {
	for key, entry := range allocatorBypassProbeRuntime.Entries {
		age := now.Sub(entry.UpdatedAt)
		if age < 0 {
			age = 0
		}
		if entry.InFlight && age >= allocatorBypassProbeMaximumLeaseAge {
			delete(allocatorBypassProbeRuntime.Entries, key)
		}
	}
	if len(allocatorBypassProbeRuntime.Entries) < allocatorBypassProbeMaximumEntries {
		allocatorBypassProbeRuntime.Saturated = false
	}
}

func reconcileAllocatorBypassProbeGenerations(
	authIndex string,
	decision allocatorBypassDecision,
	now time.Time,
) {
	if !decision.Confirmed {
		return
	}
	authPrefix := strings.TrimSpace(authIndex) + "\x00"
	keep := ""
	if decision.ProbeGeneration != "" {
		keep = allocatorBypassProbeKey(authIndex, decision.ProbeGeneration)
	}
	allocatorBypassProbeRuntime.Lock()
	defer allocatorBypassProbeRuntime.Unlock()
	pruneAllocatorBypassProbeRuntimeLocked(now.UTC())
	for key, entry := range allocatorBypassProbeRuntime.Entries {
		if !strings.HasPrefix(key, authPrefix) || key == keep || entry.InFlight {
			continue
		}
		// A confirmed fresh snapshot or a different scheduled-reset generation
		// supersedes old consumed tombstones for this auth.
		delete(allocatorBypassProbeRuntime.Entries, key)
	}
	if len(allocatorBypassProbeRuntime.Entries) < allocatorBypassProbeMaximumEntries {
		allocatorBypassProbeRuntime.Saturated = false
	}
}

func resetAllocatorBypassProbeForTest() {
	allocatorBypassProbeRuntime.Lock()
	allocatorBypassProbeRuntime.Entries = make(map[string]allocatorBypassProbeEntry)
	allocatorBypassProbeRuntime.NextLeaseID = 0
	allocatorBypassProbeRuntime.Saturated = false
	allocatorBypassProbeRuntime.Dropped = 0
	allocatorBypassProbeRuntime.Unlock()
}
