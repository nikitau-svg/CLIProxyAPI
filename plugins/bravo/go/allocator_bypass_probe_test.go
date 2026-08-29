package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAllocatorBypassPlanMarksOnlyStaleScheduledResetAsProbe(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"plan-auth": quota})
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText},
	}}}
	auths := []pluginapi.HostAuthFileEntry{{ID: "plan-auth", AuthIndex: "plan-auth", Provider: "claude"}}
	rejections := []candidateRejection{{
		Provider: "claude", Model: "claude-sonnet-5", Stage: "allocator",
		Code: "bravo_allocator_withheld", Reason: "stale scheduled reset",
	}}
	plan := allocatorBypassPlan("sonnet", model, textContract(), auths, rejections, "probe-plan", now)
	if len(plan) != 1 || !plan[0].AllocatorBypass || plan[0].AllocatorBypassProbe == nil ||
		plan[0].AllocatorBypassProbe.plannedGeneration == "" {
		t.Fatalf("stale reset bypass plan=%#v", plan)
	}

	// The same zero after a provider observation at/after ResetAt is fresh
	// exhaustion, not another reset probe.
	freshZero := quota
	freshZero.ConfirmedAt = quota.Session.ResetAt
	freshZero.RefreshedAt = freshZero.ConfirmedAt
	storeQuotaSnapshot("plan-auth", freshZero)
	if freshPlan := allocatorBypassPlan("sonnet", model, textContract(), auths, rejections, "fresh-plan", now); len(freshPlan) != 0 {
		t.Fatalf("fresh confirmed zero produced %d bypass attempts", len(freshPlan))
	}
}

func TestAllocatorBypassResetProbeSingleFlightsOneHundredConcurrentAttemptsWithoutQueue(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"reset-auth": quota})

	const workers = 100
	type result struct {
		attempt  executionAttempt
		release  func(bool)
		acquired bool
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		attempt := allocatorBypassProbeTestAttempt("reset-auth", now)
		go func(attempt executionAttempt) {
			defer group.Done()
			<-start
			release, acquired, failure := acquireExecutionAttemptLease(attempt)
			if failure != nil {
				t.Errorf("unexpected lease failure: %#v", failure)
			}
			results <- result{attempt: attempt, release: release, acquired: acquired}
		}(attempt)
	}
	close(start)
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reset probe acquisition waited or deadlocked")
	}

	acquired := make([]result, 0, 1)
	for index := 0; index < workers; index++ {
		item := <-results
		if item.acquired {
			acquired = append(acquired, item)
		}
	}
	if len(acquired) != 1 {
		t.Fatalf("concurrent reset probes acquired=%d, want 1", len(acquired))
	}
	// A real dispatch consumes the old reset generation even when the host call
	// later returns through release(false).
	markAllocatorBypassProbeDispatched(acquired[0].attempt, now.Add(time.Millisecond))
	acquired[0].release(false)
	_, again, failure := acquireExecutionAttemptLease(allocatorBypassProbeTestAttempt("reset-auth", now))
	if again || failure != nil {
		t.Fatalf("consumed reset generation reacquired=%v failure=%#v", again, failure)
	}
}

