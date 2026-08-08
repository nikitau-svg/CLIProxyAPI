package main

import (
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdaptiveFinalizeReturnsBeforeSyncAndNextPrepareWaitsInOrder(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()

	path := filepath.Join(t.TempDir(), "ordered-async-finalize.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	setAdaptivePersistenceQuota(t, "ordered-auth", 90)

	previousAppend := adaptiveWALAppendAndSync
	var calls atomic.Int64
	finalizeSyncEntered := make(chan struct{})
	unblockFinalizeSync := make(chan struct{})
	adaptiveWALAppendAndSync = func(string, []byte) error {
		call := calls.Add(1)
		if call == 2 {
			close(finalizeSyncEntered)
			<-unblockFinalizeSync
		}
		return nil
	}
	t.Cleanup(func() { adaptiveWALAppendAndSync = previousAppend })

	release, acquired := acquireAttemptLease(adaptivePersistenceAttempt("ordered-auth", 1))
	if !acquired {
		t.Fatal("first prepare was not acquired")
	}
	finalized := make(chan struct{})
	go func() {
		release(true)
		close(finalized)
	}()
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("finalize waited for WAL fsync")
	}
	select {
	case <-finalizeSyncEntered:
	case <-time.After(time.Second):
		t.Fatal("async finalize was not handed to the ordered writer")
	}

	secondResult := make(chan bool, 1)
	go func() {
		_, ok := acquireAttemptLease(adaptivePersistenceAttempt("ordered-auth", 1))
		secondResult <- ok
	}()
	for {
		bravoUsageState.mu.RLock()
		prepared := bravoUsageState.state.AdaptiveQuota.Prepared["ordered-auth"]
		staged := prepared != nil && prepared.Percent >= 1
		bravoUsageState.mu.RUnlock()
		if staged {
			break
		}
		runtime.Gosched()
	}
	select {
	case ok := <-secondResult:
		t.Fatalf("next prepare crossed an unflushed finalize: acquired=%t", ok)
	default:
	}
	close(unblockFinalizeSync)
	select {
	case ok := <-secondResult:
		if !ok {
			t.Fatal("next prepare failed after ordered finalize sync")
		}
	case <-time.After(time.Second):
		t.Fatal("next prepare did not resume after ordered finalize sync")
	}
}

func TestAdaptiveAsyncFinalizeQueueIsBoundedWithoutWaiterGoroutines(t *testing.T) {
	writer := &adaptiveWALCommitter{}
	previousAppend := adaptiveWALAppendAndSync
	writeEntered := make(chan struct{})
	unblockWrite := make(chan struct{})
	var once atomic.Bool
	adaptiveWALAppendAndSync = func(string, []byte) error {
		if once.CompareAndSwap(false, true) {
			close(writeEntered)
			<-unblockWrite
		}
		return nil
	}
	t.Cleanup(func() { adaptiveWALAppendAndSync = previousAppend })

	startGoroutines := runtime.NumGoroutine()
	first := adaptiveWALRecord{Version: adaptiveWALVersion, AuthIndex: "flood-0000", Revision: 1, RecordedAt: time.Now().UTC()}
	if err := writer.appendAsync(filepath.Join(t.TempDir(), "flood.wal"), first); err != nil {
		t.Fatal(err)
	}
	<-writeEntered
	accepted := 1
	for index := 1; index < adaptiveWALMaxPending*2; index++ {
		record := adaptiveWALRecord{Version: adaptiveWALVersion, AuthIndex: "flood-auth", Revision: uint64(index), RecordedAt: time.Now().UTC()}
		if err := writer.appendAsync("/private/tmp/bravo-adaptive-flood.wal", record); err == nil {
			accepted++
		}
	}
	writer.mu.Lock()
	pending := len(writer.pending)
	for _, request := range writer.pending {
		if request.wait || request.done != nil {
			writer.mu.Unlock()
			t.Fatal("async finalize allocated a waiter channel")
		}
	}
	writer.mu.Unlock()
	if pending > adaptiveWALMaxPending || accepted > adaptiveWALMaxPending+adaptiveWALMaxBatch {
		t.Fatalf("bounded queue pending/accepted = %d/%d", pending, accepted)
	}
	if delta := runtime.NumGoroutine() - startGoroutines; delta > 4 {
		t.Fatalf("async finalize flood spawned %d goroutines", delta)
	}
	close(unblockWrite)
	deadline := time.Now().Add(2 * time.Second)
	for {
		writer.mu.Lock()
		flushing := writer.flushing
		writer.mu.Unlock()
		if !flushing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bounded writer did not drain")
		}
		runtime.Gosched()
	}
}

func BenchmarkAdaptiveWALSteadyPrepareFinalize(b *testing.B) {
	restoreUsage := isolateBravoUsageState(b)
	defer restoreUsage()
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	path := filepath.Join(b.TempDir(), "steady.json")
	if err := configureUsageState(path); err != nil {
		b.Fatal(err)
	}
	setAdaptivePersistenceQuota(b, "benchmark-auth", 100)
	attempt := adaptivePersistenceAttempt("benchmark-auth", 0.001)
	b.ReportAllocs()
	durations := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		release, acquired := acquireAttemptLease(attempt)
		if !acquired {
			b.Fatal("prepare rejected")
		}
		release(false)
		durations = append(durations, time.Since(started))
	}
	b.StopTimer()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if len(durations) > 0 {
		p95 := durations[(len(durations)-1)*95/100]
		b.ReportMetric(float64(p95.Nanoseconds()), "p95-ns")
	}
}
