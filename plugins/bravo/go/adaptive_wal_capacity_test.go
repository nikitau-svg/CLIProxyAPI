package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdaptiveWALHardCapCheckpointsAndKeepsDebtRestartSafe(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "hard-cap.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	installAdaptiveWALLimitsForTest(t, 4096, 4)

	const requests = 20
	for index := 0; index < requests; index++ {
		attempt := adaptivePersistenceAttempt("hard-cap-auth", 0.1)
		attempt.Primary = true
		release, acquired := acquireAttemptLease(attempt)
		if !acquired {
			t.Fatalf("request %d failed instead of checkpointing at WAL cap", index)
		}
		release(true)
	}
	waitAdaptiveWALIdleForTest(t)
	stopAdaptiveCheckpointTimerForTest()
	assertAdaptiveWALWithinTestLimit(t, adaptiveWALPath(path), 4096, 4)

	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("hard-cap-auth"); math.Abs(got-requests*0.1) > 0.000001 {
		t.Fatalf("restart debt = %.6f, want %.6f", got, requests*0.1)
	}
}

func TestAdaptiveOversizedWALIsCheckpointedBeforeStartupAdmission(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "oversized-startup.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		persistAdaptivePendingCommit("oversized-startup-auth", 1, time.Now())
	}
	stopAdaptiveCheckpointTimerForTest()
	installAdaptiveWALLimitsForTest(t, 4096, 2)
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if _, errStat := os.Stat(adaptiveWALPath(path)); !os.IsNotExist(errStat) {
		t.Fatalf("oversized startup WAL was not compacted: %v", errStat)
	}
	if got := pendingReservationPercent("oversized-startup-auth"); got != 3 {
		t.Fatalf("oversized startup checkpoint debt = %.3f, want 3", got)
	}
}

func TestAdaptiveOversizedCandidateCheckpointFailureDoesNotPublishNewPath(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	root := t.TempDir()
	pathA := filepath.Join(root, "active", "state.json")
	pathB := filepath.Join(root, "candidate", "state.json")
	if err := configureUsageState(pathA); err != nil {
		t.Fatal(err)
	}
	setAdaptivePersistenceQuota(t, "active-auth", 80)
	generationA := bravoUsageState.generation.Load()

	if errMkdir := os.MkdirAll(filepath.Dir(pathB), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	stateB, errMarshal := json.Marshal(newPersistedUsageState())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(pathB, stateB, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	walPayload := make([]byte, 0, 2048)
	for revision := uint64(1); revision <= 3; revision++ {
		saturated := false
		line, errRecord := marshalAdaptiveWALRecord(adaptiveWALRecord{
			Version: adaptiveWALVersion, AuthIndex: "candidate-auth", Revision: revision,
			Pending:   &persistedAdaptivePendingState{Percent: float64(revision), UpdatedAt: time.Now().UTC()},
			Saturated: &saturated, RecordedAt: time.Now().UTC(),
		})
		if errRecord != nil {
			t.Fatal(errRecord)
		}
		walPayload = append(walPayload, line...)
	}
	if errWrite := os.WriteFile(adaptiveWALPath(pathB), walPayload, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	installAdaptiveWALLimitsForTest(t, 4096, 2)

	previousSyncDirectory := adaptiveSyncDirectory
	adaptiveSyncDirectory = func(path string) error {
		if path == filepath.Dir(pathB) {
			return errors.New("injected candidate checkpoint sync failure")
		}
		return previousSyncDirectory(path)
	}
	errConfigure := configureUsageState(pathB)
	adaptiveSyncDirectory = previousSyncDirectory
	if errConfigure == nil {
		t.Fatal("oversized candidate checkpoint failure unexpectedly published path B")
	}
	bravoUsageState.mu.RLock()
	activePath := bravoUsageState.path
	bravoUsageState.mu.RUnlock()
	if activePath != pathA || bravoUsageState.generation.Load() != generationA {
		t.Fatalf("failed switch published path/generation = %q/%d, want %q/%d", activePath, bravoUsageState.generation.Load(), pathA, generationA)
	}
	if got := pendingReservationPercent("candidate-auth"); got != 0 {
		t.Fatalf("failed candidate restored %.3f runtime debt into active A", got)
	}
	if got := quotaSnapshot("active-auth").Session.RemainingPercent; got != 80 {
		t.Fatalf("failed candidate replaced active A quota with %.3f", got)
	}

	attempt := adaptivePersistenceAttempt("active-auth", 1)
	release, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("active A could not finalize after failed B publication")
	}
	release(true)
	waitAdaptiveWALIdleForTest(t)
	stopAdaptiveCheckpointTimerForTest()
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, pathA)
	if got := pendingReservationPercent("active-auth"); got != 1 {
		t.Fatalf("active A finalize after failed switch restarted with %.3f debt, want 1", got)
	}
}

func TestAdaptiveCheckpointMaximumDelayCannotBePostponedByContinuousTraffic(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "max-delay.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	persistAdaptivePendingCommit("max-delay-auth", 1, time.Now())
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent["max-delay-auth"] = 1
	allocatorRuntime.Unlock()

	// Advance the first-dirty watermark close to the hard deadline, then keep
	// producing debounce activity beyond it. The checkpoint must still fire.
	bravoUsageState.mu.Lock()
	bravoUsageState.savePendingSince = time.Now().Add(-usageSaveMaximumDelay + 20*time.Millisecond)
	bravoUsageState.scheduleSaveLocked()
	bravoUsageState.mu.Unlock()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		bravoUsageState.mu.Lock()
		bravoUsageState.scheduleSaveLocked()
		bravoUsageState.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("maximum-delay checkpoint was postponed indefinitely: %v", errStat)
	}
	stopAdaptiveCheckpointTimerForTest()
	resetAdaptiveReserveForTest()
	simulateFreshBravoProcess(t, path)
	if got := pendingReservationPercent("max-delay-auth"); got != 1 {
		t.Fatalf("maximum-delay checkpoint restart debt = %.3f, want 1", got)
	}
}

