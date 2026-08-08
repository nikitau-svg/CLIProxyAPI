package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveReservationAccountsForModelContextAndEffort(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	tariff := tariffConfig{ID: "x5", ReservationPercent: 0.1}
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "adaptive-shape"}

	short := adaptiveReservationPercent(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`)},
	}, auth, candidate{Model: "claude-haiku-4-5", Effort: "low"}, tariff, time.Now())
	large := adaptiveReservationPercent(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{OriginalRequest: []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 512*1024) + `"}],"max_tokens":32768}`)},
	}, auth, candidate{Model: "claude-fable-5", Effort: "xhigh"}, tariff, time.Now())

	if short < tariff.ReservationPercent {
		t.Fatalf("short reservation = %.3f, want at least tariff baseline %.3f", short, tariff.ReservationPercent)
	}
	if large < short*4 {
		t.Fatalf("large Fable/xhigh reservation = %.3f, short Haiku/low = %.3f; request shape was not material", large, short)
	}
}

func TestAdaptiveTariffCapacityNormalizesLearnedUpliftWithoutWeakeningBaseline(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shape := adaptiveRequestShape{Multiplier: 3, ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large"}
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "capacity-auth"}
	key := adaptiveProfileKey(auth.AuthIndex, shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, auth.AuthIndex, shape)
	profile.Session.LearnedScale = 6
	profile.Weekly.LearnedScale = 6
	adaptiveReserveRuntime.Unlock()
	x1 := adaptiveReservationForShape(auth, tariffConfig{ID: "x1", Multiplier: 1, ReservationPercent: 0.1}, shape, now)
	x5 := adaptiveReservationForShape(auth, tariffConfig{ID: "x5", Multiplier: 5, ReservationPercent: 0.1}, shape, now)
	x20 := adaptiveReservationForShape(auth, tariffConfig{ID: "x20", Multiplier: 20, ReservationPercent: 0.1}, shape, now)
	if !(x1 > x5 && x5 > x20) {
		t.Fatalf("capacity-normalized reservations x1=%.3f x5=%.3f x20=%.3f, want descending percentage", x1, x5, x20)
	}
	for name, value := range map[string]float64{"x1": x1, "x5": x5, "x20": x20} {
		if value < 0.1 {
			t.Fatalf("%s reservation %.3f weakened operator baseline", name, value)
		}
	}
}

func TestAdaptiveX20ObservedUnderpredictionConvergesWithoutSecondCapacityDivision(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "x20-learning"}
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "codex", EffortBucket: "standard", ContextBucket: "small"}
	key := adaptiveProfileKey(auth.AuthIndex, shape)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Buckets[key] = &adaptiveReserveProfile{
		AuthIndex: auth.AuthIndex, Shape: shape,
		Session: adaptiveWindowEstimate{LearnedScale: 8}, Weekly: adaptiveWindowEstimate{LearnedScale: 1},
		LearnedScale: 8, UpdatedAt: now,
	}
	adaptiveReserveRuntime.Unlock()
	tariff := tariffConfig{ID: "x20", Multiplier: 20, ReservationPercent: 0.1}
	got := adaptiveReservationForShape(auth, tariff, shape, now)
	if math.Abs(got-0.8) > 0.000001 {
		t.Fatalf("x20 learned reservation = %.3f, want observed 8x correction 0.8 (no second /20)", got)
	}
}

func TestDeclaredOutputTokenBudgetUsesBoundedConservativeScan(t *testing.T) {
	body := []byte(`{"messages":[{"content":"x"}],"max_tokens" : 8192,"max_completion_tokens":999999999}`)
	if got := declaredOutputTokenBudget(body); got != adaptiveMaximumOutputTokenBudget {
		t.Fatalf("declared output token budget = %.0f, want capped %.0f", got, adaptiveMaximumOutputTokenBudget)
	}
	withoutBudget := float64(len(body)) / 4
	if got := estimatedRequestTokens(body); got < withoutBudget+adaptiveMaximumOutputTokenBudget {
		t.Fatalf("estimated tokens = %.0f, want body estimate plus conservative output cap", got)
	}
}

