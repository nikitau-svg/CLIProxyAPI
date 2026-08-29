package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveAllocatorConfigAllowsEnforceAndRejectsAssist(t *testing.T) {
	cfg := defaultPluginConfig()
	if cfg.AdaptiveAllocatorMode != "observe" ||
		cfg.AdaptiveCoolingHalfLifeSeconds != 5*60 ||
		cfg.AdaptiveCoolingMaxAgeSeconds != 30*60 {
		t.Fatalf("adaptive defaults = %#v", cfg)
	}
	enforce := cfg
	enforce.AdaptiveAllocatorMode = "enforce"
	if errNormalize := normalizeConfig(&enforce); errNormalize != nil || enforce.AdaptiveAllocatorMode != "enforce" {
		t.Fatalf("enforce mode rejected: %v (%q)", errNormalize, enforce.AdaptiveAllocatorMode)
	}
	invalidMode := cfg
	invalidMode.AdaptiveAllocatorMode = "assist"
	if errNormalize := normalizeConfig(&invalidMode); errNormalize == nil || !strings.Contains(errNormalize.Error(), "not supported") {
		t.Fatalf("assist mode error = %v, want unsupported rejection", errNormalize)
	}
	invalid := cfg
	invalid.AdaptiveCoolingHalfLifeSeconds = 30
	if errNormalize := normalizeConfig(&invalid); errNormalize == nil {
		t.Fatal("normalizeConfig accepted a 30-second adaptive half-life")
	}
}

func TestAdaptiveShadowEstimateUsesRequestShapeWithoutWeakeningTariff(t *testing.T) {
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := defaultPluginConfig()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "shadow-shape"}
	tariff := tariffByID(cfg, "x5")
	small := buildAdaptiveShadowRequestFeatures([]byte(`{"max_tokens":64,"messages":[]}`))
	large := buildAdaptiveShadowRequestFeatures([]byte(`{"max_tokens":1048576,"messages":[]}`))
	haiku := adaptiveShadowEstimateFor(cfg, auth, candidate{Model: "claude-haiku-4-5-20251001", Effort: "low"}, tariff, credentialQuotaState{}, small, now)
	fable := adaptiveShadowEstimateFor(cfg, auth, candidate{Model: "claude-fable-5", Effort: "max"}, tariff, credentialQuotaState{}, large, now)
	if haiku.ReservationPercent < tariff.ReservationPercent {
		t.Fatalf("haiku reservation %.4f weakened tariff %.4f", haiku.ReservationPercent, tariff.ReservationPercent)
	}
	if fable.ReservationPercent <= haiku.ReservationPercent {
		t.Fatalf("large Fable %.4f <= small Haiku %.4f", fable.ReservationPercent, haiku.ReservationPercent)
	}
	if fable.ReservationPercent > adaptiveShadowMaximumReservationPercent {
		t.Fatalf("Fable reservation %.4f exceeds cap", fable.ReservationPercent)
	}
}

func TestAdaptiveShadowOutputBudgetTrustsOnlyExactTopLevelInteger(t *testing.T) {
	valid := map[string]float64{
		`{"max_tokens":64,"messages":[]}`:             64,
		`{"max_output_tokens":128,"input":"ok"}`:      128,
		`{"max_completion_tokens":256,"messages":[]}`: 256,
	}
	for body, want := range valid {
		got, trusted := adaptiveShadowDeclaredOutputTokens([]byte(body))
		if !trusted || got != want {
			t.Errorf("valid %s = %.0f/%v, want %.0f/true", body, got, trusted, want)
		}
	}
	invalid := []string{
		`{"messages":[{"content":"quoted \"max_tokens\":1"}]}`,
		`{"tools":[{"input_schema":{"max_tokens":1}}]}`,
		`{"\u006dax_tokens":1}`,
		`{"max_tokens":1e5}`,
		`{"max_tokens":1.5}`,
		`{"max_tokens":"64"}`,
		`{"messages":[]}`,
	}
	for _, body := range invalid {
		if got, trusted := adaptiveShadowDeclaredOutputTokens([]byte(body)); trusted || got != adaptiveShadowMaximumOutputTokens {
			t.Errorf("untrusted %s = %.0f/%v", body, got, trusted)
		}
	}
}

func TestAdaptiveShadowCommitAndLearningCoolToZeroAtHardAge(t *testing.T) {
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := defaultPluginConfig()
	cfg.AdaptiveCoolingHalfLifeSeconds = 60
	cfg.AdaptiveCoolingMaxAgeSeconds = 300
	now := time.Date(2026, 8, 13, 11, 0, 0, 0, time.UTC)
	recordAdaptiveShadowCommit("cooling", 8, now)
	adaptiveShadowRuntime.Lock()
	adaptiveShadowRuntime.Accounts["cooling"].LearnedScale = 5
	adaptiveShadowRuntime.Accounts["cooling"].LearnedAt = now
	adaptiveShadowRuntime.Unlock()

	half := adaptiveShadowSummary(cfg, []string{"cooling"}, now.Add(time.Minute))
	if math.Abs(half.EffectivePendingPercent-4) > 0.0001 {
		t.Fatalf("half-life effective pending = %.4f, want 4", half.EffectivePendingPercent)
	}
	if math.Abs(half.MaximumLearnedScale-3) > 0.0001 {
		t.Fatalf("half-life learned scale = %.4f, want 3", half.MaximumLearnedScale)
	}
	expired := adaptiveShadowSummary(cfg, []string{"cooling"}, now.Add(301*time.Second))
	if expired.EffectivePendingPercent != 0 || expired.RawPendingPercent != 0 ||
		expired.MaximumLearnedScale != 1 || expired.TrackedAccounts != 0 {
		t.Fatalf("expired state did not fully cool: %#v", expired)
	}
}

