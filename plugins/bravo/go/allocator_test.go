package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
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

func TestQuotaRefreshTTLPreventsPerRequestProviderPolling(t *testing.T) {
	now := time.Now().UTC()
	quota := credentialQuotaState{
		Confidence:  "confirmed",
		RefreshedAt: now.Add(-10 * time.Second),
		Dirty:       true,
	}
	if quotaNeedsRefresh(quota, time.Minute, now) {
		t.Fatal("dirty confirmed quota refreshed before the configured TTL")
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
	fetchQuotaSnapshot = func(_ string, _ pluginapi.HostAuthFileEntry) (credentialQuotaState, error) {
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
	if got := calls.Load(); got != 1 {
		t.Fatalf("quota refresh calls = %d, want 1 for a credential with one model-scoped failure", got)
	}

	setCooldown("claude", auth.ID, "", "account-wide", now.Add(time.Hour))
	refreshQuotaSnapshots("quota-account-scope-callback", []pluginapi.HostAuthFileEntry{auth}, true)
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
