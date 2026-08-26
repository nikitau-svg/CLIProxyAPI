package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	quotaConsumptionDimensionKeyVersion = "v1"
)

// quotaObservationCounters describes one provider-confirmed quota window.
// Percentages from session, weekly and model-weekly windows are deliberately
// never added together: each window is an independent capacity constraint.
type quotaObservationCounters struct {
	Samples                          int64     `json:"samples"`
	SkippedResetOrIncreaseSamples    int64     `json:"skipped_reset_or_increase_samples,omitempty"`
	CoverageSeconds                  int64     `json:"coverage_seconds"`
	ObservedDropPercent              float64   `json:"observed_drop_percent"`
	EstimatedLocalPercent            float64   `json:"estimated_local_percent"`
	AttributedProjectPercent         float64   `json:"attributed_project_percent"`
	AttributedLocalUnassignedPercent float64   `json:"attributed_local_unassigned_percent"`
	ExternalOrEstimatorGapPercent    float64   `json:"external_or_estimator_gap_percent"`
	ForecastSamples                  int64     `json:"forecast_samples,omitempty"`
	ForecastSkippedUncalibrated      int64     `json:"forecast_skipped_uncalibrated,omitempty"`
	ForecastSkippedNoLocal           int64     `json:"forecast_skipped_no_local,omitempty"`
	ForecastCoverageSeconds          int64     `json:"forecast_coverage_seconds,omitempty"`
	ForecastPredictedPercent         float64   `json:"forecast_predicted_percent,omitempty"`
	ForecastActualPercent            float64   `json:"forecast_actual_percent,omitempty"`
	ForecastSignedErrorPercent       float64   `json:"forecast_signed_error_percent,omitempty"`
	ForecastAbsoluteErrorPercent     float64   `json:"forecast_absolute_error_percent,omitempty"`
	ForecastUnderpredictionPercent   float64   `json:"forecast_underprediction_percent,omitempty"`
	ForecastOverpredictionPercent    float64   `json:"forecast_overprediction_percent,omitempty"`
	ForecastUnderpredictionSamples   int64     `json:"forecast_underprediction_samples,omitempty"`
	ForecastOverpredictionSamples    int64     `json:"forecast_overprediction_samples,omitempty"`
	ForecastUnderpredictionBuckets   []float64 `json:"forecast_underprediction_buckets,omitempty"`
	ForecastMaximumUnderprediction   float64   `json:"forecast_maximum_underprediction_percent,omitempty"`
}

type quotaObservationAggregate struct {
	Total  quotaObservationCounters            `json:"total"`
	Hourly map[string]quotaObservationCounters `json:"hourly,omitempty"`
	Daily  map[string]quotaObservationCounters `json:"daily,omitempty"`
}

type quotaObservationUsageAggregate struct {
	AuthIndex  string                    `json:"auth_index"`
	Provider   string                    `json:"provider"`
	WindowKind string                    `json:"window_kind"`
	QuotaModel string                    `json:"quota_model,omitempty"`
	Usage      quotaObservationAggregate `json:"usage"`
}

type quotaProjectCounters struct {
	Commitments             int64   `json:"commitments"`
	EstimatedPercent        float64 `json:"estimated_percent"`
	AttributedPercent       float64 `json:"attributed_percent"`
	BaseX1EquivalentPercent float64 `json:"base_x1_equivalent_percent"`
}

type quotaProjectAggregate struct {
	Total  quotaProjectCounters            `json:"total"`
	Hourly map[string]quotaProjectCounters `json:"hourly,omitempty"`
	Daily  map[string]quotaProjectCounters `json:"daily,omitempty"`
}

type quotaProjectUsageAggregate struct {
	ProjectID    string                `json:"project_id"`
	AuthIndex    string                `json:"auth_index"`
	Provider     string                `json:"provider"`
	WindowKind   string                `json:"window_kind"`
	QuotaModel   string                `json:"quota_model,omitempty"`
	Model        string                `json:"model,omitempty"`
	LogicalModel string                `json:"logical_model,omitempty"`
	Effort       string                `json:"effort,omitempty"`
	TariffID     string                `json:"tariff_id,omitempty"`
	Multiplier   float64               `json:"multiplier"`
	Usage        quotaProjectAggregate `json:"usage"`
}

type quotaObservationMutation struct {
	AuthIndex  string
	Provider   string
	WindowKind string
	QuotaModel string
	At         time.Time
	Counters   quotaObservationCounters
}

type quotaProjectMutation struct {
	AuthIndex    string
	ProjectID    string
	Provider     string
	WindowKind   string
	QuotaModel   string
	Model        string
	LogicalModel string
	Effort       string
	TariffID     string
	Multiplier   float64
	At           time.Time
	Counters     quotaProjectCounters
}

func newQuotaObservationAggregate() quotaObservationAggregate {
	return quotaObservationAggregate{
		Hourly: make(map[string]quotaObservationCounters),
		Daily:  make(map[string]quotaObservationCounters),
	}
}

func newQuotaProjectAggregate() quotaProjectAggregate {
	return quotaProjectAggregate{
		Hourly: make(map[string]quotaProjectCounters),
		Daily:  make(map[string]quotaProjectCounters),
	}
}

func normalizeQuotaConsumptionState(state *persistedUsageState) {
	if state == nil {
		return
	}
	if state.QuotaObservations == nil {
		state.QuotaObservations = make(map[string]*quotaObservationUsageAggregate)
	}
	if state.QuotaProjectAttributions == nil {
		state.QuotaProjectAttributions = make(map[string]*quotaProjectUsageAggregate)
	}
	for _, aggregate := range state.QuotaObservations {
		if aggregate == nil {
			continue
		}
		if aggregate.Usage.Hourly == nil {
			aggregate.Usage.Hourly = make(map[string]quotaObservationCounters)
		}
		if aggregate.Usage.Daily == nil {
			aggregate.Usage.Daily = make(map[string]quotaObservationCounters)
		}
	}
	for _, aggregate := range state.QuotaProjectAttributions {
		if aggregate == nil {
			continue
		}
		if aggregate.Usage.Hourly == nil {
			aggregate.Usage.Hourly = make(map[string]quotaProjectCounters)
		}
		if aggregate.Usage.Daily == nil {
			aggregate.Usage.Daily = make(map[string]quotaProjectCounters)
		}
		if aggregate.Multiplier <= 0 || math.IsNaN(aggregate.Multiplier) || math.IsInf(aggregate.Multiplier, 0) {
			aggregate.Multiplier = 1
		}
	}
}

