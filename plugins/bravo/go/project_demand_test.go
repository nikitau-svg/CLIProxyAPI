package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProjectDemandPrefersCredentialWhoseOwnerIsIdle(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{
		{ID: "prj_busy_owner", PrimaryAuthIDs: []string{"auth-busy"}},
		{ID: "prj_idle_owner", PrimaryAuthIDs: []string{"auth-idle"}},
	}}

	busyPrimary := demandTestAttempt("prj_busy_owner", "auth-busy", true)
	for index := 0; index < 8; index++ {
		release := tracker.begin(busyPrimary, now.Add(time.Duration(index)*time.Second))
		release(true, now.Add(time.Duration(index+1)*time.Second))
	}

	attempts := []executionAttempt{
		demandTestAttempt("prj_borrower", "auth-busy", false),
		demandTestAttempt("prj_borrower", "auth-idle", false),
	}
	view := tracker.view(cfg, "prj_borrower", attempts, now.Add(10*time.Second))
	if gotBusy, gotIdle := view.penalty(attempts[0]), view.penalty(attempts[1]); gotBusy <= gotIdle {
		t.Fatalf("busy owner penalty = %.3f, idle owner penalty = %.3f; want busy > idle", gotBusy, gotIdle)
	}
}

func TestProjectDemandSpreadsHotBorrowerAcrossFreeCredentials(t *testing.T) {
	tracker := newProjectDemandTracker(2 * time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{}

	used := demandTestAttempt("prj_hot", "auth-used", false)
	for index := 0; index < 12; index++ {
		release := tracker.begin(used, now.Add(time.Duration(index)*time.Second))
		release(true, now.Add(time.Duration(index+1)*time.Second))
	}
	unused := demandTestAttempt("prj_hot", "auth-unused", false)
	view := tracker.view(cfg, "prj_hot", []executionAttempt{used, unused}, now.Add(15*time.Second))
	if gotUsed, gotUnused := view.penalty(used), view.penalty(unused); gotUsed <= gotUnused {
		t.Fatalf("used credential penalty = %.3f, unused credential penalty = %.3f; want used > unused", gotUsed, gotUnused)
	}
}

func TestProjectDemandWeightsWorkByReservationInsteadOfRequestCount(t *testing.T) {
	tracker := newProjectDemandTracker(2 * time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	heavy := demandTestAttempt("prj_hot", "auth-heavy", false)
	heavy.ReservationPercent = 2.5
	light := demandTestAttempt("prj_hot", "auth-light", false)
	light.ReservationPercent = 0.1

	heavyRelease := tracker.begin(heavy, now)
	heavyRelease(true, now.Add(time.Second))
	lightRelease := tracker.begin(light, now)
	lightRelease(true, now.Add(time.Second))

	view := tracker.view(pluginConfig{}, "prj_hot", []executionAttempt{heavy, light}, now.Add(2*time.Second))
	if gotHeavy, gotLight := view.penalty(heavy), view.penalty(light); gotHeavy < gotLight*20 {
		t.Fatalf("heavy penalty = %.3f, light penalty = %.3f; reservation-weighted demand ratio is too small", gotHeavy, gotLight)
	}
}

func TestProjectDemandNeverPenalizesRequesterPrimary(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	attempt := demandTestAttempt("prj_owner", "auth-primary", true)

	for index := 0; index < 20; index++ {
		release := tracker.begin(attempt, now)
		release(true, now.Add(time.Second))
	}
	view := tracker.view(pluginConfig{}, "prj_owner", []executionAttempt{attempt}, now.Add(2*time.Second))
	if got := view.penalty(attempt); got != 0 {
		t.Fatalf("primary penalty = %.3f, want 0", got)
	}
}

func TestProjectDemandDecaysInsteadOfPermanentlyPunishingPastActivity(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{
		{ID: "prj_owner", PrimaryAuthIDs: []string{"auth-owned"}},
	}}
	ownerAttempt := demandTestAttempt("prj_owner", "auth-owned", true)
	for index := 0; index < 10; index++ {
		release := tracker.begin(ownerAttempt, now)
		release(true, now.Add(time.Second))
	}
	borrow := demandTestAttempt("prj_other", "auth-owned", false)
	immediate := tracker.view(cfg, "prj_other", []executionAttempt{borrow}, now.Add(time.Second)).penalty(borrow)
	decayed := tracker.view(cfg, "prj_other", []executionAttempt{borrow}, now.Add(12*time.Minute)).penalty(borrow)
	if decayed >= immediate/10 {
		t.Fatalf("decayed penalty = %.3f, immediate = %.3f; old activity did not decay", decayed, immediate)
	}
}

func TestProjectDemandResolvesPrimaryOwnerByAuthIDAndName(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	owner := smartKeyConfig{ID: "prj_owner", PrimaryAuthIDs: []string{"host-auth-id", "Friendly account"}}
	authByID := pluginapi.HostAuthFileEntry{ID: "host-auth-id", AuthIndex: "idx-id"}
	authByName := pluginapi.HostAuthFileEntry{Name: "Friendly account", AuthIndex: "idx-name"}

	for _, auth := range []pluginapi.HostAuthFileEntry{authByID, authByName} {
		primary := executionAttempt{ProjectID: owner.ID, Primary: true, Auth: auth}
		release := tracker.begin(primary, now)
		release(true, now.Add(time.Second))
	}
	attempts := []executionAttempt{
		{ProjectID: "prj_borrower", Auth: authByID},
		{ProjectID: "prj_borrower", Auth: authByName},
	}
	view := tracker.view(pluginConfig{SmartKeys: []smartKeyConfig{owner}}, "prj_borrower", attempts, now.Add(time.Second))
	for _, attempt := range attempts {
		if got := view.penalty(attempt); got <= 0 {
			t.Fatalf("owner resolved penalty for %q = %.3f, want > 0", stableAuthIndex(attempt.Auth), got)
		}
	}
}

func TestProjectDemandDoesNotReserveForDisabledOwner(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	disabled := false
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{{
		ID: "prj_disabled", Enabled: &disabled, Status: projectStatusDisabled,
		PrimaryAuthIDs: []string{"auth-disabled"},
	}}}
	attempt := demandTestAttempt("prj_borrower", "auth-disabled", false)
	view := tracker.view(cfg, attempt.ProjectID, []executionAttempt{attempt}, now)
	if got := view.penalty(attempt); got != 0 {
		t.Fatalf("disabled owner penalty = %.3f, want 0", got)
	}
}

