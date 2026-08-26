package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveForecastBacktestUsesIndependentPreRequestWindowReservations(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	observedAt := now.Add(30 * time.Minute)
	previous := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: now,
		Session: quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(4 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 90, ResetAt: now.Add(6 * 24 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{
			Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 70, ResetAt: now.Add(6 * 24 * time.Hour)},
		}},
	}
	refreshed := credentialQuotaState{
		Confidence: "confirmed", Provider: "claude", ConfirmedAt: observedAt,
		Session: quotaWindowState{RemainingPercent: 79.4, ResetAt: now.Add(4 * time.Hour)},
		Weekly:  quotaWindowState{RemainingPercent: 89.95, ResetAt: now.Add(6 * 24 * time.Hour)},
		ModelWeekly: []modelQuotaWindowState{{
			Model: "fable", quotaWindowState: quotaWindowState{RemainingPercent: 69.6, ResetAt: now.Add(6 * 24 * time.Hour)},
		}},
	}
	commits := []adaptiveShadowCommit{
		{
			At: now.Add(5 * time.Minute), Percent: 0.3, SessionPercent: 0.3, WeeklyPercent: 0.05,
			ModelWeeklyPercent: 0.2, ModelWeeklyName: "fable", ProjectID: "prj_one",
			Provider: "claude", Model: "claude-fable-5",
			SessionCalibrated: true, WeeklyCalibrated: true, ModelCalibrated: true,
			EstimateConfidence: adaptiveForecastTokenCalibratedConfidence,
		},
		{
			At: now.Add(10 * time.Minute), Percent: 0.1, SessionPercent: 0.1, WeeklyPercent: 0.02,
			ModelWeeklyPercent: 0.1, ModelWeeklyName: "fable", ProjectID: "prj_two",
			Provider: "claude", Model: "claude-fable-5",
			SessionCalibrated: true, WeeklyCalibrated: true, ModelCalibrated: true,
			EstimateConfidence: adaptiveForecastTokenCalibratedConfidence,
		},
	}
	recordQuotaConsumptionReconciliation("private-auth-index", previous, refreshed, now, observedAt, commits)

	view := adaptiveForecastBacktestSummary([]string{"private-auth-index"}, observedAt.Add(time.Second))
	if view.PairedIntervals != 3 || len(view.Windows) != 3 || !view.WindowsIndependent {
		t.Fatalf("forecast summary = %#v", view)
	}
	session := findAdaptiveForecastWindow(t, view.Windows, pluginapi.HostAuthQuotaWindowKindSession, "")
	assertAdaptiveForecastValue(t, "session predicted", session.PredictedDropPercent, 0.4)
	assertAdaptiveForecastValue(t, "session actual", session.ActualDropPercent, 0.6)
	assertAdaptiveForecastValue(t, "session bias", session.MeanBiasPPPerInterval, 0.2)
	assertAdaptiveForecastValue(t, "session p95", session.UnderpredictionP95Percent, 0.2)
	assertAdaptiveForecastValue(t, "session maximum", session.MaximumUnderpredictionPercent, 0.2)
	if session.ConservativeCoveragePercent != 0 || session.UnderpredictionIntervals != 1 {
		t.Fatalf("session coverage = %#v", session)
	}

	weekly := findAdaptiveForecastWindow(t, view.Windows, pluginapi.HostAuthQuotaWindowKindWeekly, "")
	assertAdaptiveForecastValue(t, "weekly predicted", weekly.PredictedDropPercent, 0.07)
	assertAdaptiveForecastValue(t, "weekly actual", weekly.ActualDropPercent, 0.05)
	assertAdaptiveForecastValue(t, "weekly bias", weekly.MeanBiasPPPerInterval, -0.02)
	assertAdaptiveForecastValue(t, "weekly overprediction", weekly.OverpredictionPercent, 0.02)
	if weekly.ConservativeCoveragePercent != 100 || weekly.UnderpredictionP95Percent != 0 {
		t.Fatalf("weekly coverage = %#v", weekly)
	}

	modelWeekly := findAdaptiveForecastWindow(t, view.Windows, pluginapi.HostAuthQuotaWindowKindModelWeekly, "fable")
	assertAdaptiveForecastValue(t, "model-weekly predicted", modelWeekly.PredictedDropPercent, 0.3)
	assertAdaptiveForecastValue(t, "model-weekly actual", modelWeekly.ActualDropPercent, 0.4)
	analytics := collectQuotaConsumption(analyticsQuery{
		From: now, To: observedAt.Add(time.Second), Interval: analyticsIntervalHour,
	}, observedAt.Add(time.Second), true)
	analyticsSession := findQuotaConsumptionWindow(t, analytics.Windows, pluginapi.HostAuthQuotaWindowKindSession, "")
	if analyticsSession.Pool == nil || analyticsSession.Pool.ForecastBacktest == nil {
		t.Fatalf("management analytics omitted forecast backtest: %#v", analyticsSession.Pool)
	}
	assertAdaptiveForecastValue(t, "analytics session p95",
		analyticsSession.Pool.ForecastBacktest.UnderpredictionP95Percent, 0.2)

	encoded, errMarshal := json.Marshal(view)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), "private-auth-index") || strings.Contains(string(encoded), "prj_one") {
		t.Fatalf("public forecast view leaked identity: %s", encoded)
	}
}

