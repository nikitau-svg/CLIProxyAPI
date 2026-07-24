package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type bravoStatus struct {
	Version       string                 `json:"version"`
	Enabled       bool                   `json:"enabled"`
	Degraded      bool                   `json:"degraded"`
	StatusCode    string                 `json:"status_code,omitempty"`
	Mode          string                 `json:"mode"`
	Prefix        string                 `json:"prefix"`
	SmartKeyCount int                    `json:"smart_key_count"`
	ModelCount    int                    `json:"model_count"`
	Models        []bravoStatusModel     `json:"models"`
	Providers     []bravoProviderSummary `json:"providers"`
	Cooldowns     int                    `json:"cooldowns"`
	RecentSuccess int                    `json:"recent_success"`
	RecentFailure int                    `json:"recent_failure"`
	GeneratedAt   time.Time              `json:"generated_at"`
}

type bravoStatusModel struct {
	ID          string                 `json:"id"`
	DisplayName string                 `json:"display_name"`
	Description string                 `json:"description"`
	Candidates  []bravoStatusCandidate `json:"candidates"`
}

type bravoStatusCandidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Priority int    `json:"priority"`
}

type bravoProviderSummary struct {
	Provider    string `json:"provider"`
	Accounts    int    `json:"accounts"`
	Healthy     int    `json:"healthy"`
	Unavailable int    `json:"unavailable"`
	Cooldown    int    `json:"cooldown"`
	Disabled    int    `json:"disabled"`
	Errors      int    `json:"errors"`
}

type bravoAuthHealth string

const (
	bravoAuthReady       bravoAuthHealth = "ready"
	bravoAuthCooldown    bravoAuthHealth = "cooldown"
	bravoAuthDisabled    bravoAuthHealth = "disabled"
	bravoAuthUnavailable bravoAuthHealth = "unavailable"
	bravoAuthError       bravoAuthHealth = "error"

	bravoStatusAuthUnavailable = "auth_status_unavailable"
)

func registerManagement() ([]byte, error) {
	return okEnvelope(managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/bravo/status", Description: "Bravo runtime and model status."},
			{Method: http.MethodGet, Path: "/bravo/events", Description: "Recent Bravo execution attempts."},
			{Method: http.MethodGet, Path: "/bravo/config", Description: "Redacted effective Bravo configuration."},
			{Method: http.MethodGet, Path: "/bravo/projects", Description: "List redacted Bravo projects and logical model options."},
			{Method: http.MethodPost, Path: "/bravo/projects", Description: "Create a Bravo project and return its key once."},
			{Method: http.MethodPatch, Path: "/bravo/projects", Description: "Update a Bravo project."},
			{Method: http.MethodDelete, Path: "/bravo/projects", Description: "Revoke and delete a Bravo project."},
			{Method: http.MethodPost, Path: "/bravo/projects/rotate", Description: "Rotate a Bravo project key and return it once."},
			{Method: http.MethodGet, Path: "/bravo/analytics", Description: "Query redacted Bravo usage analytics by project, subscription, provider, model, and time bucket."},
			{Method: http.MethodGet, Path: "/bravo/compatibility", Description: "Compare the live host model registry with reviewed Bravo profiles and effective routes."},
			{Method: http.MethodGet, Path: "/bravo/routes", Description: "List effective or default Bravo model routes."},
			{Method: http.MethodPut, Path: "/bravo/routes", Description: "Validate, preview, and persist one Bravo route override."},
			{Method: http.MethodPost, Path: "/bravo/routes/reset", Description: "Reset one Bravo route to its configured default."},
			{Method: http.MethodGet, Path: "/bravo/subscriptions", Description: "List redacted subscription policy, quota, and usage views."},
			{Method: http.MethodPatch, Path: "/bravo/subscriptions", Description: "Update one subscription policy by auth_index."},
			{Method: http.MethodPatch, Path: "/bravo/tariffs", Description: "Update one allocator tariff."},
			{Method: http.MethodPost, Path: "/bravo/quotas/refresh", Description: "Refresh confirmed subscription quotas."},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/dashboard",
			Menu:        "Bravo",
			Description: "Smart routing, account health, mappings and recent fallback activity.",
		}},
	})
}