func pruneQuotaConsumptionState(state *persistedUsageState, reference time.Time) {
	if state == nil {
		return
	}
	reference = reference.UTC()
	hourlyBefore := reference.Add(-hourlyUsageRetention).Truncate(time.Hour)
	dailyBefore := reference.Add(-dailyUsageRetention).Truncate(24 * time.Hour)
	for key, aggregate := range state.QuotaObservations {
		if aggregate == nil {
			delete(state.QuotaObservations, key)
			continue
		}
		for bucket := range aggregate.Usage.Hourly {
			at, errParse := time.Parse(time.RFC3339, bucket)
			if errParse != nil || at.Before(hourlyBefore) {
				delete(aggregate.Usage.Hourly, bucket)
			}
		}
		for bucket := range aggregate.Usage.Daily {
			at, errParse := time.Parse(dailyUsageBucketLayout, bucket)
			if errParse != nil || at.Before(dailyBefore) {
				delete(aggregate.Usage.Daily, bucket)
			}
		}
	}
	for key, aggregate := range state.QuotaProjectAttributions {
		if aggregate == nil {
			delete(state.QuotaProjectAttributions, key)
			continue
		}
		for bucket := range aggregate.Usage.Hourly {
			at, errParse := time.Parse(time.RFC3339, bucket)
			if errParse != nil || at.Before(hourlyBefore) {
				delete(aggregate.Usage.Hourly, bucket)
			}
		}
		for bucket := range aggregate.Usage.Daily {
			at, errParse := time.Parse(dailyUsageBucketLayout, bucket)
			if errParse != nil || at.Before(dailyBefore) {
				delete(aggregate.Usage.Daily, bucket)
			}
		}
	}
}

func recordQuotaConsumptionReconciliation(
	authIndex string,
	previous credentialQuotaState,
	refreshed credentialQuotaState,
	previousAt time.Time,
	observedAt time.Time,
	commits []adaptiveShadowCommit,
) {
	authIndex = strings.TrimSpace(authIndex)
	provider := normalizeProvider(firstNonEmpty(refreshed.Provider, previous.Provider))
	if authIndex == "" || provider == "" || previousAt.IsZero() || !observedAt.After(previousAt) {
		return
	}
	observedAt = observedAt.UTC()
	previousAt = previousAt.UTC()
	observations := make([]quotaObservationMutation, 0, 2+len(previous.ModelWeekly))
	projects := make([]quotaProjectMutation, 0, len(commits)*2)
	appendWindow := func(kind, quotaModel string, before, after quotaWindowState) {
		observation, attributed := buildQuotaConsumptionWindow(
			authIndex, provider, kind, quotaModel, before, after,
			previousAt, observedAt, commits,
		)
		observations = append(observations, observation)
		projects = append(projects, attributed...)
	}
	appendWindow(pluginapi.HostAuthQuotaWindowKindSession, "", previous.Session, refreshed.Session)
	appendWindow(pluginapi.HostAuthQuotaWindowKindWeekly, "", previous.Weekly, refreshed.Weekly)
	refreshedModels := make(map[string]quotaWindowState, len(refreshed.ModelWeekly))
	for _, window := range refreshed.ModelWeekly {
		model := strings.ToLower(strings.TrimSpace(window.Model))
		if model != "" {
			refreshedModels[model] = window.quotaWindowState
		}
	}
	for _, window := range previous.ModelWeekly {
		model := strings.ToLower(strings.TrimSpace(window.Model))
		after, exists := refreshedModels[model]
		if model == "" || !exists {
			continue
		}
		appendWindow(pluginapi.HostAuthQuotaWindowKindModelWeekly, model, window.quotaWindowState, after)
	}

	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	normalizeQuotaConsumptionState(&bravoUsageState.state)
	if bravoUsageState.state.SchemaVersion == 0 {
		bravoUsageState.state.SchemaVersion = usageStateSchemaVersion
	}
	if bravoUsageState.state.QuotaAttributionStartedAt.IsZero() || previousAt.Before(bravoUsageState.state.QuotaAttributionStartedAt) {
		bravoUsageState.state.QuotaAttributionStartedAt = previousAt
	}
	for _, mutation := range observations {
		key := quotaObservationDimensionKey(mutation.AuthIndex, mutation.Provider, mutation.WindowKind, mutation.QuotaModel)
		aggregate := bravoUsageState.state.QuotaObservations[key]
		if aggregate == nil {
			aggregate = &quotaObservationUsageAggregate{
				AuthIndex: mutation.AuthIndex, Provider: mutation.Provider,
				WindowKind: mutation.WindowKind, QuotaModel: mutation.QuotaModel,
				Usage: newQuotaObservationAggregate(),
			}
			bravoUsageState.state.QuotaObservations[key] = aggregate
		}
		addQuotaObservationCounter(&aggregate.Usage, mutation.At, mutation.Counters)
	}
	for _, mutation := range projects {
		key := quotaProjectDimensionKey(mutation)
		aggregate := bravoUsageState.state.QuotaProjectAttributions[key]
		if aggregate == nil {
			aggregate = &quotaProjectUsageAggregate{
				ProjectID: mutation.ProjectID, AuthIndex: mutation.AuthIndex,
				Provider: mutation.Provider, WindowKind: mutation.WindowKind, QuotaModel: mutation.QuotaModel,
				Model: mutation.Model, LogicalModel: mutation.LogicalModel, Effort: mutation.Effort,
				TariffID: mutation.TariffID, Multiplier: mutation.Multiplier,
				Usage: newQuotaProjectAggregate(),
			}
			bravoUsageState.state.QuotaProjectAttributions[key] = aggregate
		}
		addQuotaProjectCounter(&aggregate.Usage, mutation.At, mutation.Counters)
	}
	bravoUsageState.scheduleSaveLocked()
}

