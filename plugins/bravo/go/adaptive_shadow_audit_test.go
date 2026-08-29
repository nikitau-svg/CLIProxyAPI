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
		Candidate:                             candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth:                                  pluginapi.HostAuthFileEntry{AuthIndex: "auth-secret-must-not-enter-audit"},
		ProjectID:                             "project-secret-must-not-enter-audit",
		Primary:                               false,
		AdaptiveShadow:                        true,
		AdaptiveReservationPercent:            2.5,
		AdaptiveSessionReservationPercent:     2.5,
		AdaptiveWeeklyReservationPercent:      0.4,
		AdaptiveModelWeeklyReservationPercent: 1.1,
		AdaptiveModelWeeklyName:               "fable",
		AdaptivePredictedTokens:               8192,
		AdaptiveEstimateConfidence:            "token_calibrated_complete",
		AdaptiveShadowDecision:                adaptiveShadowDecisionWithhold,
		AdaptiveShadowPendingPercent:          4,
		AdaptiveShadowHeadroomBefore:          2,
		AdaptiveShadowHeadroomAfter:           -0.5,
		AdaptiveProviderDispatched:            true,
		AdaptiveProviderAccepted:              true,
		AdaptiveEdgeGate: &adaptiveEdgeGateAttemptState{
			authIndex:              "auth-secret-must-not-enter-audit",
			state:                  adaptiveEdgeGateStateGuarded,
			decision:               adaptiveEdgeGateDecisionSkipBusy,
			reason:                 "guarded_request_busy",
			quotaConfirmed:         true,
			sessionHeadroomPercent: 3,
			weeklyHeadroomPercent:  1,
			outcomeTransition:      "counterfactual_only",
		},
	}
	recorder.success(attempt, started, http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()

	report := store.report(defaultPluginConfig(), 24*time.Hour, 10, time.Now().Add(time.Second))
	if report.RequestsObserved != 1 || report.ActualExecutionAttempts != 1 ||
		report.SuccessfulWouldWithhold != 1 || report.AdditionalProviderRequests != 0 ||
		report.RoutingChangesApplied != 0 || report.TokenCalibratedAttempts != 1 ||
		report.SuccessfulTokenCalibratedWithhold != 1 || report.LegacyShapeEstimateAttempts != 0 ||
		report.TokenCalibrationVerdict != "needs_review" {
		t.Fatalf("unexpected audit report: %#v", report)
	}
	if len(report.Recent) != 1 || len(report.Recent[0].Attempts) != 1 ||
		report.Recent[0].Attempts[0].ProviderAcceptance != "confirmed" ||
		report.Recent[0].Attempts[0].PredictedTokens != 8192 ||
		report.Recent[0].Attempts[0].WeeklyReservationPercent != 0.4 ||
		report.Recent[0].Attempts[0].EdgeGateDecision != adaptiveEdgeGateDecisionSkipBusy ||
		report.EdgeGateAttempts != 1 || report.EdgeGateSuccessfulWouldSkip != 1 {
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

func TestAdaptiveShadowAuditClosesUntrustedHostErrorTaxonomy(t *testing.T) {
	for _, marker := range []string{"bravo_QZ9OpaqueCredential774411", "QZ9OpaqueCredential774411LongAlphaNumericSecret998877"} {
		t.Run(marker[:8], func(t *testing.T) {
			temp := t.TempDir()
			statePath := filepath.Join(temp, "state.json")
			store := installAdaptiveShadowAuditTestStore(t, statePath, 8)
			classified := classifyExecutionError(&hostCallError{Code: marker, Message: "opaque provider failure", HTTPStatus: http.StatusBadGateway})
			if classified.Code != marker {
				t.Fatalf("classifier no longer preserves adversarial code; test needs a new real ingress: %#v", classified)
			}
			recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "privacy-error", StartedAt: time.Now().Add(-time.Second)}}
			attempt := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
				AdaptiveShadowDecision: adaptiveShadowDecisionAdmit, AdaptiveProviderDispatched: true}
			recorder.failure(attempt, time.Now(), classified.Status, classified)
			recorder.finish(false, classified.Status, classified)
			store.close()
			jsonl, err := os.ReadFile(store.path)
			if err != nil {
				t.Fatal(err)
			}
			sidecar, err := os.ReadFile(store.aggregatePath())
			if err != nil {
				t.Fatal(err)
			}
			for name, raw := range map[string][]byte{"jsonl": jsonl, "sidecar": sidecar} {
				if strings.Contains(string(raw), marker) {
					t.Fatalf("%s persisted opaque host error %q: %s", name, marker, raw)
				}
			}
			if !strings.Contains(string(jsonl), "unclassified_provider_error") {
				t.Fatalf("generic closed category missing: %s", jsonl)
			}
			reloaded := newAdaptiveShadowAuditStore(statePath, 8)
			reloaded.loadBoundedHistory()
			report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 10, time.Now().Add(time.Second))
			rawReport, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rawReport), marker) || !strings.Contains(string(rawReport), "unclassified_provider_error") {
				t.Fatalf("reloaded report taxonomy leak/missing category: %s", rawReport)
			}
		})
	}
}