func TestProjectDemandTrackerIsSafeUnderConcurrentLeaseUpdates(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	attempt := demandTestAttempt("prj_hot", "auth-shared", false)
	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func(offset int) {
			defer wait.Done()
			release := tracker.begin(attempt, now.Add(time.Duration(offset)*time.Millisecond))
			release(offset%2 == 0, now.Add(time.Second))
		}(index)
	}
	wait.Wait()
	view := tracker.view(pluginConfig{}, attempt.ProjectID, []executionAttempt{attempt}, now.Add(2*time.Second))
	if got := view.penalty(attempt); got <= 0 {
		t.Fatalf("concurrent demand penalty = %.3f, want > 0", got)
	}
	if got := tracker.inFlight(attempt.ProjectID); got != 0 {
		t.Fatalf("project in-flight = %d, want 0", got)
	}
}

func TestAQCP01IdleOwnerLeavesSafeSurplusBorrowable(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "owned"}
	cfg := demandGuardConfig("owner", auth.AuthIndex)
	if guard := tracker.guard(cfg, "borrower", auth, now); guard != 0 {
		t.Fatalf("idle owner demand guard = %.3f, want 0", guard)
	}
}

func TestAQCP02OwnerActivityRaisesAtomicBorrowerGuard(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	tracker := newProjectDemandTracker(time.Minute)
	bravoProjectDemand = tracker
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "owned-active"}
	cfg := demandGuardConfig("owner", auth.AuthIndex)
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 5, ReservationPercent: 1}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	owner := executionAttempt{ProjectID: "owner", Primary: true, Auth: auth, ReservationPercent: 1}
	releaseOwner := tracker.begin(owner, now)
	releaseOwner(true, now)
	if guard := tracker.guard(cfg, "borrower", auth, now); guard <= 0 {
		t.Fatal("active owner did not raise demand guard")
	}

	installAdaptiveTestQuota(t, auth.AuthIndex, 25, 25)
	borrow := executionAttempt{
		ProjectID: "borrower", Auth: auth, Candidate: candidate{Model: "claude-sonnet-5"},
		AllocatorManaged: true, ReservationPercent: 1, TariffID: "x5",
	}
	if release, acquired := acquireAttemptLease(borrow); acquired {
		release(false)
		t.Fatal("atomic lease ignored active owner's demand guard")
	}
}