func buildQuotaConsumptionWindow(
	authIndex, provider, kind, quotaModel string,
	before, after quotaWindowState,
	previousAt, observedAt time.Time,
	commits []adaptiveShadowCommit,
) (quotaObservationMutation, []quotaProjectMutation) {
	coverage := int64(observedAt.Sub(previousAt) / time.Second)
	if coverage < 0 {
		coverage = 0
	}
	drop, valid := quotaWindowObservedDrop(before, after, previousAt, observedAt)
	observation := quotaObservationMutation{
		AuthIndex: authIndex, Provider: provider, WindowKind: kind, QuotaModel: quotaModel, At: observedAt,
		Counters: quotaObservationCounters{CoverageSeconds: coverage},
	}
	if !valid {
		observation.Counters.SkippedResetOrIncreaseSamples = 1
		return observation, nil
	}
	observation.Counters.Samples = 1
	observation.Counters.ObservedDropPercent = drop

	type groupedCommit struct {
		mutation quotaProjectMutation
		count    int64
		percent  float64
		weight   float64
	}
	groups := make(map[string]*groupedCommit)
	totalEstimated := 0.0
	unassignedEstimated := 0.0
	totalWeight := 0.0
	unassignedWeight := 0.0
	matchingCommits := 0
	allTokenCalibrated := true
	for _, commit := range commits {
		percent, applies, tokenCalibrated := adaptiveShadowCommitPercentForWindow(commit, kind, quotaModel)
		if !applies {
			continue
		}
		matchingCommits++
		if !tokenCalibrated {
			allTokenCalibrated = false
		}
		totalEstimated += percent
		weight := commit.TokenUnits
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			weight = percent
		}
		totalWeight += weight
		projectID := strings.TrimSpace(commit.ProjectID)
		if projectID == "" || !validProjectID(projectID) {
			unassignedEstimated += percent
			unassignedWeight += weight
			continue
		}
		multiplier := commit.Multiplier
		if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
			multiplier = 1
		}
		mutation := quotaProjectMutation{
			AuthIndex: authIndex, ProjectID: projectID, Provider: provider,
			WindowKind: kind, QuotaModel: quotaModel,
			Model: strings.TrimSpace(commit.Model), LogicalModel: strings.TrimSpace(commit.LogicalModel),
			Effort: normalizeEffort(commit.Effort), TariffID: strings.TrimSpace(commit.TariffID),
			Multiplier: multiplier, At: observedAt,
		}
		key := quotaProjectDimensionKey(mutation)
		group := groups[key]
		if group == nil {
			group = &groupedCommit{mutation: mutation}
			groups[key] = group
		}
		group.count++
		group.percent += percent
		group.weight += weight
	}
	observation.Counters.EstimatedLocalPercent = totalEstimated
	recordAdaptiveForecastObservation(
		&observation.Counters, totalEstimated, drop, coverage,
		matchingCommits, allTokenCalibrated,
	)
	scale := 1.0
	if totalEstimated > 0 && drop < totalEstimated {
		scale = math.Max(drop/totalEstimated, 0)
	}
	attributedLocal := totalEstimated * scale
	attributedUnassigned := unassignedEstimated * scale
	if totalWeight > 0 {
		attributedUnassigned = attributedLocal * unassignedWeight / totalWeight
	}
	observation.Counters.AttributedLocalUnassignedPercent = attributedUnassigned
	observation.Counters.ExternalOrEstimatorGapPercent = math.Max(drop-attributedLocal, 0)
	mutations := make([]quotaProjectMutation, 0, len(groups))
	for _, group := range groups {
		attributed := group.percent * scale
		if totalWeight > 0 {
			attributed = attributedLocal * group.weight / totalWeight
		}
		group.mutation.Counters = quotaProjectCounters{
			Commitments:             group.count,
			EstimatedPercent:        group.percent,
			AttributedPercent:       attributed,
			BaseX1EquivalentPercent: attributed * group.mutation.Multiplier,
		}
		observation.Counters.AttributedProjectPercent += attributed
		mutations = append(mutations, group.mutation)
	}
	sort.Slice(mutations, func(i, j int) bool {
		return quotaProjectDimensionKey(mutations[i]) < quotaProjectDimensionKey(mutations[j])
	})
	return observation, mutations
}

func quotaWindowObservedDrop(before, after quotaWindowState, previousAt, observedAt time.Time) (float64, bool) {
	before = normalizeQuotaWindow(before)
	after = normalizeQuotaWindow(after)
	if !before.ResetAt.IsZero() && before.ResetAt.After(previousAt) && !before.ResetAt.After(observedAt) {
		return 0, false
	}
	if after.RemainingPercent > before.RemainingPercent+0.000001 {
		return 0, false
	}
	return math.Max(before.RemainingPercent-after.RemainingPercent, 0), true
}

func addQuotaObservationCounter(aggregate *quotaObservationAggregate, at time.Time, value quotaObservationCounters) {
	if aggregate.Hourly == nil {
		aggregate.Hourly = make(map[string]quotaObservationCounters)
	}
	if aggregate.Daily == nil {
		aggregate.Daily = make(map[string]quotaObservationCounters)
	}
	aggregate.Total = mergeQuotaObservationCounters(aggregate.Total, value)
	at = at.UTC()
	hour := at.Truncate(time.Hour).Format(time.RFC3339)
	aggregate.Hourly[hour] = mergeQuotaObservationCounters(aggregate.Hourly[hour], value)
	day := at.Format(dailyUsageBucketLayout)
	aggregate.Daily[day] = mergeQuotaObservationCounters(aggregate.Daily[day], value)
}

