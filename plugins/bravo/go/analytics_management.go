package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	analyticsIntervalHour = "hour"
	analyticsIntervalDay  = "day"
)

type analyticsQuery struct {
	From           time.Time
	To             time.Time
	Interval       string
	ProjectID      string
	SubscriptionID string
	Provider       string
	Model          string
}

type analyticsFilterInput struct {
	ProjectID      string `json:"project_id"`
	SubscriptionID string `json:"subscription_id"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
}

type analyticsFilterView struct {
	ProjectID      string `json:"project_id,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
}

type analyticsMetrics struct {
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	Failures            int64   `json:"failures"`
	LatencyMS           int64   `json:"latency_ms"`
	AverageLatencyMS    float64 `json:"average_latency_ms"`
	FailureRatePercent  float64 `json:"failure_rate_percent"`
}

type analyticsSeriesPoint struct {
	Start time.Time        `json:"start"`
	End   time.Time        `json:"end"`
	Usage analyticsMetrics `json:"usage"`
}

type analyticsBreakdownItem struct {
	ProjectID      string           `json:"project_id,omitempty"`
	SubscriptionID string           `json:"subscription_id,omitempty"`
	AuthIndex      string           `json:"auth_index,omitempty"`
	Label          string           `json:"label,omitempty"`
	Provider       string           `json:"provider,omitempty"`
	Model          string           `json:"model,omitempty"`
	LogicalModel   string           `json:"logical_model,omitempty"`
	Usage          analyticsMetrics `json:"usage"`
}

type analyticsBreakdown struct {
	Projects                  []analyticsBreakdownItem `json:"projects"`
	Subscriptions             []analyticsBreakdownItem `json:"subscriptions"`
	Providers                 []analyticsBreakdownItem `json:"providers"`
	Models                    []analyticsBreakdownItem `json:"models"`
	ProjectSubscriptionModels []analyticsBreakdownItem `json:"project_subscription_models"`
}

type analyticsRetentionView struct {
	HourlyDays int `json:"hourly_days"`
	DailyDays  int `json:"daily_days"`
}

type analyticsResponse struct {
	SchemaVersion         int                    `json:"schema_version"`
	From                  time.Time              `json:"from"`
	To                    time.Time              `json:"to"`
	Interval              string                 `json:"interval"`
	Bucket                string                 `json:"bucket"`
	Filters               analyticsFilterView    `json:"filters"`
	Retention             analyticsRetentionView `json:"retention"`
	CoverageFrom          *time.Time             `json:"coverage_from"`
	BreakdownCoverageFrom *time.Time             `json:"breakdown_coverage_from"`
	Summary               analyticsMetrics       `json:"summary"`
	Series                []analyticsSeriesPoint `json:"series"`
	Breakdown             analyticsBreakdown     `json:"breakdown"`
	GeneratedAt           time.Time              `json:"generated_at"`
}

func handleAnalyticsManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != "/v0/management/bravo/analytics" {
		return nil, nil
	}
	if req.Method != http.MethodGet {
		return analyticsFailureJSON(
			http.StatusMethodNotAllowed,
			"bravo_analytics_method_not_allowed",
			"Analytics supports GET only.",
		)
	}
	query, errQuery := parseAnalyticsQuery(req.Query, time.Now().UTC())
	if errQuery != nil {
		return analyticsFailureJSON(http.StatusBadRequest, "bravo_analytics_query_invalid", errQuery.Error())
	}
	return managementJSON(http.StatusOK, collectAnalytics(query, time.Now().UTC()))
}

