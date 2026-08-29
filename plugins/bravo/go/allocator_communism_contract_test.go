package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSubscriptionCommunismPrimaryPrecedesSharedCapacity(t *testing.T) {
	const (
		primaryIndex   = "communism-primary-first"
		secondaryIndex = "communism-secondary-after"
	)
	cfg := installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		primaryIndex:   confirmedCommunismQuota(90),
		secondaryIndex: confirmedCommunismQuota(90),
	})
	project := smartKeyConfig{
		ID:             "communism-primary-project",
		PrimaryAuthIDs: []string{primaryIndex},
		AllowedAuthIDs: []string{primaryIndex, secondaryIndex},
	}
	// Deliberately put the shared credential first. Primary ownership, rather
	// than host-list order or lower allocator stress, must win the first try.
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "shared", AuthIndex: secondaryIndex, Provider: "claude"},
		{ID: "owned", AuthIndex: primaryIndex, Provider: "claude"},
	}
	attempts := allocateCandidateAuths(
		rpcExecutorRequest{},
		cfg,
		project,
		candidate{Provider: "claude", Model: "claude-sonnet-5"},
		auths,
		"communism-primary-sticky",
	)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want primary plus shared fallback", len(attempts))
	}
	if attempts[0].Auth.AuthIndex != primaryIndex || !attempts[0].Primary {
		t.Fatalf("first attempt = %#v, want the project's primary", attempts[0])
	}
	if attempts[1].Auth.AuthIndex != secondaryIndex || attempts[1].Primary {
		t.Fatalf("second attempt = %#v, want shared secondary capacity", attempts[1])
	}
}

func TestSubscriptionCommunismExhaustedPrimaryFallsThroughToSharedCapacity(t *testing.T) {
	const (
		primaryIndex   = "communism-primary-empty"
		secondaryIndex = "communism-secondary-ready"
	)
	cfg := installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		primaryIndex:   confirmedCommunismQuota(0),
		secondaryIndex: confirmedCommunismQuota(90),
	})
	project := smartKeyConfig{
		ID:             "communism-fallback-project",
		PrimaryAuthIDs: []string{primaryIndex},
		AllowedAuthIDs: []string{primaryIndex, secondaryIndex},
	}
	attempts := allocateCandidateAuths(
		rpcExecutorRequest{},
		cfg,
		project,
		candidate{Provider: "claude", Model: "claude-sonnet-5"},
		[]pluginapi.HostAuthFileEntry{
			{ID: "owned-empty", AuthIndex: primaryIndex, Provider: "claude"},
			{ID: "shared-ready", AuthIndex: secondaryIndex, Provider: "claude"},
		},
		"communism-fallback-sticky",
	)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want exhausted primary followed by shared capacity", len(attempts))
	}
	if release, acquired := acquireAttemptLease(attempts[0]); acquired {
		release(false)
		t.Fatal("exhausted primary acquired a provider lease")
	}
	release, acquired := acquireAttemptLease(attempts[1])
	if !acquired {
		t.Fatal("shared subscription was not available after the primary was exhausted")
	}
	release(false)
}

func TestSubscriptionCommunismUnavailablePrimaryUsesSharedCapacity(t *testing.T) {
	const (
		primaryIndex   = "communism-primary-unavailable"
		secondaryIndex = "communism-secondary-healthy"
	)
	cfg := installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		secondaryIndex: confirmedCommunismQuota(90),
	})
	project := smartKeyConfig{
		ID:             "communism-unavailable-project",
		PrimaryAuthIDs: []string{primaryIndex},
		AllowedAuthIDs: []string{primaryIndex, secondaryIndex},
	}
	model := candidate{Provider: "claude", Model: "claude-sonnet-5"}
	now := time.Now().UTC()
	eligible := eligibleAuths(model, []pluginapi.HostAuthFileEntry{
		{
			ID:        "owned-unavailable",
			AuthIndex: primaryIndex,
			Provider:  "claude",
			ModelStates: map[string]pluginapi.HostAuthModelState{
				model.Model: {
					Status:         "error",
					Unavailable:    true,
					NextRetryAfter: now.Add(time.Hour),
				},
			},
		},
		{ID: "shared-healthy", AuthIndex: secondaryIndex, Provider: "claude"},
	}, now)
	attempts := allocateCandidateAuths(
		rpcExecutorRequest{}, cfg, project, model, eligible, "communism-unavailable-sticky",
	)
	if len(attempts) != 1 || attempts[0].Auth.AuthIndex != secondaryIndex || attempts[0].Primary {
		t.Fatalf("attempts = %#v, want only the healthy shared subscription", attempts)
	}
}

