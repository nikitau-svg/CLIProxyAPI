package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAdaptiveTokenCheckpointIODoesNotHoldUsageStateLock(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	now := time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC)
	key := adaptiveTokenUsageProfileKey("checkpoint-auth", "claude", "claude-sonnet-5", "", "x1")
	bravoUsageState.mu.Lock()
	bravoUsageState.state.Quotas["checkpoint-auth"] = &credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	}
	bravoUsageState.state.AdaptiveTokenUsageProfiles[key] = &persistedAdaptiveTokenUsageProfile{
		AuthIndex: "checkpoint-auth", Provider: "claude", Model: "claude-sonnet-5",
		SampleCount: 8, Samples: 8, CompletionBuckets: make([]float64, len(adaptiveTokenCompletionBuckets)+1),
		UpdatedAt: now,
	}
	bravoUsageState.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	usageSnapshotBeforePersist = func() {
		close(entered)
		<-release
	}
	defer func() { usageSnapshotBeforePersist = nil }()
	flushed := make(chan error, 1)
	go func() { flushed <- bravoUsageState.flush() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint did not reach the blocked I/O phase")
	}

	readDone := make(chan credentialQuotaState, 1)
	go func() { readDone <- quotaSnapshot("checkpoint-auth") }()
	select {
	case quota := <-readDone:
		if quota.Confidence != "confirmed" {
			t.Fatalf("quota read during checkpoint = %#v", quota)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("filesystem checkpoint held the shared usage-state lock")
	}
	close(release)
	if errFlush := <-flushed; errFlush != nil {
		t.Fatal(errFlush)
	}
	usageSnapshotBeforePersist = nil

	loaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if loaded.AdaptiveTokenUsageProfiles[key] == nil || loaded.Quotas["checkpoint-auth"] == nil {
		t.Fatalf("checkpoint lost cloned state: profile=%#v quota=%#v",
			loaded.AdaptiveTokenUsageProfiles[key], loaded.Quotas["checkpoint-auth"])
	}
}

func TestAdaptiveTokenCheckpointHasMaximumDebounce(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	usageSnapshotBeforePersist = func() {
		close(entered)
		<-release
	}
	defer func() { usageSnapshotBeforePersist = nil }()

	bravoUsageState.mu.Lock()
	bravoUsageState.savePendingSince = time.Now().UTC().Add(-usageSaveMaximumDelay)
	bravoUsageState.state.AdaptiveTokenDroppedProfiles++
	bravoUsageState.scheduleSaveLocked()
	bravoUsageState.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("continuous debounce postponed the checkpoint beyond its hard deadline")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	var loaded persistedUsageState
	for {
		var errLoad error
		loaded, errLoad = loadUsageStateFile(path)
		if errLoad == nil && loaded.AdaptiveTokenDroppedProfiles == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maximum-delay checkpoint did not complete: %v %#v", errLoad, loaded)
		}
		time.Sleep(time.Millisecond)
	}
	usageSnapshotBeforePersist = nil
	if loaded.AdaptiveTokenDroppedProfiles != 1 {
		t.Fatalf("maximum-delay checkpoint lost state: %#v", loaded)
	}
}

func TestUsageCheckpointNeverLetsOlderWriterReplaceNewerSnapshot(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenDroppedProfiles = 1
	bravoUsageState.mu.Unlock()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstHook := true
	usageSnapshotBeforePersist = func() {
		if firstHook {
			firstHook = false
			close(entered)
			<-release
		}
	}
	defer func() { usageSnapshotBeforePersist = nil }()
	firstDone := make(chan error, 1)
	go func() { firstDone <- bravoUsageState.flush() }()
	<-entered
	firstSequence := bravoUsageState.snapshotSequence.Load()

	bravoUsageState.mu.Lock()
	bravoUsageState.state.AdaptiveTokenDroppedProfiles = 2
	bravoUsageState.mu.Unlock()
	secondDone := make(chan error, 1)
	go func() { secondDone <- bravoUsageState.flush() }()
	deadline := time.Now().Add(time.Second)
	for bravoUsageState.snapshotSequence.Load() <= firstSequence {
		if time.Now().After(deadline) {
			t.Fatal("newer snapshot was not captured")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if errFirst := <-firstDone; errFirst != nil {
		t.Fatal(errFirst)
	}
	if errSecond := <-secondDone; errSecond != nil {
		t.Fatal(errSecond)
	}
	usageSnapshotBeforePersist = nil
	loaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if loaded.AdaptiveTokenDroppedProfiles != 2 {
		t.Fatalf("older checkpoint replaced newer state: %#v", loaded)
	}
}
