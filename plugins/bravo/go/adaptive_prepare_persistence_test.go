package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAdaptivePrepareClosesCrashGapBeforeProviderIO(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "prepare-crash-auth", 56)
	_, acquired := acquireAttemptLease(adaptivePersistenceAttempt("prepare-crash-auth", 3))
	if !acquired {
		t.Fatal("lease was not acquired")
	}

	// Simulate a crash after durable admission and before any release callback.
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("prepare-crash-auth"); got != 3 {
		t.Fatalf("unresolved prepare restored as %.3f, want 3", got)
	}
	if _, reopened := acquireAttemptLease(adaptivePersistenceAttempt("prepare-crash-auth", 3)); reopened {
		t.Fatal("unresolved prepare reopened the protected secondary after restart")
	}
}

func TestAdaptiveFinalizeMovesPrepareWithoutDoubleCharge(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "finalize-auth", 80)
	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("finalize-auth", 3))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	release(true)

	bravoUsageState.mu.RLock()
	pending := bravoUsageState.state.AdaptiveQuota.Pending["finalize-auth"]
	prepared := bravoUsageState.state.AdaptiveQuota.Prepared["finalize-auth"]
	bravoUsageState.mu.RUnlock()
	if pending == nil || pending.Percent != 3 || prepared != nil {
		t.Fatalf("finalized durable ledgers = pending %#v prepared %#v", pending, prepared)
	}
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("finalize-auth"); got != 3 {
		t.Fatalf("finalized restart charge = %.3f, want 3 (not 0 or 6)", got)
	}
}

func TestAdaptiveRefreshNeverConsumesLivePrepareBeyondOneHundredPercent(t *testing.T) {
	for _, outcome := range []struct {
		name       string
		crashFirst bool
		commit     bool
		want       float64
	}{
		{name: "crash-before-finalize", crashFirst: true, want: 10},
		{name: "proven-rejection", commit: false, want: 0},
		{name: "accepted", commit: true, want: 10},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			restoreUsage := isolateBravoUsageState(t)
			defer restoreUsage()
			resetAdaptiveReserveForTest()
			defer resetAdaptiveReserveForTest()

			path := filepath.Join(t.TempDir(), "bravo-state.json")
			if errConfigure := configureUsageState(path); errConfigure != nil {
				t.Fatal(errConfigure)
			}
			const authIndex = "over-one-hundred-auth"
			persistAdaptivePendingCommit(authIndex, 110, time.Now())
			allocatorRuntime.Lock()
			allocatorRuntime.PendingPercent[authIndex] = 110
			allocatorRuntime.Unlock()
			refreshWatermark := captureAdaptiveRefreshWatermark(authIndex)

			attempt := adaptivePersistenceAttempt(authIndex, 10)
			attempt.Primary = true // unknown pinned primary may run while discovery catches up
			release, acquired := acquireAttemptLease(attempt)
			if !acquired {
				t.Fatal("live prepare was not acquired")
			}
			// This is the watermark a provider refresh captured before its I/O.
			// It contains Pending only; the concurrent Prepared lease is newer.
			clearPendingReservation(authIndex, 110, refreshWatermark)

			bravoUsageState.mu.RLock()
			prepared := bravoUsageState.state.AdaptiveQuota.Prepared[authIndex]
			pending := bravoUsageState.state.AdaptiveQuota.Pending[authIndex]
			bravoUsageState.mu.RUnlock()
			if prepared == nil || prepared.Percent != 10 || pending != nil {
				t.Fatalf("post-refresh durable ledger = pending %#v prepared %#v", pending, prepared)
			}

			if !outcome.crashFirst {
				release(outcome.commit)
				waitAdaptiveWALIdleForTest(t)
			}
			resetAdaptiveReserveForTest()
			simulateFreshBravoProcess(t, path)
			if got := pendingReservationPercent(authIndex); got != outcome.want {
				t.Fatalf("restart pending = %.3f, want %.3f", got, outcome.want)
			}
			if outcome.crashFirst {
				now := time.Now().UTC()
				storeQuotaSnapshot(authIndex, refreshQuotaWithReset(40, now.Add(-time.Minute), now.Add(-time.Second)))
				orphanWatermark := captureAdaptiveRefreshWatermark(authIndex)
				if orphanWatermark.OrphanPreparedPercent != 10 {
					t.Fatalf("restart orphan watermark = %.3f, want 10", orphanWatermark.OrphanPreparedPercent)
				}
				applyQuotaRefreshSuccess(
					authIndex, quotaRefreshResourceUsage, "claude",
					refreshQuotaWithReset(100, now, now.Add(5*time.Hour)),
					orphanWatermark.PendingPercent, now, orphanWatermark,
				)
				waitAdaptiveWALIdleForTest(t)
				resetAdaptiveReserveForTest()
				simulateFreshBravoProcess(t, path)
				if got := pendingReservationPercent(authIndex); got != 0 {
					t.Fatalf("confirmed refresh resurrected orphan after restart: %.3f", got)
				}
			}
		})
	}
}