func TestSubscriptionCommunismSecondaryRespectsFloorWhileOwnerMaySpendIt(t *testing.T) {
	const authIndex = "communism-protected-floor"
	cfg := installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		authIndex: confirmedCommunismQuota(50.5),
	})
	tariff := tariffByID(cfg, "x1")
	if tariff.SessionFloorPercent != 50 || tariff.WeeklyFloorPercent != 50 || tariff.ReservationPercent != 0.5 {
		t.Fatalf("unexpected x1 tariff for boundary test: %#v", tariff)
	}
	quota := quotaSnapshot(authIndex)
	if secondaryQuotaEligibleAt(
		cfg, quota, "claude-sonnet-5", tariff, authIndex, tariff.ReservationPercent, time.Now().UTC(),
	) {
		t.Fatal("secondary was allowed to consume the final reservation at the protected floor")
	}

	secondary := executionAttempt{
		Candidate:          candidate{Provider: "claude", Model: "claude-sonnet-5"},
		Auth:               pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		ProjectID:          "secondary-project",
		AllocatorManaged:   true,
		ReservationPercent: tariff.ReservationPercent,
		TariffID:           tariff.ID,
	}
	if release, acquired := acquireAttemptLease(secondary); acquired {
		release(false)
		t.Fatal("secondary acquired a lease that reaches the protected floor")
	}

	owner := secondary
	owner.ProjectID = "owner-project"
	owner.Primary = true
	release, acquired := acquireAttemptLease(owner)
	if !acquired {
		t.Fatal("owner could not consume its own protected reserve")
	}
	release(false)
}

func TestSubscriptionCommunismBorrowsFreeSharedCapacity(t *testing.T) {
	const authIndex = "communism-free-shared"
	cfg := installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		authIndex: confirmedCommunismQuota(90),
	})
	tariff := tariffByID(cfg, "x1")
	secondary := executionAttempt{
		Candidate:          candidate{Provider: "claude", Model: "claude-sonnet-5"},
		Auth:               pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		ProjectID:          "borrowing-project",
		AllocatorManaged:   true,
		ReservationPercent: tariff.ReservationPercent,
		TariffID:           tariff.ID,
	}
	release, acquired := acquireAttemptLease(secondary)
	if !acquired {
		t.Fatal("secondary could not borrow capacity above the owner's protected floor")
	}
	release(false)
}

func TestSubscriptionCommunismHasNoFixedProjectConcurrencyCap(t *testing.T) {
	const authIndex = "communism-no-project-cap"
	installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		authIndex: confirmedCommunismQuota(100),
	})
	const agents = 30
	releases := make([]func(bool), 0, agents)
	for index := 0; index < agents; index++ {
		attempt := executionAttempt{
			Candidate:          candidate{Provider: "claude", Model: "claude-sonnet-5"},
			Auth:               pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
			ProjectID:          "same-project-with-30-agents",
			Primary:            true,
			AllocatorManaged:   true,
			ReservationPercent: 0.01,
			TariffID:           "x1",
		}
		release, acquired := acquireAttemptLease(attempt)
		if !acquired {
			for _, heldRelease := range releases {
				heldRelease(false)
			}
			t.Fatalf("agent %d was rejected despite ample quota; a fixed project concurrency cap may exist", index+1)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release(false)
	}
}

