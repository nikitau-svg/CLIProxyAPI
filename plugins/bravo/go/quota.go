package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaRefreshConcurrency = 6

var quotaRefreshLocks sync.Map

// fetchQuotaSnapshot is intentionally a narrow seam. The host owns provider
// credentials and provider-specific quota HTTP calls; Bravo only consumes the
// normalized, secret-free result.
var fetchQuotaSnapshot = func(_ string, auth pluginapi.HostAuthFileEntry) (credentialQuotaState, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthQuotaGet, pluginapi.HostAuthQuotaRequest{
		AuthIndex: strings.TrimSpace(auth.AuthIndex),
	})
	if errCall != nil {
		return credentialQuotaState{}, errCall
	}
	var response pluginapi.HostAuthQuotaResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return credentialQuotaState{}, fmt.Errorf("decode host quota response: %w", errUnmarshal)
	}
	state := credentialQuotaState{
		Status:         response.Confidence,
		Confidence:     response.Confidence,
		Provider:       normalizeProvider(response.Provider),
		Plan:           strings.TrimSpace(response.PlanLabel),
		AccountLabel:   strings.TrimSpace(response.AccountLabel),
		WorkspaceLabel: strings.TrimSpace(response.WorkspaceLabel),
		RefreshedAt:    response.ObservedAt.UTC(),
	}
	if response.Error != nil {
		state.Error = strings.TrimSpace(response.Error.Message)
	}
	for _, window := range response.Windows {
		value := quotaWindowState{
			UsedPercent:      window.UsedPercent,
			RemainingPercent: window.RemainingPercent,
			ResetAt:          window.ResetAt.UTC(),
			ResetMode:        strings.ToLower(strings.TrimSpace(window.ResetMode)),
		}
		switch strings.ToLower(strings.TrimSpace(window.Kind)) {
		case pluginapi.HostAuthQuotaWindowKindSession:
			state.Session = value
		case pluginapi.HostAuthQuotaWindowKindWeekly:
			state.Weekly = value
		case pluginapi.HostAuthQuotaWindowKindModelWeekly:
			state.ModelWeekly = append(state.ModelWeekly, modelQuotaWindowState{
				Model:            strings.ToLower(strings.TrimSpace(window.ModelFamily)),
				quotaWindowState: value,
			})
		}
	}
	if quotaConfidence(state) == pluginapi.HostAuthQuotaConfidenceConfirmed {
		if !validConfirmedQuotaWindow(state.Session) || !validConfirmedQuotaWindow(state.Weekly) {
			return credentialQuotaState{}, fmt.Errorf("host confirmed quota without required windows")
		}
	}
	return normalizedQuotaState(state), nil
}

func refreshQuotaIfNeeded(hostCallbackID string, auth pluginapi.HostAuthFileEntry, force bool) credentialQuotaState {
	authIndex := strings.TrimSpace(auth.AuthIndex)
	current := quotaSnapshot(authIndex)
	cfg := loadedConfig()
	staleAfter := time.Duration(cfg.QuotaRefreshSeconds) * time.Second
	if !force && !quotaNeedsRefresh(current, staleAfter, time.Now()) {
		return normalizedQuotaState(current)
	}
	if strings.TrimSpace(hostCallbackID) == "" || authIndex == "" {
		return normalizedQuotaState(current)
	}

	requestedAt := time.Now()
	lockValue, _ := quotaRefreshLocks.LoadOrStore(authIndex, &sync.Mutex{})
	refreshLock := lockValue.(*sync.Mutex)
	refreshLock.Lock()
	defer refreshLock.Unlock()

	current = quotaSnapshot(authIndex)
	if (!force && !quotaNeedsRefresh(current, staleAfter, time.Now())) ||
		(force && !current.RefreshedAt.IsZero() && current.RefreshedAt.After(requestedAt)) {
		return normalizedQuotaState(current)
	}
	pendingAtStart := pendingReservationPercent(authIndex)
	refreshed, errFetch := fetchQuotaSnapshot(hostCallbackID, auth)
	if errFetch != nil {
		refreshed = current
		refreshed.Status = "error"
		refreshed.Confidence = "error"
		refreshed.Provider = normalizeProvider(firstNonEmpty(refreshed.Provider, auth.Provider, auth.Type))
		refreshed.RefreshedAt = time.Now().UTC()
		refreshed.Error = "Confirmed quota refresh failed."
	} else {
		refreshed = normalizedQuotaState(refreshed)
		refreshed.Provider = normalizeProvider(firstNonEmpty(refreshed.Provider, auth.Provider, auth.Type))
		if refreshed.RefreshedAt.IsZero() {
			refreshed.RefreshedAt = time.Now().UTC()
		}
		if quotaConfidence(refreshed) == "confirmed" {
			refreshed.Error = ""
			clearPendingReservation(authIndex, pendingAtStart)
		}
	}
	storeQuotaSnapshot(authIndex, refreshed)
	return refreshed
}

func quotaNeedsRefresh(quota credentialQuotaState, staleAfter time.Duration, now time.Time) bool {
	if quota.RefreshedAt.IsZero() {
		return true
	}
	if staleAfter <= 0 {
		staleAfter = time.Minute
	}
	return now.Sub(quota.RefreshedAt) >= staleAfter
}

