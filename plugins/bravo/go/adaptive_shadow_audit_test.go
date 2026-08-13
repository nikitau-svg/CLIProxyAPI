package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveShadowAuditRecorderCapturesOnlyPrivacySafeActualAttempts(t *testing.T) {
	temp := t.TempDir()
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(temp, "state.json"), 16)

	previousTraces := bravoRouteTraces
	bravoRouteTraces = newRouteTraceStore(filepath.Join(temp, "trace-state.json"))
	t.Cleanup(func() {
		_ = bravoRouteTraces.flush()
		bravoRouteTraces = previousTraces
	})

	started := time.Now().Add(-25 * time.Millisecond)
	recorder := &routeTraceRecorder{trace: routeTrace{
		TraceID:      "trace-safe-1",
		StartedAt:    started,
		ProjectID:    "project-secret-must-not-enter-audit",
		LogicalModel: "bravo/fable",
	}}
	attempt := executionAttempt{
		Candidate:                    candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth:                         pluginapi.HostAuthFileEntry{AuthIndex: "auth-secret-must-not-enter-audit"},
		ProjectID:                    "project-secret-must-not-enter-audit",
		Primary:                      false,
		AdaptiveShadow:               true,
		AdaptiveReservationPercent:   2.5,
		AdaptiveEstimateConfidence:   "shape_estimate",
		AdaptiveShadowDecision:       adaptiveShadowDecisionWithhold,
		AdaptiveShadowPendingPercent: 4,
		AdaptiveShadowHeadroomBefore: 2,
		AdaptiveShadowHeadroomAfter:  -0.5,
		AdaptiveProviderDispatched:   true,
		AdaptiveProviderAccepted:     true,
	}
	recorder.success(attempt, started, http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()

	report := store.report(defaultPluginConfig(), 24*time.Hour, 10, time.Now().Add(time.Second))
	if report.RequestsObserved != 1 || report.ActualExecutionAttempts != 1 ||
		report.SuccessfulWouldWithhold != 1 || report.AdditionalProviderRequests != 0 ||
		report.RoutingChangesApplied != 0 {
		t.Fatalf("unexpected audit report: %#v", report)
	}
	if len(report.Recent) != 1 || len(report.Recent[0].Attempts) != 1 ||
		report.Recent[0].Attempts[0].ProviderAcceptance != "confirmed" {
		t.Fatalf("unexpected recent audit: %#v", report.Recent)
	}
	raw, errMarshal := json.Marshal(report.Recent)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{
		"auth-secret-must-not-enter-audit",
		"project-secret-must-not-enter-audit",
		"auth_index",
		"project_id",
		"prompt",
		"request_body",
		"api_key",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("shadow audit exposed forbidden value %q: %s", forbidden, raw)
		}
	}
}

func TestAdaptiveShadowAuditQueueIsNonBlockingAndBounded(t *testing.T) {
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 1)
	previous := swapAdaptiveShadowAuditStoreForTest(store)
	t.Cleanup(func() { swapAdaptiveShadowAuditStoreForTest(previous) })
	record := adaptiveShadowAuditTestRecord(time.Now(), adaptiveShadowDecisionAdmit, true, "")

	enqueueAdaptiveShadowAudit(record)
	enqueueAdaptiveShadowAudit(record)
	if len(store.queue) != 1 || store.dropped.Load() != 1 {
		t.Fatalf("queue=%d dropped=%d, want 1/1", len(store.queue), store.dropped.Load())
	}
}