func TestAdaptiveProvenRejectionClearsPreparedLease(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "rejected-auth", 80)
	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("rejected-auth", 2))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	release(false)
	waitAdaptiveWALIdleForTest(t)
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("rejected-auth"); got != 0 {
		t.Fatalf("proven rejection restored %.3f pending, want 0", got)
	}
}

func TestAdaptiveRetriesKeepIndependentDurableLeases(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "retry-auth", 90)
	first, firstAcquired := acquireAttemptLease(adaptivePersistenceAttempt("retry-auth", 1))
	_, secondAcquired := acquireAttemptLease(adaptivePersistenceAttempt("retry-auth", 2))
	if !firstAcquired || !secondAcquired {
		t.Fatalf("retry leases acquired = %v/%v", firstAcquired, secondAcquired)
	}
	first(false)
	waitAdaptiveWALIdleForTest(t)

	// The second provider call is still unresolved when the process dies. Wait
	// for the first request's proven rejection to reach the ordered writer so the
	// assertion isolates the independent second lease.
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("retry-auth"); got != 2 {
		t.Fatalf("independent retry restart charge = %.3f, want 2", got)
	}
}

func waitAdaptiveWALIdleForTest(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		adaptiveWALRuntime.mu.Lock()
		idle := !adaptiveWALRuntime.flushing && len(adaptiveWALRuntime.pending) == 0
		adaptiveWALRuntime.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("adaptive WAL did not become idle")
		}
		runtime.Gosched()
	}
}

func TestAdaptiveTornFinalizeKeepsEarlierPrepare(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "torn-finalize-auth", 80)
	_, acquired := acquireAttemptLease(adaptivePersistenceAttempt("torn-finalize-auth", 2))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	line, errMarshal := marshalAdaptiveWALRecord(adaptiveWALRecord{
		Version: adaptiveWALVersion, AuthIndex: "torn-finalize-auth", Revision: 2,
		Pending: &persistedAdaptivePendingState{Percent: 2, UpdatedAt: time.Now()}, RecordedAt: time.Now(),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	file, errOpen := os.OpenFile(adaptiveWALPath(path), os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if _, errWrite := file.Write(line[:len(line)/2]); errWrite != nil {
		_ = file.Close()
		t.Fatal(errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("torn-finalize-auth"); got != 2 {
		t.Fatalf("torn finalize lost durable prepare: got %.3f", got)
	}
}

func TestAdaptivePrepareDurabilityFailureBlocksProviderAdmission(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "durability-failure-auth", 80)
	bravoUsageState.mu.Lock()
	if bravoUsageState.saveTimer != nil {
		bravoUsageState.saveTimer.Stop()
		bravoUsageState.saveTimer = nil
	}
	bravoUsageState.path = "/dev/null/bravo-state.json"
	bravoUsageState.mu.Unlock()

	previousAppend := adaptiveWALAppendAndSync
	adaptiveWALAppendAndSync = func(string, []byte) error { return errors.New("injected WAL failure") }
	t.Cleanup(func() { adaptiveWALAppendAndSync = previousAppend })
	_, acquired, failure, effective := acquireExecutionAttemptLeaseDetailed(adaptivePersistenceAttempt("durability-failure-auth", 1))
	if acquired {
		t.Fatal("lease was admitted even though neither WAL nor snapshot could become durable")
	}
	if failure == nil || failure.Code != "bravo_adaptive_durability_unavailable" ||
		effective.AdaptiveTrace.rejectionCause != adaptiveRejectionDurabilityUnavailable {
		t.Fatalf("durability rejection = failure %#v cause %q", failure, effective.AdaptiveTrace.rejectionCause)
	}
	recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.failure(effective, time.Now(), failure.Status, *failure)
	if got := recorder.trace.Attempts[0].AdmissionRejectionCause; got != "durability_unavailable" {
		t.Fatalf("persisted durability rejection cause = %q", got)
	}
	allocatorRuntime.Lock()
	inFlight := allocatorRuntime.InFlightPercent["durability-failure-auth"]
	allocatorRuntime.Unlock()
	if inFlight != 0 {
		t.Fatalf("failed prepare leaked %.3f in-flight", inFlight)
	}
}

func TestAdaptivePreparedLeaseRejectsHotStatePathSwitchAndSurvivesRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	oldPath := filepath.Join(directory, "old-state.json")
	newPath := filepath.Join(directory, "new-state.json")
	if errConfigure := configureUsageState(oldPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "path-switch-auth", 80)
	_, acquired := acquireAttemptLease(adaptivePersistenceAttempt("path-switch-auth", 2.5))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	for index := 0; index < 5; index++ {
		if errConfigure := configureUsageState(newPath); errConfigure == nil {
			t.Fatalf("crash-path toggle %d allowed a state-path switch", index)
		}
		if errConfigure := configureUsageState(oldPath); errConfigure != nil {
			t.Fatalf("crash-path toggle %d failed idempotent source reconfigure: %v", index, errConfigure)
		}
		assertAdaptiveRuntimeLedger(t, "path-switch-auth", 2.5, 0, 2.5)
	}

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, oldPath)
	if got := pendingReservationPercent("path-switch-auth"); got != 2.5 {
		t.Fatalf("restart restored %.3f, want unresolved 2.5", got)
	}
}

func TestAdaptiveHotPathSwitchLiveSuccessDoesNotDoubleCharge(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	oldPath := filepath.Join(directory, "success-old.json")
	newPath := filepath.Join(directory, "success-new.json")
	if errConfigure := configureUsageState(oldPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "switch-success-auth", 80)
	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("switch-success-auth", 2.5))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	if errConfigure := configureUsageState(newPath); errConfigure == nil {
		t.Fatal("unresolved durable prepare allowed a state-path switch")
	}
	assertAdaptiveRuntimeLedger(t, "switch-success-auth", 2.5, 0, 2.5)

	release(true)
	assertAdaptiveRuntimeLedger(t, "switch-success-auth", 0, 2.5, 2.5)
}

