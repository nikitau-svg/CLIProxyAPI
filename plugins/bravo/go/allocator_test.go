package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDefaultStatePathDoesNotPolluteAuthDiscoveryDirectory(t *testing.T) {
	clean := filepath.ToSlash(filepath.Clean(defaultPluginConfig().StatePath))
	if clean == "/root/.cli-proxy-api" ||
		len(clean) > len("/root/.cli-proxy-api/") &&
			clean[:len("/root/.cli-proxy-api/")] == "/root/.cli-proxy-api/" {
		t.Fatalf("default state path %q is inside the auth discovery directory", clean)
	}
}

func TestAllocatorF01SortsBySafeSurplusAfterAllGuards(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	green := pluginapi.HostAuthFileEntry{ID: "green-id", AuthIndex: "green-auth", Provider: "claude"}
	near := pluginapi.HostAuthFileEntry{ID: "near-id", AuthIndex: "near-auth", Provider: "claude"}
	installAdaptiveTestQuota(t, green.AuthIndex, 80, 80)
	installAdaptiveTestQuota(t, near.AuthIndex, 55, 55)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: green.AuthIndex, Tariff: "x1"}, {AuthIndex: near.AuthIndex, Tariff: "x1"}}
	project := smartKeyConfig{ID: "surplus-project", Status: projectStatusActive, AllowedAuthIDs: []string{green.AuthIndex, near.AuthIndex}}
	attempts := allocateCandidateAuthsForShape(cfg, project,
		candidate{Provider: "claude", Model: "claude-sonnet-5"}, []pluginapi.HostAuthFileEntry{near, green}, "sticky",
		adaptiveRequestShape{Multiplier: 1, ModelFamily: "sonnet", PhysicalModel: "claude-sonnet-5", Provider: "claude", CostMode: "unknown"})
	if len(attempts) != 2 || attempts[0].Auth.AuthIndex != green.AuthIndex {
		t.Fatalf("safe-surplus order = %#v, want green before nearly-red", attempts)
	}
}

func TestAllocatorF02PendingDebtChangesSafeSurplusOrder(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	clean := pluginapi.HostAuthFileEntry{ID: "clean-id", AuthIndex: "clean-auth", Provider: "claude"}
	debted := pluginapi.HostAuthFileEntry{ID: "debted-id", AuthIndex: "debted-auth", Provider: "claude"}
	installAdaptiveTestQuota(t, clean.AuthIndex, 80, 80)
	installAdaptiveTestQuota(t, debted.AuthIndex, 80, 80)
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent[debted.AuthIndex] = 25
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		delete(allocatorRuntime.PendingPercent, debted.AuthIndex)
		allocatorRuntime.Unlock()
	})
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: clean.AuthIndex, Tariff: "x1"}, {AuthIndex: debted.AuthIndex, Tariff: "x1"}}
	project := smartKeyConfig{ID: "debt-project", Status: projectStatusActive, AllowedAuthIDs: []string{clean.AuthIndex, debted.AuthIndex}}
	attempts := allocateCandidateAuthsForShape(cfg, project,
		candidate{Provider: "claude", Model: "claude-sonnet-5"}, []pluginapi.HostAuthFileEntry{debted, clean}, "sticky",
		adaptiveRequestShape{Multiplier: 1, ModelFamily: "sonnet", PhysicalModel: "claude-sonnet-5", Provider: "claude", CostMode: "unknown"})
	if len(attempts) != 2 || attempts[0].Auth.AuthIndex != clean.AuthIndex {
		t.Fatalf("pending-aware safe-surplus order = %#v, want clean credential first", attempts)
	}
}

func TestAllocatorF12AmberWakeIsNonblockingAndCoalesced(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "amber-id", AuthIndex: "amber-auth", Provider: "claude"}
	installAdaptiveTestQuota(t, auth.AuthIndex, 55, 55)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "x1"}}
	project := smartKeyConfig{ID: "amber-project", Status: projectStatusActive, AllowedAuthIDs: []string{auth.AuthIndex}}
	previousWake := allocatorQuotaPollingWake
	allocatorAmberWakeLast.Store(0)
	var wakes atomic.Int64
	allocatorQuotaPollingWake = func() { wakes.Add(1) }
	t.Cleanup(func() {
		allocatorQuotaPollingWake = previousWake
		allocatorAmberWakeLast.Store(0)
	})
	for index := 0; index < 2; index++ {
		attempts := allocateCandidateAuthsForShape(cfg, project,
			candidate{Provider: "claude", Model: "claude-sonnet-5"}, []pluginapi.HostAuthFileEntry{auth}, "sticky",
			adaptiveRequestShape{Multiplier: 1, ModelFamily: "sonnet", PhysicalModel: "claude-sonnet-5", Provider: "claude", CostMode: "unknown"})
		if len(attempts) != 1 {
			t.Fatalf("amber wake blocked routing: attempts=%d", len(attempts))
		}
	}
	if wakes.Load() != 1 {
		t.Fatalf("two amber allocations emitted %d wakes, want one coalesced wake", wakes.Load())
	}
	maybeWakeQuotaPollingForAmber(time.Now().Add(allocatorAmberWakeCooldown + time.Second))
	if wakes.Load() != 2 {
		t.Fatalf("wake did not reopen after cooldown: %d", wakes.Load())
	}
}