func TestAdaptiveForecastBacktestRequiresCalibrationForTheSpecificWindow(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC)
	commit := adaptiveShadowCommit{
		At: now.Add(time.Minute), Percent: 0.2, SessionPercent: 0.2, WeeklyPercent: 0.3,
		Model: "claude-fable-5", SessionCalibrated: true, WeeklyCalibrated: false,
		EstimateConfidence: adaptiveForecastTokenCalibratedConfidence,
	}
	session, _ := buildQuotaConsumptionWindow(
		"auth", "claude", pluginapi.HostAuthQuotaWindowKindSession, "",
		quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(4 * time.Hour)},
		quotaWindowState{RemainingPercent: 79.8, ResetAt: now.Add(4 * time.Hour)},
		now, now.Add(10*time.Minute), []adaptiveShadowCommit{commit},
	)
	weekly, _ := buildQuotaConsumptionWindow(
		"auth", "claude", pluginapi.HostAuthQuotaWindowKindWeekly, "",
		quotaWindowState{RemainingPercent: 90, ResetAt: now.Add(6 * 24 * time.Hour)},
		quotaWindowState{RemainingPercent: 89.7, ResetAt: now.Add(6 * 24 * time.Hour)},
		now, now.Add(10*time.Minute), []adaptiveShadowCommit{commit},
	)
	if session.Counters.ForecastSamples != 1 || session.Counters.ForecastSkippedUncalibrated != 0 {
		t.Fatalf("calibrated session was not paired: %#v", session.Counters)
	}
	if weekly.Counters.ForecastSamples != 0 || weekly.Counters.ForecastSkippedUncalibrated != 1 {
		t.Fatalf("uncalibrated weekly estimate entered paired cohort: %#v", weekly.Counters)
	}
}

func TestAdaptiveForecastBacktestSkipsResetUnknownAndUncalibratedIntervals(t *testing.T) {
	now := time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC)
	legacy := adaptiveShadowCommit{
		At: now.Add(time.Minute), Percent: 1, SessionPercent: 0.2,
		Model: "claude-sonnet-5", EstimateConfidence: "shape_estimate",
	}
	uncalibrated, _ := buildQuotaConsumptionWindow(
		"auth", "claude", pluginapi.HostAuthQuotaWindowKindSession, "",
		quotaWindowState{RemainingPercent: 80, ResetAt: now.Add(4 * time.Hour)},
		quotaWindowState{RemainingPercent: 79.8, ResetAt: now.Add(4 * time.Hour)},
		now, now.Add(10*time.Minute), []adaptiveShadowCommit{legacy},
	)
	if uncalibrated.Counters.ForecastSamples != 0 || uncalibrated.Counters.ForecastSkippedUncalibrated != 1 {
		t.Fatalf("legacy interval entered calibrated cohort: %#v", uncalibrated.Counters)
	}

	noLocal, _ := buildQuotaConsumptionWindow(
		"auth", "claude", pluginapi.HostAuthQuotaWindowKindSession, "",
		quotaWindowState{RemainingPercent: 79.8, ResetAt: now.Add(4 * time.Hour)},
		quotaWindowState{RemainingPercent: 79.7, ResetAt: now.Add(4 * time.Hour)},
		now.Add(10*time.Minute), now.Add(20*time.Minute), nil,
	)
	if noLocal.Counters.ForecastSamples != 0 || noLocal.Counters.ForecastSkippedNoLocal != 1 {
		t.Fatalf("external-only interval entered paired cohort: %#v", noLocal.Counters)
	}

	reset, _ := buildQuotaConsumptionWindow(
		"auth", "claude", pluginapi.HostAuthQuotaWindowKindSession, "",
		quotaWindowState{RemainingPercent: 2, ResetAt: now.Add(5 * time.Minute)},
		quotaWindowState{RemainingPercent: 99, ResetAt: now.Add(5*time.Hour + 5*time.Minute)},
		now, now.Add(10*time.Minute), []adaptiveShadowCommit{{
			At: now.Add(time.Minute), Percent: 0.1, SessionPercent: 0.1,
			SessionCalibrated:  true,
			EstimateConfidence: adaptiveForecastTokenCalibratedConfidence,
		}},
	)
	if reset.Counters.SkippedResetOrIncreaseSamples != 1 || reset.Counters.ForecastSamples != 0 ||
		reset.Counters.ForecastSkippedNoLocal != 0 || reset.Counters.ForecastSkippedUncalibrated != 0 {
		t.Fatalf("reset interval entered forecast backtest: %#v", reset.Counters)
	}
}