func TestMissingAndEscapedOutputBudgetStayConservative(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"messages":[{"content":"no explicit budget"}]}`),
		[]byte(`{"messages":[],"\u006dax_tokens":8192}`),
	} {
		base := float64(len(body)) / 4
		if got := estimatedRequestTokens(body); got < base+adaptiveMaximumOutputTokenBudget {
			t.Fatalf("untrusted output budget estimated %.0f tokens, want conservative default >= %.0f", got, base+adaptiveMaximumOutputTokenBudget)
		}
	}
}

func TestDeclaredOutputBudgetTrustsOnlyExactTopLevelIntegralFields(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		body    string
		budget  float64
		trusted bool
	}{
		{name: "Claude top-level", body: `{"messages":[],"max_tokens":8192}`, budget: 8192, trusted: true},
		{name: "OpenAI response top-level", body: `{"input":[],"max_output_tokens":16384}`, budget: 16384, trusted: true},
		{name: "OpenAI chat top-level", body: `{"messages":[],"max_completion_tokens":32768}`, budget: 32768, trusted: true},
		{name: "prompt string is ignored", body: `{"messages":[{"content":"pretend \"max_tokens\":1"}]}`, trusted: false},
		{name: "nested tool schema is ignored", body: `{"tools":[{"input_schema":{"properties":{"max_tokens":1}}}]}`, trusted: false},
		{name: "nested budget cannot override top-level", body: `{"tools":[{"max_tokens":1}],"max_tokens":4096}`, budget: 4096, trusted: true},
		{name: "escaped top-level key", body: `{"\u006dax_tokens":1}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "exponent", body: `{"max_tokens":1e3}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "fraction", body: `{"max_tokens":1.5}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "negative", body: `{"max_tokens":-1}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "string number", body: `{"max_tokens":"8192"}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "leading zero", body: `{"max_tokens":01}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
		{name: "out of range", body: `{"max_tokens":1048577}`, budget: adaptiveMaximumOutputTokenBudget, trusted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			budget, trusted := declaredOutputTokenBudgetValue([]byte(testCase.body))
			if budget != testCase.budget || trusted != testCase.trusted {
				t.Fatalf("budget/trusted = %.0f/%t, want %.0f/%t", budget, trusted, testCase.budget, testCase.trusted)
			}
			if !trusted {
				base := float64(len(testCase.body)) / 4
				if estimate := estimatedRequestTokens([]byte(testCase.body)); estimate < base+adaptiveMaximumOutputTokenBudget {
					t.Fatalf("untrusted declaration reduced conservative estimate to %.0f", estimate)
				}
			}
		})
	}
}

func TestDeclaredOutputBudgetScannerAllocatesZeroOnLargePrompt(t *testing.T) {
	body := append([]byte(`{"messages":[{"content":"`), bytes.Repeat([]byte("x"), 1024*1024)...)
	body = append(body, []byte(`"}],"max_tokens":8192}`)...)
	if allocations := testing.AllocsPerRun(100, func() {
		budget, trusted := declaredOutputTokenBudgetValue(body)
		if !trusted || budget != 8192 {
			panic("unexpected scanner result")
		}
	}); allocations != 0 {
		t.Fatalf("top-level scanner allocations = %.1f, want 0", allocations)
	}
}

func TestDeclaredOutputAbove64KRemainsMonotonicAndConservative(t *testing.T) {
	budget64 := estimatedRequestTokens([]byte(`{"messages":[],"max_completion_tokens":65536}`))
	budget128 := estimatedRequestTokens([]byte(`{"messages":[],"max_completion_tokens":131072}`))
	budgetHuge := estimatedRequestTokens([]byte(`{"messages":[],"max_completion_tokens":999999999}`))
	if budget128 <= budget64 {
		t.Fatalf("128k output estimate %.0f did not exceed 64k estimate %.0f", budget128, budget64)
	}
	if budgetHuge < adaptiveMaximumOutputTokenBudget {
		t.Fatalf("oversized output estimate %.0f is below conservative cap %.0f", budgetHuge, adaptiveMaximumOutputTokenBudget)
	}
}

func TestMaxContextBodySkipsOutputScanAfterReservationCap(t *testing.T) {
	body := append([]byte(`{"messages":[{"content":"`), bytes.Repeat([]byte("x"), 4*1024*1024)...)
	body = append(body, []byte(`"}],"max_completion_tokens":1}`)...)
	want := float64(len(body)) / 4
	if got := estimatedRequestTokens(body); got != want {
		t.Fatalf("max-context estimate = %.0f, want body-only capped-path %.0f", got, want)
	}
	if factor := adaptiveContextFactor(want); factor != 8 {
		t.Fatalf("max-context factor = %.1f, want cap 8", factor)
	}
}

func TestAdaptiveReservationStopsBurstBeforeSecondaryFloor(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	const authIndex = "adaptive-burst-secondary"
	installAdaptiveTestQuota(t, authIndex, 40, 40)
	cfg := defaultPluginConfig()
	cfg.Tariffs = []tariffConfig{{
		ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20,
		Multiplier: 5, ReservationPercent: 0.1,
	}}
	currentConfig.Store(cfg)

	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"continue implementation"}],"max_tokens":8192}`),
	}}
	reservation := adaptiveReservationPercent(req,
		pluginapi.HostAuthFileEntry{AuthIndex: authIndex},
		candidate{Model: "claude-fable-5", Effort: "xhigh"},
		tariffByID(cfg, "x5"), time.Now())
	attempt := executionAttempt{
		Candidate: candidate{Model: "claude-fable-5", Effort: "xhigh"},
		Auth:      pluginapi.HostAuthFileEntry{AuthIndex: authIndex}, AllocatorManaged: true,
		ReservationPercent: reservation, TariffID: "x5",
	}

	accepted := 0
	for ; accepted < 109; accepted++ {
		release, ok := acquireAttemptLease(attempt)
		if !ok {
			break
		}
		release(true)
	}
	if accepted > 60 {
		t.Fatalf("accepted %d/109 burst requests with reservation %.3f%%; adaptive protection reacted too late", accepted, reservation)
	}
	if pending := pendingReservationPercent(authIndex); pending > 20.000001 {
		t.Fatalf("pending reservation %.3f%% crossed 20%% secondary headroom", pending)
	}
}

func TestAdaptiveReservePrimaryUsesZeroFloorButStopsAtExhaustion(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	const authIndex = "adaptive-primary"
	installAdaptiveTestQuota(t, authIndex, 10, 10)
	cfg := defaultPluginConfig()
	currentConfig.Store(cfg)
	attempt := executionAttempt{
		Candidate: candidate{Model: "claude-sonnet-5"},
		Auth:      pluginapi.HostAuthFileEntry{AuthIndex: authIndex}, Primary: true, AllocatorManaged: true,
		ReservationPercent: 1, TariffID: "x5",
	}

	accepted := 0
	for ; accepted < 20; accepted++ {
		release, ok := acquireAttemptLease(attempt)
		if !ok {
			break
		}
		release(true)
	}
	if accepted != 9 {
		t.Fatalf("primary accepted %d requests, want 9: floor must be zero, with strict positive remaining", accepted)
	}
}

func TestAdaptiveCalibrationLearnsObservedQuotaSlip(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Now().UTC()
	current := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 50}, Weekly: quotaWindowState{RemainingPercent: 60},
	}
	refreshed := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 30}, Weekly: quotaWindowState{RemainingPercent: 55},
	}
	observeAdaptiveQuotaRefresh("adaptive-calibration", current, refreshed, 10, now)

	baseline := tariffConfig{ID: "x5", ReservationPercent: 0.1}
	got := adaptiveReservationPercent(rpcExecutorRequest{}, pluginapi.HostAuthFileEntry{AuthIndex: "adaptive-calibration"}, candidate{Model: "claude-sonnet-5"}, baseline, now)
	withoutLearning := adaptiveReservationPercent(rpcExecutorRequest{}, pluginapi.HostAuthFileEntry{AuthIndex: "new-auth"}, candidate{Model: "claude-sonnet-5"}, baseline, now)
	if got <= withoutLearning {
		t.Fatalf("learned reservation %.3f did not exceed uncalibrated %.3f after 20%% actual / 10%% predicted", got, withoutLearning)
	}
}

func TestAcquiredLeaseIsNeverRevokedByLaterReservations(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	const authIndex = "adaptive-in-flight"
	installAdaptiveTestQuota(t, authIndex, 31, 31)
	cfg := defaultPluginConfig()
	currentConfig.Store(cfg)
	attempt := executionAttempt{
		Candidate: candidate{Model: "claude-sonnet-5"},
		Auth:      pluginapi.HostAuthFileEntry{AuthIndex: authIndex}, AllocatorManaged: true,
		ReservationPercent: 0.4, TariffID: "x5",
	}

	releaseFirst, ok := acquireAttemptLease(attempt)
	if !ok {
		t.Fatal("first request was not admitted")
	}
	for {
		release, admitted := acquireAttemptLease(attempt)
		if !admitted {
			break
		}
		release(true)
	}
	// The first request is already running. Completion must remain valid even
	// after later admissions consume all remaining headroom.
	releaseFirst(true)
	if pendingReservationPercent(authIndex) <= 0 {
		t.Fatal("started request was effectively revoked instead of committed")
	}
}

func TestSecondaryReserveFloorCannotEnterUnmanagedBypassPlan(t *testing.T) {
	model := logicalModel{Candidates: []candidate{{
		Provider: "claude", Model: "claude-fable-5", Capabilities: []string{"text"},
	}}}
	plan := allocatorBypassPlan(
		"opus", model, textContract(),
		[]pluginapi.HostAuthFileEntry{{ID: "secondary", AuthIndex: "secondary", Provider: "claude"}},
		[]candidateRejection{{
			Provider: "claude", Model: "claude-fable-5", Stage: "allocator",
			Code: "bravo_allocator_reserve_floor",
		}}, "sticky", time.Now(),
	)
	if len(plan) != 0 {
		t.Fatalf("hard secondary floor produced %d unmanaged bypass attempts", len(plan))
	}
}

func TestAdaptiveReservationNeverDropsTariffBaselineAboveInternalCap(t *testing.T) {
	const baseline = 12.5
	if got := clampAdaptiveReservation(50, baseline); got != baseline {
		t.Fatalf("clamped reservation = %.2f, want configured baseline %.2f", got, baseline)
	}
}

func TestEnforceBlocksUnknownSecondaryEvenWithLegacyAllowPolicy(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.UnknownSecondaryPolicy = "allow"
	tariff := tariffConfig{
		ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20,
		Multiplier: 5, ReservationPercent: 0.1,
	}
	if secondaryQuotaEligibleAt(
		cfg, credentialQuotaState{Confidence: "unknown"}, "claude-fable-5",
		tariff, "unknown-secondary", 0.5, time.Now(),
	) {
		t.Fatal("enforce admitted an unknown secondary through legacy allow policy")
	}

	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	release, acquired := acquireAttemptLease(executionAttempt{
		Candidate:        candidate{Model: "claude-fable-5"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: "unknown-secondary"},
		AllocatorManaged: true, Primary: false,
		ReservationPercent: 0.5, TariffID: "x5",
	})
	if acquired {
		release(false)
		t.Fatal("lease path admitted an unknown secondary in enforce mode")
	}
}

func TestAllocatorParsesRequestShapeOnceForManyAuths(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	project := smartKeyConfig{ID: "shape-once"}
	auths := make([]pluginapi.HostAuthFileEntry, 12)
	for index := range auths {
		authIndex := "shape-auth-" + string(rune('a'+index))
		auths[index] = pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"}
		installAdaptiveTestQuota(t, authIndex, 90, 90)
	}

	previousBuilder := buildAdaptiveRequestShape
	var calls atomic.Int64
	buildAdaptiveRequestShape = func(body []byte, item candidate) adaptiveRequestShape {
		calls.Add(1)
		return adaptiveRequestShapeFor(body, item)
	}
	t.Cleanup(func() { buildAdaptiveRequestShape = previousBuilder })

	got := allocateCandidateAuths(
		rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
			OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":8192}`),
		}}, cfg, project,
		candidate{Provider: "claude", Model: "claude-fable-5", Effort: "xhigh"},
		auths, "sticky",
	)
	if len(got) != len(auths) {
		t.Fatalf("allocated auths = %d, want %d", len(got), len(auths))
	}
	if calls.Load() != 1 {
		t.Fatalf("request shape parses = %d, want exactly 1 for %d auths", calls.Load(), len(auths))
	}
}

