package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaConsumptionAttributesConfirmedDropsWithoutSummingWindows(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	previous := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(4 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 90, ResetAt: now.Add(6 * 24 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{
			Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 50, ResetAt: now.Add(6 * 24 * time.Hour)},
		}},
	}
	observedAt := now.Add(time.Hour)
	refreshed := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: observedAt,
		Session: quotaWindowState{RemainingPercent: 70, ResetAt: now.Add(4 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 84, ResetAt: now.Add(6 * 24 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{
			Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 45, ResetAt: now.Add(6 * 24 * time.Hour)},
		}},
	}
	commits := []adaptiveShadowCommit{
		{At: now.Add(10 * time.Minute), Percent: 4, ProjectID: "prj_alpha", Provider: "claude", Model: "claude-fable-5", LogicalModel: "bravo/fable", Effort: "max", TariffID: "x5", Multiplier: 5},
		{At: now.Add(20 * time.Minute), Percent: 2, ProjectID: "prj_beta", Provider: "claude", Model: "claude-sonnet-4-5", LogicalModel: "bravo/sonnet", Effort: "low", TariffID: "x5", Multiplier: 5},
		{At: now.Add(30 * time.Minute), Percent: 1, Multiplier: 1},
	}
	recordQuotaConsumptionReconciliation("auth-private", previous, refreshed, now, observedAt, commits)

	view := collectQuotaConsumption(analyticsQuery{
		From: now, To: observedAt.Add(time.Second), Interval: analyticsIntervalHour,
	}, observedAt, true)
	if len(view.Windows) != 3 || !view.WindowsIndependent || view.Unit != "subscription_quota_percentage_points" {
		t.Fatalf("quota consumption view = %#v", view)
	}
	session := findQuotaConsumptionWindow(t, view.Windows, "session", "")
	if session.Pool == nil || session.Pool.ObservedDropPercent != 10 || session.Pool.AttributedProjectPercent != 6 ||
		session.Pool.AttributedLocalUnassignedPercent != 1 || session.Pool.ExternalOrEstimatorGapPercent != 3 {
		t.Fatalf("session attribution = %#v", session.Pool)
	}
	if len(session.Projects) != 2 || session.Projects[0].ProjectID != "prj_alpha" || session.Projects[0].Rank != 1 {
		t.Fatalf("session ranking = %#v", session.Projects)
	}
	if session.Projects[0].Models[0].Model != "claude-fable-5" || session.Projects[0].Models[0].Effort != "max" {
		t.Fatalf("model attribution = %#v", session.Projects[0].Models)
	}
	weekly := findQuotaConsumptionWindow(t, view.Windows, "weekly", "")
	if weekly.Pool == nil || math.Abs(weekly.Pool.AttributedProjectPercent-(6.0/7.0*6.0)) > 0.0002 || weekly.Pool.ExternalOrEstimatorGapPercent != 0 {
		t.Fatalf("weekly scaled attribution = %#v", weekly.Pool)
	}
	fable := findQuotaConsumptionWindow(t, view.Windows, "model_weekly", "fable")
	if fable.Pool == nil || fable.Pool.ObservedDropPercent != 5 || fable.Pool.AttributedProjectPercent != 4 ||
		fable.Pool.ExternalOrEstimatorGapPercent != 1 || len(fable.Projects) != 1 || fable.Projects[0].ProjectID != "prj_alpha" {
		t.Fatalf("Fable attribution = %#v", fable)
	}
	// The view has three separate windows; there is intentionally no grand
	// percentage that could misleadingly add 10 + 6 + 5 together.
	encoded, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), "auth-private") || strings.Contains(string(encoded), "total_percent") {
		t.Fatalf("public analytics leaked identity or a false cross-window total: %s", encoded)
	}
	projectOnly := collectQuotaConsumption(analyticsQuery{
		From: now, To: observedAt.Add(time.Second), Interval: analyticsIntervalHour, ProjectID: "prj_beta",
	}, observedAt, false)
	projectSession := findQuotaConsumptionWindow(t, projectOnly.Windows, "session", "")
	if projectSession.Pool != nil || len(projectSession.Projects) != 1 || projectSession.Projects[0].ProjectID != "prj_beta" || projectSession.Projects[0].Rank != 2 {
		t.Fatalf("project-scoped ranking/privacy = %#v", projectSession)
	}
	projectJSON, errProjectJSON := json.Marshal(projectOnly)
	if errProjectJSON != nil {
		t.Fatal(errProjectJSON)
	}
	if strings.Contains(string(projectJSON), "prj_alpha") || strings.Contains(string(projectJSON), "auth-private") {
		t.Fatalf("project-scoped quota view leaked neighbour or credential identity: %s", projectJSON)
	}
}