func handleManagement(raw []byte) ([]byte, error) {
	var rpcReq rpcManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &rpcReq); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if response, errProjects := handleProjectsManagement(rpcReq); response != nil || errProjects != nil {
		return response, errProjects
	}
	if response, errAllocator := handleAllocatorManagement(rpcReq); response != nil || errAllocator != nil {
		return response, errAllocator
	}
	if response, errAnalytics := handleAnalyticsManagement(rpcReq); response != nil || errAnalytics != nil {
		return response, errAnalytics
	}
	if response, errCompatibility := handleCompatibilityManagement(rpcReq); response != nil || errCompatibility != nil {
		return response, errCompatibility
	}
	if response, errRoutes := handleRoutesManagement(rpcReq); response != nil || errRoutes != nil {
		return response, errRoutes
	}
	path := strings.TrimRight(strings.TrimSpace(rpcReq.Path), "/")
	switch path {
	case "/v0/resource/plugins/bravo/dashboard":
		status, errStatus := collectBravoStatus(rpcReq.HostCallbackID)
		if errStatus != nil {
			markBravoStatusDegraded(&status, bravoStatusAuthUnavailable)
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":               []string{"text/html; charset=utf-8"},
				"Content-Security-Policy":    []string{"default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:;"},
				"X-Content-Type-Options":     []string{"nosniff"},
				"Referrer-Policy":            []string{"no-referrer"},
				"Cache-Control":              []string{"no-store"},
				"Cross-Origin-Opener-Policy": []string{"same-origin"},
			},
			Body: renderBravoDashboard(status),
		})
	case "/v0/management/bravo/status":
		status, errStatus := collectBravoStatus(rpcReq.HostCallbackID)
		if errStatus != nil {
			markBravoStatusDegraded(&status, bravoStatusAuthUnavailable)
			return managementJSON(http.StatusBadGateway, status)
		}
		return managementJSON(http.StatusOK, status)
	case "/v0/management/bravo/events":
		runtimeState.RLock()
		events := append([]attemptRecord(nil), runtimeState.Attempts...)
		runtimeState.RUnlock()
		reverseAttempts(events)
		return managementJSON(http.StatusOK, map[string]any{"events": events})
	case "/v0/management/bravo/config":
		return managementJSON(http.StatusOK, redactedBravoConfig(loadedConfig()))
	default:
		return managementJSON(http.StatusNotFound, map[string]any{"error": "Bravo management route not found"})
	}
}

func collectBravoStatus(hostCallbackID string) (bravoStatus, error) {
	cfg := loadedConfig()
	status := bravoStatus{
		Version:       pluginVersion,
		Enabled:       cfg.Enabled,
		Mode:          "smart",
		Prefix:        cfg.Prefix,
		SmartKeyCount: len(cfg.SmartKeys),
		ModelCount:    len(cfg.Models),
		GeneratedAt:   time.Now().UTC(),
	}
	modelNames := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		model := cfg.Models[name]
		view := bravoStatusModel{
			ID:          cfg.Prefix + name,
			DisplayName: model.DisplayName,
			Description: model.Description,
		}
		for _, item := range model.Candidates {
			view.Candidates = append(view.Candidates, bravoStatusCandidate{
				Provider: normalizeProvider(item.Provider),
				Model:    item.Model,
				Effort:   item.Effort,
				Priority: item.Priority,
			})
		}
		status.Models = append(status.Models, view)
	}

	raw, errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{
		"host_callback_id": hostCallbackID,
	})
	if errCall != nil {
		return status, errCall
	}
	var authResp hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &authResp); errUnmarshal != nil {
		return status, errUnmarshal
	}
	status.Providers = summarizeBravoProviders(cfg, authResp.Files, time.Now())
	for _, provider := range status.Providers {
		status.Cooldowns += provider.Cooldown
	}

	runtimeState.RLock()
	for _, event := range runtimeState.Attempts {
		if event.Success {
			status.RecentSuccess++
		} else {
			status.RecentFailure++
		}
	}
	runtimeState.RUnlock()
	return status, nil
}

func markBravoStatusDegraded(status *bravoStatus, code string) {
	if status == nil {
		return
	}
	status.Degraded = true
	status.StatusCode = strings.TrimSpace(code)
}