func TestExecutionPlanParsesRequestBodyOnceAcrossCandidates(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	isolateBravoFallbackTestState(t)
	claudeAuth := pluginapi.HostAuthFileEntry{ID: "request-wide-claude-id", AuthIndex: "request-wide-claude", Provider: "claude"}
	codexAuth := pluginapi.HostAuthFileEntry{ID: "request-wide-codex-id", AuthIndex: "request-wide-codex", Provider: "codex"}
	installAdaptiveTestQuota(t, claudeAuth.AuthIndex, 90, 90)
	installAdaptiveTestQuota(t, codexAuth.AuthIndex, 90, 90)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "request-wide-project", Status: projectStatusActive, Models: []string{"*"},
		PrimaryAuthIDs: []string{codexAuth.AuthIndex}, AllowedAuthIDs: []string{claudeAuth.AuthIndex, codexAuth.AuthIndex},
	}}
	cfg.Models = map[string]logicalModel{"request-wide": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-5.6-sol", Capabilities: []string{capabilityText}},
		{Provider: "claude", Model: "claude-haiku-4-5", Capabilities: []string{capabilityText}},
	}}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostLog {
			return mustBravoJSON(t, map[string]any{}), nil
		}
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("unexpected host callback %q", method)
		}
		return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{claudeAuth, codexAuth}}), nil
	})

	previousBuilder := buildAdaptiveRequestFeatures
	var calls atomic.Int64
	buildAdaptiveRequestFeatures = func(body []byte) adaptiveRequestFeatures {
		calls.Add(1)
		return adaptiveRequestFeaturesFor(body)
	}
	t.Cleanup(func() { buildAdaptiveRequestFeatures = previousBuilder })
	plan, errPlan := buildExecutionPlan(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"large shared prompt"}],"max_tokens":8192}`),
		Metadata:        compactProjectMetadata("request-wide-project"),
	}}, "request-wide", cfg.Models["request-wide"], requestCapabilityContract{
		Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText),
	})
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	if len(plan) != 3 {
		t.Fatalf("plan attempts = %d, want 3 candidates", len(plan))
	}
	foundClaudeSecondary, foundCodexPrimary := false, false
	for _, attempt := range plan {
		if attempt.Auth.AuthIndex == claudeAuth.AuthIndex && !attempt.Primary {
			foundClaudeSecondary = true
		}
		if attempt.Auth.AuthIndex == codexAuth.AuthIndex && attempt.Primary {
			foundCodexPrimary = true
		}
	}
	if !foundClaudeSecondary || !foundCodexPrimary {
		t.Fatalf("contract-compatible cross-provider fallback missing: %#v", plan)
	}
	if calls.Load() != 1 {
		t.Fatalf("request-wide body parses = %d, want exactly 1 across 3 candidates", calls.Load())
	}
}

func TestAdaptiveExposureForecastIsAppliedBeforeNearFloorAndIsNotCappedAtFive(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shape := adaptiveRequestShapeFor([]byte(`{"messages":[{"role":"user","content":"work"}]}`), candidate{
		Model: "claude-fable-5", Effort: "xhigh",
	})
	key := adaptiveProfileKey("forecast-auth", shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, "forecast-auth", shape)
	profile.Session.ObservedBurnPerMin = 2
	profile.Weekly.ObservedBurnPerMin = 1.5
	adaptiveReserveRuntime.Unlock()
	cfg := defaultPluginConfig()
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	quota := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 70}, Weekly: quotaWindowState{RemainingPercent: 70},
	}
	guard := adaptiveExposureGuard("forecast-auth", key, quota, adaptiveWindowSession, cfg, now)
	if guard < 19.9 {
		t.Fatalf("10-minute exposure at 2%%/min guard = %.2f%%, want about 20%% (not old 5%% cap)", guard)
	}
	if guard >= 50 {
		t.Fatalf("guard = %.2f%% unexpectedly consumed all 50%% secondary surplus", guard)
	}
}

func TestAdaptiveColdStartGuardIsNonzeroAndBlocksSecondaryAtRefreshHorizon(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	confirmedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	quota := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: confirmedAt,
		Session: quotaWindowState{RemainingPercent: 40}, Weekly: quotaWindowState{RemainingPercent: 40},
	}
	fresh := adaptiveExposureGuard("cold-auth", "", quota, adaptiveWindowSession, cfg, confirmedAt)
	half := adaptiveExposureGuard("cold-auth", "", quota, adaptiveWindowSession, cfg, confirmedAt.Add(5*time.Minute))
	due := adaptiveExposureGuard("cold-auth", "", quota, adaptiveWindowSession, cfg, confirmedAt.Add(10*time.Minute))
	if fresh <= 0 || !(fresh < half && half < due) || due != 100 {
		t.Fatalf("cold guard fresh/half/due = %.1f/%.1f/%.1f, want nonzero monotonic to 100", fresh, half, due)
	}
	tariff := tariffConfig{ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20, ReservationPercent: 0.1}
	if secondaryQuotaEligibleAt(cfg, quota, "claude-fable-5", tariff, "cold-auth", 0.1, confirmedAt.Add(10*time.Minute)) {
		t.Fatal("cold stale LKG admitted secondary at refresh horizon despite possible external burn 40→2")
	}
}

func TestAdaptiveLearningSeparatesFamilyEffortContextAndQuotaWindows(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fableShape := adaptiveRequestShapeFor(
		[]byte(`{"messages":[{"content":"`+strings.Repeat("f", 300*1024)+`"}]}`),
		candidate{Model: "claude-fable-5", Effort: "xhigh"},
	)
	haikuShape := adaptiveRequestShapeFor(
		[]byte(`{"messages":[{"content":"short"}]}`),
		candidate{Model: "claude-haiku-4-5", Effort: "low"},
	)
	fableKey := adaptiveProfileKey("split-auth", fableShape)
	haikuKey := adaptiveProfileKey("split-auth", haikuShape)
	if fableKey == haikuKey {
		t.Fatal("Fable/xhigh/large and Haiku/low/small shared one learning key")
	}
	for index := 0; index < 10; index++ {
		recordAdaptiveReservationCommitForKey("split-auth", fableKey, 0.5, now.Add(-time.Minute))
		recordAdaptiveReservationCommitForKey("split-auth", haikuKey, 0.5, now.Add(-time.Minute))
	}
	previous := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 70}, Weekly: quotaWindowState{RemainingPercent: 70},
		ModelWeekly: []modelQuotaWindowState{
			{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 60}},
			{Model: "haiku", quotaWindowState: quotaWindowState{RemainingPercent: 70}},
		},
	}
	refreshed := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 50}, Weekly: quotaWindowState{RemainingPercent: 65},
		ModelWeekly: []modelQuotaWindowState{
			{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 35}},
			{Model: "haiku", quotaWindowState: quotaWindowState{RemainingPercent: 69}},
		},
	}
	observeAdaptiveQuotaRefresh("split-auth", previous, refreshed, 10, now)

	adaptiveReserveRuntime.Lock()
	fable := *adaptiveReserveRuntime.Buckets[fableKey]
	haiku := *adaptiveReserveRuntime.Buckets[haikuKey]
	adaptiveReserveRuntime.Unlock()
	if fable.Weekly.LearnedScale <= haiku.Weekly.LearnedScale {
		t.Fatalf("Fable model-weekly scale %.2f <= Haiku %.2f; model windows were mixed", fable.Weekly.LearnedScale, haiku.Weekly.LearnedScale)
	}
	if fable.Session.LearnedScale == fable.Weekly.LearnedScale {
		t.Fatalf("session and selected model-weekly learned the same scale %.2f despite different drops", fable.Session.LearnedScale)
	}
}

func TestAdaptiveLearningSeparatesPhysicalModelProviderAndCostModes(t *testing.T) {
	plain := adaptiveRequestShapeFor([]byte(`{"messages":[]}`), candidate{Provider: "claude", Model: "claude-sonnet-5", Effort: "high"})
	cached := adaptiveRequestShapeFor([]byte(`{"cache_control":{"type":"ephemeral"},"messages":[]}`), candidate{Provider: "claude", Model: "claude-sonnet-5", Effort: "high"})
	tools := adaptiveRequestShapeFor([]byte(`{"tools":[],"messages":[]}`), candidate{Provider: "claude", Model: "claude-sonnet-5", Effort: "high"})
	reasoning := adaptiveRequestShapeFor([]byte(`{"reasoning":{"effort":"high"},"messages":[]}`), candidate{Provider: "claude", Model: "claude-sonnet-5", Effort: "high"})
	otherModel := adaptiveRequestShapeFor([]byte(`{"messages":[]}`), candidate{Provider: "claude", Model: "claude-sonnet-5-20260801", Effort: "high"})
	otherProvider := adaptiveRequestShapeFor([]byte(`{"messages":[]}`), candidate{Provider: "codex", Model: "claude-sonnet-5", Effort: "high"})
	keys := map[string]struct{}{}
	for _, shape := range []adaptiveRequestShape{plain, cached, tools, reasoning, otherModel, otherProvider} {
		keys[adaptiveProfileKey("shape-auth", shape)] = struct{}{}
	}
	if len(keys) != 6 {
		t.Fatalf("physical/provider/cost modes collapsed into %d learning identities, want 6", len(keys))
	}
	if !strings.Contains(plain.CostMode, "unknown") {
		t.Fatalf("missing cost flags entered cheap bucket %q instead of conservative unknown", plain.CostMode)
	}
}

func TestAdaptiveExactModelWeeklyLearningWorksAboveGlobalRemaining(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large"}
	key := adaptiveProfileKey("model-above-global", shape)
	recordAdaptiveReservationCommitForKey("model-above-global", key, 5, now.Add(-time.Minute))
	previous := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute),
		Session: quotaWindowState{RemainingPercent: 30}, Weekly: quotaWindowState{RemainingPercent: 30},
		ModelWeekly: []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 80}}},
	}
	refreshed := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 30}, Weekly: quotaWindowState{RemainingPercent: 30},
		ModelWeekly: []modelQuotaWindowState{{Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 60}}},
	}
	observeAdaptiveQuotaRefresh("model-above-global", previous, refreshed, 5, now)
	adaptiveReserveRuntime.Lock()
	weekly := adaptiveReserveRuntime.Buckets[key].Weekly
	adaptiveReserveRuntime.Unlock()
	if weekly.ObservedBurnPerMin <= 0 || weekly.LearnedScale <= 1 {
		t.Fatalf("exact model drop above global window was ignored: %#v", weekly)
	}
}

func TestColdStart109ConcurrentRequestsStayWithinOnePercentAndUseFallback(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	cfg.Tariffs = []tariffConfig{{
		ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20,
		Multiplier: 5, ReservationPercent: 0.1,
	}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	shape := adaptiveRequestShapeFor(
		[]byte(`{"messages":[{"content":"continue"}],"max_tokens":8192}`),
		candidate{Model: "claude-fable-5", Effort: "xhigh"},
	)
	key := adaptiveProfileKey("cold-secondary", shape)
	installAdaptiveTestQuota(t, "cold-secondary", 40, 40)
	installAdaptiveTestQuota(t, "cold-fallback", 100, 100)
	secondary := executionAttempt{
		Candidate:        candidate{Model: "claude-fable-5", Effort: "xhigh"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: "cold-secondary"},
		AllocatorManaged: true, ReservationPercent: adaptiveReservationForShape(
			pluginapi.HostAuthFileEntry{AuthIndex: "cold-secondary"}, tariffByID(cfg, "x5"), shape, now,
		),
		AdaptiveReserveKey: key, AdaptiveRequestShape: shape, AdaptiveBaselinePercent: 0.1, TariffID: "x5",
	}
	fallback := executionAttempt{
		Candidate:        candidate{Model: "gpt-5.6-sol", Effort: "xhigh"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: "cold-fallback"},
		AllocatorManaged: true, Primary: true, ReservationPercent: 0.1, TariffID: "x5",
	}

	const actualCost = 38.0 / 109.0
	var secondaryCount atomic.Int64
	var fallbackCount atomic.Int64
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < 109; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			release, acquired := acquireAttemptLease(secondary)
			if acquired {
				secondaryCount.Add(1)
				release(true)
				return
			}
			fallbackRelease, fallbackAcquired := acquireAttemptLease(fallback)
			if !fallbackAcquired {
				t.Errorf("cold-start compatible primary fallback was rejected")
				return
			}
			fallbackCount.Add(1)
			fallbackRelease(true)
		}()
	}
	close(start)
	wait.Wait()
	spent := float64(secondaryCount.Load()) * actualCost
	actualRemaining := 40 - spent
	slip := math.Max(20-actualRemaining, 0)
	if slip > 1 {
		t.Fatalf("cold-start burst spent %.2f%%, remaining %.2f%%, floor slip %.2f%% > 1%%", spent, actualRemaining, slip)
	}
	if fallbackCount.Load() == 0 {
		t.Fatal("cold-start burst never exercised the compatible fallback")
	}
}

func TestLegacyStaticPointOneIncidentControlCrossesFloor(t *testing.T) {
	const (
		requests   = 109
		start      = 40.0
		floor      = 20.0
		actualCost = 38.0 / requests
	)
	legacy := &legacyStaticPointOneController{remaining: start, floor: floor, reservation: 0.1}
	var providerCalls atomic.Int64
	startBurst := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-startBurst
			release, acquired := legacy.acquire()
			if !acquired {
				t.Error("legacy static controller unexpectedly rejected incident request")
				return
			}
			providerCalls.Add(1)
			release(true)
		}()
	}
	close(startBurst)
	wait.Wait()
	predictedRemaining := legacy.predictedRemaining()
	actualRemaining := start - float64(providerCalls.Load())*actualCost
	if providerCalls.Load() != requests || math.Abs(predictedRemaining-29.1) > 0.000001 {
		t.Fatalf("legacy lifecycle calls=%d predicted remaining=%.2f%%, want 109/29.1%%", providerCalls.Load(), predictedRemaining)
	}
	if slip := floor - actualRemaining; slip < 17.9 {
		t.Fatalf("legacy lifecycle actual remaining %.2f%%, floor slip %.2f%%; incident not reproduced", actualRemaining, slip)
	}
}

// legacyStaticPointOneController is a test-only replay of the retired
// controller. It deliberately knows only a static local 0.1% reservation; it
// is never reachable from production code.
type legacyStaticPointOneController struct {
	sync.Mutex
	remaining   float64
	floor       float64
	reservation float64
	inFlight    float64
	pending     float64
}

func (controller *legacyStaticPointOneController) acquire() (func(bool), bool) {
	controller.Lock()
	if controller.remaining-controller.inFlight-controller.pending-controller.reservation <= controller.floor {
		controller.Unlock()
		return func(bool) {}, false
	}
	controller.inFlight += controller.reservation
	controller.Unlock()
	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			controller.Lock()
			controller.inFlight -= controller.reservation
			if commit {
				controller.pending += controller.reservation
			}
			controller.Unlock()
		})
	}, true
}

func (controller *legacyStaticPointOneController) predictedRemaining() float64 {
	controller.Lock()
	defer controller.Unlock()
	return controller.remaining - controller.inFlight - controller.pending
}

func TestAdaptiveColdStartBurstUsesFullExecutionFallbackLifecycle(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	t.Cleanup(restoreUsage)
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	isolateBravoFallbackTestState(t)
	previousTracker := bravoProjectDemand
	bravoProjectDemand = newProjectDemandTracker(time.Minute)
	t.Cleanup(func() { bravoProjectDemand = previousTracker })

	claudeAuth := pluginapi.HostAuthFileEntry{ID: "burst-claude-id", AuthIndex: "burst-claude", Provider: "claude"}
	codexAuth := pluginapi.HostAuthFileEntry{ID: "burst-codex-id", AuthIndex: "burst-codex", Provider: "codex"}
	outsideAuth := pluginapi.HostAuthFileEntry{ID: "burst-outside-id", AuthIndex: "burst-outside", Provider: "claude"}
	installAdaptiveTestQuota(t, claudeAuth.AuthIndex, 40, 40)
	installAdaptiveTestQuota(t, codexAuth.AuthIndex, 100, 100)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{
		ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 5, ReservationPercent: 0.1,
	}}
	cfg.Subscriptions = []subscriptionConfig{
		{AuthIndex: claudeAuth.AuthIndex, Tariff: "x5"},
		{AuthIndex: codexAuth.AuthIndex, Tariff: "x5"},
	}
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "burst-project", Name: "Burst project", SHA256: strings.Repeat("a", 64), Status: projectStatusActive, Models: []string{"*"},
		PrimaryAuthIDs: []string{codexAuth.AuthIndex}, AllowedAuthIDs: []string{claudeAuth.AuthIndex, codexAuth.AuthIndex},
	}}
	cfg.Models = map[string]logicalModel{"burst-route": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Effort: "xhigh", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Priority: 90, Capabilities: []string{capabilityText}},
	}}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	authListRaw, errMarshal := json.Marshal(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{claudeAuth, codexAuth, outsideAuth}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	claudeResponse, errMarshal := json.Marshal(pluginapi.HostModelExecutionResponse{
		StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: []byte(`{"model":"claude-fable-5","choices":[{"message":{"role":"assistant","content":"claude"}}]}`),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	codexResponse, errMarshal := json.Marshal(pluginapi.HostModelExecutionResponse{
		StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: []byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"codex"}}]}`),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var claudeCalls atomic.Int64
	var codexCalls atomic.Int64
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return authListRaw, nil
		case pluginabi.MethodHostModelExecute:
			nested, ok := payload.(hostModelExecutionRequest)
			if !ok {
				return nil, fmt.Errorf("unexpected execution payload %T", payload)
			}
			if nested.AuthID == outsideAuth.ID {
				return nil, fmt.Errorf("execution escaped project allowed pool")
			}
			switch nested.ForcedProvider {
			case "claude":
				claudeCalls.Add(1)
				return claudeResponse, nil
			case "codex":
				codexCalls.Add(1)
				return codexResponse, nil
			default:
				return nil, fmt.Errorf("unexpected provider %q", nested.ForcedProvider)
			}
		case pluginabi.MethodHostLog:
			return json.RawMessage(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %q", method)
		}
	})

	requestRaw, errMarshal := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/burst-route", Format: protocolOpenAI, SourceFormat: protocolOpenAI,
		OriginalRequest: []byte(`{"model":"bravo/burst-route","messages":[{"role":"user","content":"continue"}],"max_tokens":8192}`),
		Metadata:        compactProjectMetadata("burst-project"),
	}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	start := make(chan struct{})
	errorsFound := make(chan error, 109)
	var wait sync.WaitGroup
	for index := 0; index < 109; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			raw, errExecute := execute(requestRaw)
			if errExecute != nil {
				errorsFound <- errExecute
				return
			}
			var env envelope
			if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
				errorsFound <- errUnmarshal
				return
			}
			if !env.OK {
				errorsFound <- fmt.Errorf("execution failed: %#v", env.Error)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for errExecution := range errorsFound {
		t.Error(errExecution)
	}
	const actualCost = 38.0 / 109.0
	spent := float64(claudeCalls.Load()) * actualCost
	if slip := math.Max(20-(40-spent), 0); slip > 1 {
		t.Fatalf("full execution burst made %d Claude calls, spent %.2f%% and slipped floor by %.2f%%", claudeCalls.Load(), spent, slip)
	}
	if claudeCalls.Load() == 0 || codexCalls.Load() == 0 || claudeCalls.Load()+codexCalls.Load() != 109 {
		t.Fatalf("provider lifecycle Claude=%d Codex=%d, want both and 109 total", claudeCalls.Load(), codexCalls.Load())
	}
}