func TestAdaptiveShadowStateIsStrictlyBounded(t *testing.T) {
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for index := 0; index < adaptiveShadowMaximumCommitsPerAccount+100; index++ {
		recordAdaptiveShadowCommit("bounded", 0.1, now.Add(time.Duration(index)*time.Millisecond))
	}
	adaptiveShadowRuntime.Lock()
	bounded := adaptiveShadowRuntime.Accounts["bounded"]
	if len(bounded.Commits) > adaptiveShadowMaximumCommitsPerAccount || bounded.OverflowPercent <= 0 {
		adaptiveShadowRuntime.Unlock()
		t.Fatalf("commit bound = %d overflow=%.2f", len(bounded.Commits), bounded.OverflowPercent)
	}
	adaptiveShadowRuntime.Unlock()
	for index := 1; index < adaptiveShadowMaximumAccounts; index++ {
		recordAdaptiveShadowCommit("bounded-"+strconv.Itoa(index), 0.1, now)
	}
	recordAdaptiveShadowCommit("must-be-dropped", 1, now)
	adaptiveShadowRuntime.Lock()
	accounts, saturated, dropped := len(adaptiveShadowRuntime.Accounts), adaptiveShadowRuntime.Saturated, adaptiveShadowRuntime.DroppedAccounts
	_, leaked := adaptiveShadowRuntime.Accounts["must-be-dropped"]
	adaptiveShadowRuntime.Unlock()
	if accounts != adaptiveShadowMaximumAccounts || !saturated || dropped != 1 || leaked {
		t.Fatalf("accounts=%d saturated=%v dropped=%d leaked=%v", accounts, saturated, dropped, leaked)
	}
}

func TestAdaptiveShadowReconcilesOnlyCoveredCommitments(t *testing.T) {
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := defaultPluginConfig()
	t0 := time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC)
	previous := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: t0,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	}
	recordAdaptiveShadowCommit("watermark", 1, t0.Add(time.Second))
	recordAdaptiveShadowCommit("watermark", 2, t0.Add(3*time.Second))
	refreshed := credentialQuotaState{
		Confidence: "confirmed", ConfirmedAt: t0.Add(2 * time.Second),
		Session: quotaWindowState{RemainingPercent: 76}, Weekly: quotaWindowState{RemainingPercent: 79},
	}
	reconcileAdaptiveShadow(cfg, "watermark", previous, refreshed, refreshed.ConfirmedAt)
	view := adaptiveShadowSummary(cfg, []string{"watermark"}, refreshed.ConfirmedAt)
	if view.RawPendingPercent != 2 || view.MaximumLearnedScale != 4 {
		t.Fatalf("reconciled view = %#v, want post-watermark=2 and learned=4", view)
	}
	// The same provider observation cannot clear a later commit.
	reconcileAdaptiveShadow(cfg, "watermark", refreshed, refreshed, refreshed.ConfirmedAt)
	if got := adaptiveShadowSummary(cfg, []string{"watermark"}, refreshed.ConfirmedAt).RawPendingPercent; got != 2 {
		t.Fatalf("equal observation cleared pending: %.4f", got)
	}
}

func TestAdaptiveShadowObservePreservesAttemptsAndAddsNoProviderIO(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	project := smartKeyConfig{ID: "shadow-project", PrimaryAuthIDs: []string{"auth-a"}}
	auths := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "auth-a", Provider: "claude"},
		{AuthIndex: "auth-b", Provider: "codex"},
	}
	attempts := []executionAttempt{
		{Candidate: candidate{Provider: "claude", Model: "claude-fable-5", Effort: "max"}, Auth: auths[0]},
		{Candidate: candidate{Provider: "codex", Model: "gpt-5.6-sol", Effort: "max"}, Auth: auths[1]},
	}
	before := append([]executionAttempt(nil), attempts...)
	providerCalls := 0
	previousFetch := fetchQuotaSnapshot
	fetchQuotaSnapshot = func(string, pluginapi.HostAuthFileEntry, string) (credentialQuotaState, error) {
		providerCalls++
		return credentialQuotaState{}, nil
	}
	t.Cleanup(func() { fetchQuotaSnapshot = previousFetch })
	now := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	features := buildAdaptiveShadowRequestFeatures([]byte(`{"max_tokens":4096,"messages":[]}`))
	got := annotateAdaptiveShadowPlan(cfg, project, auths, attempts, features, now)
	if len(got) != len(before) {
		t.Fatalf("attempt count changed: %d != %d", len(got), len(before))
	}
	for index := range got {
		if got[index].Auth.AuthIndex != before[index].Auth.AuthIndex || got[index].AllocatorManaged != before[index].AllocatorManaged {
			t.Fatalf("attempt %d routing changed: before=%#v after=%#v", index, before[index], got[index])
		}
		if !got[index].AdaptiveShadow || got[index].AdaptiveReservationPercent <= 0 {
			t.Fatalf("attempt %d lacks shadow estimate: %#v", index, got[index])
		}
	}
	baseReleased := false
	lease := wrapAdaptiveShadowLease(got[0], func(bool) { baseReleased = true })
	lease(true)
	_ = adaptiveShadowSummary(cfg, []string{"auth-a"}, now)
	if !baseReleased || providerCalls != 0 {
		t.Fatalf("baseReleased=%v providerCalls=%d", baseReleased, providerCalls)
	}
}

