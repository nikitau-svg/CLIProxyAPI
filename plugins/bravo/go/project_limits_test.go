package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProjectLimitsReturnsConfirmedResetsUsageAndHourlyCache(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	resetProjectLimitsCacheForTest()
	t.Cleanup(resetProjectLimitsCacheForTest)

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	previousNow := projectLimitsNow
	projectLimitsNow = func() time.Time { return now }
	t.Cleanup(func() { projectLimitsNow = previousNow })

	const plaintext = "brv_project_limits_test"
	cfg := projectLimitsTestConfig(t, plaintext)
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	storeQuotaSnapshot("claude-private", credentialQuotaState{
		Confidence:  "confirmed",
		Provider:    "claude",
		Session:     quotaWindowState{RemainingPercent: 0, UsedPercent: 100, ResetAt: now.Add(3 * time.Hour)},
		Weekly:      quotaWindowState{RemainingPercent: 10, UsedPercent: 90, ResetAt: now.Add(30 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{Model: "claude-fable-5", quotaWindowState: quotaWindowState{RemainingPercent: 0, UsedPercent: 100, ResetAt: now.Add(3 * time.Hour)}}},
		RefreshedAt: now,
		ConfirmedAt: now,
	})
	storeQuotaSnapshot("codex-private", credentialQuotaState{
		Confidence:  "confirmed",
		Provider:    "codex",
		Session:     quotaWindowState{RemainingPercent: 80, UsedPercent: 20, ResetAt: now.Add(4 * time.Hour)},
		Weekly:      quotaWindowState{RemainingPercent: 70, UsedPercent: 30, ResetAt: now.Add(48 * time.Hour)},
		RefreshedAt: now,
		ConfirmedAt: now,
	})
	recordAnalyticsUsage(bravoUsageState, now.Add(-24*time.Hour), "prj_limits", "claude-private", "claude", "claude-fable-5", "bravo/fable", 123)
	recordAdaptiveShadowCommit("claude-private", 2, now)
	recordAdaptiveShadowCommit("outside-private", 9, now)
	adaptiveEdgeGateRuntime.Lock()
	adaptiveEdgeGateRuntime.InFlight["claude-private"] = adaptiveEdgeGateLease{StartedAt: now}
	adaptiveEdgeGateRuntime.InFlight["outside-private"] = adaptiveEdgeGateLease{StartedAt: now}
	adaptiveEdgeGateRuntime.Breakers[adaptiveEdgeGateBreakerKey("claude", "claude-private", "claude-fable-5")] = adaptiveEdgeGateBreaker{
		AuthIndex: "claude-private", Provider: "claude", Model: "claude-fable-5", Until: now.Add(time.Minute),
	}
	adaptiveEdgeGateRuntime.Breakers[adaptiveEdgeGateBreakerKey("claude", "outside-private", "claude-fable-5")] = adaptiveEdgeGateBreaker{
		AuthIndex: "outside-private", Provider: "claude", Model: "claude-fable-5", Until: now.Add(time.Minute),
	}
	adaptiveEdgeGateRuntime.Unlock()

	hostCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("unexpected host method %q", method)
		}
		hostCalls++
		return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "claude-private", Provider: "claude", Name: "must-not-leak@example.test"},
			{AuthIndex: "codex-private", Provider: "codex", Name: "also-secret@example.test"},
			{AuthIndex: "outside-private", Provider: "claude", Name: "outside@example.test"},
		}}), nil
	})

	status, headers, body := callProjectKeyEndpoint(t, http.MethodGet, projectLimitsPath, plaintext, url.Values{"format": {"json"}})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if !strings.Contains(headers.Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q", headers.Get("Content-Type"))
	}
	if headers.Get("X-Bravo-Cache") != "MISS" {
		t.Fatalf("cache header = %q, want MISS", headers.Get("X-Bravo-Cache"))
	}
	var response projectLimitsResponse
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.Project.ID != "prj_limits" || response.Usage.Summary.Requests != 1 || response.Usage.Summary.TotalTokens != 123 {
		t.Fatalf("response = %#v", response)
	}
	if response.Cached || response.NextRefreshAt != now.Add(5*time.Minute) || response.RefreshSeconds != 300 {
		t.Fatalf("fresh cache metadata = %#v", response)
	}
	if response.SchemaVersion != 3 || response.AdaptiveAllocator.Mode != "observe" ||
		response.AdaptiveAllocator.RoutingEnforced || response.AdaptiveAllocator.AdditionalProviderRequests {
		t.Fatalf("adaptive project view = %#v", response.AdaptiveAllocator)
	}
	if response.AdaptiveAllocator.TrackedAccounts != 1 || response.AdaptiveAllocator.RawPendingPercent != 2 {
		t.Fatalf("adaptive project scope = %#v, want only allowed account aggregate", response.AdaptiveAllocator)
	}
	if response.AdaptiveAllocator.EdgeGate.Mode != "observe" ||
		response.AdaptiveAllocator.EdgeGate.RoutingEnforced ||
		response.AdaptiveAllocator.EdgeGate.QueuesRequests ||
		response.AdaptiveAllocator.EdgeGate.AdditionalProviderRequests ||
		response.AdaptiveAllocator.EdgeGate.InFlightGuards != 1 ||
		response.AdaptiveAllocator.EdgeGate.TrackedBreakers != 1 {
		t.Fatalf("edge gate project scope = %#v", response.AdaptiveAllocator.EdgeGate)
	}
	claude := findProjectLimitProvider(t, response.Providers, "claude")
	if claude.Status == "available" || claude.AccountsTotal != 1 {
		t.Fatalf("claude = %#v", claude)
	}
	fable := findProjectLimitWindow(t, claude.Windows, pluginapi.HostAuthQuotaWindowKindModelWeekly, "claude-fable-5")
	if fable.ResetsInSeconds == nil || *fable.ResetsInSeconds != int64(3*time.Hour/time.Second) {
		t.Fatalf("fable reset = %#v", fable)
	}
	codex := findProjectLimitProvider(t, response.Providers, "codex")
	if codex.Status != "available" || codex.AccountsTotal != 1 {
		t.Fatalf("codex = %#v", codex)
	}
	if raw := string(body); strings.Contains(raw, "private") || strings.Contains(raw, "example.test") {
		t.Fatalf("response leaked account identity: %s", raw)
	}

	status, headers, body = callProjectKeyEndpoint(t, http.MethodGet, projectLimitsPath, plaintext, url.Values{"format": {"text"}})
	if status != http.StatusOK || !strings.Contains(headers.Get("Content-Type"), "text/plain") {
		t.Fatalf("text status=%d headers=%v body=%s", status, headers, body)
	}
	if headers.Get("X-Bravo-Cache") != "HIT" || hostCalls != 1 {
		t.Fatalf("cached request headers=%v hostCalls=%d", headers, hostCalls)
	}
	for _, expected := range []string{"Проект Bravo: Limits test", "Источник: кэш", "всегда возвращает этот результат с HTTP 200", "сброс через 3 ч", "Адаптивный allocator: observe", "дополнительных запросов к подпискам: нет", "Usage за 30 дней: 1 запросов"} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("text body %q does not contain %q", body, expected)
		}
	}
	if raw := string(body); strings.Contains(raw, "private") || strings.Contains(raw, "example.test") {
		t.Fatalf("text response leaked account identity: %s", raw)
	}
	status, headers, body = callProjectKeyEndpoint(t, http.MethodGet, projectLimitsPath, plaintext, url.Values{"format": {"json"}})
	if status != http.StatusOK || headers.Get("X-Bravo-Cache") != "HIT" || hostCalls != 1 {
		t.Fatalf("cached JSON status=%d headers=%v hostCalls=%d body=%s", status, headers, hostCalls, body)
	}
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !response.Cached || !response.GeneratedAt.Equal(now) {
		t.Fatalf("cached JSON response = %#v", response)
	}

	now = now.Add(5*time.Minute + time.Second)
	status, headers, body = callProjectKeyEndpoint(t, http.MethodGet, projectLimitsPath, plaintext, url.Values{"format": {"json"}})
	if status != http.StatusOK || headers.Get("X-Bravo-Cache") != "MISS" || hostCalls != 2 {
		t.Fatalf("refreshed request status=%d headers=%v hostCalls=%d body=%s", status, headers, hostCalls, body)
	}
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.Cached || !response.GeneratedAt.Equal(now) {
		t.Fatalf("refreshed response = %#v", response)
	}
}

