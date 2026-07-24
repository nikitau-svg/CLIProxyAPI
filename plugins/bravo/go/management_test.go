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

func TestClassifyBravoAuthHealthMatchesRouterEligibility(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	item := candidate{Provider: "claude", Model: "claude-test"}
	cases := []struct {
		name string
		auth pluginapi.HostAuthFileEntry
		want bravoAuthHealth
	}{
		{
			name: "ready",
			auth: pluginapi.HostAuthFileEntry{ID: "ready", Provider: "claude"},
			want: bravoAuthReady,
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
			name: "unavailable",
			auth: pluginapi.HostAuthFileEntry{ID: "unavailable", Provider: "claude", Unavailable: true},
			want: bravoAuthUnavailable,
		},
		{
			name: "error status",
			auth: pluginapi.HostAuthFileEntry{ID: "error", Provider: "claude", Status: "error"},
			want: bravoAuthError,
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
			if gotEligible, wantEligible := len(eligible) == 1, testCase.want == bravoAuthReady; gotEligible != wantEligible {
				t.Fatalf("eligibleAuths() eligible = %v, want %v for state %q", gotEligible, wantEligible, testCase.want)
			}
		})
	}

	cooldownAuth := pluginapi.HostAuthFileEntry{ID: "plugin-cooldown", Provider: "claude"}
	setCooldown("claude", cooldownAuth.ID, "rate_limit", now.Add(time.Minute))
	if got := classifyBravoAuthHealth("claude", cooldownAuth, now); got != bravoAuthCooldown {
		t.Fatalf("plugin cooldown health = %q, want %q", got, bravoAuthCooldown)
	}
	if eligible := eligibleAuths(item, []pluginapi.HostAuthFileEntry{cooldownAuth}, now); len(eligible) != 0 {
		t.Fatal("router accepted an account in the Bravo cooldown map")
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