func TestAdaptiveShadowBuildPlanMatchesOffAndPerformsOnlyAuthListIO(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	const plaintext = "brv_shadow_plan_parity"
	sum := sha256.Sum256([]byte(plaintext))
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-shadow", AuthIndex: "shadow-claude", Provider: "claude"},
		{ID: "codex-shadow", AuthIndex: "shadow-codex", Provider: "codex"},
	}
	authListCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("adaptive shadow caused unexpected host/provider call %q", method)
		}
		authListCalls++
		return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
	})
	base := defaultPluginConfig()
	base.AllocatorMode = "off"
	base.Models = map[string]logicalModel{"shadow": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Effort: "max", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-5.6-sol", Effort: "max", Priority: 90, Capabilities: []string{capabilityText}},
	}}}
	base.SmartKeys = []smartKeyConfig{{
		ID: "shadow-project", Name: "Shadow", SHA256: hex.EncodeToString(sum[:]), Models: []string{"*"},
		AllowedAuthIDs: []string{"shadow-claude", "shadow-codex"},
	}}
	previous := loadedConfig()
	t.Cleanup(func() { currentConfig.Store(previous) })
	build := func(mode string) []executionAttempt {
		cfg := base
		cfg.AdaptiveAllocatorMode = mode
		if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
			t.Fatal(errNormalize)
		}
		currentConfig.Store(cfg)
		plan, errPlan := buildExecutionPlan(
			rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "bravo/shadow", Headers: http.Header{"Authorization": []string{"Bearer " + plaintext}},
				OriginalRequest: []byte(`{"model":"bravo/shadow","max_tokens":4096,"messages":[]}`),
			}, HostCallbackID: "shadow-plan-parity"},
			"shadow", cfg.Models["shadow"], requestCapabilityContract{Protocol: protocolOpenAI, Capabilities: newCapabilitySet(capabilityText)},
		)
		if errPlan != nil {
			t.Fatal(errPlan)
		}
		return plan
	}
	off := build("off")
	observe := build("observe")
	if authListCalls != 2 {
		t.Fatalf("host auth list calls = %d, want one per plan and no quota/provider calls", authListCalls)
	}
	if len(off) != len(observe) || len(off) != 2 {
		t.Fatalf("off=%#v observe=%#v", off, observe)
	}
	for index := range off {
		if off[index].Auth.AuthIndex != observe[index].Auth.AuthIndex ||
			off[index].Candidate.Provider != observe[index].Candidate.Provider ||
			off[index].Candidate.Model != observe[index].Candidate.Model ||
			off[index].AllocatorManaged != observe[index].AllocatorManaged {
			t.Fatalf("routing changed at %d: off=%#v observe=%#v", index, off[index], observe[index])
		}
		if off[index].AdaptiveShadow || !observe[index].AdaptiveShadow {
			t.Fatalf("shadow markers at %d: off=%#v observe=%#v", index, off[index], observe[index])
		}
	}
}

func TestAdaptiveShadowLargeRequestScanAllocatesNothing(t *testing.T) {
	prefix := []byte(`{"messages":[{"role":"user","content":"`)
	body := make([]byte, 0, len(prefix)+(1<<20)+64)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte("x"), 1<<20)...)
	body = append(body, []byte(`"}],"max_tokens":65536}`)...)
	allocations := testing.AllocsPerRun(50, func() {
		features := buildAdaptiveShadowRequestFeatures(body)
		if !features.OutputTrusted || features.EstimatedTokens <= 0 {
			panic("invalid features")
		}
	})
	if allocations != 0 {
		t.Fatalf("1 MiB shadow scan allocations = %.2f, want 0", allocations)
	}
}

func BenchmarkAdaptiveShadowRequestFeatures4MiB(b *testing.B) {
	prefix := []byte(`{"messages":[{"role":"user","content":"`)
	body := make([]byte, 0, len(prefix)+(4<<20)+64)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte("x"), 4<<20)...)
	body = append(body, []byte(`"}],"max_tokens":65536}`)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = buildAdaptiveShadowRequestFeatures(body)
	}
}
