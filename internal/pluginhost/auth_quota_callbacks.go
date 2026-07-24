package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	claudeQuotaUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeQuotaProfileURL = "https://api.anthropic.com/api/oauth/profile"
	codexQuotaUsageURL    = "https://chatgpt.com/backend-api/wham/usage"

	authQuotaRequestTimeout = 12 * time.Second
	authQuotaClockSkew      = 15 * time.Minute
	authQuotaMaxBodyBytes   = 1 << 20
)

type authQuotaHTTPDoer interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type authQuotaFailure struct {
	code    string
	message string
}

type authQuotaResult struct {
	workspaceLabel string
	accountLabel   string
	planLabel      string
	observedAt     time.Time
	windows        []pluginapi.HostAuthQuotaWindow
}

type claudeQuotaWindowPayload struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeQuotaUsagePayload struct {
	FiveHour          *claudeQuotaWindowPayload `json:"five_hour"`
	SevenDay          *claudeQuotaWindowPayload `json:"seven_day"`
	SevenDayOAuthApps *claudeQuotaWindowPayload `json:"seven_day_oauth_apps"`
	SevenDayOpus      *claudeQuotaWindowPayload `json:"seven_day_opus"`
	SevenDaySonnet    *claudeQuotaWindowPayload `json:"seven_day_sonnet"`
	SevenDayCowork    *claudeQuotaWindowPayload `json:"seven_day_cowork"`
}

type claudeQuotaProfilePayload struct {
	Account      *claudeQuotaProfileAccount      `json:"account"`
	Organization *claudeQuotaProfileOrganization `json:"organization"`
}

type claudeQuotaProfileAccount struct {
	FullName     string `json:"full_name"`
	DisplayName  string `json:"display_name"`
	Email        string `json:"email"`
	HasClaudeMax *bool  `json:"has_claude_max"`
	HasClaudePro *bool  `json:"has_claude_pro"`
}

type claudeQuotaProfileOrganization struct {
	Name               string `json:"name"`
	OrganizationType   string `json:"organization_type"`
	RateLimitTier      string `json:"rate_limit_tier"`
	SubscriptionStatus string `json:"subscription_status"`
}

type flexibleQuotaNumber float64

func (n *flexibleQuotaNumber) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("number is missing")
	}
	valueText := string(raw)
	if raw[0] == '"' {
		var value string
		if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
			return fmt.Errorf("invalid quoted number: %w", errUnmarshal)
		}
		valueText = strings.TrimSpace(value)
	}
	value, errParse := strconv.ParseFloat(valueText, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("invalid number")
	}
	*n = flexibleQuotaNumber(value)
	return nil
}

type codexQuotaUsagePayload struct {
	PlanType  string               `json:"plan_type"`
	RateLimit *codexQuotaRateLimit `json:"rate_limit"`
}

type codexQuotaRateLimit struct {
	PrimaryWindow   *codexQuotaWindowPayload `json:"primary_window"`
	SecondaryWindow *codexQuotaWindowPayload `json:"secondary_window"`
}

type codexQuotaWindowPayload struct {
	UsedPercent        *flexibleQuotaNumber `json:"used_percent"`
	LimitWindowSeconds *flexibleQuotaNumber `json:"limit_window_seconds"`
	ResetAfterSeconds  *flexibleQuotaNumber `json:"reset_after_seconds"`
	ResetAt            *flexibleQuotaNumber `json:"reset_at"`
}