func TestAllocatorObserveModeKeepsLegacyOrderAndAttachesShadowDecisions(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	near := pluginapi.HostAuthFileEntry{ID: "observe-near", AuthIndex: "observe-near", Provider: "claude", Priority: 100}
	green := pluginapi.HostAuthFileEntry{ID: "observe-green", AuthIndex: "observe-green", Provider: "claude", Priority: 10}
	installAdaptiveTestQuota(t, near.AuthIndex, 49, 49)
	installAdaptiveTestQuota(t, green.AuthIndex, 90, 90)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Models = map[string]logicalModel{"observe-route": {Candidates: []candidate{{Provider: "claude", Model: "claude-sonnet-5", Capabilities: []string{capabilityText}}}}}
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: near.AuthIndex, Tariff: "x1"}, {AuthIndex: green.AuthIndex, Tariff: "x1"}}
	cfg.SmartKeys = []smartKeyConfig{{ID: "observe-project", Name: "Observe project", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: projectStatusActive, Enabled: boolPointer(true), Models: []string{"*"}, AllowedAuthIDs: []string{near.AuthIndex, green.AuthIndex}}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("unexpected host callback %q", method)
		}
		return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{near, green}}), nil
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:    "bravo/observe-route",
		Metadata: compactProjectMetadata("observe-project"),
	}}
	plan, errPlan := buildExecutionPlan(req, "observe-route", cfg.Models["observe-route"], textContract())
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	if len(plan) != 2 || plan[0].Auth.AuthIndex != near.AuthIndex || plan[1].Auth.AuthIndex != green.AuthIndex {
		t.Fatalf("observe execution order = %#v, want legacy priority order near,green", plan)
	}
	if plan[0].AllocatorManaged || plan[1].AllocatorManaged {
		t.Fatal("observe mode unexpectedly enabled enforcement leases")
	}
	if plan[0].AdaptiveTrace.mode != "observe" || plan[0].AdaptiveTrace.rejection == "" {
		t.Fatalf("withheld shadow decision = %#v", plan[0].AdaptiveTrace)
	}
	if plan[1].AdaptiveTrace.mode != "observe" || plan[1].AdaptiveTrace.decision == "" {
		t.Fatalf("admitted shadow decision = %#v", plan[1].AdaptiveTrace)
	}
}

func TestAllocatorObserveShadowPredictsBurstFloorWithoutChangingLegacyTraffic(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "observe-burst", AuthIndex: "observe-burst", Provider: "claude"}
	installAdaptiveTestQuota(t, auth.AuthIndex, 40, 40)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Tariffs = []tariffConfig{{ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 5, ReservationPercent: 1}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "x5"}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	const requests = 109
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
		ProjectID: "observe-burst-project", AllocatorObserve: true,
		ReservationPercent: 1, TariffID: "x5",
	}
	type result struct {
		release func(bool)
		attempt executionAttempt
	}
	results := make(chan result, requests)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < requests; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			release, acquired, failure, effective := acquireExecutionAttemptLeaseDetailed(attempt)
			if !acquired || failure != nil {
				results <- result{release: func(bool) {}, attempt: executionAttempt{}}
				return
			}
			results <- result{release: release, attempt: effective}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	providerCalls, shadowAdmitted, shadowRejected := 0, 0, 0
	releases := make([]func(bool), 0, requests)
	for item := range results {
		providerCalls++ // observe mode always permits the legacy provider call.
		releases = append(releases, item.release)
		if item.attempt.AdaptiveTrace.decision != "" {
			shadowAdmitted++
		} else if item.attempt.AdaptiveTrace.rejection != "" {
			shadowRejected++
		}
	}
	for _, release := range releases {
		release(true)
	}
	if providerCalls != requests {
		t.Fatalf("legacy provider calls = %d, want %d", providerCalls, requests)
	}
	if shadowAdmitted > 19 || shadowRejected < requests-19 {
		t.Fatalf("shadow burst decisions admitted=%d rejected=%d, want <=19 admitted and >=90 fallback points", shadowAdmitted, shadowRejected)
	}
	allocatorObserveRuntime.Lock()
	shadow := allocatorObserveRuntime.Accounts[auth.AuthIndex]
	allocatorObserveRuntime.Unlock()
	if shadow.InFlight != 0 || shadow.Pending <= 0 {
		t.Fatalf("committed shadow lifecycle did not retain bounded pending evidence: %#v", shadow)
	}
}