func TestAdaptiveShadowAuditCountsOnlyRealInferenceFallbackAttempts(t *testing.T) {
	temp := t.TempDir()
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(temp, "state.json"), 8)
	previousTraces := bravoRouteTraces
	bravoRouteTraces = newRouteTraceStore(filepath.Join(temp, "trace-state.json"))
	t.Cleanup(func() {
		_ = bravoRouteTraces.flush()
		bravoRouteTraces = previousTraces
	})

	recorder := &routeTraceRecorder{trace: routeTrace{
		TraceID:   "trace-fallback",
		StartedAt: time.Now().Add(-time.Second),
	}}
	first := executionAttempt{
		Candidate:                    candidate{Provider: "claude", Model: "claude-fable-5"},
		AdaptiveShadow:               true,
		AdaptiveShadowDecision:       adaptiveShadowDecisionAdmit,
		AdaptiveReservationPercent:   2,
		AdaptiveProviderDispatched:   true,
		AdaptiveShadowHeadroomBefore: 30,
		AdaptiveShadowHeadroomAfter:  28,
	}
	second := first
	second.Candidate = candidate{Provider: "codex", Model: "gpt-5.6"}
	second.AdaptiveShadowDecision = adaptiveShadowDecisionWithhold
	second.AdaptiveProviderAccepted = true
	failure := executionFailure{Code: "bravo_subscription_quota_exhausted", Status: http.StatusTooManyRequests}
	recorder.failure(first, time.Now().Add(-time.Second), failure.Status, failure)
	recorder.success(second, time.Now().Add(-500*time.Millisecond), http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()

	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.RequestsObserved != 1 || report.ActualExecutionAttempts != 2 ||
		report.RequestsWithFallback != 1 || report.QuotaFailuresWouldAdmit != 1 ||
		report.SuccessfulWouldWithhold != 1 {
		t.Fatalf("fallback audit=%#v", report)
	}

	countRecorder := &routeTraceRecorder{}
	countRecorder.disableAdaptiveAudit()
	countRecorder.success(second, time.Now(), http.StatusOK)
	if len(countRecorder.adaptiveAuditAttempts) != 0 || countRecorder.adaptiveAuditExecutionAttempts != 0 {
		t.Fatalf("count-token recorder contaminated inference audit: %#v", countRecorder)
	}
}

func TestAdaptiveShadowAuditCapturesSettledStreamingFailureOnce(t *testing.T) {
	recorder := &routeTraceRecorder{}
	released := 0
	run := &bravoStreamAttemptRun{
		attempt: executionAttempt{
			Candidate:                  candidate{Provider: "claude", Model: "claude-fable-5"},
			AdaptiveShadow:             true,
			AdaptiveShadowDecision:     adaptiveShadowDecisionAdmit,
			AdaptiveProviderDispatched: true,
		},
		started:       time.Now().Add(-time.Millisecond),
		releaseLease:  func(bool) { released++ },
		traceRecorder: recorder,
	}
	failure := executionFailure{Code: "request_canceled", Status: 499}
	results := make(chan bravoStreamBootstrapResult, 1)
	results <- bravoStreamBootstrapResult{failure: &failure, accepted: false}

	settleBravoCompetingAttempt(run, results, nil)
	if released != 1 || len(recorder.adaptiveAuditAttempts) != 1 ||
		recorder.adaptiveAuditExecutionAttempts != 1 {
		t.Fatalf("release=%d audit=%#v", released, recorder)
	}
	if got := recorder.adaptiveAuditAttempts[0].ProviderAcceptance; got != "unknown" {
		t.Fatalf("provider acceptance=%q, want conservative unknown", got)
	}
}

