package main

import (
	"bytes"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	adaptiveShadowMaximumOutputTokens       = 1024 * 1024.0
	adaptiveShadowContextCapBodyBytes       = 2 * 1024 * 1024
	adaptiveShadowMaximumReservationPercent = 10.0
	adaptiveShadowMaximumLearnedScale       = 8.0
	adaptiveShadowMaximumAccounts           = 4096
	adaptiveShadowMaximumCommitsPerAccount  = 256
	adaptiveShadowMaximumOverflowModels     = 64
	adaptiveShadowDecisionAdmit             = "would_admit"
	adaptiveShadowDecisionWithhold          = "would_withhold"
	adaptiveShadowDecisionUnknown           = "unknown"
)

var adaptiveShadowNow = func() time.Time { return time.Now().UTC() }

type adaptiveShadowRequestFeatures struct {
	InputTokens          float64
	DeclaredOutputTokens float64
	EstimatedTokens      float64
	ContextFactor        float64
	OutputTrusted        bool
}

type adaptiveShadowEstimate struct {
	ReservationPercent            float64
	SessionReservationPercent     float64
	WeeklyReservationPercent      float64
	ModelWeeklyReservationPercent float64
	ModelWeeklyName               string
	SessionTokenCalibrated        bool
	WeeklyTokenCalibrated         bool
	ModelWeeklyTokenCalibrated    bool
	PredictedTokens               float64
	LearnedScale                  float64
	Confidence                    string
}

type adaptiveShadowCommit struct {
	At                 time.Time
	Percent            float64
	WindowKind         string
	ProjectID          string
	Provider           string
	Model              string
	LogicalModel       string
	Effort             string
	TariffID           string
	Multiplier         float64
	TokenUnits         float64
	SessionPercent     float64
	WeeklyPercent      float64
	ModelWeeklyPercent float64
	ModelWeeklyName    string
	SessionCalibrated  bool
	WeeklyCalibrated   bool
	ModelCalibrated    bool
	EstimateConfidence string
}

type adaptiveShadowAccount struct {
	Commits                           []adaptiveShadowCommit
	OverflowAt                        time.Time
	OverflowPercent                   float64
	OverflowSessionPercent            float64
	OverflowWeeklyPercent             float64
	OverflowModelWeeklyPercent        map[string]float64
	OverflowUnknownModelWeeklyPercent float64
	LearnedScale                      float64
	LearnedAt                         time.Time
	UpdatedAt                         time.Time
}

var adaptiveShadowRuntime = struct {
	sync.Mutex
	Accounts        map[string]*adaptiveShadowAccount
	Saturated       bool
	DroppedAccounts uint64
}{Accounts: make(map[string]*adaptiveShadowAccount)}

// adaptiveShadowPublicView deliberately contains no credential or project
// identity. It is safe to expose through a project-scoped endpoint after the
// caller's allowed account pool has been applied.
type adaptiveShadowPublicView struct {
	Mode                       string                             `json:"mode"`
	Effect                     string                             `json:"effect"`
	RoutingEnforced            bool                               `json:"routing_enforced"`
	AdditionalProviderRequests bool                               `json:"additional_provider_requests"`
	QuotaSnapshotSource        string                             `json:"quota_snapshot_source"`
	CoolingHalfLifeSeconds     int                                `json:"cooling_half_life_seconds"`
	CoolingMaxAgeSeconds       int                                `json:"cooling_max_age_seconds"`
	TrackedAccounts            int                                `json:"tracked_accounts"`
	TrackedCommitments         int                                `json:"tracked_commitments"`
	RawPendingPercent          float64                            `json:"raw_pending_percent"`
	EffectivePendingPercent    float64                            `json:"effective_pending_percent"`
	MaximumLearnedScale        float64                            `json:"maximum_learned_scale"`
	Saturated                  bool                               `json:"saturated"`
	DroppedAccounts            uint64                             `json:"dropped_accounts"`
	TokenCalibration           adaptiveTokenCalibrationPublicView `json:"token_calibration"`
	ForecastBacktest           adaptiveForecastBacktestPublicView `json:"forecast_backtest"`
	Note                       string                             `json:"note"`
}

func adaptiveShadowEffect(cfg pluginConfig) string {
	if cfg.AdaptiveAllocatorMode == "off" {
		return "disabled"
	}
	return "shadow_only"
}

func buildAdaptiveShadowRequestFeatures(body []byte) adaptiveShadowRequestFeatures {
	// At two MiB the request bytes alone already reach the estimator's maximum
	// context factor. Scanning the remaining multi-megabyte prompt cannot raise
	// the reservation, so stop here instead of spending linear CPU on it.
	if len(body) >= adaptiveShadowContextCapBodyBytes {
		input := math.Max(float64(len(body))/4, 1024)
		return adaptiveShadowRequestFeatures{
			InputTokens: input, DeclaredOutputTokens: adaptiveShadowMaximumOutputTokens,
			EstimatedTokens: input + adaptiveShadowMaximumOutputTokens, ContextFactor: 8, OutputTrusted: false,
		}
	}
	output, trusted := adaptiveShadowDeclaredOutputTokens(body)
	if !trusted {
		output = adaptiveShadowMaximumOutputTokens
	}
	tokens := math.Max(float64(len(body))/4+output, 1024)
	return adaptiveShadowRequestFeatures{
		InputTokens:          math.Max(float64(len(body))/4, 1),
		DeclaredOutputTokens: output,
		EstimatedTokens:      tokens,
		ContextFactor:        math.Min(math.Max(math.Sqrt(math.Max(tokens, 8192)/8192), 1), 8),
		OutputTrusted:        trusted,
	}
}

