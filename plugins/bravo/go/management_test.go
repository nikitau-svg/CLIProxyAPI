package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDashboardHostFailureRendersSanitizedDegradedState(t *testing.T) {
	request, errMarshal := json.Marshal(rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource/plugins/bravo/dashboard",
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errHandle := handleManagement(request)
	if errHandle != nil {
		t.Fatal(errHandle)
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	page := string(response.Body)
	for _, expected := range []string{
		`data-state="degraded"`,
		"Данные о подписках временно недоступны.",
		`"status_code":"auth_status_unavailable"`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	csp := response.Headers.Get("Content-Security-Policy")
	for _, expected := range []string{"base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, expected) {
			t.Errorf("dashboard CSP does not contain %q", expected)
		}
	}
	if strings.Contains(page, "host callback") || strings.Contains(page, "code=1") {
		t.Fatal("public dashboard exposed a raw host callback error")
	}
}

func TestDashboardDisclosuresStartClosedAndDynamicDataIsEscaped(t *testing.T) {
	const attack = `</script><script>globalThis.dashboardPwned=true</script>`
	page := string(renderBravoDashboard(bravoStatus{
		Version:     pluginVersion,
		Enabled:     true,
		ModelCount:  1,
		GeneratedAt: time.Now().UTC(),
		Models: []bravoStatusModel{{
			ID:          "bravo/test",
			DisplayName: attack,
			Description: `<img src=x onerror=alert(1)>`,
			Candidates: []bravoStatusCandidate{{
				Provider: "claude",
				Model:    attack,
			}, {
				Provider: "codex",
				Model:    "fallback",
			}},
		}},
	}))

	if strings.Contains(page, attack) || strings.Contains(page, `<img src=x onerror=alert(1)>`) {
		t.Fatal("dashboard embedded model data as executable HTML")
	}
	if regexp.MustCompile(`<details[^>]*\sopen(?:\s|>)`).MatchString(page) {
		t.Fatal("dashboard contains a disclosure that is open by default")
	}
	for _, expected := range []string{
		`id="mappingSection"`,
		`class="fallbacks"`,
		"m.display_name||m.id",
		`aria-live="polite"`,
		`for="search"`,
		`id="clearSearch"`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	if strings.Contains(page, "%!") {
		t.Fatal("dashboard contains an fmt formatting error")
	}
}

func TestSupersededHedgeIsNeutralInRecentStatus(t *testing.T) {
	status := bravoStatus{}
	summarizeRecentAttempts(&status, []attemptRecord{
		{Success: true, Status: http.StatusOK},
		{Status: 499, ErrorCode: "bravo_attempt_superseded"},
		{Status: http.StatusBadGateway, ErrorCode: "upstream_failed"},
	})
	if status.RecentSuccess != 1 ||
		status.RecentSuperseded != 1 ||
		status.RecentFailure != 1 {
		t.Fatalf("recent attempt summary = %#v, want success/superseded/failure = 1/1/1", status)
	}

	page := string(renderBravoDashboard(status))
	if !strings.Contains(page, "data.recent_superseded") ||
		!strings.Contains(page, "переключено") {
		t.Fatal("dashboard does not render superseded attempts as a neutral bucket")
	}
}

func TestRedactedBravoConfigOmitsSmartKeyDigest(t *testing.T) {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	raw, errMarshal := json.Marshal(redactedBravoConfig(pluginConfig{
		Enabled: true,
		SmartKeys: []smartKeyConfig{{
			Name:   "private-project-name",
			SHA256: digest,
			Models: []string{"frontier"},
		}},
	}))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	text := string(raw)
	if strings.Contains(text, digest) || strings.Contains(text, `"sha256"`) {
		t.Fatal("redacted config contains a smart-key digest")
	}
	if !strings.Contains(text, "private-project-name") {
		t.Fatal("redacted config removed the non-secret project name")
	}
}

// classifyBravoAuthHealth is the summary and quota-refresher verdict, so it still
// reports every roll-up field verbatim. Routing eligibility deliberately differs:
// a flag the host only clears on a successful request must not stop that request.
// wantEligible records that split per case.
func TestClassifyBravoAuthHealthMatchesRouterEligibility(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	item := candidate{Provider: "claude", Model: "claude-test"}
	cases := []struct {
		name string
		auth pluginapi.HostAuthFileEntry
		want bravoAuthHealth
		// wantEligible is set only where routing diverges from want == ready.
		wantEligible bool
	}{
		{
			name:         "ready",
			auth:         pluginapi.HostAuthFileEntry{ID: "ready", Provider: "claude"},
			want:         bravoAuthReady,
			wantEligible: true,
		},
		{
			name: "disabled flag",
			auth: pluginapi.HostAuthFileEntry{ID: "disabled", Provider: "claude", Disabled: true},
			want: bravoAuthDisabled,
		},
		{
			name: "disabled status",
			auth: pluginapi.HostAuthFileEntry{ID: "disabled-status", Provider: "claude", Status: "disabled"},
			want: bravoAuthDisabled,
		},
		{
			// Deadline-free: the host keeps routing here and clears the flag on the
			// next success, so refusing it would strand the credential.
			name:         "unavailable",
			auth:         pluginapi.HostAuthFileEntry{ID: "unavailable", Provider: "claude", Unavailable: true},
			want:         bravoAuthUnavailable,
			wantEligible: true,
		},
		{
			name:         "error status",
			auth:         pluginapi.HostAuthFileEntry{ID: "error", Provider: "claude", Status: "error"},
			want:         bravoAuthError,
			wantEligible: true,
		},
		{
			// The same flags with a deadline the host set are honoured.
			name: "unavailable with deadline",
			auth: pluginapi.HostAuthFileEntry{
				ID: "unavailable-deadline", Provider: "claude",
				Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			},
			want: bravoAuthUnavailable,
		},
		{
			name: "provider retry",
			auth: pluginapi.HostAuthFileEntry{ID: "retry", Provider: "claude", NextRetryAfter: now.Add(time.Minute)},
			want: bravoAuthCooldown,
		},
		{
			name: "missing stable id",
			auth: pluginapi.HostAuthFileEntry{Name: "file-without-id", Provider: "claude"},
			want: bravoAuthUnavailable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := classifyBravoAuthHealth("claude", testCase.auth, now)
			if got != testCase.want {
				t.Fatalf("classifyBravoAuthHealth() = %q, want %q", got, testCase.want)
			}
			eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{testCase.auth}, now)
			if gotEligible := len(eligible) == 1; gotEligible != testCase.wantEligible {
				t.Fatalf("eligibleAuths() eligible = %v, want %v for state %q", gotEligible, testCase.wantEligible, testCase.want)
			}
		})
	}

	cooldownAuth := pluginapi.HostAuthFileEntry{ID: "plugin-cooldown", Provider: "claude"}
	setCooldown("claude", cooldownAuth.ID, "", "rate_limit", now.Add(time.Minute))
	if got := classifyBravoAuthHealth("claude", cooldownAuth, now); got != bravoAuthCooldown {
		t.Fatalf("plugin cooldown health = %q, want %q", got, bravoAuthCooldown)
	}
	if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{cooldownAuth}, now); len(eligible) != 0 {
		t.Fatal("router accepted an account in the Bravo cooldown map")
	}
}