func TestAdaptiveShadowAuditAggregatesEdgeGateCounterfactuals(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 4)
	for index := 0; index < adaptiveShadowAuditReviewRequests; index++ {
		at := now.Add(-adaptiveShadowAuditReviewCoverage).Add(
			time.Duration(index) * adaptiveShadowAuditReviewCoverage / time.Duration(adaptiveShadowAuditReviewRequests-1),
		)
		record := adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionAdmit, true, "")
		attempt := &record.Attempts[0]
		attempt.EdgeGateState = adaptiveEdgeGateStateGreen
		attempt.EdgeGateDecision = adaptiveEdgeGateDecisionDispatch
		attempt.EdgeGateReason = "confirmed_headroom"
		attempt.EdgeGateQuotaConfirmed = true
		attempt.EdgeGateSessionHeadroom = 50
		attempt.EdgeGateWeeklyHeadroom = 40
		switch index {
		case 1:
			attempt.EdgeGateState = adaptiveEdgeGateStateGuarded
			attempt.EdgeGateDecision = adaptiveEdgeGateDecisionSkipBusy
			attempt.EdgeGateOutcomeTransition = "counterfactual_only"
		case 2:
			record.Success = false
			record.Status = http.StatusTooManyRequests
			attempt.Success = false
			attempt.Status = http.StatusTooManyRequests
			attempt.ErrorCode = "bravo_subscription_quota_exhausted"
			attempt.EdgeGateState = adaptiveEdgeGateStateTripped
			attempt.EdgeGateDecision = adaptiveEdgeGateDecisionSkipTripped
			attempt.EdgeGateOutcomeTransition = "counterfactual_only"
		case 3:
			attempt.EdgeGateState = adaptiveEdgeGateStateHalfOpen
			attempt.EdgeGateDecision = adaptiveEdgeGateDecisionProbe
			attempt.EdgeGateOutcomeTransition = "reopened"
		case 4:
			record.Success = false
			record.Status = http.StatusTooManyRequests
			attempt.Success = false
			attempt.Status = http.StatusTooManyRequests
			attempt.ErrorCode = "bravo_subscription_quota_exhausted"
			attempt.EdgeGateOutcomeTransition = "tripped_model"
		}
		store.appendMemory(sanitizeAdaptiveShadowAuditRecord(record))
	}

	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.EdgeGateVerdict != "ready_for_review" ||
		report.EdgeGateAttempts != adaptiveShadowAuditReviewRequests ||
		report.EdgeGateCoverageSeconds < int64(adaptiveShadowAuditReviewCoverage/time.Second) ||
		report.EdgeGateSuccessfulWouldSkip != 1 ||
		report.EdgeGateQuotaFailuresWouldSkip != 1 ||
		report.EdgeGateQuotaFailuresWhileDispatch != 1 ||
		report.EdgeGateTripsObserved != 1 ||
		report.EdgeGateReopensObserved != 1 ||
		report.EdgeGateWouldSkipBusy != 1 ||
		report.EdgeGateWouldSkipTripped != 1 ||
		report.EdgeGateWouldProbe != 1 {
		t.Fatalf("unexpected edge-gate audit: %#v", report)
	}
}

func TestAdaptiveShadowAuditKeepsPartialCalibrationOutOfV2Cohort(t *testing.T) {
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 8)
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	record := adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionWithhold, true, "")
	record.Attempts[0].EstimateConfidence = "partial_token_calibration_1_windows"
	store.appendMemory(record)

	report := store.report(defaultPluginConfig(), time.Hour, 0, at.Add(time.Minute))
	if report.TokenCalibratedAttempts != 0 || report.LegacyShapeEstimateAttempts != 1 ||
		report.TokenCalibrationVerdict != "collecting" {
		t.Fatalf("partial calibration contaminated v2 cohort: %#v", report)
	}
}