func adaptiveShadowEstimateFor(
	cfg pluginConfig,
	auth pluginapi.HostAuthFileEntry,
	item candidate,
	tariff tariffConfig,
	quota credentialQuotaState,
	features adaptiveShadowRequestFeatures,
	now time.Time,
) adaptiveShadowEstimate {
	baseline := tariff.ReservationPercent
	if baseline <= 0 {
		baseline = 0.1
	}
	learned := adaptiveShadowLearnedScale(strings.TrimSpace(auth.AuthIndex), cfg, now)
	shape := adaptiveShadowModelFactor(item.Model) * adaptiveShadowEffortFactor(item.Effort) * features.ContextFactor
	shape = math.Max(shape, 1)
	value := baseline*shape*learned + baseline*(shape-1)/math.Max(tariff.Multiplier, 1)
	value = math.Min(math.Max(value, baseline), math.Max(adaptiveShadowMaximumReservationPercent, baseline))
	confidence := "shape_estimate"
	if !features.OutputTrusted {
		confidence = "conservative_unknown_output"
	}
	if learned > 1.000001 {
		confidence += "+cooled_provider_calibration"
	}
	estimate := adaptiveShadowEstimate{
		ReservationPercent: value, SessionReservationPercent: value, WeeklyReservationPercent: value,
		PredictedTokens: features.EstimatedTokens,
		LearnedScale:    learned, Confidence: confidence,
	}
	for _, window := range quota.ModelWeekly {
		if quotaModelMatches(item.Model, window.Model) {
			estimate.ModelWeeklyReservationPercent = value
			estimate.ModelWeeklyName = strings.ToLower(strings.TrimSpace(window.Model))
			break
		}
	}
	token := adaptiveTokenCalibrationFor(
		strings.TrimSpace(auth.AuthIndex),
		normalizeProvider(firstNonEmpty(item.Provider, auth.Provider, auth.Type)),
		item.Model, item.Effort, tariff.ID, quota, features, now,
	)
	if token.Session.Available {
		estimate.SessionReservationPercent = token.Session.Percent
		estimate.SessionTokenCalibrated = true
	}
	if token.Weekly.Available {
		estimate.WeeklyReservationPercent = token.Weekly.Percent
		estimate.WeeklyTokenCalibrated = true
	}
	if token.ModelWeekly.Available {
		estimate.ModelWeeklyReservationPercent = token.ModelWeekly.Percent
		estimate.ModelWeeklyName = token.ModelWeeklyName
		estimate.ModelWeeklyTokenCalibrated = true
	}
	if token.Session.Available || token.Weekly.Available || token.ModelWeekly.Available {
		estimate.ReservationPercent = math.Max(
			estimate.SessionReservationPercent,
			math.Max(estimate.WeeklyReservationPercent, estimate.ModelWeeklyReservationPercent),
		)
		estimate.PredictedTokens = token.PredictedTokens
		estimate.Confidence = token.Confidence
	}
	return estimate
}

func annotateAdaptiveShadowPlan(
	cfg pluginConfig,
	project smartKeyConfig,
	auths []pluginapi.HostAuthFileEntry,
	attempts []executionAttempt,
	features adaptiveShadowRequestFeatures,
	now time.Time,
) []executionAttempt {
	if cfg.AdaptiveAllocatorMode != "observe" || strings.TrimSpace(project.ID) == "" {
		return attempts
	}
	primary := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	for index := range attempts {
		authIndex := strings.TrimSpace(attempts[index].Auth.AuthIndex)
		if authIndex == "" {
			continue
		}
		quota := normalizedQuotaState(quotaSnapshot(authIndex))
		subscription := subscriptionPolicy(cfg, authIndex)
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(attempts[index].Auth.Provider, attempts[index].Auth.Type), quota)
		item := attempts[index].Candidate
		if attempts[index].EffectiveEffort != "" {
			item.Effort = attempts[index].EffectiveEffort
		}
		estimate := adaptiveShadowEstimateFor(cfg, attempts[index].Auth, item, tariff, quota, features, now)
		attempts[index].AdaptiveShadow = true
		attempts[index].AdaptiveReservationPercent = estimate.ReservationPercent
		attempts[index].AdaptiveSessionReservationPercent = estimate.SessionReservationPercent
		attempts[index].AdaptiveWeeklyReservationPercent = estimate.WeeklyReservationPercent
		attempts[index].AdaptiveModelWeeklyReservationPercent = estimate.ModelWeeklyReservationPercent
		attempts[index].AdaptiveModelWeeklyName = estimate.ModelWeeklyName
		attempts[index].AdaptiveSessionTokenCalibrated = estimate.SessionTokenCalibrated
		attempts[index].AdaptiveWeeklyTokenCalibrated = estimate.WeeklyTokenCalibrated
		attempts[index].AdaptiveModelWeeklyTokenCalibrated = estimate.ModelWeeklyTokenCalibrated
		attempts[index].AdaptivePredictedTokens = estimate.PredictedTokens
		attempts[index].AdaptiveEstimateConfidence = estimate.Confidence
		// These fields are metadata only for unmanaged legacy attempts. Setting
		// them makes the shadow decision auditable without changing acquisition.
		if attempts[index].ProjectID == "" {
			attempts[index].ProjectID = project.ID
		}
		if _, ok := primary[authIndex]; ok {
			attempts[index].Primary = true
		}
		decision, pending, before, after := adaptiveShadowDecisionFor(
			cfg,
			attempts[index],
			quota,
			tariff,
			estimate.ReservationPercent,
			now,
		)
		attempts[index].AdaptiveShadowDecision = decision
		attempts[index].AdaptiveShadowPendingPercent = pending
		attempts[index].AdaptiveShadowHeadroomBefore = before
		attempts[index].AdaptiveShadowHeadroomAfter = after
	}
	return attempts
}

