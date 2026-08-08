package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfigureUsageStateDoesNotInvertAllocatorAndStoreLocks(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	resetAdaptiveReserveForTest()

	storeLocked := make(chan struct{})
	releaseStoreHook := make(chan struct{})
	configureUsageStateStoreLockedHook = func() {
		close(storeLocked)
		<-releaseStoreHook
	}

	configured := make(chan error, 1)
	go func() {
		configured <- configureUsageState(filepath.Join(t.TempDir(), "deadlock-state.json"))
	}()

	select {
	case <-storeLocked:
	case <-time.After(2 * time.Second):
		t.Fatal("configure did not reach the store-locked barrier")
	}

	allocationFinished := make(chan struct{})
	go func() {
		// This is the lock order used by acquireAttemptLease: allocator first,
		// then the usage-state read in quotaSnapshot.
		allocatorRuntime.Lock()
		close(releaseStoreHook)
		_ = quotaSnapshot("deadlock-secondary")
		allocatorRuntime.Unlock()
		close(allocationFinished)
	}()

	deadline := time.After(2 * time.Second)
	for configured != nil || allocationFinished != nil {
		select {
		case errConfigure := <-configured:
			if errConfigure != nil {
				t.Fatalf("configure usage state: %v", errConfigure)
			}
			configured = nil
		case <-allocationFinished:
			allocationFinished = nil
		case <-deadline:
			t.Fatal("configure and allocator formed an ABBA lock cycle")
		}
	}

	configureUsageStateStoreLockedHook = nil
	resetAdaptiveReserveForTest()
	restoreUsage()
}
