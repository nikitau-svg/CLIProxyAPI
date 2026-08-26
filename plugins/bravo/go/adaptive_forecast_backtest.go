package main

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	adaptiveForecastBacktestPeriod            = 7 * 24 * time.Hour
	adaptiveForecastMinimumIntervals          = 12
	adaptiveForecastMinimumCoverage           = 6 * time.Hour
	adaptiveForecastTokenCalibratedConfidence = "token_calibrated_complete"
)

var adaptiveForecastUnderpredictionBounds = [...]float64{
	0, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2,
	0.5, 1, 2, 5, 10, 20, 50, 100,
}

// adaptiveForecastBacktestPublicView contains only aggregate quota-window
// statistics. Credential and project identities never enter the public view.
type adaptiveForecastBacktestPublicView struct {
	Status                       string                               `json:"status"`
	PeriodHours                  int                                  `json:"period_hours"`
	WindowsIndependent           bool                                 `json:"windows_independent"`
	ReadyWindows                 int                                  `json:"ready_windows"`
	PairedIntervals              int64                                `json:"paired_intervals"`
	SkippedUncalibratedIntervals int64                                `json:"skipped_uncalibrated_intervals"`
	SkippedNoLocalIntervals      int64                                `json:"skipped_no_local_intervals"`
	Windows                      []adaptiveForecastBacktestWindowView `json:"windows,omitempty"`
	Note                         string                               `json:"note"`
}

type adaptiveForecastBacktestWindowView struct {
	Provider                       string  `json:"provider"`
	WindowKind                     string  `json:"window_kind"`
	QuotaModel                     string  `json:"quota_model,omitempty"`
	Status                         string  `json:"status"`
	PairedIntervals                int64   `json:"paired_intervals"`
	SkippedUncalibratedIntervals   int64   `json:"skipped_uncalibrated_intervals"`
	SkippedNoLocalIntervals        int64   `json:"skipped_no_local_intervals"`
	CoverageSeconds                int64   `json:"coverage_seconds"`
	PredictedDropPercent           float64 `json:"predicted_drop_percent"`
	ActualDropPercent              float64 `json:"actual_drop_percent"`
	MeanPredictedPPPerInterval     float64 `json:"mean_predicted_pp_per_interval"`
	MeanActualPPPerInterval        float64 `json:"mean_actual_pp_per_interval"`
	MeanBiasPPPerInterval          float64 `json:"mean_bias_pp_per_interval"`
	MeanAbsoluteErrorPPPerInterval float64 `json:"mean_absolute_error_pp_per_interval"`
	UnderpredictionPercent         float64 `json:"underprediction_percent"`
	OverpredictionPercent          float64 `json:"overprediction_percent"`
	UnderpredictionIntervals       int64   `json:"underprediction_intervals"`
	OverpredictionIntervals        int64   `json:"overprediction_intervals"`
	ConservativeCoveragePercent    float64 `json:"conservative_coverage_percent"`
	UnderpredictionP95Percent      float64 `json:"underprediction_p95_percent"`
	MaximumUnderpredictionPercent  float64 `json:"maximum_underprediction_percent"`
}

func adaptiveShadowCommitPercentForWindow(
	commit adaptiveShadowCommit,
	kind string,
	quotaModel string,
) (float64, bool, bool) {
	kind = strings.TrimSpace(kind)
	if commit.WindowKind != "" && commit.WindowKind != kind {
		return 0, false, false
	}
	percent := commit.Percent
	calibrated := false
	switch kind {
	case pluginapi.HostAuthQuotaWindowKindSession:
		calibrated = commit.SessionCalibrated
		if commit.SessionPercent > 0 {
			percent = commit.SessionPercent
		}
	case pluginapi.HostAuthQuotaWindowKindWeekly:
		calibrated = commit.WeeklyCalibrated
		if commit.WeeklyPercent > 0 {
			percent = commit.WeeklyPercent
		}
	case pluginapi.HostAuthQuotaWindowKindModelWeekly:
		calibrated = commit.ModelCalibrated
		model := strings.TrimSpace(firstNonEmpty(commit.ModelWeeklyName, commit.Model))
		if model == "" && commit.WindowKind != pluginapi.HostAuthQuotaWindowKindModelWeekly {
			return 0, false, false
		}
		if model != "" && !quotaModelMatches(model, quotaModel) {
			return 0, false, false
		}
		if commit.ModelWeeklyPercent > 0 {
			percent = commit.ModelWeeklyPercent
		}
	default:
		return 0, false, false
	}
	if percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, false, false
	}
	return percent, true, calibrated
}