func TestAdaptiveShadowAuditRotatesWithinHardDiskAndMemoryBounds(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newAdaptiveShadowAuditStore(statePath, 256)
	store.fileLimit = 4096
	go store.run()
	for index := 0; index < adaptiveShadowAuditMemoryRecords+150; index++ {
		record := adaptiveShadowAuditTestRecord(
			time.Now().Add(time.Duration(index)*time.Millisecond),
			adaptiveShadowDecisionAdmit,
			true,
			"",
		)
		store.queue <- sanitizeAdaptiveShadowAuditRecord(record)
	}
	store.close()

	store.mu.RLock()
	memoryRecords := len(store.records)
	store.mu.RUnlock()
	if memoryRecords != adaptiveShadowAuditMemoryRecords {
		t.Fatalf("memory records=%d, want hard cap %d", memoryRecords, adaptiveShadowAuditMemoryRecords)
	}
	total := int64(0)
	for _, path := range []string{store.path, store.rotatedPath} {
		info, errStat := os.Stat(path)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil {
			t.Fatal(errStat)
		}
		if info.Size() > store.fileLimit {
			t.Fatalf("%s size=%d exceeds %d", path, info.Size(), store.fileLimit)
		}
		total += info.Size()
	}
	if total > 2*store.fileLimit {
		t.Fatalf("total audit bytes=%d exceed %d", total, 2*store.fileLimit)
	}

	reloaded := newAdaptiveShadowAuditStore(statePath, 1)
	reloaded.fileLimit = store.fileLimit
	reloaded.loadBoundedHistory()
	report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 100, time.Now().Add(time.Hour))
	if report.RequestsObserved == 0 || report.DiskBytes > report.DiskLimitBytes {
		t.Fatalf("reloaded report=%#v", report)
	}
}

func TestAdaptiveShadowAuditVerdictRequiresCleanVolumeAndCoverage(t *testing.T) {
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 4)
	for index := 0; index < adaptiveShadowAuditReviewRequests; index++ {
		at := now.Add(-adaptiveShadowAuditReviewCoverage).Add(
			time.Duration(index) * adaptiveShadowAuditReviewCoverage / time.Duration(adaptiveShadowAuditReviewRequests-1),
		)
		store.appendMemory(adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionAdmit, true, ""))
	}
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict != "ready_for_review" || report.CoverageSeconds < int64(adaptiveShadowAuditReviewCoverage/time.Second) {
		t.Fatalf("clean report verdict=%#v", report)
	}

	store.appendMemory(adaptiveShadowAuditTestRecord(now, adaptiveShadowDecisionWithhold, true, ""))
	report = store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict != "needs_review" || report.SuccessfulWouldWithhold != 1 {
		t.Fatalf("mismatch report verdict=%#v", report)
	}

	store.dropped.Add(1)
	report = store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict != "telemetry_degraded" || report.Status != "warning" {
		t.Fatalf("degraded report verdict=%#v", report)
	}

	unknownStore := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 4)
	for index := 0; index < adaptiveShadowAuditReviewRequests; index++ {
		at := now.Add(-adaptiveShadowAuditReviewCoverage).Add(
			time.Duration(index) * adaptiveShadowAuditReviewCoverage / time.Duration(adaptiveShadowAuditReviewRequests-1),
		)
		unknownStore.appendMemory(adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionUnknown, true, ""))
	}
	unknownReport := unknownStore.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if unknownReport.Verdict == "ready_for_review" || unknownReport.UnknownDecisionAttempts == 0 {
		t.Fatalf("unknown decisions incorrectly became review-ready: %#v", unknownReport)
	}
}

