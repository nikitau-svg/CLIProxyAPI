package main

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
)

func TestTaxonomyV1ClassificationIsClassFirstAndScopeAuthoritative(t *testing.T) {
	tests := []struct {
		name       string
		detail     providererror.Detail
		wantCode   string
		wantStatus int
		wantScope  string
		wantClass  string
		wantRoute  bool
		wantWide   bool
	}{
		{"quota_account_ignores_context_text", providererror.Detail{Type: "invalid_request_error", Code: "context_window_exceeded", Message: "prompt is too long", TaxonomyVersion: 1, Class: providererror.ClassQuota, Scope: providererror.ScopeAccount}, "bravo_subscription_quota_exhausted", 429, providererror.ScopeAccount, providererror.ClassQuota, true, true},
		{"context_request_ignores_rate_credits", providererror.Detail{Type: "rate_limit_error", Code: "credits_required", Message: "quota", TaxonomyVersion: 1, Class: providererror.ClassContextWindow, Scope: providererror.ScopeRequest, RequiredTokens: 200, LimitTokens: 100}, "bravo_context_window_exceeded", 400, providererror.ScopeRequest, providererror.ClassContextWindow, true, false},
		{"precise_invalid_ignores_quota_text", providererror.Detail{Type: "rate_limit_error", Code: "context_window_exceeded", Parameter: "max_tokens", Message: "quota exhausted", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "bravo_provider_invalid_request", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, false, false},
		{"auth_account_ignores_context_text", providererror.Detail{Type: "invalid_request_error", Code: "context_window_exceeded", Message: "prompt too long", TaxonomyVersion: 1, Class: providererror.ClassAuthentication, Scope: providererror.ScopeAccount}, "bravo_provider_authentication_failed", 401, providererror.ScopeAccount, providererror.ClassAuthentication, true, true},
		{"exact_model_credits", providererror.Detail{Type: "anything", Code: "credits_required", TaxonomyVersion: 1, Class: providererror.ClassQuota, Scope: providererror.ScopeModel}, "bravo_subscription_model_credits_exhausted", 429, providererror.ScopeModel, providererror.ClassQuota, true, false},
		{"generic_invalid_empty_code_fallback", providererror.Detail{Type: "contradictory_type", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "bravo_provider_ambiguous_invalid_request", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, true, false},
		{"generic_invalid_request_code_fallback", providererror.Detail{Type: "rate_limit_error", Code: "invalid_request_error", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "bravo_provider_ambiguous_invalid_request", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, true, false},
		{"stable_opaque_code_terminal", providererror.Detail{Type: "invalid_request_error", Code: "opaque_stable_code", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "bravo_provider_invalid_request", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, false, false},
		{"precise_message_without_parameter_terminal", providererror.Detail{Type: "anything", Code: "invalid_request_error", Message: "max_tokens must not exceed the model limit", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "invalid_request_error", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, false, false},
		{"reviewed_reason_without_parameter_terminal", providererror.Detail{Type: "anything", Code: "invalid_request_error", Reason: "invalid_json_schema", TaxonomyVersion: 1, Class: providererror.ClassInvalidRequest, Scope: providererror.ScopeRequest}, "invalid_request_error", 400, providererror.ScopeRequest, providererror.ClassInvalidRequest, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProviderFailureDetail(executionFailure{Status: http.StatusTeapot}, tc.detail)
			if got.Code != tc.wantCode || got.Status != tc.wantStatus || got.RouteFallback != tc.wantRoute || got.AccountWide != tc.wantWide || got.Provider == nil || got.Provider.Scope != tc.wantScope || got.Provider.Class != tc.wantClass {
				t.Fatalf("classification=%#v provider=%#v", got, got.Provider)
			}
		})
	}
}

func TestInvalidTaxonomyV1FallsBackToLegacyInference(t *testing.T) {
	detail := providererror.Detail{
		Type: "invalid_request_error", Code: "context_window_exceeded", Message: "prompt is too long",
		TaxonomyVersion: 1, Class: "invented", Scope: providererror.ScopeAccount,
	}
	got := classifyProviderFailureDetail(executionFailure{Status: http.StatusBadRequest}, detail)
	if got.Code != "bravo_context_window_exceeded" || got.Provider == nil || got.Provider.TaxonomyVersion != 0 {
		t.Fatalf("invalid taxonomy did not fall back to legacy: %#v", got)
	}
}