func recordAdaptiveForecastObservation(
	counters *quotaObservationCounters,
	predicted float64,
	actual float64,
	coverageSeconds int64,
	matchingCommits int,
	allTokenCalibrated bool,
) {
	if counters == nil {
		return
	}
	if matchingCommits == 0 {
		counters.ForecastSkippedNoLocal++
		return
	}
	if !allTokenCalibrated {
		counters.ForecastSkippedUncalibrated++
		return
	}
	if !adaptiveForecastFiniteNonNegative(predicted) || !adaptiveForecastFiniteNonNegative(actual) {
		counters.ForecastSkippedUncalibrated++
		return
	}
	counters.ForecastSamples++
	counters.ForecastCoverageSeconds += maxInt64(coverageSeconds, 0)
	counters.ForecastPredictedPercent += predicted
	counters.ForecastActualPercent += actual
	errorPercent := actual - predicted
	counters.ForecastSignedErrorPercent += errorPercent
	counters.ForecastAbsoluteErrorPercent += math.Abs(errorPercent)
	underprediction := math.Max(errorPercent, 0)
	overprediction := math.Max(-errorPercent, 0)
	counters.ForecastUnderpredictionPercent += underprediction
	counters.ForecastOverpredictionPercent += overprediction
	if underprediction > 0.000001 {
		counters.ForecastUnderpredictionSamples++
	}
	if overprediction > 0.000001 {
		counters.ForecastOverpredictionSamples++
	}
	if len(counters.ForecastUnderpredictionBuckets) != len(adaptiveForecastUnderpredictionBounds)+1 {
		counters.ForecastUnderpredictionBuckets = make([]float64, len(adaptiveForecastUnderpredictionBounds)+1)
	}
	bucket := sort.Search(len(adaptiveForecastUnderpredictionBounds), func(index int) bool {
		return underprediction <= adaptiveForecastUnderpredictionBounds[index]
	})
	counters.ForecastUnderpredictionBuckets[bucket]++
	counters.ForecastMaximumUnderprediction = math.Max(counters.ForecastMaximumUnderprediction, underprediction)
}

func mergeAdaptiveForecastBuckets(left, right []float64) []float64 {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	out := make([]float64, length)
	copy(out, left)
	for index, value := range right {
		out[index] += value
	}
	return out
}

func adaptiveForecastBacktestSummary(authIndexes []string, now time.Time) adaptiveForecastBacktestPublicView {
	allowed := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
			allowed[authIndex] = struct{}{}
		}
	}
	filter := authIndexes != nil
	from := now.UTC().Add(-adaptiveForecastBacktestPeriod)
	windowCounters := make(map[string]quotaObservationCounters)
	windowMetadata := make(map[string]adaptiveForecastBacktestWindowView)
	bravoUsageState.mu.RLock()
	for _, aggregate := range bravoUsageState.state.QuotaObservations {
		if aggregate == nil {
			continue
		}
		if filter {
			if _, ok := allowed[strings.TrimSpace(aggregate.AuthIndex)]; !ok {
				continue
			}
		}
		key := quotaConsumptionWindowKey(aggregate.Provider, aggregate.WindowKind, aggregate.QuotaModel)
		windowCounters[key] = mergeQuotaObservationCounters(
			windowCounters[key],
			quotaObservationCountersBetween(&aggregate.Usage, from, now.UTC()),
		)
		windowMetadata[key] = adaptiveForecastBacktestWindowView{
			Provider: normalizeProvider(aggregate.Provider), WindowKind: aggregate.WindowKind,
			QuotaModel: strings.ToLower(strings.TrimSpace(aggregate.QuotaModel)),
		}
	}
	bravoUsageState.mu.RUnlock()

	view := adaptiveForecastBacktestPublicView{
		Status:             "collecting",
		PeriodHours:        int(adaptiveForecastBacktestPeriod / time.Hour),
		WindowsIndependent: true,
		Windows:            []adaptiveForecastBacktestWindowView{},
		Note:               "Backtest compares the pre-request token-calibrated reservation with the next provider-confirmed quota drop. Positive error is an upper bound on estimator slippage because shared subscriptions may also have external traffic.",
	}
	keys := make([]string, 0, len(windowCounters))
	for key := range windowCounters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		counters := windowCounters[key]
		if counters.ForecastSamples == 0 && counters.ForecastSkippedUncalibrated == 0 &&
			counters.ForecastSkippedNoLocal == 0 {
			continue
		}
		window := adaptiveForecastWindowFromCounters(windowMetadata[key], counters)
		if window.Status == "available" {
			view.ReadyWindows++
		}
		view.PairedIntervals += counters.ForecastSamples
		view.SkippedUncalibratedIntervals += counters.ForecastSkippedUncalibrated
		view.SkippedNoLocalIntervals += counters.ForecastSkippedNoLocal
		view.Windows = append(view.Windows, window)
	}
	if view.ReadyWindows > 0 {
		view.Status = "available"
	}
	return view
}