func adaptiveShadowDecisionFor(
	cfg pluginConfig,
	attempt executionAttempt,
	quota credentialQuotaState,
	tariff tariffConfig,
	reservation float64,
	now time.Time,
) (string, float64, float64, float64) {
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	sessionPending := adaptiveShadowEffectivePendingForWindow(authIndex, pluginapi.HostAuthQuotaWindowKindSession, "", cfg, now)
	weeklyKind, quotaModel := adaptiveShadowWeeklyWindow(quota, attempt.Candidate.Model)
	weeklyPending := adaptiveShadowEffectivePendingForWindow(authIndex, weeklyKind, quotaModel, cfg, now)
	if quotaRoutingConfidenceAt(quota, attempt.Candidate.Model, cfg, now) != "confirmed" {
		return adaptiveShadowDecisionUnknown, adaptiveShadowRound(math.Max(sessionPending, weeklyPending)), 0, 0
	}
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	sessionFloor, weeklyFloor := tariff.SessionFloorPercent, tariff.WeeklyFloorPercent
	if attempt.Primary {
		sessionFloor, weeklyFloor = 0, 0
	}
	sessionReservation := attempt.AdaptiveSessionReservationPercent
	if sessionReservation <= 0 {
		sessionReservation = reservation
	}
	weeklyReservation := attempt.AdaptiveWeeklyReservationPercent
	if weeklyKind == pluginapi.HostAuthQuotaWindowKindModelWeekly && attempt.AdaptiveModelWeeklyReservationPercent > 0 {
		weeklyReservation = attempt.AdaptiveModelWeeklyReservationPercent
	}
	if weeklyReservation <= 0 {
		weeklyReservation = reservation
	}
	sessionBefore := session.RemainingPercent - sessionFloor - sessionPending
	weeklyBefore := weekly.RemainingPercent - weeklyFloor - weeklyPending
	before := math.Min(sessionBefore, weeklyBefore)
	after := math.Min(sessionBefore-sessionReservation, weeklyBefore-weeklyReservation)
	decision := adaptiveShadowDecisionAdmit
	if after <= 0 {
		decision = adaptiveShadowDecisionWithhold
	}
	return decision, adaptiveShadowRound(math.Max(sessionPending, weeklyPending)), adaptiveShadowRound(before), adaptiveShadowRound(after)
}

func adaptiveShadowWeeklyWindow(quota credentialQuotaState, model string) (string, string) {
	weekly := normalizeQuotaWindow(quota.Weekly)
	kind, quotaModel := pluginapi.HostAuthQuotaWindowKindWeekly, ""
	for _, candidate := range quota.ModelWeekly {
		if !quotaModelMatches(model, candidate.Model) {
			continue
		}
		window := normalizeQuotaWindow(candidate.quotaWindowState)
		if window.RemainingPercent < weekly.RemainingPercent {
			weekly = window
			kind = pluginapi.HostAuthQuotaWindowKindModelWeekly
			quotaModel = strings.ToLower(strings.TrimSpace(candidate.Model))
		}
	}
	return kind, quotaModel
}

func adaptiveShadowEffectivePendingFor(authIndex string, cfg pluginConfig, now time.Time) float64 {
	return adaptiveShadowEffectivePendingForWindow(authIndex, "", "", cfg, now)
}

