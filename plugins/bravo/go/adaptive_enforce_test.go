package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveEnforceThirtyConcurrentAcquisitionsReserveAtomicallyWithoutQueue(t *testing.T) {
	now := time.Now().UTC()
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{
		"burst-auth": adaptiveEnforcementQuota(now, 20),
	})
	const workers = 30
	start := make(chan struct{})
	type result struct {
		release func(bool)
		ok      bool
	}
	results := make(chan result, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		attempt := adaptiveEnforcementAttempt("burst-auth", true, 5, adaptiveEnforcementQuota(now, 20), now)
		go func() {
			defer group.Done()
			<-start
			release, acquired, _ := acquireAdaptiveEnforcementLease(attempt, now)
			results <- result{release: release, ok: acquired}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("adaptive acquisition waited or deadlocked")
	}
	acquired := 0
	releases := make([]func(bool), 0, workers)
	for index := 0; index < workers; index++ {
		item := <-results
		if item.ok {
			acquired++
			releases = append(releases, item.release)
		}
	}
	// 20% remaining with a 5pp forecast permits three reservations. The fourth
	// would leave zero safe headroom and is immediately routed elsewhere.
	if acquired != 3 {
		t.Fatalf("concurrent acquisitions=%d, want 3", acquired)
	}
	if got := adaptiveShadowSummary(loadedConfig(), []string{"burst-auth"}, now).InFlightReservations; got != 3 {
		t.Fatalf("in-flight reservations=%d, want 3", got)
	}
	for _, release := range releases {
		release(false)
	}
	if got := adaptiveShadowSummary(loadedConfig(), []string{"burst-auth"}, now).InFlightReservations; got != 0 {
		t.Fatalf("released in-flight reservations=%d, want 0", got)
	}
}

func TestAdaptiveEnforcePublicViewsReportRealRoutingEffectWithoutQueues(t *testing.T) {
	now := time.Now().UTC()
	installAdaptiveEnforcementTestState(t, nil)
	view := adaptiveShadowSummary(loadedConfig(), nil, now)
	if view.Mode != "enforce" || view.Effect != "routing_enforced" || !view.RoutingEnforced ||
		view.AdditionalProviderRequests || !view.EdgeGate.RoutingEnforced ||
		view.EdgeGate.Effect != "routing_enforced" || view.EdgeGate.QueuesRequests ||
		view.EdgeGate.AdditionalProviderRequests {
		t.Fatalf("enforce public view=%#v", view)
	}
}

func TestAdaptiveEnforceUnknownAndStaleQuotaFailOpen(t *testing.T) {
	now := time.Now().UTC()
	stale := adaptiveEnforcementQuota(now.Add(-20*time.Minute), 1)
	unknown := credentialQuotaState{Confidence: "unknown"}
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{
		"unknown-auth": unknown,
		"stale-auth":   stale,
	})
	for authIndex, quota := range map[string]credentialQuotaState{
		"unknown-auth": unknown,
		"stale-auth":   stale,
	} {
		t.Run(authIndex, func(t *testing.T) {
			attempt := adaptiveEnforcementAttempt(authIndex, true, 10, quota, now)
			attempt.AllocatorManaged = false
			release, acquired, failure := acquireExecutionAttemptLease(attempt)
			if !acquired || failure != nil {
				t.Fatalf("fail-open acquisition=%v failure=%#v", acquired, failure)
			}
			snapshot := attempt.AdaptiveEdgeGate.snapshot()
			if snapshot.Decision != adaptiveEdgeGateDecisionDispatch ||
				(snapshot.Reason != "quota_unconfirmed_fail_open" && snapshot.Reason != "quota_stale_fail_open" &&
					snapshot.Reason != "quota_or_forecast_unconfirmed_fail_open") {
				t.Fatalf("fail-open edge snapshot=%#v", snapshot)
			}
			release(false)
		})
	}
	if view := adaptiveEdgeGateSummary(loadedConfig(), []string{"unknown-auth", "stale-auth"}, now); view.InFlightGuards != 0 {
		t.Fatalf("unknown/stale quota created enforced guards: %#v", view)
	}
}