func parseAnalyticsQuery(values url.Values, now time.Time) (analyticsQuery, error) {
	input := analyticsFilterInput{
		ProjectID:      strings.TrimSpace(values.Get("project_id")),
		SubscriptionID: strings.TrimSpace(values.Get("subscription_id")),
		Provider:       strings.TrimSpace(values.Get("provider")),
		Model:          strings.TrimSpace(values.Get("model")),
	}
	if rawFilters := strings.TrimSpace(values.Get("filters")); rawFilters != "" {
		filters, errFilters := decodeAnalyticsFilters(rawFilters)
		if errFilters != nil {
			return analyticsQuery{}, errFilters
		}
		var errMerge error
		input.ProjectID, errMerge = mergeAnalyticsFilter("project_id", input.ProjectID, filters.ProjectID)
		if errMerge != nil {
			return analyticsQuery{}, errMerge
		}
		input.SubscriptionID, errMerge = mergeAnalyticsFilter("subscription_id", input.SubscriptionID, filters.SubscriptionID)
		if errMerge != nil {
			return analyticsQuery{}, errMerge
		}
		input.Provider, errMerge = mergeAnalyticsFilter("provider", input.Provider, filters.Provider)
		if errMerge != nil {
			return analyticsQuery{}, errMerge
		}
		input.Model, errMerge = mergeAnalyticsFilter("model", input.Model, filters.Model)
		if errMerge != nil {
			return analyticsQuery{}, errMerge
		}
	}

	interval := strings.ToLower(strings.TrimSpace(values.Get("interval")))
	bucket := strings.ToLower(strings.TrimSpace(values.Get("bucket")))
	if interval == "" {
		interval = bucket
	} else if bucket != "" && bucket != interval {
		return analyticsQuery{}, fmt.Errorf("interval and bucket must match when both are provided")
	}
	if interval == "" {
		interval = analyticsIntervalDay
	}
	if interval != analyticsIntervalHour && interval != analyticsIntervalDay {
		return analyticsQuery{}, fmt.Errorf("interval must be hour or day")
	}

	to := now.UTC()
	if rawTo := strings.TrimSpace(values.Get("to")); rawTo != "" {
		parsed, errParse := parseAnalyticsBoundary(rawTo, true)
		if errParse != nil {
			return analyticsQuery{}, fmt.Errorf("invalid to: %w", errParse)
		}
		// A date picker naturally sends today's date as the inclusive end.
		// Keep it useful during the current day instead of rejecting tomorrow
		// midnight as a future timestamp.
		if len(rawTo) == len(dailyUsageBucketLayout) &&
			parsed.After(now.UTC()) &&
			parsed.Sub(now.UTC()) <= 24*time.Hour {
			parsed = now.UTC()
		}
		to = parsed
	}
	defaultWindow := 7 * 24 * time.Hour
	if interval == analyticsIntervalHour {
		defaultWindow = 24 * time.Hour
	}
	from := to.Add(-defaultWindow)
	if rawFrom := strings.TrimSpace(values.Get("from")); rawFrom != "" {
		parsed, errParse := parseAnalyticsBoundary(rawFrom, false)
		if errParse != nil {
			return analyticsQuery{}, fmt.Errorf("invalid from: %w", errParse)
		}
		from = parsed
	}
	if !from.Before(to) {
		return analyticsQuery{}, fmt.Errorf("from must be earlier than to")
	}
	maxWindow := dailyUsageRetention
	if interval == analyticsIntervalHour {
		maxWindow = hourlyUsageRetention
	}
	if to.Sub(from) > maxWindow {
		return analyticsQuery{}, fmt.Errorf("%s interval supports at most %d days", interval, int(maxWindow/(24*time.Hour)))
	}
	if to.After(now.UTC().Add(5 * time.Minute)) {
		return analyticsQuery{}, fmt.Errorf("to cannot be in the future")
	}
	if input.ProjectID != "" && !validProjectID(input.ProjectID) {
		return analyticsQuery{}, fmt.Errorf("project_id is invalid")
	}
	if input.SubscriptionID != "" && !validAnalyticsSubscriptionID(input.SubscriptionID) {
		return analyticsQuery{}, fmt.Errorf("subscription_id is invalid")
	}
	input.Provider = normalizeProvider(input.Provider)
	if len(input.Provider) > 64 || strings.ContainsAny(input.Provider, "\x00\r\n") {
		return analyticsQuery{}, fmt.Errorf("provider is invalid")
	}
	if len(input.Model) > 256 || strings.ContainsAny(input.Model, "\x00\r\n") {
		return analyticsQuery{}, fmt.Errorf("model is invalid")
	}
	return analyticsQuery{
		From:           from.UTC(),
		To:             to.UTC(),
		Interval:       interval,
		ProjectID:      input.ProjectID,
		SubscriptionID: input.SubscriptionID,
		Provider:       input.Provider,
		Model:          input.Model,
	}, nil
}