func TestAdaptiveHotPathSwitchLiveRejectionLeavesNoPhantom(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	oldPath := filepath.Join(directory, "rejection-old.json")
	newPath := filepath.Join(directory, "rejection-new.json")
	if errConfigure := configureUsageState(oldPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setAdaptivePersistenceQuota(t, "switch-rejection-auth", 80)
	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("switch-rejection-auth", 2.5))
	if !acquired {
		t.Fatal("lease was not acquired")
	}
	if errConfigure := configureUsageState(newPath); errConfigure == nil {
		t.Fatal("unresolved durable prepare allowed a state-path switch")
	}
	assertAdaptiveRuntimeLedger(t, "switch-rejection-auth", 2.5, 0, 2.5)

	release(false)
	assertAdaptiveRuntimeLedger(t, "switch-rejection-auth", 0, 0, 0)
}

func TestAdaptiveRepeatedPathToggleNeverDuplicatesUnresolvedLedger(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%t", commit), func(t *testing.T) {
			restoreUsage := isolateBravoUsageState(t)
			defer restoreUsage()
			resetAdaptiveReserveForTest()
			defer resetAdaptiveReserveForTest()

			directory := t.TempDir()
			pathA := filepath.Join(directory, "a.json")
			pathB := filepath.Join(directory, "b.json")
			if errConfigure := configureUsageState(pathA); errConfigure != nil {
				t.Fatal(errConfigure)
			}
			setAdaptivePersistenceQuota(t, "toggle-auth", 80)
			release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("toggle-auth", 2.5))
			if !acquired {
				t.Fatal("lease was not acquired")
			}
			for index := 0; index < 5; index++ {
				if errConfigure := configureUsageState(pathB); errConfigure == nil {
					t.Fatalf("toggle %d unexpectedly switched A to B", index)
				}
				if errConfigure := configureUsageState(pathA); errConfigure != nil {
					t.Fatalf("toggle %d failed idempotent A reconfigure: %v", index, errConfigure)
				}
				assertAdaptiveRuntimeLedger(t, "toggle-auth", 2.5, 0, 2.5)
			}
			release(commit)
			if commit {
				assertAdaptiveRuntimeLedger(t, "toggle-auth", 0, 2.5, 2.5)
			} else {
				assertAdaptiveRuntimeLedger(t, "toggle-auth", 0, 0, 0)
				if errConfigure := configureUsageState(pathB); errConfigure != nil {
					t.Fatalf("resolved ledger still blocked path switch: %v", errConfigure)
				}
			}
		})
	}
}

func assertAdaptiveRuntimeLedger(t *testing.T, authIndex string, wantInFlight, wantPending, wantDurable float64) {
	t.Helper()
	allocatorRuntime.Lock()
	inFlight := allocatorRuntime.InFlightPercent[authIndex]
	pending := allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	bravoUsageState.mu.RLock()
	durable := 0.0
	if entry := bravoUsageState.state.AdaptiveQuota.Pending[authIndex]; entry != nil {
		durable += entry.Percent
	}
	if entry := bravoUsageState.state.AdaptiveQuota.Prepared[authIndex]; entry != nil {
		durable += entry.Percent
	}
	bravoUsageState.mu.RUnlock()
	if inFlight != wantInFlight || pending != wantPending || durable != wantDurable || inFlight+pending != durable {
		t.Fatalf(
			"ledger = in-flight %.3f pending %.3f durable %.3f, want %.3f/%.3f/%.3f",
			inFlight, pending, durable, wantInFlight, wantPending, wantDurable,
		)
	}
}
