package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var allocatorMutationMu sync.Mutex

type subscriptionWindowView struct {
	Model            string     `json:"model,omitempty"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at"`
	ResetMode        string     `json:"reset_mode,omitempty"`
	Eligible         bool       `json:"eligible"`
	Reason           string     `json:"reason,omitempty"`
}

type subscriptionQuotaView struct {
	Confidence  string                   `json:"confidence"`
	Freshness   string                   `json:"freshness"`
	ObservedAt  time.Time                `json:"observed_at,omitempty"`
	Error       string                   `json:"error,omitempty"`
	Refresh     quotaRefreshState        `json:"refresh"`
	Session     subscriptionWindowView   `json:"session"`
	Weekly      subscriptionWindowView   `json:"weekly"`
	ModelWeekly []subscriptionWindowView `json:"model_weekly,omitempty"`
}

type subscriptionView struct {
	AuthIndex         string                           `json:"auth_index"`
	AnalyticsID       string                           `json:"analytics_id"`
	AuthID            string                           `json:"auth_id,omitempty"`
	Provider          string                           `json:"provider,omitempty"`
	Label             string                           `json:"label,omitempty"`
	DisplayName       string                           `json:"display_name,omitempty"`
	Note              string                           `json:"note,omitempty"`
	Email             string                           `json:"email,omitempty"`
	Workspace         string                           `json:"workspace,omitempty"`
	Plan              string                           `json:"plan,omitempty"`
	Tariff            string                           `json:"tariff"`
	EffectiveTariff   string                           `json:"effective_tariff"`
	Enabled           bool                             `json:"enabled"`
	Health            string                           `json:"health"`
	ModelIssues       []subscriptionModelIssueView     `json:"model_issues,omitempty"`
	PrimaryProjectIDs []string                         `json:"primary_project_ids"`
	Quota             subscriptionQuotaView            `json:"quota"`
	ProfileRefresh    quotaRefreshState                `json:"profile_refresh"`
	Usage             usageSummaryView                 `json:"usage"`
	Allocator         subscriptionAllocatorRuntimeView `json:"allocator"`
}

type subscriptionModelIssueView struct {
	Model                    string     `json:"model"`
	ProviderErrorCode        string     `json:"provider_error_code"`
	ProviderModel            string     `json:"provider_model"`
	ProviderModelDisplayName string     `json:"provider_model_display_name,omitempty"`
	ProviderNoticeTitle      string     `json:"provider_notice_title,omitempty"`
	ProviderNoticeText       string     `json:"provider_notice_text,omitempty"`
	ProviderDisabledReason   string     `json:"provider_disabled_reason,omitempty"`
	ProviderErrorReason      string     `json:"provider_error_reason,omitempty"`
	Scope                    string     `json:"scope"`
	RetryAt                  *time.Time `json:"retry_at,omitempty"`
	ObservedAt               *time.Time `json:"observed_at,omitempty"`
}