// A rate limit on one model must not disable the account's other models. The
// account-wide cooldown cascaded a single 429 on Opus into an empty pool for
// Sonnet and Haiku on the same subscription.
func TestCooldownIsScopedToTheModelThatFailed(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{ID: "scoped-cooldown", Provider: "claude"}
	setCooldown("claude", auth.ID, "claude-opus-5", "rate_limit", now.Add(time.Minute))

	limited := candidate{Provider: "claude", Model: "claude-opus-5"}
	if eligible := eligibleAuths(limited, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
		t.Fatal("router kept the rate-limited model eligible on its own account")
	}

	sibling := candidate{Provider: "claude", Model: "claude-sonnet-5"}
	if eligible := eligibleAuths(sibling, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 1 {
		t.Fatal("a per-model cooldown disabled a sibling model on the same account")
	}
}

// Credential-level rejections are not model-specific, so an empty model scope
// must still disable every model on the account.
func TestAccountWideCooldownDisablesEveryModel(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{ID: "account-cooldown", Provider: "claude"}
	setCooldown("claude", auth.ID, "", "unauthorized", now.Add(time.Minute))

	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		item := candidate{Provider: "claude", Model: model}
		if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
			t.Fatalf("account-wide cooldown left %s eligible", model)
		}
	}
}