func TestQuotaConsumptionSkipsProviderResetAndPersistsProjectDimensions(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	previous := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 2, ResetAt: now.Add(30 * time.Minute)},
		Weekly:  quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(5 * 24 * time.Hour)},
	}
	refreshed := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now.Add(time.Hour),
		Session: quotaWindowState{RemainingPercent: 99, ResetAt: now.Add(5*time.Hour + 30*time.Minute)},
		Weekly:  quotaWindowState{RemainingPercent: 75, ResetAt: now.Add(5 * 24 * time.Hour)},
	}
	commits := []adaptiveShadowCommit{{
		At: now.Add(10 * time.Minute), Percent: 3, ProjectID: "prj_alpha", Provider: "claude",
		Model: "claude-fable-5", LogicalModel: "bravo/fable", Effort: "max", TariffID: "x20", Multiplier: 20,
	}}
	recordQuotaConsumptionReconciliation("auth-private", previous, refreshed, now, now.Add(time.Hour), commits)
	flushUsageState()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(raw), "brv_") {
		t.Fatal("quota attribution persisted a project credential")
	}
	reloaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	query := analyticsQuery{From: now, To: now.Add(2 * time.Hour), Interval: analyticsIntervalHour, ProjectID: "prj_alpha"}
	view := collectQuotaConsumptionLocked(&reloaded, query, now.Add(2*time.Hour), true)
	session := findQuotaConsumptionWindow(t, view.Windows, "session", "")
	if session.Confidence != "collecting" || len(session.Projects) != 0 || session.Pool == nil || session.Pool.SkippedResetOrIncreaseSamples != 1 {
		t.Fatalf("reset window was treated as consumption: %#v", session)
	}
	weekly := findQuotaConsumptionWindow(t, view.Windows, "weekly", "")
	if len(weekly.Projects) != 1 || weekly.Projects[0].AttributedPercent != 3 ||
		weekly.Projects[0].Plans[0].BaseX1EquivalentWindows != 0.6 {
		t.Fatalf("persisted weekly capacity = %#v", weekly)
	}
	cfg := defaultPluginConfig()
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: "auth-private", Tariff: "x20"}}
	annotateProjectQuotaCapacity(cfg, []pluginapi.HostAuthFileEntry{{AuthIndex: "auth-private", Provider: "claude"}}, &view)
	weekly = findQuotaConsumptionWindow(t, view.Windows, "weekly", "")
	if weekly.Projects[0].Plans[0].CurrentSubscriptions != 1 || weekly.Projects[0].Plans[0].SuggestedAction == "" {
		t.Fatalf("capacity annotation = %#v", weekly.Projects[0].Plans[0])
	}
}

func TestQuotaConsumptionAttributesSharedDropByTokenVolume(t *testing.T) {
	previousAt := time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC)
	observedAt := previousAt.Add(time.Hour)
	before := quotaWindowState{RemainingPercent: 80}
	after := quotaWindowState{RemainingPercent: 70}
	commits := []adaptiveShadowCommit{
		{At: previousAt.Add(time.Minute), Percent: 5, TokenUnits: 9000, ProjectID: "prj_large", Model: "claude-sonnet-5", Multiplier: 1},
		{At: previousAt.Add(2 * time.Minute), Percent: 5, TokenUnits: 1000, ProjectID: "prj_small", Model: "claude-sonnet-5", Multiplier: 1},
	}
	observation, projects := buildQuotaConsumptionWindow(
		"auth-private", "claude", pluginapi.HostAuthQuotaWindowKindSession, "",
		before, after, previousAt, observedAt, commits,
	)
	if len(projects) != 2 || observation.Counters.AttributedProjectPercent != 10 {
		t.Fatalf("token attribution = observation %#v projects %#v", observation, projects)
	}
	values := make(map[string]float64, len(projects))
	for _, project := range projects {
		values[project.ProjectID] = project.Counters.AttributedPercent
	}
	if values["prj_large"] != 9 || values["prj_small"] != 1 {
		t.Fatalf("drop was not apportioned 90/10 by tokens: %#v", values)
	}
}

func TestQuotaConsumptionHighConfidenceFlagsFableMaxAndCapacity(t *testing.T) {
	project := &quotaConsumptionProjectAccumulator{
		ProjectID: "prj_heavy",
		Counters: quotaProjectCounters{
			Commitments: 20, EstimatedPercent: 60, AttributedPercent: 50, BaseX1EquivalentPercent: 250,
		},
		Hourly: map[string]quotaProjectCounters{
			"2026-08-13T10:00:00Z": {AttributedPercent: 20},
		},
		Models: map[string]*quotaConsumptionModelAccumulator{
			"fable": {
				Provider: "claude", Model: "claude-fable-5", LogicalModel: "bravo/fable", Effort: "max", TariffID: "x5",
				Counters: quotaProjectCounters{Commitments: 16, AttributedPercent: 40, BaseX1EquivalentPercent: 200},
			},
			"sonnet": {
				Provider: "claude", Model: "claude-sonnet-4-5", LogicalModel: "bravo/sonnet", Effort: "medium", TariffID: "x5",
				Counters: quotaProjectCounters{Commitments: 4, AttributedPercent: 10, BaseX1EquivalentPercent: 50},
			},
		},
		Plans: map[string]*quotaConsumptionPlanAccumulator{
			"x5": {
				TariffID: "x5", Multiplier: 5,
				Counters: quotaProjectCounters{Commitments: 20, AttributedPercent: 50, BaseX1EquivalentPercent: 250},
				Hourly:   map[string]quotaProjectCounters{"2026-08-13T10:00:00Z": {AttributedPercent: 20}},
			},
		},
	}
	view := buildQuotaConsumptionProjectView(
		&quotaConsumptionWindowAccumulator{Provider: "claude", Kind: "weekly"},
		project, 100, 10, "high",
	)
	if len(view.Signals) == 0 || !strings.Contains(view.Signals[0], "Fable") || !strings.Contains(view.Signals[0], "max") {
		t.Fatalf("optimization signals = %#v", view.Signals)
	}
	if len(view.Plans) != 1 || view.Plans[0].EstimatedSubscriptionsAtAveragePace != 8.4 ||
		view.Plans[0].EstimatedSubscriptionsAtPeakPace != 33.6 {
		t.Fatalf("capacity estimate = %#v", view.Plans)
	}
}