func TestAdaptiveEnforceUnknownFailsOpenToOrdinaryAllocatorAuthority(t *testing.T) {
	now := time.Now().UTC()
	unknown := credentialQuotaState{Confidence: "unknown"}
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"unknown-secondary": unknown})
	attempt := adaptiveEnforcementAttempt("unknown-secondary", false, 5, unknown, now)
	attempt.AllocatorManaged = true
	attempt.ReservationPercent = 1
	attempt.TariffID = "x1"
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure != nil {
		t.Fatalf("ordinary allocator verdict was not authoritative: acquired=%v failure=%#v", acquired, failure)
	}
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.Decision != adaptiveEdgeGateDecisionDispatch ||
		snapshot.OutcomeTransition != "not_dispatched" {
		t.Fatalf("adaptive fail-open/base-deny lifecycle=%#v", snapshot)
	}
	if view := adaptiveShadowSummary(loadedConfig(), []string{"unknown-secondary"}, now); view.InFlightReservations != 0 {
		t.Fatalf("base denial leaked adaptive reservation: %#v", view)
	}
}

func TestAdaptiveEnforcePlanTimeFreshQuotaThatBecomesUnknownFailsOpen(t *testing.T) {
	now := time.Now().UTC()
	fresh := adaptiveEnforcementQuota(now, 1)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"drift-auth": fresh})
	attempt := adaptiveEnforcementAttempt("drift-auth", true, 5, fresh, now)
	// Simulate quota expiring between plan construction and lease acquisition.
	storeQuotaSnapshot("drift-auth", credentialQuotaState{Confidence: "unknown"})
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("plan/acquire drift did not fail open: acquired=%v failure=%#v", acquired, failure)
	}
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.Decision != adaptiveEdgeGateDecisionDispatch ||
		snapshot.Reason != "quota_or_forecast_unconfirmed_fail_open" {
		t.Fatalf("refreshed edge snapshot=%#v", snapshot)
	}
	release(false)
}

func TestAdaptiveEnforceSecondarySkipContinuesToPrimaryNeighbor(t *testing.T) {
	now := time.Now().UTC()
	secondaryQuota := adaptiveEnforcementQuota(now, 55)
	primaryQuota := adaptiveEnforcementQuota(now, 10)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{
		"shared-secondary": secondaryQuota,
		"owned-primary":    primaryQuota,
	})
	secondary := adaptiveEnforcementAttempt("shared-secondary", false, 5, secondaryQuota, now)
	primary := adaptiveEnforcementAttempt("owned-primary", true, 5, primaryQuota, now)

	_, acquired, failure := acquireExecutionAttemptLease(secondary)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_quota_withheld" || !failure.RouteFallback {
		t.Fatalf("secondary result acquired=%v failure=%#v", acquired, failure)
	}
	release, acquired, failure := acquireExecutionAttemptLease(primary)
	if !acquired || failure != nil {
		t.Fatalf("primary neighbor acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
}

func TestAdaptiveEnforcePrimaryHasZeroFloorButNotZeroQuotaExemption(t *testing.T) {
	now := time.Now().UTC()
	zero := adaptiveEnforcementQuota(now, 0)
	positive := adaptiveEnforcementQuota(now, 2)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{
		"zero-primary":     zero,
		"positive-primary": positive,
	})
	_, acquired, failure := acquireExecutionAttemptLease(
		adaptiveEnforcementAttempt("zero-primary", true, 1, zero, now),
	)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_quota_withheld" {
		t.Fatalf("zero primary acquired=%v failure=%#v", acquired, failure)
	}
	release, acquired, failure := acquireExecutionAttemptLease(
		adaptiveEnforcementAttempt("positive-primary", true, 1, positive, now),
	)
	if !acquired || failure != nil {
		t.Fatalf("positive primary acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
}

func TestAdaptiveEnforceAcceptedCommitCoolsAndPreAcceptanceFailureReleases(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 30)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{
		"accepted-auth": quota,
		"failed-auth":   quota,
	})
	accepted := adaptiveEnforcementAttempt("accepted-auth", true, 5, quota, now)
	release, acquired, failure := acquireExecutionAttemptLease(accepted)
	if !acquired || failure != nil {
		t.Fatalf("accepted lease acquired=%v failure=%#v", acquired, failure)
	}
	release(true)
	view := adaptiveShadowSummary(loadedConfig(), []string{"accepted-auth"}, now)
	if view.InFlightReservations != 0 || view.TrackedCommitments != 1 || view.RawPendingPercent != 5 {
		t.Fatalf("accepted commitment view=%#v", view)
	}

	failed := adaptiveEnforcementAttempt("failed-auth", true, 5, quota, now)
	release, acquired, failure = acquireExecutionAttemptLease(failed)
	if !acquired || failure != nil {
		t.Fatalf("failed lease acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
	failedView := adaptiveShadowSummary(loadedConfig(), []string{"failed-auth"}, now)
	if failedView.InFlightReservations != 0 || failedView.TrackedCommitments != 0 || failedView.RawPendingPercent != 0 {
		t.Fatalf("failed-before-accept view=%#v", failedView)
	}
}

func TestAdaptiveEnforceComposesAndReleasesOrdinaryAllocatorLeaseExactlyOnce(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 30)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"composed-auth": quota})
	attempt := adaptiveEnforcementAttempt("composed-auth", true, 5, quota, now)
	attempt.AllocatorManaged = true
	attempt.ReservationPercent = 1
	attempt.TariffID = "x1"
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("composed lease acquired=%v failure=%#v", acquired, failure)
	}
	allocatorRuntime.Lock()
	baseInFlight := allocatorRuntime.InFlightPercent["composed-auth"]
	allocatorRuntime.Unlock()
	if baseInFlight != 1 {
		t.Fatalf("ordinary allocator in-flight=%.2f, want 1", baseInFlight)
	}
	release(false)
	release(true)
	allocatorRuntime.Lock()
	baseInFlight = allocatorRuntime.InFlightPercent["composed-auth"]
	basePending := allocatorRuntime.PendingPercent["composed-auth"]
	allocatorRuntime.Unlock()
	view := adaptiveShadowSummary(loadedConfig(), []string{"composed-auth"}, now)
	if baseInFlight != 0 || basePending != 0 || view.InFlightReservations != 0 || view.TrackedCommitments != 0 {
		t.Fatalf("double release leaked accounting: base %.2f/%.2f adaptive=%#v", baseInFlight, basePending, view)
	}
}

