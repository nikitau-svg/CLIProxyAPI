package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAdaptiveWALCommitDoesNotSerializeFullUsageSnapshot(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	// Make the in-memory analytics snapshot deliberately much larger than one
	// adaptive record. The commit path must still touch only the sidecar WAL.
	bravoUsageState.mu.Lock()
	for index := 0; index < 2000; index++ {
		bravoUsageState.state.ProjectTotals[time.Unix(int64(index), 0).String()] = &usageAggregate{
			Total:  usageCounters{TotalTokens: int64(index + 1)},
			Hourly: map[string]usageCounters{"large-analytics-bucket": {TotalTokens: int64(index + 1)}},
			Daily:  map[string]usageCounters{"2026-08-08": {TotalTokens: int64(index + 1)}},
		}
	}
	bravoUsageState.mu.Unlock()

	beforeFlushes := adaptiveWALRuntime.flushes.Load()
	persistAdaptivePendingCommit("wal-shape-auth", 0.75, time.Now())
	if got := adaptiveWALRuntime.flushes.Load() - beforeFlushes; got != 1 {
		t.Fatalf("WAL flushes = %d, want 1", got)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("accepted commit unexpectedly wrote full state snapshot: %v", errStat)
	}
	info, errStat := os.Stat(adaptiveWALPath(path))
	if errStat != nil {
		t.Fatal(errStat)
	}
	if info.Size() <= 0 || info.Size() >= 16*1024 {
		t.Fatalf("minimal WAL size = %d bytes, want 1..16383", info.Size())
	}
}

func TestAdaptiveWALConcurrentCommitsCoalesceFlushes(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}

	const workers = adaptiveWALMaxBatch + 32
	start := make(chan struct{})
	beforeFlushes := adaptiveWALRuntime.flushes.Load()
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer group.Done()
			<-start
			persistAdaptivePendingCommit("group-auth-"+time.Unix(int64(index), 0).Format("150405"), 0.1, time.Now())
		}(index)
	}
	close(start)
	group.Wait()

	if got := adaptiveWALRuntime.flushes.Load() - beforeFlushes; got <= 0 || got >= workers {
		t.Fatalf("concurrent commits used %d fsync groups, want 1..%d", got, workers-1)
	}
	if got := adaptiveWALRuntime.maxBatch.Load(); got <= 1 || got > adaptiveWALMaxBatch {
		t.Fatalf("maximum WAL batch = %d, want 2..%d", got, adaptiveWALMaxBatch)
	}
}

func TestAdaptiveWALReplayIsIdempotentAndIgnoresTornTail(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	persistAdaptivePendingCommit("torn-tail-auth", 1.25, time.Now())
	validInfo, errStat := os.Stat(adaptiveWALPath(path))
	if errStat != nil {
		t.Fatal(errStat)
	}
	file, errOpen := os.OpenFile(adaptiveWALPath(path), os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if _, errWrite := file.WriteString(`{"record":{"version":1,"auth_index":"torn`); errWrite != nil {
		_ = file.Close()
		t.Fatal(errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	state := newPersistedUsageState()
	if errReplay := replayAdaptiveWALFile(adaptiveWALPath(path), &state); errReplay != nil {
		t.Fatal(errReplay)
	}
	if errReplay := replayAdaptiveWALFile(adaptiveWALPath(path), &state); errReplay != nil {
		t.Fatal(errReplay)
	}
	if got := state.AdaptiveQuota.Pending["torn-tail-auth"]; got == nil || got.Percent != 1.25 {
		t.Fatalf("idempotent replay pending = %#v, want 1.25", got)
	}
	if got := state.AdaptiveQuota.Revisions["torn-tail-auth"]; got != 1 {
		t.Fatalf("idempotent replay revision = %d, want 1", got)
	}
	healedInfo, errStat := os.Stat(adaptiveWALPath(path))
	if errStat != nil {
		t.Fatal(errStat)
	}
	if healedInfo.Size() != validInfo.Size() {
		t.Fatalf("healed WAL size = %d, want valid prefix %d", healedInfo.Size(), validInfo.Size())
	}

	// A new absolute record appended after healing must remain reachable on the
	// next restart. Before the fix it landed behind the old corrupt tail and the
	// scanner stopped before seeing it.
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if !persistAdaptivePrepare("post-heal-auth", 2, time.Now()) {
		t.Fatal("post-heal durable prepare failed")
	}
	persistAdaptiveFinalize("post-heal-auth", 2, true, time.Now())
	waitAdaptiveWALIdleForTest(t)
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("torn-tail-auth"); got != 1.25 {
		t.Fatalf("valid prefix debt after second restart = %.3f, want 1.25", got)
	}
	if got := pendingReservationPercent("post-heal-auth"); got != 2 {
		t.Fatalf("post-heal debt after second restart = %.3f, want 2", got)
	}
}

func TestAdaptiveWALCompactionAndConfirmedClearSurviveRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	persistAdaptivePendingCommit("compact-auth", 4, time.Now())
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent["compact-auth"] = 4
	allocatorRuntime.Unlock()
	flushUsageState()
	if _, errStat := os.Stat(adaptiveWALPath(path)); !os.IsNotExist(errStat) {
		t.Fatalf("WAL survived full-state compaction: %v", errStat)
	}

	clearPendingReservation("compact-auth", 4)
	if _, errStat := os.Stat(adaptiveWALPath(path)); errStat != nil {
		t.Fatalf("confirmed clear did not append its absolute WAL record: %v", errStat)
	}
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("compact-auth"); got != 0 {
		t.Fatalf("confirmed clear replay restored %.3f pending, want 0", got)
	}
}

func TestAdaptiveSnapshotSyncsDirectoryBeforeWALRemoval(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	persistAdaptivePendingCommit("ordering-auth", 1, time.Now())

	previousSync := adaptiveSyncDirectory
	walExistsAtSync := make([]bool, 0, 2)
	adaptiveSyncDirectory = func(string) error {
		_, errStat := os.Stat(adaptiveWALPath(path))
		walExistsAtSync = append(walExistsAtSync, errStat == nil)
		return nil
	}
	t.Cleanup(func() { adaptiveSyncDirectory = previousSync })
	flushUsageState()
	if len(walExistsAtSync) < 2 || !walExistsAtSync[0] || walExistsAtSync[len(walExistsAtSync)-1] {
		t.Fatalf("directory sync ordering = %v, want snapshot sync with WAL present then removal sync", walExistsAtSync)
	}
}