func TestQuotaConsumptionPartialFirstHourNeverReportsPeakBelowAverage(t *testing.T) {
	project := &quotaConsumptionProjectAccumulator{
		ProjectID: "prj_partial",
		Counters:  quotaProjectCounters{Commitments: 7, AttributedPercent: 3},
		Hourly: map[string]quotaProjectCounters{
			"2026-08-14T01:00:00Z": {Commitments: 7, AttributedPercent: 3},
		},
		Models: map[string]*quotaConsumptionModelAccumulator{},
		Plans: map[string]*quotaConsumptionPlanAccumulator{
			"x5": {
				TariffID: "x5", Multiplier: 5,
				Counters: quotaProjectCounters{Commitments: 7, AttributedPercent: 3},
				Hourly: map[string]quotaProjectCounters{
					"2026-08-14T01:00:00Z": {Commitments: 7, AttributedPercent: 3},
				},
			},
		},
	}

	view := buildQuotaConsumptionProjectView(
		&quotaConsumptionWindowAccumulator{Provider: "claude", Kind: pluginapi.HostAuthQuotaWindowKindSession},
		project, 3, 0.25, "collecting",
	)

	if view.AveragePPPerHour != 12 || view.PeakHourlyPP != 12 {
		t.Fatalf("partial-hour average/peak = %.2f/%.2f, want 12/12", view.AveragePPPerHour, view.PeakHourlyPP)
	}
	if len(view.Plans) != 1 || view.Plans[0].EstimatedSubscriptionsAtAveragePace != 0.6 ||
		view.Plans[0].EstimatedSubscriptionsAtPeakPace != 0.6 {
		t.Fatalf("partial-hour capacity estimate = %#v", view.Plans)
	}
}

func TestQuotaConsumptionCapacityUsesEachProjectsAllowedPool(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	storeQuotaSnapshot("auth-alpha", credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	})
	storeQuotaSnapshot("auth-beta", credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80}, Weekly: quotaWindowState{RemainingPercent: 80},
	})
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{
		{ID: "prj_alpha", Enabled: boolPointer(true), AllowedAuthIDs: []string{"auth-alpha"}},
		{ID: "prj_beta", Enabled: boolPointer(true), AllowedAuthIDs: []string{"auth-beta"}},
	}
	cfg.Subscriptions = []subscriptionConfig{
		{AuthIndex: "auth-alpha", Tariff: "x1"},
		{AuthIndex: "auth-beta", Tariff: "x1"},
	}
	view := quotaConsumptionAnalyticsView{Windows: []quotaConsumptionWindowView{{
		Provider: "claude", Kind: pluginapi.HostAuthQuotaWindowKindWeekly, Confidence: "high",
		Projects: []quotaConsumptionProjectView{
			{ProjectID: "prj_alpha", Plans: []quotaConsumptionPlanCapacityView{{TariffID: "x1", Multiplier: 1}}},
			{ProjectID: "prj_beta", Plans: []quotaConsumptionPlanCapacityView{{TariffID: "x1", Multiplier: 1}}},
		},
	}}}
	auths := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "auth-alpha", Provider: "claude"},
		{AuthIndex: "auth-beta", Provider: "claude"},
	}

	annotateProjectQuotaCapacity(cfg, auths, &view)

	for _, project := range view.Windows[0].Projects {
		if got := project.Plans[0].CurrentSubscriptions; got != 1 {
			t.Fatalf("project %s capacity used the global pool: got %d, want 1", project.ProjectID, got)
		}
	}
}

func findQuotaConsumptionWindow(t *testing.T, values []quotaConsumptionWindowView, kind, model string) quotaConsumptionWindowView {
	t.Helper()
	for _, value := range values {
		if value.Kind == kind && value.QuotaModel == model {
			return value
		}
	}
	t.Fatalf("quota window %s/%s missing from %#v", kind, model, values)
	return quotaConsumptionWindowView{}
}
