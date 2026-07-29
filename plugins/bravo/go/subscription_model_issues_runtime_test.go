package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSubscriptionViewIncludesScopedRuntimeCreditsRestriction(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	retryAt := now.Add(time.Hour)
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir",
		AuthIndex: "palantir-index",
		Provider:  "claude",
	}
	setCooldownWithProviderError(
		"claude",
		auth.ID,
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		retryAt,
		runtimeCreditsProviderDetail("claude-fable-5"),
	)
	if entries := activeProviderModelCooldowns("claude", auth.ID, now); len(entries) != 1 {
		t.Fatalf("active runtime cooldowns = %#v, want one Fable restriction", entries)
	}

	view := runtimeIssueSubscriptionView(auth)
	if view.Health != string(bravoAuthReady) {
		t.Fatalf("subscription health = %q, want ready with only one model cooling", view.Health)
	}
	if len(view.ModelIssues) != 1 {
		t.Fatalf("model issues = %#v, want one runtime Fable restriction", view.ModelIssues)
	}
	issue := view.ModelIssues[0]
	if issue.Model != "claude-fable-5" ||
		issue.ProviderModel != "claude-fable-5" ||
		issue.ProviderErrorCode != "credits_required" ||
		issue.Scope != "model" {
		t.Fatalf("runtime model issue = %#v", issue)
	}
	if issue.RetryAt == nil || !issue.RetryAt.Equal(retryAt) {
		t.Fatalf("runtime retry_at = %v, want %v", issue.RetryAt, retryAt)
	}

	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-fable-5", now); got != bravoAuthCooldown {
		t.Fatalf("Fable health = %q, want cooldown", got)
	}
	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-sonnet-5", now); got != bravoAuthReady {
		t.Fatalf("sibling Sonnet health = %q, want ready", got)
	}
}

func TestSubscriptionRuntimeRestrictionUsesExactAuthAndProviderScope(t *testing.T) {
	isolateBravoCooldowns(t)
	setCooldownWithProviderError(
		"claude",
		"palantir",
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		time.Now().Add(time.Hour),
		runtimeCreditsProviderDetail("claude-fable-5"),
	)

	tests := []struct {
		name string
		auth pluginapi.HostAuthFileEntry
		want int
	}{
		{
			name: "exact auth and provider",
			auth: pluginapi.HostAuthFileEntry{ID: "palantir", AuthIndex: "exact", Provider: "claude"},
			want: 1,
		},
		{
			name: "provider alias normalizes",
			auth: pluginapi.HostAuthFileEntry{ID: "palantir", AuthIndex: "alias", Provider: "anthropic"},
			want: 1,
		},
		{
			name: "auth id prefix is not the same credential",
			auth: pluginapi.HostAuthFileEntry{ID: "palantir-copy", AuthIndex: "other-auth", Provider: "claude"},
		},
		{
			name: "same auth id under another provider is isolated",
			auth: pluginapi.HostAuthFileEntry{ID: "palantir", AuthIndex: "other-provider", Provider: "codex"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := runtimeIssueSubscriptionView(test.auth)
			if got := len(view.ModelIssues); got != test.want {
				t.Fatalf("model issues = %#v, want %d", view.ModelIssues, test.want)
			}
		})
	}
}

func TestSubscriptionRuntimeRestrictionExpiresAndIsRemoved(t *testing.T) {
	isolateBravoCooldowns(t)
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir",
		AuthIndex: "palantir-index",
		Provider:  "claude",
	}
	key := cooldownKey("claude", auth.ID, "claude-fable-5")
	runtimeState.Lock()
	runtimeState.Cooldowns[key] = cooldownEntry{
		Until:  time.Now().Add(-time.Minute),
		Reason: "bravo_subscription_model_credits_exhausted",
	}
	runtimeState.Unlock()

	if issues := runtimeIssueSubscriptionView(auth).ModelIssues; len(issues) != 0 {
		t.Fatalf("expired runtime model issues = %#v, want none", issues)
	}
	runtimeState.RLock()
	_, retained := runtimeState.Cooldowns[key]
	runtimeState.RUnlock()
	if retained {
		t.Fatal("expired runtime model restriction remained in the cooldown map")
	}
}