func TestAQCP03TenAndSixtyMinuteTempoSurviveShortPause(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "owned-paused"}
	cfg := demandGuardConfig("owner", auth.AuthIndex)
	release := tracker.begin(executionAttempt{ProjectID: "owner", Primary: true, Auth: auth, ReservationPercent: 4}, now.Add(-5*time.Minute))
	release(true, now.Add(-5*time.Minute))
	ownerKey := projectDemandLoanKey{projectID: "owner", authIndex: auth.AuthIndex}
	if rate1 := projectDemandWindowRate(tracker.projects[ownerKey], now, 1); rate1 != 0 {
		t.Fatalf("1-minute rate after pause = %.3f, want 0", rate1)
	}
	if guard := tracker.guard(cfg, "borrower", auth, now); guard <= 0 {
		t.Fatal("10/60-minute history released all owner capacity after a short pause")
	}
}

func TestAQCP04InactiveOwnerDemandDecaysAndStateIsPruned(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "owned-inactive"}
	cfg := demandGuardConfig("owner", auth.AuthIndex)
	release := tracker.begin(executionAttempt{ProjectID: "owner", Primary: true, Auth: auth, ReservationPercent: 4}, now.Add(-3*time.Hour))
	release(true, now.Add(-3*time.Hour))
	tracker.prune(now)
	if guard := tracker.guard(cfg, "borrower", auth, now); guard != 0 {
		t.Fatalf("inactive owner guard = %.3f, want 0", guard)
	}
	if _, exists := tracker.projects[projectDemandLoanKey{projectID: "owner", authIndex: auth.AuthIndex}]; exists {
		t.Fatal("inactive owner state survived TTL pruning")
	}
}

func TestAQCP05MultipleOwnerDemandAggregatesAndStaysCapped(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "shared-owned"}
	cfg := demandGuardConfig("owner-a", auth.AuthIndex)
	cfg.SmartKeys = append(cfg.SmartKeys, smartKeyConfig{ID: "owner-b", Status: projectStatusActive, PrimaryAuthIDs: []string{auth.AuthIndex}})
	for _, ownerID := range []string{"owner-a", "owner-b"} {
		release := tracker.begin(executionAttempt{ProjectID: ownerID, Primary: true, Auth: auth, ReservationPercent: 2}, now)
		release(true, now)
	}
	guard := tracker.guard(cfg, "borrower", auth, now)
	if guard <= 15 || guard > projectDemandMaximumGuard {
		t.Fatalf("two-owner guard = %.3f, want aggregate > one owner and <= %.1f cap", guard, projectDemandMaximumGuard)
	}
}

func TestAQCP06OwnerDemandIsolatedFromUnrelatedAccount(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	authA := pluginapi.HostAuthFileEntry{AuthIndex: "owned-a"}
	authB := pluginapi.HostAuthFileEntry{AuthIndex: "owned-b"}
	cfg := demandGuardConfig("owner-a", authA.AuthIndex)
	release := tracker.begin(executionAttempt{ProjectID: "owner-a", Primary: true, Auth: authA, ReservationPercent: 2}, now)
	release(true, now)
	if guard := tracker.guard(cfg, "borrower", authA, now); guard <= 0 {
		t.Fatal("related account did not receive owner guard")
	}
	if guard := tracker.guard(cfg, "borrower", authB, now); guard != 0 {
		t.Fatalf("unrelated account inherited %.3f owner guard", guard)
	}
}

