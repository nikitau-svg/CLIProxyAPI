package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
)

const corePalantirCreditsPayload = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","notice":{"title":"You've hit your monthly spend limit","text":"Ask your admin to raise your spend limit, or switch models to continue this chat.","cta":{"copy":"Switch models","intent":"switch_model","redirect_hint":null},"is_dismissible":true},"model_display_name":"Fable 5","can_user_purchase_credits":false,"model":"claude-fable-5","has_chargeable_saved_payment_method":true,"disabled_reason":"org_level_disabled_until","exhausted_included_allowance":false}},"request_id":"req_core_state_private"}`

type rawPalantirCoreError struct{}

func (rawPalantirCoreError) Error() string     { return corePalantirCreditsPayload }
func (rawPalantirCoreError) ErrorCode() string { return "credits_required" }
func (rawPalantirCoreError) StatusCode() int   { return http.StatusTooManyRequests }
func (rawPalantirCoreError) Retryable() bool   { return true }
func (rawPalantirCoreError) ProviderErrorDetail() (providererror.Detail, bool) {
	return providererror.Parse(corePalantirCreditsPayload)
}

type typedCoreProviderError struct {
	detail providererror.Detail
	status int
}

func (e typedCoreProviderError) Error() string     { return e.detail.Message }
func (e typedCoreProviderError) ErrorCode() string { return e.detail.Code }
func (e typedCoreProviderError) StatusCode() int   { return e.status }
func (e typedCoreProviderError) Retryable() bool {
	return e.detail.Scope == providererror.ScopeModel
}
func (e typedCoreProviderError) ProviderErrorDetail() (providererror.Detail, bool) {
	return e.detail, true
}

