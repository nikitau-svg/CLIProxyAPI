package main

import (
	"fmt"
	"strings"
)

type executionFailureTrace struct {
	Provider string
	Model    string
	Failure  executionFailure
}

func initialExecutionFailureTraces(plan []executionAttempt) []executionFailureTrace {
	if len(plan) == 0 || len(plan[0].PreflightRejections) == 0 {
		return nil
	}
	traces := make([]executionFailureTrace, 0, len(plan[0].PreflightRejections))
	for _, rejection := range plan[0].PreflightRejections {
		traces = append(traces, executionFailureTrace{
			Provider: normalizeProvider(rejection.Provider),
			Model:    strings.TrimSpace(rejection.Model),
			Failure: executionFailure{
				Code:          rejection.Code,
				Message:       rejection.Reason,
				Status:        503,
				Retryable:     true,
				RouteFallback: true,
			},
		})
	}
	return traces
}

func appendExecutionFailureTrace(
	traces []executionFailureTrace,
	attempt executionAttempt,
	failure executionFailure,
) []executionFailureTrace {
	return append(traces, executionFailureTrace{
		Provider: normalizeProvider(attempt.Candidate.Provider),
		Model:    strings.TrimSpace(attempt.Candidate.Model),
		Failure:  failure,
	})
}

func executionFailureCanContinueRoute(failure executionFailure) bool {
	return failure.Retryable || failure.RouteFallback
}

func executionFailureBlocksPhysicalModel(failure executionFailure) bool {
	return failure.Code == "bravo_context_window_exceeded"
}

func executionFailureModelKey(attempt executionAttempt) string {
	return normalizeProvider(attempt.Candidate.Provider) + "\x00" +
		strings.TrimSpace(attempt.Candidate.Model)
}

func finalExecutionFailure(
	traces []executionFailureTrace,
	fallback executionFailure,
) executionFailure {
	if len(traces) == 0 {
		return fallback
	}
	if len(traces) == 1 {
		trace := traces[0]
		if summary := strings.TrimSpace(safeExecutionFailureSummary(trace)); summary != "" {
			fallback.Message = strings.TrimSuffix(summary, ".") + "."
		}
		return normalizeExhaustedRouteFailure(traces, fallback)
	}
	if summary := russianActionableRouteSummary(traces); summary != "" {
		fallback.Message = summary
		return normalizeExhaustedRouteFailure(traces, fallback)
	}
	parts := make([]string, 0, len(traces))
	seen := make(map[string]bool, len(traces))
	for _, trace := range traces {
		summary := safeExecutionFailureSummary(trace)
		key := strings.ToLower(strings.TrimSpace(summary))
		if key != "" && !seen[key] {
			parts = append(parts, summary)
			seen[key] = true
		}
		if len(parts) == 4 {
			break
		}
	}
	if len(parts) < 2 {
		return normalizeExhaustedRouteFailure(traces, fallback)
	}
	fallback.Message = "Bravo исчерпал доступный маршрут: " + strings.Join(parts, "; ") + "."
	return normalizeExhaustedRouteFailure(traces, fallback)
}

func normalizeExhaustedRouteFailure(
	traces []executionFailureTrace,
	failure executionFailure,
) executionFailure {
	// A context incompatibility remains the terminal client action even when an
	// adaptive route rejection happened earlier in the path.
	for index := len(traces) - 1; index >= 0; index-- {
		if traces[index].Failure.Code == "bravo_context_window_exceeded" {
			contextFailure := traces[index].Failure
			// finalExecutionFailure has already built a redacted Russian summary
			// for the complete path; preserve it while restoring the typed code.
			contextFailure.Message = failure.Message
			return contextFailure
		}
	}
	if code := dominantAdaptiveFailureCode(traces); code != "" {
		failure.Code = code
		failure.Message = routeTraceMessageRU(code)
		failure.Status = 503
		failure.Retryable = code != "bravo_adaptive_ledger_saturated"
		failure.RouteFallback = false
		failure.AccountWide = false
		failure.Provider = nil
		return failure
	}
	for _, trace := range traces {
		if !executionFailureCanContinueRoute(trace.Failure) {
			return failure
		}
	}

	// Every provider attempt failed for a reviewed transient reason. Report the
	// exhausted pool as temporarily unavailable so SDKs honor Retry-After
	// instead of immediately replaying the entire route.
	failure.Code = "bravo_route_temporarily_unavailable"
	failure.Status = 503
	failure.Retryable = true
	failure.RouteFallback = false
	failure.AccountWide = false
	failure.Provider = nil
	return failure
}

func dominantAdaptiveFailureCode(traces []executionFailureTrace) string {
	bestCode := ""
	bestPriority := 0
	allAdaptive := len(traces) > 0
	for _, trace := range traces {
		code := strings.TrimSpace(trace.Failure.Code)
		priority := adaptiveFailureCodePriority(code)
		if priority == 0 {
			allAdaptive = false
			continue
		}
		if priority > bestPriority {
			bestCode, bestPriority = code, priority
		}
	}
	if !allAdaptive {
		return ""
	}
	return bestCode
}

func adaptiveFailureCodePriority(code string) int {
	switch strings.TrimSpace(code) {
	case "bravo_adaptive_durability_unavailable":
		return 60
	case "bravo_adaptive_ledger_saturated":
		return 50
	case "bravo_adaptive_estimator_saturated":
		return 40
	case "bravo_adaptive_demand_saturated":
		return 30
	case "bravo_adaptive_quota_stale", "bravo_adaptive_primary_zero":
		return 25
	case "bravo_allocator_reserve_floor":
		return 20
	case "bravo_adaptive_concurrency_recheck":
		return 10
	default:
		return 0
	}
}