func TestAdaptiveEnforceRuntimeSaturationFailsOpen(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 20)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"saturated-auth": quota})
	adaptiveShadowRuntime.Lock()
	account := &adaptiveShadowAccount{LearnedScale: 1, InFlight: make(map[uint64]adaptiveShadowCommit)}
	for index := 1; index <= adaptiveEnforcementMaximumInFlight; index++ {
		account.InFlight[uint64(index)] = adaptiveShadowCommit{At: now, Percent: 0.001, SessionPercent: 0.001, WeeklyPercent: 0.001}
	}
	adaptiveShadowRuntime.Accounts["saturated-auth"] = account
	adaptiveShadowRuntime.Unlock()
	attempt := adaptiveEnforcementAttempt("saturated-auth", true, 5, quota, now)
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("saturated runtime did not fail open: acquired=%v failure=%#v", acquired, failure)
	}
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.Decision != adaptiveEdgeGateDecisionDispatch ||
		snapshot.Reason != "runtime_saturated_fail_open" {
		t.Fatalf("saturated edge snapshot=%#v", snapshot)
	}
	release(false)
	view := adaptiveShadowSummary(loadedConfig(), []string{"saturated-auth"}, now)
	if !view.Saturated || view.DroppedReservations != 1 {
		t.Fatalf("saturation view=%#v", view)
	}
}