func TestProjectLimitsReportsProjectEffectiveAssistMode(t *testing.T) {
	now := time.Now().UTC()
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "assist"
	project := smartKeyConfig{ID: "assist-opt-out", Name: "Opt out"}
	response := buildProjectLimitsResponse(cfg, project, nil, now, now.Add(5*time.Minute))
	if response.AdaptiveAllocator.Mode != "breaker" || response.AdaptiveAllocator.SoftAssistEnabled ||
		response.AdaptiveAllocator.ForecastRoutingEnforced {
		t.Fatalf("opt-out limits leaked global assist: %#v", response.AdaptiveAllocator)
	}
	project.AdaptiveAssist = true
	response = buildProjectLimitsResponse(cfg, project, nil, now, now.Add(5*time.Minute))
	if response.AdaptiveAllocator.Mode != "assist" || !response.AdaptiveAllocator.SoftAssistEnabled ||
		response.AdaptiveAllocator.ForecastRoutingEnforced {
		t.Fatalf("opt-in limits hid effective assist: %#v", response.AdaptiveAllocator)
	}
}

func TestProjectLimitsRejectsInvalidFormatBeforeHostIO(t *testing.T) {
	resetProjectLimitsCacheForTest()
	t.Cleanup(resetProjectLimitsCacheForTest)
	const plaintext = "brv_project_limits_invalid_format"
	cfg := projectLimitsTestConfig(t, plaintext)
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	hostCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		hostCalls++
		return nil, nil
	})
	status, _, body := callProjectKeyEndpoint(t, http.MethodGet, projectLimitsPath, plaintext, url.Values{"format": {"xml"}})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "bravo_project_limits_format_invalid") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if hostCalls != 0 {
		t.Fatalf("invalid format performed %d host calls", hostCalls)
	}
}