func (h *Host) callHostAuthQuotaGet(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostAuthQuotaRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host auth quota request: %w", errUnmarshal)
	}
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("auth_index is required")
	}
	auth, errGet := h.authByIndex(authIndex)
	if errGet != nil {
		return nil, errGet
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	response := newHostAuthQuotaResponse(auth, provider)
	if provider != "claude" && provider != "codex" {
		return marshalRPCResult(unknownHostAuthQuotaResponse(response, authQuotaFailure{
			code:    "provider_not_supported",
			message: "live quota is available only for claude and codex credentials",
		}))
	}

	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, authQuotaRequestTimeout)
	defer cancel()

	client := h.newHTTPClient(auth, provider)
	var (
		result  authQuotaResult
		failure *authQuotaFailure
	)
	switch provider {
	case "claude":
		result, failure = fetchClaudeAuthQuota(fetchCtx, client, auth)
	case "codex":
		result, failure = fetchCodexAuthQuota(fetchCtx, client, auth)
	}
	if failure != nil {
		response.ObservedAt = time.Now().UTC()
		return marshalRPCResult(unknownHostAuthQuotaResponse(response, *failure))
	}

	response.WorkspaceLabel = result.workspaceLabel
	response.AccountLabel = result.accountLabel
	response.PlanLabel = result.planLabel
	response.ObservedAt = result.observedAt
	response.Confidence = pluginapi.HostAuthQuotaConfidenceConfirmed
	response.Windows = result.windows
	return marshalRPCResult(response)
}

func newHostAuthQuotaResponse(auth *coreauth.Auth, provider string) pluginapi.HostAuthQuotaResponse {
	response := pluginapi.HostAuthQuotaResponse{
		Provider:       provider,
		ObservedAt:     time.Now().UTC(),
		Confidence:     pluginapi.HostAuthQuotaConfidenceUnknown,
		Windows:        make([]pluginapi.HostAuthQuotaWindow, 0),
		WorkspaceLabel: safeQuotaLabel(authMetadataString(auth, "organization_name")),
		AccountLabel:   safeQuotaLabel(authMetadataString(auth, "email")),
		PlanLabel:      safeQuotaLabel(authAttribute(auth, "plan_type")),
	}
	if auth != nil {
		auth.EnsureIndex()
		response.AuthIndex = auth.Index
		response.AuthID = strings.TrimSpace(auth.ID)
	}
	if response.PlanLabel == "" {
		response.PlanLabel = safeQuotaLabel(authMetadataString(auth, "plan_type"))
	}
	return response
}

func unknownHostAuthQuotaResponse(response pluginapi.HostAuthQuotaResponse, failure authQuotaFailure) pluginapi.HostAuthQuotaResponse {
	response.Confidence = pluginapi.HostAuthQuotaConfidenceUnknown
	response.Windows = make([]pluginapi.HostAuthQuotaWindow, 0)
	response.Error = &pluginapi.HostAuthQuotaError{
		Code:    failure.code,
		Message: failure.message,
	}
	if response.ObservedAt.IsZero() {
		response.ObservedAt = time.Now().UTC()
	}
	return response
}

func fetchClaudeAuthQuota(ctx context.Context, client authQuotaHTTPDoer, auth *coreauth.Auth) (authQuotaResult, *authQuotaFailure) {
	token, failure := authQuotaAccessToken(auth)
	if failure != nil {
		return authQuotaResult{}, failure
	}
	headers := http.Header{
		"Authorization":  []string{"Bearer " + token},
		"Accept":         []string{"application/json"},
		"Content-Type":   []string{"application/json"},
		"anthropic-beta": []string{"oauth-2025-04-20"},
	}
	var (
		usageBody      []byte
		profileBody    []byte
		usageFailure   *authQuotaFailure
		profileFailure *authQuotaFailure
		wait           sync.WaitGroup
	)
	wait.Add(2)
	go func() {
		defer wait.Done()
		usageBody, usageFailure = fetchAuthQuotaEndpoint(ctx, client, claudeQuotaUsageURL, headers)
	}()
	go func() {
		defer wait.Done()
		profileBody, profileFailure = fetchAuthQuotaEndpoint(ctx, client, claudeQuotaProfileURL, headers)
	}()
	wait.Wait()
	if usageFailure != nil {
		return authQuotaResult{}, usageFailure
	}
	// Profile data is presentation metadata only. A transient profile failure
	// must not discard strictly validated usage windows or disable the
	// secondary pool; labels safely fall back to the local auth record.
	if profileFailure != nil {
		profileBody = nil
	}
	observedAt := time.Now().UTC()
	return parseClaudeAuthQuota(usageBody, profileBody, observedAt, auth)
}

