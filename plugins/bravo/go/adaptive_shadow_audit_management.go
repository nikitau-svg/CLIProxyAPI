package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const adaptiveShadowAuditManagementPath = "/v0/management/bravo/adaptive-audit"

func handleAdaptiveShadowAuditManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != adaptiveShadowAuditManagementPath {
		return nil, nil
	}
	if req.Method != http.MethodGet {
		return managementJSON(http.StatusMethodNotAllowed, map[string]any{
			"code":    "bravo_adaptive_audit_method_not_allowed",
			"message": "Shadow-аудит поддерживает только GET.",
		})
	}
	hours, errHours := boundedAdaptiveAuditQueryInt(req.Query.Get("hours"), 24, 1, 168)
	if errHours != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"code":    "bravo_adaptive_audit_query_invalid",
			"message": "Параметр hours должен быть целым числом от 1 до 168.",
		})
	}
	recent, errRecent := boundedAdaptiveAuditQueryInt(req.Query.Get("recent"), 20, 0, 100)
	if errRecent != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"code":    "bravo_adaptive_audit_query_invalid",
			"message": "Параметр recent должен быть целым числом от 0 до 100.",
		})
	}
	report := currentAdaptiveShadowAuditReport(
		loadedConfig(),
		time.Duration(hours)*time.Hour,
		recent,
		time.Now().UTC(),
	)
	format := strings.ToLower(strings.TrimSpace(req.Query.Get("format")))
	if format == "" || format == "json" {
		return managementJSON(http.StatusOK, report)
	}
	if format != "text" {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"code":    "bravo_adaptive_audit_query_invalid",
			"message": "Параметр format поддерживает только json или text.",
		})
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           []string{"text/plain; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: []byte(renderAdaptiveShadowAuditText(report, hours)),
	})
}

func boundedAdaptiveAuditQueryInt(raw string, fallback, minimum, maximum int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, errParse := strconv.Atoi(raw)
	if errParse != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("value outside %d..%d", minimum, maximum)
	}
	return value, nil
}

func renderAdaptiveShadowAuditText(report adaptiveShadowAuditReport, hours int) string {
	var out strings.Builder
	title := "теневой аудит адаптивного распределителя"
	if report.RoutingEnforced {
		title = "аудит боевого адаптивного распределителя"
	}
	fmt.Fprintf(&out, "Bravo: %s (%d ч)\n", title, hours)
	fmt.Fprintf(&out, "Вердикт: %s — %s\n", report.Verdict, report.VerdictMessage)
	influence := "нет"
	if report.RoutingEnforced {
		influence = "да"
	}
	fmt.Fprintf(&out, "Режим: %s; влияние на маршрутизацию: %s\n", report.Mode, influence)
	fmt.Fprintf(&out, "Сохранённая история: с %s, %d сек.; детали усечены: %t; высокая скорость усечения: %t\n",
		report.OldestRetainedAt.Format(time.RFC3339), report.RetainedHistorySpanSeconds,
		report.HistoryTruncated, report.HighRateTruncation)
	if len(report.ReadinessBlockers) > 0 {
		fmt.Fprintf(&out, "Что мешает readiness: %s\n", strings.Join(report.ReadinessBlockers, ", "))
	}
	fmt.Fprintf(&out, "Запросы: %d; фактические попытки выполнения: %d; fallback: %d\n",
		report.RequestsObserved, report.ActualExecutionAttempts, report.RequestsWithFallback)
	fmt.Fprintf(&out, "Shadow-решения: пропустить %d; удержать %d; неизвестно %d\n",
		report.WouldAdmitAttempts, report.WouldWithholdAttempts, report.UnknownDecisionAttempts)
	fmt.Fprintf(&out, "Расхождения: успешных при would_withhold %d; quota-ошибок при would_admit %d\n",
		report.SuccessfulWouldWithhold, report.QuotaFailuresWouldAdmit)
	fmt.Fprintf(&out, "Токен-калибровка: попыток %d; пропустить %d; удержать %d; ложных удержаний %d; пропущенных quota-ошибок %d; cold/legacy %d\n",
		report.TokenCalibratedAttempts, report.TokenCalibratedWouldAdmit, report.TokenCalibratedWouldWithhold,
		report.SuccessfulTokenCalibratedWithhold, report.TokenCalibratedQuotaFailuresOnAdmit,
		report.LegacyShapeEstimateAttempts)
	fmt.Fprintf(&out, "Вердикт новой формулы: %s — %s\n",
		report.TokenCalibrationVerdict, report.TokenCalibrationVerdictMessage)
	fmt.Fprintf(&out, "Shadow-турникет: %s — %s\n",
		report.EdgeGateVerdict, report.EdgeGateVerdictMessage)
	fmt.Fprintf(&out, "Состояния турникета: green %d; guarded %d; tripped %d; half-open %d\n",
		report.EdgeGateGreenAttempts, report.EdgeGateGuardedAttempts,
		report.EdgeGateTrippedAttempts, report.EdgeGateHalfOpenAttempts)
	fmt.Fprintf(&out, "Решения турникета: выполнить %d; probe %d; сразу сменить маршрут из-за busy %d; из-за breaker %d\n",
		report.EdgeGateWouldDispatch, report.EdgeGateWouldProbe,
		report.EdgeGateWouldSkipBusy, report.EdgeGateWouldSkipTripped)
	fmt.Fprintf(&out, "Проверка турникета: успешных would_skip %d; quota-ошибок на would_skip %d; quota-ошибок при dispatch/probe %d; trip/reopen %d/%d\n",
		report.EdgeGateSuccessfulWouldSkip, report.EdgeGateQuotaFailuresWouldSkip,
		report.EdgeGateQuotaFailuresWhileDispatch, report.EdgeGateTripsObserved,
		report.EdgeGateReopensObserved)
	fmt.Fprintf(&out, "Дополнительные обращения к подпискам/провайдерам: %d; применённых изменений маршрута: %d\n",
		report.AdditionalProviderRequests, report.RoutingChangesApplied)
	fmt.Fprintf(&out, "Assist: deferred %d; tail reached/dispatched/success %d/%d/%d; neighbor success %d; invariant lost/duplicate/primary/stream-hedge %d/%d/%d/%d\n",
		report.AssistActuallyDeferred, report.AssistTailReached, report.AssistTailDispatched, report.AssistTailSuccess,
		report.AssistNeighborSuccess, report.AssistLostTail, report.AssistDuplicateTail, report.AssistPrimaryDeferred, report.AssistStreamHedge)
	fmt.Fprintf(&out, "Assist requests: всего %d; success/failure %d/%d\n", report.AssistRequests, report.AssistSuccessfulRequests, report.AssistFailedRequests)
	fmt.Fprintf(&out, "Assist saved tail not reached: %d; terminal/cancel before tail: %d\n", report.AssistTailNotReached, report.AssistTerminalBeforeTail)
	fmt.Fprintf(&out, "Телеметрия: очередь %d/%d; потеряно %d; ошибок записи %d; диск %d/%d байт\n",
		report.QueueDepth, report.QueueCapacity, report.DroppedRecords, report.WriteFailures,
		report.DiskBytes, report.DiskLimitBytes)
	if report.Warning != "" {
		fmt.Fprintf(&out, "Предупреждение: %s\n", report.Warning)
	}
	return out.String()
}