func TestProjectLimitsCacheCoalescesConcurrentRefresh(t *testing.T) {
	resetProjectLimitsCacheForTest()
	t.Cleanup(resetProjectLimitsCacheForTest)
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	if _, hit, wait := lookupProjectLimitsCache("same-project", now); hit || wait != nil {
		t.Fatalf("first lookup hit=%v wait=%v", hit, wait != nil)
	}
	if _, hit, wait := lookupProjectLimitsCache("same-project", now); hit || wait == nil {
		t.Fatalf("concurrent lookup hit=%v wait=%v", hit, wait != nil)
	}
	response := projectLimitsResponse{Object: "bravo.project_limits", GeneratedAt: now}
	finishProjectLimitsCache("same-project", response, now.Add(5*time.Minute), true)
	got, hit, wait := lookupProjectLimitsCache("same-project", now)
	if !hit || wait != nil || got.GeneratedAt != now {
		t.Fatalf("cached lookup got=%#v hit=%v wait=%v", got, hit, wait != nil)
	}
}

func TestProjectEndpointsRejectNonProjectKeyBeforeHostIO(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	hostCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		hostCalls++
		return nil, nil
	})
	for _, path := range []string{projectLimitsPath, projectRoutesPath} {
		status, _, body := callProjectKeyEndpoint(t, http.MethodGet, path, "ordinary-api-key", nil)
		if status != http.StatusUnauthorized || !strings.Contains(string(body), "bravo_smart_key_required") {
			t.Fatalf("%s status=%d body=%s", path, status, body)
		}
	}
	if hostCalls != 0 {
		t.Fatalf("unauthorized endpoint performed %d host calls", hostCalls)
	}
}