func TestAdaptiveWALCapCompactionFailureFailsPrepareClosedAndStaysBounded(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(t.TempDir(), "cap-failure.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	installAdaptiveWALLimitsForTest(t, 4096, 1)
	persistAdaptivePendingCommit("cap-fill-auth", 1, time.Now())
	stopAdaptiveCheckpointTimerForTest()

	previousSyncDirectory := adaptiveSyncDirectory
	adaptiveSyncDirectory = func(string) error { return errors.New("injected checkpoint directory sync failure") }
	t.Cleanup(func() { adaptiveSyncDirectory = previousSyncDirectory })
	attempt := adaptivePersistenceAttempt("cap-new-auth", 0.5)
	attempt.Primary = true
	_, acquired, failure, _ := acquireExecutionAttemptLeaseDetailed(attempt)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_durability_unavailable" {
		t.Fatalf("cap compaction failure admission=%t failure=%#v", acquired, failure)
	}
	stopAdaptiveCheckpointTimerForTest()
	assertAdaptiveWALWithinTestLimit(t, adaptiveWALPath(path), 4096, 1)
}

func installAdaptiveWALLimitsForTest(t *testing.T, maxBytes, maxRecords int64) {
	t.Helper()
	waitAdaptiveWALIdleForTest(t)
	adaptiveWALRuntime.ioMu.Lock()
	previousBytes, previousRecords := adaptiveWALRuntime.maxBytes, adaptiveWALRuntime.maxRecords
	previousDisk := adaptiveWALRuntime.disk
	adaptiveWALRuntime.maxBytes = maxBytes
	adaptiveWALRuntime.maxRecords = maxRecords
	adaptiveWALRuntime.disk = make(map[string]adaptiveWALDiskUsage)
	adaptiveWALRuntime.ioMu.Unlock()
	t.Cleanup(func() {
		waitAdaptiveWALIdleForTest(t)
		adaptiveWALRuntime.ioMu.Lock()
		adaptiveWALRuntime.maxBytes = previousBytes
		adaptiveWALRuntime.maxRecords = previousRecords
		adaptiveWALRuntime.disk = previousDisk
		adaptiveWALRuntime.ioMu.Unlock()
	})
}

func stopAdaptiveCheckpointTimerForTest() {
	bravoUsageState.mu.Lock()
	if bravoUsageState.saveTimer != nil {
		bravoUsageState.saveTimer.Stop()
		bravoUsageState.saveTimer = nil
	}
	bravoUsageState.mu.Unlock()
}

func assertAdaptiveWALWithinTestLimit(t *testing.T, path string, maxBytes, maxRecords int64) {
	t.Helper()
	info, errStat := os.Stat(path)
	if os.IsNotExist(errStat) {
		return
	}
	if errStat != nil {
		t.Fatal(errStat)
	}
	if info.Size() > maxBytes {
		t.Fatalf("WAL bytes = %d, hard limit %d", info.Size(), maxBytes)
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() { _ = file.Close() }()
	records := int64(0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		records++
	}
	if errScan := scanner.Err(); errScan != nil {
		t.Fatal(errScan)
	}
	if records > maxRecords {
		t.Fatalf("WAL records = %d, hard limit %d", records, maxRecords)
	}
}
