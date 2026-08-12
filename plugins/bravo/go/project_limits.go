package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	projectLimitsPath         = "/v0/management/bravo/project-limits"
	projectLimitsPublicPath   = "/v1/bravo/limits"
	projectLimitsRateInterval = time.Hour
	projectLimitsUsageWindow  = 30 * 24 * time.Hour
	projectLimitsRateMaxKeys  = 4096
)

var projectLimitsNow = func() time.Time { return time.Now().UTC() }

var projectLimitsRate = struct {
	sync.Mutex
	Next map[string]time.Time
}{Next: make(map[string]time.Time)}

type projectLimitsDocumentation struct {
	Endpoint             string   `json:"endpoint"`
	Method               string   `json:"method"`
	Formats              []string `json:"formats"`
	RateLimitSeconds     int64    `json:"rate_limit_seconds"`
	JSONCommandTemplate  string   `json:"json_command_template"`
	TextCommandTemplate  string   `json:"text_command_template"`
	AuthenticationHeader string   `json:"authentication_header"`
}

type projectLimitsProjectView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type projectLimitWindowView struct {
	Kind                 string     `json:"kind"`
	Model                string     `json:"model,omitempty"`
	Status               string     `json:"status"`
	AccountsTotal        int        `json:"accounts_total"`
	AccountsAvailable    int        `json:"accounts_available"`
	AccountsBlocked      int        `json:"accounts_blocked"`
	BestRemainingPercent *float64   `json:"best_remaining_percent,omitempty"`
	ResetAt              *time.Time `json:"reset_at,omitempty"`
	ResetsInSeconds      *int64     `json:"resets_in_seconds,omitempty"`
	Freshness            string     `json:"freshness"`
}

type projectLimitProviderView struct {
	Provider          string                   `json:"provider"`
	Status            string                   `json:"status"`
	AccountsTotal     int                      `json:"accounts_total"`
	AccountsAvailable int                      `json:"accounts_available"`
	AccountsBlocked   int                      `json:"accounts_blocked"`
	Windows           []projectLimitWindowView `json:"windows"`
}

type projectLimitsUsageView struct {
	PeriodDays int                      `json:"period_days"`
	From       time.Time                `json:"from"`
	To         time.Time                `json:"to"`
	Summary    analyticsMetrics         `json:"summary"`
	Daily      []analyticsSeriesPoint   `json:"daily"`
	Providers  []analyticsBreakdownItem `json:"by_provider"`
	Models     []analyticsBreakdownItem `json:"by_model"`
}

type projectLimitsResponse struct {
	SchemaVersion     int                        `json:"schema_version"`
	Object            string                     `json:"object"`
	Project           projectLimitsProjectView   `json:"project"`
	GeneratedAt       time.Time                  `json:"generated_at"`
	NextAllowedAt     time.Time                  `json:"next_allowed_at"`
	RateLimitSeconds  int64                      `json:"rate_limit_seconds"`
	SnapshotFreshness string                     `json:"snapshot_freshness"`
	Providers         []projectLimitProviderView `json:"providers"`
	Usage             projectLimitsUsageView     `json:"usage"`
}

type projectLimitWindowAccumulator struct {
	Provider          string
	Kind              string
	Model             string
	AccountsTotal     int
	AccountsAvailable int
	AccountsBlocked   int
	BestRemaining     float64
	HasRemaining      bool
	NearestReset      time.Time
	Freshness         string
	Statuses          map[string]int
}

type projectLimitProviderAccumulator struct {
	Provider        string
	Accounts        map[string]struct{}
	RoutesAvailable int
	RoutesBlocked   int
	Statuses        map[string]int
	Windows         map[string]*projectLimitWindowAccumulator
}

func projectLimitsDocs() projectLimitsDocumentation {
	return projectLimitsDocumentation{
		Endpoint:             projectLimitsPublicPath,
		Method:               http.MethodGet,
		Formats:              []string{"json", "text"},
		RateLimitSeconds:     int64(projectLimitsRateInterval / time.Second),
		JSONCommandTemplate:  "curl -sS '<BRAVO_BASE_URL>/v1/bravo/limits?format=json' -H 'Authorization: Bearer <PROJECT_KEY>'",
		TextCommandTemplate:  "curl -sS '<BRAVO_BASE_URL>/v1/bravo/limits?format=text' -H 'Authorization: Bearer <PROJECT_KEY>'",
		AuthenticationHeader: "Authorization: Bearer <PROJECT_KEY>",
	}
}