func safeExecutionFailureSummary(trace executionFailureTrace) string {
	model := strings.TrimSpace(trace.Model)
	if trace.Failure.Provider != nil {
		detail := *trace.Failure.Provider
		if strings.TrimSpace(detail.ModelDisplayName) != "" {
			model = strings.TrimSpace(detail.ModelDisplayName)
		} else if strings.TrimSpace(detail.Model) != "" {
			model = strings.TrimSpace(detail.Model)
		}
	}
	switch trace.Failure.Code {
	case "bravo_adaptive_durability_unavailable":
		return failureModelSummary(model, "Bravo не смог надёжно записать резерв на диск; запрос провайдеру не отправлен")
	case "bravo_adaptive_ledger_saturated":
		return failureModelSummary(model, "журнал адаптивных резервов Bravo переполнен и требует сверки лимитов")
	case "bravo_adaptive_estimator_saturated":
		return failureModelSummary(model, "оценщик расхода Bravo переполнен и требует сверки лимитов")
	case "bravo_adaptive_demand_saturated":
		return failureModelSummary(model, "учёт спроса проектов Bravo переполнен и требует сверки лимитов")
	case "bravo_adaptive_quota_stale":
		return failureModelSummary(model, "квота подписки устарела или ещё не подтверждена; требуется обновление квот")
	case "bravo_adaptive_primary_zero":
		return failureModelSummary(model, "основная подписка достигла подтверждённого нулевого остатка")
	case "bravo_adaptive_concurrency_recheck":
		return failureModelSummary(model, "параллельный запрос занял доступный резерв подписки")
	case "bravo_allocator_reserve_floor":
		return failureModelSummary(model, "Claude не вызван: подписка достигла внутреннего резервного порога CLIProxyAPI")
	case "bravo_allocator_withheld":
		return failureModelSummary(model, "внутренний распределитель CLIProxyAPI не выпустил подписку")
	case "bravo_compact_bypass_cooldown":
		return failureModelSummary(model, strings.TrimSuffix(strings.TrimSpace(trace.Failure.Message), "."))
	case "bravo_context_window_exceeded":
		if trace.Failure.Provider != nil && trace.Failure.Provider.RequiredTokens > 0 &&
			trace.Failure.Provider.LimitTokens > 0 {
			return failureModelSummary(model, fmt.Sprintf(
				"контекст содержит %s токенов при лимите %s токенов",
				formatContextTokenCount(trace.Failure.Provider.RequiredTokens),
				formatContextTokenCount(trace.Failure.Provider.LimitTokens),
			))
		}
		return failureModelSummary(model, "контекст переписки не помещается в окно модели")
	case "bravo_subscription_model_credits_exhausted":
		return failureModelSummary(model, "исчерпан отдельный лимит расходов этой модели у провайдера")
	case "bravo_subscription_quota_exhausted", "rate_limit_error", "rate_limited":
		return failureModelSummary(model, "подписка достигла лимита провайдера")
	case "bravo_subscription_auth_unavailable", "authentication_error":
		return failureModelSummary(model, "авторизация подписки недоступна")
	case "bravo_subscription_access_denied", "permission_error":
		return failureModelSummary(model, "подписка не имеет доступа к запросу")
	case "bravo_subscription_model_unavailable":
		return failureModelSummary(model, "модель недоступна на этой подписке")
	case "overloaded_error":
		return failureModelSummary(model, "провайдер временно перегружен")
	case "server_error":
		return failureModelSummary(model, "внутренняя ошибка провайдера")
	}
	code := strings.TrimSpace(trace.Failure.Code)
	if code == "" {
		return ""
	}
	return failureModelSummary(model, code)
}

func russianActionableRouteSummary(traces []executionFailureTrace) string {
	var allocator *executionFailureTrace
	var compactCooldown *executionFailureTrace
	var contextFailure *executionFailureTrace
	for index := range traces {
		trace := &traces[index]
		switch trace.Failure.Code {
		case "bravo_allocator_reserve_floor":
			if normalizeProvider(trace.Provider) == "claude" && allocator == nil {
				allocator = trace
			}
		case "bravo_compact_bypass_cooldown":
			if compactCooldown == nil {
				compactCooldown = trace
			}
		case "bravo_context_window_exceeded":
			contextFailure = trace
		}
	}
	if contextFailure == nil {
		return ""
	}
	fallbackModel := friendlyModelName(contextFailure.Model)
	if allocator != nil {
		claudeModel := friendlyModelName(allocator.Model)
		return fmt.Sprintf(
			"Подписки Claude для модели %s не исчерпаны у провайдера, но достигли внутренних резервных порогов CLIProxyAPI, поэтому запрос был перенаправлен в %s. Модель %s не может вместить весь контекст этой переписки. Поднимите внутренние лимиты Claude в Bravo, выполните /compact или начните новую сессию.",
			claudeModel,
			fallbackModel,
			fallbackModel,
		)
	}
	if compactCooldown != nil {
		return fmt.Sprintf(
			"Команда /compact не смогла повторно использовать резерв Claude: действует внутренний cooldown. Запрос был перенаправлен в %s, но модель не может вместить весь контекст переписки. Подождите время из Retry-After и повторите /compact либо начните новую сессию.",
			fallbackModel,
		)
	}
	return ""
}

func failureModelSummary(model, summary string) string {
	model = strings.TrimSpace(model)
	summary = strings.TrimSpace(summary)
	switch {
	case model == "":
		return summary
	case summary == "":
		return model
	default:
		return fmt.Sprintf("%s — %s", model, summary)
	}
}