func TestProjectDemandOwnerTempoIsScopedToTheCredentialActuallyUsed(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	authUsed := pluginapi.HostAuthFileEntry{AuthIndex: "owner-used"}
	authIdle := pluginapi.HostAuthFileEntry{AuthIndex: "owner-idle"}
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "owner", Status: projectStatusActive,
		PrimaryAuthIDs: []string{authUsed.AuthIndex, authIdle.AuthIndex},
	}}

	release := tracker.begin(executionAttempt{
		ProjectID: "owner", Primary: true, Auth: authUsed, ReservationPercent: 4,
	}, now)
	release(true, now)

	if guard := tracker.guard(cfg, "borrower", authUsed, now); guard <= 0 {
		t.Fatal("used owner credential did not receive its demand guard")
	}
	if guard := tracker.guard(cfg, "borrower", authIdle, now); guard != 0 {
		t.Fatalf("idle owner credential inherited %.3f%% demand from another primary", guard)
	}
}

func TestProjectDemandMapsAreHardBounded(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	for index := 0; index < projectDemandMaximumEntries+100; index++ {
		attempt := demandTestAttempt(fmt.Sprintf("project-%05d", index), fmt.Sprintf("auth-%05d", index), false)
		release := tracker.begin(attempt, now.Add(time.Duration(index)*time.Second))
		release(true, now.Add(time.Duration(index)*time.Second))
	}
	tracker.mu.Lock()
	projects, loans := len(tracker.projects), len(tracker.loans)
	tracker.mu.Unlock()
	if projects > projectDemandMaximumEntries || loans > projectDemandMaximumEntries {
		t.Fatalf("demand maps projects=%d loans=%d, cap=%d", projects, loans, projectDemandMaximumEntries)
	}
}

func TestProjectDemandMapsStayBoundedWhenEveryEntryIsInFlight(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	releases := make([]func(bool, time.Time), 0, 2*(projectDemandMaximumEntries+100))
	for index := 0; index < projectDemandMaximumEntries+100; index++ {
		projectID := fmt.Sprintf("project-active-%05d", index)
		releases = append(releases,
			tracker.begin(demandTestAttempt(projectID, fmt.Sprintf("auth-primary-%05d", index), true), now),
			tracker.begin(demandTestAttempt(projectID, fmt.Sprintf("auth-loan-%05d", index), false), now),
		)
	}
	tracker.mu.Lock()
	projects, loans := len(tracker.projects), len(tracker.loans)
	projectOverflow, loanOverflow := len(tracker.projectOverflow), len(tracker.loanOverflow)
	tracker.mu.Unlock()
	if projects > projectDemandMaximumEntries || loans > projectDemandMaximumEntries {
		t.Fatalf("all-active demand maps projects=%d loans=%d, cap=%d", projects, loans, projectDemandMaximumEntries)
	}
	if projectOverflow > projectDemandMaximumOverflow || loanOverflow > projectDemandMaximumOverflow {
		t.Fatalf("all-active overflow maps projects=%d loans=%d, cap=%d", projectOverflow, loanOverflow, projectDemandMaximumOverflow)
	}
	for _, release := range releases {
		release(false, now.Add(time.Second))
	}
}

func TestProjectDemandOverflowIsCredentialScopedAtSaturation(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	releases := make([]func(bool, time.Time), 0, projectDemandMaximumEntries+1)
	for index := 0; index < projectDemandMaximumEntries; index++ {
		releases = append(releases, tracker.begin(demandTestAttempt(
			fmt.Sprintf("saturation-%05d", index), "hot-auth", true,
		), now))
	}
	releases = append(releases, tracker.begin(demandTestAttempt("overflow-owner", "hot-auth", true), now))
	cfg := defaultPluginConfig()
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	cfg.SmartKeys = []smartKeyConfig{
		{ID: "overflow-owner", Status: projectStatusActive, PrimaryAuthIDs: []string{"hot-auth"}},
		{ID: "idle-owner", Status: projectStatusActive, PrimaryAuthIDs: []string{"idle-auth"}},
	}
	if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: "hot-auth"}, now); guard <= 0 {
		t.Fatal("saturated hot credential lost its scoped overflow guard")
	}
	if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: "idle-auth"}, now); guard != 0 {
		t.Fatalf("unrelated idle credential inherited %.3f%% overflow guard", guard)
	}
	for _, release := range releases {
		release(false, now.Add(time.Second))
	}
}

