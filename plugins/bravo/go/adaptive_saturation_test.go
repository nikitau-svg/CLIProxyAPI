package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdaptiveLedgerCapacityFailsNewAuthClosedButExistingAuthContinues(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "capacity.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fillAdaptivePreparedLedgerForTest(adaptiveMaximumPersistedAuthRecords, now)

	if persistAdaptivePrepare("new-auth", 1, now) {
		t.Fatal("new auth acquired durable prepare after the ledger reached its cap")
	}
	if !persistAdaptivePrepare("cap-0000", 0.25, now) {
		t.Fatal("existing auth was blocked even though it did not grow the ledger union")
	}
	persistAdaptiveFinalize("cap-0000", 0.25, false, now)

	bravoUsageState.mu.RLock()
	count := adaptiveLedgerAuthCountLocked()
	_, admitted := bravoUsageState.state.AdaptiveQuota.Prepared["new-auth"]
	bravoUsageState.mu.RUnlock()
	if count != adaptiveMaximumPersistedAuthRecords || admitted {
		t.Fatalf("ledger count/new auth = %d/%t, want %d/false", count, admitted, adaptiveMaximumPersistedAuthRecords)
	}
}

func TestAdaptiveLedgerBelowCapacityDoesNotBlockUnrelatedAuth(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "below-capacity.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fillAdaptivePreparedLedgerForTest(adaptiveMaximumPersistedAuthRecords-1, now)
	if !persistAdaptivePrepare("unrelated-auth", 0.5, now) {
		t.Fatal("unrelated auth was blocked below the union cap")
	}
	persistAdaptiveFinalize("unrelated-auth", 0.5, false, now)
}

func TestAdaptiveOversizedLegacyLedgerPersistsSaturationAcrossRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	path := filepath.Join(directory, "legacy-oversized.json")
	state := newPersistedUsageState()
	now := time.Now().UTC()
	for index := 0; index < adaptiveMaximumPersistedAuthRecords+3; index++ {
		authIndex := fmt.Sprintf("legacy-%04d", index)
		state.AdaptiveQuota.Prepared[authIndex] = &persistedAdaptivePendingState{Percent: 0.5, UpdatedAt: now.Add(time.Duration(index) * time.Second)}
		state.AdaptiveQuota.Revisions[authIndex] = 1
	}
	raw, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	assertAdaptiveSaturationState(t, adaptiveMaximumPersistedAuthRecords+3)
	if _, acquired := acquireAttemptLease(adaptivePersistenceAttempt("legacy-4098", 0.5)); acquired {
		t.Fatal("secondary lease reopened while oversized work was represented by saturation")
	}

	newPrimary := adaptivePersistenceAttempt("new-primary-during-saturation", 0.5)
	newPrimary.Primary = true
	setAdaptivePersistenceQuota(t, newPrimary.Auth.AuthIndex, 80)
	if _, acquired := acquireAttemptLease(newPrimary); acquired {
		t.Fatal("new primary created untracked work while the ledger was saturated")
	}

	primary := adaptivePersistenceAttempt("legacy-4098", 0.5)
	primary.Primary = true
	setAdaptivePersistenceQuota(t, primary.Auth.AuthIndex, 80)
	release, acquired := acquireAttemptLease(primary)
	if !acquired {
		t.Fatal("saturation incorrectly blocked the primary")
	}
	release(false)
	assertAdaptiveRuntimeLedger(t, primary.Auth.AuthIndex, 0, 0.5, 0.5)

	// Save the normalized marker, then emulate a process with no live in-flight
	// map. The restart must remain fail-closed without the omitted auth records.
	bravoUsageState.mu.Lock()
	if errSave := bravoUsageState.saveLocked(); errSave != nil {
		bravoUsageState.mu.Unlock()
		t.Fatal(errSave)
	}
	bravoUsageState.mu.Unlock()
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	assertAdaptiveSaturationState(t, adaptiveMaximumPersistedAuthRecords+3)
	if _, acquired := acquireAttemptLease(adaptivePersistenceAttempt("after-restart", 0.5)); acquired {
		t.Fatal("secondary lease reopened after saturated restart")
	}
}

