package pluginhost

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type quotaCallbackTestDoer struct {
	mu        sync.Mutex
	responses map[string]pluginapi.HTTPResponse
	requests  []pluginapi.HTTPRequest
}

func (d *quotaCallbackTestDoer) Do(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, request)
	response, ok := d.responses[request.URL]
	if !ok {
		return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected URL %s", request.URL)
	}
	return response, nil
}

func TestFetchClaudeAuthQuotaKeepsConfirmedUsageWhenProfileFails(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	usage := fmt.Sprintf(
		`{"five_hour":{"utilization":12.5,"resets_at":%q},"seven_day":{"utilization":34.5,"resets_at":%q},"seven_day_opus":{"utilization":40,"resets_at":%q}}`,
		now.Add(4*time.Hour).Format(time.RFC3339),
		now.Add(6*24*time.Hour).Format(time.RFC3339),
		now.Add(6*24*time.Hour).Format(time.RFC3339),
	)
	client := &quotaCallbackTestDoer{responses: map[string]pluginapi.HTTPResponse{
		claudeQuotaUsageURL:   {StatusCode: http.StatusOK, Body: []byte(usage)},
		claudeQuotaProfileURL: {StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":"unavailable"}`)},
	}}
	auth := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":      "secret-token",
			"email":             "member@example.com",
			"organization_name": "Workspace A",
		},
	}

	result, failure := fetchClaudeAuthQuota(context.Background(), client, auth)
	if failure != nil {
		t.Fatalf("fetchClaudeAuthQuota() failure = %#v", failure)
	}
	if len(result.windows) != 3 {
		t.Fatalf("windows = %#v, want session, weekly, and opus weekly", result.windows)
	}
	if result.accountLabel != "member@example.com" || result.workspaceLabel != "Workspace A" {
		t.Fatalf("fallback labels = %q / %q", result.accountLabel, result.workspaceLabel)
	}
	for _, window := range result.windows {
		if window.ResetAt.IsZero() ||
			window.ResetMode != pluginapi.HostAuthQuotaResetModeScheduled ||
			window.RemainingPercent < 0 ||
			window.RemainingPercent > 100 {
			t.Fatalf("invalid normalized window: %#v", window)
		}
	}
}

func TestParseClaudeAuthQuotaConfirmsUnusedSessionWithoutReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	usage := fmt.Sprintf(
		`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":39,"resets_at":%q}}`,
		now.Add(6*24*time.Hour).Format(time.RFC3339),
	)
	result, failure := parseClaudeAuthQuota([]byte(usage), nil, now, nil)
	if failure != nil {
		t.Fatalf("parseClaudeAuthQuota() failure = %#v", failure)
	}
	if len(result.windows) != 2 {
		t.Fatalf("windows = %#v, want session and weekly", result.windows)
	}
	session := result.windows[0]
	if session.Kind != pluginapi.HostAuthQuotaWindowKindSession ||
		session.ResetMode != pluginapi.HostAuthQuotaResetModeInactive ||
		!session.ResetAt.IsZero() ||
		session.UsedPercent != 0 ||
		session.RemainingPercent != 100 {
		t.Fatalf("inactive session = %#v", session)
	}
}

func TestParseClaudeAuthQuotaRejectsUsedSessionWithoutReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	usage := fmt.Sprintf(
		`{"five_hour":{"utilization":1,"resets_at":null},"seven_day":{"utilization":39,"resets_at":%q}}`,
		now.Add(6*24*time.Hour).Format(time.RFC3339),
	)
	_, failure := parseClaudeAuthQuota([]byte(usage), nil, now, nil)
	if failure == nil || failure.code != "quota_response_invalid" {
		t.Fatalf("failure = %#v, want quota_response_invalid", failure)
	}
}

func TestParseClaudeAuthQuotaFailsClosedWithoutRequiredWindow(t *testing.T) {
	now := time.Now().UTC()
	usage := fmt.Sprintf(
		`{"five_hour":{"utilization":5,"resets_at":%q}}`,
		now.Add(time.Hour).Format(time.RFC3339),
	)
	_, failure := parseClaudeAuthQuota([]byte(usage), nil, now, nil)
	if failure == nil || failure.code != "quota_windows_missing" {
		t.Fatalf("failure = %#v, want quota_windows_missing", failure)
	}
}