// This is intentionally a RED contract test against the current implementation.
// allocatorBypassPlan currently rebuilds an unmanaged attempt after the normal
// allocator withheld a secondary at its configured reserve floor. That behavior
// keeps a request alive, but it spends capacity explicitly protected for the
// subscription owner and therefore violates the subscription-communism contract.
func TestSubscriptionCommunismReserveFloorIsNeverBypassed(t *testing.T) {
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	secondary := pluginapi.HostAuthFileEntry{
		ID: "protected-owner", AuthIndex: "communism-bypass-protected", Provider: "claude",
	}
	plan := allocatorBypassPlan(
		"sonnet",
		model,
		requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)},
		[]pluginapi.HostAuthFileEntry{secondary},
		[]candidateRejection{{
			Provider: "claude",
			Model:    "claude-sonnet-5",
			Stage:    "allocator",
			Code:     "bravo_allocator_reserve_floor",
			Reason:   "secondary is at the owner's protected floor",
		}},
		"communism-floor-bypass",
		time.Now().UTC(),
	)
	if len(plan) != 0 {
		t.Fatalf("reserve-floor bypass produced %d unmanaged attempt(s); protected owner capacity would be spent", len(plan))
	}
}

func TestSubscriptionCommunismConfirmedExhaustionIsNeverBypassed(t *testing.T) {
	const authIndex = "communism-bypass-exhausted"
	installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		authIndex: confirmedCommunismQuota(0),
	})
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	plan := allocatorBypassPlan(
		"sonnet",
		model,
		requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)},
		[]pluginapi.HostAuthFileEntry{{ID: "empty", AuthIndex: authIndex, Provider: "claude"}},
		[]candidateRejection{{
			Provider: "claude", Model: "claude-sonnet-5", Stage: "allocator",
			Code: "bravo_allocator_withheld", Reason: "confirmed quota exhausted",
		}},
		"communism-exhausted-bypass",
		time.Now().UTC(),
	)
	if len(plan) != 0 {
		t.Fatalf("confirmed exhaustion produced %d unmanaged attempt(s)", len(plan))
	}
}

func TestSubscriptionCommunismDisabledSubscriptionIsNeverBypassed(t *testing.T) {
	const authIndex = "communism-bypass-disabled"
	cfg := installSubscriptionCommunismTestState(t, nil)
	cfg.Subscriptions = []subscriptionConfig{{
		AuthIndex: authIndex, Tariff: "x1", Enabled: boolPointer(false),
	}}
	currentConfig.Store(cfg)
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	plan := allocatorBypassPlan(
		"sonnet",
		model,
		requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)},
		[]pluginapi.HostAuthFileEntry{{ID: "disabled", AuthIndex: authIndex, Provider: "claude"}},
		[]candidateRejection{{
			Provider: "claude", Model: "claude-sonnet-5", Stage: "allocator",
			Code: "bravo_allocator_withheld", Reason: "subscription disabled",
		}},
		"communism-disabled-bypass",
		time.Now().UTC(),
	)
	if len(plan) != 0 {
		t.Fatalf("disabled subscription produced %d unmanaged attempt(s)", len(plan))
	}
}

func TestSubscriptionCommunismMixedPoolCannotHideProtectedFloor(t *testing.T) {
	const (
		floorIndex     = "communism-mixed-floor"
		exhaustedIndex = "communism-mixed-exhausted"
	)
	installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{
		floorIndex:     confirmedCommunismQuota(50.5),
		exhaustedIndex: confirmedCommunismQuota(0),
	})
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	plan := allocatorBypassPlan(
		"sonnet",
		model,
		requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)},
		[]pluginapi.HostAuthFileEntry{
			{ID: "at-floor", AuthIndex: floorIndex, Provider: "claude"},
			{ID: "empty", AuthIndex: exhaustedIndex, Provider: "claude"},
		},
		[]candidateRejection{{
			Provider: "claude", Model: "claude-sonnet-5", Stage: "allocator",
			Code: "bravo_allocator_withheld", Reason: "mixed allocator rejection",
		}},
		"communism-mixed-bypass",
		time.Now().UTC(),
	)
	if len(plan) != 0 {
		t.Fatalf("mixed rejection produced %d unmanaged attempt(s); a protected floor was hidden by another auth", len(plan))
	}
}