// The production failure this guards: one model fails, the host marks the whole
// credential StatusError (conductor.go applyResult), and Bravo read that roll-up
// and dropped the account for *every* model. The native selector meanwhile routes
// on per-model state, so bravo/<model> answered 503 while the unprefixed model
// served 200 off the same subscription in the same second.
func TestHostRollupDoesNotDisableModelsWithHealthyOwnState(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:       "rollup-poisoned",
		Provider: "claude",
		// Roll-up fields as the host leaves them after a single model failure.
		Status:         "error",
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Second),
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-opus-5": {
				Status:         "error",
				Unavailable:    true,
				NextRetryAfter: now.Add(30 * time.Second),
			},
			"claude-sonnet-5": {Status: "active"},
		},
	}

	failed := candidate{Provider: "claude", Model: "claude-opus-5"}
	if got := classifyBravoAuthHealthForModel("claude", auth, failed.Model, now); got != bravoAuthCooldown {
		t.Fatalf("failed model health = %q, want %q", got, bravoAuthCooldown)
	}
	if eligible := eligibleAuths(failed, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
		t.Fatal("router kept the model that is actually cooling")
	}

	healthy := candidate{Provider: "claude", Model: "claude-sonnet-5"}
	if got := classifyBravoAuthHealthForModel("claude", auth, healthy.Model, now); got != bravoAuthReady {
		t.Fatalf("sibling model health = %q, want %q", got, bravoAuthReady)
	}
	if eligible := eligibleAuths(healthy, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 1 {
		t.Fatal("credential-wide roll-up disabled a model the host still serves")
	}

	// A thinking suffix must resolve to the same state as its base model.
	suffixed := candidate{Provider: "claude", Model: "claude-opus-5(8192)"}
	if got := classifyBravoAuthHealthForModel("claude", auth, suffixed.Model, now); got != bravoAuthCooldown {
		t.Fatalf("suffixed model health = %q, want %q", got, bravoAuthCooldown)
	}

	// The native selector treats an absent state as available whenever the
	// request names a model. The aggregate deadline was derived from the known
	// failed model and must not poison a previously unused sibling.
	unknown := candidate{Provider: "claude", Model: "claude-haiku-4-5-20251001"}
	if got := classifyBravoAuthHealthForModel("claude", auth, unknown.Model, now); got != bravoAuthReady {
		t.Fatalf("unknown sibling model health = %q, want %q", got, bravoAuthReady)
	}
	if eligible := eligibleAuths(unknown, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 1 {
		t.Fatal("aggregate state from one failed model disabled an unseen sibling")
	}
}

// Bravo keeps its immediate provider cooldown in memory, while Core persists
// the actual execution model. Automatic effort therefore restores keys such as
// "claude-fable-5(xhigh)" after a process restart. The base Bravo candidate
// must still see that state, without turning it into a credential-wide block.
func TestPersistedEffortQualifiedModelStateBlocksOnlyItsPhysicalModelAfterRestart(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	affected := pluginapi.HostAuthFileEntry{
		ID:             "palantir",
		Provider:       "claude",
		Status:         "error",
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Minute),
		ModelStates: map[string]pluginapi.HostAuthModelState{
			// A clean state for another effort must never win map iteration and
			// hide the active restriction on the same physical model.
			"claude-fable-5(max)": {
				Status: "active",
			},
			"claude-fable-5(medium)": {
				Status:         "error",
				Unavailable:    true,
				NextRetryAfter: now.Add(-time.Minute),
			},
			"claude-fable-5(xhigh)": {
				Status:                   "error",
				StatusMessage:            "Fable 5: monthly spend limit reached",
				ErrorCode:                "credits_required",
				ProviderModel:            "claude-fable-5",
				ProviderModelDisplayName: "Fable 5",
				Scope:                    "model",
				Unavailable:              true,
				NextRetryAfter:           now.Add(30 * time.Minute),
			},
		},
	}
	otherClaude := pluginapi.HostAuthFileEntry{
		ID:       "other-claude",
		Provider: "claude",
	}
	codex := pluginapi.HostAuthFileEntry{
		ID:       "codex-x20",
		Provider: "codex",
	}

	if cooldownActive("claude", affected.ID, "claude-fable-5", now) {
		t.Fatal("test must model a restart with no Bravo in-memory cooldown")
	}

	fable := candidate{Provider: "claude", Model: "claude-fable-5"}
	for iteration := 0; iteration < 64; iteration++ {
		if got := classifyBravoAuthHealthForModel("claude", affected, fable.Model, now); got != bravoAuthCooldown {
			t.Fatalf("base Fable health on iteration %d = %q, want cooldown from persisted xhigh state", iteration, got)
		}
	}
	fableEligible := eligibleAuths(fable, []pluginapi.HostAuthFileEntry{affected, otherClaude}, now)
	if len(fableEligible) != 1 || fableEligible[0].ID != otherClaude.ID {
		t.Fatalf("Fable eligible auths = %#v, want only the unaffected Claude auth", fableEligible)
	}

	sonnet := candidate{Provider: "claude", Model: "claude-sonnet-5"}
	if eligible := eligibleAuths(sonnet, []pluginapi.HostAuthFileEntry{affected}, now); len(eligible) != 1 {
		t.Fatal("persisted Fable state disabled Sonnet on the same Claude auth")
	}

	sol := candidate{Provider: "codex", Model: "gpt-5.6-sol"}
	if eligible := eligibleAuths(sol, []pluginapi.HostAuthFileEntry{codex}, now); len(eligible) != 1 {
		t.Fatal("persisted Claude state removed the Codex fallback")
	}
}