func TestParseCodexAuthQuotaNormalizesSessionAndWeeklyWindows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	payload := fmt.Sprintf(
		`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":"20","limit_window_seconds":"18000","reset_at":"%d"},"secondary_window":{"used_percent":45,"limit_window_seconds":604800,"reset_after_seconds":500000}}}`,
		now.Add(4*time.Hour).Unix(),
	)
	result, failure := parseCodexAuthQuota([]byte(payload), now, &coreauth.Auth{
		Metadata: map[string]any{"email": "codex@example.com"},
	})
	if failure != nil {
		t.Fatalf("parseCodexAuthQuota() failure = %#v", failure)
	}
	if result.planLabel != "pro" || len(result.windows) != 2 {
		t.Fatalf("result = %#v", result)
	}
	kinds := map[string]bool{}
	for _, window := range result.windows {
		kinds[window.Kind] = true
	}
	if !kinds[pluginapi.HostAuthQuotaWindowKindSession] || !kinds[pluginapi.HostAuthQuotaWindowKindWeekly] {
		t.Fatalf("window kinds = %#v", kinds)
	}
}

func TestParseCodexAuthQuotaAcceptsWeeklyOnlyPlan(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	payload := fmt.Sprintf(
		`{"plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":37,"limit_window_seconds":604800,"reset_at":%d},"secondary_window":null}}`,
		now.Add(6*24*time.Hour).Unix(),
	)
	result, failure := parseCodexAuthQuota([]byte(payload), now, nil)
	if failure != nil {
		t.Fatalf("parseCodexAuthQuota() failure = %#v", failure)
	}
	if len(result.windows) != 2 {
		t.Fatalf("windows = %#v, want weekly and not-applicable session", result.windows)
	}
	var session, weekly *pluginapi.HostAuthQuotaWindow
	for index := range result.windows {
		switch result.windows[index].Kind {
		case pluginapi.HostAuthQuotaWindowKindSession:
			session = &result.windows[index]
		case pluginapi.HostAuthQuotaWindowKindWeekly:
			weekly = &result.windows[index]
		}
	}
	if weekly == nil || weekly.ResetMode != pluginapi.HostAuthQuotaResetModeScheduled || weekly.UsedPercent != 37 {
		t.Fatalf("weekly = %#v", weekly)
	}
	if session == nil ||
		session.ResetMode != pluginapi.HostAuthQuotaResetModeNotApplicable ||
		!session.ResetAt.IsZero() ||
		session.RemainingPercent != 100 {
		t.Fatalf("session = %#v", session)
	}
}

func TestUnknownHostAuthQuotaResponseNeverIncludesWindows(t *testing.T) {
	response := unknownHostAuthQuotaResponse(pluginapi.HostAuthQuotaResponse{
		AuthIndex:  "deadbeefdeadbeef",
		Confidence: pluginapi.HostAuthQuotaConfidenceConfirmed,
		Windows: []pluginapi.HostAuthQuotaWindow{{
			ID: "should-be-removed",
		}},
	}, authQuotaFailure{code: "quota_unavailable", message: "live quota request failed"})
	if response.Confidence != pluginapi.HostAuthQuotaConfidenceUnknown ||
		len(response.Windows) != 0 ||
		response.Error == nil ||
		response.Error.Code != "quota_unavailable" {
		t.Fatalf("unknown response = %#v", response)
	}
}

func TestQuota429PreservesRetryAfterMetadata(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	client := &quotaCallbackTestDoer{responses: map[string]pluginapi.HTTPResponse{
		claudeQuotaUsageURL: {
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"120"}},
			Body:       []byte(`{"type":"error"}`),
		},
	}}
	_, failure := fetchAuthQuotaEndpointAt(context.Background(), client, claudeQuotaUsageURL, nil, now)
	if failure == nil || failure.code != "rate_limited" || failure.statusCode != http.StatusTooManyRequests ||
		!failure.retryable || failure.retryAfter != "120" || !failure.retryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("429 failure = %#v", failure)
	}
}

func TestQuotaRetryAfterHTTPDateIsParsed(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	retryAt := now.Add(3 * time.Minute)
	client := &quotaCallbackTestDoer{responses: map[string]pluginapi.HTTPResponse{
		codexQuotaUsageURL: {
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{retryAt.Format(http.TimeFormat)}},
		},
	}}
	_, failure := fetchAuthQuotaEndpointAt(context.Background(), client, codexQuotaUsageURL, nil, now)
	if failure == nil || !failure.retryAt.Equal(retryAt) {
		t.Fatalf("HTTP-date Retry-After failure = %#v, want %v", failure, retryAt)
	}
}

func TestClaudeTeamWorkspaceOverridesPersonalProFlag(t *testing.T) {
	hasPro := true
	profile := claudeQuotaProfilePayload{
		Account: &claudeQuotaProfileAccount{HasClaudePro: &hasPro},
		Organization: &claudeQuotaProfileOrganization{
			OrganizationType: "claude_team",
		},
	}
	if got := claudeQuotaPlanLabel(profile, nil); got != "team" {
		t.Fatalf("claudeQuotaPlanLabel() = %q, want team", got)
	}
}