func summarizeBravoProviders(cfg pluginConfig, auths []pluginapi.HostAuthFileEntry, now time.Time) []bravoProviderSummary {
	usedProviders := configuredBravoProviders(cfg)
	providers := make(map[string]*bravoProviderSummary)
	for _, auth := range auths {
		provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type, "unknown"))
		if _, used := usedProviders[provider]; !used {
			continue
		}
		summary := providers[provider]
		if summary == nil {
			summary = &bravoProviderSummary{Provider: provider}
			providers[provider] = summary
		}
		summary.Accounts++
		switch classifyBravoAuthHealth(provider, auth, now) {
		case bravoAuthReady:
			summary.Healthy++
		case bravoAuthCooldown:
			summary.Cooldown++
			summary.Unavailable++
		case bravoAuthDisabled:
			summary.Disabled++
			summary.Unavailable++
		case bravoAuthError:
			summary.Errors++
			summary.Unavailable++
		default:
			summary.Unavailable++
		}
	}
	providerNames := make([]string, 0, len(providers))
	for provider := range providers {
		providerNames = append(providerNames, provider)
	}
	sort.Strings(providerNames)
	out := make([]bravoProviderSummary, 0, len(providerNames))
	for _, provider := range providerNames {
		out = append(out, *providers[provider])
	}
	return out
}

func configuredBravoProviders(cfg pluginConfig) map[string]struct{} {
	providers := make(map[string]struct{})
	for _, model := range cfg.Models {
		for _, item := range model.Candidates {
			provider := normalizeProvider(item.Provider)
			if provider != "" {
				providers[provider] = struct{}{}
			}
		}
	}
	return providers
}

func classifyBravoAuthHealth(provider string, auth pluginapi.HostAuthFileEntry, now time.Time) bravoAuthHealth {
	status := strings.ToLower(strings.TrimSpace(auth.Status))
	if auth.Disabled || status == "disabled" {
		return bravoAuthDisabled
	}
	if auth.Unavailable || status == "unavailable" {
		return bravoAuthUnavailable
	}
	if status == "error" {
		return bravoAuthError
	}
	if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return bravoAuthCooldown
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return bravoAuthUnavailable
	}
	if cooldownActive(provider, authID, now) {
		return bravoAuthCooldown
	}
	return bravoAuthReady
}

func redactedBravoConfig(cfg pluginConfig) map[string]any {
	models := make(map[string]logicalModel, len(cfg.Models))
	for name, model := range cfg.Models {
		models[name] = model
	}
	keys := make([]map[string]any, 0, len(cfg.SmartKeys))
	for _, key := range cfg.SmartKeys {
		keys = append(keys, map[string]any{
			"id":               key.ID,
			"name":             key.Name,
			"enabled":          smartKeyEnabled(key),
			"status":           key.Status,
			"models":           append([]string(nil), key.Models...),
			"primary_auth_ids": append([]string(nil), key.PrimaryAuthIDs...),
			"policy":           cloneProjectPolicy(key.Policy),
			"created_at":       key.CreatedAt,
			"updated_at":       key.UpdatedAt,
		})
	}
	return map[string]any{
		"enabled":                  cfg.Enabled,
		"prefix":                   cfg.Prefix,
		"require_smart_key":        cfg.RequireSmartKey,
		"max_attempts":             cfg.MaxAttempts,
		"cooldown_seconds":         cfg.CooldownSeconds,
		"allocator_mode":           cfg.AllocatorMode,
		"quota_refresh_seconds":    cfg.QuotaRefreshSeconds,
		"unknown_secondary_policy": cfg.UnknownSecondaryPolicy,
		"tariffs":                  append([]tariffConfig(nil), cfg.Tariffs...),
		"subscriptions":            append([]subscriptionConfig(nil), cfg.Subscriptions...),
		"smart_keys":               keys,
		"models":                   models,
	}
}

func managementJSON(status int, value any) ([]byte, error) {
	body, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           []string{"application/json; charset=utf-8"},
			"X-Content-Type-Options": []string{"nosniff"},
			"Cache-Control":          []string{"no-store"},
		},
		Body: body,
	})
}

func reverseAttempts(events []attemptRecord) {
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
}

