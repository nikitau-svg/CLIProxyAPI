package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRouteTraceWALReplaysDurableRecordsAndTruncatesTornTail(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(statePath)
	t.Cleanup(func() { _ = store.close() })
	now := time.Now().UTC()
	for index, traceID := range []string{"trc_first", "trc_second"} {
		errAppend := store.appendDurable(routeTrace{
			TraceID:   traceID,
			StartedAt: now.Add(time.Duration(index) * time.Millisecond),
			Status:    503,
			FinalCode: "bravo_route_temporarily_unavailable",
		})
		if errAppend != nil {
			t.Fatalf("append durable trace %d: %v", index, errAppend)
		}
	}
	wal, errOpen := os.OpenFile(routeTraceWALPath(statePath), os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if _, errWrite := wal.Write([]byte("torn-frame")); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := wal.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	reloaded := newRouteTraceStore(statePath)
	t.Cleanup(func() { _ = reloaded.close() })
	traces, errList := reloaded.list(routeTraceQuery{Limit: 10}, now.Add(time.Second))
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 2 || traces[0].TraceID != "trc_second" || traces[1].TraceID != "trc_first" {
		t.Fatalf("replayed traces = %#v", traces)
	}
	if !strings.Contains(reloaded.warning(), "Хвост журнала") {
		t.Fatalf("torn-tail warning = %q", reloaded.warning())
	}
	raw, errRead := os.ReadFile(routeTraceWALPath(statePath))
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(raw), "torn-frame") {
		t.Fatal("torn WAL tail was not truncated before future appends")
	}
}

