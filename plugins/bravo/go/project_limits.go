package main

import (
	"crypto/sha256"
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
	projectLimitsPath            = "/v0/management/bravo/project-limits"
	projectLimitsPublicPath      = "/v1/bravo/limits"
	projectLimitsRefreshInterval = 5 * time.Minute
	projectLimitsUsageWindow     = 30 * 24 * time.Hour
	projectLimitsCacheMaxKeys    = 4096
)

var projectLimitsNow = func() time.Time { return time.Now().UTC() }

type projectLimitsCacheEntry struct {
	Response  projectLimitsResponse
	ExpiresAt time.Time
}

type projectLimitsCacheFlight struct {
	Done chan struct{}
}

var projectLimitsCache = struct {
	sync.Mutex
	Entries map[string]projectLimitsCacheEntry
	Flights map[string]*projectLimitsCacheFlight
}{
	Entries: make(map[string]projectLimitsCacheEntry),
	Flights: make(map[string]*projectLimitsCacheFlight),
}

type projectLimitsDocumentation struct {
	Endpoint               string   `json:"endpoint"`
	Method                 string   `json:"method"`
	Formats                []string `json:"formats"`
	RateLimitSeconds       int64    `json:"rate_limit_seconds"`
	RefreshIntervalSeconds int64    `json:"refresh_interval_seconds"`
	RepeatedRequests       string   `json:"repeated_requests"`
	JSONCommandTemplate    string   `json:"json_command_template"`
	TextCommandTemplate    string   `json:"text_command_template"`
	AuthenticationHeader   string   `json:"authentication_header"`
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
	Cached            bool                       `json:"cached"`
	GeneratedAt       time.Time                  `json:"generated_at"`
	NextAllowedAt     time.Time                  `json:"next_allowed_at"`
	NextRefreshAt     time.Time                  `json:"next_refresh_at"`
	RateLimitSeconds  int64                      `json:"rate_limit_seconds"`
	RefreshSeconds    int64                      `json:"refresh_interval_seconds"`
	SnapshotFreshness string                     `json:"snapshot_freshness"`
	Providers         []projectLimitProviderView `json:"providers"`
	AdaptiveAllocator adaptiveShadowPublicView   `json:"adaptive_allocator"`
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
		Endpoint:               projectLimitsPublicPath,
		Method:                 http.MethodGet,
		Formats:                []string{"json", "text"},
		RateLimitSeconds:       int64(projectLimitsRefreshInterval / time.Second),
		RefreshIntervalSeconds: int64(projectLimitsRefreshInterval / time.Second),
		RepeatedRequests:       "cached_http_200",
		JSONCommandTemplate:    "curl --fail-with-body -sS '<BRAVO_BASE_URL>/v1/bravo/limits?format=json' -H 'Authorization: Bearer <PROJECT_KEY>'",
		TextCommandTemplate:    "curl --fail-with-body -sS '<BRAVO_BASE_URL>/v1/bravo/limits?format=text' -H 'Authorization: Bearer <PROJECT_KEY>'",
		AuthenticationHeader:   "Authorization: Bearer <PROJECT_KEY>",
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
	format := strings.ToLower(strings.TrimSpace(firstQueryValue(req.Query, "format")))
	if format != "" && format != "json" && format != "text" {
		return projectLimitsError(http.StatusBadRequest, "bravo_project_limits_format_invalid", "Формат должен быть json или text.", time.Time{})
	}
	cfg := loadedConfig()
	plaintext := requestCredential(req.Headers, req.Query)
	project, authenticated := matchSmartKey(cfg, plaintext)
	if !authenticated {
		return projectLimitsError(http.StatusUnauthorized, "bravo_smart_key_required", "Для получения лимитов нужен действующий ключ проекта Bravo.", time.Time{})
	}
	now := projectLimitsNow().UTC()
	cacheKey, errFingerprint := projectLimitsCacheKey(cfg, project)
	if errFingerprint != nil {
		return projectLimitsError(http.StatusInternalServerError, "bravo_project_limits_cache_unavailable", "Не удалось подготовить локальный снимок лимитов. Повторите запрос.", time.Time{})
	}
	for {
		response, hit, wait := lookupProjectLimitsCache(cacheKey, now)
		if hit {
			response.Cached = true
			return renderProjectLimitsResponse(format, response, "HIT")
		}
		if wait == nil {
			break
		}
		<-wait
	}
	auths, errList := listHostAuths(req.HostCallbackID)
	if errList != nil {
		finishProjectLimitsCache(cacheKey, projectLimitsResponse{}, time.Time{}, false)
		return projectHostFailureJSON(errList)
	}
	auths = filterProjectAllowedAuths(project, auths)
	nextAllowed := now.Add(projectLimitsRefreshInterval)
	response := buildProjectLimitsResponse(cfg, project, auths, now, nextAllowed)
	finishProjectLimitsCache(cacheKey, response, nextAllowed, true)
	return renderProjectLimitsResponse(format, response, "MISS")
}

func projectLimitsCacheKey(cfg pluginConfig, project smartKeyConfig) (string, error) {
	projectConfig := cfg
	projectConfig.SmartKeys = []smartKeyConfig{project}
	encoded, errMarshal := json.Marshal(projectConfig)
	if errMarshal != nil {
		return "", errMarshal
	}
	fingerprint := sha256.Sum256(encoded)
	return strings.TrimSpace(project.ID) + ":" + fmt.Sprintf("%x", fingerprint[:]), nil
}