func TestAllocatorObserveShadowReconcilesPendingOnlyOnNewerConfirmedQuota(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "observe-refresh", AuthIndex: "observe-refresh", Provider: "claude"}
	confirmedAt := time.Now().UTC().Add(-time.Minute)
	storeQuotaSnapshot(auth.AuthIndex, adaptivePersistenceQuota(80, confirmedAt))
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "x1"}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
		ProjectID: "observe-refresh-project", AllocatorObserve: true,
		ReservationPercent: 1, TariffID: "x1",
	}
	release, acquired, failure, first := acquireExecutionAttemptLeaseDetailed(attempt)
	if !acquired || failure != nil || first.AdaptiveTrace.decision == "" {
		t.Fatalf("first shadow lease = acquired %t failure %#v trace %#v", acquired, failure, first.AdaptiveTrace)
	}
	release(true)
	allocatorObserveRuntime.Lock()
	pending := allocatorObserveRuntime.Accounts[auth.AuthIndex].Pending
	allocatorObserveRuntime.Unlock()
	if pending <= 0 {
		t.Fatal("committed shadow request did not retain pending evidence")
	}
	watermark := captureAdaptiveRefreshWatermark(auth.AuthIndex)
	watermark.CapturedAt = time.Now().UTC()

	// An equal/cached timestamp cannot clear debt.
	applyQuotaRefreshSuccess(auth.AuthIndex, quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(80, confirmedAt), 0, time.Now().UTC(), watermark)
	equalRelease, _, _, equal := acquireExecutionAttemptLeaseDetailed(attempt)
	if equal.AdaptiveTrace.pendingGuard < pending {
		t.Fatalf("equal quota timestamp cleared shadow pending: before %.3f trace %#v", pending, equal.AdaptiveTrace)
	}
	equalRelease(false)

	// A strictly newer provider-confirmed observation reconciles all completed
	// shadow work before making the next canary decision.
	refreshedAt := time.Now().UTC().Add(time.Second)
	applyQuotaRefreshSuccess(auth.AuthIndex, quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(79, refreshedAt), 0, refreshedAt, watermark)
	newRelease, _, _, refreshed := acquireExecutionAttemptLeaseDetailed(attempt)
	if refreshed.AdaptiveTrace.pendingGuard != 0 || refreshed.AdaptiveTrace.decision == "" {
		t.Fatalf("new confirmed quota did not reopen shadow capacity: %#v", refreshed.AdaptiveTrace)
	}
	newRelease(false)
}

func TestAllocatorObserveShadowRefreshWatermarkPreservesPostStartCommit(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "observe-watermark", AuthIndex: "observe-watermark", Provider: "claude"}
	confirmedAt := time.Now().UTC().Add(-time.Minute)
	storeQuotaSnapshot(auth.AuthIndex, adaptivePersistenceQuota(80, confirmedAt))
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "x1"}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
		ProjectID: "observe-watermark-project", AllocatorObserve: true,
		ReservationPercent: 1, TariffID: "x1",
	}
	preRelease, _, _, _ := acquireExecutionAttemptLeaseDetailed(attempt)
	preRelease(true)
	watermark := captureAdaptiveRefreshWatermark(auth.AuthIndex)
	watermark.CapturedAt = time.Now().UTC()
	postRelease, _, _, _ := acquireExecutionAttemptLeaseDetailed(attempt)
	postRelease(true)
	refreshedAt := time.Now().UTC().Add(time.Second)
	applyQuotaRefreshSuccess(auth.AuthIndex, quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(78, refreshedAt), 0, refreshedAt, watermark)
	allocatorObserveRuntime.Lock()
	remaining := allocatorObserveRuntime.Accounts[auth.AuthIndex].Pending
	allocatorObserveRuntime.Unlock()
	if remaining < 0.999 || remaining > 1.001 {
		t.Fatalf("refresh cleared post-start shadow commit: pending %.3f, want 1", remaining)
	}
}

func TestAllocatorObserveShadowContinuousCommitsStayBounded(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "observe-bounded", AuthIndex: "observe-bounded", Provider: "claude"}
	storeQuotaSnapshot(auth.AuthIndex, adaptivePersistenceQuota(100, time.Now().UTC().Add(-time.Minute)))
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Tariffs = []tariffConfig{{ID: "tiny", SessionFloorPercent: 0, WeeklyFloorPercent: 0, Multiplier: 1, ReservationPercent: 0.001}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "tiny"}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
		ProjectID: "observe-bounded-project", AllocatorObserve: true,
		ReservationPercent: 0.001, TariffID: "tiny",
	}
	for index := 0; index < 1000; index++ {
		release, acquired, failure, _ := acquireExecutionAttemptLeaseDetailed(attempt)
		if !acquired || failure != nil {
			t.Fatalf("legacy observe lease %d blocked: acquired=%t failure=%#v", index, acquired, failure)
		}
		release(true)
	}
	allocatorObserveRuntime.Lock()
	state := allocatorObserveRuntime.Accounts[auth.AuthIndex]
	accountCount := len(allocatorObserveRuntime.Accounts)
	allocatorObserveRuntime.Unlock()
	if accountCount != 1 || len(state.Commits) > allocatorObserveMaximumCommits {
		t.Fatalf("continuous shadow state accounts=%d commits=%d", accountCount, len(state.Commits))
	}
}