func TestAdaptiveShadowAuditHighVolumeRetainsReviewableTimeCohort(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 8)
	const requests = adaptiveShadowAuditMemoryRecords + 1000
	for index := 0; index < requests; index++ {
		at := now.Add(-7 * time.Hour).Add(time.Duration(index) * 7 * time.Hour / time.Duration(requests-1))
		record := adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionAdmit, true, "")
		record.Attempts[0].EstimateConfidence = "token_calibrated_complete"
		store.appendMemory(sanitizeAdaptiveShadowAuditRecord(record))
	}
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict != "ready_for_review" || report.TokenCalibrationVerdict != "ready_for_review" {
		t.Fatalf("high-volume clean cohort never became reviewable: %#v", report)
	}
	if report.RequestsObserved != requests || report.CoverageSeconds < int64(6*time.Hour/time.Second) ||
		!report.HistoryTruncated || !report.HighRateTruncation || report.OldestRetainedAt.IsZero() {
		t.Fatalf("high-volume retention metadata=%#v", report)
	}
}

func TestAdaptiveShadowAuditHighVolumeBurstDoesNotFakeCoverage(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 8)
	const requests = adaptiveShadowAuditMemoryRecords + 1000
	for index := 0; index < requests; index++ {
		at := now.Add(-time.Hour).Add(time.Duration(index) * time.Hour / time.Duration(requests-1))
		record := adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionAdmit, true, "")
		record.Attempts[0].EstimateConfidence = "token_calibrated_complete"
		store.appendMemory(sanitizeAdaptiveShadowAuditRecord(record))
	}
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict == "ready_for_review" || report.TokenCalibrationVerdict == "ready_for_review" {
		t.Fatalf("one-hour burst falsely became reviewable: %#v", report)
	}
	if report.CoverageSeconds >= int64(6*time.Hour/time.Second) || !adaptiveAuditContains(report.ReadinessBlockers, "minimum_coverage") {
		t.Fatalf("burst coverage/blockers=%#v", report)
	}
}

func TestAdaptiveShadowAuditHourlyCohortSurvivesReloadWithoutMigration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newAdaptiveShadowAuditStore(statePath, 8)
	const requests = adaptiveShadowAuditMemoryRecords + 1000
	for index := 0; index < requests; index++ {
		at := now.Add(-7 * time.Hour).Add(time.Duration(index) * 7 * time.Hour / time.Duration(requests-1))
		record := adaptiveShadowAuditTestRecord(at, adaptiveShadowDecisionAdmit, true, "")
		record.ActualExecutionAttempts = 2
		record.FallbackUsed = true
		record.RoutingEnforced = true
		record.RoutingChangesApplied = 1
		record.Attempts[0].EdgeGateState = adaptiveEdgeGateStateGreen
		record.Attempts[0].EdgeGateDecision = adaptiveEdgeGateDecisionDispatch
		store.appendMemory(sanitizeAdaptiveShadowAuditRecord(record))
	}
	store.saveHours()
	reloaded := newAdaptiveShadowAuditStore(statePath, 8)
	reloaded.loadBoundedHistory()
	report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.Verdict != "ready_for_review" || report.RequestsObserved != requests ||
		report.SuccessfulRequests != requests || report.ActualExecutionAttempts != 2*requests ||
		report.RequestsWithFallback != requests || report.WouldAdmitAttempts != requests ||
		report.EdgeGateAttempts != requests || report.EdgeGateWouldDispatch != requests ||
		report.RoutingChangesApplied != requests || !report.RoutingEnforced || report.OldestRetainedAt.IsZero() {
		t.Fatalf("persisted hourly cohort was not restored: %#v", report)
	}
}

func TestAdaptiveShadowAuditReloadAddsJSONLTailNewerThanSidecar(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newAdaptiveShadowAuditStore(statePath, 8)
	old := adaptiveShadowAuditTestRecord(now.Add(-2*time.Hour), adaptiveShadowDecisionAdmit, true, "")
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(old))
	store.saveHours()
	newer := sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-time.Hour), adaptiveShadowDecisionWithhold, true, ""))
	newer.Sequence = store.aggregateCheckpoint + 1
	raw, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(store.path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := newAdaptiveShadowAuditStore(statePath, 8)
	reloaded.loadBoundedHistory()
	report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.RequestsObserved != 2 || report.WouldAdmitAttempts != 1 || report.WouldWithholdAttempts != 1 || report.SuccessfulWouldWithhold != 1 {
		t.Fatalf("jsonl crash tail was lost or double-counted: %#v", report)
	}
}

