package main

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type subscriptionAllocatorRuntimeView struct {
	AuthIndex                   string  `json:"-"`
	Mode                        string  `json:"mode"`
	Status                      string  `json:"status"`
	Reason                      string  `json:"reason"`
	ReservationPercent          float64 `json:"reservation_percent"`
	EstimateMinPercent          float64 `json:"estimate_min_percent"`
	EstimateMaxPercent          float64 `json:"estimate_max_percent"`
	SnapshotAgeSeconds          int64   `json:"snapshot_age_seconds"`
	SessionBurnPercentPerMinute float64 `json:"session_burn_percent_per_minute"`
	WeeklyBurnPercentPerMinute  float64 `json:"weekly_burn_percent_per_minute"`
	SessionExposureGuardPercent float64 `json:"session_exposure_guard_percent"`
	WeeklyExposureGuardPercent  float64 `json:"weekly_exposure_guard_percent"`
	DemandGuardPercent          float64 `json:"demand_guard_percent"`
	PendingPercent              float64 `json:"pending_percent"`
	InFlightPercent             float64 `json:"in_flight_percent"`
	PendingRequestCount         int     `json:"pending_request_count"`
	InFlightRequestCount        int     `json:"in_flight_request_count"`
	Tempo1MRequestsPerMinute    float64 `json:"tempo_1m_requests_per_minute"`
	Tempo10MRequestsPerMinute   float64 `json:"tempo_10m_requests_per_minute"`
	Tempo60MRequestsPerMinute   float64 `json:"tempo_60m_requests_per_minute"`
	ProfileCount                int     `json:"profile_count"`
	SessionAdmissionCutoff      float64 `json:"session_admission_cutoff_percent"`
	WeeklyAdmissionCutoff       float64 `json:"weekly_admission_cutoff_percent"`
	SessionHeadroomBefore       float64 `json:"session_headroom_before_percent"`
	SessionHeadroomAfter        float64 `json:"session_headroom_after_percent"`
	WeeklyHeadroomBefore        float64 `json:"weekly_headroom_before_percent"`
	WeeklyHeadroomAfter         float64 `json:"weekly_headroom_after_percent"`
	AdaptiveLedgerSaturated     bool    `json:"adaptive_ledger_saturated"`
	EstimatorSaturated          bool    `json:"estimator_saturated"`
	RetainedLedgerAuthCount     int     `json:"retained_ledger_auth_count"`
	OverflowLedgerAuthCount     int     `json:"overflow_ledger_auth_count"`
	ReasonMessageRU             string  `json:"reason_message_ru,omitempty"`
	RecoveryActionRU            string  `json:"recovery_action_ru,omitempty"`
	QuotaFreshness              string  `json:"quota_freshness"`
}

const adaptiveMaximumStatusEntries = 4096

type adaptiveStatusState struct {
	Status string
	Reason string
}

var adaptiveStatusRuntime = struct {
	sync.Mutex
	Modes map[string]adaptiveStatusState
}{Modes: make(map[string]adaptiveStatusState)}