func TestAdaptiveShadowAuditDiskFailureDoesNotBlockOrFailRoutingTelemetry(t *testing.T) {
	temp := t.TempDir()
	parentFile := filepath.Join(temp, "not-a-directory")
	if errWrite := os.WriteFile(parentFile, []byte("x"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := newAdaptiveShadowAuditStore(filepath.Join(parentFile, "state.json"), 4)
	go store.run()
	store.queue <- sanitizeAdaptiveShadowAuditRecord(
		adaptiveShadowAuditTestRecord(time.Now(), adaptiveShadowDecisionAdmit, true, ""),
	)
	store.close()
	if store.writeFailures.Load() == 0 {
		t.Fatal("unwritable audit path did not record a telemetry-only write failure")
	}
	if len(store.records) != 1 {
		t.Fatalf("memory audit records=%d, want 1", len(store.records))
	}
}

func TestAdaptiveShadowAuditManagementJSONAndRussianText(t *testing.T) {
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 4)
	store.appendMemory(adaptiveShadowAuditTestRecord(time.Now(), adaptiveShadowDecisionAdmit, true, ""))
	previous := swapAdaptiveShadowAuditStoreForTest(store)
	t.Cleanup(func() { swapAdaptiveShadowAuditStoreForTest(previous) })

	for _, format := range []string{"json", "text"} {
		raw, errHandle := handleAdaptiveShadowAuditManagement(rpcManagementRequest{
			ManagementRequest: pluginapi.ManagementRequest{
				Method: http.MethodGet,
				Path:   adaptiveShadowAuditManagementPath,
				Query:  url.Values{"format": []string{format}, "hours": []string{"24"}},
			},
		})
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		var env envelope
		if errDecode := json.Unmarshal(raw, &env); errDecode != nil {
			t.Fatal(errDecode)
		}
		var response pluginapi.ManagementResponse
		if errDecode := json.Unmarshal(env.Result, &response); errDecode != nil {
			t.Fatal(errDecode)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("format=%s status=%d body=%s", format, response.StatusCode, response.Body)
		}
		if format == "text" {
			text := string(response.Body)
			for _, expected := range []string{
				"теневой аудит адаптивного распределителя",
				"влияние на маршрутизацию: нет",
				"Дополнительные обращения к подпискам/провайдерам: 0",
			} {
				if !strings.Contains(text, expected) {
					t.Errorf("text report missing %q: %s", expected, text)
				}
			}
		}
	}
}

func BenchmarkAdaptiveShadowAuditEnqueue(b *testing.B) {
	store := newAdaptiveShadowAuditStore(filepath.Join(b.TempDir(), "state.json"), 1)
	previous := swapAdaptiveShadowAuditStoreForTest(store)
	b.Cleanup(func() { swapAdaptiveShadowAuditStoreForTest(previous) })
	record := adaptiveShadowAuditTestRecord(time.Now(), adaptiveShadowDecisionAdmit, true, "")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		enqueueAdaptiveShadowAudit(record)
		<-store.queue
	}
}

func adaptiveShadowAuditTestRecord(
	at time.Time,
	decision string,
	success bool,
	errorCode string,
) adaptiveShadowAuditRecord {
	status := http.StatusOK
	if !success {
		status = http.StatusTooManyRequests
	}
	return adaptiveShadowAuditRecord{
		SchemaVersion:              adaptiveShadowAuditSchemaVersion,
		At:                         at.UTC(),
		TraceID:                    "trace-test",
		LogicalModel:               "bravo/fable",
		Success:                    success,
		Status:                     status,
		ActualExecutionAttempts:    1,
		RoutingEnforced:            false,
		AdditionalProviderRequests: 0,
		Attempts: []adaptiveShadowAuditAttempt{{
			Provider:            "claude",
			Model:               "claude-fable-5",
			Decision:            decision,
			EstimateConfidence:  "shape_estimate",
			ReservationPercent:  1.5,
			SafeHeadroomBefore:  30,
			SafeHeadroomAfter:   28.5,
			Outcome:             firstNonEmpty(map[bool]string{true: "succeeded", false: "failed"}[success], "failed"),
			Status:              status,
			Success:             success,
			ProviderAcceptance:  "confirmed",
			LatencyMilliseconds: 12,
			ErrorCode:           errorCode,
		}},
	}
}

func installAdaptiveShadowAuditTestStore(t *testing.T, statePath string, capacity int) *adaptiveShadowAuditStore {
	t.Helper()
	store := newAdaptiveShadowAuditStore(statePath, capacity)
	store.loadBoundedHistory()
	go store.run()
	previous := swapAdaptiveShadowAuditStoreForTest(store)
	t.Cleanup(func() {
		store.close()
		swapAdaptiveShadowAuditStoreForTest(previous)
	})
	return store
}

func swapAdaptiveShadowAuditStoreForTest(store *adaptiveShadowAuditStore) *adaptiveShadowAuditStore {
	adaptiveShadowAuditGlobal.Lock()
	previous := adaptiveShadowAuditGlobal.store
	adaptiveShadowAuditGlobal.store = store
	adaptiveShadowAuditGlobal.Unlock()
	return previous
}