func refreshQuotaSnapshots(hostCallbackID string, auths []pluginapi.HostAuthFileEntry, force bool) {
	if strings.TrimSpace(hostCallbackID) == "" || len(auths) == 0 {
		return
	}
	sem := make(chan struct{}, quotaRefreshConcurrency)
	var wg sync.WaitGroup
	for _, auth := range auths {
		auth := auth
		provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type))
		if provider != "claude" && provider != "codex" {
			continue
		}
		if strings.TrimSpace(auth.AuthIndex) == "" || classifyBravoAuthHealth(provider, auth, time.Now()) != bravoAuthReady {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = refreshQuotaIfNeeded(hostCallbackID, auth, force)
		}()
	}
	wg.Wait()
}

func normalizedQuotaState(quota credentialQuotaState) credentialQuotaState {
	quota.Confidence = quotaConfidence(quota)
	quota.Status = quota.Confidence
	quota.Session = normalizeQuotaWindow(quota.Session)
	quota.Weekly = normalizeQuotaWindow(quota.Weekly)
	for index := range quota.ModelWeekly {
		quota.ModelWeekly[index].quotaWindowState = normalizeQuotaWindow(quota.ModelWeekly[index].quotaWindowState)
		quota.ModelWeekly[index].Model = strings.ToLower(strings.TrimSpace(quota.ModelWeekly[index].Model))
	}
	return quota
}

func quotaConfidence(quota credentialQuotaState) string {
	value := strings.ToLower(strings.TrimSpace(firstNonEmpty(quota.Confidence, quota.Status)))
	switch value {
	case "confirmed", "error", "unknown":
		return value
	default:
		return "unknown"
	}
}

func normalizeQuotaWindow(window quotaWindowState) quotaWindowState {
	if window.RemainingPercent == 0 && window.UsedPercent > 0 {
		window.RemainingPercent = 100 - window.UsedPercent
	}
	window.UsedPercent = clampPercent(window.UsedPercent)
	window.RemainingPercent = clampPercent(window.RemainingPercent)
	window.ResetMode = strings.ToLower(strings.TrimSpace(window.ResetMode))
	if !window.ResetAt.IsZero() && window.ResetMode == "" {
		window.ResetMode = pluginapi.HostAuthQuotaResetModeScheduled
	}
	return window
}

func validConfirmedQuotaWindow(window quotaWindowState) bool {
	window = normalizeQuotaWindow(window)
	switch window.ResetMode {
	case pluginapi.HostAuthQuotaResetModeScheduled:
		return !window.ResetAt.IsZero()
	case pluginapi.HostAuthQuotaResetModeInactive, pluginapi.HostAuthQuotaResetModeNotApplicable:
		return window.ResetAt.IsZero() && window.UsedPercent == 0 && window.RemainingPercent == 100
	default:
		return false
	}
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func effectiveQuotaWindows(quota credentialQuotaState, model string) (quotaWindowState, quotaWindowState) {
	session := normalizeQuotaWindow(quota.Session)
	weekly := normalizeQuotaWindow(quota.Weekly)
	model = strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range quota.ModelWeekly {
		if !quotaModelMatches(model, candidate.Model) {
			continue
		}
		window := normalizeQuotaWindow(candidate.quotaWindowState)
		if window.RemainingPercent < weekly.RemainingPercent {
			weekly = window
		}
	}
	return session, weekly
}

func quotaModelMatches(requested, quotaModel string) bool {
	quotaModel = strings.ToLower(strings.TrimSpace(quotaModel))
	if quotaModel == "" {
		return false
	}
	if requested == quotaModel || strings.Contains(requested, quotaModel) {
		return true
	}
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(quotaModel, family) && strings.Contains(requested, family) {
			return true
		}
	}
	return false
}

func tariffByID(cfg pluginConfig, id string) tariffConfig {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, tariff := range cfg.Tariffs {
		if tariff.ID == id {
			return tariff
		}
	}
	for _, tariff := range cfg.Tariffs {
		if tariff.ID == "x1" {
			return tariff
		}
	}
	return tariffConfig{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5}
}

func subscriptionPolicy(cfg pluginConfig, authIndex string) subscriptionConfig {
	authIndex = strings.TrimSpace(authIndex)
	for _, subscription := range cfg.Subscriptions {
		if subscription.AuthIndex == authIndex {
			return subscription
		}
	}
	return subscriptionConfig{AuthIndex: authIndex, Tariff: "auto"}
}

func subscriptionEnabled(subscription subscriptionConfig) bool {
	return subscription.Enabled == nil || *subscription.Enabled
}

func effectiveTariff(cfg pluginConfig, subscription subscriptionConfig, provider string, quota credentialQuotaState) tariffConfig {
	id := strings.ToLower(strings.TrimSpace(subscription.Tariff))
	if id == "" || id == "auto" {
		id = inferredTariffID(firstNonEmpty(quota.Provider, provider), quota.Plan)
	}
	return tariffByID(cfg, id)
}

func inferredTariffID(provider, plan string) string {
	provider = normalizeProvider(provider)
	plan = strings.ToLower(strings.TrimSpace(plan))
	// CLIProxyAPI already derives Codex plan_type from the signed OpenAI token
	// and exposes it as the normalized host quota PlanLabel. "pro" is
	// provider-specific: ChatGPT Pro is the 20x tier, while Claude Pro remains
	// the baseline tier.
	if provider == "codex" && (plan == "pro" || strings.Contains(plan, "chatgpt pro") || strings.Contains(plan, "x20")) {
		return "x20"
	}
	for _, marker := range []string{"team", "business", "enterprise", "max", "x5"} {
		if strings.Contains(plan, marker) {
			return "x5"
		}
	}
	return "x1"
}