func TestIncident109ConcurrentRequestsStayWithinOnePercentAndUseFallback(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	cfg.Tariffs = []tariffConfig{{
		ID: "x5", SessionFloorPercent: 20, WeeklyFloorPercent: 20,
		Multiplier: 5, ReservationPercent: 0.1,
	}}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	shape := adaptiveRequestShapeFor(
		[]byte(`{"messages":[{"content":"continue"}],"max_tokens":8192}`),
		candidate{Model: "claude-fable-5", Effort: "xhigh"},
	)
	key := adaptiveProfileKey("incident-secondary", shape)
	// Replay the confirmed incident 40 -> 2/0 so the next cycle has real
	// provider feedback, not only a local PendingPercent assertion.
	for index := 0; index < 109; index++ {
		recordAdaptiveReservationCommitForKey("incident-secondary", key, 0.1, now.Add(-time.Minute))
	}
	observeAdaptiveQuotaRefresh(
		"incident-secondary",
		credentialQuotaState{Confidence: "confirmed", ConfirmedAt: now.Add(-10 * time.Minute), Session: quotaWindowState{RemainingPercent: 40}, Weekly: quotaWindowState{RemainingPercent: 40}},
		credentialQuotaState{Confidence: "confirmed", ConfirmedAt: now, Session: quotaWindowState{RemainingPercent: 2}, Weekly: quotaWindowState{RemainingPercent: 0}},
		10.9, now,
	)
	// New provider window starts at 40%; secondary must protect 20%, while a
	// compatible primary fallback remains available.
	installAdaptiveTestQuota(t, "incident-secondary", 40, 40)
	installAdaptiveTestQuota(t, "incident-fallback", 100, 100)
	secondaryReservation := adaptiveReservationForShape(
		pluginapi.HostAuthFileEntry{AuthIndex: "incident-secondary"}, tariffByID(cfg, "x5"), shape, now,
	)
	secondary := executionAttempt{
		Candidate:        candidate{Model: "claude-fable-5", Effort: "xhigh"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: "incident-secondary"},
		AllocatorManaged: true, ReservationPercent: secondaryReservation,
		AdaptiveReserveKey: key, TariffID: "x5",
	}
	fallback := executionAttempt{
		Candidate:        candidate{Model: "gpt-5.6-sol", Effort: "xhigh"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: "incident-fallback"},
		AllocatorManaged: true, Primary: true, ReservationPercent: 0.1, TariffID: "x5",
	}

	const actualCost = 38.0 / 109.0
	var actualSpent atomic.Uint64
	var fallbackCount atomic.Int64
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < 109; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			release, acquired := acquireAttemptLease(secondary)
			if acquired {
				actualSpent.Add(uint64(math.Round(actualCost * 1_000_000)))
				release(true)
				return
			}
			fallbackRelease, fallbackAcquired := acquireAttemptLease(fallback)
			if !fallbackAcquired {
				t.Errorf("compatible primary fallback was rejected")
				return
			}
			fallbackCount.Add(1)
			fallbackRelease(true)
		}()
	}
	close(start)
	wait.Wait()
	spent := float64(actualSpent.Load()) / 1_000_000
	actualRemaining := 40 - spent
	slip := math.Max(20-actualRemaining, 0)
	if slip > 1.0 {
		t.Fatalf("concurrent replay spent %.2f%%, remaining %.2f%%, floor slip %.2f%% > 1%%", spent, actualRemaining, slip)
	}
	if fallbackCount.Load() == 0 {
		t.Fatal("all 109 requests stayed on secondary; compatible fallback was never exercised")
	}
}