func handleProjectLimits(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != projectLimitsPath {
		return nil, nil
	}
	if req.Method != http.MethodGet {
		return projectLimitsError(http.StatusMethodNotAllowed, "bravo_project_limits_method_not_allowed", "Статус проекта поддерживает только GET.", time.Time{})
	}
	cfg := loadedConfig()
	plaintext := requestCredential(req.Headers, req.Query)
	project, authenticated := matchSmartKey(cfg, plaintext)
	if !authenticated {
		return projectLimitsError(http.StatusUnauthorized, "bravo_smart_key_required", "Для получения лимитов нужен действующий ключ проекта Bravo.", time.Time{})
	}
	now := projectLimitsNow().UTC()
	allowed, nextAllowed := acquireProjectLimitsRate(project.ID, now)
	if !allowed {
		return projectLimitsError(
			http.StatusTooManyRequests,
			"bravo_project_limits_rate_limited",
			"Статус этого проекта можно обновлять не чаще одного раза в час.",
			nextAllowed,
		)
	}
	auths, errList := listHostAuths(req.HostCallbackID)
	if errList != nil {
		releaseProjectLimitsRate(project.ID, nextAllowed)
		return projectHostFailureJSON(errList)
	}
	auths = filterProjectAllowedAuths(project, auths)
	response := buildProjectLimitsResponse(cfg, project, auths, now, nextAllowed)
	format := strings.ToLower(strings.TrimSpace(firstQueryValue(req.Query, "format")))
	switch format {
	case "", "json":
		return projectLimitsJSON(http.StatusOK, response, 0)
	case "text":
		return projectLimitsText(http.StatusOK, renderProjectLimitsText(response), 0)
	default:
		releaseProjectLimitsRate(project.ID, nextAllowed)
		return projectLimitsError(http.StatusBadRequest, "bravo_project_limits_format_invalid", "Формат должен быть json или text.", time.Time{})
	}
}

func acquireProjectLimitsRate(projectID string, now time.Time) (bool, time.Time) {
	projectID = strings.TrimSpace(projectID)
	projectLimitsRate.Lock()
	defer projectLimitsRate.Unlock()
	for key, value := range projectLimitsRate.Next {
		if !value.After(now) {
			delete(projectLimitsRate.Next, key)
		}
	}
	if next := projectLimitsRate.Next[projectID]; next.After(now) {
		return false, next
	}
	if len(projectLimitsRate.Next) >= projectLimitsRateMaxKeys {
		return false, now.Add(projectLimitsRateInterval)
	}
	next := now.Add(projectLimitsRateInterval)
	projectLimitsRate.Next[projectID] = next
	return true, next
}

func releaseProjectLimitsRate(projectID string, expected time.Time) {
	projectLimitsRate.Lock()
	if projectLimitsRate.Next[projectID].Equal(expected) {
		delete(projectLimitsRate.Next, projectID)
	}
	projectLimitsRate.Unlock()
}

func resetProjectLimitsRateForTest() {
	projectLimitsRate.Lock()
	projectLimitsRate.Next = make(map[string]time.Time)
	projectLimitsRate.Unlock()
}

func buildProjectLimitsResponse(
	cfg pluginConfig,
	project smartKeyConfig,
	auths []pluginapi.HostAuthFileEntry,
	now time.Time,
	nextAllowed time.Time,
) projectLimitsResponse {
	providers := buildProjectLimitProviders(cfg, project, auths, now)
	freshness := ""
	for _, provider := range providers {
		for _, window := range provider.Windows {
			freshness = mergeProjectLimitFreshness(freshness, window.Freshness)
		}
	}
	if freshness == "" {
		freshness = "unknown"
	}
	analytics := collectAnalytics(analyticsQuery{
		From:      now.Add(-projectLimitsUsageWindow),
		To:        now,
		Interval:  analyticsIntervalDay,
		ProjectID: project.ID,
	}, now)
	return projectLimitsResponse{
		SchemaVersion:     1,
		Object:            "bravo.project_limits",
		Project:           projectLimitsProjectView{ID: project.ID, Name: project.Name},
		GeneratedAt:       now,
		NextAllowedAt:     nextAllowed,
		RateLimitSeconds:  int64(projectLimitsRateInterval / time.Second),
		SnapshotFreshness: freshness,
		Providers:         providers,
		Usage: projectLimitsUsageView{
			PeriodDays: int(projectLimitsUsageWindow / (24 * time.Hour)),
			From:       analytics.From,
			To:         analytics.To,
			Summary:    analytics.Summary,
			Daily:      analytics.Series,
			Providers:  analytics.Breakdown.Providers,
			Models:     analytics.Breakdown.Models,
		},
	}
}