func TestAdaptiveShadowAuditSequenceRecoversDelayedOlderTimestamp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newAdaptiveShadowAuditStore(statePath, 8)
	t2 := store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-time.Hour), adaptiveShadowDecisionAdmit, true, "")))
	store.saveHours()
	t1 := sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-2*time.Hour), adaptiveShadowDecisionWithhold, true, ""))
	t1.Sequence = t2.Sequence + 1 // processed later even though its event timestamp is older
	raw, err := json.Marshal(t1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(store.path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := newAdaptiveShadowAuditStore(statePath, 8)
	reloaded.loadBoundedHistory()
	report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.RequestsObserved != 2 || report.WouldAdmitAttempts != 1 || report.WouldWithholdAttempts != 1 {
		t.Fatalf("delayed older-timestamp record was lost: %#v", report)
	}
}

func TestAdaptiveShadowAuditTwoRotationsDurablyCheckpointCrashTail(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := newAdaptiveShadowAuditStore(statePath, 8)
	// Processing order deliberately disagrees with event time.
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-time.Hour), adaptiveShadowDecisionAdmit, true, "")))
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-3*time.Hour), adaptiveShadowDecisionWithhold, true, "")))
	if err := os.WriteFile(store.path, []byte("generation-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.currentBytes.Store(15)
	if err := store.rotate(); err != nil {
		t.Fatal(err)
	}
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-2*time.Hour), adaptiveShadowDecisionAdmit, true, "")))
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-4*time.Hour), adaptiveShadowDecisionWithhold, true, "")))
	if err := os.WriteFile(store.path, []byte("generation-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.currentBytes.Store(15)
	if err := store.rotate(); err != nil {
		t.Fatal(err)
	}
	// Simulate an immediate crash: no ticker and no clean close/save follows.
	reloaded := newAdaptiveShadowAuditStore(statePath, 8)
	reloaded.loadBoundedHistory()
	report := reloaded.report(defaultPluginConfig(), 24*time.Hour, 0, now)
	if report.RequestsObserved != 4 || report.WouldAdmitAttempts != 2 || report.WouldWithholdAttempts != 2 || reloaded.aggregateCheckpoint != 4 {
		t.Fatalf("two rotations lost acknowledged crash-tail records: %#v checkpoint=%d", report, reloaded.aggregateCheckpoint)
	}
}

func TestAdaptiveShadowAuditAssistLifecycleDoesNotContaminateLegacyWithhold(t *testing.T) {
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-audit", StartedAt: time.Now().Add(-time.Second)}}
	deferred := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
		AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
	recorder.failure(deferred, time.Now(), http.StatusTooManyRequests, executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests})
	tail := deferred
	tail.AdaptiveAssistTail = true
	recorder.registerAdaptiveAssistTail(tail)
	tail.AdaptiveProviderDispatched = true
	tail.AdaptiveProviderAccepted = true
	recorder.success(tail, time.Now(), http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.AssistActuallyDeferred != 1 || report.AssistTailReached != 1 || report.AssistTailDispatched != 1 || report.AssistTailSuccess != 1 ||
		report.AssistNeighborSuccess != 0 || report.AssistLostTail != 0 || report.AssistDuplicateTail != 0 || report.AssistPrimaryDeferred != 0 ||
		report.WouldWithholdAttempts != 0 || report.SuccessfulWouldWithhold != 0 {
		t.Fatalf("assist lifecycle audit=%#v", report)
	}
}

func TestAdaptiveShadowAuditDetectsActuallyDispatchedAssistStreamHedgeMarker(t *testing.T) {
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-hedge-detector", Stream: true, StartedAt: time.Now().Add(-time.Second)}}
	attempt := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
		AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionAdmit, AdaptiveAuditStreamHedge: true,
		AdaptiveProviderDispatched: true, AdaptiveProviderAccepted: true}
	recorder.success(attempt, time.Now(), http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.AssistStreamHedge != 1 || report.AssistTailReached != 0 || report.WouldAdmitAttempts != 0 ||
		!adaptiveAuditContains(report.ReadinessBlockers, "assist_lifecycle_invariant") {
		t.Fatalf("dispatched assist stream hedge marker was not detected: %#v", report)
	}
}

func TestAdaptiveShadowAuditAssistConservesMultipleDeferredCopies(t *testing.T) {
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-multiple", StartedAt: time.Now().Add(-time.Second)}}
	base := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
		AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
	a := base
	a.Auth.AuthIndex = "same-auth"
	a.EffectiveEffort = "max"
	a.TariffID = "x5"
	b := base
	b.Auth.AuthIndex = "b"
	failure := executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests}
	recorder.failure(a, time.Now(), failure.Status, failure)
	recorder.failure(b, time.Now(), failure.Status, failure)
	tailA := a
	tailA.AdaptiveAssistTail = true
	tailB := b
	tailB.AdaptiveAssistTail = true
	recorder.registerAdaptiveAssistTail(tailA)
	recorder.registerAdaptiveAssistTail(tailB)
	tailA.AdaptiveProviderDispatched = true
	tailA.AdaptiveProviderAccepted = true
	recorder.success(tailA, time.Now(), http.StatusOK)
	recorder.finish(true, http.StatusOK, executionFailure{})
	store.close()
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.AssistActuallyDeferred != 2 || report.AssistTailReached != 1 || report.AssistNeighborSuccess != 1 ||
		report.AssistLostTail != 0 || report.AssistDuplicateTail != 0 || report.AssistRequests != 1 || report.AssistSuccessfulRequests != 1 {
		t.Fatalf("multiple deferred conservation=%#v", report)
	}
}