func TestLeaseReevaluatesReservationAfterPlanLearningChanges(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	const authIndex = "late-learning"
	installAdaptiveTestQuota(t, authIndex, 90, 90)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	shape := adaptiveRequestShapeFor(
		[]byte(`{"messages":[{"content":"work"}],"max_tokens":8192}`),
		candidate{Model: "claude-fable-5", Effort: "xhigh"},
	)
	key := adaptiveProfileKey(authIndex, shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, authIndex, shape)
	profile.Session.LearnedScale = 5
	profile.Weekly.LearnedScale = 4
	adaptiveReserveRuntime.Unlock()

	attempt := executionAttempt{
		Candidate:        candidate{Model: "claude-fable-5", Effort: "xhigh"},
		Auth:             pluginapi.HostAuthFileEntry{AuthIndex: authIndex},
		AllocatorManaged: true, ReservationPercent: 0.1,
		AdaptiveReserveKey: key, AdaptiveRequestShape: shape,
		AdaptiveBaselinePercent: 0.1, TariffID: "x5",
	}
	release, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("re-evaluated lease was unexpectedly rejected far from floor")
	}
	release(true)
	if pending := pendingReservationPercent(authIndex); pending <= 0.5 {
		t.Fatalf("pending after late learning = %.3f%%, want current learned reservation instead of stale 0.1%%", pending)
	}
}