func buildProjectLimitProviders(
	cfg pluginConfig,
	project smartKeyConfig,
	auths []pluginapi.HostAuthFileEntry,
	now time.Time,
) []projectLimitProviderView {
	primary := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	groups := make(map[string]*projectLimitProviderAccumulator)
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		quota := normalizedQuotaState(quotaSnapshot(authIndex))
		provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type, quota.Provider))
		if provider != "claude" && provider != "codex" {
			continue
		}
		group := groups[provider]
		if group == nil {
			group = &projectLimitProviderAccumulator{
				Provider: provider,
				Accounts: make(map[string]struct{}),
				Statuses: make(map[string]int),
				Windows:  make(map[string]*projectLimitWindowAccumulator),
			}
			groups[provider] = group
		}
		group.Accounts[authIndex] = struct{}{}
		subscription := subscriptionPolicy(cfg, authIndex)
		tariff := effectiveTariff(cfg, subscription, provider, quota)
		_, isPrimary := primary[authIndex]
		routeStatus := "available"
		for _, item := range []struct {
			kind   string
			model  string
			window quotaWindowState
			floor  float64
		}{
			{kind: pluginapi.HostAuthQuotaWindowKindSession, window: quota.Session, floor: projectLimitFloor(isPrimary, tariff.SessionFloorPercent)},
			{kind: pluginapi.HostAuthQuotaWindowKindWeekly, window: quota.Weekly, floor: projectLimitFloor(isPrimary, tariff.WeeklyFloorPercent)},
		} {
			status, freshness := projectLimitWindowStatus(cfg, quota, item.model, subscription, item.window, item.floor, now)
			appendProjectLimitWindow(group, item.kind, item.model, item.window, status, freshness, now)
			routeStatus = mergeProjectLimitRouteStatus(routeStatus, status)
		}
		for _, modelWindow := range quota.ModelWeekly {
			window := normalizeQuotaWindow(modelWindow.quotaWindowState)
			status, freshness := projectLimitWindowStatus(cfg, quota, modelWindow.Model, subscription, window, projectLimitFloor(isPrimary, tariff.WeeklyFloorPercent), now)
			appendProjectLimitWindow(group, pluginapi.HostAuthQuotaWindowKindModelWeekly, modelWindow.Model, window, status, freshness, now)
		}
		group.Statuses[routeStatus]++
		if routeStatus == "available" {
			group.RoutesAvailable++
		} else {
			group.RoutesBlocked++
		}
	}
	providers := make([]projectLimitProviderView, 0, len(groups))
	for _, group := range groups {
		windows := make([]projectLimitWindowView, 0, len(group.Windows))
		for _, window := range group.Windows {
			view := projectLimitWindowView{
				Kind:              window.Kind,
				Model:             window.Model,
				Status:            aggregateProjectLimitStatus(window.Statuses),
				AccountsTotal:     window.AccountsTotal,
				AccountsAvailable: window.AccountsAvailable,
				AccountsBlocked:   window.AccountsBlocked,
				Freshness:         window.Freshness,
			}
			if window.HasRemaining {
				remaining := window.BestRemaining
				view.BestRemainingPercent = &remaining
			}
			if !window.NearestReset.IsZero() {
				reset := window.NearestReset.UTC()
				seconds := int64(ceilProjectLimitDuration(reset.Sub(now), time.Second) / time.Second)
				if seconds < 0 {
					seconds = 0
				}
				view.ResetAt = &reset
				view.ResetsInSeconds = &seconds
			}
			windows = append(windows, view)
		}
		sort.Slice(windows, func(i, j int) bool {
			if windows[i].Kind != windows[j].Kind {
				return projectLimitWindowRank(windows[i].Kind) < projectLimitWindowRank(windows[j].Kind)
			}
			return windows[i].Model < windows[j].Model
		})
		providers = append(providers, projectLimitProviderView{
			Provider:          group.Provider,
			Status:            aggregateProjectLimitStatus(group.Statuses),
			AccountsTotal:     len(group.Accounts),
			AccountsAvailable: group.RoutesAvailable,
			AccountsBlocked:   group.RoutesBlocked,
			Windows:           windows,
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Provider < providers[j].Provider })
	return providers
}