func decodeAnalyticsFilters(raw string) (analyticsFilterInput, error) {
	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(raw), &object); errUnmarshal != nil {
		return analyticsFilterInput{}, fmt.Errorf("filters must be a JSON object")
	}
	if object == nil {
		return analyticsFilterInput{}, fmt.Errorf("filters must be a JSON object")
	}
	allowed := map[string]bool{
		"project_id":      true,
		"subscription_id": true,
		"provider":        true,
		"model":           true,
	}
	for key := range object {
		if !allowed[key] {
			return analyticsFilterInput{}, fmt.Errorf("filters contains unknown field %q", key)
		}
	}
	var filters analyticsFilterInput
	if errUnmarshal := json.Unmarshal([]byte(raw), &filters); errUnmarshal != nil {
		return analyticsFilterInput{}, fmt.Errorf("filters values must be strings")
	}
	filters.ProjectID = strings.TrimSpace(filters.ProjectID)
	filters.SubscriptionID = strings.TrimSpace(filters.SubscriptionID)
	filters.Provider = strings.TrimSpace(filters.Provider)
	filters.Model = strings.TrimSpace(filters.Model)
	return filters, nil
}

func mergeAnalyticsFilter(name, direct, nested string) (string, error) {
	if direct != "" && nested != "" && direct != nested {
		return "", fmt.Errorf("%s conflicts with filters.%s", name, name)
	}
	if direct != "" {
		return direct, nil
	}
	return nested, nil
}

func parseAnalyticsBoundary(raw string, end bool) (time.Time, error) {
	if value, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return value.UTC(), nil
	}
	value, errParse := time.Parse(dailyUsageBucketLayout, raw)
	if errParse != nil {
		return time.Time{}, fmt.Errorf("use RFC3339 or YYYY-MM-DD")
	}
	if end {
		value = value.Add(24 * time.Hour)
	}
	return value.UTC(), nil
}

func collectAnalytics(query analyticsQuery, generatedAt time.Time) analyticsResponse {
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()

	state := &bravoUsageState.state
	selected := selectAnalyticsAggregates(state, query)
	summary := aggregateAnalyticsUsage(selected, query)
	coverage := earliestAnalyticsCoverage(selected, query.Interval)
	breakdownCoverage := optionalAnalyticsTime(state.DimensionalStartedAt)
	return analyticsResponse{
		SchemaVersion: usageStateSchemaVersion,
		From:          query.From,
		To:            query.To,
		Interval:      query.Interval,
		Bucket:        query.Interval,
		Filters: analyticsFilterView{
			ProjectID:      query.ProjectID,
			SubscriptionID: query.SubscriptionID,
			Provider:       query.Provider,
			Model:          query.Model,
		},
		Retention: analyticsRetentionView{
			HourlyDays: int(hourlyUsageRetention / (24 * time.Hour)),
			DailyDays:  int(dailyUsageRetention / (24 * time.Hour)),
		},
		CoverageFrom:          coverage,
		BreakdownCoverageFrom: breakdownCoverage,
		Summary:               analyticsMetricsFromCounters(summary),
		Series:                buildAnalyticsSeries(selected, query),
		Breakdown:             buildAnalyticsBreakdown(state, query),
		GeneratedAt:           generatedAt.UTC(),
	}
}