func adaptiveShadowEffectivePendingForWindow(authIndex, kind, quotaModel string, cfg pluginConfig, now time.Time) float64 {
	if authIndex == "" {
		return 0
	}
	adaptiveShadowRuntime.Lock()
	defer adaptiveShadowRuntime.Unlock()
	account := adaptiveShadowRuntime.Accounts[authIndex]
	if account == nil {
		return 0
	}
	pruneAdaptiveShadowAccount(account, cfg, now)
	overflow := account.OverflowPercent
	switch kind {
	case pluginapi.HostAuthQuotaWindowKindSession:
		overflow = account.OverflowSessionPercent
	case pluginapi.HostAuthQuotaWindowKindWeekly:
		overflow = account.OverflowWeeklyPercent
	case pluginapi.HostAuthQuotaWindowKindModelWeekly:
		overflow = account.OverflowUnknownModelWeeklyPercent
		for model, percent := range account.OverflowModelWeeklyPercent {
			if strings.EqualFold(model, quotaModel) || quotaModelMatches(model, quotaModel) {
				overflow += percent
			}
		}
	}
	effective := adaptiveShadowCommitWeight(overflow, account.OverflowAt, cfg, now)
	for _, item := range account.Commits {
		percent := item.Percent
		switch kind {
		case pluginapi.HostAuthQuotaWindowKindSession:
			if item.SessionPercent > 0 {
				percent = item.SessionPercent
			}
		case pluginapi.HostAuthQuotaWindowKindWeekly:
			if item.WeeklyPercent > 0 {
				percent = item.WeeklyPercent
			}
		case pluginapi.HostAuthQuotaWindowKindModelWeekly:
			if item.ModelWeeklyPercent > 0 &&
				(strings.EqualFold(strings.TrimSpace(item.ModelWeeklyName), strings.TrimSpace(quotaModel)) ||
					(item.ModelWeeklyName == "" && quotaModelMatches(item.Model, quotaModel))) {
				percent = item.ModelWeeklyPercent
			} else {
				continue
			}
		}
		effective += adaptiveShadowCommitWeight(percent, item.At, cfg, now)
	}
	return effective
}

func wrapAdaptiveShadowLease(attempt executionAttempt, release func(bool)) func(bool) {
	if !attempt.AdaptiveShadow || attempt.AdaptiveReservationPercent <= 0 {
		return release
	}
	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			if commit {
				recordAdaptiveShadowAttemptCommit(attempt, adaptiveShadowNow())
			}
			release(commit)
		})
	}
}

func recordAdaptiveShadowAttemptCommit(attempt executionAttempt, at time.Time) {
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	percent := attempt.AdaptiveReservationPercent
	if authIndex == "" || percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return
	}
	cfg := loadedConfig()
	quota := normalizedQuotaState(quotaSnapshot(authIndex))
	subscription := subscriptionPolicy(cfg, authIndex)
	tariff := effectiveTariff(cfg, subscription, firstNonEmpty(attempt.Auth.Provider, attempt.Auth.Type), quota)
	item := adaptiveShadowCommit{
		At:                 at.UTC(),
		Percent:            percent,
		ProjectID:          strings.TrimSpace(attempt.ProjectID),
		Provider:           normalizeProvider(firstNonEmpty(attempt.Candidate.Provider, attempt.Auth.Provider, attempt.Auth.Type)),
		Model:              strings.TrimSpace(attempt.Candidate.Model),
		LogicalModel:       strings.TrimSpace(attempt.LogicalModel),
		Effort:             normalizeEffort(firstNonEmpty(attempt.EffectiveEffort, attempt.Candidate.Effort, attempt.RequestedEffort)),
		TariffID:           tariff.ID,
		Multiplier:         math.Max(tariff.Multiplier, 1),
		TokenUnits:         math.Max(attempt.AdaptivePredictedTokens, 0),
		SessionPercent:     attempt.AdaptiveSessionReservationPercent,
		WeeklyPercent:      attempt.AdaptiveWeeklyReservationPercent,
		ModelWeeklyPercent: attempt.AdaptiveModelWeeklyReservationPercent,
		ModelWeeklyName:    attempt.AdaptiveModelWeeklyName,
		SessionCalibrated:  attempt.AdaptiveSessionTokenCalibrated,
		WeeklyCalibrated:   attempt.AdaptiveWeeklyTokenCalibrated,
		ModelCalibrated:    attempt.AdaptiveModelWeeklyTokenCalibrated,
		EstimateConfidence: attempt.AdaptiveEstimateConfidence,
	}
	recordAdaptiveShadowCommitValue(authIndex, item)
}

func recordAdaptiveShadowCommit(authIndex string, percent float64, at time.Time) {
	recordAdaptiveShadowCommitValue(authIndex, adaptiveShadowCommit{At: at.UTC(), Percent: percent, Multiplier: 1})
}

func recordAdaptiveShadowCommitValue(authIndex string, item adaptiveShadowCommit) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || item.Percent <= 0 || math.IsNaN(item.Percent) || math.IsInf(item.Percent, 0) {
		return
	}
	item.At = item.At.UTC()
	if item.Multiplier <= 0 || math.IsNaN(item.Multiplier) || math.IsInf(item.Multiplier, 0) {
		item.Multiplier = 1
	}
	adaptiveShadowRuntime.Lock()
	defer adaptiveShadowRuntime.Unlock()
	account := adaptiveShadowRuntime.Accounts[authIndex]
	if account == nil {
		if len(adaptiveShadowRuntime.Accounts) >= adaptiveShadowMaximumAccounts {
			adaptiveShadowRuntime.Saturated = true
			adaptiveShadowRuntime.DroppedAccounts++
			return
		}
		account = &adaptiveShadowAccount{LearnedScale: 1}
		adaptiveShadowRuntime.Accounts[authIndex] = account
	}
	account.Commits = append(account.Commits, item)
	account.UpdatedAt = item.At
	boundAdaptiveShadowCommits(account)
}