func TestAdaptiveRuntimeStateIsHardBounded(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shape := adaptiveRequestShapeFor([]byte(`{"messages":[{"content":"x"}]}`), candidate{Model: "claude-sonnet-5"})
	key := adaptiveProfileKey("bounded-auth", shape)
	for index := 0; index < 10_000; index++ {
		recordAdaptiveReservationCommitForKey("bounded-auth", key, 0.01, now)
	}
	adaptiveReserveRuntime.Lock()
	commitments := len(adaptiveReserveRuntime.Buckets[key].RecentCommitments)
	adaptiveReserveRuntime.Unlock()
	if commitments > adaptiveMaximumRecentCommitments {
		t.Fatalf("recent commitments = %d, cap = %d", commitments, adaptiveMaximumRecentCommitments)
	}

	adaptiveReserveRuntime.Lock()
	for index := 0; index < adaptiveMaximumProfileEntries+50; index++ {
		authIndex := fmt.Sprintf("bounded-%05d", index)
		bucketShape := shape
		bucketShape.ContextBucket = authIndex
		ensureAdaptiveBucketLocked(adaptiveProfileKey(authIndex, bucketShape), authIndex, bucketShape).UpdatedAt = now.Add(time.Duration(index) * time.Second)
	}
	profiles := len(adaptiveReserveRuntime.Buckets)
	adaptiveReserveRuntime.Unlock()
	if profiles > adaptiveMaximumProfileEntries {
		t.Fatalf("adaptive profiles = %d, cap = %d", profiles, adaptiveMaximumProfileEntries)
	}
	adaptiveReserveRuntime.Lock()
	for index := 0; index < adaptiveMaximumProfileEntries+50; index++ {
		ensureAdaptiveProfileLocked(fmt.Sprintf("aggregate-%05d", index)).UpdatedAt = now.Add(time.Duration(index) * time.Second)
	}
	aggregates := len(adaptiveReserveRuntime.Profiles)
	adaptiveReserveRuntime.Unlock()
	if aggregates > adaptiveMaximumProfileEntries {
		t.Fatalf("adaptive aggregates = %d, cap = %d", aggregates, adaptiveMaximumProfileEntries)
	}
	adaptiveReserveRuntime.Lock()
	for index := 0; index < adaptiveMaximumOverflowEntries+50; index++ {
		ensureAdaptiveOverflowLocked(fmt.Sprintf("overflow-%05d", index)).UpdatedAt = now.Add(time.Duration(index) * time.Second)
	}
	overflow := len(adaptiveReserveRuntime.Overflow)
	adaptiveReserveRuntime.Unlock()
	if overflow > adaptiveMaximumOverflowEntries {
		t.Fatalf("adaptive overflow profiles = %d, cap = %d", overflow, adaptiveMaximumOverflowEntries)
	}
}

