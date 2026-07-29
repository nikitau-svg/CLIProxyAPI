package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const anthropicCreditsRequiredPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_bravo_credits_private"}`

func TestCreditsRequiredPayloadClassifiesAsModelCreditsExhausted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failure executionFailure
	}{
		{
			name: "host callback",
			failure: classifyExecutionError(&hostCallError{
				Code:       "model_execution_failed",
				Message:    anthropicCreditsRequiredPayload,
				HTTPStatus: http.StatusTooManyRequests,
			}),
		},
		{
			name: "HTTP response body",
			failure: classifyHTTPFailure(
				http.StatusTooManyRequests,
				nil,
				"candidate returned an HTTP error",
				[]byte(anthropicCreditsRequiredPayload),
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.failure.Code != "bravo_subscription_model_credits_exhausted" {
				t.Errorf("code = %q, want bravo_subscription_model_credits_exhausted", test.failure.Code)
			}
			if !test.failure.Retryable {
				t.Error("model credits exhaustion must continue to another eligible credential or provider")
			}
			if test.failure.AccountWide {
				t.Error("a provider error naming claude-fable-5 must not disable sibling models")
			}
		})
	}
}

func TestCreditsRequiredCooldownIsModelScoped(t *testing.T) {
	isolateBravoCooldowns(t)

	auth := pluginapi.HostAuthFileEntry{
		ID:       "palantir-subscription",
		Provider: "claude",
	}
	fable := candidate{Provider: "claude", Model: "claude-fable-5"}
	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    anthropicCreditsRequiredPayload,
		HTTPStatus: http.StatusTooManyRequests,
	})
	applyFailureCooldown(executionAttempt{Candidate: fable, Auth: auth}, failure)

	now := time.Now()
	if eligible := eligibleAuths(fable, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
		t.Fatal("the exhausted Fable 5 route remained eligible on the affected subscription")
	}

	sibling := candidate{Provider: "claude", Model: "claude-sonnet-5"}
	if eligible := eligibleAuths(sibling, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 1 {
		t.Fatal("the Fable 5 credits failure disabled a sibling Claude model")
	}
}

func TestCreditsRequiredCooldownProbeIntervalPolicy(t *testing.T) {
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider:     "claude",
		Model:        "claude-fable-5",
		Capabilities: []string{capabilityText},
	}}})

	now := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	t.Run("exact credits without hint uses minimum probe interval", func(t *testing.T) {
		got := failureCooldownUntil(executionFailure{
			Code:      "bravo_subscription_model_credits_exhausted",
			Retryable: true,
		}, now)
		minimum := now.Add(creditsRequiredMinimumProbeInterval)
		if got.Before(minimum) {
			t.Fatalf("credits cooldown = %v, want at least %v", got, minimum)
		}
	})

	t.Run("generic retryable failure keeps configured cooldown", func(t *testing.T) {
		got := failureCooldownUntil(executionFailure{
			Code:      "bravo_candidate_http_error",
			Status:    http.StatusTooManyRequests,
			Retryable: true,
		}, now)
		want := now.Add(time.Duration(loadedConfig().CooldownSeconds) * time.Second)
		if !got.Equal(want) {
			t.Fatalf("generic cooldown = %v, want configured deadline %v", got, want)
		}
	})

	t.Run("explicit shorter retry after remains authoritative", func(t *testing.T) {
		got := failureCooldownUntil(executionFailure{
			Code:       "bravo_subscription_model_credits_exhausted",
			Retryable:  true,
			RetryAfter: "7",
		}, now)
		want := now.Add(7 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("explicit cooldown = %v, want %v", got, want)
		}
	})
}

func TestCreditsRequiredScopeOverridesForbiddenHTTPEnvelope(t *testing.T) {
	isolateBravoCooldowns(t)

	auth := pluginapi.HostAuthFileEntry{
		ID:       "palantir-subscription",
		Provider: "claude",
	}
	fable := candidate{Provider: "claude", Model: "claude-fable-5"}
	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    anthropicCreditsRequiredPayload,
		HTTPStatus: http.StatusForbidden,
	})
	applyFailureCooldown(executionAttempt{Candidate: fable, Auth: auth}, failure)

	now := time.Now()
	if !cooldownActive("claude", auth.ID, fable.Model, now) {
		t.Fatal("explicit model credits restriction did not cool the named model")
	}
	if cooldownActive("claude", auth.ID, "", now) {
		t.Fatal("HTTP 403 overrode the provider's explicit model scope")
	}
}