func selectAnalyticsAggregates(state *persistedUsageState, query analyticsQuery) []*usageAggregate {
	if state == nil {
		return nil
	}
	hasProject := query.ProjectID != ""
	hasSubscription := query.SubscriptionID != ""
	hasProvider := query.Provider != ""
	hasModel := query.Model != ""
	switch {
	case !hasProject && !hasSubscription && !hasProvider && !hasModel:
		return []*usageAggregate{&state.GlobalTotal}
	case hasProject && !hasSubscription && !hasProvider && !hasModel:
		if aggregate := state.ProjectTotals[query.ProjectID]; aggregate != nil {
			return []*usageAggregate{aggregate}
		}
		return nil
	case hasSubscription && !hasProject && !hasProvider && !hasModel:
		for authIndex, aggregate := range state.AuthTotals {
			if analyticsSubscriptionID(authIndex) == query.SubscriptionID {
				return []*usageAggregate{aggregate}
			}
		}
		return nil
	case hasProvider && !hasProject && !hasSubscription && !hasModel:
		if aggregate := state.ProviderTotals[query.Provider]; aggregate != nil {
			return []*usageAggregate{aggregate}
		}
		return nil
	case hasModel && !hasProject && !hasSubscription:
		var out []*usageAggregate
		for _, aggregate := range state.ModelTotals {
			if aggregate != nil && analyticsModelMatches(query.Model, aggregate.Model, aggregate.LogicalModel) &&
				(query.Provider == "" || query.Provider == aggregate.Provider) {
				out = append(out, &aggregate.Usage)
			}
		}
		return out
	default:
		var out []*usageAggregate
		for _, aggregate := range state.ProjectSubscriptionModelTotals {
			if aggregate != nil && analyticsDimensionsMatch(query, aggregate) {
				out = append(out, &aggregate.Usage)
			}
		}
		return out
	}
}

func analyticsDimensionsMatch(query analyticsQuery, aggregate *projectSubscriptionModelUsageAggregate) bool {
	if aggregate == nil {
		return false
	}
	return (query.ProjectID == "" || query.ProjectID == aggregate.ProjectID) &&
		(query.SubscriptionID == "" || query.SubscriptionID == analyticsSubscriptionID(aggregate.AuthIndex)) &&
		(query.Provider == "" || query.Provider == aggregate.Provider) &&
		(query.Model == "" || analyticsModelMatches(query.Model, aggregate.Model, aggregate.LogicalModel))
}

func analyticsModelMatches(filter, model, logicalModel string) bool {
	filter = strings.TrimSpace(filter)
	return filter == strings.TrimSpace(model) || filter == strings.TrimSpace(logicalModel)
}

func aggregateAnalyticsUsage(aggregates []*usageAggregate, query analyticsQuery) usageCounters {
	var out usageCounters
	for _, aggregate := range aggregates {
		out = mergeUsageCounters(out, analyticsUsageBetween(aggregate, query.From, query.To, query.Interval))
	}
	return out
}

func analyticsUsageBetween(aggregate *usageAggregate, from, to time.Time, interval string) usageCounters {
	if aggregate == nil {
		return usageCounters{}
	}
	var out usageCounters
	if interval == analyticsIntervalHour {
		for key, value := range aggregate.Hourly {
			at, errParse := time.Parse(time.RFC3339, key)
			if errParse == nil && analyticsBucketOverlaps(at, at.Add(time.Hour), from, to) {
				out = mergeUsageCounters(out, value)
			}
		}
		return out
	}
	for key, value := range aggregate.Daily {
		at, errParse := time.Parse(dailyUsageBucketLayout, key)
		if errParse == nil && analyticsBucketOverlaps(at, at.Add(24*time.Hour), from, to) {
			out = mergeUsageCounters(out, value)
		}
	}
	return out
}

func analyticsBucketOverlaps(start, end, from, to time.Time) bool {
	return start.Before(to) && end.After(from)
}

func buildAnalyticsSeries(aggregates []*usageAggregate, query analyticsQuery) []analyticsSeriesPoint {
	step := analyticsIntervalDuration(query.Interval)
	start := analyticsBucketStart(query.From, query.Interval)
	series := make([]analyticsSeriesPoint, 0, int(query.To.Sub(start)/step)+1)
	for at := start; at.Before(query.To); at = at.Add(step) {
		end := at.Add(step)
		var value usageCounters
		for _, aggregate := range aggregates {
			value = mergeUsageCounters(value, analyticsUsageBetween(aggregate, at, end, query.Interval))
		}
		series = append(series, analyticsSeriesPoint{
			Start: at,
			End:   end,
			Usage: analyticsMetricsFromCounters(value),
		})
	}
	return series
}

func analyticsIntervalDuration(interval string) time.Duration {
	if interval == analyticsIntervalHour {
		return time.Hour
	}
	return 24 * time.Hour
}