func adaptiveForecastWindowFromCounters(
	window adaptiveForecastBacktestWindowView,
	counters quotaObservationCounters,
) adaptiveForecastBacktestWindowView {
	window.Status = "collecting"
	window.PairedIntervals = counters.ForecastSamples
	window.SkippedUncalibratedIntervals = counters.ForecastSkippedUncalibrated
	window.SkippedNoLocalIntervals = counters.ForecastSkippedNoLocal
	window.CoverageSeconds = counters.ForecastCoverageSeconds
	window.PredictedDropPercent = counters.ForecastPredictedPercent
	window.ActualDropPercent = counters.ForecastActualPercent
	window.UnderpredictionPercent = counters.ForecastUnderpredictionPercent
	window.OverpredictionPercent = counters.ForecastOverpredictionPercent
	window.UnderpredictionIntervals = counters.ForecastUnderpredictionSamples
	window.OverpredictionIntervals = counters.ForecastOverpredictionSamples
	window.MaximumUnderpredictionPercent = counters.ForecastMaximumUnderprediction
	window.UnderpredictionP95Percent = adaptiveForecastUnderpredictionQuantile(
		counters.ForecastUnderpredictionBuckets, 0.95,
	)
	if counters.ForecastSamples > 0 {
		samples := float64(counters.ForecastSamples)
		window.MeanPredictedPPPerInterval = counters.ForecastPredictedPercent / samples
		window.MeanActualPPPerInterval = counters.ForecastActualPercent / samples
		window.MeanBiasPPPerInterval = counters.ForecastSignedErrorPercent / samples
		window.MeanAbsoluteErrorPPPerInterval = counters.ForecastAbsoluteErrorPercent / samples
		window.ConservativeCoveragePercent = math.Max(
			float64(counters.ForecastSamples-counters.ForecastUnderpredictionSamples)/samples*100,
			0,
		)
	}
	if counters.ForecastSamples >= adaptiveForecastMinimumIntervals &&
		counters.ForecastCoverageSeconds >= int64(adaptiveForecastMinimumCoverage/time.Second) {
		window.Status = "available"
	}
	return roundAdaptiveForecastBacktestWindow(window)
}

func adaptiveForecastUnderpredictionQuantile(buckets []float64, quantile float64) float64 {
	total := 0.0
	for _, value := range buckets {
		if adaptiveForecastFiniteNonNegative(value) {
			total += value
		}
	}
	if total <= 0 {
		return 0
	}
	target := total * math.Min(math.Max(quantile, 0), 1)
	seen := 0.0
	for index, value := range buckets {
		seen += math.Max(value, 0)
		if seen+0.000000001 < target {
			continue
		}
		if index < len(adaptiveForecastUnderpredictionBounds) {
			return adaptiveForecastUnderpredictionBounds[index]
		}
		return 100
	}
	return 100
}

func roundAdaptiveForecastBacktestWindow(view adaptiveForecastBacktestWindowView) adaptiveForecastBacktestWindowView {
	view.PredictedDropPercent = adaptiveShadowRound(view.PredictedDropPercent)
	view.ActualDropPercent = adaptiveShadowRound(view.ActualDropPercent)
	view.MeanPredictedPPPerInterval = adaptiveShadowRound(view.MeanPredictedPPPerInterval)
	view.MeanActualPPPerInterval = adaptiveShadowRound(view.MeanActualPPPerInterval)
	view.MeanBiasPPPerInterval = adaptiveShadowRound(view.MeanBiasPPPerInterval)
	view.MeanAbsoluteErrorPPPerInterval = adaptiveShadowRound(view.MeanAbsoluteErrorPPPerInterval)
	view.UnderpredictionPercent = adaptiveShadowRound(view.UnderpredictionPercent)
	view.OverpredictionPercent = adaptiveShadowRound(view.OverpredictionPercent)
	view.ConservativeCoveragePercent = adaptiveShadowRound(view.ConservativeCoveragePercent)
	view.UnderpredictionP95Percent = adaptiveShadowRound(view.UnderpredictionP95Percent)
	view.MaximumUnderpredictionPercent = adaptiveShadowRound(view.MaximumUnderpredictionPercent)
	return view
}

func adaptiveForecastFiniteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func normalizeAdaptiveForecastCounters(counters quotaObservationCounters) quotaObservationCounters {
	valid := counters.ForecastSamples >= 0 && counters.ForecastSkippedUncalibrated >= 0 &&
		counters.ForecastSkippedNoLocal >= 0 && counters.ForecastCoverageSeconds >= 0 &&
		counters.ForecastUnderpredictionSamples >= 0 && counters.ForecastOverpredictionSamples >= 0 &&
		counters.ForecastUnderpredictionSamples <= counters.ForecastSamples &&
		counters.ForecastOverpredictionSamples <= counters.ForecastSamples
	for _, value := range []float64{
		counters.ForecastPredictedPercent,
		counters.ForecastActualPercent,
		counters.ForecastAbsoluteErrorPercent,
		counters.ForecastUnderpredictionPercent,
		counters.ForecastOverpredictionPercent,
		counters.ForecastMaximumUnderprediction,
	} {
		valid = valid && adaptiveForecastFiniteNonNegative(value)
	}
	valid = valid && !math.IsNaN(counters.ForecastSignedErrorPercent) &&
		!math.IsInf(counters.ForecastSignedErrorPercent, 0)
	if counters.ForecastSamples == 0 {
		valid = valid && counters.ForecastCoverageSeconds == 0 &&
			counters.ForecastPredictedPercent == 0 && counters.ForecastActualPercent == 0 &&
			counters.ForecastSignedErrorPercent == 0 && counters.ForecastAbsoluteErrorPercent == 0 &&
			counters.ForecastUnderpredictionPercent == 0 && counters.ForecastOverpredictionPercent == 0 &&
			counters.ForecastUnderpredictionSamples == 0 && counters.ForecastOverpredictionSamples == 0 &&
			counters.ForecastMaximumUnderprediction == 0
		if valid {
			counters.ForecastUnderpredictionBuckets = nil
			return counters
		}
	}
	if len(counters.ForecastUnderpredictionBuckets) != len(adaptiveForecastUnderpredictionBounds)+1 {
		valid = false
	}
	bucketSamples := 0.0
	for _, value := range counters.ForecastUnderpredictionBuckets {
		if !adaptiveForecastFiniteNonNegative(value) {
			valid = false
			break
		}
		bucketSamples += value
	}
	if math.Abs(bucketSamples-float64(counters.ForecastSamples)) > 0.000001 {
		valid = false
	}
	if valid {
		return counters
	}
	counters.ForecastSamples = 0
	counters.ForecastCoverageSeconds = 0
	counters.ForecastPredictedPercent = 0
	counters.ForecastActualPercent = 0
	counters.ForecastSignedErrorPercent = 0
	counters.ForecastAbsoluteErrorPercent = 0
	counters.ForecastUnderpredictionPercent = 0
	counters.ForecastOverpredictionPercent = 0
	counters.ForecastUnderpredictionSamples = 0
	counters.ForecastOverpredictionSamples = 0
	counters.ForecastUnderpredictionBuckets = nil
	counters.ForecastMaximumUnderprediction = 0
	if counters.ForecastSkippedUncalibrated < 0 {
		counters.ForecastSkippedUncalibrated = 0
	}
	if counters.ForecastSkippedNoLocal < 0 {
		counters.ForecastSkippedNoLocal = 0
	}
	return counters
}

func normalizeAdaptiveForecastState(state *persistedUsageState) {
	if state == nil {
		return
	}
	for _, aggregate := range state.QuotaObservations {
		if aggregate == nil {
			continue
		}
		aggregate.Usage.Total = normalizeAdaptiveForecastCounters(aggregate.Usage.Total)
		for bucket, counters := range aggregate.Usage.Hourly {
			aggregate.Usage.Hourly[bucket] = normalizeAdaptiveForecastCounters(counters)
		}
		for bucket, counters := range aggregate.Usage.Daily {
			aggregate.Usage.Daily[bucket] = normalizeAdaptiveForecastCounters(counters)
		}
	}
}