func fetchCodexAuthQuota(ctx context.Context, client authQuotaHTTPDoer, auth *coreauth.Auth) (authQuotaResult, *authQuotaFailure) {
	token, failure := authQuotaAccessToken(auth)
	if failure != nil {
		return authQuotaResult{}, failure
	}
	headers := http.Header{
		"Authorization": []string{"Bearer " + token},
		"Accept":        []string{"application/json"},
		"Content-Type":  []string{"application/json"},
		"User-Agent":    []string{"codex_cli_rs/0.76.0"},
	}
	if accountID := safeHeaderValue(authMetadataString(auth, "account_id")); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	usageBody, failure := fetchAuthQuotaEndpoint(ctx, client, codexQuotaUsageURL, headers)
	if failure != nil {
		return authQuotaResult{}, failure
	}
	observedAt := time.Now().UTC()
	return parseCodexAuthQuota(usageBody, observedAt, auth)
}

func fetchAuthQuotaEndpoint(ctx context.Context, client authQuotaHTTPDoer, url string, headers http.Header) ([]byte, *authQuotaFailure) {
	if client == nil {
		return nil, &authQuotaFailure{code: "quota_unavailable", message: "host quota transport is unavailable"}
	}
	response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     url,
		Headers: cloneHeader(headers),
	})
	if errDo != nil {
		return nil, &authQuotaFailure{code: "quota_unavailable", message: "live quota request failed"}
	}
	if response.StatusCode == http.StatusUnauthorized {
		return nil, &authQuotaFailure{code: "auth_stale", message: "provider rejected the current credential"}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &authQuotaFailure{
			code:    "quota_unavailable",
			message: fmt.Sprintf("provider quota endpoint returned HTTP %d", response.StatusCode),
		}
	}
	if len(response.Body) == 0 {
		return nil, &authQuotaFailure{code: "quota_response_invalid", message: "provider quota response is empty"}
	}
	if len(response.Body) > authQuotaMaxBodyBytes {
		return nil, &authQuotaFailure{code: "quota_response_invalid", message: "provider quota response is too large"}
	}
	return bytes.Clone(response.Body), nil
}

func parseClaudeAuthQuota(usageBody, profileBody []byte, observedAt time.Time, auth *coreauth.Auth) (authQuotaResult, *authQuotaFailure) {
	var usage claudeQuotaUsagePayload
	if errDecode := decodeAuthQuotaJSON(usageBody, &usage); errDecode != nil {
		return authQuotaResult{}, invalidQuotaPayloadFailure()
	}
	var profile claudeQuotaProfilePayload
	if len(profileBody) > 0 {
		if errDecode := decodeAuthQuotaJSON(profileBody, &profile); errDecode != nil {
			profile = claudeQuotaProfilePayload{}
		}
	}
	if usage.FiveHour == nil || usage.SevenDay == nil {
		return authQuotaResult{}, &authQuotaFailure{
			code:    "quota_windows_missing",
			message: "provider quota response is missing required session or weekly windows",
		}
	}

	observedAt = observedAt.UTC()
	windows := make([]pluginapi.HostAuthQuotaWindow, 0, 6)
	coreWindows := []struct {
		id      string
		kind    string
		family  string
		maxAge  time.Duration
		payload *claudeQuotaWindowPayload
	}{
		{
			id:      "five_hour",
			kind:    pluginapi.HostAuthQuotaWindowKindSession,
			maxAge:  5 * time.Hour,
			payload: usage.FiveHour,
		},
		{
			id:      "seven_day",
			kind:    pluginapi.HostAuthQuotaWindowKindWeekly,
			maxAge:  7 * 24 * time.Hour,
			payload: usage.SevenDay,
		},
	}
	for _, item := range coreWindows {
		window, failure := normalizeClaudeQuotaWindow(item.id, item.kind, item.family, item.maxAge, item.payload, observedAt)
		if failure != nil {
			return authQuotaResult{}, failure
		}
		windows = append(windows, window)
	}

	modelWindows := []struct {
		id      string
		family  string
		payload *claudeQuotaWindowPayload
	}{
		{id: "seven_day_oauth_apps", family: "oauth_apps", payload: usage.SevenDayOAuthApps},
		{id: "seven_day_opus", family: "opus", payload: usage.SevenDayOpus},
		{id: "seven_day_sonnet", family: "sonnet", payload: usage.SevenDaySonnet},
		{id: "seven_day_cowork", family: "cowork", payload: usage.SevenDayCowork},
	}
	for _, item := range modelWindows {
		if item.payload == nil {
			continue
		}
		window, failure := normalizeClaudeQuotaWindow(
			item.id,
			pluginapi.HostAuthQuotaWindowKindModelWeekly,
			item.family,
			7*24*time.Hour,
			item.payload,
			observedAt,
		)
		if failure != nil {
			return authQuotaResult{}, failure
		}
		windows = append(windows, window)
	}

	accountLabel := firstSafeQuotaLabel(
		claudeProfileAccountValue(profile.Account, func(account *claudeQuotaProfileAccount) string { return account.DisplayName }),
		claudeProfileAccountValue(profile.Account, func(account *claudeQuotaProfileAccount) string { return account.FullName }),
		claudeProfileAccountValue(profile.Account, func(account *claudeQuotaProfileAccount) string { return account.Email }),
		authMetadataString(auth, "email"),
	)
	workspaceLabel := firstSafeQuotaLabel(
		claudeProfileOrganizationValue(profile.Organization, func(organization *claudeQuotaProfileOrganization) string { return organization.Name }),
		authMetadataString(auth, "organization_name"),
	)
	return authQuotaResult{
		workspaceLabel: workspaceLabel,
		accountLabel:   accountLabel,
		planLabel:      claudeQuotaPlanLabel(profile, auth),
		observedAt:     observedAt,
		windows:        windows,
	}, nil
}

