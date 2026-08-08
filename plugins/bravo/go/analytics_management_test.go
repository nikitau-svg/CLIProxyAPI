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

func TestUsageStateV1MigrationPreservesTotalsQuotasAndBuildsDailyBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	raw := `{
		"schema_version": 1,
		"auth_totals": {
			"private-auth-index": {
				"total": {"requests": 3, "input_tokens": 12, "total_tokens": 18},
				"hourly": {
					"2026-07-20T10:00:00Z": {"requests": 3, "input_tokens": 12, "total_tokens": 18}
				}
			}
		},
		"project_totals": {
			"prj_alpha": {
				"total": {"requests": 2, "input_tokens": 7, "total_tokens": 11},
				"hourly": {
					"2026-07-20T10:00:00Z": {"requests": 2, "input_tokens": 7, "total_tokens": 11}
				}
			}
		},
		"quotas": {
			"private-auth-index": {
				"confidence": "confirmed",
				"provider": "claude",
				"plan": "team",
				"weekly": {"used_percent": 23, "remaining_percent": 77}
			}
		},
		"updated_at": "2026-07-20T11:00:00Z"
	}`
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	state, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if state.SchemaVersion != 4 {
		t.Fatalf("schema version = %d, want 4", state.SchemaVersion)
	}
	if state.AuthTotals["private-auth-index"].Total.TotalTokens != 18 ||
		state.ProjectTotals["prj_alpha"].Total.TotalTokens != 11 ||
		state.GlobalTotal.Total.TotalTokens != 18 {
		t.Fatalf("migration lost totals: %#v", state)
	}
	if got := state.ProjectTotals["prj_alpha"].Daily["2026-07-20"]; got.TotalTokens != 11 || got.Requests != 2 {
		t.Fatalf("migrated daily bucket = %#v", got)
	}
	quota := state.Quotas["private-auth-index"]
	if quota == nil || quota.Plan != "team" || quota.Weekly.RemainingPercent != 77 {
		t.Fatalf("migration lost quota = %#v", quota)
	}
	if len(state.ProjectSubscriptionModelTotals) != 0 {
		t.Fatal("v1 migration fabricated an unrecoverable project/subscription/model correlation")
	}
	if state.DimensionalStartedAt.IsZero() {
		t.Fatal("v1 migration did not mark dimensional coverage start")
	}
}

func TestUsageStateKeepsHourlyAndDailyRetentionWithoutLosingTotals(t *testing.T) {
	store := usageStateStore{state: newPersistedUsageState()}
	recent := time.Date(2026, 7, 20, 10, 15, 0, 0, time.UTC)
	recordAt := func(at time.Time, tokens int64) {
		store.record(pluginapi.UsageRecord{
			Provider:    "anthropic",
			Model:       "claude-opus-4-8",
			Alias:       "bravo/opus",
			APIKey:      "bravo:prj_alpha",
			AuthIndex:   "private-auth-index",
			RequestedAt: at,
			Detail: pluginapi.UsageDetail{
				InputTokens:     tokens - 2,
				OutputTokens:    1,
				ReasoningTokens: 1,
			},
		})
	}
	recordAt(recent.Add(-401*24*time.Hour), 3)
	recordAt(recent.Add(-32*24*time.Hour), 5)
	recordAt(recent, 7)
	recordAt(recent.Add(65*time.Minute), 11)

	aggregate := store.state.ProjectTotals["prj_alpha"]
	if aggregate == nil {
		t.Fatal("project aggregate was not created")
	}
	if aggregate.Total.TotalTokens != 26 || aggregate.Total.Requests != 4 {
		t.Fatalf("all-time total = %#v", aggregate.Total)
	}
	if len(aggregate.Hourly) != 2 {
		t.Fatalf("hourly buckets = %#v, want two recent buckets", aggregate.Hourly)
	}
	if len(aggregate.Daily) != 2 {
		t.Fatalf("daily buckets = %#v, want 32-day-old and current days", aggregate.Daily)
	}
	if got := aggregate.Daily["2026-07-20"]; got.TotalTokens != 18 || got.Requests != 2 {
		t.Fatalf("current daily bucket = %#v", got)
	}
	if got := store.state.ProviderTotals["claude"]; got == nil || got.Total.TotalTokens != 26 {
		t.Fatalf("provider aggregation = %#v", got)
	}
	if len(store.state.ModelTotals) != 1 || len(store.state.ProjectSubscriptionModelTotals) != 1 {
		t.Fatalf("dimensional maps = models:%d cross:%d", len(store.state.ModelTotals), len(store.state.ProjectSubscriptionModelTotals))
	}
}