func TestRouteTraceWALCompactionIsIdempotentAndRetainsBoundedHistory(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(statePath)
	store.maxEntries = 2
	store.compactAfterRecords = 2
	t.Cleanup(func() { _ = store.close() })
	now := time.Now().UTC()
	for index, traceID := range []string{"trc_old", "trc_middle", "trc_new"} {
		if errAppend := store.appendDurable(routeTrace{TraceID: traceID, StartedAt: now.Add(time.Duration(index) * time.Millisecond), Status: 503}); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, errStat := os.Stat(routeTracePath(statePath)); errStat == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("asynchronous WAL compaction did not publish a snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	if errFlush := store.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	reloaded := newRouteTraceStore(statePath)
	t.Cleanup(func() { _ = reloaded.close() })
	traces, errList := reloaded.list(routeTraceQuery{Limit: 10}, now.Add(time.Second))
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 2 || traces[0].TraceID != "trc_new" || traces[1].TraceID != "trc_middle" {
		t.Fatalf("compacted traces = %#v", traces)
	}
	walInfo, errStat := os.Stat(routeTraceWALPath(statePath))
	if errStat != nil {
		t.Fatal(errStat)
	}
	if walInfo.Size() != 0 {
		t.Fatalf("WAL size after orderly compaction = %d, want 0", walInfo.Size())
	}
}

func TestRouteTraceCorruptSnapshotIsReplacedBeforeNewDurableAppend(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	if errWrite := os.WriteFile(routeTracePath(statePath), []byte("{corrupt"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	installGlobalRouteTraceStoreForTest(t, newRouteTraceStore(filepath.Join(t.TempDir(), "initial.json")))
	if errConfigure := configureRouteTraceStore(statePath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	_, warning, errList := listCurrentRouteTraces(routeTraceQuery{Limit: 1}, time.Now().UTC())
	if errList != nil || !strings.Contains(warning, "поврежд") {
		t.Fatalf("recovery warning/error = %q/%v", warning, errList)
	}
	trace := routeTrace{TraceID: "trc_after_recovery", StartedAt: time.Now().UTC(), Status: 503}
	if errAppend := appendCurrentRouteTrace(trace, true); errAppend != nil {
		t.Fatal(errAppend)
	}
	if errConfigure := configureRouteTraceStore(statePath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	traces, _, errList := listCurrentRouteTraces(routeTraceQuery{TraceID: trace.TraceID}, time.Now().UTC())
	if errList != nil || len(traces) != 1 {
		t.Fatalf("trace after corrupt-storage restart = %#v error=%v", traces, errList)
	}
}

func TestRouteTraceCorruptRecoveryEpochSurvivesWALResetFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	if errWrite := os.WriteFile(routeTracePath(statePath), []byte("{corrupt"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	oldFrame, errFrame := marshalRouteTraceWALFrame(routeTraceWALRecord{
		SchemaVersion: routeTraceSchemaVersion,
		Revision:      7,
		Trace:         routeTrace{TraceID: "trc_old_generation", StartedAt: time.Now().UTC()},
	})
	if errFrame != nil {
		t.Fatal(errFrame)
	}
	if errWrite := os.WriteFile(routeTraceWALPath(statePath), oldFrame, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := newRouteTraceStore(statePath)
	t.Cleanup(func() { _ = store.close() })
	if errLoad := store.load(); errLoad == nil {
		t.Fatal("corrupt snapshot unexpectedly loaded")
	}
	store.beforeWALReset = func() error { return errors.New("injected reset failure after snapshot rename") }
	if errRecover := store.recoverAfterLoadFailure(); errRecover == nil {
		t.Fatal("recovery unexpectedly completed despite WAL reset failure")
	}
	store.beforeWALReset = nil
	trace := routeTrace{TraceID: "trc_new_generation", StartedAt: time.Now().UTC(), Status: 503}
	if errAppend := store.appendDurable(trace); errAppend != nil {
		t.Fatal(errAppend)
	}
	reloaded := newRouteTraceStore(statePath)
	t.Cleanup(func() { _ = reloaded.close() })
	traces, errList := reloaded.list(routeTraceQuery{TraceID: trace.TraceID}, time.Now().UTC())
	if errList != nil || len(traces) != 1 {
		t.Fatalf("post-reset-failure trace = %#v error=%v", traces, errList)
	}
	if reloaded.nextRevision <= 7 {
		t.Fatalf("recovery revision = %d, want epoch above old WAL", reloaded.nextRevision)
	}
}

func TestRouteTraceOversizedLegacyFilesAreReadWithHardBounds(t *testing.T) {
	t.Run("wal", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "bravo-state.json")
		frame, errFrame := marshalRouteTraceWALFrame(routeTraceWALRecord{
			SchemaVersion: routeTraceSchemaVersion,
			Revision:      1,
			Trace:         routeTrace{TraceID: "trc_valid_prefix", StartedAt: time.Now().UTC()},
		})
		if errFrame != nil {
			t.Fatal(errFrame)
		}
		walPath := routeTraceWALPath(statePath)
		if errWrite := os.WriteFile(walPath, frame, 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		if errTruncate := os.Truncate(walPath, (16<<20)+1); errTruncate != nil {
			t.Fatal(errTruncate)
		}
		store := newRouteTraceStore(statePath)
		t.Cleanup(func() { _ = store.close() })
		traces, errList := store.list(routeTraceQuery{Limit: 10}, time.Now().UTC())
		if errList != nil || len(traces) != 1 || traces[0].TraceID != "trc_valid_prefix" {
			t.Fatalf("bounded oversized WAL replay = %#v error=%v", traces, errList)
		}
		info, errStat := os.Stat(walPath)
		if errStat != nil {
			t.Fatal(errStat)
		}
		if info.Size() != int64(len(frame)) {
			t.Fatalf("oversized WAL truncated to %d, want valid prefix %d", info.Size(), len(frame))
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "bravo-state.json")
		snapshotPath := routeTracePath(statePath)
		if errWrite := os.WriteFile(snapshotPath, []byte("{"), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		if errTruncate := os.Truncate(snapshotPath, routeTraceSnapshotMaxBytes+1); errTruncate != nil {
			t.Fatal(errTruncate)
		}
		store := newRouteTraceStore(statePath)
		t.Cleanup(func() { _ = store.close() })
		if errLoad := store.load(); errLoad == nil || !strings.Contains(errLoad.Error(), "exceeds") {
			t.Fatalf("oversized snapshot load error = %v", errLoad)
		}
		if errRecover := store.recoverAfterLoadFailure(); errRecover != nil {
			t.Fatal(errRecover)
		}
		info, errStat := os.Stat(snapshotPath)
		if errStat != nil || info.Size() >= routeTraceSnapshotMaxBytes {
			t.Fatalf("recovered snapshot size/info = %v/%v", info, errStat)
		}
	})
}

func TestRouteTraceWALHardLimitStopsGrowthWhenCompactionFails(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(statePath)
	store.compactAfterRecords = 1
	store.maxWALRecords = 4
	store.maxWALBytes = 1 << 20
	store.beforeSnapshot = func() error { return errors.New("injected snapshot failure") }
	t.Cleanup(func() {
		store.beforeSnapshot = nil
		_ = store.close()
	})
	now := time.Now().UTC()
	failures := 0
	for index := 0; index < 20; index++ {
		errAppend := store.appendDurable(routeTrace{TraceID: "trc_limit", StartedAt: now, Status: 503})
		if errAppend != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("persistent compaction failure never stopped durable appends")
	}
	store.mu.Lock()
	walRecords := store.walRecords
	walBytes := store.walBytes
	store.mu.Unlock()
	if walRecords > store.maxWALRecords || walBytes > store.maxWALBytes {
		t.Fatalf("WAL exceeded hard bound: records=%d/%d bytes=%d/%d", walRecords, store.maxWALRecords, walBytes, store.maxWALBytes)
	}
	if !strings.Contains(store.warning(), "остаётся доступна в памяти") {
		t.Fatalf("hard-limit warning = %q", store.warning())
	}
}

func TestRouteTracePersistenceQueueAndMemoryStayBoundedUnderErrorStorm(t *testing.T) {
	store := newRouteTraceStore(filepath.Join(t.TempDir(), "bravo-state.json"))
	store.maxEntries = 32
	store.terminalQueueTimeout = 20 * time.Millisecond
	store.terminalWaitTimeout = 50 * time.Millisecond
	blocked := make(chan struct{})
	store.beforePersist = func() { <-blocked }
	t.Cleanup(func() {
		close(blocked)
		_ = store.close()
	})
	now := time.Now().UTC()
	store.append(routeTrace{TraceID: "trc_block_writer", StartedAt: now})
	for index := 0; index < cap(store.persistQueue)+64; index++ {
		store.append(routeTrace{TraceID: "trc_storm", StartedAt: now.Add(time.Duration(index) * time.Nanosecond)})
	}
	started := time.Now()
	errAppend := store.appendDurable(routeTrace{TraceID: "trc_terminal", StartedAt: now, Status: 503})
	if errAppend == nil {
		t.Fatal("terminal append unexpectedly entered a saturated persistence queue")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("terminal queue backpressure took %s, want bounded wait", elapsed)
	}
	store.mu.Lock()
	traceCount := len(store.traces)
	drops := store.persistenceDrops
	store.mu.Unlock()
	if traceCount != store.maxEntries {
		t.Fatalf("in-memory trace count = %d, want hard bound %d", traceCount, store.maxEntries)
	}
	if drops == 0 || len(store.persistQueue) > cap(store.persistQueue) {
		t.Fatalf("queue state: drops=%d len=%d cap=%d", drops, len(store.persistQueue), cap(store.persistQueue))
	}
}

func TestRouteTraceGlobalStoreConfigureAndReadAreRaceSafe(t *testing.T) {
	initial := newRouteTraceStore(filepath.Join(t.TempDir(), "initial-state.json"))
	installGlobalRouteTraceStoreForTest(t, initial)

	stop := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = appendCurrentRouteTrace(routeTrace{StartedAt: time.Now().UTC(), Success: true}, false)
					_, _, _ = listCurrentRouteTraces(routeTraceQuery{Limit: 1}, time.Now().UTC())
				}
			}
		}()
	}
	root := t.TempDir()
	for index := 0; index < 8; index++ {
		if errConfigure := configureRouteTraceStore(filepath.Join(root, "state-"+string(rune('a'+index))+".json")); errConfigure != nil {
			close(stop)
			workers.Wait()
			t.Fatal(errConfigure)
		}
	}
	close(stop)
	workers.Wait()
}

func TestRouteTraceConfigureFlushesOldStoreBeforeLoadingReplacement(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "shared-state.json")
	initial := newRouteTraceStore(statePath)
	installGlobalRouteTraceStoreForTest(t, initial)
	if errAppend := appendCurrentRouteTrace(routeTrace{TraceID: "trc_before_configure", StartedAt: time.Now().UTC()}, true); errAppend != nil {
		t.Fatal(errAppend)
	}
	enteredSnapshot := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var once sync.Once
	initial.beforeSnapshot = func() error {
		once.Do(func() { close(enteredSnapshot) })
		<-releaseSnapshot
		return nil
	}
	configured := make(chan error, 1)
	go func() { configured <- configureRouteTraceStore(statePath) }()
	<-enteredSnapshot
	appended := make(chan error, 1)
	go func() {
		appended <- appendCurrentRouteTrace(routeTrace{TraceID: "trc_during_configure", StartedAt: time.Now().UTC()}, true)
	}()
	select {
	case errAppend := <-appended:
		if errAppend != nil {
			t.Fatal(errAppend)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("observability reload blocked a request on filesystem I/O")
	}
	close(releaseSnapshot)
	if errConfigure := <-configured; errConfigure != nil {
		t.Fatal(errConfigure)
	}
	traces, _, errList := listCurrentRouteTraces(routeTraceQuery{Limit: 10}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	ids := make(map[string]bool)
	for _, trace := range traces {
		ids[trace.TraceID] = true
	}
	if !ids["trc_before_configure"] || !ids["trc_during_configure"] {
		t.Fatalf("traces across configure barrier = %#v", traces)
	}
}

func TestRouteTraceConfigureCloseFailureInstallsServiceableStore(t *testing.T) {
	initialPath := filepath.Join(t.TempDir(), "initial-state.json")
	initial := newRouteTraceStore(initialPath)
	installGlobalRouteTraceStoreForTest(t, initial)
	if errAppend := appendCurrentRouteTrace(routeTrace{TraceID: "trc_before_close_failure", StartedAt: time.Now().UTC()}, true); errAppend != nil {
		t.Fatal(errAppend)
	}
	initial.beforeSnapshot = func() error { return errors.New("injected close snapshot failure") }
	if errConfigure := configureRouteTraceStore(filepath.Join(t.TempDir(), "replacement-state.json")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if errAppend := appendCurrentRouteTrace(routeTrace{TraceID: "trc_after_close_failure", StartedAt: time.Now().UTC()}, true); errAppend != nil {
		t.Fatal(errAppend)
	}
	traces, warning, errList := listCurrentRouteTraces(routeTraceQuery{Limit: 10}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	ids := make(map[string]bool)
	for _, trace := range traces {
		ids[trace.TraceID] = true
	}
	if !ids["trc_before_close_failure"] || !ids["trc_after_close_failure"] {
		t.Fatalf("serviceable replacement traces = %#v", traces)
	}
	if warning == "" {
		t.Fatal("close failure was not exposed to management")
	}
}

func TestRouteTraceAppendP95RemainsBoundedDuringPersistenceBackpressure(t *testing.T) {
	store := newRouteTraceStore(filepath.Join(t.TempDir(), "bravo-state.json"))
	store.maxEntries = defaultRouteTraceLimit
	blocked := make(chan struct{})
	store.beforePersist = func() { <-blocked }
	t.Cleanup(func() {
		close(blocked)
		_ = store.close()
	})
	now := time.Now().UTC()
	store.append(routeTrace{TraceID: "trc_block_writer", StartedAt: now})
	for index := 0; index < defaultRouteTraceLimit; index++ {
		store.append(routeTrace{TraceID: "trc_prefill", StartedAt: now})
	}
	durations := make([]time.Duration, 1000)
	for index := range durations {
		started := time.Now()
		store.append(routeTrace{TraceID: "trc_hot", StartedAt: now})
		durations[index] = time.Since(started)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95)/100]
	if p95 > 10*time.Millisecond {
		t.Fatalf("append p95 = %s under full history and blocked storage, want <= 10ms", p95)
	}
}

func TestRouteTraceAttemptsAreHardCappedWithSecretFreeSummary(t *testing.T) {
	const secret = "sk-secret-omitted-attempt"
	attempts := make([]routeTraceAttempt, 5000)
	for index := range attempts {
		attempts[index] = routeTraceAttempt{Provider: "claude"}
		if index >= 64 && index < len(attempts)-1 {
			attempts[index].Provider = "claude-" + secret
			attempts[index].SubscriptionLabel = secret
			attempts[index].ErrorMessage = secret
		}
	}
	started := time.Now()
	trace := sanitizeRouteTrace(routeTrace{TraceID: "trc_many_attempts", StartedAt: time.Now().UTC(), Attempts: attempts})
	if elapsed := time.Since(started); elapsed > 25*time.Millisecond {
		t.Fatalf("sanitize large attempt list took %s", elapsed)
	}
	if len(trace.Attempts) != 64 {
		t.Fatalf("persisted attempts = %d, want 64", len(trace.Attempts))
	}
	if trace.AttemptSummary.Total != 5000 || trace.AttemptSummary.Persisted != 64 || trace.AttemptSummary.Omitted != 4936 {
		t.Fatalf("attempt summary = %#v", trace.AttemptSummary)
	}
	frame, errFrame := marshalRouteTraceWALFrame(routeTraceWALRecord{
		SchemaVersion: routeTraceSchemaVersion,
		Revision:      1,
		Trace:         trace,
	})
	if errFrame != nil {
		t.Fatal(errFrame)
	}
	if len(frame) > routeTraceWALMaxPayload+routeTraceWALHeaderSize {
		t.Fatalf("bounded WAL frame size = %d", len(frame))
	}
	if strings.Contains(string(frame), secret) {
		t.Fatal("attempt omission summary or persisted trace leaked secret data")
	}
}

func TestRouteTraceRecorderBoundsAttemptsBeforeFinishAndPreservesOrdinalGap(t *testing.T) {
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "trc_rolling", StartedAt: time.Now().UTC()}}
	for ordinal := 1; ordinal <= 5000; ordinal++ {
		recorder.appendAttempt(routeTraceAttempt{Provider: "claude", Model: "claude-fable-5", Outcome: "failed"})
		if len(recorder.trace.Attempts) > maxPersistedRouteTraceAttempts {
			t.Fatalf("recorder attempts grew to %d", len(recorder.trace.Attempts))
		}
	}
	trace := sanitizeRouteTrace(recorder.trace)
	if len(trace.Attempts) != maxPersistedRouteTraceAttempts {
		t.Fatalf("persisted attempts = %d", len(trace.Attempts))
	}
	if trace.Attempts[0].Ordinal != 1 || trace.Attempts[len(trace.Attempts)-1].Ordinal != 5000 {
		t.Fatalf("retained ordinals = first %d last %d", trace.Attempts[0].Ordinal, trace.Attempts[len(trace.Attempts)-1].Ordinal)
	}
	if trace.AttemptSummary.Total != 5000 || trace.AttemptSummary.Omitted != 4936 {
		t.Fatalf("rolling attempt summary = %#v", trace.AttemptSummary)
	}
}

func BenchmarkRouteTraceAppendHotPath(b *testing.B) {
	store := newRouteTraceStore(filepath.Join(b.TempDir(), "bravo-state.json"))
	blocked := make(chan struct{})
	store.beforePersist = func() { <-blocked }
	store.append(routeTrace{TraceID: "trc_block_writer", StartedAt: time.Now().UTC()})
	b.Cleanup(func() {
		close(blocked)
		_ = store.close()
	})
	trace := routeTrace{TraceID: "trc_bench", StartedAt: time.Now().UTC(), Success: true}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		store.append(trace)
	}
}

func installGlobalRouteTraceStoreForTest(t *testing.T, store *routeTraceStore) {
	t.Helper()
	bravoRouteTraceStores.Lock()
	original := bravoRouteTraceStores.store
	bravoRouteTraceStores.store = store
	bravoRouteTraceStores.Unlock()
	t.Cleanup(func() {
		bravoRouteTraceStores.Lock()
		current := bravoRouteTraceStores.store
		bravoRouteTraceStores.store = original
		bravoRouteTraceStores.Unlock()
		if current != nil && current != original {
			_ = current.close()
		}
	})
}