type patchSubscriptionRequest struct {
	AuthIndex string `json:"auth_index"`
	Tariff    string `json:"tariff"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

type patchTariffRequest struct {
	ID                  string   `json:"id"`
	Multiplier          *float64 `json:"multiplier,omitempty"`
	SessionFloorPercent *float64 `json:"session_floor_percent,omitempty"`
	WeeklyFloorPercent  *float64 `json:"weekly_floor_percent,omitempty"`
	ReservationPercent  *float64 `json:"reservation_percent,omitempty"`
}

type refreshQuotaRequest struct {
	AuthIndexes []string `json:"auth_indexes,omitempty"`
}

type quotaRequestCountersView struct {
	Attempts uint64 `json:"attempts"`
	Success  uint64 `json:"success"`
	Failure  uint64 `json:"failure"`
}

type quotaPollingView struct {
	UsageIntervalSeconds   int                      `json:"usage_interval_seconds"`
	MinimumIntervalSeconds int                      `json:"minimum_interval_seconds"`
	MaximumIntervalSeconds int                      `json:"maximum_interval_seconds"`
	ProfileIntervalSeconds int                      `json:"profile_interval_seconds"`
	Usage                  quotaRequestCountersView `json:"usage_requests"`
	Profile                quotaRequestCountersView `json:"profile_requests"`
}

func handleAllocatorManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	switch {
	case path == "/v0/management/bravo/subscriptions" && req.Method == http.MethodGet:
		views, errViews := collectSubscriptionViews(req.HostCallbackID)
		if errViews != nil {
			return projectHostFailureJSON(errViews)
		}
		return managementJSON(http.StatusOK, map[string]any{
			"subscriptions": views,
			"tariffs":       append([]tariffConfig(nil), loadedConfig().Tariffs...),
			"quota_polling": quotaPollingSummary(),
		})
	case path == "/v0/management/bravo/subscriptions" && req.Method == http.MethodPatch:
		allocatorMutationMu.Lock()
		defer allocatorMutationMu.Unlock()
		return patchSubscription(req)
	case path == "/v0/management/bravo/tariffs" && req.Method == http.MethodPatch:
		allocatorMutationMu.Lock()
		defer allocatorMutationMu.Unlock()
		return patchTariff(req)
	case path == "/v0/management/bravo/quotas/refresh" && req.Method == http.MethodPost:
		return refreshQuotas(req)
	case path == "/v0/management/bravo/allocator/reconcile" && req.Method == http.MethodPost:
		return reconcileAdaptiveAllocator(req)
	default:
		return nil, nil
	}
}

type reconcileAdaptiveAllocatorRequest struct {
	Confirmed bool `json:"confirmed"`
}

var reconcileAdaptiveAfterLedgerClearHook func()

func reconcileAdaptiveAllocator(req rpcManagementRequest) ([]byte, error) {
	var input reconcileAdaptiveAllocatorRequest
	if failure := decodeAllocatorBody(req.Body, &input, false); failure != nil {
		return projectFailureJSON(*failure)
	}
	if !input.Confirmed {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_adaptive_reconciliation_confirmation_required",
			Message: "Подтвердите сверку лимитов всех подписок перед сбросом аварийного состояния Bravo.",
			Status:  http.StatusBadRequest,
		})
	}
	adaptiveAdmissionMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			adaptiveAdmissionMu.Unlock()
		}
	}()
	if errEstimator := adaptiveEstimatorReadyForReset(); errEstimator != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_adaptive_reconciliation_incomplete",
			Message: "Сброс отклонён: в адаптивной оценке остаётся неподтверждённый расход. Обновите лимиты и повторите сверку.",
			Status:  http.StatusConflict,
		})
	}
	if errDemand := bravoProjectDemand.readyForSaturationReset(); errDemand != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_adaptive_reconciliation_incomplete",
			Message: "Сброс отклонён: распределение проектов всё ещё содержит выполняющиеся запросы. Дождитесь их завершения и повторите сверку.",
			Status:  http.StatusConflict,
		})
	}
	if errClear := clearAdaptiveRoutingSaturationAfterReconciliationLocked(time.Now().UTC()); errClear != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_adaptive_reconciliation_incomplete",
			Message: "Сброс отклонён: в Bravo остаются незавершённые или выполняющиеся запросы. Обновите лимиты и повторите сверку.",
			Status:  http.StatusConflict,
		})
	}
	if reconcileAdaptiveAfterLedgerClearHook != nil {
		reconcileAdaptiveAfterLedgerClearHook()
	}
	resetAdaptiveEstimatorSaturationAfterReconciliation()
	resetAdaptiveStatusRuntime()
	if errDemand := bravoProjectDemand.resetSaturationAfterReconciliation(); errDemand != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_adaptive_reconciliation_incomplete",
			Message: "Журнал сверён, но новые запросы появились до сброса распределения проектов. Дождитесь их завершения и повторите сверку.",
			Status:  http.StatusConflict,
		})
	}
	adaptiveAdmissionMu.Unlock()
	admissionLocked = false
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   "warn",
		"message": "Bravo: оператор завершил сверку и сбросил аварийное состояние адаптивного журнала",
	})
	return managementJSON(http.StatusOK, map[string]any{
		"status":              "reconciled",
		"message_ru":          "Сверка завершена. Вторичные маршруты Bravo снова доступны.",
		"adaptive_saturated":  adaptiveRoutingSaturated.Load(),
		"estimator_saturated": false,
		"demand_saturated":    false,
	})
}

func quotaPollingSummary() quotaPollingView {
	cfg := loadedConfig()
	view := quotaPollingView{
		UsageIntervalSeconds:   cfg.QuotaUsageRefreshSeconds,
		MinimumIntervalSeconds: minimumQuotaUsageRefreshSeconds,
		MaximumIntervalSeconds: maximumQuotaUsageRefreshSeconds,
		ProfileIntervalSeconds: cfg.QuotaProfileRefreshSeconds,
	}
	bravoUsageState.mu.RLock()
	defer bravoUsageState.mu.RUnlock()
	for _, quota := range bravoUsageState.state.Quotas {
		if quota == nil {
			continue
		}
		view.Usage.Attempts += quota.UsageRefresh.AttemptCount
		view.Usage.Success += quota.UsageRefresh.SuccessCount
		view.Usage.Failure += quota.UsageRefresh.FailureCount
		view.Profile.Attempts += quota.ProfileRefresh.AttemptCount
		view.Profile.Success += quota.ProfileRefresh.SuccessCount
		view.Profile.Failure += quota.ProfileRefresh.FailureCount
	}
	return view
}

func collectSubscriptionViews(hostCallbackID string) ([]subscriptionView, error) {
	cfg := loadedConfig()
	auths, errList := listHostAuths(hostCallbackID)
	if errList != nil {
		return nil, errList
	}
	known := make(map[string]pluginapi.HostAuthFileEntry, len(auths))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if authIndex != "" {
			known[authIndex] = auth
		}
	}
	for _, subscription := range cfg.Subscriptions {
		if _, exists := known[subscription.AuthIndex]; !exists {
			known[subscription.AuthIndex] = pluginapi.HostAuthFileEntry{AuthIndex: subscription.AuthIndex}
		}
	}
	primaryOwners := primaryProjectOwners(cfg, auths)
	indexes := make([]string, 0, len(known))
	for authIndex := range known {
		indexes = append(indexes, authIndex)
	}
	sort.Strings(indexes)
	views := make([]subscriptionView, 0, len(indexes))
	for _, authIndex := range indexes {
		auth := known[authIndex]
		quota := normalizedQuotaState(quotaSnapshot(authIndex))
		subscription := subscriptionPolicy(cfg, authIndex)
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
		views = append(views, buildSubscriptionView(cfg, auth, subscription, tariff, quota, primaryOwners[authIndex]))
	}
	return views, nil
}

func buildSubscriptionView(
	cfg pluginConfig,
	auth pluginapi.HostAuthFileEntry,
	subscription subscriptionConfig,
	tariff tariffConfig,
	quota credentialQuotaState,
	primaryProjects []string,
) subscriptionView {
	confidence := quotaConfidence(quota)
	now := time.Now()
	freshness := quotaFreshnessAt(quota, "", cfg, now)
	routingConfidence := quotaRoutingConfidenceAt(quota, "", cfg, now)
	session, weekly := effectiveQuotaWindows(quota, "")
	enabled := subscriptionEnabled(subscription)
	sessionEligible, sessionReason := quotaWindowEligibility(cfg, routingConfidence, enabled, session.RemainingPercent, tariff.SessionFloorPercent)
	weeklyEligible, weeklyReason := quotaWindowEligibility(cfg, routingConfidence, enabled, weekly.RemainingPercent, tariff.WeeklyFloorPercent)
	modelWeekly := make([]subscriptionWindowView, 0, len(quota.ModelWeekly))
	for _, modelWindow := range quota.ModelWeekly {
		window := normalizeQuotaWindow(modelWindow.quotaWindowState)
		modelConfidence := quotaRoutingConfidenceAt(quota, modelWindow.Model, cfg, now)
		eligible, reason := quotaWindowEligibility(cfg, modelConfidence, enabled, window.RemainingPercent, tariff.WeeklyFloorPercent)
		modelWeekly = append(modelWeekly, quotaWindowView(
			confidence,
			modelWindow.Model,
			window,
			eligible,
			reason,
		))
	}
	tariffID := strings.ToLower(strings.TrimSpace(subscription.Tariff))
	if tariffID == "" {
		tariffID = "auto"
	}
	presentation := subscriptionPresentationFor(auth, quota)
	modelIssues := subscriptionModelIssues(auth)
	usageRefresh := quotaRefreshStateForView(quota.UsageRefresh)
	profileRefresh := quotaRefreshStateForView(quota.ProfileRefresh)
	refreshError := ""
	if usageRefresh.Error != nil {
		refreshError = usageRefresh.Error.Message
	}
	return subscriptionView{
		AuthIndex:   strings.TrimSpace(auth.AuthIndex),
		AnalyticsID: analyticsSubscriptionID(auth.AuthIndex),
		AuthID:      strings.TrimSpace(auth.ID),
		Provider:    normalizeProvider(firstNonEmpty(auth.Provider, auth.Type, quota.Provider)),
		// Label stays as a compatibility alias for older Management Center builds.
		// New clients use DisplayName and render Note as its own, operator-authored field.
		Label:           presentation.DisplayName,
		DisplayName:     presentation.DisplayName,
		Note:            presentation.Note,
		Email:           presentation.Email,
		Workspace:       presentation.Workspace,
		Plan:            strings.TrimSpace(quota.Plan),
		Tariff:          tariffID,
		EffectiveTariff: tariff.ID,
		Enabled:         enabled,
		// A credential-wide status is a lossy roll-up of per-model state. Present
		// the account as unavailable only for an operator disable or a live
		// account-wide deadline/cooldown; individual model restrictions are listed
		// separately below.
		Health:            string(classifyBravoSubscriptionHealth(firstNonEmpty(auth.Provider, auth.Type), auth, time.Now())),
		ModelIssues:       modelIssues,
		PrimaryProjectIDs: append([]string(nil), primaryProjects...),
		Quota: subscriptionQuotaView{
			Confidence:  confidence,
			Freshness:   freshness,
			ObservedAt:  quotaConfirmedAt(quota),
			Error:       refreshError,
			Refresh:     usageRefresh,
			Session:     quotaWindowView(confidence, "", session, sessionEligible, sessionReason),
			Weekly:      quotaWindowView(confidence, "", weekly, weeklyEligible, weeklyReason),
			ModelWeekly: modelWeekly,
		},
		ProfileRefresh: profileRefresh,
		Usage:          authUsageSummary(auth.AuthIndex, time.Now()),
		Allocator:      adaptiveSubscriptionRuntimeView(cfg, auth, tariff, quota, now),
	}
}

func quotaRefreshStateForView(state quotaRefreshState) quotaRefreshState {
	if state.Error == nil {
		return state
	}
	errorCopy := *state.Error
	switch strings.ToLower(strings.TrimSpace(errorCopy.Code)) {
	case "rate_limited":
		errorCopy.Message = "Провайдер временно ограничил частоту обновления квоты. Последняя подтверждённая квота сохранена."
	case "timeout":
		errorCopy.Message = "Обновление квоты не успело завершиться. Последняя подтверждённая квота сохранена."
	case "transport_unavailable":
		errorCopy.Message = "Сервис не смог подключиться к странице квоты провайдера. Последняя подтверждённая квота сохранена."
	case "provider_unavailable":
		errorCopy.Message = "Страница квоты провайдера временно недоступна. Последняя подтверждённая квота сохранена."
	case "auth_stale":
		errorCopy.Message = "Провайдер отклонил авторизацию при обновлении квоты. Проверьте подключение аккаунта."
	case "forbidden":
		errorCopy.Message = "Провайдер запретил чтение квоты для этой подписки."
	case "response_invalid", "quota_response_invalid", "windows_missing", "quota_windows_missing":
		errorCopy.Message = "Провайдер вернул неполные или некорректные данные квоты. Последняя подтверждённая квота сохранена."
	default:
		errorCopy.Message = "Не удалось обновить квоту. Последняя подтверждённая квота сохранена."
	}
	state.Error = &errorCopy
	return state
}

func classifyBravoSubscriptionHealth(provider string, auth pluginapi.HostAuthFileEntry, now time.Time) bravoAuthHealth {
	if len(auth.ModelStates) > 0 {
		return classifyBravoAuthCredentialGate(provider, auth, now)
	}
	return classifyBravoAuthHealthDeadline(provider, auth, now)
}

func subscriptionModelIssues(auth pluginapi.HostAuthFileEntry) []subscriptionModelIssueView {
	now := time.Now()
	issuesByModel := make(map[string]subscriptionModelIssueView)

	models := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := auth.ModelStates[model]
		if !subscriptionModelIssueActive(state, now) {
			continue
		}
		detail := providererror.Detail{
			Code:             state.ErrorCode,
			Message:          state.ErrorMessage,
			Model:            state.ProviderModel,
			ModelDisplayName: state.ProviderModelDisplayName,
			NoticeTitle:      state.ProviderNoticeTitle,
			NoticeText:       state.ProviderNoticeText,
			DisabledReason:   state.ProviderDisabledReason,
			Scope:            state.Scope,
			Reason:           state.Reason,
		}
		if strings.TrimSpace(detail.Code) == "" {
			detail, _ = providererror.Parse(state.StatusMessage)
		}
		issue, ok := subscriptionModelIssueFromDetail(
			model,
			detail,
			state.NextRetryAfter,
			state.UpdatedAt,
		)
		if ok {
			issuesByModel[issue.Model] = issue
		}
	}

	provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type))
	for _, cooldown := range activeProviderModelCooldowns(provider, auth.ID, now) {
		issue, ok := subscriptionModelIssueFromDetail(
			cooldown.Model,
			cooldown.ProviderError,
			cooldown.Until,
			cooldown.ObservedAt,
		)
		if ok {
			// Bravo's cooldown is the execution source of truth and may outlive
			// Core's transient ModelStates snapshot. Preserve richer safe host
			// labels while taking the latest active retry barrier.
			if hostIssue, exists := issuesByModel[issue.Model]; exists {
				issue = mergeSubscriptionModelIssues(hostIssue, issue)
			}
			issuesByModel[issue.Model] = issue
		}
	}

	models = models[:0]
	for model := range issuesByModel {
		models = append(models, model)
	}
	sort.Strings(models)
	issues := make([]subscriptionModelIssueView, 0, len(models))
	for _, model := range models {
		issues = append(issues, issuesByModel[model])
	}
	return issues
}

func subscriptionModelIssueFromDetail(
	model string,
	detail providererror.Detail,
	retryAt, observedAt time.Time,
) (subscriptionModelIssueView, bool) {
	detail = providererror.Sanitize(detail)
	if !strings.EqualFold(strings.TrimSpace(detail.Code), "credits_required") {
		return subscriptionModelIssueView{}, false
	}
	model = baseModelKey(strings.TrimSpace(model))
	safeModel := providererror.Sanitize(providererror.Detail{Model: model}).Model
	if safeModel == "" {
		return subscriptionModelIssueView{}, false
	}
	if strings.TrimSpace(detail.Model) == "" {
		detail.Model = safeModel
	}
	if strings.TrimSpace(detail.Scope) == "" {
		detail.Scope = "model"
	}
	if !strings.EqualFold(strings.TrimSpace(detail.Scope), "model") {
		return subscriptionModelIssueView{}, false
	}
	issue := subscriptionModelIssueView{
		Model:                    safeModel,
		ProviderErrorCode:        detail.Code,
		ProviderModel:            detail.Model,
		ProviderModelDisplayName: detail.ModelDisplayName,
		ProviderNoticeTitle:      detail.NoticeTitle,
		ProviderNoticeText:       detail.NoticeText,
		ProviderDisabledReason:   detail.DisabledReason,
		ProviderErrorReason:      detail.Reason,
		Scope:                    detail.Scope,
	}
	if !retryAt.IsZero() {
		retryAtCopy := retryAt
		issue.RetryAt = &retryAtCopy
	}
	if !observedAt.IsZero() {
		observedAtCopy := observedAt
		issue.ObservedAt = &observedAtCopy
	}
	return issue, true
}

func mergeSubscriptionModelIssues(
	hostIssue, runtimeIssue subscriptionModelIssueView,
) subscriptionModelIssueView {
	merged := hostIssue
	if merged.Model == "" {
		merged.Model = runtimeIssue.Model
	}
	if merged.ProviderErrorCode == "" {
		merged.ProviderErrorCode = runtimeIssue.ProviderErrorCode
	}
	if merged.ProviderModel == "" {
		merged.ProviderModel = runtimeIssue.ProviderModel
	}
	if merged.ProviderModelDisplayName == "" {
		merged.ProviderModelDisplayName = runtimeIssue.ProviderModelDisplayName
	}
	if merged.ProviderNoticeTitle == "" {
		merged.ProviderNoticeTitle = runtimeIssue.ProviderNoticeTitle
	}
	if merged.ProviderNoticeText == "" {
		merged.ProviderNoticeText = runtimeIssue.ProviderNoticeText
	}
	if merged.ProviderDisabledReason == "" {
		merged.ProviderDisabledReason = runtimeIssue.ProviderDisabledReason
	}
	if merged.ProviderErrorReason == "" {
		merged.ProviderErrorReason = runtimeIssue.ProviderErrorReason
	}
	if merged.Scope == "" {
		merged.Scope = runtimeIssue.Scope
	}
	merged.RetryAt = laterTimePointer(merged.RetryAt, runtimeIssue.RetryAt)
	merged.ObservedAt = laterTimePointer(merged.ObservedAt, runtimeIssue.ObservedAt)
	return merged
}

func laterTimePointer(left, right *time.Time) *time.Time {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	case right.After(*left):
		return right
	default:
		return left
	}
}

func subscriptionModelIssueActive(state pluginapi.HostAuthModelState, now time.Time) bool {
	if state.Unavailable &&
		!state.NextRetryAfter.IsZero() &&
		state.NextRetryAfter.After(now) {
		return true
	}
	if state.QuotaExceeded &&
		!state.QuotaRecoverAt.IsZero() &&
		state.QuotaRecoverAt.After(now) {
		return true
	}
	return false
}

type subscriptionPresentation struct {
	DisplayName string
	Note        string
	Email       string
	Workspace   string
	Provider    string
}

func subscriptionPresentationFor(auth pluginapi.HostAuthFileEntry, quota credentialQuotaState) subscriptionPresentation {
	note := strings.TrimSpace(auth.Note)
	email := strings.TrimSpace(firstNonEmpty(auth.Email, quota.AccountLabel))
	provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type, quota.Provider))
	workspace := strings.TrimSpace(firstNonEmpty(
		quota.WorkspaceLabel,
		auth.ProjectID,
		auth.Account,
		auth.AccountType,
	))
	displayName := note
	if displayName == "" {
		displayName = joinSubscriptionIdentity(workspace, email)
	}
	if displayName == "" {
		displayName = analyticsSubscriptionLabel(analyticsSubscriptionID(auth.AuthIndex), provider)
	}
	if displayName == "" {
		displayName = "Subscription"
	}
	return subscriptionPresentation{
		DisplayName: displayName,
		Note:        note,
		Email:       email,
		Workspace:   workspace,
		Provider:    provider,
	}
}

func joinSubscriptionIdentity(workspace, email string) string {
	workspace = strings.TrimSpace(workspace)
	email = strings.TrimSpace(email)
	switch {
	case workspace == "":
		return email
	case email == "" || strings.EqualFold(workspace, email):
		return workspace
	default:
		return workspace + " · " + email
	}
}

func quotaWindowView(
	confidence string,
	model string,
	window quotaWindowState,
	eligible bool,
	reason string,
) subscriptionWindowView {
	view := subscriptionWindowView{
		Model:    strings.TrimSpace(model),
		Eligible: eligible,
		Reason:   reason,
	}
	if confidence != "confirmed" {
		return view
	}
	used := window.UsedPercent
	remaining := window.RemainingPercent
	view.UsedPercent = &used
	view.RemainingPercent = &remaining
	view.ResetMode = window.ResetMode
	if !window.ResetAt.IsZero() {
		resetAt := window.ResetAt
		view.ResetAt = &resetAt
	}
	return view
}

func quotaWindowEligibility(cfg pluginConfig, confidence string, enabled bool, remaining, floor float64) (bool, string) {
	if !enabled {
		return false, "subscription_disabled"
	}
	if confidence != "confirmed" {
		if cfg.UnknownSecondaryPolicy == "allow" {
			return true, "quota_unknown_allowed"
		}
		return false, "quota_" + confidence
	}
	if remaining <= floor {
		return false, "reserve_floor"
	}
	return true, "ready"
}

func listHostAuths(hostCallbackID string) ([]pluginapi.HostAuthFileEntry, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{
		"host_callback_id": strings.TrimSpace(hostCallbackID),
	})
	if errCall != nil {
		return nil, errCall
	}
	var response hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host auth list: %w", errUnmarshal)
	}
	return response.Files, nil
}

func patchSubscription(req rpcManagementRequest) ([]byte, error) {
	var input patchSubscriptionRequest
	if failure := decodeAllocatorBody(req.Body, &input, false); failure != nil {
		return projectFailureJSON(*failure)
	}
	input.AuthIndex = strings.TrimSpace(input.AuthIndex)
	input.Tariff = strings.ToLower(strings.TrimSpace(input.Tariff))
	if input.AuthIndex == "" {
		return projectFailureJSON(projectFailure{Code: "bravo_subscription_auth_index_required", Message: "auth_index is required.", Status: http.StatusBadRequest})
	}
	auths, errList := listHostAuths(req.HostCallbackID)
	if errList != nil {
		return projectHostFailureJSON(errList)
	}
	found := false
	for _, auth := range auths {
		if strings.TrimSpace(auth.AuthIndex) == input.AuthIndex {
			found = true
			break
		}
	}
	if !found {
		return projectFailureJSON(projectFailure{Code: "bravo_subscription_not_found", Message: "No credential has this auth_index.", Status: http.StatusConflict})
	}
	cfg := loadedConfig()
	current := subscriptionPolicy(cfg, input.AuthIndex)
	if input.Tariff != "" {
		current.Tariff = input.Tariff
	}
	if input.Enabled != nil {
		current.Enabled = boolPointer(*input.Enabled)
	}
	if current.Tariff == "" {
		current.Tariff = "auto"
	}
	if current.Tariff != "auto" && tariffByID(cfg, current.Tariff).ID != current.Tariff {
		return projectFailureJSON(projectFailure{Code: "bravo_subscription_tariff_unknown", Message: "Unknown tariff: " + current.Tariff, Status: http.StatusBadRequest})
	}
	items, errPersist := persistAllocatorListItem(req.HostCallbackID, "subscriptions", "auth_index", current.AuthIndex, current, subscriptionConfigured(cfg, current.AuthIndex))
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedSubscriptions(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	views, errViews := collectSubscriptionViews(req.HostCallbackID)
	if errViews != nil {
		return projectHostFailureJSON(errViews)
	}
	for _, view := range views {
		if view.AuthIndex == current.AuthIndex {
			return managementJSON(http.StatusOK, map[string]any{"subscription": view})
		}
	}
	return projectFailureJSON(projectFailure{Code: "bravo_subscription_not_found", Message: "Subscription disappeared after persistence.", Status: http.StatusConflict})
}

func patchTariff(req rpcManagementRequest) ([]byte, error) {
	var input patchTariffRequest
	if failure := decodeAllocatorBody(req.Body, &input, false); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	id := strings.ToLower(strings.TrimSpace(input.ID))
	var current tariffConfig
	found := false
	for _, tariff := range cfg.Tariffs {
		if tariff.ID == id {
			current = tariff
			found = true
			break
		}
	}
	if !found {
		return projectFailureJSON(projectFailure{Code: "bravo_tariff_not_found", Message: "Unknown tariff: " + id, Status: http.StatusNotFound})
	}
	if input.SessionFloorPercent != nil {
		current.SessionFloorPercent = *input.SessionFloorPercent
	}
	if input.Multiplier != nil {
		current.Multiplier = *input.Multiplier
	}
	if input.WeeklyFloorPercent != nil {
		current.WeeklyFloorPercent = *input.WeeklyFloorPercent
	}
	if input.ReservationPercent != nil {
		current.ReservationPercent = *input.ReservationPercent
	}
	candidate := cfg
	for index := range candidate.Tariffs {
		if candidate.Tariffs[index].ID == id {
			candidate.Tariffs[index] = current
		}
	}
	if errNormalize := normalizeConfig(&candidate); errNormalize != nil {
		return projectFailureJSON(projectFailure{Code: "bravo_tariff_invalid", Message: errNormalize.Error(), Status: http.StatusBadRequest})
	}
	items, errPersist := persistAllocatorListItem(
		req.HostCallbackID,
		"tariffs",
		"id",
		id,
		current,
		cfg.PersistedTariffIDs[id],
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedTariffs(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return managementJSON(http.StatusOK, map[string]any{"tariff": current})
}

func refreshQuotas(req rpcManagementRequest) ([]byte, error) {
	var input refreshQuotaRequest
	if failure := decodeAllocatorBody(req.Body, &input, true); failure != nil {
		return projectFailureJSON(*failure)
	}
	requested := make(map[string]struct{}, len(input.AuthIndexes))
	for _, authIndex := range normalizeOpaqueStrings(input.AuthIndexes) {
		requested[authIndex] = struct{}{}
	}
	auths, errList := listHostAuths(req.HostCallbackID)
	if errList != nil {
		return projectHostFailureJSON(errList)
	}
	selectedAuths := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	refreshed := make([]string, 0, len(auths))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if len(requested) > 0 {
			if _, selected := requested[authIndex]; !selected {
				continue
			}
		}
		selectedAuths = append(selectedAuths, auth)
		refreshed = append(refreshed, authIndex)
	}
	refreshQuotaSnapshots(req.HostCallbackID, selectedAuths, true)
	views, errViews := collectSubscriptionViews(req.HostCallbackID)
	if errViews != nil {
		return projectHostFailureJSON(errViews)
	}
	return managementJSON(http.StatusOK, map[string]any{
		"refreshed_auth_indexes": refreshed,
		"subscriptions":          views,
		"quota_polling":          quotaPollingSummary(),
	})
}

func decodeAllocatorBody(body []byte, value any, optional bool) *projectFailure {
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return nil
		}
		return &projectFailure{Code: "bravo_allocator_body_required", Message: "A JSON request body is required.", Status: http.StatusBadRequest}
	}
	if len(body) > maxProjectManagementBodyBytes {
		return &projectFailure{Code: "bravo_allocator_body_too_large", Message: "The request body is too large.", Status: http.StatusRequestEntityTooLarge}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(value); errDecode != nil {
		return &projectFailure{Code: "bravo_allocator_body_invalid", Message: "The request body must be a valid JSON object with known fields.", Status: http.StatusBadRequest}
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return &projectFailure{Code: "bravo_allocator_body_invalid", Message: "The request body must contain exactly one JSON object.", Status: http.StatusBadRequest}
	}
	return nil
}

func subscriptionConfigured(cfg pluginConfig, authIndex string) bool {
	for _, subscription := range cfg.Subscriptions {
		if subscription.AuthIndex == authIndex {
			return true
		}
	}
	return false
}

func persistAllocatorListItem(hostCallbackID, field, matchField, matchValue string, value any, replace bool) ([]json.RawMessage, error) {
	rawValue, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	operation := "append"
	if replace {
		operation = "replace"
	}
	raw, errCall := callHost(pluginabi.MethodHostPluginConfigListMutate, hostPluginConfigListMutationRequest{
		HostCallbackID: strings.TrimSpace(hostCallbackID),
		Field:          field,
		Operation:      operation,
		MatchField:     matchField,
		MatchValue:     matchValue,
		Value:          rawValue,
		UniqueFields:   []string{matchField},
	})
	if errCall != nil {
		return nil, errCall
	}
	var response hostPluginConfigListMutationResult
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode persisted %s: %w", field, errUnmarshal)
	}
	return response.Items, nil
}

func installPersistedSubscriptions(items []json.RawMessage) error {
	values := make([]subscriptionConfig, 0, len(items))
	for index, raw := range items {
		var value subscriptionConfig
		if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
			return fmt.Errorf("decode persisted subscription %d: %w", index, errUnmarshal)
		}
		values = append(values, value)
	}
	cfg := loadedConfig()
	cfg.Subscriptions = values
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		return errNormalize
	}
	currentConfig.Store(cfg)
	resetAdaptiveStatusRuntime()
	return nil
}

func installPersistedTariffs(items []json.RawMessage) error {
	values := make([]tariffConfig, 0, len(items))
	for index, raw := range items {
		var value tariffConfig
		if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
			return fmt.Errorf("decode persisted tariff %d: %w", index, errUnmarshal)
		}
		values = append(values, value)
	}
	cfg := loadedConfig()
	cfg.Tariffs = values
	cfg.PersistedTariffIDs = make(map[string]bool, len(values))
	for _, value := range values {
		cfg.PersistedTariffIDs[strings.ToLower(strings.TrimSpace(value.ID))] = true
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		return errNormalize
	}
	currentConfig.Store(cfg)
	resetAdaptiveStatusRuntime()
	return nil
}

func primaryProjectOwners(cfg pluginConfig, auths []pluginapi.HostAuthFileEntry) map[string][]string {
	out := make(map[string][]string)
	for _, project := range cfg.SmartKeys {
		if !smartKeyActive(project) {
			continue
		}
		for authIndex := range resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths) {
			out[authIndex] = append(out[authIndex], project.ID)
		}
	}
	for authIndex := range out {
		sort.Strings(out[authIndex])
	}
	return out
}

func validateAndCanonicalizeProjectPrimaries(
	hostCallbackID string,
	cfg pluginConfig,
	previous *smartKeyConfig,
	project *smartKeyConfig,
) *projectFailure {
	if project == nil {
		return nil
	}
	strictPool := len(project.AllowedAuthIDs) > 0
	// Existing configurations may use arbitrary auth IDs or filenames. They
	// remain routable through the legacy resolver, but only the fixed 16-hex
	// AuthIndex identity is accepted for new strict ownership guarantees.
	requiresIndexValidation := false
	for _, value := range project.PrimaryAuthIDs {
		if looksLikeAuthIndex(value) {
			requiresIndexValidation = true
			break
		}
	}
	if !strictPool && !requiresIndexValidation {
		return nil
	}
	if strings.TrimSpace(hostCallbackID) == "" {
		code := "bravo_primary_auth_validation_unavailable"
		message := "Could not validate primary auth_index."
		if strictPool {
			code = "bravo_allowed_auth_validation_unavailable"
			message = "Could not validate allowed_auth_ids."
		}
		return &projectFailure{Code: code, Message: message, Status: http.StatusBadGateway}
	}
	auths, errList := listHostAuths(hostCallbackID)
	if errList != nil {
		if strictPool {
			return &projectFailure{Code: "bravo_allowed_auth_validation_unavailable", Message: "Could not validate allowed_auth_ids.", Status: http.StatusBadGateway}
		}
		// Opaque legacy labels remain compatible when an older host/test double
		// cannot expose auth indexes. Index-shaped new values still fail closed.
		for _, value := range project.PrimaryAuthIDs {
			if looksLikeAuthIndex(value) {
				return &projectFailure{Code: "bravo_primary_auth_validation_unavailable", Message: "Could not validate primary auth_index.", Status: http.StatusBadGateway}
			}
		}
		return nil
	}
	if strictPool {
		canonicalAllowed := make([]string, 0, len(project.AllowedAuthIDs))
		for _, value := range normalizeOpaqueStrings(project.AllowedAuthIDs) {
			authIndex, resolved := resolvePrimaryAuthIndex(value, auths)
			if !resolved {
				return &projectFailure{
					Code:    "bravo_allowed_auth_not_found",
					Message: "Allowed auth_index " + value + " does not exist. The project pool fails closed.",
					Status:  http.StatusConflict,
				}
			}
			canonicalAllowed = append(canonicalAllowed, authIndex)
		}
		project.AllowedAuthIDs = normalizeOpaqueStrings(canonicalAllowed)
	}
	legacyPrevious := make(map[string]struct{})
	if previous != nil {
		for _, value := range previous.PrimaryAuthIDs {
			legacyPrevious[value] = struct{}{}
		}
	}
	canonical := make([]string, 0, len(project.PrimaryAuthIDs))
	for _, value := range normalizeOpaqueStrings(project.PrimaryAuthIDs) {
		authIndex, resolved := resolvePrimaryAuthIndex(value, auths)
		if !resolved {
			if !strictPool {
				if _, legacy := legacyPrevious[value]; legacy || !looksLikeAuthIndex(value) {
					canonical = append(canonical, value)
					continue
				}
			}
			return &projectFailure{
				Code:    "bravo_primary_auth_not_found",
				Message: "Primary auth_index " + value + " does not exist. Bravo never matches credentials by email.",
				Status:  http.StatusConflict,
			}
		}
		canonical = append(canonical, authIndex)
	}
	project.PrimaryAuthIDs = normalizeOpaqueStrings(canonical)
	if strictPool {
		allowed := make(map[string]struct{}, len(project.AllowedAuthIDs))
		for _, authIndex := range project.AllowedAuthIDs {
			allowed[authIndex] = struct{}{}
		}
		for _, primary := range project.PrimaryAuthIDs {
			if _, exists := allowed[primary]; !exists {
				return &projectFailure{
					Code:    "bravo_primary_auth_outside_allowed_pool",
					Message: "Primary auth_index " + primary + " is outside allowed_auth_ids.",
					Status:  http.StatusConflict,
				}
			}
		}
	}
	if !smartKeyActive(*project) {
		return nil
	}
	projectIndexes := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	for _, other := range cfg.SmartKeys {
		if other.ID == project.ID || !smartKeyActive(other) {
			continue
		}
		otherIndexes := resolvedPrimaryAuthIndexes(other.PrimaryAuthIDs, auths)
		for authIndex := range projectIndexes {
			if _, duplicate := otherIndexes[authIndex]; duplicate {
				return &projectFailure{
					Code:    "bravo_primary_auth_conflict",
					Message: "Primary auth_index " + authIndex + " already belongs to active project " + other.ID + ".",
					Status:  http.StatusConflict,
				}
			}
		}
	}
	return nil
}

func resolvePrimaryAuthIndex(value string, auths []pluginapi.HostAuthFileEntry) (string, bool) {
	value = strings.TrimSpace(value)
	for _, auth := range auths {
		if strings.TrimSpace(auth.AuthIndex) == value {
			return strings.TrimSpace(auth.AuthIndex), true
		}
	}
	for _, auth := range auths {
		if strings.TrimSpace(auth.ID) == value {
			return strings.TrimSpace(auth.AuthIndex), strings.TrimSpace(auth.AuthIndex) != ""
		}
	}
	for _, auth := range auths {
		if strings.TrimSpace(auth.Name) == value {
			return strings.TrimSpace(auth.AuthIndex), strings.TrimSpace(auth.AuthIndex) != ""
		}
	}
	return "", false
}

func looksLikeAuthIndex(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 16 {
		return false
	}
	_, errDecode := hex.DecodeString(value)
	return errDecode == nil
}