func addQuotaProjectCounter(aggregate *quotaProjectAggregate, at time.Time, value quotaProjectCounters) {
	if aggregate.Hourly == nil {
		aggregate.Hourly = make(map[string]quotaProjectCounters)
	}
	if aggregate.Daily == nil {
		aggregate.Daily = make(map[string]quotaProjectCounters)
	}
	aggregate.Total = mergeQuotaProjectCounters(aggregate.Total, value)
	at = at.UTC()
	hour := at.Truncate(time.Hour).Format(time.RFC3339)
	aggregate.Hourly[hour] = mergeQuotaProjectCounters(aggregate.Hourly[hour], value)
	day := at.Format(dailyUsageBucketLayout)
	aggregate.Daily[day] = mergeQuotaProjectCounters(aggregate.Daily[day], value)
}

func mergeQuotaObservationCounters(left, right quotaObservationCounters) quotaObservationCounters {
	left.Samples += right.Samples
	left.SkippedResetOrIncreaseSamples += right.SkippedResetOrIncreaseSamples
	left.CoverageSeconds += right.CoverageSeconds
	left.ObservedDropPercent += right.ObservedDropPercent
	left.EstimatedLocalPercent += right.EstimatedLocalPercent
	left.AttributedProjectPercent += right.AttributedProjectPercent
	left.AttributedLocalUnassignedPercent += right.AttributedLocalUnassignedPercent
	left.ExternalOrEstimatorGapPercent += right.ExternalOrEstimatorGapPercent
	left.ForecastSamples += right.ForecastSamples
	left.ForecastSkippedUncalibrated += right.ForecastSkippedUncalibrated
	left.ForecastSkippedNoLocal += right.ForecastSkippedNoLocal
	left.ForecastCoverageSeconds += right.ForecastCoverageSeconds
	left.ForecastPredictedPercent += right.ForecastPredictedPercent
	left.ForecastActualPercent += right.ForecastActualPercent
	left.ForecastSignedErrorPercent += right.ForecastSignedErrorPercent
	left.ForecastAbsoluteErrorPercent += right.ForecastAbsoluteErrorPercent
	left.ForecastUnderpredictionPercent += right.ForecastUnderpredictionPercent
	left.ForecastOverpredictionPercent += right.ForecastOverpredictionPercent
	left.ForecastUnderpredictionSamples += right.ForecastUnderpredictionSamples
	left.ForecastOverpredictionSamples += right.ForecastOverpredictionSamples
	left.ForecastUnderpredictionBuckets = mergeAdaptiveForecastBuckets(
		left.ForecastUnderpredictionBuckets,
		right.ForecastUnderpredictionBuckets,
	)
	left.ForecastMaximumUnderprediction = math.Max(
		left.ForecastMaximumUnderprediction,
		right.ForecastMaximumUnderprediction,
	)
	return left
}

func mergeQuotaProjectCounters(left, right quotaProjectCounters) quotaProjectCounters {
	left.Commitments += right.Commitments
	left.EstimatedPercent += right.EstimatedPercent
	left.AttributedPercent += right.AttributedPercent
	left.BaseX1EquivalentPercent += right.BaseX1EquivalentPercent
	return left
}

func quotaObservationDimensionKey(authIndex, provider, kind, quotaModel string) string {
	raw := strings.Join([]string{
		quotaConsumptionDimensionKeyVersion, strings.TrimSpace(authIndex), normalizeProvider(provider),
		strings.TrimSpace(kind), strings.ToLower(strings.TrimSpace(quotaModel)),
	}, "\x1f")
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("qo_%x", digest[:16])
}

func quotaProjectDimensionKey(value quotaProjectMutation) string {
	raw := strings.Join([]string{
		quotaConsumptionDimensionKeyVersion,
		strings.TrimSpace(value.ProjectID), strings.TrimSpace(value.AuthIndex), normalizeProvider(value.Provider),
		strings.TrimSpace(value.WindowKind), strings.ToLower(strings.TrimSpace(value.QuotaModel)),
		strings.TrimSpace(value.Model), strings.TrimSpace(value.LogicalModel), normalizeEffort(value.Effort),
		strings.TrimSpace(value.TariffID), fmt.Sprintf("%.6f", value.Multiplier),
	}, "\x1f")
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("qp_%x", digest[:16])
}

type quotaConsumptionAnalyticsView struct {
	Unit               string                       `json:"unit"`
	Status             string                       `json:"status"`
	WindowsIndependent bool                         `json:"windows_independent"`
	SharedPoolVisible  bool                         `json:"shared_pool_visible"`
	CoverageFrom       *time.Time                   `json:"coverage_from"`
	GeneratedAt        time.Time                    `json:"generated_at"`
	AttributionMethod  string                       `json:"attribution_method"`
	Windows            []quotaConsumptionWindowView `json:"windows"`
	Note               string                       `json:"note"`
}

type quotaConsumptionWindowView struct {
	Provider   string                        `json:"provider"`
	Kind       string                        `json:"kind"`
	QuotaModel string                        `json:"quota_model,omitempty"`
	Confidence string                        `json:"confidence"`
	Pool       *quotaConsumptionPoolView     `json:"shared_pool,omitempty"`
	Projects   []quotaConsumptionProjectView `json:"projects"`
}

type quotaConsumptionPoolView struct {
	Samples                          int64                               `json:"samples"`
	SkippedResetOrIncreaseSamples    int64                               `json:"skipped_reset_or_increase_samples"`
	SubscriptionHours                float64                             `json:"subscription_hours"`
	ObservedDropPercent              float64                             `json:"observed_drop_percent"`
	AttributedProjectPercent         float64                             `json:"attributed_project_percent"`
	AttributedLocalUnassignedPercent float64                             `json:"attributed_local_unassigned_percent"`
	ExternalOrEstimatorGapPercent    float64                             `json:"external_or_estimator_gap_percent"`
	AverageObservedPPPerSubHour      float64                             `json:"average_observed_pp_per_subscription_hour"`
	AverageExternalPPPerSubHour      float64                             `json:"average_external_pp_per_subscription_hour"`
	ForecastBacktest                 *adaptiveForecastBacktestWindowView `json:"forecast_backtest,omitempty"`
}