func TestProjectDemandOverflowCapPlusOneFailsClosedOnlyForUntrackableCredential(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tracker.mu.Lock()
	for index := 0; index < projectDemandMaximumEntries; index++ {
		key := projectDemandLoanKey{projectID: fmt.Sprintf("active-%05d", index), authIndex: fmt.Sprintf("active-auth-%05d", index)}
		tracker.projects[key] = &projectDemandSample{at: now, lastActivity: now, inFlight: 1}
	}
	for index := 0; index < projectDemandMaximumOverflow; index++ {
		authIndex := fmt.Sprintf("overflow-auth-%05d", index)
		tracker.projectOverflow[authIndex] = &projectDemandSample{at: now, lastActivity: now, inFlight: 1}
	}
	tracker.lastPrune = now
	tracker.mu.Unlock()
	release := tracker.begin(demandTestAttempt("untrackable-owner", "untrackable-auth", true), now)
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{
		{ID: "untrackable-owner", Status: projectStatusActive, PrimaryAuthIDs: []string{"untrackable-auth"}},
		{ID: "unrelated-owner", Status: projectStatusActive, PrimaryAuthIDs: []string{"unrelated-auth"}},
	}
	if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: "untrackable-auth"}, now); guard != projectDemandMaximumGuard {
		t.Fatalf("untrackable credential guard = %.3f, want fail-closed %.3f", guard, projectDemandMaximumGuard)
	}
	if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: "unrelated-auth"}, now); guard != 0 {
		t.Fatalf("unrelated credential inherited %.3f saturation guard", guard)
	}
	release(false, now.Add(time.Second))
	tracker.mu.Lock()
	blocked := len(tracker.projectBlocked)
	tracker.mu.Unlock()
	if blocked > projectDemandMaximumOverflow {
		t.Fatalf("blocked credential markers = %d, cap=%d", blocked, projectDemandMaximumOverflow)
	}
}

func TestProjectDemandBlockedMarkerSaturationNeverReopensEvictedCredential(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tracker.mu.Lock()
	for index := 0; index < projectDemandMaximumOverflow; index++ {
		authIndex := fmt.Sprintf("blocked-%05d", index)
		if markDemandBlockedLocked(tracker.projectBlocked, authIndex, now.Add(time.Duration(index)*time.Second)) {
			t.Fatalf("exact blocked marker %d unexpectedly saturated", index)
		}
	}
	if !markDemandBlockedLocked(tracker.projectBlocked, "blocked-over-cap", now.Add(time.Hour)) {
		t.Fatal("cap+1 blocked credential did not request global fail-closed mode")
	}
	tracker.projectSaturated = true
	tracker.mu.Unlock()

	cfg := defaultPluginConfig()
	for _, authIndex := range []string{"blocked-00000", "blocked-over-cap", "previously-unseen"} {
		if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: authIndex}, now); guard != projectDemandMaximumGuard {
			t.Fatalf("credential %q reopened after blocked marker saturation: guard %.3f", authIndex, guard)
		}
	}
}