func TestAdaptiveEnforceExpiredBreakerKeepsOneHalfOpenProbeWithStaleQuotaAndReopens(t *testing.T) {
	now := time.Now().UTC()
	fresh := adaptiveEnforcementQuota(now, 50)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"probe-auth": fresh})
	failed := adaptiveEnforcementAttempt("probe-auth", true, 1, fresh, now)
	beginAdaptiveEdgeGateShadow(failed, now)
	observeAdaptiveEdgeGateOutcome(failed, false, executionFailure{
		Code: "bravo_subscription_quota_exhausted", Status: 429, RetryAfter: "1", Retryable: true,
	}, now)
	stale := adaptiveEnforcementQuota(now.Add(-10*time.Minute), 50)
	storeQuotaSnapshot("probe-auth", stale)
	probe := adaptiveEnforcementAttempt("probe-auth", true, 1, stale, now.Add(2*time.Second))
	release, acquired, failure := acquireAdaptiveEnforcementLease(probe, now.Add(2*time.Second))
	if !acquired || failure != nil || probe.AdaptiveEdgeGate.snapshot().Decision != adaptiveEdgeGateDecisionProbe {
		t.Fatalf("half-open probe acquired=%v failure=%#v edge=%#v", acquired, failure, probe.AdaptiveEdgeGate.snapshot())
	}
	competing := adaptiveEnforcementAttempt("probe-auth", true, 1, stale, now.Add(2*time.Second))
	_, acquired, failure = acquireAdaptiveEnforcementLease(competing, now.Add(2*time.Second))
	if acquired || failure == nil || failure.Code != "bravo_adaptive_edge_busy" {
		t.Fatalf("competing half-open request acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
	observeAdaptiveEdgeGateOutcome(probe, true, executionFailure{}, now.Add(3*time.Second))
	reopened := adaptiveEnforcementAttempt("probe-auth", true, 1, stale, now.Add(4*time.Second))
	release, acquired, failure = acquireAdaptiveEnforcementLease(reopened, now.Add(4*time.Second))
	if !acquired || failure != nil || reopened.AdaptiveEdgeGate.snapshot().Decision != adaptiveEdgeGateDecisionDispatch {
		t.Fatalf("reopened route acquired=%v failure=%#v edge=%#v", acquired, failure, reopened.AdaptiveEdgeGate.snapshot())
	}
	release(false)
	if view := adaptiveEdgeGateSummary(loadedConfig(), []string{"probe-auth"}, now.Add(4*time.Second)); view.TrackedBreakers != 0 {
		t.Fatalf("successful probe left breaker behind: %#v", view)
	}
}

func TestAdaptiveEnforceCompactBypassIsNotWithheld(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 1)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"compact-auth": quota})
	attempt := adaptiveEnforcementAttempt("compact-auth", false, 5, quota, now)
	attempt.CompactBypass = true
	attempt.CompactBypassKey = fmt.Sprintf("compact-enforce-%d", now.UnixNano())
	attempt.CompactBypassCooldownSeconds = 60
	// Rebuild edge state after marking the attempt as the explicit bypass.
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(
		loadedConfig(), attempt, quota, tariffByID(loadedConfig(), "x1"), now,
	)
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("compact bypass acquired=%v failure=%#v", acquired, failure)
	}
	if snapshot := attempt.AdaptiveEdgeGate.snapshot(); snapshot.Decision != adaptiveEdgeGateDecisionDispatch || snapshot.Reason != "compact_bypass_fail_open" {
		t.Fatalf("compact edge snapshot=%#v", snapshot)
	}
	release(false)
}

func TestAdaptiveEnforcedSkipIsPersistedWithoutProviderCall(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 55)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"audit-secondary": quota})
	enforceConfig := loadedConfig()
	observeConfig := enforceConfig
	observeConfig.AdaptiveAllocatorMode = "observe"
	// The recorder exists before the execution plan. Simulate observe -> enforce
	// in that gap; the immutable attempt must determine the audit mode.
	currentConfig.Store(observeConfig)
	recorder := newRouteTraceRecorder(rpcExecutorRequest{}, "bravo/fable", protocolClaude, false)
	currentConfig.Store(enforceConfig)
	temp := t.TempDir()
	store := installAdaptiveShadowAuditTestStore(t, filepath.Join(temp, "state.json"), 8)
	previousTraces := bravoRouteTraces
	bravoRouteTraces = newRouteTraceStore(filepath.Join(temp, "routes.json"))
	t.Cleanup(func() { bravoRouteTraces = previousTraces })

	attempt := adaptiveEnforcementAttempt("audit-secondary", false, 5, quota, now)
	attempt.AdaptiveShadowDecision = adaptiveShadowDecisionWithhold
	attempt.AdaptiveShadowHeadroomBefore = 5
	attempt.AdaptiveShadowHeadroomAfter = 0
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure == nil {
		t.Fatalf("enforced skip acquired=%v failure=%#v", acquired, failure)
	}
	recorder.trace.TraceID = "adaptive-enforced-skip"
	recorder.trace.StartedAt = now
	recorder.failure(attempt, now, failure.Status, *failure)
	// The attempt, rather than an earlier or later global config read, owns the
	// mode used by its asynchronous audit record.
	currentConfig.Store(observeConfig)
	recorder.finish(false, failure.Status, *failure)
	currentConfig.Store(enforceConfig)
	store.close()

	// Reporting a historical window after an enforce -> observe hot reload must
	// not erase evidence that this request changed routing.
	currentConfig.Store(observeConfig)
	report := store.report(loadedConfig(), 24*time.Hour, 10, now.Add(time.Second))
	if report.Mode != "observe" || !report.RoutingEnforced || report.RoutingChangesApplied != 1 ||
		report.ActualExecutionAttempts != 0 || report.AdditionalProviderRequests != 0 ||
		len(report.Recent) != 1 || len(report.Recent[0].Attempts) != 1 {
		t.Fatalf("enforced audit report=%#v", report)
	}
	recent := report.Recent[0]
	if !recent.RoutingEnforced || recent.RoutingChangesApplied != 1 ||
		recent.ActualExecutionAttempts != 0 || recent.AdditionalProviderRequests != 0 ||
		recent.Attempts[0].Outcome != "withheld" ||
		recent.Attempts[0].ProviderAcceptance != "not_dispatched" {
		t.Fatalf("enforced audit record=%#v", recent)
	}
	raw, errRead := os.ReadFile(store.path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	text := string(raw)
	for _, marker := range []string{
		`"routing_enforced":true`, `"routing_changes_applied":1`,
		`"actual_execution_attempts":0`, `"additional_provider_requests":0`,
		`"provider_acceptance":"not_dispatched"`,
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("JSONL missing %s: %s", marker, text)
		}
	}
}