func TestAllocatorBypassResetProbePreDispatchFailureReleasesButCommitConsumes(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"release-auth": quota})

	first := allocatorBypassProbeTestAttempt("release-auth", now)
	release, acquired, failure := acquireExecutionAttemptLease(first)
	if !acquired || failure != nil {
		t.Fatalf("first probe acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
	second := allocatorBypassProbeTestAttempt("release-auth", now)
	release, acquired, failure = acquireExecutionAttemptLease(second)
	if !acquired || failure != nil {
		t.Fatalf("pre-dispatch release did not reopen: acquired=%v failure=%#v", acquired, failure)
	}
	// Commit is a second proof of dispatch for callers that do not use the
	// explicit marker.
	release(true)
	_, acquired, failure = acquireExecutionAttemptLease(allocatorBypassProbeTestAttempt("release-auth", now))
	if acquired || failure != nil {
		t.Fatalf("committed generation reacquired=%v failure=%#v", acquired, failure)
	}
}

func TestAllocatorBypassResetProbeFreshQuotaSupersedesConsumedGeneration(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"fresh-auth": quota})
	attempt := allocatorBypassProbeTestAttempt("fresh-auth", now)
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("stale reset probe acquired=%v failure=%#v", acquired, failure)
	}
	markAllocatorBypassProbeDispatched(attempt, now)
	release(false)

	// The provider has now confirmed a new healthy window. The old consumed
	// generation must not block ordinary availability.
	fresh := allocatorBypassProbeTestQuota(now, 80)
	fresh.ConfirmedAt = now.Add(time.Minute)
	fresh.RefreshedAt = fresh.ConfirmedAt
	fresh.Session.ResetAt = now.Add(time.Hour)
	storeQuotaSnapshot("fresh-auth", fresh)
	freshAttempt := allocatorBypassProbeTestAttempt("fresh-auth", now.Add(time.Minute))
	release, acquired, failure = acquireExecutionAttemptLease(freshAttempt)
	if !acquired || failure != nil {
		t.Fatalf("fresh quota stayed blocked: acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
}

func TestAllocatorBypassUnknownQuotaRemainsFailOpenAndNeverTakesProbeGuard(t *testing.T) {
	now := time.Now().UTC()
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{
		"unknown-auth": {Confidence: "unknown"},
	})
	const workers = 30
	for index := 0; index < workers; index++ {
		attempt := allocatorBypassProbeTestAttempt("unknown-auth", now)
		release, acquired, failure := acquireExecutionAttemptLease(attempt)
		if !acquired || failure != nil {
			t.Fatalf("unknown fail-open %d acquired=%v failure=%#v", index, acquired, failure)
		}
		release(false)
	}
	allocatorBypassProbeRuntime.Lock()
	entries := len(allocatorBypassProbeRuntime.Entries)
	allocatorBypassProbeRuntime.Unlock()
	if entries != 0 {
		t.Fatalf("unknown quota created %d reset-probe entries", entries)
	}
}

func TestAllocatorBypassResetProbeRuntimeIsBoundedAndPrunesOldEntries(t *testing.T) {
	now := time.Now().UTC()
	quota := allocatorBypassProbeTestQuota(now, 0)
	installAllocatorBypassProbeTestState(t, map[string]credentialQuotaState{"bounded-auth": quota})
	allocatorBypassProbeRuntime.Lock()
	for index := 0; index < allocatorBypassProbeMaximumEntries; index++ {
		key := allocatorBypassProbeKey("bounded-auth", fmt.Sprintf("old-generation-%d", index))
		allocatorBypassProbeRuntime.Entries[key] = allocatorBypassProbeEntry{
			LeaseID: uint64(index + 1), Consumed: true,
			UpdatedAt: now.Add(-24 * time.Hour),
		}
	}
	allocatorBypassProbeRuntime.Unlock()
	release, acquired, failure := acquireExecutionAttemptLease(allocatorBypassProbeTestAttempt("bounded-auth", now))
	if !acquired || failure != nil {
		t.Fatalf("old bounded entries were not pruned: acquired=%v failure=%#v", acquired, failure)
	}
	release(false)

	allocatorBypassProbeRuntime.Lock()
	allocatorBypassProbeRuntime.Entries = make(map[string]allocatorBypassProbeEntry, allocatorBypassProbeMaximumEntries)
	for index := 0; index < allocatorBypassProbeMaximumEntries; index++ {
		allocatorBypassProbeRuntime.Entries[fmt.Sprintf("fresh-%d", index)] = allocatorBypassProbeEntry{
			LeaseID: uint64(index + 1), Consumed: true, UpdatedAt: now,
		}
	}
	allocatorBypassProbeRuntime.Unlock()
	_, acquired, failure = acquireExecutionAttemptLease(allocatorBypassProbeTestAttempt("bounded-auth", now))
	if acquired || failure != nil {
		t.Fatalf("saturated reset runtime acquired=%v failure=%#v", acquired, failure)
	}
	allocatorBypassProbeRuntime.Lock()
	saturated, dropped, entries := allocatorBypassProbeRuntime.Saturated,
		allocatorBypassProbeRuntime.Dropped, len(allocatorBypassProbeRuntime.Entries)
	allocatorBypassProbeRuntime.Unlock()
	if !saturated || dropped != 1 || entries != allocatorBypassProbeMaximumEntries {
		t.Fatalf("bounded runtime saturated=%v dropped=%d entries=%d", saturated, dropped, entries)
	}
}