func TestProjectDemandLoanMarkerSaturationKeepsPenaltyFailClosed(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tracker.mu.Lock()
	for index := 0; index < projectDemandMaximumOverflow; index++ {
		authIndex := fmt.Sprintf("loan-blocked-%05d", index)
		if markDemandBlockedLocked(tracker.loanBlocked, authIndex, now.Add(time.Duration(index)*time.Second)) {
			t.Fatalf("exact loan marker %d unexpectedly saturated", index)
		}
	}
	if !markDemandBlockedLocked(tracker.loanBlocked, "loan-over-cap", now.Add(time.Hour)) {
		t.Fatal("cap+1 loan marker did not request global fail-closed mode")
	}
	tracker.loanSaturated = true
	tracker.mu.Unlock()

	attempts := []executionAttempt{
		demandTestAttempt("borrower", "loan-blocked-00000", false),
		demandTestAttempt("borrower", "loan-over-cap", false),
		demandTestAttempt("borrower", "previously-unseen", false),
	}
	view := tracker.view(defaultPluginConfig(), "borrower", attempts, now)
	for _, attempt := range attempts {
		if penalty := view.penalty(attempt); penalty < projectDemandMaximumGuard*borrowActivityWeight {
			t.Fatalf("credential %q loan penalty reopened after saturation: %.3f", attempt.Auth.AuthIndex, penalty)
		}
	}
}

func TestProjectDemandBlockedLongRunningWorkSurvivesTTL(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-projectDemandStateTTL - time.Hour)
	tracker.mu.Lock()
	tracker.projectBlocked["long-primary"] = stale
	tracker.loanBlocked["long-loan"] = stale
	tracker.pruneLocked(now)
	_, primaryBlocked := tracker.projectBlocked["long-primary"]
	_, loanBlocked := tracker.loanBlocked["long-loan"]
	tracker.mu.Unlock()
	if !primaryBlocked || !loanBlocked {
		t.Fatalf("TTL reopened long-running blocked demand: primary=%t loan=%t", primaryBlocked, loanBlocked)
	}
	cfg := defaultPluginConfig()
	if guard := tracker.guard(cfg, "borrower", pluginapi.HostAuthFileEntry{AuthIndex: "long-primary"}, now); guard != projectDemandMaximumGuard {
		t.Fatalf("long-running blocked primary guard = %.3f, want %.3f", guard, projectDemandMaximumGuard)
	}
	attempt := demandTestAttempt("borrower", "long-loan", false)
	if penalty := tracker.view(cfg, "borrower", []executionAttempt{attempt}, now).penalty(attempt); penalty < projectDemandMaximumGuard*borrowActivityWeight {
		t.Fatalf("long-running blocked loan penalty = %.3f, want sticky fail-closed fairness", penalty)
	}
}

func TestProjectDemandTTLMarkersClearOnlyThroughIdleAuthenticatedReconcile(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	previousTracker := bravoProjectDemand
	tracker := newProjectDemandTracker(time.Minute)
	bravoProjectDemand = tracker
	defer func() { bravoProjectDemand = previousTracker }()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-projectDemandStateTTL - time.Hour)
	tracker.mu.Lock()
	tracker.projectBlocked["reconcile-primary"] = stale
	tracker.loanBlocked["reconcile-loan"] = stale
	tracker.projectSaturated = true
	tracker.loanSaturated = true
	tracker.projectOverflow["live-overflow"] = &projectDemandSample{at: stale, lastActivity: stale, inFlight: 1}
	tracker.pruneLocked(now)
	tracker.mu.Unlock()

	status, body := callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 409 {
		t.Fatalf("reconcile with live demand status=%d body=%#v, want 409", status, body)
	}
	tracker.mu.Lock()
	stillSaturated := tracker.projectSaturated && tracker.loanSaturated && len(tracker.projectBlocked) == 1 && len(tracker.loanBlocked) == 1
	tracker.projectOverflow["live-overflow"].inFlight = 0
	tracker.mu.Unlock()
	if !stillSaturated {
		t.Fatal("failed reconciliation cleared sticky demand saturation")
	}

	status, body = callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 200 || body["demand_saturated"] != false {
		t.Fatalf("idle demand reconciliation status=%d body=%#v, want truthful available state", status, body)
	}
	tracker.mu.Lock()
	projectSaturated, loanSaturated := tracker.projectSaturated, tracker.loanSaturated
	projectMarkers, loanMarkers := len(tracker.projectBlocked), len(tracker.loanBlocked)
	tracker.mu.Unlock()
	if projectSaturated || loanSaturated || projectMarkers != 0 || loanMarkers != 0 {
		t.Fatalf("authenticated reconcile retained demand state project=%t/%d loan=%t/%d", projectSaturated, projectMarkers, loanSaturated, loanMarkers)
	}
}