type quotaConsumptionProjectView struct {
	Rank                         int                                `json:"rank"`
	ProjectID                    string                             `json:"project_id"`
	Commitments                  int64                              `json:"commitments"`
	EstimatedPercent             float64                            `json:"estimated_percent"`
	AttributedPercent            float64                            `json:"attributed_percent"`
	ShareOfAttributedPoolPercent float64                            `json:"share_of_attributed_pool_percent"`
	AveragePPPerHour             float64                            `json:"average_pp_per_hour"`
	PeakHourlyPP                 float64                            `json:"peak_hourly_pp"`
	SubscriptionWindowsConsumed  float64                            `json:"subscription_windows_consumed"`
	BaseX1EquivalentWindows      float64                            `json:"base_x1_equivalent_windows"`
	Models                       []quotaConsumptionModelView        `json:"models"`
	Plans                        []quotaConsumptionPlanCapacityView `json:"plans"`
	Signals                      []string                           `json:"signals"`
}

type quotaConsumptionModelView struct {
	Provider                string  `json:"provider"`
	Model                   string  `json:"model,omitempty"`
	LogicalModel            string  `json:"logical_model,omitempty"`
	Effort                  string  `json:"effort,omitempty"`
	TariffID                string  `json:"tariff_id,omitempty"`
	Commitments             int64   `json:"commitments"`
	AttributedPercent       float64 `json:"attributed_percent"`
	ShareOfProjectPercent   float64 `json:"share_of_project_percent"`
	BaseX1EquivalentPercent float64 `json:"base_x1_equivalent_percent"`
}

type quotaConsumptionPlanCapacityView struct {
	TariffID                            string  `json:"tariff_id,omitempty"`
	Multiplier                          float64 `json:"multiplier"`
	AttributedPercent                   float64 `json:"attributed_percent"`
	BaseX1EquivalentWindows             float64 `json:"base_x1_equivalent_windows"`
	AveragePPPerHour                    float64 `json:"average_pp_per_hour"`
	PeakHourlyPP                        float64 `json:"peak_hourly_pp"`
	EstimatedSubscriptionsAtAveragePace float64 `json:"estimated_subscriptions_at_average_pace"`
	EstimatedSubscriptionsAtPeakPace    float64 `json:"estimated_subscriptions_at_peak_pace"`
	CurrentSubscriptions                int     `json:"current_subscriptions,omitempty"`
	EstimatedAdditionalAtPeakPace       float64 `json:"estimated_additional_at_peak_pace,omitempty"`
	EstimatedSpareAtPeakPace            float64 `json:"estimated_spare_at_peak_pace,omitempty"`
	SuggestedAction                     string  `json:"suggested_action,omitempty"`
}

type quotaConsumptionWindowAccumulator struct {
	Provider     string
	Kind         string
	QuotaModel   string
	Observations quotaObservationCounters
	Projects     map[string]*quotaConsumptionProjectAccumulator
}

type quotaConsumptionProjectAccumulator struct {
	ProjectID string
	Counters  quotaProjectCounters
	Hourly    map[string]quotaProjectCounters
	Models    map[string]*quotaConsumptionModelAccumulator
	Plans     map[string]*quotaConsumptionPlanAccumulator
}

type quotaConsumptionModelAccumulator struct {
	Provider     string
	Model        string
	LogicalModel string
	Effort       string
	TariffID     string
	Counters     quotaProjectCounters
}

type quotaConsumptionPlanAccumulator struct {
	TariffID   string
	Multiplier float64
	Counters   quotaProjectCounters
	Hourly     map[string]quotaProjectCounters
}

func collectQuotaConsumption(query analyticsQuery, generatedAt time.Time, includeSharedPool bool) quotaConsumptionAnalyticsView {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	return collectQuotaConsumptionLocked(&bravoUsageState.state, query, generatedAt, includeSharedPool)
}