func claudeProfileAccountValue(
	account *claudeQuotaProfileAccount,
	value func(*claudeQuotaProfileAccount) string,
) string {
	if account == nil || value == nil {
		return ""
	}
	return value(account)
}

func claudeProfileOrganizationValue(
	organization *claudeQuotaProfileOrganization,
	value func(*claudeQuotaProfileOrganization) string,
) string {
	if organization == nil || value == nil {
		return ""
	}
	return value(organization)
}

func normalizeClaudeQuotaWindow(
	id string,
	kind string,
	family string,
	maxAge time.Duration,
	payload *claudeQuotaWindowPayload,
	observedAt time.Time,
) (pluginapi.HostAuthQuotaWindow, *authQuotaFailure) {
	if payload == nil || payload.Utilization == nil {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	used := *payload.Utilization
	if !validQuotaPercent(used) {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	if payload.ResetsAt == nil {
		// Anthropic returns utilization=0 with resets_at=null before a rolling
		// window has been started. This is a confirmed full window, not missing
		// quota data; the first request starts its reset clock.
		if used == 0 {
			return inactiveQuotaWindow(id, kind, family), nil
		}
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	resetAt, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(*payload.ResetsAt))
	if errParse != nil {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	resetAt = resetAt.UTC()
	if !validQuotaReset(resetAt, observedAt, maxAge) {
		return pluginapi.HostAuthQuotaWindow{}, staleQuotaPayloadFailure()
	}
	return normalizedQuotaWindow(id, kind, family, used, resetAt), nil
}

func parseCodexAuthQuota(usageBody []byte, observedAt time.Time, auth *coreauth.Auth) (authQuotaResult, *authQuotaFailure) {
	var usage codexQuotaUsagePayload
	if errDecode := decodeAuthQuotaJSON(usageBody, &usage); errDecode != nil {
		return authQuotaResult{}, invalidQuotaPayloadFailure()
	}
	if usage.RateLimit == nil ||
		(usage.RateLimit.PrimaryWindow == nil && usage.RateLimit.SecondaryWindow == nil) {
		return authQuotaResult{}, &authQuotaFailure{
			code:    "quota_windows_missing",
			message: "provider quota response is missing all quota windows",
		}
	}

	observedAt = observedAt.UTC()
	windows := make([]pluginapi.HostAuthQuotaWindow, 0, 2)
	if usage.RateLimit.PrimaryWindow != nil {
		primary, failure := normalizeCodexQuotaWindow("primary_window", usage.RateLimit.PrimaryWindow, observedAt)
		if failure != nil {
			return authQuotaResult{}, failure
		}
		windows = append(windows, primary)
	}
	if usage.RateLimit.SecondaryWindow != nil {
		secondary, failure := normalizeCodexQuotaWindow("secondary_window", usage.RateLimit.SecondaryWindow, observedAt)
		if failure != nil {
			return authQuotaResult{}, failure
		}
		windows = append(windows, secondary)
	}
	hasSession := false
	hasWeekly := false
	for _, window := range windows {
		switch window.Kind {
		case pluginapi.HostAuthQuotaWindowKindSession:
			if hasSession {
				return authQuotaResult{}, invalidQuotaPayloadFailure()
			}
			hasSession = true
		case pluginapi.HostAuthQuotaWindowKindWeekly:
			if hasWeekly {
				return authQuotaResult{}, invalidQuotaPayloadFailure()
			}
			hasWeekly = true
		default:
			return authQuotaResult{}, invalidQuotaPayloadFailure()
		}
	}
	// Some Codex plans expose only a weekly limit. Keep that live window
	// confirmed and explicitly mark the absent class as not applicable instead
	// of disabling the whole credential.
	if !hasSession {
		windows = append(windows, notApplicableQuotaWindow("session_not_applicable", pluginapi.HostAuthQuotaWindowKindSession))
	}
	if !hasWeekly {
		windows = append(windows, notApplicableQuotaWindow("weekly_not_applicable", pluginapi.HostAuthQuotaWindowKindWeekly))
	}

	planLabel := safeQuotaLabel(usage.PlanType)
	if planLabel == "" {
		planLabel = firstSafeQuotaLabel(
			authAttribute(auth, "plan_type"),
			authMetadataString(auth, "plan_type"),
		)
	}
	return authQuotaResult{
		workspaceLabel: firstSafeQuotaLabel(authMetadataString(auth, "organization_name")),
		accountLabel:   firstSafeQuotaLabel(authMetadataString(auth, "email")),
		planLabel:      planLabel,
		observedAt:     observedAt,
		windows:        windows,
	}, nil
}

func normalizeCodexQuotaWindow(id string, payload *codexQuotaWindowPayload, observedAt time.Time) (pluginapi.HostAuthQuotaWindow, *authQuotaFailure) {
	if payload == nil || payload.UsedPercent == nil || payload.LimitWindowSeconds == nil {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	used := float64(*payload.UsedPercent)
	windowSeconds := float64(*payload.LimitWindowSeconds)
	if !validQuotaPercent(used) || !validPositiveWholeNumber(windowSeconds) {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	if windowSeconds < 60 || windowSeconds > float64((32*24*time.Hour)/time.Second) {
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}

	kind := pluginapi.HostAuthQuotaWindowKindSession
	if windowSeconds > float64((24*time.Hour)/time.Second) {
		kind = pluginapi.HostAuthQuotaWindowKindWeekly
	}
	maxAge := time.Duration(windowSeconds) * time.Second

	var resetAt time.Time
	switch {
	case payload.ResetAt != nil:
		resetUnix := float64(*payload.ResetAt)
		if !validPositiveWholeNumber(resetUnix) {
			return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
		}
		resetAt = time.Unix(int64(resetUnix), 0).UTC()
	case payload.ResetAfterSeconds != nil:
		resetAfter := float64(*payload.ResetAfterSeconds)
		if !validPositiveWholeNumber(resetAfter) || resetAfter > windowSeconds+authQuotaClockSkew.Seconds() {
			return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
		}
		resetAt = observedAt.Add(time.Duration(resetAfter) * time.Second).UTC()
	default:
		return pluginapi.HostAuthQuotaWindow{}, invalidQuotaPayloadFailure()
	}
	if !validQuotaReset(resetAt, observedAt, maxAge) {
		return pluginapi.HostAuthQuotaWindow{}, staleQuotaPayloadFailure()
	}
	return normalizedQuotaWindow(id, kind, "", used, resetAt), nil
}

func normalizedQuotaWindow(id, kind, family string, used float64, resetAt time.Time) pluginapi.HostAuthQuotaWindow {
	remaining := 100 - used
	if math.Abs(remaining) < 1e-9 {
		remaining = 0
	}
	return pluginapi.HostAuthQuotaWindow{
		ID:               id,
		Kind:             kind,
		ModelFamily:      family,
		UsedPercent:      used,
		RemainingPercent: remaining,
		ResetAt:          resetAt.UTC(),
		ResetMode:        pluginapi.HostAuthQuotaResetModeScheduled,
	}
}

func inactiveQuotaWindow(id, kind, family string) pluginapi.HostAuthQuotaWindow {
	return pluginapi.HostAuthQuotaWindow{
		ID:               id,
		Kind:             kind,
		ModelFamily:      family,
		UsedPercent:      0,
		RemainingPercent: 100,
		ResetMode:        pluginapi.HostAuthQuotaResetModeInactive,
	}
}

func notApplicableQuotaWindow(id, kind string) pluginapi.HostAuthQuotaWindow {
	return pluginapi.HostAuthQuotaWindow{
		ID:               id,
		Kind:             kind,
		UsedPercent:      0,
		RemainingPercent: 100,
		ResetMode:        pluginapi.HostAuthQuotaResetModeNotApplicable,
	}
}

func validQuotaPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func validPositiveWholeNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && math.Trunc(value) == value
}

func validQuotaReset(resetAt, observedAt time.Time, maxAge time.Duration) bool {
	if resetAt.IsZero() || observedAt.IsZero() || maxAge <= 0 {
		return false
	}
	return resetAt.After(observedAt) && !resetAt.After(observedAt.Add(maxAge+authQuotaClockSkew))
}

func claudeQuotaPlanLabel(profile claudeQuotaProfilePayload, auth *coreauth.Auth) string {
	if profile.Account != nil {
		if profile.Account.HasClaudeMax != nil && *profile.Account.HasClaudeMax {
			return "max"
		}
	}
	if profile.Organization != nil {
		organizationType := strings.ToLower(strings.TrimSpace(profile.Organization.OrganizationType))
		for _, plan := range []string{"enterprise", "business", "team"} {
			if strings.Contains(organizationType, plan) {
				return plan
			}
		}
	}
	if profile.Account != nil {
		if profile.Account.HasClaudePro != nil && *profile.Account.HasClaudePro {
			return "pro"
		}
	}
	if profile.Account != nil &&
		profile.Account.HasClaudeMax != nil &&
		profile.Account.HasClaudePro != nil &&
		!*profile.Account.HasClaudeMax &&
		!*profile.Account.HasClaudePro {
		return "free"
	}
	return firstSafeQuotaLabel(
		authAttribute(auth, "plan_type"),
		authMetadataString(auth, "plan_type"),
	)
}

func authQuotaAccessToken(auth *coreauth.Auth) (string, *authQuotaFailure) {
	token := strings.TrimSpace(authMetadataString(auth, "access_token"))
	if token == "" || len(token) > 16*1024 || strings.ContainsAny(token, "\r\n") {
		return "", &authQuotaFailure{code: "auth_stale", message: "current OAuth access token is unavailable"}
	}
	return token, nil
}

func authMetadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func safeHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func safeQuotaLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	const maxLabelRunes = 160
	if utf8.RuneCountInString(value) <= maxLabelRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxLabelRunes])
}

func firstSafeQuotaLabel(values ...string) string {
	for _, value := range values {
		if safe := safeQuotaLabel(value); safe != "" {
			return safe
		}
	}
	return ""
}

func decodeAuthQuotaJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > authQuotaMaxBodyBytes {
		return fmt.Errorf("invalid quota payload size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if errDecode := decoder.Decode(target); errDecode != nil {
		return errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		if errTrailing == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return errTrailing
	}
	return nil
}

func invalidQuotaPayloadFailure() *authQuotaFailure {
	return &authQuotaFailure{
		code:    "quota_response_invalid",
		message: "provider quota response failed strict validation",
	}
}

func staleQuotaPayloadFailure() *authQuotaFailure {
	return &authQuotaFailure{
		code:    "quota_response_stale",
		message: "provider quota response contains an expired or implausible reset time",
	}
}