func TestAdaptiveSaturationClearsOnlyAfterExplicitReconciliation(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "saturation-recovery.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 7
	bravoUsageState.state.AdaptiveQuota.Pending["still-unresolved"] = &persistedAdaptivePendingState{Percent: 2, UpdatedAt: now}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)
	if err := clearAdaptiveRoutingSaturationAfterReconciliation(now); err == nil {
		t.Fatal("saturation cleared before retained unresolved work was reconciled")
	}

	// Represents an operator-confirmed refresh of every retained and overflowed
	// account. The recovery action itself remains fail-closed until this proof.
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Pending = make(map[string]*persistedAdaptivePendingState)
	bravoUsageState.state.AdaptiveQuota.Prepared = make(map[string]*persistedAdaptivePendingState)
	bravoUsageState.mu.Unlock()
	if err := clearAdaptiveRoutingSaturationAfterReconciliation(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if adaptiveRoutingSaturated.Load() {
		t.Fatal("runtime saturation remained active after durable reconciliation")
	}

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	bravoUsageState.mu.RLock()
	saturated := bravoUsageState.state.AdaptiveQuota.Saturated
	overflow := bravoUsageState.state.AdaptiveQuota.OverflowAuthCount
	bravoUsageState.mu.RUnlock()
	if saturated || overflow != 0 || adaptiveRoutingSaturated.Load() {
		t.Fatalf("restart restored saturation/overflow = %t/%d", saturated, overflow)
	}
	setAdaptivePersistenceQuota(t, "recovered-secondary", 80)
	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("recovered-secondary", 0.5))
	if !acquired {
		t.Fatal("secondary remained blocked after durable recovery")
	}
	release(false)
}

func TestAdaptiveReconciliationManagementRequiresConfirmationAndNoLiveDebt(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "management-reconcile.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 4
	bravoUsageState.state.AdaptiveQuota.Pending["redacted-auth"] = &persistedAdaptivePendingState{Percent: 1, UpdatedAt: now}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)

	status, _ := callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":false}`)
	if status != 400 {
		t.Fatalf("unconfirmed reconciliation status = %d, want 400", status)
	}
	status, body := callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 409 || !adaptiveRoutingSaturated.Load() {
		t.Fatalf("live-debt reconciliation = status %d body %#v saturated %t", status, body, adaptiveRoutingSaturated.Load())
	}
	if fmt.Sprint(body) == "" {
		t.Fatal("management reconciliation response is empty")
	}

	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Pending = make(map[string]*persistedAdaptivePendingState)
	bravoUsageState.mu.Unlock()
	status, body = callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
	if status != 200 || adaptiveRoutingSaturated.Load() {
		t.Fatalf("reconciled management state = status %d body %#v saturated %t", status, body, adaptiveRoutingSaturated.Load())
	}
	if strings.Contains(fmt.Sprint(body), "redacted-auth") {
		t.Fatalf("management response leaked auth identity: %#v", body)
	}
}

func TestAdaptiveReconciliationCompactsResolvedRevisionCapBeforeNewPrepare(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "revision-cap-reconcile.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 1
	for index := 0; index < adaptiveMaximumPersistedAuthRecords; index++ {
		bravoUsageState.state.AdaptiveQuota.Revisions[fmt.Sprintf("resolved-%04d", index)] = 1
	}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)

	if err := clearAdaptiveRoutingSaturationAfterReconciliation(now); err != nil {
		t.Fatal(err)
	}
	bravoUsageState.mu.RLock()
	revisions := len(bravoUsageState.state.AdaptiveQuota.Revisions)
	bravoUsageState.mu.RUnlock()
	if revisions != 0 {
		t.Fatalf("reconciliation retained %d resolved revisions, want 0", revisions)
	}

	attempt := adaptivePersistenceAttempt("first-post-recovery-auth", 0.75)
	attempt.Primary = true
	_, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("first post-recovery auth failed before provider I/O")
	}
	// Crash before finalize: the new durable prepare must remain exact rather
	// than becoming a truncated 4097th identity.
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("first-post-recovery-auth"); got != 0.75 {
		t.Fatalf("post-recovery prepare replay = %.3f, want 0.75", got)
	}
	bravoUsageState.mu.RLock()
	saturated := bravoUsageState.state.AdaptiveQuota.Saturated
	bravoUsageState.mu.RUnlock()
	if saturated || adaptiveRoutingSaturated.Load() {
		t.Fatal("first post-recovery prepare re-saturated the ledger")
	}
}