func collectQuotaConsumptionLocked(
	state *persistedUsageState,
	query analyticsQuery,
	generatedAt time.Time,
	includeSharedPool bool,
) quotaConsumptionAnalyticsView {
	view := quotaConsumptionAnalyticsView{
		Unit:               "subscription_quota_percentage_points",
		Status:             "collecting",
		WindowsIndependent: true,
		SharedPoolVisible:  includeSharedPool,
		GeneratedAt:        generatedAt.UTC(),
		AttributionMethod:  "provider-confirmed window drop distributed over completed local attempts; residual remains external_or_estimator_gap",
		Windows:            []quotaConsumptionWindowView{},
		Note:               "Session, weekly and model-weekly percentages are independent constraints and must not be summed. Results are estimates, not billing data.",
	}
	if state == nil {
		return view
	}
	view.CoverageFrom = optionalAnalyticsTime(state.QuotaAttributionStartedAt)
	windows := make(map[string]*quotaConsumptionWindowAccumulator)
	ensureWindow := func(provider, kind, quotaModel string) *quotaConsumptionWindowAccumulator {
		key := quotaConsumptionWindowKey(provider, kind, quotaModel)
		window := windows[key]
		if window == nil {
			window = &quotaConsumptionWindowAccumulator{
				Provider: normalizeProvider(provider), Kind: kind, QuotaModel: strings.ToLower(strings.TrimSpace(quotaModel)),
				Projects: make(map[string]*quotaConsumptionProjectAccumulator),
			}
			windows[key] = window
		}
		return window
	}

	// First collect all project rows that match non-project filters. Keeping all
	// projects in the denominator lets a project-scoped response report its
	// honest share without exposing the identities of its neighbours.
	for _, aggregate := range state.QuotaProjectAttributions {
		if aggregate == nil || !quotaProjectAggregateMatches(query, aggregate, false) {
			continue
		}
		counters, hourly := quotaProjectCountersBetween(&aggregate.Usage, query.From, query.To)
		if counters.Commitments == 0 && counters.AttributedPercent == 0 && counters.EstimatedPercent == 0 {
			continue
		}
		window := ensureWindow(aggregate.Provider, aggregate.WindowKind, aggregate.QuotaModel)
		project := window.Projects[aggregate.ProjectID]
		if project == nil {
			project = &quotaConsumptionProjectAccumulator{
				ProjectID: aggregate.ProjectID,
				Hourly:    make(map[string]quotaProjectCounters),
				Models:    make(map[string]*quotaConsumptionModelAccumulator),
				Plans:     make(map[string]*quotaConsumptionPlanAccumulator),
			}
			window.Projects[aggregate.ProjectID] = project
		}
		project.Counters = mergeQuotaProjectCounters(project.Counters, counters)
		for bucket, value := range hourly {
			project.Hourly[bucket] = mergeQuotaProjectCounters(project.Hourly[bucket], value)
		}
		modelKey := strings.Join([]string{aggregate.Provider, aggregate.Model, aggregate.LogicalModel, aggregate.Effort, aggregate.TariffID}, "\x1f")
		model := project.Models[modelKey]
		if model == nil {
			model = &quotaConsumptionModelAccumulator{
				Provider: aggregate.Provider, Model: aggregate.Model, LogicalModel: aggregate.LogicalModel,
				Effort: aggregate.Effort, TariffID: aggregate.TariffID,
			}
			project.Models[modelKey] = model
		}
		model.Counters = mergeQuotaProjectCounters(model.Counters, counters)
		planKey := strings.TrimSpace(aggregate.TariffID) + "\x1f" + fmt.Sprintf("%.6f", aggregate.Multiplier)
		plan := project.Plans[planKey]
		if plan == nil {
			plan = &quotaConsumptionPlanAccumulator{
				TariffID: aggregate.TariffID, Multiplier: math.Max(aggregate.Multiplier, 1),
				Hourly: make(map[string]quotaProjectCounters),
			}
			project.Plans[planKey] = plan
		}
		plan.Counters = mergeQuotaProjectCounters(plan.Counters, counters)
		for bucket, value := range hourly {
			plan.Hourly[bucket] = mergeQuotaProjectCounters(plan.Hourly[bucket], value)
		}
	}

	for _, aggregate := range state.QuotaObservations {
		if aggregate == nil || !quotaObservationAggregateMatches(query, aggregate) {
			continue
		}
		window := ensureWindow(aggregate.Provider, aggregate.WindowKind, aggregate.QuotaModel)
		window.Observations = mergeQuotaObservationCounters(
			window.Observations,
			quotaObservationCountersBetween(&aggregate.Usage, query.From, query.To),
		)
	}

	durationHours := query.To.Sub(query.From).Hours()
	if !state.QuotaAttributionStartedAt.IsZero() && state.QuotaAttributionStartedAt.After(query.From) {
		durationHours = query.To.Sub(state.QuotaAttributionStartedAt).Hours()
	}
	if durationHours <= 0 {
		durationHours = 1
	}
	keys := make([]string, 0, len(windows))
	for key := range windows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		window := windows[key]
		item := quotaConsumptionWindowView{
			Provider: window.Provider, Kind: window.Kind, QuotaModel: window.QuotaModel,
			Confidence: quotaConsumptionConfidence(window.Observations),
			Projects:   []quotaConsumptionProjectView{},
		}
		if includeSharedPool {
			subscriptionHours := float64(window.Observations.CoverageSeconds) / 3600
			pool := &quotaConsumptionPoolView{
				Samples:                          window.Observations.Samples,
				SkippedResetOrIncreaseSamples:    window.Observations.SkippedResetOrIncreaseSamples,
				SubscriptionHours:                subscriptionHours,
				ObservedDropPercent:              window.Observations.ObservedDropPercent,
				AttributedProjectPercent:         window.Observations.AttributedProjectPercent,
				AttributedLocalUnassignedPercent: window.Observations.AttributedLocalUnassignedPercent,
				ExternalOrEstimatorGapPercent:    window.Observations.ExternalOrEstimatorGapPercent,
			}
			if subscriptionHours > 0 {
				pool.AverageObservedPPPerSubHour = pool.ObservedDropPercent / subscriptionHours
				pool.AverageExternalPPPerSubHour = pool.ExternalOrEstimatorGapPercent / subscriptionHours
			}
			if window.Observations.ForecastSamples > 0 ||
				window.Observations.ForecastSkippedUncalibrated > 0 ||
				window.Observations.ForecastSkippedNoLocal > 0 {
				forecast := adaptiveForecastWindowFromCounters(adaptiveForecastBacktestWindowView{
					Provider: window.Provider, WindowKind: window.Kind, QuotaModel: window.QuotaModel,
				}, window.Observations)
				pool.ForecastBacktest = &forecast
			}
			item.Pool = pool
		}
		projectTotal := 0.0
		for _, project := range window.Projects {
			projectTotal += project.Counters.AttributedPercent
		}
		allProjects := make([]quotaConsumptionProjectView, 0, len(window.Projects))
		for _, project := range window.Projects {
			projectView := buildQuotaConsumptionProjectView(window, project, projectTotal, durationHours, item.Confidence)
			allProjects = append(allProjects, projectView)
		}
		sort.Slice(allProjects, func(i, j int) bool {
			if allProjects[i].AttributedPercent != allProjects[j].AttributedPercent {
				return allProjects[i].AttributedPercent > allProjects[j].AttributedPercent
			}
			return allProjects[i].ProjectID < allProjects[j].ProjectID
		})
		for index := range allProjects {
			allProjects[index].Rank = index + 1
			if query.ProjectID == "" || allProjects[index].ProjectID == query.ProjectID {
				item.Projects = append(item.Projects, allProjects[index])
			}
		}
		if len(item.Projects) > 0 || item.Pool != nil && (item.Pool.Samples > 0 || item.Pool.SkippedResetOrIncreaseSamples > 0) {
			view.Windows = append(view.Windows, item)
		}
	}
	if len(view.Windows) > 0 {
		view.Status = "available"
	}
	return roundQuotaConsumptionView(view)
}

