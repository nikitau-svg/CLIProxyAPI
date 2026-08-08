package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageCheckpointIOCannotBlockProviderAdmission(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "nonblocking-checkpoint.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	const workers = 4
	for index := 0; index < workers; index++ {
		setAdaptivePersistenceQuota(t, fmt.Sprintf("checkpoint-auth-%d", index), 80)
	}
	stopAdaptiveCheckpointTimerForTest()

	enteredIO := make(chan struct{}, 1)
	unblockIO := make(chan struct{})
	var unblockOnce sync.Once
	previousHook := usageSnapshotIOHook
	usageSnapshotIOHook = func() {
		enteredIO <- struct{}{}
		<-unblockIO
	}
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(unblockIO) })
		usageSnapshotIOHook = previousHook
	})
	checkpointDone := make(chan error, 1)
	go func() { checkpointDone <- bravoUsageState.flush() }()
	select {
	case <-enteredIO:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint did not reach injected filesystem barrier")
	}

	type admissionResult struct {
		release  func(bool)
		acquired bool
	}
	admissions := make(chan admissionResult, workers)
	started := time.Now()
	for index := 0; index < workers; index++ {
		index := index
		go func() {
			attempt := adaptivePersistenceAttempt(fmt.Sprintf("checkpoint-auth-%d", index), 0.5)
			attempt.Primary = true
			release, acquired := acquireAttemptLease(attempt)
			admissions <- admissionResult{release: release, acquired: acquired}
		}()
	}
	for index := 0; index < workers; index++ {
		select {
		case result := <-admissions:
			if !result.acquired {
				t.Fatalf("worker %d was rejected during checkpoint I/O", index)
			}
			result.release(false)
		case <-time.After(2 * time.Second):
			t.Fatalf("worker %d admission blocked behind checkpoint filesystem I/O", index)
		}
	}
	elapsed := time.Since(started)
	t.Logf("four provider admissions while checkpoint I/O was paused: %s", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("four admissions took %s while checkpoint I/O was paused", elapsed)
	}

	unblockOnce.Do(func() { close(unblockIO) })
	if errCheckpoint := <-checkpointDone; errCheckpoint != nil {
		t.Fatal(errCheckpoint)
	}
	waitAdaptiveWALIdleForTest(t)
	usageSnapshotIOHook = previousHook
	stopAdaptiveCheckpointTimerForTest()
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	for index := 0; index < workers; index++ {
		if got := pendingReservationPercent(fmt.Sprintf("checkpoint-auth-%d", index)); got != 0 {
			t.Fatalf("worker %d proven rejection restarted with %.3f debt", index, got)
		}
	}
}

func TestCooldownPersistenceIOCannotBlockUnrelatedProviderStarts(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "nonblocking-cooldown.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	const workers = 4
	for index := 0; index < workers; index++ {
		setAdaptivePersistenceQuota(t, fmt.Sprintf("cooldown-unrelated-%d", index), 80)
	}
	stopAdaptiveCheckpointTimerForTest()

	enteredIO := make(chan struct{}, 1)
	unblockIO := make(chan struct{})
	var unblockOnce sync.Once
	previousHook := usageSnapshotIOHook
	usageSnapshotIOHook = func() {
		enteredIO <- struct{}{}
		<-unblockIO
	}
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(unblockIO) })
		usageSnapshotIOHook = previousHook
	})

	cooldownDone := make(chan struct{})
	go func() {
		defer close(cooldownDone)
		applyFailureCooldown(executionAttempt{
			Auth: pluginapi.HostAuthFileEntry{
				ID:        "cooldown-failing-auth",
				AuthIndex: "cooldown-failing-index",
				Provider:  "claude",
			},
			Candidate: candidate{Provider: "claude", Model: "claude-fable-5"},
		}, executionFailure{
			Code:       "upstream_overloaded",
			Status:     529,
			Retryable:  true,
			RetryAfter: "3600",
		})
	}()
	select {
	case <-enteredIO:
	case <-time.After(2 * time.Second):
		t.Fatal("cooldown persistence did not reach injected filesystem barrier")
	}

	type providerStartResult struct {
		release  func(bool)
		acquired bool
	}
	results := make(chan providerStartResult, workers)
	var providerStarts atomic.Int32
	started := time.Now()
	for index := 0; index < workers; index++ {
		index := index
		go func() {
			attempt := adaptivePersistenceAttempt(fmt.Sprintf("cooldown-unrelated-%d", index), 0.5)
			attempt.Primary = true
			release, acquired := acquireAttemptLease(attempt)
			if acquired {
				// The real executor crosses the provider boundary immediately after
				// this lease gate. Count that boundary without involving the host ABI,
				// whose cancellation contract has an independent integration suite.
				providerStarts.Add(1)
			}
			results <- providerStartResult{release: release, acquired: acquired}
		}()
	}
	for index := 0; index < workers; index++ {
		select {
		case result := <-results:
			if !result.acquired {
				t.Fatalf("unrelated worker %d was rejected during cooldown I/O", index)
			}
			result.release(false)
		case <-time.After(2 * time.Second):
			t.Fatalf("unrelated worker %d blocked behind cooldown filesystem I/O", index)
		}
	}
	if got := providerStarts.Load(); got != workers {
		t.Fatalf("provider starts = %d, want %d", got, workers)
	}
	elapsed := time.Since(started)
	t.Logf("four unrelated provider starts while cooldown fsync was paused: %s", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("four provider starts took %s while cooldown fsync was paused", elapsed)
	}

	unblockOnce.Do(func() { close(unblockIO) })
	select {
	case <-cooldownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cooldown persistence did not finish after filesystem barrier release")
	}
	usageSnapshotIOHook = previousHook
	waitAdaptiveWALIdleForTest(t)
	stopAdaptiveCheckpointTimerForTest()
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if !cooldownActive("claude", "cooldown-failing-auth", "claude-fable-5", time.Now()) {
		t.Fatal("cooldown did not survive restart after nonblocking persistence")
	}
}