func boundAdaptiveShadowCommits(account *adaptiveShadowAccount) {
	if account == nil || len(account.Commits) <= adaptiveShadowMaximumCommitsPerAccount {
		return
	}
	overflowCount := len(account.Commits) - adaptiveShadowMaximumCommitsPerAccount + 1
	for _, item := range account.Commits[:overflowCount] {
		account.OverflowPercent += item.Percent
		sessionPercent := item.SessionPercent
		if sessionPercent <= 0 {
			sessionPercent = item.Percent
		}
		account.OverflowSessionPercent += sessionPercent
		weeklyPercent := item.WeeklyPercent
		if weeklyPercent <= 0 {
			weeklyPercent = item.Percent
		}
		account.OverflowWeeklyPercent += weeklyPercent
		if item.ModelWeeklyPercent > 0 {
			model := strings.ToLower(strings.TrimSpace(firstNonEmpty(item.ModelWeeklyName, item.Model)))
			if model == "" {
				account.OverflowUnknownModelWeeklyPercent += item.ModelWeeklyPercent
			} else {
				if account.OverflowModelWeeklyPercent == nil {
					account.OverflowModelWeeklyPercent = make(map[string]float64)
				}
				if _, exists := account.OverflowModelWeeklyPercent[model]; exists ||
					len(account.OverflowModelWeeklyPercent) < adaptiveShadowMaximumOverflowModels {
					account.OverflowModelWeeklyPercent[model] += item.ModelWeeklyPercent
				} else {
					// Telemetry remains bounded. Unknown overflow is conservatively
					// applied to model-weekly windows only; it never contaminates
					// session or global-weekly pending.
					account.OverflowUnknownModelWeeklyPercent += item.ModelWeeklyPercent
				}
			}
		}
		// Using the newest coalesced timestamp makes decay conservative: the
		// aggregate never cools faster than any item it replaced.
		if item.At.After(account.OverflowAt) {
			account.OverflowAt = item.At
		}
	}
	copy(account.Commits, account.Commits[overflowCount:])
	account.Commits = account.Commits[:len(account.Commits)-overflowCount]
}

func reconcileAdaptiveShadow(
	cfg pluginConfig,
	authIndex string,
	previous credentialQuotaState,
	refreshed credentialQuotaState,
	observedAt time.Time,
) {
	authIndex = strings.TrimSpace(authIndex)
	previousAt := quotaConfirmedAt(previous)
	if cfg.AdaptiveAllocatorMode != "observe" || authIndex == "" || observedAt.IsZero() ||
		quotaConfidence(previous) != "confirmed" || quotaConfidence(refreshed) != "confirmed" ||
		previousAt.IsZero() || !observedAt.After(previousAt) {
		return
	}
	observedAt = observedAt.UTC()
	tokenEvents := reconcileAdaptiveTokenCalibration(cfg, authIndex, previous, refreshed, previousAt, observedAt)
	adaptiveShadowRuntime.Lock()
	account := adaptiveShadowRuntime.Accounts[authIndex]
	covered := make([]adaptiveShadowCommit, 0)
	predicted := 0.0
	if account != nil {
		remaining := account.Commits[:0]
		for _, item := range account.Commits {
			if !item.At.After(observedAt) {
				if item.At.After(previousAt) {
					predicted += item.Percent
					covered = append(covered, item)
				}
				continue
			}
			remaining = append(remaining, item)
		}
		account.Commits = remaining
		if !account.OverflowAt.IsZero() && !account.OverflowAt.After(observedAt) {
			if account.OverflowAt.After(previousAt) {
				predicted += account.OverflowPercent
				// Coalescing deliberately drops per-request calibration proof. The
				// synthetic commits still preserve attribution, but the interval is
				// excluded from the paired forecast cohort instead of claiming a
				// precision that can no longer be demonstrated.
				if account.OverflowSessionPercent > 0 {
					covered = append(covered, adaptiveShadowCommit{
						At: account.OverflowAt, Percent: account.OverflowSessionPercent,
						WindowKind:     pluginapi.HostAuthQuotaWindowKindSession,
						SessionPercent: account.OverflowSessionPercent, Multiplier: 1,
					})
				}
				if account.OverflowWeeklyPercent > 0 {
					covered = append(covered, adaptiveShadowCommit{
						At: account.OverflowAt, Percent: account.OverflowWeeklyPercent,
						WindowKind:    pluginapi.HostAuthQuotaWindowKindWeekly,
						WeeklyPercent: account.OverflowWeeklyPercent, Multiplier: 1,
					})
				}
				for model, percent := range account.OverflowModelWeeklyPercent {
					if percent <= 0 {
						continue
					}
					covered = append(covered, adaptiveShadowCommit{
						At: account.OverflowAt, Percent: percent,
						WindowKind: pluginapi.HostAuthQuotaWindowKindModelWeekly,
						Model:      model, ModelWeeklyName: model, ModelWeeklyPercent: percent,
						Multiplier: 1,
					})
				}
				if account.OverflowUnknownModelWeeklyPercent > 0 {
					covered = append(covered, adaptiveShadowCommit{
						At: account.OverflowAt, Percent: account.OverflowUnknownModelWeeklyPercent,
						WindowKind:         pluginapi.HostAuthQuotaWindowKindModelWeekly,
						ModelWeeklyPercent: account.OverflowUnknownModelWeeklyPercent,
						Multiplier:         1,
					})
				}
			}
			account.OverflowAt = time.Time{}
			account.OverflowPercent = 0
			account.OverflowSessionPercent = 0
			account.OverflowWeeklyPercent = 0
			account.OverflowModelWeeklyPercent = nil
			account.OverflowUnknownModelWeeklyPercent = 0
		}
		pruneAdaptiveShadowAccount(account, cfg, observedAt)
		actual := math.Max(
			math.Max(previous.Session.RemainingPercent-refreshed.Session.RemainingPercent, 0),
			math.Max(previous.Weekly.RemainingPercent-refreshed.Weekly.RemainingPercent, 0),
		)
		if predicted > 0 && actual > 0 {
			current := effectiveAdaptiveShadowLearnedScale(account, cfg, observedAt)
			ratio := math.Min(math.Max(actual/predicted, 1), adaptiveShadowMaximumLearnedScale)
			account.LearnedScale = math.Max(current, ratio)
			account.LearnedAt = observedAt
		}
		account.UpdatedAt = observedAt
	}
	adaptiveShadowRuntime.Unlock()

	recordQuotaConsumptionReconciliation(
		authIndex, previous, refreshed, previousAt, observedAt,
		applyAdaptiveTokenWeightsToShadowCommits(covered, tokenEvents),
	)
}