func buildQuotaConsumptionProjectView(
	window *quotaConsumptionWindowAccumulator,
	project *quotaConsumptionProjectAccumulator,
	poolAttributed float64,
	durationHours float64,
	confidence string,
) quotaConsumptionProjectView {
	average := project.Counters.AttributedPercent / durationHours
	view := quotaConsumptionProjectView{
		ProjectID:                   project.ProjectID,
		Commitments:                 project.Counters.Commitments,
		EstimatedPercent:            project.Counters.EstimatedPercent,
		AttributedPercent:           project.Counters.AttributedPercent,
		AveragePPPerHour:            average,
		PeakHourlyPP:                peakQuotaProjectPace(project.Hourly, average),
		SubscriptionWindowsConsumed: project.Counters.AttributedPercent / 100,
		BaseX1EquivalentWindows:     project.Counters.BaseX1EquivalentPercent / 100,
		Models:                      []quotaConsumptionModelView{}, Plans: []quotaConsumptionPlanCapacityView{}, Signals: []string{},
	}
	if poolAttributed > 0 {
		view.ShareOfAttributedPoolPercent = project.Counters.AttributedPercent / poolAttributed * 100
	}
	for _, model := range project.Models {
		entry := quotaConsumptionModelView{
			Provider: model.Provider, Model: model.Model, LogicalModel: model.LogicalModel,
			Effort: model.Effort, TariffID: model.TariffID,
			Commitments:             model.Counters.Commitments,
			AttributedPercent:       model.Counters.AttributedPercent,
			BaseX1EquivalentPercent: model.Counters.BaseX1EquivalentPercent,
		}
		if project.Counters.AttributedPercent > 0 {
			entry.ShareOfProjectPercent = entry.AttributedPercent / project.Counters.AttributedPercent * 100
		}
		view.Models = append(view.Models, entry)
	}
	sort.Slice(view.Models, func(i, j int) bool {
		if view.Models[i].AttributedPercent != view.Models[j].AttributedPercent {
			return view.Models[i].AttributedPercent > view.Models[j].AttributedPercent
		}
		return view.Models[i].Model+view.Models[i].Effort < view.Models[j].Model+view.Models[j].Effort
	})
	windowHours := quotaCapacityWindowHours(window.Kind)
	for _, plan := range project.Plans {
		average := plan.Counters.AttributedPercent / durationHours
		peak := peakQuotaProjectPace(plan.Hourly, average)
		view.Plans = append(view.Plans, quotaConsumptionPlanCapacityView{
			TariffID: plan.TariffID, Multiplier: plan.Multiplier,
			AttributedPercent:       plan.Counters.AttributedPercent,
			BaseX1EquivalentWindows: plan.Counters.BaseX1EquivalentPercent / 100,
			AveragePPPerHour:        average, PeakHourlyPP: peak,
			EstimatedSubscriptionsAtAveragePace: average * windowHours / 100,
			EstimatedSubscriptionsAtPeakPace:    peak * windowHours / 100,
		})
	}
	sort.Slice(view.Plans, func(i, j int) bool {
		if view.Plans[i].AttributedPercent != view.Plans[j].AttributedPercent {
			return view.Plans[i].AttributedPercent > view.Plans[j].AttributedPercent
		}
		return view.Plans[i].TariffID < view.Plans[j].TariffID
	})
	if confidence == "collecting" || confidence == "low" {
		view.Signals = append(view.Signals, "Недостаточно подтверждённых снимков для рекомендации по числу подписок.")
	} else if len(view.Models) > 0 {
		top := view.Models[0]
		if strings.Contains(strings.ToLower(top.Model+" "+top.LogicalModel), "fable") &&
			(top.Effort == "max" || top.Effort == "ultra" || top.Effort == "xhigh") && top.ShareOfProjectPercent >= 40 {
			view.Signals = append(view.Signals, fmt.Sprintf(
				"%.0f%% расхода создаёт Fable с effort %s; сначала проверьте, нужен ли этот режим всем задачам.",
				top.ShareOfProjectPercent, top.Effort,
			))
		}
	}
	if view.PeakHourlyPP > view.AveragePPPerHour*2 && view.PeakHourlyPP > 0.1 {
		view.Signals = append(view.Signals, "Пиковый темп более чем вдвое выше среднего; дополнительная ёмкость нужна прежде всего для всплесков.")
	}
	return view
}

func quotaProjectAggregateMatches(query analyticsQuery, aggregate *quotaProjectUsageAggregate, includeProject bool) bool {
	if aggregate == nil {
		return false
	}
	if includeProject && query.ProjectID != "" && aggregate.ProjectID != query.ProjectID {
		return false
	}
	if query.SubscriptionID != "" && analyticsSubscriptionID(aggregate.AuthIndex) != query.SubscriptionID {
		return false
	}
	if query.Provider != "" && normalizeProvider(aggregate.Provider) != query.Provider {
		return false
	}
	return query.Model == "" || analyticsModelMatches(query.Model, aggregate.Model, aggregate.LogicalModel)
}

func quotaObservationAggregateMatches(query analyticsQuery, aggregate *quotaObservationUsageAggregate) bool {
	if aggregate == nil {
		return false
	}
	if query.SubscriptionID != "" && analyticsSubscriptionID(aggregate.AuthIndex) != query.SubscriptionID {
		return false
	}
	if query.Provider != "" && normalizeProvider(aggregate.Provider) != query.Provider {
		return false
	}
	if query.Model != "" && aggregate.QuotaModel != "" && !quotaModelMatches(query.Model, aggregate.QuotaModel) {
		return false
	}
	return true
}

func quotaProjectCountersBetween(
	aggregate *quotaProjectAggregate,
	from, to time.Time,
) (quotaProjectCounters, map[string]quotaProjectCounters) {
	total := quotaProjectCounters{}
	hourly := make(map[string]quotaProjectCounters)
	if aggregate == nil {
		return total, hourly
	}
	for key, value := range aggregate.Hourly {
		start, errParse := time.Parse(time.RFC3339, key)
		if errParse != nil || !analyticsBucketOverlaps(start, start.Add(time.Hour), from, to) {
			continue
		}
		total = mergeQuotaProjectCounters(total, value)
		hourly[key] = mergeQuotaProjectCounters(hourly[key], value)
	}
	return total, hourly
}