// An expired per-model deadline is stale state, not a live cooldown: the host
// clears it lazily on the next update and routes the model in the meantime.
func TestExpiredModelDeadlineIsUsableAgain(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:       "expired-state",
		Provider: "claude",
		Status:   "error",
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-opus-5": {
				Status:         "error",
				Unavailable:    true,
				NextRetryAfter: now.Add(-time.Minute),
			},
			// Unavailable with no deadline is how the host spells "usable".
			"claude-sonnet-5": {Status: "error", Unavailable: true},
		},
	}
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		if got := classifyBravoAuthHealthForModel("claude", auth, model, now); got != bravoAuthReady {
			t.Fatalf("%s health = %q, want %q", model, got, bravoAuthReady)
		}
	}
}

// Disabled is an operator decision about the credential, so it must outrank any
// per-model state — unlike StatusError, it is never a side effect of one model.
func TestDisabledCredentialOutranksPerModelState(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:          "disabled-credential",
		Provider:    "claude",
		Disabled:    true,
		ModelStates: map[string]pluginapi.HostAuthModelState{"claude-opus-5": {Status: "active"}},
	}
	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-opus-5", now); got != bravoAuthDisabled {
		t.Fatalf("health = %q, want %q", got, bravoAuthDisabled)
	}
	item := candidate{Provider: "claude", Model: "claude-opus-5"}
	if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
		t.Fatal("router used a disabled credential because one model looked healthy")
	}
}