func adaptiveShadowLearnedScale(authIndex string, cfg pluginConfig, now time.Time) float64 {
	if authIndex == "" {
		return 1
	}
	adaptiveShadowRuntime.Lock()
	defer adaptiveShadowRuntime.Unlock()
	account := adaptiveShadowRuntime.Accounts[authIndex]
	if account == nil {
		return 1
	}
	pruneAdaptiveShadowAccount(account, cfg, now)
	return effectiveAdaptiveShadowLearnedScale(account, cfg, now)
}

func effectiveAdaptiveShadowLearnedScale(account *adaptiveShadowAccount, cfg pluginConfig, now time.Time) float64 {
	if account == nil || account.LearnedScale <= 1 || account.LearnedAt.IsZero() {
		return 1
	}
	age := now.UTC().Sub(account.LearnedAt)
	if age <= 0 {
		return math.Max(account.LearnedScale, 1)
	}
	if age >= time.Duration(cfg.AdaptiveCoolingMaxAgeSeconds)*time.Second {
		return 1
	}
	decay := math.Pow(0.5, age.Seconds()/float64(cfg.AdaptiveCoolingHalfLifeSeconds))
	return 1 + (math.Max(account.LearnedScale, 1)-1)*decay
}

func adaptiveShadowCommitWeight(percent float64, at time.Time, cfg pluginConfig, now time.Time) float64 {
	age := now.UTC().Sub(at.UTC())
	if age <= 0 {
		return percent
	}
	if age >= time.Duration(cfg.AdaptiveCoolingMaxAgeSeconds)*time.Second {
		return 0
	}
	return percent * math.Pow(0.5, age.Seconds()/float64(cfg.AdaptiveCoolingHalfLifeSeconds))
}

func pruneAdaptiveShadowAccount(account *adaptiveShadowAccount, cfg pluginConfig, now time.Time) {
	if account == nil {
		return
	}
	cutoff := now.UTC().Add(-time.Duration(cfg.AdaptiveCoolingMaxAgeSeconds) * time.Second)
	kept := account.Commits[:0]
	for _, item := range account.Commits {
		if item.At.After(cutoff) {
			kept = append(kept, item)
		}
	}
	account.Commits = kept
	if !account.OverflowAt.After(cutoff) {
		account.OverflowAt = time.Time{}
		account.OverflowPercent = 0
		account.OverflowSessionPercent = 0
		account.OverflowWeeklyPercent = 0
		account.OverflowModelWeeklyPercent = nil
		account.OverflowUnknownModelWeeklyPercent = 0
	}
	if !account.LearnedAt.After(cutoff) {
		account.LearnedScale = 1
		account.LearnedAt = time.Time{}
	}
}