func quotaObservationCountersBetween(aggregate *quotaObservationAggregate, from, to time.Time) quotaObservationCounters {
	total := quotaObservationCounters{}
	if aggregate == nil {
		return total
	}
	for key, value := range aggregate.Hourly {
		start, errParse := time.Parse(time.RFC3339, key)
		if errParse == nil && analyticsBucketOverlaps(start, start.Add(time.Hour), from, to) {
			total = mergeQuotaObservationCounters(total, value)
		}
	}
	return total
}

func quotaConsumptionConfidence(value quotaObservationCounters) string {
	if value.Samples == 0 || value.CoverageSeconds < 30*60 {
		return "collecting"
	}
	gapRatio := 1.0
	if value.ObservedDropPercent > 0 {
		gapRatio = value.ExternalOrEstimatorGapPercent / value.ObservedDropPercent
	} else if value.EstimatedLocalPercent == 0 {
		gapRatio = 0
	}
	if value.Samples >= 12 && value.CoverageSeconds >= int64(12*time.Hour/time.Second) && gapRatio <= 0.35 {
		return "high"
	}
	if value.Samples >= 3 && value.CoverageSeconds >= int64(time.Hour/time.Second) {
		return "medium"
	}
	return "low"
}

func quotaCapacityWindowHours(kind string) float64 {
	if kind == pluginapi.HostAuthQuotaWindowKindSession {
		return sessionUsageWindow.Hours()
	}
	return weeklyUsageWindow.Hours()
}

func peakQuotaProjectPercent(hourly map[string]quotaProjectCounters) float64 {
	peak := 0.0
	for _, value := range hourly {
		peak = math.Max(peak, value.AttributedPercent)
	}
	return peak
}

// Hour buckets are stored as percentage-point totals. During the first
// partially observed hour that raw total can be smaller than the correctly
// normalised average pace (for example, 3pp collected over 20 minutes is
// 9pp/h, not a 3pp/h peak). An hourly peak can never be below the average over
// the same interval, so keep that invariant until complete hour buckets are
// available.
func peakQuotaProjectPace(hourly map[string]quotaProjectCounters, averagePPPerHour float64) float64 {
	return math.Max(peakQuotaProjectPercent(hourly), averagePPPerHour)
}

func quotaConsumptionWindowKey(provider, kind, quotaModel string) string {
	return normalizeProvider(provider) + "\x1f" + strings.TrimSpace(kind) + "\x1f" + strings.ToLower(strings.TrimSpace(quotaModel))
}

func roundQuotaConsumptionView(view quotaConsumptionAnalyticsView) quotaConsumptionAnalyticsView {
	for windowIndex := range view.Windows {
		window := &view.Windows[windowIndex]
		if window.Pool != nil {
			pool := window.Pool
			pool.SubscriptionHours = adaptiveShadowRound(pool.SubscriptionHours)
			pool.ObservedDropPercent = adaptiveShadowRound(pool.ObservedDropPercent)
			pool.AttributedProjectPercent = adaptiveShadowRound(pool.AttributedProjectPercent)
			pool.AttributedLocalUnassignedPercent = adaptiveShadowRound(pool.AttributedLocalUnassignedPercent)
			pool.ExternalOrEstimatorGapPercent = adaptiveShadowRound(pool.ExternalOrEstimatorGapPercent)
			pool.AverageObservedPPPerSubHour = adaptiveShadowRound(pool.AverageObservedPPPerSubHour)
			pool.AverageExternalPPPerSubHour = adaptiveShadowRound(pool.AverageExternalPPPerSubHour)
		}
		for projectIndex := range window.Projects {
			project := &window.Projects[projectIndex]
			project.EstimatedPercent = adaptiveShadowRound(project.EstimatedPercent)
			project.AttributedPercent = adaptiveShadowRound(project.AttributedPercent)
			project.ShareOfAttributedPoolPercent = adaptiveShadowRound(project.ShareOfAttributedPoolPercent)
			project.AveragePPPerHour = adaptiveShadowRound(project.AveragePPPerHour)
			project.PeakHourlyPP = adaptiveShadowRound(project.PeakHourlyPP)
			project.SubscriptionWindowsConsumed = adaptiveShadowRound(project.SubscriptionWindowsConsumed)
			project.BaseX1EquivalentWindows = adaptiveShadowRound(project.BaseX1EquivalentWindows)
			for modelIndex := range project.Models {
				model := &project.Models[modelIndex]
				model.AttributedPercent = adaptiveShadowRound(model.AttributedPercent)
				model.ShareOfProjectPercent = adaptiveShadowRound(model.ShareOfProjectPercent)
				model.BaseX1EquivalentPercent = adaptiveShadowRound(model.BaseX1EquivalentPercent)
			}
			for planIndex := range project.Plans {
				plan := &project.Plans[planIndex]
				plan.AttributedPercent = adaptiveShadowRound(plan.AttributedPercent)
				plan.BaseX1EquivalentWindows = adaptiveShadowRound(plan.BaseX1EquivalentWindows)
				plan.AveragePPPerHour = adaptiveShadowRound(plan.AveragePPPerHour)
				plan.PeakHourlyPP = adaptiveShadowRound(plan.PeakHourlyPP)
				plan.EstimatedSubscriptionsAtAveragePace = adaptiveShadowRound(plan.EstimatedSubscriptionsAtAveragePace)
				plan.EstimatedSubscriptionsAtPeakPace = adaptiveShadowRound(plan.EstimatedSubscriptionsAtPeakPace)
				plan.EstimatedAdditionalAtPeakPace = adaptiveShadowRound(plan.EstimatedAdditionalAtPeakPace)
				plan.EstimatedSpareAtPeakPace = adaptiveShadowRound(plan.EstimatedSpareAtPeakPace)
			}
		}
	}
	return view
}