func TestSubscriptionModelIssueMergeDeduplicatesHostAndRuntimeState(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	hostRetryAt := now.Add(time.Hour)
	runtimeRetryAt := now.Add(2 * time.Hour)
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir",
		AuthIndex: "palantir-index",
		Provider:  "claude",
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-fable-5": {
				Status:                   "error",
				ErrorCode:                "credits_required",
				ErrorMessage:             "Usage credits are required for this model.",
				ProviderModel:            "claude-fable-5",
				ProviderModelDisplayName: "Fable 5",
				ProviderNoticeTitle:      "You've hit your monthly spend limit",
				ProviderNoticeText:       "Ask your admin to raise your spend limit, or switch models to continue this chat.",
				ProviderDisabledReason:   "org_level_disabled_until",
				Scope:                    "model",
				Reason:                   "monthly_spend_limit",
				Unavailable:              true,
				NextRetryAfter:           hostRetryAt,
				UpdatedAt:                now,
			},
		},
	}
	setCooldownWithProviderError(
		"claude",
		auth.ID,
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		runtimeRetryAt,
		&providererror.Detail{
			Type:  "rate_limit_error",
			Code:  "credits_required",
			Model: "claude-fable-5",
			Scope: "model",
		},
	)

	view := runtimeIssueSubscriptionView(auth)
	if len(view.ModelIssues) != 1 {
		t.Fatalf("merged model issues = %#v, want one deduplicated issue", view.ModelIssues)
	}
	issue := view.ModelIssues[0]
	if issue.ProviderModelDisplayName != "Fable 5" ||
		issue.ProviderNoticeTitle != "You've hit your monthly spend limit" ||
		issue.ProviderErrorReason != "monthly_spend_limit" {
		t.Fatalf("runtime merge discarded richer host detail: %#v", issue)
	}
	if issue.RetryAt == nil || !issue.RetryAt.Equal(runtimeRetryAt) {
		t.Fatalf("merged retry_at = %v, want later active barrier %v", issue.RetryAt, runtimeRetryAt)
	}
}

func TestSubscriptionRuntimeRestrictionNeverExposesRawReason(t *testing.T) {
	isolateBravoCooldowns(t)
	const privateReason = `{"type":"error","request_id":"req_private","password":"hunter2"}`
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir",
		AuthIndex: "palantir-index",
		Provider:  "claude",
	}
	runtimeState.Lock()
	runtimeState.Cooldowns[cooldownKey("claude", auth.ID, "claude-secret-model")] = cooldownEntry{
		Until:      time.Now().Add(time.Hour),
		ObservedAt: time.Now(),
		Reason:     privateReason,
		Provider:   "claude",
		AuthID:     auth.ID,
		Model:      "claude-secret-model",
		ProviderError: providererror.Detail{
			Type:        "rate_limit_error",
			Code:        "credits_required",
			Message:     privateReason,
			Model:       "claude-secret-model",
			NoticeTitle: privateReason,
			Scope:       "model",
			Reason:      privateReason,
		},
	}
	runtimeState.Unlock()

	raw, errMarshal := json.Marshal(runtimeIssueSubscriptionView(auth))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, forbidden := range []string{"req_private", "hunter2", privateReason, `\"type\":\"error\"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("runtime restriction leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSubscriptionAccountWideRuntimeCooldownIsNotAModelIssue(t *testing.T) {
	isolateBravoCooldowns(t)
	auth := pluginapi.HostAuthFileEntry{
		ID:        "palantir",
		AuthIndex: "palantir-index",
		Provider:  "claude",
	}
	setCooldown(
		"claude",
		auth.ID,
		"",
		"bravo_subscription_access_denied",
		time.Now().Add(time.Hour),
	)

	view := runtimeIssueSubscriptionView(auth)
	if view.Health != string(bravoAuthCooldown) {
		t.Fatalf("account-wide health = %q, want cooldown", view.Health)
	}
	if len(view.ModelIssues) != 0 {
		t.Fatalf("account-wide cooldown became model issues: %#v", view.ModelIssues)
	}
}

func runtimeIssueSubscriptionView(auth pluginapi.HostAuthFileEntry) subscriptionView {
	return buildSubscriptionView(
		defaultPluginConfig(),
		auth,
		subscriptionConfig{AuthIndex: auth.AuthIndex, Tariff: "x1"},
		tariffConfig{ID: "x1"},
		credentialQuotaState{Confidence: "unknown"},
		nil,
	)
}

func runtimeCreditsProviderDetail(model string) *providererror.Detail {
	return &providererror.Detail{
		Type:             "rate_limit_error",
		Code:             "credits_required",
		Message:          "Usage credits are required for this model.",
		Model:            model,
		ModelDisplayName: "Fable 5",
		NoticeTitle:      "You've hit your monthly spend limit",
		NoticeText:       "Ask your admin to raise your spend limit, or switch models to continue this chat.",
		DisabledReason:   "org_level_disabled_until",
		Scope:            "model",
		Reason:           "monthly_spend_limit",
	}
}