func TestAdaptiveOverflowIsCredentialScopedAtSaturation(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Profiles["hot-auth"] = &adaptiveReserveProfile{
		AuthIndex: "hot-auth", LearnedScale: 6, ObservedBurnPerMin: 2, UpdatedAt: now.Add(-time.Hour),
	}
	for index := 1; index < adaptiveMaximumProfileEntries; index++ {
		authIndex := fmt.Sprintf("aggregate-%05d", index)
		adaptiveReserveRuntime.Profiles[authIndex] = &adaptiveReserveProfile{
			AuthIndex: authIndex, LearnedScale: 1, UpdatedAt: now.Add(time.Duration(index) * time.Second),
		}
	}
	ensureAdaptiveProfileLocked("new-auth")
	hotOverflow := adaptiveReserveRuntime.Overflow["hot-auth"]
	idleOverflow := adaptiveReserveRuntime.Overflow["idle-auth"]
	profiles, overflow := len(adaptiveReserveRuntime.Profiles), len(adaptiveReserveRuntime.Overflow)
	adaptiveReserveRuntime.Unlock()
	if hotOverflow == nil || hotOverflow.ObservedBurnPerMin != 2 || hotOverflow.LearnedScale != 6 {
		t.Fatalf("hot credential overflow was not retained: %#v", hotOverflow)
	}
	if idleOverflow != nil {
		t.Fatalf("idle credential received unrelated overflow: %#v", idleOverflow)
	}
	if profiles > adaptiveMaximumProfileEntries || overflow > adaptiveMaximumOverflowEntries {
		t.Fatalf("adaptive maps profiles=%d overflow=%d exceed caps", profiles, overflow)
	}
	cfg := defaultPluginConfig()
	cfg.QuotaUsageRefreshSeconds = 10 * 60
	quota := adaptivePersistenceQuota(80, now)
	if guard := adaptiveExposureGuard("hot-auth", "", quota, adaptiveWindowSession, cfg, now); guard <= 0 {
		t.Fatal("hot credential lost its scoped overflow burn guard")
	}
	if guard := adaptiveExposureGuard("idle-auth", "", quota, adaptiveWindowSession, cfg, now); guard <= 0 || guard >= 1 {
		t.Fatalf("idle credential cold-start guard = %.3f%%, want small nonzero isolated prior", guard)
	}
}

func TestAdaptiveOverflowEvictionKeepsCredentialFailClosed(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	adaptiveReserveRuntime.Lock()
	for index := 0; index < adaptiveMaximumOverflowEntries; index++ {
		authIndex := fmt.Sprintf("overflow-%05d", index)
		adaptiveReserveRuntime.Overflow[authIndex] = &adaptiveReserveProfile{
			AuthIndex: authIndex, LearnedScale: 2, ObservedBurnPerMin: 1,
			UpdatedAt: now.Add(time.Duration(index) * time.Second),
		}
	}
	ensureAdaptiveOverflowLocked("overflow-cap-plus-one")
	_, marked := adaptiveReserveRuntime.Saturated["overflow-00000"]
	overflowCount := len(adaptiveReserveRuntime.Overflow)
	adaptiveReserveRuntime.Unlock()
	if !marked {
		t.Fatal("oldest evicted overflow credential lost its uncertainty marker")
	}
	if overflowCount != adaptiveMaximumOverflowEntries {
		t.Fatalf("overflow entries = %d, want hard cap %d", overflowCount, adaptiveMaximumOverflowEntries)
	}
	cfg := defaultPluginConfig()
	quota := adaptivePersistenceQuota(80, now)
	if guard := adaptiveExposureGuard("overflow-00000", "", quota, adaptiveWindowSession, cfg, now); guard != 100 {
		t.Fatalf("revisited evicted credential guard = %.3f, want fail-closed 100", guard)
	}
	if guard := adaptiveExposureGuard("unrelated-auth", "", quota, adaptiveWindowSession, cfg, now); guard <= 0 || guard >= 1 {
		t.Fatalf("unrelated credential cold-start guard = %.3f, want small nonzero isolated prior", guard)
	}
}

func TestAdaptiveSaturationMarkerCapNeverReopensFirstCredential(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	adaptiveReserveRuntime.Lock()
	for index := 0; index < adaptiveMaximumOverflowEntries; index++ {
		markAdaptiveSaturatedLocked(fmt.Sprintf("evicted-%05d", index), now.Add(time.Duration(index)*time.Second))
	}
	markAdaptiveSaturatedLocked("evicted-cap-plus-one", now.Add(time.Hour))
	global := adaptiveReserveRuntime.SaturationGlobal
	markers := len(adaptiveReserveRuntime.Saturated)
	adaptiveReserveRuntime.Unlock()
	if !global {
		t.Fatal("cap+1 uncertainty marker did not enter global fail-closed mode")
	}
	if markers != adaptiveMaximumOverflowEntries {
		t.Fatalf("saturation markers = %d, want cap %d", markers, adaptiveMaximumOverflowEntries)
	}
	cfg := defaultPluginConfig()
	quota := adaptivePersistenceQuota(80, now)
	for _, authIndex := range []string{"evicted-00000", "evicted-cap-plus-one", "previously-unseen"} {
		if guard := adaptiveExposureGuard(authIndex, "", quota, adaptiveWindowSession, cfg, now); guard != 100 {
			t.Fatalf("credential %q reopened after saturation: guard %.3f", authIndex, guard)
		}
	}
}

func TestAdaptiveTTLNeverPrunesSaturationOrUnresolvedOverflow(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-adaptiveProfileStateTTL - time.Hour)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Saturated["ttl-saturated"] = stale
	adaptiveReserveRuntime.SaturationGlobal = true
	adaptiveReserveRuntime.Overflow["ttl-unresolved"] = &adaptiveReserveProfile{
		AuthIndex: "ttl-unresolved", LearnedScale: 4, ObservedBurnPerMin: 2,
		UnobservedPercent: 3, UpdatedAt: stale,
	}
	adaptiveReserveRuntime.Profiles["ttl-aggregate"] = &adaptiveReserveProfile{
		AuthIndex: "ttl-aggregate", LearnedScale: 4, ObservedBurnPerMin: 2,
		UnobservedPercent: 3, UpdatedAt: stale,
	}
	pruneAdaptiveProfilesLocked(now)
	_, markerPresent := adaptiveReserveRuntime.Saturated["ttl-saturated"]
	_, overflowPresent := adaptiveReserveRuntime.Overflow["ttl-unresolved"]
	_, aggregatePresent := adaptiveReserveRuntime.Profiles["ttl-aggregate"]
	global := adaptiveReserveRuntime.SaturationGlobal
	adaptiveReserveRuntime.Unlock()
	if !markerPresent || !global {
		t.Fatalf("TTL reopened estimator saturation: marker=%t global=%t", markerPresent, global)
	}
	if !overflowPresent || !aggregatePresent {
		t.Fatalf("TTL pruned unresolved estimator state: overflow=%t aggregate=%t", overflowPresent, aggregatePresent)
	}
	cfg := defaultPluginConfig()
	quota := adaptivePersistenceQuota(100, now)
	if guard := adaptiveExposureGuard("ttl-saturated", "", quota, adaptiveWindowSession, cfg, now); guard != 100 {
		t.Fatalf("TTL+1 saturated guard = %.3f, want 100 until authenticated reconciliation", guard)
	}
}

