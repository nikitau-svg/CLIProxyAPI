package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConfigureAdmissionGatePreservesFinalizeAfterPublish(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	pathA, pathB := filepath.Join(directory, "a.json"), filepath.Join(directory, "b.json")
	if err := configureUsageState(pathA); err != nil {
		t.Fatal(err)
	}
	writeAdaptiveAdmissionState(t, pathB, "publish-auth", 80, 0)

	continueRestore := make(chan struct{})
	restoreEntered := make(chan struct{})
	configureUsageStateBeforeRuntimeRestoreHook = func() { close(restoreEntered); <-continueRestore }
	defer func() { configureUsageStateBeforeRuntimeRestoreHook = nil }()
	configured := make(chan error, 1)
	go func() { configured <- configureUsageState(pathB) }()
	<-restoreEntered

	started := make(chan struct{})
	var once sync.Once
	acquireAttemptLeaseBeforeAdmissionHook = func() { once.Do(func() { close(started) }) }
	defer func() { acquireAttemptLeaseBeforeAdmissionHook = nil }()
	type leaseResult struct {
		release  func(bool)
		acquired bool
	}
	lease := make(chan leaseResult, 1)
	go func() {
		release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("publish-auth", 1))
		lease <- leaseResult{release: release, acquired: acquired}
	}()
	<-started
	select {
	case result := <-lease:
		t.Fatalf("request crossed publish/restore gate early: acquired=%t", result.acquired)
	default:
	}
	close(continueRestore)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	result := <-lease
	if !result.acquired {
		t.Fatal("request did not acquire after empty destination restore")
	}
	result.release(true)
	assertAdaptiveRuntimeLedger(t, "publish-auth", 0, 1, 1)
}

func TestConfigureAdmissionGateRestoresDestinationPendingBeforeAdmission(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	directory := t.TempDir()
	pathA, pathB := filepath.Join(directory, "a.json"), filepath.Join(directory, "b.json")
	if err := configureUsageState(pathA); err != nil {
		t.Fatal(err)
	}
	writeAdaptiveAdmissionState(t, pathB, "destination-pending", 80, 40)

	continueRestore := make(chan struct{})
	restoreEntered := make(chan struct{})
	configureUsageStateBeforeRuntimeRestoreHook = func() { close(restoreEntered); <-continueRestore }
	defer func() { configureUsageStateBeforeRuntimeRestoreHook = nil }()
	configured := make(chan error, 1)
	go func() { configured <- configureUsageState(pathB) }()
	<-restoreEntered

	started := make(chan struct{})
	var once sync.Once
	acquireAttemptLeaseBeforeAdmissionHook = func() { once.Do(func() { close(started) }) }
	defer func() { acquireAttemptLeaseBeforeAdmissionHook = nil }()
	acquired := make(chan bool, 1)
	go func() {
		_, ok := acquireAttemptLease(adaptivePersistenceAttempt("destination-pending", 1))
		acquired <- ok
	}()
	<-started
	select {
	case ok := <-acquired:
		t.Fatalf("request crossed pending restore gate early: acquired=%t", ok)
	default:
	}
	close(continueRestore)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	if <-acquired {
		t.Fatal("destination pending debt was not applied before admission")
	}
	if got := pendingReservationPercent("destination-pending"); got != 40 {
		t.Fatalf("restored pending = %.3f, want 40", got)
	}
}

func writeAdaptiveAdmissionState(t *testing.T, path, authIndex string, remaining, pending float64) {
	t.Helper()
	state := newPersistedUsageState()
	quota := adaptivePersistenceQuota(remaining, time.Now().UTC())
	state.Quotas[authIndex] = &quota
	if pending > 0 {
		state.AdaptiveQuota.Pending[authIndex] = &persistedAdaptivePendingState{Percent: pending, UpdatedAt: time.Now().UTC()}
	}
	raw, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}