func adaptiveShadowSummary(cfg pluginConfig, authIndexes []string, now time.Time) adaptiveShadowPublicView {
	tokenCalibration := adaptiveTokenCalibrationSummary(authIndexes, now)
	forecastBacktest := adaptiveForecastBacktestSummary(authIndexes, now)
	allowed := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
			allowed[authIndex] = struct{}{}
		}
	}
	filter := authIndexes != nil
	view := adaptiveShadowPublicView{
		Mode:                       cfg.AdaptiveAllocatorMode,
		Effect:                     adaptiveShadowEffect(cfg),
		RoutingEnforced:            false,
		AdditionalProviderRequests: false,
		QuotaSnapshotSource:        "existing_background_cache",
		CoolingHalfLifeSeconds:     cfg.AdaptiveCoolingHalfLifeSeconds,
		CoolingMaxAgeSeconds:       cfg.AdaptiveCoolingMaxAgeSeconds,
		MaximumLearnedScale:        1,
		TokenCalibration:           tokenCalibration,
		ForecastBacktest:           forecastBacktest,
		Note:                       "Теневой расчёт не блокирует запросы и не меняет маршруты; он не выполняет дополнительных обращений к подпискам.",
	}
	adaptiveShadowRuntime.Lock()
	defer adaptiveShadowRuntime.Unlock()
	for authIndex, account := range adaptiveShadowRuntime.Accounts {
		if filter {
			if _, ok := allowed[authIndex]; !ok {
				continue
			}
		}
		pruneAdaptiveShadowAccount(account, cfg, now)
		raw, effective := account.OverflowPercent, adaptiveShadowCommitWeight(account.OverflowPercent, account.OverflowAt, cfg, now)
		for _, item := range account.Commits {
			raw += item.Percent
			effective += adaptiveShadowCommitWeight(item.Percent, item.At, cfg, now)
		}
		learned := effectiveAdaptiveShadowLearnedScale(account, cfg, now)
		if raw <= 0 && learned <= 1 && now.UTC().Sub(account.UpdatedAt) >= time.Duration(cfg.AdaptiveCoolingMaxAgeSeconds)*time.Second {
			delete(adaptiveShadowRuntime.Accounts, authIndex)
			continue
		}
		view.TrackedAccounts++
		view.TrackedCommitments += len(account.Commits)
		if account.OverflowPercent > 0 {
			view.TrackedCommitments++
		}
		view.RawPendingPercent += raw
		view.EffectivePendingPercent += effective
		view.MaximumLearnedScale = math.Max(view.MaximumLearnedScale, learned)
	}
	if len(adaptiveShadowRuntime.Accounts) < adaptiveShadowMaximumAccounts {
		adaptiveShadowRuntime.Saturated = false
	}
	view.Saturated = adaptiveShadowRuntime.Saturated
	view.DroppedAccounts = adaptiveShadowRuntime.DroppedAccounts
	view.RawPendingPercent = adaptiveShadowRound(view.RawPendingPercent)
	view.EffectivePendingPercent = adaptiveShadowRound(view.EffectivePendingPercent)
	view.MaximumLearnedScale = adaptiveShadowRound(view.MaximumLearnedScale)
	return view
}