func TestManagerFailureScopeControlsHealthMutation(t *testing.T) {
	tests := []struct {
		name                string
		detail              *providererror.Detail
		wantAuthUnavailable bool
		wantModelState      bool
		wantLastError       bool
	}{
		{
			name: "request context overflow is health neutral",
			detail: &providererror.Detail{
				Type:            "invalid_request_error",
				Code:            "context_window_exceeded",
				Message:         "Input requires 1003466 tokens and exceeds the model context limit of 1000000 tokens.",
				Scope:           providererror.ScopeRequest,
				Reason:          "prompt_too_long",
				TaxonomyVersion: providererror.FailureTaxonomyV1,
				Class:           providererror.ClassContextWindow,
				RequiredTokens:  1003466,
				LimitTokens:     1000000,
			},
		},
		{
			name: "model failure changes only model state",
			detail: &providererror.Detail{
				Type:            "rate_limit_error",
				Code:            "rate_limit_error",
				Message:         "The provider rate limit was reached.",
				Scope:           providererror.ScopeModel,
				TaxonomyVersion: providererror.FailureTaxonomyV1,
				Class:           providererror.ClassRateLimit,
			},
			wantModelState: true,
			wantLastError:  true,
		},
		{
			name: "account failure is not narrowed by result model",
			detail: &providererror.Detail{
				Type:            "authentication_error",
				Code:            "authentication_error",
				Message:         "The provider rejected the subscription credentials.",
				Scope:           providererror.ScopeAccount,
				TaxonomyVersion: providererror.FailureTaxonomyV1,
				Class:           providererror.ClassAuthentication,
			},
			wantAuthUnavailable: true,
			wantLastError:       true,
		},
		{
			name: "unknown typed scope is terminal and health neutral",
			detail: &providererror.Detail{
				Type:    "future_provider_error",
				Code:    "future_provider_error",
				Message: "The provider returned an unclassified failure.",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			authID := "scope-test-" + strings.ReplaceAll(test.name, " ", "-")
			model := "claude-opus-5"
			auth := &Auth{
				ID:       authID,
				Provider: "claude",
				Status:   StatusActive,
				ModelStates: map[string]*ModelState{
					"claude-sonnet-5": {Status: StatusActive},
				},
			}
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			status := http.StatusBadRequest
			if test.detail.Scope == providererror.ScopeModel {
				status = http.StatusTooManyRequests
			}
			if test.detail.Scope == providererror.ScopeAccount {
				status = http.StatusUnauthorized
			}
			resultErr := resultErrorFromError(typedCoreProviderError{detail: *test.detail, status: status})

			manager.MarkResult(WithSkipPersist(context.Background()), Result{
				AuthID:   authID,
				Provider: "claude",
				Model:    model,
				Success:  false,
				Error:    resultErr,
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("updated auth was not found")
			}
			if updated.Unavailable != test.wantAuthUnavailable {
				t.Errorf("Auth.Unavailable = %t, want %t", updated.Unavailable, test.wantAuthUnavailable)
			}
			_, hasModelState := updated.ModelStates[model]
			if hasModelState != test.wantModelState {
				t.Errorf("model state present = %t, want %t: %#v", hasModelState, test.wantModelState, updated.ModelStates)
			}
			if (updated.LastError != nil) != test.wantLastError {
				t.Errorf("LastError = %#v, want present=%t", updated.LastError, test.wantLastError)
			}
		})
	}
}

func TestResultErrorFromErrorSanitizesPalantirProviderDetail(t *testing.T) {
	resultErr := resultErrorFromError(rawPalantirCoreError{})
	assertSafeCoreProviderError(t, "result error", resultErr)
}

func TestManagerPersistsOnlySafePalantirProviderDetail(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{ID: "palantir-auth", Provider: "claude", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-fable-5",
		Success:  false,
		Error:    resultErrorFromError(rawPalantirCoreError{}),
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth was not found")
	}
	assertSafeCoreProviderError(t, "auth last error", updated.LastError)
	state := updated.ModelStates["claude-fable-5"]
	if state == nil {
		t.Fatal("claude-fable-5 model state was not recorded")
	}
	assertSafeCoreProviderError(t, "model last error", state.LastError)

	store.mu.Lock()
	records := cloneCooldownStateRecords(store.records)
	store.mu.Unlock()
	if len(records) == 0 {
		t.Fatal("persisted cooldown records are empty")
	}
	foundModelRecord := false
	for index := range records {
		record := records[index]
		if record.Provider != "claude" || record.AuthID != auth.ID {
			t.Fatalf("persisted cooldown scope = %#v, want exact Claude auth", record)
		}
		if record.Model == "claude-fable-5" {
			foundModelRecord = true
		}
		assertSafeCoreProviderError(t, "persisted cooldown last error", record.LastError)
		serialized, errMarshal := json.Marshal(record)
		if errMarshal != nil {
			t.Fatalf("marshal persisted cooldown: %v", errMarshal)
		}
		assertNoPrivatePalantirFields(t, "persisted cooldown", serialized)
	}
	if !foundModelRecord {
		t.Fatalf("persisted cooldown records = %#v, want exact claude-fable-5 model record", records)
	}
}

func assertSafeCoreProviderError(t *testing.T, label string, value *Error) {
	t.Helper()
	if value == nil {
		t.Fatalf("%s = nil", label)
	}
	serialized, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal %s: %v", label, errMarshal)
	}
	assertNoPrivatePalantirFields(t, label, serialized)

	want, ok := providererror.Parse(corePalantirCreditsPayload)
	if !ok {
		t.Fatal("test fixture is not a reviewed provider error")
	}
	if value.Message != want.Summary() {
		t.Errorf("%s message = %q, want safe summary %q", label, value.Message, want.Summary())
	}
	if value.Code != want.Code {
		t.Errorf("%s code = %q, want %q", label, value.Code, want.Code)
	}
}

func assertNoPrivatePalantirFields(t *testing.T, label string, serialized []byte) {
	t.Helper()
	lower := strings.ToLower(string(serialized))
	for _, forbidden := range []string{
		"req_core_state_private",
		"request_id",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		"exhausted_included_allowance",
		`"cta"`,
		"switch_model",
		"redirect_hint",
		`\"type\":\"error\"`,
	} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Errorf("%s leaks forbidden provider field %q: %s", label, forbidden, serialized)
		}
	}
}