func TestAllocatorObserveShadowPendingSurvivesTTLWithoutProviderProof(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	auth := pluginapi.HostAuthFileEntry{ID: "observe-ttl", AuthIndex: "observe-ttl", Provider: "claude"}
	confirmedAt := time.Now().UTC()
	storeQuotaSnapshot(auth.AuthIndex, adaptivePersistenceQuota(22, confirmedAt))
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "observe"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: auth.AuthIndex, Tariff: "x1"}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
		ProjectID: "observe-ttl-project", AllocatorObserve: true,
		ReservationPercent: 1, TariffID: "x1",
	}
	release, _, _, admitted := acquireExecutionAttemptLeaseDetailed(attempt)
	if admitted.AdaptiveTrace.decision == "" {
		t.Fatalf("initial shadow request rejected: %#v", admitted.AdaptiveTrace)
	}
	release(true)
	watermark := captureAdaptiveRefreshWatermark(auth.AuthIndex)
	watermark.CapturedAt = time.Now().UTC()
	allocatorObserveRuntime.Lock()
	state := allocatorObserveRuntime.Accounts[auth.AuthIndex]
	state.Updated = time.Now().Add(-allocatorObservePendingTTL - time.Minute)
	allocatorObserveRuntime.Accounts[auth.AuthIndex] = state
	allocatorObserveRuntime.Unlock()
	staleRelease, _, _, stale := acquireExecutionAttemptLeaseDetailed(attempt)
	if stale.AdaptiveTrace.rejection == "" || stale.AdaptiveTrace.pendingGuard < 0.999 {
		t.Fatalf("TTL forgot unconfirmed shadow debt: %#v", stale.AdaptiveTrace)
	}
	staleRelease(false)

	refreshedAt := time.Now().UTC().Add(time.Second)
	applyQuotaRefreshSuccess(auth.AuthIndex, quotaRefreshResourceUsage, "claude",
		adaptivePersistenceQuota(21.5, refreshedAt), 0, refreshedAt, watermark)
	refreshedRelease, _, _, refreshed := acquireExecutionAttemptLeaseDetailed(attempt)
	if refreshed.AdaptiveTrace.pendingGuard != 0 {
		t.Fatalf("proven refresh did not clear shadow ledger: %#v", refreshed.AdaptiveTrace)
	}
	refreshedRelease(false)
}

func TestAllocatorObserveShadowMatchesPrimarySecondarySafetySemantics(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		primary   bool
		unknown   bool
		ledgerSat bool
		estimator bool
		admitted  bool
	}{
		{name: "primary unknown", primary: true, unknown: true, admitted: true},
		{name: "secondary unknown", unknown: true, admitted: false},
		{name: "primary ledger saturation", primary: true, ledgerSat: true, admitted: true},
		{name: "secondary ledger saturation", ledgerSat: true, admitted: false},
		{name: "primary estimator saturation", primary: true, estimator: true, admitted: true},
		{name: "secondary estimator saturation", estimator: true, admitted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resetAdaptiveReserveForTest()
			t.Cleanup(resetAdaptiveReserveForTest)
			previousTracker := bravoProjectDemand
			bravoProjectDemand = newProjectDemandTracker(time.Minute)
			t.Cleanup(func() { bravoProjectDemand = previousTracker })
			authIndex := "observe-parity-" + testCase.name
			auth := pluginapi.HostAuthFileEntry{ID: authIndex, AuthIndex: authIndex, Provider: "claude"}
			quota := adaptivePersistenceQuota(80, time.Now().UTC())
			if testCase.unknown {
				quota.Confidence, quota.Status = "unknown", "unknown"
				quota.ConfirmedAt = time.Time{}
			}
			storeQuotaSnapshot(authIndex, quota)
			cfg := defaultPluginConfig()
			cfg.AllocatorMode = "observe"
			cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
			cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			t.Cleanup(func() { currentConfig.Store(previousConfig) })
			adaptiveRoutingSaturated.Store(testCase.ledgerSat)
			if testCase.estimator {
				adaptiveReserveRuntime.Lock()
				adaptiveReserveRuntime.Saturated[authIndex] = time.Now().UTC()
				adaptiveReserveRuntime.Unlock()
			}
			attempt := executionAttempt{
				Candidate: candidate{Provider: "claude", Model: "claude-fable-5"}, Auth: auth,
				ProjectID: "observe-parity-project", Primary: testCase.primary, AllocatorObserve: true,
				ReservationPercent: 1, TariffID: "x1",
			}
			release, acquired, failure, effective := acquireExecutionAttemptLeaseDetailed(attempt)
			if !acquired || failure != nil {
				t.Fatalf("observe changed legacy traffic: acquired=%t failure=%#v", acquired, failure)
			}
			gotAdmitted := effective.AdaptiveTrace.decision != ""
			if gotAdmitted != testCase.admitted {
				t.Fatalf("shadow admitted=%t trace=%#v, want %t", gotAdmitted, effective.AdaptiveTrace, testCase.admitted)
			}
			release(false)
		})
	}
}

func TestQuotaRefreshTTLPreventsPerRequestProviderPolling(t *testing.T) {
	now := time.Now().UTC()
	quota := credentialQuotaState{
		Confidence:  "confirmed",
		RefreshedAt: now.Add(-10 * time.Second),
		Dirty:       true,
	}
	if !quotaNeedsRefresh(quota, time.Minute, now) {
		t.Fatal("dirty confirmed quota did not request an early background refresh")
	}
	if !quotaNeedsRefresh(quota, time.Minute, now.Add(time.Minute)) {
		t.Fatal("quota did not refresh after the configured TTL")
	}
}