func TestAdaptiveShadowAuditAssistLostAndDuplicateCannotCancel(t *testing.T) {
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-conservation", StartedAt: time.Now().Add(-time.Second)}}
	base := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
		AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
	a := base
	a.Auth.AuthIndex = "same-auth"
	a.EffectiveEffort = "max"
	a.TariffID = "x5"
	failure := executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests}
	recorder.failure(a, time.Now(), failure.Status, failure)
	tailB := base
	tailB.Auth.AuthIndex = "same-auth"
	tailB.EffectiveEffort = "low"
	tailB.TariffID = "x1"
	tailB.AdaptiveAssistTail = true
	tailB.AdaptiveProviderDispatched = true
	recorder.failure(tailB, time.Now(), http.StatusBadGateway, executionFailure{Code: "provider_failed", Status: http.StatusBadGateway})
	recorder.finish(false, http.StatusBadGateway, executionFailure{Code: "provider_failed", Status: http.StatusBadGateway})
	store.close()
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.AssistLostTail != 1 || report.AssistDuplicateTail != 1 || report.Verdict != "needs_review" || report.AssistFailedRequests != 1 {
		t.Fatalf("lost+duplicate cancellation escaped audit=%#v", report)
	}
}

func TestAdaptiveShadowAuditPrimaryAssistWithholdIsInvariant(t *testing.T) {
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
	recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-primary", StartedAt: time.Now().Add(-time.Second)}}
	attempt := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Primary: true,
		AdaptiveShadow: true, AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
	failure := executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests}
	recorder.failure(attempt, time.Now(), failure.Status, failure)
	recorder.finish(false, failure.Status, failure)
	store.close()
	report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
	if report.AssistPrimaryDeferred != 1 || report.Verdict != "needs_review" || report.WouldWithholdAttempts != 0 {
		t.Fatalf("primary assist invariant not detected=%#v", report)
	}
}

func TestAdaptiveShadowAuditSavedTailNotReachedIsNotInvariant(t *testing.T) {
	for _, testCase := range []struct {
		name, code string
		status     int
	}{
		{name: "terminal", code: "provider_invalid_request", status: http.StatusBadRequest},
		{name: "cancellation", code: "context_canceled", status: 499},
		{name: "ordinary_cooldown_or_blocked_model", code: "bravo_route_temporarily_unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
			recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-not-reached-" + testCase.name, StartedAt: time.Now().Add(-time.Second)}}
			attempt := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
				AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
			tail := adaptiveAssistTailAttempt(attempt)
			recorder.registerAdaptiveAssistTail(tail)
			deferFailure := executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests}
			recorder.failure(attempt, time.Now(), deferFailure.Status, deferFailure)
			terminal := executionFailure{Code: testCase.code, Status: testCase.status}
			recorder.finish(false, terminal.Status, terminal)
			store.close()
			report := store.report(defaultPluginConfig(), 24*time.Hour, 0, time.Now().Add(time.Second))
			if report.AssistTailNotReached != 1 || report.AssistTerminalBeforeTail != 1 || report.AssistLostTail != 0 ||
				report.AssistDuplicateTail != 0 || adaptiveAuditContains(report.ReadinessBlockers, "assist_lifecycle_invariant") {
				t.Fatalf("saved tail not reached became invariant: %#v", report)
			}
		})
	}
}

