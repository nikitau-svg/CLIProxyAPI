package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func finalExecutionFailureForRequest(
	req rpcExecutorRequest,
	traces []executionFailureTrace,
	fallback executionFailure,
) executionFailure {
	failure := finalExecutionFailure(traces, fallback)
	project, authenticated := authenticatedExecutionProject(req, loadedConfig())
	if !authenticated {
		return failure
	}
	return enrichProjectQuotaContextFailure(project, traces, failure, time.Now().UTC())
}

func enrichProjectQuotaContextFailure(
	project smartKeyConfig,
	traces []executionFailureTrace,
	failure executionFailure,
	now time.Time,
) executionFailure {
	var contextTrace *executionFailureTrace
	quotaRelated := false
	for index := range traces {
		trace := &traces[index]
		if trace.Failure.Code == "bravo_context_window_exceeded" {
			contextTrace = trace
		}
		switch trace.Failure.Code {
		case "bravo_allocator_reserve_floor", "bravo_allocator_withheld", "bravo_subscription_model_credits_exhausted", "bravo_subscription_quota_exhausted", "rate_limit_error", "rate_limited":
			quotaRelated = true
		}
	}
	if contextTrace == nil || !quotaRelated {
		return failure
	}
	providers := projectLimitProvidersFromState(loadedConfig(), project, now)
	claude, hasClaude := projectLimitProviderByName(providers, "claude")
	if !hasClaude {
		return failure
	}
	limitParts := blockedProjectLimitParts(claude.Windows, now)
	if len(limitParts) == 0 {
		return failure
	}
	contextModel := friendlyModelName(contextTrace.Model)
	contextDescription := "не может вместить весь контекст переписки"
	if detail := contextTrace.Failure.Provider; detail != nil && detail.RequiredTokens > 0 && detail.LimitTokens > 0 {
		contextDescription = fmt.Sprintf(
			"не может вместить контекст: требуется %s токенов, окно — %s",
			formatContextTokenCount(detail.RequiredTokens),
			formatContextTokenCount(detail.LimitTokens),
		)
	}
	codexPrefix := "Запасной маршрут Codex был выбран, но"
	if codex, ok := projectLimitProviderByName(providers, "codex"); ok && codex.Status == "available" {
		codexPrefix = "Лимиты Codex доступны, но"
	}
	failure.Code = "bravo_quota_then_context_exhausted"
	failure.Status = http.StatusBadRequest
	failure.Retryable = false
	failure.RouteFallback = false
	failure.AccountWide = false
	failure.Provider = nil
	failure.Message = fmt.Sprintf(
		"Маршрут Claude временно недоступен для проекта: %s. %s модель %s %s. Выполните /compact или начните новую сессию. Точный статус: GET %s?format=text.",
		strings.Join(limitParts, "; "),
		codexPrefix,
		contextModel,
		contextDescription,
		projectLimitsPublicPath,
	)
	return failure
}

func projectLimitProvidersFromState(cfg pluginConfig, project smartKeyConfig, now time.Time) []projectLimitProviderView {
	allowed := make(map[string]struct{}, len(project.AllowedAuthIDs))
	for _, value := range normalizeOpaqueStrings(project.AllowedAuthIDs) {
		allowed[value] = struct{}{}
	}
	bravoUsageState.mu.RLock()
	auths := make([]pluginapi.HostAuthFileEntry, 0, len(bravoUsageState.state.Quotas))
	for authIndex, quota := range bravoUsageState.state.Quotas {
		if quota == nil {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[authIndex]; !ok {
				continue
			}
		}
		auths = append(auths, pluginapi.HostAuthFileEntry{
			AuthIndex: authIndex,
			Provider:  normalizeProvider(quota.Provider),
		})
	}
	bravoUsageState.mu.RUnlock()
	return buildProjectLimitProviders(cfg, project, auths, now)
}

func projectLimitProviderByName(providers []projectLimitProviderView, name string) (projectLimitProviderView, bool) {
	for _, provider := range providers {
		if provider.Provider == name {
			return provider, true
		}
	}
	return projectLimitProviderView{}, false
}

func blockedProjectLimitParts(windows []projectLimitWindowView, now time.Time) []string {
	type rankedPart struct {
		rank int
		text string
	}
	parts := make([]rankedPart, 0, len(windows))
	for _, window := range windows {
		if window.Status == "available" || window.Status == "unknown" || window.Status == "disabled" {
			continue
		}
		label := projectLimitWindowNameRU(window)
		state := projectLimitStatusRU(window.Status)
		text := label + " " + state
		if window.ResetAt != nil && window.ResetAt.After(now) {
			text += " — сброс через " + formatProjectLimitDuration(window.ResetAt.Sub(now))
		}
		parts = append(parts, rankedPart{rank: projectLimitWindowRank(window.Kind), text: text})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].rank > parts[j].rank })
	if len(parts) > 2 {
		parts = parts[:2]
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.text)
	}
	return out
}