func TestUsageStateReconfigureSamePathFlushesWithoutDiscardingAnalytics(t *testing.T) {
	store := usageStateStore{}
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := store.configure(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	recordAnalyticsUsage(&store, at, "prj_alpha", "private-auth-index", "claude", "opus", "bravo/opus", 13)
	if errConfigure := store.configure(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if got := store.state.ProjectTotals["prj_alpha"]; got == nil || got.Total.TotalTokens != 13 {
		t.Fatalf("same-path reconfigure discarded analytics: %#v", got)
	}
	reloaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if got := reloaded.ProjectTotals["prj_alpha"]; got == nil || got.Total.TotalTokens != 13 {
		t.Fatalf("same-path reconfigure did not flush analytics: %#v", got)
	}
}

func TestAnalyticsCrossDimensionAndSubscriptionRedaction(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()

	at := time.Date(2026, 7, 23, 12, 20, 0, 0, time.UTC)
	recordAnalyticsUsage(bravoUsageState, at, "prj_alpha", "auth-alpha-secret", "anthropic", "claude-opus-4-8", "bravo/opus", 10)
	recordAnalyticsUsage(bravoUsageState, at.Add(time.Hour), "prj_alpha", "auth-beta-secret", "openai", "gpt-5.6-codex", "bravo/opus", 20)
	recordAnalyticsUsage(bravoUsageState, at.Add(2*time.Hour), "prj_beta", "auth-beta-secret", "openai", "gpt-5.6-codex", "bravo/opus", 30)

	query := analyticsQuery{
		From:      at.Truncate(24 * time.Hour),
		To:        at.Truncate(24 * time.Hour).Add(24 * time.Hour),
		Interval:  analyticsIntervalHour,
		ProjectID: "prj_alpha",
	}
	response := collectAnalytics(query, at.Add(3*time.Hour))
	if response.Summary.TotalTokens != 30 || response.Summary.Requests != 2 {
		t.Fatalf("project summary = %#v", response.Summary)
	}
	if response.Summary.ReasoningTokens != 2 ||
		response.Summary.CacheReadTokens != 2 ||
		response.Summary.CacheCreationTokens != 2 {
		t.Fatalf("project token detail = %#v", response.Summary)
	}
	if response.Summary.AverageLatencyMS != 2000 ||
		!strings.Contains(response.MetricSemantics.AverageLatencyMS, "full attempt duration") {
		t.Fatalf("latency contract = %#v / %#v", response.Summary, response.MetricSemantics)
	}
	if len(response.Breakdown.Subscriptions) != 2 ||
		len(response.Breakdown.Models) != 2 ||
		len(response.Breakdown.ProjectSubscriptionModels) != 2 {
		t.Fatalf("cross breakdown = %#v", response.Breakdown)
	}
	if len(response.SubscriptionTimeline) != 2 ||
		response.SubscriptionTimeline[0].SubscriptionID != analyticsSubscriptionID("auth-alpha-secret") ||
		response.SubscriptionTimeline[1].SubscriptionID != analyticsSubscriptionID("auth-beta-secret") {
		t.Fatalf("subscription timeline = %#v", response.SubscriptionTimeline)
	}
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), "auth-alpha-secret") || strings.Contains(string(encoded), "auth-beta-secret") {
		t.Fatal("analytics response exposed a raw auth index")
	}
	subscriptionID := analyticsSubscriptionID("auth-beta-secret")
	filtered := query
	filtered.SubscriptionID = subscriptionID
	filtered.Provider = "codex"
	filteredResponse := collectAnalytics(filtered, at.Add(3*time.Hour))
	if filteredResponse.Summary.TotalTokens != 20 || len(filteredResponse.Breakdown.ProjectSubscriptionModels) != 1 {
		t.Fatalf("project/subscription/provider filter = %#v", filteredResponse)
	}
	if filteredResponse.Breakdown.Subscriptions[0].AuthIndex != subscriptionID {
		t.Fatal("auth_index compatibility field is not redacted")
	}
}