func projectLimitFloor(primary bool, secondaryFloor float64) float64 {
	if primary {
		return 0
	}
	return secondaryFloor
}

func projectLimitWindowStatus(
	cfg pluginConfig,
	quota credentialQuotaState,
	model string,
	subscription subscriptionConfig,
	window quotaWindowState,
	floor float64,
	now time.Time,
) (string, string) {
	freshness := quotaFreshnessAt(quota, model, cfg, now)
	if !subscriptionEnabled(subscription) {
		return "disabled", freshness
	}
	if quotaRoutingConfidenceAt(quota, model, cfg, now) != "confirmed" {
		return "unknown", freshness
	}
	window = normalizeQuotaWindow(window)
	if window.RemainingPercent <= 0 {
		return "exhausted", freshness
	}
	if window.RemainingPercent <= floor {
		return "reserve_floor", freshness
	}
	return "available", freshness
}

func appendProjectLimitWindow(
	provider *projectLimitProviderAccumulator,
	kind, model string,
	window quotaWindowState,
	status, freshness string,
	now time.Time,
) {
	key := kind + "\x00" + strings.ToLower(strings.TrimSpace(model))
	accumulator := provider.Windows[key]
	if accumulator == nil {
		accumulator = &projectLimitWindowAccumulator{
			Provider:  provider.Provider,
			Kind:      kind,
			Model:     strings.ToLower(strings.TrimSpace(model)),
			Freshness: freshness,
			Statuses:  make(map[string]int),
		}
		provider.Windows[key] = accumulator
	}
	accumulator.AccountsTotal++
	accumulator.Statuses[status]++
	accumulator.Freshness = mergeProjectLimitFreshness(accumulator.Freshness, freshness)
	if status == "available" {
		accumulator.AccountsAvailable++
	} else {
		accumulator.AccountsBlocked++
	}
	if quota := normalizeQuotaWindow(window); status != "unknown" {
		if !accumulator.HasRemaining || quota.RemainingPercent > accumulator.BestRemaining {
			accumulator.BestRemaining = quota.RemainingPercent
			accumulator.HasRemaining = true
		}
		if status != "available" && quota.ResetAt.After(now) &&
			(accumulator.NearestReset.IsZero() || quota.ResetAt.Before(accumulator.NearestReset)) {
			accumulator.NearestReset = quota.ResetAt.UTC()
		}
	}
}

func aggregateProjectLimitStatus(statuses map[string]int) string {
	for _, status := range []string{"available", "reserve_floor", "exhausted", "unknown", "disabled"} {
		if statuses[status] > 0 {
			return status
		}
	}
	return "unknown"
}

func mergeProjectLimitRouteStatus(current, next string) string {
	if next == "available" {
		return current
	}
	if current == "available" {
		return next
	}
	priority := map[string]int{"unknown": 4, "exhausted": 3, "reserve_floor": 2, "disabled": 1}
	if priority[next] > priority[current] {
		return next
	}
	return current
}

func mergeProjectLimitFreshness(current, next string) string {
	priority := map[string]int{"unknown": 4, quotaFreshnessExpired: 3, quotaFreshnessStale: 2, quotaFreshnessFresh: 1}
	if priority[next] > priority[current] {
		return next
	}
	return current
}

func projectLimitWindowRank(kind string) int {
	switch kind {
	case pluginapi.HostAuthQuotaWindowKindSession:
		return 0
	case pluginapi.HostAuthQuotaWindowKindWeekly:
		return 1
	case pluginapi.HostAuthQuotaWindowKindModelWeekly:
		return 2
	default:
		return 3
	}
}

func projectLimitsError(status int, code, message string, nextAllowed time.Time) ([]byte, error) {
	body := map[string]any{"error": map[string]any{"code": code, "message": message}}
	retryAfter := int64(0)
	if !nextAllowed.IsZero() {
		retryAfter = int64(ceilProjectLimitDuration(nextAllowed.Sub(projectLimitsNow().UTC()), time.Second) / time.Second)
		if retryAfter < 1 {
			retryAfter = 1
		}
		errorBody := body["error"].(map[string]any)
		errorBody["next_allowed_at"] = nextAllowed.UTC()
		errorBody["retry_after_seconds"] = retryAfter
	}
	return projectLimitsJSON(status, body, retryAfter)
}