func TestAdaptiveShadowAuditAssistTailBreakerRecoveryIsOneLifecycle(t *testing.T) {
	for _, testCase := range []struct {
		name                                string
		recoveryDispatched, recoverySuccess bool
	}{
		{name: "success", recoveryDispatched: true, recoverySuccess: true},
		{name: "recovery_denied", recoveryDispatched: false, recoverySuccess: false},
		{name: "recovery_provider_failure", recoveryDispatched: true, recoverySuccess: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := installAdaptiveShadowAuditTestStore(t, filepath.Join(t.TempDir(), "state.json"), 8)
			recorder := &routeTraceRecorder{trace: routeTrace{TraceID: "assist-tail-recovery-" + testCase.name, StartedAt: time.Now().Add(-time.Second)}}
			base := executionAttempt{Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, AdaptiveShadow: true,
				AdaptiveAllocatorMode: "assist", AdaptiveShadowDecision: adaptiveShadowDecisionWithhold}
			tail := adaptiveAssistTailAttempt(base)
			recorder.registerAdaptiveAssistTail(tail)
			deferFailure := executionFailure{Code: "bravo_adaptive_quota_withheld", Status: http.StatusTooManyRequests}
			recorder.failure(base, time.Now(), deferFailure.Status, deferFailure)
			trip := executionFailure{Code: "bravo_adaptive_edge_tripped", Status: http.StatusServiceUnavailable}
			recorder.failure(tail, time.Now(), trip.Status, trip)
			recovery := adaptiveBreakerLastChanceAttempt(tail)
			recovery.AdaptiveProviderDispatched = testCase.recoveryDispatched
			recovery.AdaptiveProviderAccepted = testCase.recoverySuccess
			if testCase.recoverySuccess {
				recorder.success(recovery, time.Now(), http.StatusOK)
				recorder.finish(true, http.StatusOK, executionFailure{})
			} else {
				failure := executionFailure{Code: "bravo_adaptive_edge_busy", Status: http.StatusServiceUnavailable}
				if testCase.recoveryDispatched {
					failure.Code = "provider_failed"
					failure.Status = http.StatusBadGateway
				}
				recorder.failure(recovery, time.Now(), failure.Status, failure)
				recorder.finish(false, failure.Status, failure)
			}
			store.close()
			report := store.report(defaultPluginConfig(), 24*time.Hour, 10, time.Now().Add(time.Second))
			wantDispatched, wantSuccess := 0, 0
			if testCase.recoveryDispatched {
				wantDispatched = 1
			}
			if testCase.recoverySuccess {
				wantSuccess = 1
			}
			if report.AssistTailReached != 1 || report.AssistTailDispatched != wantDispatched || report.AssistTailSuccess != wantSuccess ||
				report.AssistDuplicateTail != 0 || report.AssistLostTail != 0 || adaptiveAuditContains(report.ReadinessBlockers, "assist_lifecycle_invariant") {
				t.Fatalf("tail recovery split one lifecycle: %#v", report)
			}
		})
	}
}

func TestAdaptiveShadowAuditExcludesPartialBoundaryHour(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 30, 0, 0, time.UTC)
	store := newAdaptiveShadowAuditStore(filepath.Join(t.TempDir(), "state.json"), 8)
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-90*time.Minute), adaptiveShadowDecisionAdmit, true, "")))
	store.appendMemory(sanitizeAdaptiveShadowAuditRecord(adaptiveShadowAuditTestRecord(now.Add(-30*time.Minute), adaptiveShadowDecisionWithhold, true, "")))
	report := store.report(defaultPluginConfig(), time.Hour, 0, now)
	if report.RequestsObserved != 1 || report.WouldAdmitAttempts != 0 || report.WouldWithholdAttempts != 1 || report.CoverageSeconds != 0 {
		t.Fatalf("partial leading hour overstated cohort: %#v", report)
	}
}

func adaptiveAuditContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