func TestAdaptiveReconciliationDrainsQueuedFinalizeBeforeRevisionReset(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "queued-finalize-reconcile.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if !persistAdaptivePrepare("old-finalize-auth", 0.5, now) {
		t.Fatal("old prepare failed")
	}

	originalAppend := adaptiveWALAppendAndSync
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	adaptiveWALAppendAndSync = func(path string, payload []byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
		return originalAppend(path, payload)
	}
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(unblock) })
		adaptiveWALAppendAndSync = originalAppend
	})
	persistAdaptiveFinalize("old-finalize-auth", 0.5, false, now.Add(time.Millisecond))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("queued finalize did not reach blocked WAL writer")
	}

	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 1
	for index := 0; index < adaptiveMaximumPersistedAuthRecords-1; index++ {
		bravoUsageState.state.AdaptiveQuota.Revisions[fmt.Sprintf("old-resolved-%04d", index)] = 1
	}
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)

	reconciled := make(chan error, 1)
	go func() {
		reconciled <- clearAdaptiveRoutingSaturationAfterReconciliation(now.Add(time.Second))
	}()
	select {
	case err := <-reconciled:
		t.Fatalf("reconciliation crossed blocked queued finalize: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	unblockOnce.Do(func() { close(unblock) })
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	waitAdaptiveWALIdleForTest(t)
	adaptiveWALAppendAndSync = originalAppend

	attempt := adaptivePersistenceAttempt("new-epoch-auth", 0.75)
	attempt.Primary = true
	_, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("new epoch prepare failed before provider I/O")
	}
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("new-epoch-auth"); got != 0.75 {
		t.Fatalf("queued old revision shadowed new prepare: %.3f, want 0.75", got)
	}
}

func TestAdaptiveManagementReconciliationIsOneAdmissionTransaction(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "transactional-reconcile.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveQuota.Saturated = true
	bravoUsageState.state.AdaptiveQuota.OverflowAuthCount = 1
	bravoUsageState.mu.Unlock()
	adaptiveRoutingSaturated.Store(true)
	setAdaptivePersistenceQuota(t, "post-reconcile-auth", 80)

	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	previousHook := reconcileAdaptiveAfterLedgerClearHook
	reconcileAdaptiveAfterLedgerClearHook = func() {
		entered <- struct{}{}
		<-unblock
	}
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(unblock) })
		reconcileAdaptiveAfterLedgerClearHook = previousHook
	})

	type managementResult struct {
		status int
		body   any
	}
	reconciled := make(chan managementResult, 1)
	go func() {
		status, body := callProjectManagement(t, "POST", "/v0/management/bravo/allocator/reconcile", `{"confirmed":true}`)
		reconciled <- managementResult{status: status, body: body}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation did not pause after durable ledger clear")
	}

	type leaseResult struct {
		release  func(bool)
		acquired bool
	}
	leaseDone := make(chan leaseResult, 1)
	go func() {
		attempt := adaptivePersistenceAttempt("post-reconcile-auth", 0.5)
		attempt.Primary = true
		release, acquired := acquireAttemptLease(attempt)
		leaseDone <- leaseResult{release: release, acquired: acquired}
	}()
	select {
	case result := <-leaseDone:
		if result.acquired {
			result.release(false)
		}
		t.Fatal("new admission crossed reconciliation between ledger clear and runtime resets")
	case <-time.After(25 * time.Millisecond):
	}

	unblockOnce.Do(func() { close(unblock) })
	result := <-reconciled
	if result.status != 200 {
		t.Fatalf("transactional reconciliation status=%d body=%v", result.status, result.body)
	}
	lease := <-leaseDone
	if !lease.acquired {
		t.Fatal("post-transaction admission remained blocked")
	}
	lease.release(false)
	reconcileAdaptiveAfterLedgerClearHook = previousHook
}

func fillAdaptivePreparedLedgerForTest(count int, at time.Time) {
	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	for index := 0; index < count; index++ {
		authIndex := fmt.Sprintf("cap-%04d", index)
		bravoUsageState.state.AdaptiveQuota.Prepared[authIndex] = &persistedAdaptivePendingState{Percent: 0.5, UpdatedAt: at}
	}
}

func assertAdaptiveSaturationState(t *testing.T, original int) {
	t.Helper()
	bravoUsageState.mu.RLock()
	state := bravoUsageState.state.AdaptiveQuota
	retained := adaptiveLedgerAuthCountLocked()
	bravoUsageState.mu.RUnlock()
	if !state.Saturated || !adaptiveRoutingSaturated.Load() {
		t.Fatal("oversized ledger did not install persisted and runtime saturation")
	}
	if retained > adaptiveMaximumPersistedAuthRecords {
		t.Fatalf("retained %d auth records above cap %d", retained, adaptiveMaximumPersistedAuthRecords)
	}
	if retained+state.OverflowAuthCount < original {
		t.Fatalf("retained %d + overflow %d forgot unresolved total %d", retained, state.OverflowAuthCount, original)
	}
}