func TestAnalyticsSubscriptionTimelineUsesPresentationWithoutPersistingIt(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()

	at := time.Date(2026, 7, 25, 9, 10, 0, 0, time.UTC)
	recordAnalyticsUsage(bravoUsageState, at, "prj_alpha", "auth-alpha-secret", "anthropic", "opus", "bravo/opus", 10)
	recordAnalyticsUsage(bravoUsageState, at.Add(20*time.Minute), "prj_alpha", "auth-alpha-secret", "anthropic", "sonnet", "bravo/sonnet", 20)
	recordAnalyticsUsage(bravoUsageState, at.Add(time.Hour), "prj_alpha", "auth-alpha-secret", "anthropic", "sonnet", "bravo/sonnet", 30)

	query := analyticsQuery{
		From:      at.Truncate(time.Hour),
		To:        at.Truncate(time.Hour).Add(2 * time.Hour),
		Interval:  analyticsIntervalHour,
		ProjectID: "prj_alpha",
	}
	presentations := map[string]subscriptionPresentation{
		"auth-alpha-secret": {
			DisplayName: "Рабочая подписка",
			Note:        "Рабочая подписка",
			Email:       "member@example.com",
			Workspace:   "Workspace A",
			Provider:    "claude",
		},
	}
	response := collectAnalyticsWithPresentations(query, at.Add(2*time.Hour), presentations)
	if len(response.SubscriptionTimeline) != 2 {
		t.Fatalf("timeline = %#v, want one compact point per used hour", response.SubscriptionTimeline)
	}
	first := response.SubscriptionTimeline[0]
	if first.Usage.Requests != 2 || first.Usage.TotalTokens != 30 ||
		first.DisplayName != "Рабочая подписка" || first.Note != "Рабочая подписка" ||
		first.Email != "member@example.com" || first.Workspace != "Workspace A" {
		t.Fatalf("first timeline point = %#v", first)
	}
	if response.SubscriptionTimeline[1].Usage.Requests != 1 ||
		response.SubscriptionTimeline[1].Usage.TotalTokens != 30 {
		t.Fatalf("second timeline point = %#v", response.SubscriptionTimeline[1])
	}
	if len(response.Breakdown.Subscriptions) != 1 ||
		response.Breakdown.Subscriptions[0].DisplayName != "Рабочая подписка" {
		t.Fatalf("subscription presentation = %#v", response.Breakdown.Subscriptions)
	}
	encodedState, errMarshal := json.Marshal(bravoUsageState.state)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, presentationValue := range []string{"Рабочая подписка", "member@example.com", "Workspace A"} {
		if strings.Contains(string(encodedState), presentationValue) {
			t.Fatalf("persisted analytics state unexpectedly contains presentation %q", presentationValue)
		}
	}
	encodedResponse, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encodedResponse), "auth-alpha-secret") {
		t.Fatal("timeline response exposed a raw auth index")
	}
}

func TestAnalyticsManagementQueryContractAndValidation(t *testing.T) {
	restore := isolateBravoUsageState(t)
	defer restore()

	at := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Hour)
	recordAnalyticsUsage(bravoUsageState, at, "prj_alpha", "auth-secret", "anthropic", "claude-opus-4-8", "bravo/opus", 17)
	query := url.Values{
		"from":     []string{at.Format(time.RFC3339)},
		"to":       []string{at.Add(3 * time.Hour).Format(time.RFC3339)},
		"interval": []string{"hour"},
		"filters":  []string{`{"project_id":"prj_alpha","provider":"claude"}`},
	}
	status, body := callAnalyticsManagement(t, http.MethodGet, query)
	if status != http.StatusOK {
		t.Fatalf("analytics status = %d body=%s", status, body)
	}
	if strings.Contains(body, "auth-secret") {
		t.Fatal("query API exposed raw credential identity")
	}
	var response analyticsResponse
	if errUnmarshal := json.Unmarshal([]byte(body), &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.Interval != "hour" || response.Bucket != "hour" ||
		response.Summary.TotalTokens != 17 || len(response.Series) != 3 {
		t.Fatalf("analytics response = %#v", response)
	}
	if response.BreakdownCoverageFrom == nil {
		t.Fatal("analytics response omitted truthful dimensional coverage")
	}

	status, _ = callAnalyticsManagement(t, http.MethodGet, url.Values{"interval": []string{"minute"}})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid interval status = %d", status)
	}
	status, _ = callAnalyticsManagement(t, http.MethodGet, url.Values{"subscription_id": []string{"auth-secret"}})
	if status != http.StatusBadRequest {
		t.Fatalf("raw subscription identity status = %d", status)
	}
	status, _ = callAnalyticsManagement(t, http.MethodPost, nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("non-GET analytics status = %d", status)
	}
}