func lookupProjectLimitsCache(cacheKey string, now time.Time) (projectLimitsResponse, bool, <-chan struct{}) {
	projectLimitsCache.Lock()
	defer projectLimitsCache.Unlock()
	for key, entry := range projectLimitsCache.Entries {
		if !entry.ExpiresAt.After(now) {
			delete(projectLimitsCache.Entries, key)
		}
	}
	if entry, ok := projectLimitsCache.Entries[cacheKey]; ok && entry.ExpiresAt.After(now) {
		return entry.Response, true, nil
	}
	if flight := projectLimitsCache.Flights[cacheKey]; flight != nil {
		return projectLimitsResponse{}, false, flight.Done
	}
	projectLimitsCache.Flights[cacheKey] = &projectLimitsCacheFlight{Done: make(chan struct{})}
	return projectLimitsResponse{}, false, nil
}

func finishProjectLimitsCache(cacheKey string, response projectLimitsResponse, expiresAt time.Time, store bool) {
	projectLimitsCache.Lock()
	if store {
		for len(projectLimitsCache.Entries) >= projectLimitsCacheMaxKeys {
			oldestKey := ""
			var oldestExpiry time.Time
			for key, entry := range projectLimitsCache.Entries {
				if oldestKey == "" || entry.ExpiresAt.Before(oldestExpiry) {
					oldestKey = key
					oldestExpiry = entry.ExpiresAt
				}
			}
			if oldestKey == "" {
				break
			}
			delete(projectLimitsCache.Entries, oldestKey)
		}
		projectLimitsCache.Entries[cacheKey] = projectLimitsCacheEntry{Response: response, ExpiresAt: expiresAt}
	}
	if flight := projectLimitsCache.Flights[cacheKey]; flight != nil {
		delete(projectLimitsCache.Flights, cacheKey)
		close(flight.Done)
	}
	projectLimitsCache.Unlock()
}

func resetProjectLimitsCacheForTest() {
	projectLimitsCache.Lock()
	projectLimitsCache.Entries = make(map[string]projectLimitsCacheEntry)
	for key, flight := range projectLimitsCache.Flights {
		delete(projectLimitsCache.Flights, key)
		close(flight.Done)
	}
	projectLimitsCache.Unlock()
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
		SchemaVersion:     2,
		Object:            "bravo.project_limits",
		Project:           projectLimitsProjectView{ID: project.ID, Name: project.Name},
		Cached:            false,
		GeneratedAt:       now,
		NextAllowedAt:     nextAllowed,
		NextRefreshAt:     nextAllowed,
		RateLimitSeconds:  int64(projectLimitsRefreshInterval / time.Second),
		RefreshSeconds:    int64(projectLimitsRefreshInterval / time.Second),
		SnapshotFreshness: freshness,
		Providers:         providers,
		AdaptiveAllocator: adaptiveShadowSummary(cfg, adaptiveShadowAuthIndexes(auths), now),
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
	return projectLimitsResponseEnvelope(status, "application/json; charset=utf-8", body, retryAfter, "")
}

func projectLimitsText(status int, value string, retryAfter int64) ([]byte, error) {
	return projectLimitsResponseEnvelope(status, "text/plain; charset=utf-8", []byte(value), retryAfter, "")
}

func renderProjectLimitsResponse(format string, response projectLimitsResponse, cacheStatus string) ([]byte, error) {
	if format == "text" {
		return projectLimitsResponseEnvelope(http.StatusOK, "text/plain; charset=utf-8", []byte(renderProjectLimitsText(response)), 0, cacheStatus)
	}
	body, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return projectLimitsResponseEnvelope(http.StatusOK, "application/json; charset=utf-8", body, 0, cacheStatus)
}

func projectLimitsResponseEnvelope(status int, contentType string, body []byte, retryAfter int64, cacheStatus string) ([]byte, error) {
	headers := http.Header{
		"Content-Type":           []string{contentType},
		"X-Content-Type-Options": []string{"nosniff"},
		"Cache-Control":          []string{"no-store"},
	}
	if cacheStatus != "" {
		headers.Set("X-Bravo-Cache", cacheStatus)
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
	if response.Cached {
		fmt.Fprintf(&builder, "Источник: кэш. Новый локальный снимок будет собран после %s; до этого команда всегда возвращает этот результат с HTTP 200.\n", response.NextRefreshAt.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&builder, "Источник: свежий локальный снимок. Повторные команды до %s вернут его из кэша с HTTP 200.\n", response.NextRefreshAt.Format(time.RFC3339))
	}
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
	fmt.Fprintf(&builder, "\nАдаптивный allocator: %s (%s). Маршруты не блокирует; дополнительных запросов к подпискам: нет. Оценка полностью остывает не позднее чем через %s.\n",
		response.AdaptiveAllocator.Mode,
		response.AdaptiveAllocator.Effect,
		formatProjectLimitDuration(time.Duration(response.AdaptiveAllocator.CoolingMaxAgeSeconds)*time.Second),
	)
	fmt.Fprintf(&builder, "\nUsage за %d дней: %d запросов, %d токенов, ошибок %d (%.1f%%)\n",
		response.Usage.PeriodDays,
		response.Usage.Summary.Requests,
		response.Usage.Summary.TotalTokens,
		response.Usage.Summary.Failures,
		response.Usage.Summary.FailureRatePercent,
	)
	fmt.Fprintf(&builder, "Следующее обновление снимка: %s\n", response.NextRefreshAt.Format(time.RFC3339))
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