func analyticsBucketStart(value time.Time, interval string) time.Time {
	value = value.UTC()
	if interval == analyticsIntervalHour {
		return value.Truncate(time.Hour)
	}
	return value.Truncate(24 * time.Hour)
}

func earliestAnalyticsCoverage(aggregates []*usageAggregate, interval string) *time.Time {
	var earliest time.Time
	for _, aggregate := range aggregates {
		if aggregate == nil {
			continue
		}
		if interval == analyticsIntervalHour {
			for key := range aggregate.Hourly {
				at, errParse := time.Parse(time.RFC3339, key)
				if errParse == nil && (earliest.IsZero() || at.Before(earliest)) {
					earliest = at
				}
			}
			continue
		}
		for key := range aggregate.Daily {
			at, errParse := time.Parse(dailyUsageBucketLayout, key)
			if errParse == nil && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
	}
	return optionalAnalyticsTime(earliest)
}

func optionalAnalyticsTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func analyticsMetricsFromCounters(value usageCounters) analyticsMetrics {
	metrics := analyticsMetrics{
		Requests:            value.Requests,
		InputTokens:         value.InputTokens,
		OutputTokens:        value.OutputTokens,
		ReasoningTokens:     value.ReasoningTokens,
		CachedTokens:        value.CachedTokens,
		CacheReadTokens:     value.CacheReadTokens,
		CacheCreationTokens: value.CacheCreationTokens,
		TotalTokens:         value.TotalTokens,
		Failures:            value.Failures,
		LatencyMS:           value.LatencyMS,
	}
	if value.Requests > 0 {
		metrics.AverageLatencyMS = float64(value.LatencyMS) / float64(value.Requests)
		metrics.FailureRatePercent = float64(value.Failures) * 100 / float64(value.Requests)
	}
	return metrics
}

func buildAnalyticsBreakdown(state *persistedUsageState, query analyticsQuery) analyticsBreakdown {
	out := analyticsBreakdown{
		Projects:                  []analyticsBreakdownItem{},
		Subscriptions:             []analyticsBreakdownItem{},
		Providers:                 []analyticsBreakdownItem{},
		Models:                    []analyticsBreakdownItem{},
		ProjectSubscriptionModels: []analyticsBreakdownItem{},
	}
	if state == nil {
		return out
	}
	projects := make(map[string]usageCounters)
	subscriptions := make(map[string]usageCounters)
	subscriptionProviders := make(map[string]string)
	providers := make(map[string]usageCounters)
	models := make(map[string]usageCounters)
	modelDimensions := make(map[string]projectSubscriptionModelUsageAggregate)
	cross := make(map[string]usageCounters)
	crossDimensions := make(map[string]projectSubscriptionModelUsageAggregate)
	for _, aggregate := range state.ProjectSubscriptionModelTotals {
		if aggregate == nil || !analyticsDimensionsMatch(query, aggregate) {
			continue
		}
		value := analyticsUsageBetween(&aggregate.Usage, query.From, query.To, query.Interval)
		if usageCountersEmpty(value) {
			continue
		}
		if aggregate.ProjectID != "" {
			projects[aggregate.ProjectID] = mergeUsageCounters(projects[aggregate.ProjectID], value)
		}
		if aggregate.AuthIndex != "" {
			subscriptionID := analyticsSubscriptionID(aggregate.AuthIndex)
			subscriptions[subscriptionID] = mergeUsageCounters(subscriptions[subscriptionID], value)
			if subscriptionProviders[subscriptionID] == "" {
				subscriptionProviders[subscriptionID] = aggregate.Provider
			} else if subscriptionProviders[subscriptionID] != aggregate.Provider {
				subscriptionProviders[subscriptionID] = ""
			}
		}
		if aggregate.Provider != "" {
			providers[aggregate.Provider] = mergeUsageCounters(providers[aggregate.Provider], value)
		}
		modelKey := usageDimensionKey("", "", aggregate.Provider, aggregate.Model, aggregate.LogicalModel)
		models[modelKey] = mergeUsageCounters(models[modelKey], value)
		modelDimensions[modelKey] = *aggregate
		crossKey := usageDimensionKey(
			aggregate.ProjectID,
			aggregate.AuthIndex,
			aggregate.Provider,
			aggregate.Model,
			aggregate.LogicalModel,
		)
		cross[crossKey] = mergeUsageCounters(cross[crossKey], value)
		crossDimensions[crossKey] = *aggregate
	}
	for projectID, value := range projects {
		out.Projects = append(out.Projects, analyticsBreakdownItem{
			ProjectID: projectID,
			Usage:     analyticsMetricsFromCounters(value),
		})
	}
	for subscriptionID, value := range subscriptions {
		provider := subscriptionProviders[subscriptionID]
		out.Subscriptions = append(out.Subscriptions, analyticsBreakdownItem{
			SubscriptionID: subscriptionID,
			AuthIndex:      subscriptionID,
			Label:          analyticsSubscriptionLabel(subscriptionID, provider),
			Provider:       provider,
			Usage:          analyticsMetricsFromCounters(value),
		})
	}
	for provider, value := range providers {
		out.Providers = append(out.Providers, analyticsBreakdownItem{
			Provider: provider,
			Usage:    analyticsMetricsFromCounters(value),
		})
	}
	for key, value := range models {
		dimensions := modelDimensions[key]
		out.Models = append(out.Models, analyticsBreakdownItem{
			Provider:     dimensions.Provider,
			Model:        dimensions.Model,
			LogicalModel: dimensions.LogicalModel,
			Usage:        analyticsMetricsFromCounters(value),
		})
	}
	for key, value := range cross {
		dimensions := crossDimensions[key]
		subscriptionID := analyticsSubscriptionID(dimensions.AuthIndex)
		out.ProjectSubscriptionModels = append(out.ProjectSubscriptionModels, analyticsBreakdownItem{
			ProjectID:      dimensions.ProjectID,
			SubscriptionID: subscriptionID,
			AuthIndex:      subscriptionID,
			Label:          analyticsSubscriptionLabel(subscriptionID, dimensions.Provider),
			Provider:       dimensions.Provider,
			Model:          dimensions.Model,
			LogicalModel:   dimensions.LogicalModel,
			Usage:          analyticsMetricsFromCounters(value),
		})
	}
	sortAnalyticsBreakdown(&out)
	return out
}

func sortAnalyticsBreakdown(out *analyticsBreakdown) {
	if out == nil {
		return
	}
	byUsageThenIdentity := func(values []analyticsBreakdownItem) {
		sort.Slice(values, func(left, right int) bool {
			if values[left].Usage.TotalTokens != values[right].Usage.TotalTokens {
				return values[left].Usage.TotalTokens > values[right].Usage.TotalTokens
			}
			leftKey := values[left].ProjectID + values[left].SubscriptionID + values[left].Provider +
				values[left].LogicalModel + values[left].Model
			rightKey := values[right].ProjectID + values[right].SubscriptionID + values[right].Provider +
				values[right].LogicalModel + values[right].Model
			return leftKey < rightKey
		})
	}
	byUsageThenIdentity(out.Projects)
	byUsageThenIdentity(out.Subscriptions)
	byUsageThenIdentity(out.Providers)
	byUsageThenIdentity(out.Models)
	byUsageThenIdentity(out.ProjectSubscriptionModels)
}

func analyticsSubscriptionID(authIndex string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("bravo-analytics-subscription-v1\x00" + authIndex))
	return fmt.Sprintf("sub_%x", sum[:8])
}

func validAnalyticsSubscriptionID(value string) bool {
	if len(value) != len("sub_")+16 || !strings.HasPrefix(value, "sub_") {
		return false
	}
	for _, char := range value[len("sub_"):] {
		if (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') {
			continue
		}
		return false
	}
	return true
}

func analyticsSubscriptionLabel(subscriptionID, provider string) string {
	if subscriptionID == "" {
		return ""
	}
	suffix := strings.TrimPrefix(subscriptionID, "sub_")
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	switch normalizeProvider(provider) {
	case "claude":
		return "Claude · " + suffix
	case "codex":
		return "OpenAI · " + suffix
	default:
		return "Subscription · " + suffix
	}
}

func analyticsFailureJSON(status int, code, message string) ([]byte, error) {
	return managementJSON(status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}