func TestAnalyticsDatePickerTreatsTodayAsInclusiveWithoutFutureError(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 30, 0, 0, time.UTC)
	query, errParse := parseAnalyticsQuery(url.Values{
		"from": []string{"2026-07-23"},
		"to":   []string{"2026-07-24"},
	}, now)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !query.To.Equal(now) {
		t.Fatalf("inclusive current date to = %s, want %s", query.To, now)
	}
}

func TestAnalyticsRouteIsAuthenticatedManagementOnly(t *testing.T) {
	raw, errRegister := registerManagement()
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var registration managementRegistrationResponse
	if errUnmarshal := json.Unmarshal(env.Result, &registration); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	found := false
	for _, route := range registration.Routes {
		if route.Method == http.MethodGet && route.Path == "/bravo/analytics" {
			found = true
		}
	}
	if !found {
		t.Fatal("authenticated analytics management route is missing")
	}
	for _, resource := range registration.Resources {
		if strings.Contains(resource.Path, "analytics") {
			t.Fatalf("analytics was exposed as an unauthenticated resource: %#v", resource)
		}
	}
}

func TestSubscriptionViewExposesStableAnalyticsJoinID(t *testing.T) {
	const authIndex = "private-auth-index"
	view := buildSubscriptionView(
		defaultPluginConfig(),
		pluginapi.HostAuthFileEntry{AuthIndex: authIndex, Provider: "claude"},
		subscriptionConfig{AuthIndex: authIndex, Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)
	if view.AnalyticsID == "" || view.AnalyticsID != analyticsSubscriptionID(authIndex) {
		t.Fatalf("analytics join id = %q", view.AnalyticsID)
	}
	if strings.Contains(view.AnalyticsID, authIndex) || !validAnalyticsSubscriptionID(view.AnalyticsID) {
		t.Fatalf("analytics join id is not safely redacted: %q", view.AnalyticsID)
	}
}

func isolateBravoUsageState(t testing.TB) func() {
	t.Helper()
	bravoUsageState.mu.Lock()
	previousPath := bravoUsageState.path
	previousState := bravoUsageState.state
	previousTimer := bravoUsageState.saveTimer
	previousPendingSince := bravoUsageState.savePendingSince
	if previousTimer != nil {
		previousTimer.Stop()
	}
	bravoUsageState.path = ""
	bravoUsageState.state = newPersistedUsageState()
	bravoUsageState.saveTimer = nil
	bravoUsageState.savePendingSince = time.Time{}
	bravoUsageState.mu.Unlock()
	return func() {
		waitAdaptiveWALIdleForTest(t)
		bravoUsageState.mu.Lock()
		if bravoUsageState.saveTimer != nil {
			bravoUsageState.saveTimer.Stop()
		}
		bravoUsageState.path = previousPath
		bravoUsageState.state = previousState
		bravoUsageState.saveTimer = previousTimer
		bravoUsageState.savePendingSince = previousPendingSince
		bravoUsageState.mu.Unlock()
	}
}

func recordAnalyticsUsage(
	store *usageStateStore,
	at time.Time,
	projectID, authIndex, provider, model, logicalModel string,
	totalTokens int64,
) {
	store.record(pluginapi.UsageRecord{
		Provider:    provider,
		Model:       model,
		Alias:       logicalModel,
		APIKey:      "bravo:" + projectID,
		AuthIndex:   authIndex,
		RequestedAt: at,
		Latency:     2 * time.Second,
		Detail: pluginapi.UsageDetail{
			InputTokens:         totalTokens - 5,
			OutputTokens:        2,
			ReasoningTokens:     1,
			CacheReadTokens:     1,
			CacheCreationTokens: 1,
			TotalTokens:         totalTokens,
		},
	})
}

func callAnalyticsManagement(t *testing.T, method string, query url.Values) (int, string) {
	t.Helper()
	rawRequest, errMarshal := json.Marshal(rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{
			Method: method,
			Path:   "/v0/management/bravo/analytics",
			Query:  query,
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errHandle := handleManagement(rawRequest)
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
	return response.StatusCode, string(response.Body)
}