func adaptiveSubscriptionRuntimeView(
	cfg pluginConfig,
	auth pluginapi.HostAuthFileEntry,
	tariff tariffConfig,
	quota credentialQuotaState,
	now time.Time,
) subscriptionAllocatorRuntimeView {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	view := subscriptionAllocatorRuntimeView{AuthIndex: authIndex, Mode: strings.TrimSpace(cfg.AllocatorMode)}
	allocatorRuntime.Lock()
	view.PendingPercent = allocatorRuntime.PendingPercent[authIndex]
	view.InFlightPercent = allocatorRuntime.InFlightPercent[authIndex]
	view.PendingRequestCount = allocatorRuntime.PendingRequests[authIndex]
	view.InFlightRequestCount = allocatorRuntime.InFlightRequests[authIndex]
	allocatorRuntime.Unlock()
	bravoUsageState.mu.RLock()
	view.AdaptiveLedgerSaturated = bravoUsageState.state.AdaptiveQuota.Saturated
	view.RetainedLedgerAuthCount = adaptiveLedgerAuthCountLocked()
	view.OverflowLedgerAuthCount = bravoUsageState.state.AdaptiveQuota.OverflowAuthCount
	bravoUsageState.mu.RUnlock()
	if view.AdaptiveLedgerSaturated {
		view.ReasonMessageRU = adaptiveLedgerSaturatedMessageRU
		view.RecoveryActionRU = "Обновите подтверждённые лимиты всех подписок, завершите сверку незавершённого расхода и выполните сброс аварийного состояния Bravo."
	}

	shapes := make([]adaptiveRequestShape, 0)
	adaptiveReserveRuntime.Lock()
	_, identitySaturated := adaptiveReserveRuntime.Saturated[authIndex]
	identitySaturated = identitySaturated || adaptiveReserveRuntime.SaturationGlobal
	for _, profile := range adaptiveReserveRuntime.Buckets {
		if profile == nil || profile.AuthIndex != authIndex {
			continue
		}
		view.ProfileCount++
		shapes = append(shapes, profile.Shape)
		view.SessionBurnPercentPerMinute = math.Max(view.SessionBurnPercentPerMinute, profile.Session.ObservedBurnPerMin)
		view.WeeklyBurnPercentPerMinute = math.Max(view.WeeklyBurnPercentPerMinute, profile.Weekly.ObservedBurnPerMin)
	}
	if aggregate := adaptiveReserveRuntime.Profiles[authIndex]; aggregate != nil {
		if view.SessionBurnPercentPerMinute <= 0 {
			view.SessionBurnPercentPerMinute = aggregate.ObservedBurnPerMin
		}
		if view.WeeklyBurnPercentPerMinute <= 0 {
			view.WeeklyBurnPercentPerMinute = aggregate.ObservedBurnPerMin
		}
	}
	if overflow := adaptiveReserveRuntime.Overflow[authIndex]; overflow != nil {
		view.SessionBurnPercentPerMinute = math.Max(view.SessionBurnPercentPerMinute, overflow.ObservedBurnPerMin)
		view.WeeklyBurnPercentPerMinute = math.Max(view.WeeklyBurnPercentPerMinute, overflow.ObservedBurnPerMin)
	}
	adaptiveReserveRuntime.Unlock()
	view.EstimatorSaturated = identitySaturated
	if view.EstimatorSaturated && !view.AdaptiveLedgerSaturated {
		view.ReasonMessageRU = "Адаптивная оценка переполнена и не может безопасно восстановить историю этой подписки."
		view.RecoveryActionRU = "Обновите подтверждённые лимиты, дождитесь завершения запросов и выполните подтверждённую сверку Bravo."
	}

	if len(shapes) == 0 {
		shapes = append(shapes, adaptiveRequestShape{
			Provider:    firstNonEmpty(auth.Provider, auth.Type),
			Multiplier:  1,
			ModelFamily: "unknown",
			CostMode:    "unknown",
		})
	}
	view.EstimateMinPercent = math.Inf(1)
	for _, shape := range shapes {
		reservation := adaptiveReservationForShapePeek(auth, tariff, shape, now)
		view.EstimateMinPercent = math.Min(view.EstimateMinPercent, reservation)
		view.EstimateMaxPercent = math.Max(view.EstimateMaxPercent, reservation)
	}
	if math.IsInf(view.EstimateMinPercent, 1) {
		view.EstimateMinPercent = tariff.ReservationPercent
	}
	view.ReservationPercent = view.EstimateMaxPercent
	confirmedAt := quotaConfirmedAt(quota)
	if !confirmedAt.IsZero() && now.After(confirmedAt) {
		view.SnapshotAgeSeconds = int64(now.Sub(confirmedAt).Seconds())
	}
	exposure := adaptiveSnapshotExposure(cfg, quota, now)
	view.SessionExposureGuardPercent = math.Min(view.SessionBurnPercentPerMinute*exposure.Minutes(), 100)
	view.WeeklyExposureGuardPercent = math.Min(view.WeeklyBurnPercentPerMinute*exposure.Minutes(), 100)
	if view.SessionBurnPercentPerMinute <= 0 {
		view.SessionExposureGuardPercent = adaptiveColdStartExposureGuard(cfg, quota, now)
	}
	if view.WeeklyBurnPercentPerMinute <= 0 {
		view.WeeklyExposureGuardPercent = adaptiveColdStartExposureGuard(cfg, quota, now)
	}
	if identitySaturated {
		view.ReservationPercent = math.Max(view.ReservationPercent, adaptiveMaximumReservationPercent)
		view.SessionExposureGuardPercent = 100
		view.WeeklyExposureGuardPercent = 100
	}
	view.DemandGuardPercent = projectDemandGuard(cfg, executionAttempt{ProjectID: "", Auth: auth}, now)
	tempo := bravoProjectDemand.tempo(authIndex, now)
	view.Tempo1MRequestsPerMinute = tempo.OneMinuteRequestsPerMinute
	view.Tempo10MRequestsPerMinute = tempo.TenMinuteRequestsPerMinute
	view.Tempo60MRequestsPerMinute = tempo.SixtyMinuteRequestsPerMinute
	session, weekly := effectiveQuotaWindows(quota, "")
	view.QuotaFreshness = quotaFreshnessAt(quota, "", cfg, now)
	for _, shape := range shapes {
		model := strings.TrimSpace(shape.PhysicalModel)
		if model == "" {
			continue
		}
		modelSession, modelWeekly := effectiveQuotaWindows(quota, model)
		if modelSession.RemainingPercent < session.RemainingPercent {
			session = modelSession
		}
		if modelWeekly.RemainingPercent < weekly.RemainingPercent {
			weekly = modelWeekly
		}
		freshness := quotaFreshnessAt(quota, model, cfg, now)
		if adaptiveFreshnessRank(freshness) > adaptiveFreshnessRank(view.QuotaFreshness) {
			view.QuotaFreshness = freshness
		}
	}
	if !quotaWindowApplicable(session) {
		view.SessionBurnPercentPerMinute = 0
		view.SessionExposureGuardPercent = 0
	}
	if !quotaWindowApplicable(weekly) {
		view.WeeklyBurnPercentPerMinute = 0
		view.WeeklyExposureGuardPercent = 0
	}
	sharedGuard := view.PendingPercent + view.InFlightPercent + view.ReservationPercent + view.DemandGuardPercent
	view.SessionHeadroomBefore = quotaWindowSafeSurplus(session, tariff.SessionFloorPercent, 0, 0, 0, 0)
	view.WeeklyHeadroomBefore = quotaWindowSafeSurplus(weekly, tariff.WeeklyFloorPercent, 0, 0, 0, 0)
	view.SessionHeadroomAfter = quotaWindowSafeSurplus(
		session, tariff.SessionFloorPercent,
		view.PendingPercent+view.InFlightPercent, view.ReservationPercent,
		view.SessionExposureGuardPercent, view.DemandGuardPercent,
	)
	view.WeeklyHeadroomAfter = quotaWindowSafeSurplus(
		weekly, tariff.WeeklyFloorPercent,
		view.PendingPercent+view.InFlightPercent, view.ReservationPercent,
		view.WeeklyExposureGuardPercent, view.DemandGuardPercent,
	)
	if quotaWindowApplicable(session) {
		view.SessionAdmissionCutoff = tariff.SessionFloorPercent + sharedGuard + view.SessionExposureGuardPercent
	}
	if quotaWindowApplicable(weekly) {
		view.WeeklyAdmissionCutoff = tariff.WeeklyFloorPercent + sharedGuard + view.WeeklyExposureGuardPercent
	}
	view.Status, view.Reason = adaptiveAllocatorStatus(cfg, quota, view)
	if view.ReasonMessageRU == "" {
		view.ReasonMessageRU, view.RecoveryActionRU = adaptiveAllocatorReasonRU(view.Status, view.Reason)
	}
	return view
}