func adaptiveShadowAuthIndexes(auths []pluginapi.HostAuthFileEntry) []string {
	out := make([]string, 0, len(auths))
	for _, auth := range auths {
		if value := strings.TrimSpace(auth.AuthIndex); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func adaptiveShadowRound(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func adaptiveShadowModelFactor(model string) float64 {
	value := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(value, "fable"):
		return 2.2
	case strings.Contains(value, "opus"):
		return 2
	case strings.Contains(value, "sonnet"):
		return 1.25
	case strings.Contains(value, "haiku"):
		return 0.7
	case strings.Contains(value, "codex"), strings.Contains(value, "gpt"):
		return 1.35
	default:
		return 1
	}
}

func adaptiveShadowEffortFactor(effort string) float64 {
	switch normalizeEffort(effort) {
	case "minimal", "low":
		return 0.8
	case "medium", "auto", "none", "":
		return 1
	case "high":
		return 1.25
	case "xhigh":
		return 1.6
	case "ultra", "max":
		return 2
	default:
		return 1.15
	}
}

func resetAdaptiveShadowForTest() {
	adaptiveShadowRuntime.Lock()
	adaptiveShadowRuntime.Accounts = make(map[string]*adaptiveShadowAccount)
	adaptiveShadowRuntime.Saturated = false
	adaptiveShadowRuntime.DroppedAccounts = 0
	adaptiveShadowRuntime.Unlock()
	resetAdaptiveTokenCalibrationForTest()
}

// adaptiveShadowDeclaredOutputTokens is a bounded-allocation top-level JSON
// scanner. Strings and nested tool schemas cannot impersonate max_tokens.
func adaptiveShadowDeclaredOutputTokens(body []byte) (float64, bool) {
	cursor := adaptiveShadowSkipWhitespace(body, 0)
	if cursor >= len(body) || body[cursor] != '{' {
		return adaptiveShadowMaximumOutputTokens, false
	}
	cursor++
	maximum, found := uint64(0), false
	for {
		cursor = adaptiveShadowSkipWhitespace(body, cursor)
		if cursor >= len(body) {
			return adaptiveShadowMaximumOutputTokens, false
		}
		if body[cursor] == '}' {
			cursor = adaptiveShadowSkipWhitespace(body, cursor+1)
			if cursor != len(body) {
				return adaptiveShadowMaximumOutputTokens, false
			}
			if !found {
				return adaptiveShadowMaximumOutputTokens, false
			}
			return float64(maximum), found
		}
		if body[cursor] != '"' {
			return adaptiveShadowMaximumOutputTokens, false
		}
		keyStart := cursor + 1
		keyEnd, escaped, ok := adaptiveShadowScanString(body, cursor)
		if !ok || escaped {
			return adaptiveShadowMaximumOutputTokens, false
		}
		key := body[keyStart : keyEnd-1]
		cursor = adaptiveShadowSkipWhitespace(body, keyEnd)
		if cursor >= len(body) || body[cursor] != ':' {
			return adaptiveShadowMaximumOutputTokens, false
		}
		cursor = adaptiveShadowSkipWhitespace(body, cursor+1)
		if adaptiveShadowOutputKey(key) {
			value, next, valid := adaptiveShadowScanUnsigned(body, cursor)
			if !valid || value > uint64(adaptiveShadowMaximumOutputTokens) {
				return adaptiveShadowMaximumOutputTokens, false
			}
			if value > maximum {
				maximum = value
			}
			found = true
			cursor = next
		} else {
			var valid bool
			cursor, valid = adaptiveShadowSkipValue(body, cursor)
			if !valid {
				return adaptiveShadowMaximumOutputTokens, false
			}
		}
		cursor = adaptiveShadowSkipWhitespace(body, cursor)
		if cursor >= len(body) {
			return adaptiveShadowMaximumOutputTokens, false
		}
		switch body[cursor] {
		case ',':
			cursor++
		case '}':
			continue
		default:
			return adaptiveShadowMaximumOutputTokens, false
		}
	}
}

func adaptiveShadowOutputKey(key []byte) bool {
	return bytes.Equal(key, []byte("max_tokens")) ||
		bytes.Equal(key, []byte("max_output_tokens")) ||
		bytes.Equal(key, []byte("max_completion_tokens"))
}

func adaptiveShadowSkipWhitespace(body []byte, cursor int) int {
	for cursor < len(body) {
		switch body[cursor] {
		case ' ', '\t', '\r', '\n':
			cursor++
		default:
			return cursor
		}
	}
	return cursor
}

func adaptiveShadowScanString(body []byte, cursor int) (int, bool, bool) {
	if cursor >= len(body) || body[cursor] != '"' {
		return cursor, false, false
	}
	escaped := false
	for cursor++; cursor < len(body); cursor++ {
		switch body[cursor] {
		case '"':
			return cursor + 1, escaped, true
		case '\\':
			escaped = true
			cursor++
			if cursor >= len(body) {
				return cursor, escaped, false
			}
			if body[cursor] == 'u' {
				if cursor+4 >= len(body) {
					return cursor, escaped, false
				}
				for index := cursor + 1; index <= cursor+4; index++ {
					if !adaptiveShadowHex(body[index]) {
						return cursor, escaped, false
					}
				}
				cursor += 4
			} else if !strings.ContainsRune(`"\\/bfnrt`, rune(body[cursor])) {
				return cursor, escaped, false
			}
		default:
			if body[cursor] < 0x20 {
				return cursor, escaped, false
			}
		}
	}
	return cursor, escaped, false
}

func adaptiveShadowHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func adaptiveShadowScanUnsigned(body []byte, cursor int) (uint64, int, bool) {
	if cursor >= len(body) || body[cursor] < '0' || body[cursor] > '9' {
		return 0, cursor, false
	}
	start := cursor
	value := uint64(0)
	for cursor < len(body) && body[cursor] >= '0' && body[cursor] <= '9' {
		digit := uint64(body[cursor] - '0')
		if value > (uint64(adaptiveShadowMaximumOutputTokens)-digit)/10 {
			return 0, cursor, false
		}
		value = value*10 + digit
		cursor++
	}
	if cursor-start > 1 && body[start] == '0' {
		return 0, cursor, false
	}
	next := adaptiveShadowSkipWhitespace(body, cursor)
	if next >= len(body) || body[next] != ',' && body[next] != '}' {
		return 0, cursor, false
	}
	return value, cursor, true
}

func adaptiveShadowSkipValue(body []byte, cursor int) (int, bool) {
	if cursor >= len(body) {
		return cursor, false
	}
	if body[cursor] == '"' {
		next, _, ok := adaptiveShadowScanString(body, cursor)
		return next, ok
	}
	if body[cursor] != '{' && body[cursor] != '[' {
		start := cursor
		for cursor < len(body) && body[cursor] != ',' && body[cursor] != '}' && body[cursor] != ']' &&
			body[cursor] != ' ' && body[cursor] != '\t' && body[cursor] != '\r' && body[cursor] != '\n' {
			cursor++
		}
		return cursor, cursor > start
	}
	var stack [128]byte
	depth := 1
	stack[0] = body[cursor]
	for cursor++; cursor < len(body); cursor++ {
		switch body[cursor] {
		case '"':
			next, _, ok := adaptiveShadowScanString(body, cursor)
			if !ok {
				return cursor, false
			}
			cursor = next - 1
		case '{', '[':
			if depth >= len(stack) {
				return cursor, false
			}
			stack[depth] = body[cursor]
			depth++
		case '}', ']':
			want := byte('{')
			if body[cursor] == ']' {
				want = '['
			}
			if depth == 0 || stack[depth-1] != want {
				return cursor, false
			}
			depth--
			if depth == 0 {
				return cursor + 1, true
			}
		}
	}
	return cursor, false
}