func TestAllocatorBypassResetProbeCleanupAcrossManyAuthsAndGenerations(t *testing.T) {
	now := time.Now().UTC()
	const authCount = 32
	const generationsPerAuth = allocatorBypassProbeMaximumEntries / authCount
	quotas := make(map[string]credentialQuotaState, authCount)
	for authIndex := 0; authIndex < authCount; authIndex++ {
		quotas[fmt.Sprintf("matrix-auth-%d", authIndex)] = allocatorBypassProbeTestQuota(now, 0)
	}
	installAllocatorBypassProbeTestState(t, quotas)
	allocatorBypassProbeRuntime.Lock()
	leaseID := uint64(0)
	for authIndex := 0; authIndex < authCount; authIndex++ {
		auth := fmt.Sprintf("matrix-auth-%d", authIndex)
		for generation := 0; generation < generationsPerAuth; generation++ {
			leaseID++
			key := allocatorBypassProbeKey(auth, fmt.Sprintf("obsolete-%d", generation))
			allocatorBypassProbeRuntime.Entries[key] = allocatorBypassProbeEntry{
				LeaseID: leaseID, Consumed: true, UpdatedAt: now,
			}
		}
	}
	allocatorBypassProbeRuntime.Unlock()

	for authIndex := 0; authIndex < authCount; authIndex++ {
		auth := fmt.Sprintf("matrix-auth-%d", authIndex)
		attempt := allocatorBypassProbeTestAttempt(auth, now)
		release, acquired, failure := acquireExecutionAttemptLease(attempt)
		if !acquired || failure != nil {
			t.Fatalf("auth %s could not replace obsolete generations: acquired=%v failure=%#v", auth, acquired, failure)
		}
		markAllocatorBypassProbeDispatched(attempt, now)
		release(false)
	}
	allocatorBypassProbeRuntime.Lock()
	entries := len(allocatorBypassProbeRuntime.Entries)
	allocatorBypassProbeRuntime.Unlock()
	if entries != authCount {
		t.Fatalf("multi-generation cleanup left %d entries, want one current generation per auth (%d)", entries, authCount)
	}
}

func installAllocatorBypassProbeTestState(t *testing.T, quotas map[string]credentialQuotaState) {
	t.Helper()
	installSubscriptionCommunismTestState(t, quotas)
	resetAllocatorBypassProbeForTest()
	t.Cleanup(resetAllocatorBypassProbeForTest)
}

func allocatorBypassProbeTestQuota(now time.Time, remaining float64) credentialQuotaState {
	return credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-2 * time.Hour), RefreshedAt: now.Add(-2 * time.Hour),
		Session: quotaWindowState{
			UsedPercent: 100 - remaining, RemainingPercent: remaining,
			ResetAt: now.Add(-time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled,
		},
		Weekly: quotaWindowState{
			UsedPercent: 0, RemainingPercent: 100,
			ResetAt: now.Add(24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled,
		},
	}
}

func allocatorBypassProbeTestAttempt(authIndex string, now time.Time) executionAttempt {
	item := candidate{Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText}}
	auth := pluginapi.HostAuthFileEntry{ID: authIndex, AuthIndex: authIndex, Provider: "claude"}
	decision := allocatorBypassAuthDecision(loadedConfig(), item, auth, now)
	return executionAttempt{
		LogicalModel: "sonnet", Candidate: item, Auth: auth,
		AllocatorBypass: true, AllocatorBypassProbe: newAllocatorBypassProbeAttemptState(decision),
	}
}