func adaptiveAllocatorReasonRU(status, reason string) (string, string) {
	switch reason {
	case "quota_unknown":
		return "Нет свежего подтверждённого снимка лимитов; вторичная подписка защищена от неизвестного расхода.", "Обновите квоту. Если обновление не проходит, переподключите подписку или используйте другой безопасный маршрут."
	case "reserve_floor":
		return "Новый запрос пересёк бы настроенный резервный порог сессионного или недельного лимита.", "Дождитесь сброса лимита, выберите другую подписку либо осознанно измените резерв проекта."
	case "pending":
		return "Неподтверждённый провайдером расход уже удерживает доступный запас над резервным порогом.", "Дождитесь подтверждённого обновления квоты или используйте другой маршрут."
	case "in_flight":
		return "Выполняющиеся запросы уже зарезервировали безопасный остаток подписки.", "Дождитесь завершения текущих запросов или используйте другую подписку."
	case "demand_guard":
		return "Часть безопасного остатка сохранена для прогнозируемой нагрузки проектов-владельцев подписки.", "Используйте менее загруженную подписку или измените пул проекта."
	case "age_burn_guard":
		return "Возраст снимка и наблюдаемый темп расхода требуют дополнительного защитного запаса.", "Обновите квоту или используйте более свежую подписку."
	case "adaptive_guard_active":
		return "Подписка доступна, но адаптивные защитные резервы уже уменьшают безопасный остаток.", "Bravo автоматически предпочтет более безопасный fallback; проверьте обновление квоты, если статус долго не возвращается в зелёный."
	case "allocator_off":
		return "Адаптивное ограничение выключено.", "Включите режим observe или enforce, чтобы видеть и применять защитные решения."
	case "headroom_available":
		return "Подтверждённого безопасного остатка достаточно для текущей оценки запроса.", "Действий не требуется."
	default:
		if status == "red" {
			return "Подписка временно защищена от небезопасного нового расхода.", "Проверьте лимиты, незавершённые запросы и доступные fallback-маршруты."
		}
		if status == "amber" {
			return "Безопасный остаток подписки сокращается.", "Bravo продолжит работу через наиболее безопасный доступный маршрут."
		}
		return "Адаптивный распределитель не обнаружил ограничений.", "Действий не требуется."
	}
}

