package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func handleRouteTraceManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != "/v0/management/bravo/traces" {
		return nil, nil
	}
	if req.Method != http.MethodGet {
		return managementJSON(http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]any{
				"code":    "bravo_traces_method_not_allowed",
				"message": "Трассы маршрутов доступны только для чтения.",
			},
		})
	}
	query := routeTraceQuery{
		ProjectID: strings.TrimSpace(req.Query.Get("project_id")),
		TraceID:   strings.TrimSpace(req.Query.Get("trace_id")),
	}
	if rawLimit := strings.TrimSpace(req.Query.Get("limit")); rawLimit != "" {
		limit, errParse := strconv.Atoi(rawLimit)
		if errParse != nil || limit < 1 || limit > 500 {
			return managementJSON(http.StatusBadRequest, map[string]any{
				"error": map[string]any{
					"code":    "bravo_traces_query_invalid",
					"message": "Параметр limit должен быть целым числом от 1 до 500.",
				},
			})
		}
		query.Limit = limit
	}
	if rawErrorsOnly := strings.TrimSpace(req.Query.Get("errors_only")); rawErrorsOnly != "" {
		value, errParse := strconv.ParseBool(rawErrorsOnly)
		if errParse != nil {
			return managementJSON(http.StatusBadRequest, map[string]any{
				"error": map[string]any{
					"code":    "bravo_traces_query_invalid",
					"message": "Параметр errors_only должен быть true или false.",
				},
			})
		}
		query.ErrorsOnly = value
	}
	if query.ProjectID != "" && !validProjectID(query.ProjectID) {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "bravo_traces_query_invalid",
				"message": "Идентификатор проекта имеет неверный формат.",
			},
		})
	}
	if query.TraceID != "" && safeRouteTraceIdentifier(query.TraceID) != query.TraceID {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "bravo_traces_query_invalid",
				"message": "Идентификатор трассы имеет неверный формат.",
			},
		})
	}
	traces, warning, errList := listCurrentRouteTraces(query, time.Now().UTC())
	if errList != nil {
		return managementJSON(http.StatusInternalServerError, map[string]any{
			"error": map[string]any{
				"code":    "bravo_traces_unavailable",
				"message": "Не удалось прочитать безопасные трассы маршрутов.",
			},
		})
	}
	traces = routeTracesWithLiveSubscriptionLabels(traces, req.HostCallbackID)
	return managementJSON(http.StatusOK, map[string]any{
		"schema_version": routeTraceSchemaVersion,
		"retention_days": int(defaultRouteTraceTTL / (24 * time.Hour)),
		"traces":         traces,
		"warning":        warning,
		"storage":        currentRouteTraceStorageStatus(),
	})
}

func routeTracesWithLiveSubscriptionLabels(traces []routeTrace, hostCallbackID string) []routeTrace {
	labels := make(map[string]string)
	if auths, errList := listHostAuths(hostCallbackID); errList == nil {
		for _, auth := range auths {
			authIndex := strings.TrimSpace(auth.AuthIndex)
			if authIndex == "" {
				continue
			}
			presentation := subscriptionPresentationFor(auth, quotaSnapshot(authIndex))
			labels[analyticsSubscriptionID(authIndex)] = strings.TrimSpace(presentation.DisplayName)
		}
	}
	for traceIndex := range traces {
		traces[traceIndex].Attempts = append([]routeTraceAttempt(nil), traces[traceIndex].Attempts...)
		for attemptIndex := range traces[traceIndex].Attempts {
			attempt := &traces[traceIndex].Attempts[attemptIndex]
			attempt.SubscriptionLabel = labels[attempt.SubscriptionID]
		}
	}
	return traces
}