func TestAdaptiveForecastBacktestPersistsAndBecomesAvailable(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for interval := 0; interval < adaptiveForecastMinimumIntervals; interval++ {
		previousAt := start.Add(time.Duration(interval) * 30 * time.Minute)
		observedAt := previousAt.Add(30 * time.Minute)
		remaining := 90 - float64(interval)*0.2
		previous := credentialQuotaState{
			Confidence: "confirmed", Provider: "claude", ConfirmedAt: previousAt,
			Session: quotaWindowState{RemainingPercent: remaining, ResetAt: start.Add(5 * time.Hour)},
			Weekly:  quotaWindowState{RemainingPercent: remaining, ResetAt: start.Add(7 * 24 * time.Hour)},
		}
		refreshed := credentialQuotaState{
			Confidence: "confirmed", Provider: "claude", ConfirmedAt: observedAt,
			Session: quotaWindowState{RemainingPercent: remaining - 0.2, ResetAt: start.Add(5 * time.Hour)},
			Weekly:  quotaWindowState{RemainingPercent: remaining - 0.2, ResetAt: start.Add(7 * 24 * time.Hour)},
		}
		// The session reset interval is deliberately excluded by the provider
		// watermark. Keep the session reset beyond the test horizon so all twelve
		// intervals form a clean six-hour cohort.
		previous.Session.ResetAt = start.Add(12 * time.Hour)
		refreshed.Session.ResetAt = start.Add(12 * time.Hour)
		recordQuotaConsumptionReconciliation(
			"persisted-auth", previous, refreshed, previousAt, observedAt,
			[]adaptiveShadowCommit{{
				At: previousAt.Add(time.Minute), Percent: 0.25,
				SessionPercent: 0.25, WeeklyPercent: 0.25,
				Model: "claude-sonnet-5", SessionCalibrated: true, WeeklyCalibrated: true,
				EstimateConfidence: adaptiveForecastTokenCalibratedConfidence,
			}},
		)
	}
	flushUsageState()
	loaded, errLoad := loadUsageStateFile(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	bravoUsageState.mu.Lock()
	bravoUsageState.state = loaded
	bravoUsageState.mu.Unlock()
	view := adaptiveForecastBacktestSummary([]string{"persisted-auth"}, start.Add(6*time.Hour+time.Second))
	if view.Status != "available" || view.ReadyWindows != 2 || view.PairedIntervals != 24 {
		t.Fatalf("persisted backtest did not become available: %#v", view)
	}
	for _, window := range view.Windows {
		if window.Status != "available" || window.PairedIntervals != adaptiveForecastMinimumIntervals ||
			window.CoverageSeconds != int64(adaptiveForecastMinimumCoverage/time.Second) ||
			window.ConservativeCoveragePercent != 100 {
			t.Fatalf("persisted window = %#v", window)
		}
		assertAdaptiveForecastValue(t, "persisted p95", window.UnderpredictionP95Percent, 0)
	}
	cfg := defaultPluginConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	public := adaptiveShadowSummary(cfg, []string{"persisted-auth"}, start.Add(6*time.Hour+time.Second))
	if public.RoutingEnforced || public.AdditionalProviderRequests || public.Effect != "shadow_only" {
		t.Fatalf("forecast persistence unexpectedly changed allocator authority: %#v", public)
	}
}

func TestNormalizeAdaptiveForecastCountersDropsImpossibleZeroSampleTotals(t *testing.T) {
	normalized := normalizeAdaptiveForecastCounters(quotaObservationCounters{
		ForecastSamples:          0,
		ForecastPredictedPercent: 12,
		ForecastActualPercent:    15,
	})
	if normalized.ForecastSamples != 0 || normalized.ForecastPredictedPercent != 0 ||
		normalized.ForecastActualPercent != 0 || normalized.ForecastUnderpredictionBuckets != nil {
		t.Fatalf("impossible zero-sample totals survived normalization: %#v", normalized)
	}
}

func findAdaptiveForecastWindow(
	t *testing.T,
	windows []adaptiveForecastBacktestWindowView,
	kind string,
	quotaModel string,
) adaptiveForecastBacktestWindowView {
	t.Helper()
	for _, window := range windows {
		if window.WindowKind == kind && window.QuotaModel == quotaModel {
			return window
		}
	}
	t.Fatalf("forecast window %s/%s not found: %#v", kind, quotaModel, windows)
	return adaptiveForecastBacktestWindowView{}
}

func assertAdaptiveForecastValue(t *testing.T, label string, got float64, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Fatalf("%s = %.6f, want %.6f", label, got, want)
	}
}