func TestAdaptiveSaturatedPrimaryUsesConservativeReservation(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Now().UTC()
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "evicted-primary"}
	shape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large"}
	adaptiveReserveRuntime.Lock()
	markAdaptiveSaturatedLocked(auth.AuthIndex, now)
	adaptiveReserveRuntime.Unlock()
	tariff := tariffConfig{ID: "x5", Multiplier: 5, ReservationPercent: 0.1}
	if got := adaptiveReservationForShape(auth, tariff, shape, now); got != adaptiveMaximumReservationPercent {
		t.Fatalf("saturated primary reservation = %.3f, want conservative maximum %.3f", got, adaptiveMaximumReservationPercent)
	}

	installAdaptiveTestQuota(t, auth.AuthIndex, 5, 5)
	previous := loadedConfig()
	cfg := previous
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x5", Multiplier: 5, SessionFloorPercent: 20, WeeklyFloorPercent: 20, ReservationPercent: 0.1}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
	if _, acquired := acquireAttemptLease(executionAttempt{
		Auth: auth, Primary: true, AllocatorManaged: true, TariffID: "x5",
		ReservationPercent: 0.1, AdaptiveBaselinePercent: 0.1, AdaptiveRequestShape: shape,
	}); acquired {
		t.Fatal("saturated primary with only 5% confirmed remaining bypassed conservative uncertainty")
	}
}

func TestAdaptiveConfirmedIdleIntervalsDecayBurnOnlyAfterRepeatedEvidence(t *testing.T) {
	window := adaptiveWindowEstimate{LearnedScale: 4, ObservedBurnPerMin: 2}
	for interval := 0; interval < 2; interval++ {
		updateAdaptiveWindow(&window, 0, 0, 10)
	}
	if window.ObservedBurnPerMin != 2 || window.LearnedScale != 4 {
		t.Fatalf("one or two idle confirmations decayed protection too early: %#v", window)
	}
	updateAdaptiveWindow(&window, 0, 0, 10)
	if window.ObservedBurnPerMin >= 2 || window.LearnedScale >= 4 {
		t.Fatalf("repeated confirmed idle intervals did not decay stale burn: %#v", window)
	}
}

func TestAdaptiveRefreshWatermarkPreservesCommitsCompletedAfterFetchStart(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	shapeBefore := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large"}
	shapeAfter := adaptiveRequestShape{Multiplier: 1, ModelFamily: "sonnet", EffortBucket: "standard", ContextBucket: "small"}
	beforeKey := adaptiveProfileKey("watermark-auth", shapeBefore)
	afterKey := adaptiveProfileKey("watermark-auth", shapeAfter)
	recordAdaptiveReservationCommitForKey("watermark-auth", beforeKey, 4, now.Add(-time.Minute))
	watermark := captureAdaptiveRefreshWatermark("watermark-auth")
	recordAdaptiveReservationCommitForKey("watermark-auth", beforeKey, 6, now.Add(time.Second))
	recordAdaptiveReservationCommitForKey("watermark-auth", afterKey, 2, now.Add(2*time.Second))

	previous := adaptivePersistenceQuota(80, now.Add(-10*time.Minute))
	refreshed := adaptivePersistenceQuota(76, now)
	observeAdaptiveQuotaRefresh("watermark-auth", previous, refreshed, 4, now, watermark)

	adaptiveReserveRuntime.Lock()
	before := *adaptiveReserveRuntime.Buckets[beforeKey]
	after := *adaptiveReserveRuntime.Buckets[afterKey]
	adaptiveReserveRuntime.Unlock()
	if math.Abs(before.UnobservedPercent-6) > 0.000001 {
		t.Fatalf("post-watermark work was cleared from existing bucket: %.3f", before.UnobservedPercent)
	}
	if math.Abs(after.UnobservedPercent-2) > 0.000001 {
		t.Fatalf("post-watermark work was cleared from new bucket: %.3f", after.UnobservedPercent)
	}
	if after.Session.ObservedBurnPerMin != 0 || after.Session.LearnedScale != 1 {
		t.Fatalf("post-watermark bucket learned from an earlier provider interval: %#v", after.Session)
	}
}

func TestAdaptiveRefreshDistributesObservedBurnAcrossBucketWeights(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	lightShape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "haiku", EffortBucket: "low", ContextBucket: "small"}
	heavyShape := adaptiveRequestShape{Multiplier: 1, ModelFamily: "fable", EffortBucket: "xhigh", ContextBucket: "large"}
	lightKey := adaptiveProfileKey("weighted-auth", lightShape)
	heavyKey := adaptiveProfileKey("weighted-auth", heavyShape)
	recordAdaptiveReservationCommitForKey("weighted-auth", lightKey, 2, now.Add(-time.Minute))
	recordAdaptiveReservationCommitForKey("weighted-auth", heavyKey, 8, now.Add(-time.Minute))
	watermark := captureAdaptiveRefreshWatermark("weighted-auth")

	previous := adaptivePersistenceQuota(90, now.Add(-10*time.Minute))
	refreshed := adaptivePersistenceQuota(70, now)
	observeAdaptiveQuotaRefresh("weighted-auth", previous, refreshed, 10, now, watermark)

	adaptiveReserveRuntime.Lock()
	lightRate := adaptiveReserveRuntime.Buckets[lightKey].Session.ObservedBurnPerMin
	heavyRate := adaptiveReserveRuntime.Buckets[heavyKey].Session.ObservedBurnPerMin
	aggregateRate := adaptiveReserveRuntime.Profiles["weighted-auth"].Session.ObservedBurnPerMin
	adaptiveReserveRuntime.Unlock()
	if math.Abs((lightRate+heavyRate)-2) > 0.000001 {
		t.Fatalf("bucket burn rates sum = %.3f, want provider-observed 2%%/min", lightRate+heavyRate)
	}
	if math.Abs(heavyRate/lightRate-4) > 0.000001 {
		t.Fatalf("burn attribution heavy/light = %.3f, want reservation weight ratio 4", heavyRate/lightRate)
	}
	if aggregateRate != 0 {
		t.Fatalf("aggregate double-counted %.3f%%/min after buckets received the full provider drop", aggregateRate)
	}
}

func installAdaptiveTestQuota(t *testing.T, authIndex string, session, weekly float64) {
	t.Helper()
	previousConfig := loadedConfig()
	bravoUsageState.mu.Lock()
	if bravoUsageState.state.Quotas == nil {
		bravoUsageState.state.Quotas = make(map[string]*credentialQuotaState)
	}
	_, hadQuota := bravoUsageState.state.Quotas[authIndex]
	previousQuota := credentialQuotaState{}
	if hadQuota {
		previousQuota = *bravoUsageState.state.Quotas[authIndex]
	}
	bravoUsageState.mu.Unlock()
	storeQuotaSnapshot(authIndex, credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: time.Now().UTC(),
		Session: quotaWindowState{RemainingPercent: session},
		Weekly:  quotaWindowState{RemainingPercent: weekly},
	})
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
		if hadQuota {
			storeQuotaSnapshot(authIndex, previousQuota)
		} else {
			bravoUsageState.mu.Lock()
			delete(bravoUsageState.state.Quotas, authIndex)
			bravoUsageState.mu.Unlock()
		}
	})
}