func TestQuotaRefreshIgnoresSingleModelAggregatePoison(t *testing.T) {
	isolateBravoCooldowns(t)

	const authIndex = "quota-model-scope-test"
	bravoUsageState.mu.Lock()
	if bravoUsageState.state.Quotas == nil {
		bravoUsageState.state.Quotas = make(map[string]*credentialQuotaState)
	}
	previousQuota, hadPreviousQuota := bravoUsageState.state.Quotas[authIndex]
	delete(bravoUsageState.state.Quotas, authIndex)
	bravoUsageState.mu.Unlock()
	t.Cleanup(func() {
		bravoUsageState.mu.Lock()
		if hadPreviousQuota {
			bravoUsageState.state.Quotas[authIndex] = previousQuota
		} else {
			delete(bravoUsageState.state.Quotas, authIndex)
		}
		bravoUsageState.mu.Unlock()
	})

	previousFetch := fetchQuotaSnapshot
	var calls atomic.Int64
	fetchQuotaSnapshot = func(_ string, _ pluginapi.HostAuthFileEntry, resource string) (credentialQuotaState, error) {
		if resource == quotaRefreshResourceProfile {
			return credentialQuotaState{ProfileRefreshedAt: time.Now().UTC()}, nil
		}
		calls.Add(1)
		return credentialQuotaState{
			Confidence:  "unknown",
			RefreshedAt: time.Now().UTC(),
		}, nil
	}
	t.Cleanup(func() {
		fetchQuotaSnapshot = previousFetch
	})

	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:             "palantir",
		AuthIndex:      authIndex,
		Provider:       "claude",
		Status:         "error",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Hour),
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-fable-5": {
				Status:         "error",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				ErrorCode:      "credits_required",
				Scope:          "model",
			},
		},
	}

	refreshQuotaSnapshots("quota-model-scope-callback", []pluginapi.HostAuthFileEntry{auth}, true)
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("quota refresh calls = %d, want 1 for a credential with one model-scoped failure", got)
	}

	setCooldown("claude", auth.ID, "", "account-wide", now.Add(time.Hour))
	refreshQuotaSnapshots("quota-account-scope-callback", []pluginapi.HostAuthFileEntry{auth}, true)
	waitQuotaRefreshIdle(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("quota refresh calls = %d after account-wide cooldown, want unchanged", got)
	}
}

func TestConfirmedQuotaAcceptsInactiveAndNotApplicableFullWindows(t *testing.T) {
	for _, mode := range []string{
		pluginapi.HostAuthQuotaResetModeInactive,
		pluginapi.HostAuthQuotaResetModeNotApplicable,
	} {
		window := quotaWindowState{
			UsedPercent:      0,
			RemainingPercent: 100,
			ResetMode:        mode,
		}
		if !validConfirmedQuotaWindow(window) {
			t.Fatalf("mode %q was not accepted as a confirmed full window", mode)
		}
		window.UsedPercent = 1
		window.RemainingPercent = 99
		if validConfirmedQuotaWindow(window) {
			t.Fatalf("mode %q accepted a non-full zero-reset window", mode)
		}
	}
}

func TestNotApplicableSessionCannotBlockCodexSecondary(t *testing.T) {
	resetAdaptiveReserveForTest()
	defer resetAdaptiveReserveForTest()
	now := time.Date(2026, 8, 8, 17, 0, 0, 0, time.UTC)
	const authIndex = "codex-session-not-applicable"
	shape := adaptiveRequestShape{
		Multiplier: 1, Provider: "codex", PhysicalModel: "gpt-5.6-sol",
		ModelFamily: "codex", EffortBucket: "standard", ContextBucket: "large",
	}
	profileKey := adaptiveProfileKey(authIndex, shape)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Buckets[profileKey] = &adaptiveReserveProfile{
		AuthIndex: authIndex, Shape: shape, UpdatedAt: now,
		Session: adaptiveWindowEstimate{LearnedScale: 1, ObservedBurnPerMin: 9},
		Weekly:  adaptiveWindowEstimate{LearnedScale: 1, ObservedBurnPerMin: 0.01},
	}
	adaptiveReserveRuntime.Unlock()
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent[authIndex] = 66.743
	allocatorRuntime.Unlock()
	defer func() {
		allocatorRuntime.Lock()
		delete(allocatorRuntime.PendingPercent, authIndex)
		allocatorRuntime.Unlock()
	}()

	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.QuotaUsageRefreshSeconds = 15 * 60
	tariff := tariffConfig{
		ID: "x20", SessionFloorPercent: 10, WeeklyFloorPercent: 5,
		Multiplier: 20, ReservationPercent: 0.05,
	}
	quota := credentialQuotaState{
		Status: "confirmed", Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute),
		Session: quotaWindowState{
			UsedPercent: 0, RemainingPercent: 100,
			ResetMode: pluginapi.HostAuthQuotaResetModeNotApplicable,
		},
		Weekly: quotaWindowState{
			UsedPercent: 26, RemainingPercent: 74,
			ResetMode: pluginapi.HostAuthQuotaResetModeScheduled, ResetAt: now.Add(7 * 24 * time.Hour),
		},
	}
	if guard := adaptiveExposureGuard(authIndex, profileKey, quota, adaptiveWindowSession, cfg, now); guard != 0 {
		t.Fatalf("not_applicable session exposure guard = %.3f, want 0", guard)
	}
	if !secondaryQuotaEligibleWithKey(cfg, quota, "gpt-5.6-sol", tariff, authIndex, profileKey, 0.734, 0, now) {
		t.Fatal("not_applicable session window blocked a Codex secondary whose applicable weekly window still had safe capacity")
	}
}