func TestCreditsRequiredAttemptExposesOnlySafeProviderDetails(t *testing.T) {
	isolateBravoFallbackTestState(t)

	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    anthropicCreditsRequiredPayload,
		HTTPStatus: http.StatusTooManyRequests,
	})
	recordExecutionAttempt(executionAttempt{
		LogicalModel: "sonnet",
		Candidate: candidate{
			Provider: "claude",
			Model:    "claude-fable-5",
		},
		Auth: pluginapi.HostAuthFileEntry{
			ID:       "palantir-subscription",
			Provider: "claude",
			Note:     "Palantir",
		},
	}, time.Now(), failure.Status, false, failure)

	runtimeState.RLock()
	records := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(records) != 1 {
		t.Fatalf("attempt records = %d, want 1", len(records))
	}

	record := marshalJSONObject(t, records[0])
	assertSafeCreditsFields(t, record)

	serialized, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		t.Fatalf("marshal attempt: %v", errMarshal)
	}
	if strings.Contains(string(serialized), "req_bravo_credits_private") {
		t.Errorf("attempt leaked the provider request id: %s", serialized)
	}
	if rawError, _ := record["error"].(string); strings.HasPrefix(strings.TrimSpace(rawError), "{") {
		t.Errorf("attempt exposed raw provider JSON instead of a safe summary: %s", rawError)
	}
}

func TestSubscriptionViewExposesModelCreditsIssue(t *testing.T) {
	view := buildSubscriptionView(
		defaultPluginConfig(),
		pluginapi.HostAuthFileEntry{
			ID:        "palantir-subscription",
			AuthIndex: "palantir-auth-index",
			Provider:  "claude",
			Note:      "Palantir",
			ModelStates: map[string]pluginapi.HostAuthModelState{
				"claude-fable-5": {
					Status:         "error",
					StatusMessage:  anthropicCreditsRequiredPayload,
					Unavailable:    true,
					QuotaExceeded:  true,
					NextRetryAfter: time.Now().Add(time.Hour),
				},
			},
		},
		subscriptionConfig{AuthIndex: "palantir-auth-index", Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)

	payload := marshalJSONObject(t, view)
	rawIssues, ok := payload["model_issues"].([]any)
	if !ok || len(rawIssues) != 1 {
		t.Fatalf("model_issues = %#v, want one redacted Fable 5 issue", payload["model_issues"])
	}
	issue, ok := rawIssues[0].(map[string]any)
	if !ok {
		t.Fatalf("model issue = %#v, want object", rawIssues[0])
	}
	assertSafeCreditsFields(t, issue)

	serialized, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal subscription view: %v", errMarshal)
	}
	if strings.Contains(string(serialized), "req_bravo_credits_private") {
		t.Errorf("subscription view leaked the provider request id: %s", serialized)
	}
}

func TestSubscriptionViewKeepsSiblingModelsAndAccountReady(t *testing.T) {
	now := time.Now()
	view := buildSubscriptionView(
		defaultPluginConfig(),
		pluginapi.HostAuthFileEntry{
			ID:             "palantir-subscription",
			AuthIndex:      "palantir-auth-index",
			Provider:       "claude",
			Status:         "error",
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Hour),
			ModelStates: map[string]pluginapi.HostAuthModelState{
				"claude-fable-5": {
					Status:                   "error",
					StatusMessage:            "Fable 5: monthly spend limit reached",
					ErrorCode:                "credits_required",
					ProviderModel:            "claude-fable-5",
					ProviderModelDisplayName: "Fable 5",
					Scope:                    "model",
					Unavailable:              true,
					NextRetryAfter:           now.Add(time.Hour),
				},
			},
		},
		subscriptionConfig{AuthIndex: "palantir-auth-index", Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)

	if view.Health != string(bravoAuthReady) {
		t.Fatalf("subscription health = %q, want ready with one model issue", view.Health)
	}
	if len(view.ModelIssues) != 1 {
		t.Fatalf("model issues = %#v, want one active Fable restriction", view.ModelIssues)
	}
}