func TestProjectDemandUntrackedSaturatedLeaseBlocksReconcileUntilRelease(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	previousTracker := bravoProjectDemand
	tracker := newProjectDemandTracker(time.Minute)
	bravoProjectDemand = tracker
	defer func() { bravoProjectDemand = previousTracker }()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tracker.mu.Lock()
	for index := 0; index < projectDemandMaximumEntries; index++ {
		key := projectDemandLoanKey{projectID: fmt.Sprintf("tracked-%05d", index), authIndex: fmt.Sprintf("tracked-auth-%05d", index)}
		tracker.projects[key] = &projectDemandSample{at: now, lastActivity: now, inFlight: 1}
	}
	for index := 0; index < projectDemandMaximumOverflow; index++ {
		authIndex := fmt.Sprintf("overflow-%05d", index)
		tracker.projectOverflow[authIndex] = &projectDemandSample{at: now, lastActivity: now, inFlight: 1}
	}
	tracker.lastPrune = now
	tracker.mu.Unlock()

	releaseExtra := tracker.begin(demandTestAttempt("untracked-owner", "untracked-auth", true), now)
	tracker.mu.Lock()
	if tracker.untrackedProjectInFlight != 1 {
		tracker.mu.Unlock()
		t.Fatalf("untracked primary counter = %d, want 1", tracker.untrackedProjectInFlight)
	}
	// All representable leases complete; only the cap+1 lease remains live.
	for _, sample := range tracker.projects {
		sample.inFlight = 0
	}
	for _, sample := range tracker.projectOverflow {
		sample.inFlight = 0
	}
	tracker.mu.Unlock()

	status, body := callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 409 {
		t.Fatalf("untracked live lease reconcile status=%d body=%#v, want 409", status, body)
	}
	releaseExtra(false, now.Add(time.Minute))
	tracker.mu.Lock()
	untracked := tracker.untrackedProjectInFlight
	tracker.mu.Unlock()
	if untracked != 0 {
		t.Fatalf("released untracked lease counter = %d, want 0", untracked)
	}
	status, body = callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 200 || body["demand_saturated"] != false {
		t.Fatalf("released untracked reconcile status=%d body=%#v, want 200/available", status, body)
	}
}

func TestProjectDemandMaintenanceIsAmortizedAcrossCandidates(t *testing.T) {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tracker.maintain(now)
	tracker.mu.Lock()
	initialRuns := tracker.pruneRuns
	tracker.mu.Unlock()
	for candidateIndex := 0; candidateIndex < 100; candidateIndex++ {
		tracker.maintain(now.Add(30 * time.Second))
	}
	tracker.mu.Lock()
	runsWithinWindow := tracker.pruneRuns
	tracker.mu.Unlock()
	if runsWithinWindow != initialRuns {
		t.Fatalf("%d candidate checks triggered %d extra full prunes", 100, runsWithinWindow-initialRuns)
	}
	tracker.maintain(now.Add(61 * time.Second))
	tracker.mu.Lock()
	runsAfterWindow := tracker.pruneRuns
	tracker.mu.Unlock()
	if runsAfterWindow != initialRuns+1 {
		t.Fatalf("amortized maintenance runs = %d, want %d", runsAfterWindow, initialRuns+1)
	}
}

func demandGuardConfig(ownerID, authIndex string) pluginConfig {
	cfg := defaultPluginConfig()
	cfg.QuotaUsageRefreshSeconds = 15 * 60
	cfg.SmartKeys = []smartKeyConfig{{
		ID: ownerID, Status: projectStatusActive, PrimaryAuthIDs: []string{authIndex},
	}}
	return cfg
}

func demandTestAttempt(projectID, authIndex string, primary bool) executionAttempt {
	return executionAttempt{
		ProjectID: projectID,
		Primary:   primary,
		Auth: pluginapi.HostAuthFileEntry{
			ID:        authIndex + "-id",
			AuthIndex: authIndex,
			Name:      authIndex + "-name",
		},
	}
}