func TestConfirmedQuotaRequiresScheduledResetForActiveWindow(t *testing.T) {
	window := quotaWindowState{
		UsedPercent:      10,
		RemainingPercent: 90,
		ResetMode:        pluginapi.HostAuthQuotaResetModeScheduled,
	}
	if validConfirmedQuotaWindow(window) {
		t.Fatal("scheduled window without reset was accepted")
	}
	window.ResetAt = time.Now().UTC().Add(time.Hour)
	if !validConfirmedQuotaWindow(window) {
		t.Fatal("scheduled window with reset was rejected")
	}
}

func TestInferredTariffIsConservativeAndExplicitPlansStayDistinct(t *testing.T) {
	for _, test := range []struct {
		provider string
		plan     string
		want     string
	}{
		{provider: "claude", plan: "pro", want: "x1"},
		{provider: "anthropic", plan: "Claude Pro", want: "x1"},
		{provider: "codex", plan: "pro", want: "x20"},
		{provider: "openai", plan: "ChatGPT Pro", want: "x20"},
		{provider: "", plan: "pro", want: "x1"},
		{provider: "codex", plan: "ChatGPT Plus", want: "x1"},
		{provider: "codex", plan: "free", want: "x1"},
		{provider: "claude", plan: "Claude Team", want: "x5"},
		{provider: "claude", plan: "Business", want: "x5"},
		{provider: "claude", plan: "Max 5x", want: "x5"},
	} {
		if got := inferredTariffID(test.provider, test.plan); got != test.want {
			t.Fatalf("inferredTariffID(%q, %q) = %q, want %q", test.provider, test.plan, got, test.want)
		}
	}
}

func TestDefaultTariffsIncludeConservativeChatGPTProTier(t *testing.T) {
	cfg := defaultPluginConfig()
	tariff := tariffByID(cfg, "x20")
	if tariff.ID != "x20" || tariff.Multiplier != 20 {
		t.Fatalf("x20 tariff = %#v", tariff)
	}
	if tariff.SessionFloorPercent < 20 || tariff.WeeklyFloorPercent < 20 {
		t.Fatalf("x20 reserve floors are not conservative: %#v", tariff)
	}
	if tariff.ReservationPercent <= 0 || tariff.ReservationPercent > 0.1 {
		t.Fatalf("x20 reservation is not appropriately small: %#v", tariff)
	}
}

func TestEffectiveTariffUsesHostProviderWhenSnapshotProviderIsEmpty(t *testing.T) {
	cfg := defaultPluginConfig()
	got := effectiveTariff(
		cfg,
		subscriptionConfig{Tariff: "auto"},
		"openai",
		credentialQuotaState{Plan: "pro"},
	)
	if got.ID != "x20" {
		t.Fatalf("effective tariff = %q, want x20", got.ID)
	}
}