func TestProjectRoutesReturnsOnlyAllowedEffectiveRoutes(t *testing.T) {
	const plaintext = "brv_project_routes_test"
	cfg := projectLimitsTestConfig(t, plaintext)
	cfg.SmartKeys[0].Models = []string{"fable"}
	cfg.RouteOverrides = []routeOverrideConfig{{ID: "fable", Candidates: []candidate{{Provider: "codex", Model: "gpt-5.6-sol", Effort: "max", Priority: 100}}}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	status, _, body := callProjectKeyEndpoint(t, http.MethodGet, projectRoutesPath, plaintext, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var response projectRoutesResponse
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(response.Routes) != 1 || response.Routes[0].ID != "fable" || response.Routes[0].Source != "override" {
		t.Fatalf("routes = %#v", response.Routes)
	}
	if response.SchemaVersion != 2 || response.Policy.AdaptiveAllocatorMode != "observe" ||
		response.Policy.AdaptiveRoutingEnforced || response.Policy.AdaptiveAdditionalProviderRequests {
		t.Fatalf("adaptive route policy = %#v", response.Policy)
	}
	if got := response.Routes[0].Candidates; len(got) != 1 || got[0].Order != 1 || got[0].Provider != "codex" || got[0].PhysicalModel != "gpt-5.6-sol" {
		t.Fatalf("candidates = %#v", got)
	}
	if raw := string(body); strings.Contains(raw, "claude-private") || strings.Contains(raw, "codex-private") {
		t.Fatalf("routes leaked account identity: %s", raw)
	}
}

func TestProjectQuotaAndContextFailureNamesResetsAndAvailableCodex(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cfg := projectLimitsTestConfig(t, "brv_failure_summary_test")
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	storeQuotaSnapshot("claude-private", credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now, RefreshedAt: now,
		Session: quotaWindowState{RemainingPercent: 0, ResetAt: now.Add(3 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 0, ResetAt: now.Add(30 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{
			Model:            "claude-fable-5",
			quotaWindowState: quotaWindowState{RemainingPercent: 0, ResetAt: now.Add(3 * time.Hour)},
		}},
	})
	storeQuotaSnapshot("codex-private", credentialQuotaState{
		Confidence: "confirmed", Provider: "codex", ConfirmedAt: now, RefreshedAt: now,
		Session: quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(4 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 70, ResetAt: now.Add(48 * time.Hour)},
	})
	traces := []executionFailureTrace{
		{Provider: "claude", Model: "claude-fable-5", Failure: executionFailure{Code: "bravo_subscription_quota_exhausted", Status: 429, Retryable: true}},
		{Provider: "codex", Model: "gpt-5.6-terra", Failure: executionFailure{
			Code: "bravo_context_window_exceeded", Status: 400,
			Provider: &providererror.Detail{RequiredTokens: 314000, LimitTokens: 256000},
		}},
	}
	failure := enrichProjectQuotaContextFailure(cfg.SmartKeys[0], traces, executionFailure{}, now)
	if failure.Code != "bravo_quota_then_context_exhausted" || failure.Status != http.StatusBadRequest || failure.Retryable {
		t.Fatalf("failure = %#v", failure)
	}
	for _, expected := range []string{"Fable 5", "3 ч", "30 ч", "Лимиты Codex доступны", "Terra", "314 000", "256 000", "/v1/bravo/limits"} {
		if !strings.Contains(failure.Message, expected) {
			t.Fatalf("message %q does not contain %q", failure.Message, expected)
		}
	}
}

func projectLimitsTestConfig(t *testing.T, plaintext string) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	sum := sha256.Sum256([]byte(plaintext))
	cfg.SmartKeys = []smartKeyConfig{{
		ID:             "prj_limits",
		Name:           "Limits test",
		SHA256:         hex.EncodeToString(sum[:]),
		Models:         []string{"fable", "terra"},
		AllowedAuthIDs: []string{"claude-private", "codex-private"},
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	return cfg
}

func callProjectKeyEndpoint(t *testing.T, method, path, plaintext string, query url.Values) (int, http.Header, []byte) {
	t.Helper()
	raw, errHandle := handleManagement(mustJSONValue(t, rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{
			Method:  method,
			Path:    path,
			Headers: http.Header{"Authorization": []string{"Bearer " + plaintext}},
			Query:   query,
		},
		HostCallbackID: "project-api-callback",
	}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return response.StatusCode, response.Headers, response.Body
}

func findProjectLimitProvider(t *testing.T, providers []projectLimitProviderView, name string) projectLimitProviderView {
	t.Helper()
	for _, provider := range providers {
		if provider.Provider == name {
			return provider
		}
	}
	t.Fatalf("provider %q not found in %#v", name, providers)
	return projectLimitProviderView{}
}

func findProjectLimitWindow(t *testing.T, windows []projectLimitWindowView, kind, model string) projectLimitWindowView {
	t.Helper()
	for _, window := range windows {
		if window.Kind == kind && window.Model == model {
			return window
		}
	}
	t.Fatalf("window %s/%s not found in %#v", kind, model, windows)
	return projectLimitWindowView{}
}