func projectLimitsJSON(status int, value any, retryAfter int64) ([]byte, error) {
	body, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return projectLimitsResponseEnvelope(status, "application/json; charset=utf-8", body, retryAfter)
}

func projectLimitsText(status int, value string, retryAfter int64) ([]byte, error) {
	return projectLimitsResponseEnvelope(status, "text/plain; charset=utf-8", []byte(value), retryAfter)
}

func projectLimitsResponseEnvelope(status int, contentType string, body []byte, retryAfter int64) ([]byte, error) {
	headers := http.Header{
		"Content-Type":           []string{contentType},
		"X-Content-Type-Options": []string{"nosniff"},
		"Cache-Control":          []string{"no-store"},
	}
	if retryAfter > 0 {
		headers.Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body})
}

func renderProjectLimitsText(response projectLimitsResponse) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Проект Bravo: %s\n", response.Project.Name)
	fmt.Fprintf(&builder, "Снимок: %s (%s)\n", response.GeneratedAt.Format(time.RFC3339), response.SnapshotFreshness)
	for _, provider := range response.Providers {
		fmt.Fprintf(&builder, "\n%s: %s, доступно %d/%d подписок\n", strings.ToUpper(provider.Provider), projectLimitStatusRU(provider.Status), provider.AccountsAvailable, provider.AccountsTotal)
		for _, window := range provider.Windows {
			name := projectLimitWindowNameRU(window)
			remaining := "не подтвержден"
			if window.BestRemainingPercent != nil {
				remaining = fmt.Sprintf("%.1f%%", *window.BestRemainingPercent)
			}
			fmt.Fprintf(&builder, "  %s: %s, остаток %s", name, projectLimitStatusRU(window.Status), remaining)
			if window.ResetsInSeconds != nil {
				fmt.Fprintf(&builder, ", сброс через %s", formatProjectLimitDuration(time.Duration(*window.ResetsInSeconds)*time.Second))
			}
			builder.WriteByte('\n')
		}
	}
	fmt.Fprintf(&builder, "\nUsage за %d дней: %d запросов, %d токенов, ошибок %d (%.1f%%)\n",
		response.Usage.PeriodDays,
		response.Usage.Summary.Requests,
		response.Usage.Summary.TotalTokens,
		response.Usage.Summary.Failures,
		response.Usage.Summary.FailureRatePercent,
	)
	fmt.Fprintf(&builder, "Следующее обновление разрешено: %s\n", response.NextAllowedAt.Format(time.RFC3339))
	return builder.String()
}

func projectLimitWindowNameRU(window projectLimitWindowView) string {
	switch window.Kind {
	case pluginapi.HostAuthQuotaWindowKindSession:
		return "лимит сессии"
	case pluginapi.HostAuthQuotaWindowKindWeekly:
		return "общий недельный лимит"
	case pluginapi.HostAuthQuotaWindowKindModelWeekly:
		if window.Model != "" {
			return "лимит модели " + friendlyModelName(window.Model)
		}
		return "лимит модели"
	default:
		return window.Kind
	}
}

func projectLimitStatusRU(status string) string {
	switch status {
	case "available":
		return "доступен"
	case "reserve_floor":
		return "удерживается внутренним резервом"
	case "exhausted":
		return "исчерпан"
	case "disabled":
		return "отключён"
	default:
		return "состояние не подтверждено"
	}
}

func formatProjectLimitDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	minutes := int64(ceilProjectLimitDuration(value, time.Minute) / time.Minute)
	days := int64(0)
	if minutes >= 48*60 {
		days = minutes / (24 * 60)
		minutes %= 24 * 60
	}
	hours := minutes / 60
	minutes %= 60
	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d д", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d ч", hours))
	}
	if minutes > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d мин", minutes))
	}
	return strings.Join(parts, " ")
}

func ceilProjectLimitDuration(value, unit time.Duration) time.Duration {
	if value <= 0 || unit <= 0 {
		return 0
	}
	whole := value / unit
	if value%unit != 0 {
		whole++
	}
	return whole * unit
}