func TestNormalizeConfigBackfillsX20WithoutOverwritingLegacyTariffs(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Tariffs = []tariffConfig{
		{ID: "x1", SessionFloorPercent: 61, WeeklyFloorPercent: 62, Multiplier: 1, ReservationPercent: 0.7},
		{ID: "x5", SessionFloorPercent: 31, WeeklyFloorPercent: 32, Multiplier: 5, ReservationPercent: 0.2},
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	if got := tariffByID(cfg, "x1"); got.SessionFloorPercent != 61 || got.WeeklyFloorPercent != 62 {
		t.Fatalf("legacy x1 customization was overwritten: %#v", got)
	}
	if got := tariffByID(cfg, "x5"); got.SessionFloorPercent != 31 || got.WeeklyFloorPercent != 32 {
		t.Fatalf("legacy x5 customization was overwritten: %#v", got)
	}
	if got := effectiveTariff(
		cfg,
		subscriptionConfig{Tariff: "auto"},
		"codex",
		credentialQuotaState{Provider: "codex", Plan: "pro"},
	); got.ID != "x20" || got.Multiplier != 20 {
		t.Fatalf("legacy config Codex Pro tariff = %#v, want backfilled x20", got)
	}
}

func TestAttemptLeaseRetainsSuccessfulReservationUntilConfirmedRefresh(t *testing.T) {
	const authIndex = "1111111111111111"
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.UnknownSecondaryPolicy = "block"
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	bravoUsageState.mu.Lock()
	if bravoUsageState.state.Quotas == nil {
		bravoUsageState.state.Quotas = make(map[string]*credentialQuotaState)
	}
	previousQuota := bravoUsageState.state.Quotas[authIndex]
	bravoUsageState.state.Quotas[authIndex] = &credentialQuotaState{
		Confidence: "confirmed",
		Session: quotaWindowState{
			UsedPercent:      10,
			RemainingPercent: 90,
		},
		Weekly: quotaWindowState{
			UsedPercent:      20,
			RemainingPercent: 80,
		},
	}
	bravoUsageState.mu.Unlock()
	t.Cleanup(func() {
		bravoUsageState.mu.Lock()
		if previousQuota == nil {
			delete(bravoUsageState.state.Quotas, authIndex)
		} else {
			bravoUsageState.state.Quotas[authIndex] = previousQuota
		}
		bravoUsageState.mu.Unlock()
	})

	allocatorRuntime.Lock()
	delete(allocatorRuntime.InFlightPercent, authIndex)
	delete(allocatorRuntime.PendingPercent, authIndex)
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		delete(allocatorRuntime.InFlightPercent, authIndex)
		delete(allocatorRuntime.PendingPercent, authIndex)
		allocatorRuntime.Unlock()
	})

	attempt := executionAttempt{
		Candidate:          candidate{Model: "claude-sonnet-5"},
		Auth:               pluginapi.HostAuthFileEntry{AuthIndex: authIndex},
		AllocatorManaged:   true,
		ReservationPercent: 0.5,
		TariffID:           "x1",
	}
	release, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("eligible secondary lease was not acquired")
	}
	release(true)
	if got := pendingReservationPercent(authIndex); got != 0.5 {
		t.Fatalf("pending reservation = %v, want 0.5", got)
	}
	clearPendingReservation(authIndex, 0.5)
	if got := pendingReservationPercent(authIndex); got != 0 {
		t.Fatalf("pending reservation after confirmed refresh = %v, want 0", got)
	}
}

func TestUnknownQuotaViewUsesNullPercentagesAndKeepsModelIdentity(t *testing.T) {
	cfg := defaultPluginConfig()
	view := buildSubscriptionView(
		cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: "2222222222222222", Provider: "claude"},
		subscriptionConfig{AuthIndex: "2222222222222222", Tariff: "x1"},
		tariffByID(cfg, "x1"),
		credentialQuotaState{
			Confidence: "unknown",
			ModelWeekly: []modelQuotaWindowState{{
				Model: "opus",
				quotaWindowState: quotaWindowState{
					UsedPercent:      40,
					RemainingPercent: 60,
				},
			}},
		},
		nil,
	)
	if view.Quota.Session.UsedPercent != nil || view.Quota.Session.RemainingPercent != nil {
		t.Fatalf("unknown quota exposed numeric percentages: %#v", view.Quota.Session)
	}
	if len(view.Quota.ModelWeekly) != 1 || view.Quota.ModelWeekly[0].Model != "opus" {
		t.Fatalf("model weekly view = %#v", view.Quota.ModelWeekly)
	}
	if view.Quota.ModelWeekly[0].UsedPercent != nil {
		t.Fatalf("unknown model quota exposed a numeric percentage: %#v", view.Quota.ModelWeekly[0])
	}
}

func TestSubscriptionViewExposesNoteAndStableDisplayName(t *testing.T) {
	cfg := defaultPluginConfig()
	withNote := buildSubscriptionView(
		cfg,
		pluginapi.HostAuthFileEntry{
			AuthIndex: "note-auth-index",
			Provider:  "claude",
			Email:     "member@example.com",
			Note:      "Рабочая подписка",
			Label:     "legacy email label",
		},
		subscriptionConfig{AuthIndex: "note-auth-index", Tariff: "x5"},
		tariffByID(cfg, "x5"),
		credentialQuotaState{
			Confidence:     "confirmed",
			WorkspaceLabel: "Workspace A",
			AccountLabel:   "member@example.com",
		},
		nil,
	)
	if withNote.Note != "Рабочая подписка" ||
		withNote.DisplayName != "Рабочая подписка" ||
		withNote.Label != withNote.DisplayName {
		t.Fatalf("note presentation = %#v", withNote)
	}

	withoutNote := buildSubscriptionView(
		cfg,
		pluginapi.HostAuthFileEntry{
			AuthIndex: "fallback-auth-index",
			Provider:  "claude",
			Email:     "member@example.com",
			Label:     "legacy email label",
		},
		subscriptionConfig{AuthIndex: "fallback-auth-index", Tariff: "x1"},
		tariffByID(cfg, "x1"),
		credentialQuotaState{
			Confidence:     "confirmed",
			WorkspaceLabel: "Workspace A",
			AccountLabel:   "member@example.com",
		},
		nil,
	)
	if withoutNote.Note != "" ||
		withoutNote.DisplayName != "Workspace A · member@example.com" ||
		withoutNote.Label != withoutNote.DisplayName {
		t.Fatalf("fallback presentation = %#v", withoutNote)
	}

	const technicalAuthIndex = "claude-private-account.json"
	technicalOnly := buildSubscriptionView(
		cfg,
		pluginapi.HostAuthFileEntry{
			AuthIndex: technicalAuthIndex,
			Provider:  "claude",
			Name:      "claude-private-account.json",
			Label:     "claude-private-account.json",
		},
		subscriptionConfig{AuthIndex: technicalAuthIndex, Tariff: "x1"},
		tariffByID(cfg, "x1"),
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)
	wantRedacted := analyticsSubscriptionLabel(analyticsSubscriptionID(technicalAuthIndex), "claude")
	if technicalOnly.DisplayName != wantRedacted ||
		technicalOnly.Label != wantRedacted ||
		technicalOnly.DisplayName == technicalAuthIndex ||
		technicalOnly.DisplayName == technicalOnly.AuthID {
		t.Fatalf("technical-only presentation = %#v, want redacted %q", technicalOnly, wantRedacted)
	}
}