func renderBravoDashboard(status bravoStatus) []byte {
	data, _ := json.Marshal(status)
	title := "Bravo"
	state := "active"
	stateLabel := "активен"
	banner := ""
	if !status.Enabled {
		title += " — disabled"
		state = "disabled"
		stateLabel = "выключен"
	} else if status.Degraded {
		title += " — degraded"
		state = "degraded"
		stateLabel = "ограниченный статус"
		banner = `<div class="status-banner" role="status"><strong>Данные о подписках временно недоступны.</strong><span>Маршрутизация продолжает работать, но показатели готовности на этой странице могут быть неполными. Обновите снимок через несколько секунд.</span></div>`
	}
	page := fmt.Sprintf(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
:root{color-scheme:dark;--bg:#0b0d10;--panel:#13171c;--soft:#1a2027;--line:#29313a;--text:#f5f7fa;--muted:#9ba7b4;--ok:#62d49b;--warn:#ffcb6b;--bad:#ff7a90;--brand:#ad8cff}
*{box-sizing:border-box}
body{margin:0;background:radial-gradient(circle at 90%% 0,#201934 0,transparent 35%%),var(--bg);color:var(--text);font:14px/1.45 ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
button,input{font:inherit}button{min-height:44px;border:1px solid var(--line);border-radius:11px;background:var(--panel);color:var(--text);padding:9px 13px;cursor:pointer}button:hover{border-color:#475463}button:disabled{cursor:default;opacity:.48}
button:focus-visible,input:focus-visible,summary:focus-visible{outline:3px solid var(--brand);outline-offset:3px}
main{max-width:1180px;margin:auto;padding:28px 22px 64px}.top{display:flex;align-items:flex-start;justify-content:space-between;gap:24px;margin-bottom:20px}.top-actions{display:flex;align-items:center;gap:10px;flex-wrap:wrap;justify-content:flex-end}
h1{font-size:32px;line-height:1.05;margin:0 0 8px;letter-spacing:-.04em}h2{font-size:19px;margin:0}.lead{color:var(--muted);max-width:680px;margin:0}
.badge{display:inline-flex;align-items:center;gap:7px;border:1px solid var(--line);background:var(--panel);padding:9px 12px;border-radius:999px;font-weight:650;white-space:nowrap}.dot{width:8px;height:8px;border-radius:50%%;flex:0 0 auto}.state-active .dot{background:var(--ok);box-shadow:0 0 14px var(--ok)}.state-degraded .dot{background:var(--warn);box-shadow:0 0 14px var(--warn)}.state-disabled .dot{background:var(--bad);box-shadow:0 0 14px var(--bad)}
.refresh{background:var(--soft)}.updated{width:100%%;color:var(--muted);font-size:12px;text-align:right}.status-banner{display:grid;gap:3px;border:1px solid #66542c;background:#241f14;color:#f3d899;border-radius:12px;padding:13px 15px;margin:0 0 18px}.status-banner span{color:#d9c58f}
.cards{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-bottom:20px}.card{min-width:0;background:rgba(19,23,28,.9);border:1px solid var(--line);border-radius:15px;padding:16px}.label{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.08em}.value{font-size:24px;font-weight:720;margin-top:7px}.subvalue{color:var(--muted);font-size:12px;margin-top:3px}.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}
section{margin-top:20px}.section-heading{display:flex;align-items:baseline;justify-content:space-between;gap:12px;margin-bottom:10px}.section-meta{color:var(--muted);font-size:13px}
.providers{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:10px}.provider{min-width:0;padding:14px;background:var(--panel);border:1px solid var(--line);border-radius:12px}.provider strong{font-size:16px}.provider p{margin:7px 0 0;color:var(--muted)}.provider small{display:block;color:var(--muted);margin-top:4px}
details{border:1px solid var(--line);background:rgba(19,23,28,.84);border-radius:14px;overflow:hidden}summary{cursor:pointer;list-style:none;display:flex;align-items:center;gap:12px;min-height:48px}summary::-webkit-details-marker{display:none}.chevron{margin-left:auto;color:var(--muted);font-size:22px;line-height:1;transition:transform .15s ease;flex:0 0 auto}details[open]>summary .chevron{transform:rotate(90deg)}
.section-disclosure>summary{padding:15px 17px;font-weight:680}.section-disclosure>.section-body{border-top:1px solid var(--line);padding:16px 17px 18px}.summary-meta{margin-left:auto;color:var(--muted);font-size:13px;font-weight:400}.summary-meta+.chevron{margin-left:0}
.toolbar{display:grid;gap:7px;margin-bottom:14px}.search-label{font-weight:650}.search-row{display:flex;gap:9px;align-items:center}input{width:100%%;min-width:0;min-height:44px;border:1px solid var(--line);border-radius:11px;background:var(--panel);color:var(--text);padding:10px 13px}.search-status{min-height:20px;color:var(--muted);font-size:13px}
.model{margin:9px 0}.model>summary{padding:13px 15px}.model-heading{display:grid;gap:3px;min-width:0}.model-name{font-weight:700;overflow-wrap:anywhere}.model-id{display:inline-block;width:max-content;max-width:100%%;font:12px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);overflow-wrap:anywhere}.primary-route{margin-left:auto;display:flex;align-items:center;gap:7px;min-width:0;color:var(--muted);font-size:13px}.primary-route code{max-width:260px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.model-body{border-top:1px solid var(--line);padding:12px 15px 15px}.description-text{margin:0 0 10px;color:var(--muted)}
.route-list{display:grid}.route{display:grid;grid-template-columns:38px 84px minmax(0,1fr) minmax(78px,auto);gap:10px;align-items:center;padding:9px 0;border-bottom:1px solid #20262d}.route:last-child{border-bottom:0}.rank{color:var(--muted)}code{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--soft);padding:3px 6px;border-radius:6px;overflow-wrap:anywhere;min-width:0}.effort{color:var(--muted);text-align:right}
.fallbacks{margin-top:9px;background:var(--soft);border-radius:10px}.fallbacks>summary{padding:10px 12px;min-height:44px}.fallbacks>.route-list{padding:0 12px 5px;border-top:1px solid var(--line)}
.advanced{margin-top:18px;background:transparent}.advanced>summary{padding:12px 14px}.advanced .note{border-top:1px solid var(--line);color:var(--muted);padding:13px 14px}.empty{padding:28px;text-align:center;color:var(--muted)}
@media(prefers-reduced-motion:reduce){.chevron{transition:none}}
@media(forced-colors:active){body{background:Canvas}.card,.provider,details,.status-banner,button,input{border-color:CanvasText}.dot{background:currentColor!important;box-shadow:none!important;border:1px solid currentColor}.chevron{color:CanvasText}}
@media(max-width:780px){main{padding:20px 14px 50px}.top{display:block}.top-actions{justify-content:flex-start;margin-top:16px}.updated{text-align:left}.cards{grid-template-columns:repeat(2,minmax(0,1fr))}.section-disclosure>.section-body{padding:14px 12px 16px}.model>summary{align-items:flex-start;flex-wrap:wrap}.model-heading{flex:1 1 60%%}.primary-route{order:3;flex:1 0 100%%;margin-left:0;padding-left:0}.primary-route code{max-width:100%%}.route{grid-template-columns:30px 70px minmax(0,1fr);gap:8px}.route .effort{grid-column:3;text-align:left}.search-row{align-items:stretch}.search-row button{flex:0 0 auto}}
@media(max-width:360px){.cards{grid-template-columns:1fr}.search-row{display:grid}.summary-meta{display:none}}
</style>
</head>
<body><main>
<div class="top"><div><h1>Bravo</h1><p class="lead">Единая логическая модель поверх подписок Claude и OpenAI. Ретраи, выбор аккаунта и межпровайдерный fallback выполняются внутри CLIProxyAPI.</p></div><div class="top-actions"><div class="badge state-%s" data-state="%s"><span class="dot" aria-hidden="true"></span>%s · v%s</div><button class="refresh" id="refresh" type="button">Обновить</button><div class="updated">Снимок: <time id="generatedAt">—</time></div></div></div>
%s
<div class="cards">
<div class="card"><div class="label">Проекты</div><div class="value" id="projectCount">0</div><div class="subvalue">без раскрытия ключей</div></div>
<div class="card"><div class="label">Здоровые аккаунты</div><div class="value ok" id="healthyCount">0</div></div>
<div class="card"><div class="label">Активные cooldown</div><div class="value warn" id="cooldownCount">0</div></div>
<div class="card"><div class="label">Попытки в буфере</div><div class="value" id="attemptCount">0</div><div class="subvalue" id="attemptBreakdown">—</div></div>
</div>
<section aria-labelledby="subscriptionsHeading"><div class="section-heading"><h2 id="subscriptionsHeading">Подписки</h2><span class="section-meta">только провайдеры Bravo</span></div><div class="providers" id="providers"></div></section>
<section aria-labelledby="modelsHeading"><div class="section-heading"><h2 id="modelsHeading">Модели</h2></div><details class="section-disclosure" id="mappingSection"><summary><span>Маппинг логических моделей</span><span class="summary-meta" id="modelCount">0 моделей</span><span class="chevron" aria-hidden="true">›</span></summary><div class="section-body">
<div class="toolbar"><label class="search-label" for="search">Поиск по моделям</label><div class="search-row"><input id="search" type="search" placeholder="Логическая или физическая модель" autocomplete="off" aria-describedby="searchStatus"><button id="clearSearch" type="button" disabled>Очистить</button></div><div class="search-status" id="searchStatus" role="status" aria-live="polite"></div></div>
<div id="models"></div></div></details></section>
<details class="advanced"><summary><span>Технические детали</span><span class="chevron" aria-hidden="true">›</span></summary><div class="note">Smart-key хранятся только как SHA‑256 и на публичной странице не показываются. Поля политики редактируются в конфигурации плагина; <b>ultra</b> нормализуется в поддерживаемый CLI уровень <b>max</b>.</div></details>
</main>
<script>
const data=%s;
const q=s=>document.querySelector(s);
q("#projectCount").textContent=data.smart_key_count;
q("#modelCount").textContent=data.model_count+" моделей";
q("#cooldownCount").textContent=data.cooldowns;
q("#attemptCount").textContent=data.recent_success+data.recent_failure;
q("#attemptBreakdown").innerHTML=`+"`"+`<span class="ok">${data.recent_success} успешно</span> · <span class="${data.recent_failure?"bad":""}">${data.recent_failure} ошибок</span>`+"`"+`;
q("#healthyCount").textContent=data.providers.reduce((n,p)=>n+p.healthy,0);
function escapeHTML(v){return String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]))}
function providerDetails(p){const details=[];if(p.cooldown)details.push(p.cooldown+" cooldown");if(p.disabled)details.push(p.disabled+" выключено");if(p.errors)details.push(p.errors+" с ошибкой");return details.length?"<small>"+details.join(" · ")+"</small>":""}
q("#providers").innerHTML=data.providers.length?data.providers.map(p=>`+"`"+`<div class="provider"><strong>${escapeHTML(p.provider)}</strong><p><span class="ok">${p.healthy} готовы</span> · ${p.unavailable} не готовы · ${p.accounts} всего</p>${providerDetails(p)}</div>`+"`"+`).join(""):`+"`"+`<div class="empty">Подходящие аккаунты пока не обнаружены</div>`+"`"+`;
function renderRoute(c,index){return `+"`"+`<div class="route" role="listitem"><span class="rank">#${index+1}</span><strong>${escapeHTML(c.provider)}</strong><code>${escapeHTML(c.model)}</code><span class="effort">${escapeHTML(c.effort||"по умолчанию")}</span></div>`+"`"+`}
function renderModel(m){const candidates=Array.isArray(m.candidates)?m.candidates:[];const primary=candidates[0]||{provider:"—",model:"не настроено",effort:""};const fallbacks=candidates.slice(1);return `+"`"+`<details class="model"><summary><span class="model-heading"><span class="model-name">${escapeHTML(m.display_name||m.id)}</span><span class="model-id">${escapeHTML(m.id)}</span></span><span class="primary-route"><span>${escapeHTML(primary.provider)}</span><code>${escapeHTML(primary.model)}</code></span><span class="chevron" aria-hidden="true">›</span></summary><div class="model-body"><p class="description-text">${escapeHTML(m.description||"Описание не задано")}</p><div class="route-list" role="list" aria-label="Основной маршрут">${renderRoute(primary,0)}</div>${fallbacks.length?`+"`"+`<details class="fallbacks"><summary><span>Резервные маршруты · ${fallbacks.length}</span><span class="chevron" aria-hidden="true">›</span></summary><div class="route-list" role="list">${fallbacks.map((c,i)=>renderRoute(c,i+1)).join("")}</div></details>`+"`"+`:""}</div></details>`+"`"+`}
function render(filter=""){const needle=filter.trim().toLowerCase();const rows=data.models.filter(m=>JSON.stringify(m).toLowerCase().includes(needle));q("#models").innerHTML=rows.length?rows.map(renderModel).join(""):`+"`"+`<div class="empty">Ничего не найдено</div>`+"`"+`;q("#searchStatus").textContent=needle?`+"`"+`Найдено: ${rows.length} из ${data.models.length}`+"`"+`:`+"`"+`Доступно моделей: ${data.models.length}`+"`"+`;q("#clearSearch").disabled=!needle}
const generated=new Date(data.generated_at);if(!Number.isNaN(generated.valueOf())){q("#generatedAt").dateTime=generated.toISOString();q("#generatedAt").textContent=new Intl.DateTimeFormat("ru",{dateStyle:"short",timeStyle:"medium"}).format(generated)}
q("#search").addEventListener("input",e=>render(e.target.value));q("#clearSearch").addEventListener("click",()=>{q("#search").value="";render();q("#search").focus()});q("#refresh").addEventListener("click",()=>location.reload());render();
</script></body></html>`,
		html.EscapeString(title),
		html.EscapeString(state),
		html.EscapeString(state),
		html.EscapeString(stateLabel),
		html.EscapeString(status.Version),
		banner,
		data,
	)
	return []byte(page)
}