func TestSubscriptionCommunismExpiredKnownZeroWaitsForScheduledReset(t *testing.T) {
	const authIndex = "communism-expired-known-zero"
	quota := confirmedCommunismQuota(0)
	quota.ConfirmedAt = time.Now().UTC().Add(-24 * time.Hour)
	quota.RefreshedAt = quota.ConfirmedAt
	quota.Session.ResetAt = time.Now().UTC().Add(time.Hour)
	quota.Weekly.ResetAt = time.Now().UTC().Add(24 * time.Hour)
	installSubscriptionCommunismTestState(t, map[string]credentialQuotaState{authIndex: quota})
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	plan := allocatorBypassPlan(
		"sonnet",
		model,
		requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)},
		[]pluginapi.HostAuthFileEntry{{ID: "expired-empty", AuthIndex: authIndex, Provider: "claude"}},
		[]candidateRejection{{
			Provider: "claude", Model: "claude-sonnet-5", Stage: "allocator",
			Code: "bravo_allocator_withheld", Reason: "expired known exhaustion",
		}},
		"communism-expired-zero-bypass",
		time.Now().UTC(),
	)
	if len(plan) != 0 {
		t.Fatalf("expired known-zero quota produced %d attempts before its scheduled reset", len(plan))
	}
}

func installSubscriptionCommunismTestState(t *testing.T, quotas map[string]credentialQuotaState) pluginConfig {
	t.Helper()
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.UnknownSecondaryPolicy = "block"
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	allocatorRuntime.Lock()
	previousInFlight := allocatorRuntime.InFlightPercent
	previousPending := allocatorRuntime.PendingPercent
	allocatorRuntime.InFlightPercent = make(map[string]float64)
	allocatorRuntime.PendingPercent = make(map[string]float64)
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		allocatorRuntime.InFlightPercent = previousInFlight
		allocatorRuntime.PendingPercent = previousPending
		allocatorRuntime.Unlock()
	})

	bravoUsageState.mu.Lock()
	if bravoUsageState.state.Quotas == nil {
		bravoUsageState.state.Quotas = make(map[string]*credentialQuotaState)
	}
	type previousQuota struct {
		value  *credentialQuotaState
		exists bool
	}
	previous := make(map[string]previousQuota, len(quotas))
	for authIndex, quota := range quotas {
		value, exists := bravoUsageState.state.Quotas[authIndex]
		previous[authIndex] = previousQuota{value: value, exists: exists}
		copyQuota := quota
		bravoUsageState.state.Quotas[authIndex] = &copyQuota
	}
	bravoUsageState.mu.Unlock()
	t.Cleanup(func() {
		bravoUsageState.mu.Lock()
		for authIndex, old := range previous {
			if old.exists {
				bravoUsageState.state.Quotas[authIndex] = old.value
			} else {
				delete(bravoUsageState.state.Quotas, authIndex)
			}
		}
		bravoUsageState.mu.Unlock()
	})
	return cfg
}

func confirmedCommunismQuota(remaining float64) credentialQuotaState {
	now := time.Now().UTC()
	return credentialQuotaState{
		Confidence:  "confirmed",
		ConfirmedAt: now,
		RefreshedAt: now,
		Session: quotaWindowState{
			UsedPercent:      100 - remaining,
			RemainingPercent: remaining,
			ResetAt:          now.Add(time.Hour),
			ResetMode:        pluginapi.HostAuthQuotaResetModeScheduled,
		},
		Weekly: quotaWindowState{
			UsedPercent:      100 - remaining,
			RemainingPercent: remaining,
			ResetAt:          now.Add(24 * time.Hour),
			ResetMode:        pluginapi.HostAuthQuotaResetModeScheduled,
		},
	}
}