func TestAdaptiveAuditObserveAttemptDoesNotInheritRecorderCreationMode(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 80)
	installAdaptiveEnforcementTestState(t, map[string]credentialQuotaState{"audit-observe": quota})
	// Reverse drift: recorder sees enforce, but the actual plan is built after a
	// reload to observe. The dispatched attempt must remain non-enforcing.
	recorder := newRouteTraceRecorder(rpcExecutorRequest{}, "bravo/fable", protocolClaude, false)
	observeConfig := loadedConfig()
	observeConfig.AdaptiveAllocatorMode = "observe"
	currentConfig.Store(observeConfig)
	attempt := adaptiveEnforcementAttempt("audit-observe", true, 1, quota, now)
	attempt.AdaptiveProviderDispatched = true
	recorder.captureAdaptiveAuditAttempt(attempt, now, http.StatusOK, true, "succeeded", "")
	if recorder.adaptiveAuditRoutingEnforced {
		t.Fatal("observe attempt inherited enforce from recorder creation before hot reload")
	}
}

func installAdaptiveEnforcementTestState(t *testing.T, quotas map[string]credentialQuotaState) {
	t.Helper()
	restoreUsage := isolateBravoUsageState(t)
	t.Cleanup(restoreUsage)
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "enforce"
	cfg.AllocatorMode = "off"
	cfg.QuotaUsageRefreshSeconds = 5 * 60
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	allocatorRuntime.Lock()
	previousInFlight := allocatorRuntime.InFlightPercent
	previousPending := allocatorRuntime.PendingPercent
	allocatorRuntime.InFlightPercent = make(map[string]float64)
	allocatorRuntime.PendingPercent = make(map[string]float64)
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		allocatorRuntime.InFlightPercent = previousInFlight
		allocatorRuntime.PendingPercent = previousPending
		allocatorRuntime.Unlock()
	})
	for authIndex, quota := range quotas {
		storeQuotaSnapshot(authIndex, quota)
	}
}

func adaptiveEnforcementQuota(at time.Time, remaining float64) credentialQuotaState {
	return credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: at, RefreshedAt: at,
		Session: quotaWindowState{
			UsedPercent: 100 - remaining, RemainingPercent: remaining,
			ResetAt: at.Add(time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled,
		},
		Weekly: quotaWindowState{
			UsedPercent: 100 - remaining, RemainingPercent: remaining,
			ResetAt: at.Add(24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled,
		},
	}
}

func adaptiveEnforcementAttempt(
	authIndex string,
	primary bool,
	reservation float64,
	quota credentialQuotaState,
	now time.Time,
) executionAttempt {
	cfg := loadedConfig()
	attempt := executionAttempt{
		Candidate:                         candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth:                              pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		ProjectID:                         "adaptive-enforce-test",
		Primary:                           primary,
		AdaptiveShadow:                    true,
		AdaptiveReservationPercent:        reservation,
		AdaptiveSessionReservationPercent: reservation,
		AdaptiveWeeklyReservationPercent:  reservation,
		AdaptiveEstimateConfidence:        "token_calibrated_complete",
		AdaptivePredictedTokens:           4096,
	}
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(
		cfg, attempt, normalizedQuotaState(quota), tariffByID(cfg, "x1"), now,
	)
	return attempt
}