func adaptiveAllocatorStatus(
	cfg pluginConfig,
	quota credentialQuotaState,
	view subscriptionAllocatorRuntimeView,
) (string, string) {
	headroom := math.Min(view.SessionHeadroomAfter, view.WeeklyHeadroomAfter)
	if view.AdaptiveLedgerSaturated {
		return applyAdaptiveStatusHysteresis(view.AuthIndex, "red", "adaptive_ledger_saturated", headroom)
	}
	if view.EstimatorSaturated {
		return applyAdaptiveStatusHysteresis(view.AuthIndex, "red", "estimator_saturated", headroom)
	}
	if strings.TrimSpace(cfg.AllocatorMode) == "off" {
		return "green", "allocator_off"
	}
	if quotaConfidence(quota) != "confirmed" || view.QuotaFreshness == quotaFreshnessExpired {
		return applyAdaptiveStatusHysteresis(view.AuthIndex, "red", "quota_unknown", headroom)
	}
	if view.SessionHeadroomAfter <= 0 || view.WeeklyHeadroomAfter <= 0 {
		reason := "reserve_floor"
		switch {
		case view.DemandGuardPercent > 0:
			reason = "demand_guard"
		case view.SessionExposureGuardPercent > 0 || view.WeeklyExposureGuardPercent > 0:
			reason = "age_burn_guard"
		case view.PendingPercent > 0:
			reason = "pending"
		case view.InFlightPercent > 0:
			reason = "in_flight"
		}
		return applyAdaptiveStatusHysteresis(view.AuthIndex, "red", reason, headroom)
	}
	status, reason := "green", "headroom_available"
	if headroom <= 5 {
		status, reason = "amber", "adaptive_guard_active"
	}
	return applyAdaptiveStatusHysteresis(view.AuthIndex, status, reason, headroom)
}

func applyAdaptiveStatusHysteresis(authIndex, status, reason string, headroom float64) (string, string) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return status, reason
	}
	adaptiveStatusRuntime.Lock()
	defer adaptiveStatusRuntime.Unlock()
	previous, exists := adaptiveStatusRuntime.Modes[authIndex]
	if exists {
		switch {
		case previous.Status == "red" && status != "red" && headroom < 2:
			status, reason = "red", "hysteresis_red"
		case previous.Status == "amber" && status == "green" && headroom < 7:
			status, reason = "amber", "adaptive_guard_active"
		}
	}
	if exists || len(adaptiveStatusRuntime.Modes) < adaptiveMaximumStatusEntries {
		adaptiveStatusRuntime.Modes[authIndex] = adaptiveStatusState{Status: status, Reason: reason}
	}
	return status, reason
}

func resetAdaptiveStatusRuntime() {
	adaptiveStatusRuntime.Lock()
	adaptiveStatusRuntime.Modes = make(map[string]adaptiveStatusState)
	adaptiveStatusRuntime.Unlock()
}

func adaptiveFreshnessRank(value string) int {
	switch value {
	case quotaFreshnessExpired:
		return 2
	case quotaFreshnessStale:
		return 1
	default:
		return 0
	}
}

func adaptiveSnapshotExposure(cfg pluginConfig, quota credentialQuotaState, now time.Time) time.Duration {
	confirmedAt := quotaConfirmedAt(quota)
	age := time.Duration(0)
	if !confirmedAt.IsZero() && now.After(confirmedAt) {
		age = now.Sub(confirmedAt)
	}
	refresh := time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	if refresh <= 0 {
		refresh = time.Duration(cfg.QuotaRefreshSeconds) * time.Second
	}
	if refresh <= 0 {
		refresh = time.Minute
	}
	exposure := refresh
	if age > exposure {
		exposure = age
	}
	if exposure > adaptiveMaximumExposure {
		exposure = adaptiveMaximumExposure
	}
	return exposure
}