// A per-model quota window is a real block even when Unavailable is not set.
func TestPerModelQuotaWindowBlocksOnlyItsModel(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:       "model-quota",
		Provider: "claude",
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-opus-5":   {Status: "error", QuotaExceeded: true, QuotaRecoverAt: now.Add(time.Hour)},
			"claude-sonnet-5": {Status: "active"},
		},
	}
	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-opus-5", now); got != bravoAuthCooldown {
		t.Fatalf("quota-blocked model health = %q, want %q", got, bravoAuthCooldown)
	}
	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-sonnet-5", now); got != bravoAuthReady {
		t.Fatalf("sibling model health = %q, want %q", got, bravoAuthReady)
	}
}

// The live production state, read from the management API while the pool was
// degraded: two Claude credentials at status "error" with a transient network
// message, no NextRetryAfter, and no per-model state at all. The host keeps
// routing to them (its selector blocks only on a deadline) and the next success
// clears the flag in clearAuthStateOnSuccess. Bravo used to refuse them outright,
// which removed the only thing that could clear the flag — so the credential
// stayed out of the pool permanently, not for 30 seconds.
func TestStuckErrorStatusWithoutDeadlineStaysRoutable(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	for _, message := range []string{
		`Post "https://api.anthropic.com/v1/messages?beta=true": unexpected EOF`,
		`Post "https://api.anthropic.com/v1/messages?beta=true": socks connect tcp: unknown error general SOCKS`,
	} {
		auth := pluginapi.HostAuthFileEntry{
			ID:            "stuck-error",
			Provider:      "claude",
			Status:        "error",
			StatusMessage: message,
			// No NextRetryAfter and no ModelStates — exactly as the host left it.
		}
		item := candidate{Provider: "claude", Model: "claude-opus-5"}
		if got := classifyBravoAuthHealthForModel("claude", auth, item.Model, now); got != bravoAuthReady {
			t.Fatalf("health for %q = %q, want %q", message, got, bravoAuthReady)
		}
		if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 1 {
			t.Fatalf("a stuck error flag with no deadline removed the credential (%q)", message)
		}
	}
}

// A roll-up deadline is real: the host set it, it expires on its own, and the
// native selector honours it. Only the deadline-free flag is ignored.
func TestRollupDeadlineStillBlocksWhenNoModelStateExists(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:             "rollup-deadline",
		Provider:       "claude",
		Status:         "error",
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Second),
	}
	item := candidate{Provider: "claude", Model: "claude-opus-5"}
	if got := classifyBravoAuthHealthForModel("claude", auth, item.Model, now); got != bravoAuthCooldown {
		t.Fatalf("health = %q, want %q", got, bravoAuthCooldown)
	}
	if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{auth}, now); len(eligible) != 0 {
		t.Fatal("a live roll-up deadline was ignored")
	}

	// Once it expires the credential returns without needing any state update.
	later := auth.NextRetryAfter.Add(time.Second)
	if got := classifyBravoAuthHealthForModel("claude", auth, item.Model, later); got != bravoAuthReady {
		t.Fatalf("health after the deadline = %q, want %q", got, bravoAuthReady)
	}
}

// disabled is an operator decision and has no deadline to wait for, so it must
// still block on the stateless path.
func TestDisabledStillBlocksWithoutModelState(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	for _, auth := range []pluginapi.HostAuthFileEntry{
		{ID: "flagged", Provider: "claude", Disabled: true},
		{ID: "by-status", Provider: "claude", Status: "disabled"},
	} {
		if got := classifyBravoAuthHealthForModel("claude", auth, "claude-opus-5", now); got != bravoAuthDisabled {
			t.Fatalf("health for %+v = %q, want %q", auth, got, bravoAuthDisabled)
		}
	}
}