func TestInactiveQuotaViewUsesNullResetAndKeepsResetMode(t *testing.T) {
	cfg := defaultPluginConfig()
	view := buildSubscriptionView(
		cfg,
		pluginapi.HostAuthFileEntry{AuthIndex: "3333333333333333", Provider: "claude"},
		subscriptionConfig{AuthIndex: "3333333333333333", Tariff: "x1"},
		tariffByID(cfg, "x1"),
		credentialQuotaState{
			Confidence: "confirmed",
			Session: quotaWindowState{
				UsedPercent:      0,
				RemainingPercent: 100,
				ResetMode:        pluginapi.HostAuthQuotaResetModeInactive,
			},
			Weekly: quotaWindowState{
				UsedPercent:      25,
				RemainingPercent: 75,
				ResetAt:          time.Now().UTC().Add(4 * 24 * time.Hour),
				ResetMode:        pluginapi.HostAuthQuotaResetModeScheduled,
			},
		},
		nil,
	)
	if view.Quota.Session.ResetAt != nil ||
		view.Quota.Session.ResetMode != pluginapi.HostAuthQuotaResetModeInactive ||
		view.Quota.Session.RemainingPercent == nil ||
		*view.Quota.Session.RemainingPercent != 100 {
		t.Fatalf("inactive session view = %#v", view.Quota.Session)
	}
}

func TestTariffPatchContractAcceptsMultiplier(t *testing.T) {
	var request patchTariffRequest
	if failure := decodeAllocatorBody(
		[]byte(`{"id":"x5","multiplier":5,"session_floor_percent":30,"weekly_floor_percent":25,"reservation_percent":0.1}`),
		&request,
		false,
	); failure != nil {
		t.Fatalf("decodeAllocatorBody() failure = %#v", failure)
	}
	if request.Multiplier == nil || *request.Multiplier != 5 {
		t.Fatalf("multiplier = %#v, want 5", request.Multiplier)
	}
}

func TestTariffPatchAppendsInjectedDefaultThenReplacesPersistedOverride(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	if cfg.PersistedTariffIDs["x20"] {
		t.Fatal("injected default was incorrectly marked persisted")
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	var stored []json.RawMessage
	var operations []string
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostPluginConfigListMutate {
			t.Fatalf("unexpected host callback %q", method)
		}
		var request hostPluginConfigListMutationRequest
		decodeBravoPayload(t, payload, &request)
		if request.Field != "tariffs" || request.MatchValue != "x20" {
			t.Fatalf("tariff mutation = %#v", request)
		}
		operations = append(operations, request.Operation)
		switch request.Operation {
		case "append":
			if len(stored) != 0 {
				t.Fatal("append attempted after tariff already existed")
			}
			stored = append(stored, append(json.RawMessage(nil), request.Value...))
		case "replace":
			if len(stored) != 1 {
				t.Fatal("replace attempted before tariff existed")
			}
			stored[0] = append(json.RawMessage(nil), request.Value...)
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
		}
		return mustBravoJSON(t, hostPluginConfigListMutationResult{Items: stored}), nil
	})

	status, response := callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/tariffs",
		`{"id":"x20","session_floor_percent":24}`,
	)
	if status != http.StatusOK {
		t.Fatalf("first patch status/body = %d %#v", status, response)
	}
	hot := loadedConfig()
	if !hot.PersistedTariffIDs["x20"] ||
		tariffByID(hot, "x20").SessionFloorPercent != 24 ||
		tariffByID(hot, "x1").ID != "x1" ||
		tariffByID(hot, "x5").ID != "x5" {
		t.Fatalf("hot config after sparse append = %#v", hot.Tariffs)
	}

	status, response = callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/tariffs",
		`{"id":"x20","weekly_floor_percent":23}`,
	)
	if status != http.StatusOK {
		t.Fatalf("second patch status/body = %d %#v", status, response)
	}
	if len(operations) != 2 || operations[0] != "append" || operations[1] != "replace" {
		t.Fatalf("tariff operations = %#v", operations)
	}
	if got := tariffByID(loadedConfig(), "x20"); got.SessionFloorPercent != 24 || got.WeeklyFloorPercent != 23 {
		t.Fatalf("persisted x20 tariff = %#v", got)
	}
}