func TestSubscriptionViewDropsExpiredModelCreditsIssue(t *testing.T) {
	view := buildSubscriptionView(
		defaultPluginConfig(),
		pluginapi.HostAuthFileEntry{
			ID:        "palantir-subscription",
			AuthIndex: "palantir-auth-index",
			Provider:  "claude",
			ModelStates: map[string]pluginapi.HostAuthModelState{
				"claude-fable-5": {
					Status:                   "error",
					ErrorCode:                "credits_required",
					ProviderModel:            "claude-fable-5",
					ProviderModelDisplayName: "Fable 5",
					Scope:                    "model",
					Unavailable:              true,
					NextRetryAfter:           time.Now().Add(-time.Minute),
				},
			},
		},
		subscriptionConfig{AuthIndex: "palantir-auth-index", Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)

	if len(view.ModelIssues) != 0 {
		t.Fatalf("expired model issues = %#v, want none", view.ModelIssues)
	}
}

func TestSubscriptionViewRequiresLiveDeadlineForModelCreditsIssue(t *testing.T) {
	baseState := pluginapi.HostAuthModelState{
		Status:                   "error",
		ErrorCode:                "credits_required",
		ProviderModel:            "claude-fable-5",
		ProviderModelDisplayName: "Fable 5",
		Scope:                    "model",
	}
	for _, test := range []struct {
		name  string
		state pluginapi.HostAuthModelState
		want  int
	}{
		{
			name: "unavailable without retry deadline is usable",
			state: func() pluginapi.HostAuthModelState {
				state := baseState
				state.Unavailable = true
				return state
			}(),
		},
		{
			name: "quota without recovery deadline is not permanently active",
			state: func() pluginapi.HostAuthModelState {
				state := baseState
				state.QuotaExceeded = true
				return state
			}(),
		},
		{
			name: "future retry deadline is active",
			state: func() pluginapi.HostAuthModelState {
				state := baseState
				state.Unavailable = true
				state.NextRetryAfter = time.Now().Add(time.Hour)
				return state
			}(),
			want: 1,
		},
		{
			name: "future quota recovery is active",
			state: func() pluginapi.HostAuthModelState {
				state := baseState
				state.QuotaExceeded = true
				state.QuotaRecoverAt = time.Now().Add(time.Hour)
				return state
			}(),
			want: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := pluginapi.HostAuthFileEntry{
				ID:       "palantir-subscription",
				Provider: "claude",
				ModelStates: map[string]pluginapi.HostAuthModelState{
					"claude-fable-5": test.state,
				},
			}
			if got := len(subscriptionModelIssues(auth)); got != test.want {
				t.Fatalf("model issues = %d, want %d for state %#v", got, test.want, test.state)
			}
		})
	}
}