// Bravo's own cooldown map is credential-wide when its model scope is empty, and
// that must survive the per-model path — 401/403 revoke the whole credential.
func TestBravoAccountWideCooldownSurvivesPerModelState(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	auth := pluginapi.HostAuthFileEntry{
		ID:          "bravo-account-cooldown",
		Provider:    "claude",
		ModelStates: map[string]pluginapi.HostAuthModelState{"claude-opus-5": {Status: "active"}},
	}
	setCooldown("claude", auth.ID, "", "unauthorized", now.Add(time.Minute))
	if got := classifyBravoAuthHealthForModel("claude", auth, "claude-opus-5", now); got != bravoAuthCooldown {
		t.Fatalf("health = %q, want %q", got, bravoAuthCooldown)
	}
}

func TestAccountWideCooldownStatusCoversCredentialRejections(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if !accountWideCooldownStatus(status) {
			t.Fatalf("status %d must disable the whole account", status)
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		if accountWideCooldownStatus(status) {
			t.Fatalf("status %d must only disable the model that failed", status)
		}
	}
}

func TestSummarizeBravoProvidersFiltersUnusedProvidersAndClassifiesHealth(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	cfg := pluginConfig{Models: map[string]logicalModel{
		"test": {Candidates: []candidate{
			{Provider: "claude", Model: "claude-test"},
			{Provider: "codex", Model: "codex-test"},
		}},
	}}
	summaries := summarizeBravoProviders(cfg, []pluginapi.HostAuthFileEntry{
		{ID: "claude-ready", Provider: "anthropic"},
		{ID: "claude-error", Provider: "claude", Status: "error"},
		{ID: "claude-disabled", Provider: "claude", Disabled: true},
		{ID: "codex-retry", Provider: "openai", NextRetryAfter: now.Add(time.Minute)},
		{ID: "gemini-ready", Provider: "gemini"},
	}, now)

	if len(summaries) != 2 {
		t.Fatalf("provider summaries = %#v, want claude and codex only", summaries)
	}
	claude := summaries[0]
	if claude.Provider != "claude" || claude.Accounts != 3 || claude.Healthy != 1 ||
		claude.Unavailable != 2 || claude.Disabled != 1 || claude.Errors != 1 {
		t.Fatalf("claude summary = %#v", claude)
	}
	codex := summaries[1]
	if codex.Provider != "codex" || codex.Accounts != 1 || codex.Healthy != 0 ||
		codex.Unavailable != 1 || codex.Cooldown != 1 {
		t.Fatalf("codex summary = %#v", codex)
	}
}

func TestProviderSummaryIgnoresSingleModelAggregatePoison(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	cfg := pluginConfig{Models: map[string]logicalModel{
		"test": {Candidates: []candidate{{Provider: "claude", Model: "claude-sonnet-5"}}},
	}}
	auth := pluginapi.HostAuthFileEntry{
		ID:             "palantir",
		Provider:       "claude",
		Status:         "error",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Hour),
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-fable-5": {
				Status:         "error",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				ErrorCode:      "credits_required",
				Scope:          "model",
			},
		},
	}

	summaries := summarizeBravoProviders(cfg, []pluginapi.HostAuthFileEntry{auth}, now)
	if len(summaries) != 1 || summaries[0].Healthy != 1 || summaries[0].Unavailable != 0 {
		t.Fatalf("model-scoped provider summary = %#v, want one healthy account", summaries)
	}

	setCooldown("claude", auth.ID, "", "account-wide", now.Add(time.Hour))
	summaries = summarizeBravoProviders(cfg, []pluginapi.HostAuthFileEntry{auth}, now)
	if len(summaries) != 1 || summaries[0].Cooldown != 1 || summaries[0].Unavailable != 1 {
		t.Fatalf("account-wide provider summary = %#v, want one cooling account", summaries)
	}
}

func isolateBravoCooldowns(t *testing.T) {
	t.Helper()
	runtimeState.Lock()
	previous := runtimeState.Cooldowns
	runtimeState.Cooldowns = make(map[string]cooldownEntry)
	runtimeState.Unlock()
	t.Cleanup(func() {
		runtimeState.Lock()
		runtimeState.Cooldowns = previous
		runtimeState.Unlock()
	})
}