func TestSubscriptionViewKeepsCreditsIssueFromBravoCooldownAfterHostStateDisappears(t *testing.T) {
	isolateBravoFallbackTestState(t)

	started := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir-subscription",
		AuthIndex: "palantir-auth-index",
		Provider:  "claude",
		Note:      "Palantir",
	}
	attempt := executionAttempt{
		LogicalModel: "sonnet",
		Candidate: candidate{
			Provider: "claude",
			Model:    "claude-fable-5",
		},
		Auth: auth,
	}
	failure := classifyExecutionError(&hostCallError{
		Code:       "credits_required",
		Message:    anthropicCreditsRequiredPayload,
		HTTPStatus: http.StatusTooManyRequests,
		Retryable:  true,
		RetryAfter: "600",
	})
	applyFailureCooldown(attempt, failure)

	// The host has already refreshed/replaced its transient ModelStates snapshot.
	// Bravo's own 600-second route cooldown is still active and must remain the
	// management source of truth for the affected model.
	view := buildSubscriptionView(
		defaultPluginConfig(),
		auth,
		subscriptionConfig{AuthIndex: auth.AuthIndex, Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)
	if len(view.ModelIssues) != 1 {
		t.Fatalf("model issues = %#v, want one issue backed by Bravo cooldown", view.ModelIssues)
	}
	issue := view.ModelIssues[0]
	if issue.ProviderErrorCode != "credits_required" ||
		issue.Model != "claude-fable-5" ||
		issue.ProviderModelDisplayName != "Fable 5" ||
		issue.Scope != "model" {
		t.Fatalf("cooldown-backed model issue = %#v", issue)
	}
	if issue.RetryAt == nil || issue.RetryAt.Before(started.Add(9*time.Minute)) {
		t.Fatalf("cooldown-backed retry_at = %v, want the active 600-second deadline", issue.RetryAt)
	}
	serialized, errMarshal := json.Marshal(issue)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{
		"req_bravo_credits_private",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
	} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("cooldown-backed issue leaked %q: %s", forbidden, serialized)
		}
	}

	runtimeState.Lock()
	entry := runtimeState.Cooldowns[cooldownKey("claude", auth.ID, "claude-fable-5")]
	entry.Until = time.Now().Add(-time.Second)
	runtimeState.Cooldowns[cooldownKey("claude", auth.ID, "claude-fable-5")] = entry
	runtimeState.Unlock()
	view = buildSubscriptionView(
		defaultPluginConfig(),
		auth,
		subscriptionConfig{AuthIndex: auth.AuthIndex, Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)
	if len(view.ModelIssues) != 0 {
		t.Fatalf("expired Bravo cooldown kept model issues active: %#v", view.ModelIssues)
	}
}

func TestUnknownStructuredProviderFailureIsRedactedEverywhere(t *testing.T) {
	isolateBravoFallbackTestState(t)
	const unreviewed = `{"type":"error","error":{"type":"billing_error","message":"private diagnostic","details":{"payment_method":"pm_private"}},"request_id":"req_private"}`
	failure := classifyExecutionError(&hostCallError{
		Code:       "model_execution_failed",
		Message:    unreviewed,
		HTTPStatus: http.StatusBadGateway,
	})
	attempt := executionAttempt{
		LogicalModel: "sonnet",
		Candidate:    candidate{Provider: "claude", Model: "claude-sonnet-5"},
		Auth:         pluginapi.HostAuthFileEntry{ID: "palantir", Provider: "claude"},
	}
	recordExecutionAttempt(attempt, time.Now(), failure.Status, false, failure)

	runtimeState.RLock()
	record := runtimeState.Attempts[len(runtimeState.Attempts)-1]
	runtimeState.RUnlock()
	envelopeRaw := failureEnvelope(failure)
	serialized := string(mustJSONValue(t, record)) + string(envelopeRaw)
	for _, forbidden := range []string{"req_private", "pm_private", "private diagnostic", `{"type":"error"`} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("unknown structured provider failure leaked %q: %s", forbidden, serialized)
		}
	}
	if strings.TrimSpace(record.Error) == "" || strings.HasPrefix(strings.TrimSpace(record.Error), "{") {
		t.Fatalf("attempt error = %q, want a safe generic diagnostic", record.Error)
	}
}

func assertSafeCreditsFields(t *testing.T, object map[string]any) {
	t.Helper()

	want := map[string]string{
		"provider_error_code":         "credits_required",
		"provider_model":              "claude-fable-5",
		"provider_model_display_name": "Fable 5",
		"provider_notice_title":       "You've hit your monthly spend limit",
		"provider_notice_text":        "Ask your admin to raise your spend limit, or switch models to continue this chat.",
		"provider_disabled_reason":    "org_level_disabled_until",
		"scope":                       "model",
	}
	for field, expected := range want {
		if actual, _ := object[field].(string); actual != expected {
			t.Errorf("%s = %q, want %q", field, actual, expected)
		}
	}
}

func marshalJSONObject(t *testing.T, value any) map[string]any {
	t.Helper()

	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal object: %v", errMarshal)
	}
	var object map[string]any
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		t.Fatalf("unmarshal object: %v", errUnmarshal)
	}
	return object
}
