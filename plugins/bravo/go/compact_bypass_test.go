package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const claudeCompactPromptFixture = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.
Create a compact summary of the current conversation.
Your response must be an <analysis> block followed by a <summary> block.
REMINDER: Do NOT call any tools. Tool calls will be rejected and you will fail the task.`

func TestClaudeCLICompactDetectionUsesCurrentPromptAndTransportIdentity(t *testing.T) {
	project := smartKeyConfig{ID: "prj_compact"}
	request := compactTestRequest(claudeCompactPromptFixture)
	if _, ok := claudeCLICompactBypassKey(request, project); !ok {
		t.Fatal("real Claude CLI compact prompt was not detected")
	}

	tests := []struct {
		name   string
		mutate func(*rpcExecutorRequest)
	}{
		{
			name: "ordinary request with historical command",
			mutate: func(req *rpcExecutorRequest) {
				req.OriginalRequest = []byte(`{"model":"bravo/fable","messages":[{"role":"user","content":[{"type":"text","text":"<command-name>/compact</command-name>"}]},{"role":"assistant","content":"old summary"},{"role":"user","content":"continue"}]}`)
			},
		},
		{
			name: "missing session id",
			mutate: func(req *rpcExecutorRequest) {
				req.Headers.Del("X-Claude-Code-Session-Id")
			},
		},
		{
			name: "non CLI user agent",
			mutate: func(req *rpcExecutorRequest) {
				req.Headers.Set("User-Agent", "custom-client")
			},
		},
		{
			name: "different protocol",
			mutate: func(req *rpcExecutorRequest) {
				req.Format = protocolOpenAI
				req.SourceFormat = protocolOpenAI
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := compactTestRequest(claudeCompactPromptFixture)
			testCase.mutate(&candidate)
			if _, ok := claudeCLICompactBypassKey(candidate, project); ok {
				t.Fatal("request incorrectly received compact bypass identity")
			}
		})
	}
}

func TestCompactBypassLeaseIsSingleFlightAndCooldownBounded(t *testing.T) {
	isolateCompactBypassState(t)
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	release, _, acquired := reserveCompactBypass("project\x00session", 15*time.Minute, now)
	if !acquired {
		t.Fatal("first compact bypass lease was rejected")
	}
	if _, _, duplicate := reserveCompactBypass("project\x00session", 15*time.Minute, now); duplicate {
		t.Fatal("concurrent compact bypass lease was accepted")
	}
	release(true, now.Add(time.Minute))
	if _, wait, immediate := reserveCompactBypass("project\x00session", 15*time.Minute, now.Add(2*time.Minute)); immediate || wait != 14*time.Minute {
		t.Fatalf("cooldown result = acquired %v wait %v, want false and 14m", immediate, wait)
	}
	secondRelease, _, afterCooldown := reserveCompactBypass("project\x00session", 15*time.Minute, now.Add(16*time.Minute))
	if !afterCooldown {
		t.Fatal("compact bypass stayed blocked after cooldown")
	}
	secondRelease(false, now.Add(16*time.Minute))
	if _, _, afterRollback := reserveCompactBypass("project\x00session", 15*time.Minute, now.Add(16*time.Minute)); !afterRollback {
		t.Fatal("uncommitted compact bypass did not roll back its lease")
	}
}

func TestCompactBypassRequiresConfirmedPositiveQuota(t *testing.T) {
	tests := []struct {
		name  string
		quota credentialQuotaState
	}{
		{
			name: "quota has not been confirmed",
			quota: credentialQuotaState{
				Confidence: "unknown",
				Session:    quotaWindowState{RemainingPercent: 100},
				Weekly:     quotaWindowState{RemainingPercent: 100},
			},
		},
		{
			name: "session quota is exhausted",
			quota: credentialQuotaState{
				Confidence: "confirmed",
				Session:    quotaWindowState{RemainingPercent: 0, UsedPercent: 100},
				Weekly:     quotaWindowState{RemainingPercent: 100},
			},
		},
		{
			name: "weekly quota is exhausted",
			quota: credentialQuotaState{
				Confidence: "confirmed",
				Session:    quotaWindowState{RemainingPercent: 100},
				Weekly:     quotaWindowState{RemainingPercent: 0, UsedPercent: 100},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if compactBypassQuotaEligible(testCase.quota, "claude-fable-5", "auth", 0.5) {
				t.Fatal("compact bypass accepted quota that is unknown or exhausted")
			}
		})
	}
}

func TestCompactPlanBypassesOnlyInternalClaudeFloor(t *testing.T) {
	isolateBravoFallbackTestState(t)
	isolateCompactBypassState(t)
	const (
		claudeIndex = "1111111111111111"
		codexIndex  = "2222222222222222"
		projectID   = "prj_compact_floor"
	)
	now := time.Now().UTC()
	installCompactQuotaState(t, map[string]*credentialQuotaState{
		claudeIndex: {
			Provider:    "claude",
			Plan:        "team",
			Confidence:  "confirmed",
			RefreshedAt: now,
			Session:     quotaWindowState{RemainingPercent: 37, UsedPercent: 63},
			Weekly:      quotaWindowState{RemainingPercent: 75, UsedPercent: 25},
		},
		codexIndex: {
			Provider:    "codex",
			Plan:        "pro",
			Confidence:  "confirmed",
			RefreshedAt: now,
			Session:     quotaWindowState{RemainingPercent: 100},
			Weekly:      quotaWindowState{RemainingPercent: 80, UsedPercent: 20},
		},
	})

	cfg := defaultPluginConfig()
	cfg.Models = map[string]logicalModel{"fallback-probe": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-5.6-sol", Priority: 90, Capabilities: []string{capabilityText}},
	}}}
	cfg.Tariffs = []tariffConfig{
		{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 0.5},
		{ID: "x20", SessionFloorPercent: 10, WeeklyFloorPercent: 10, Multiplier: 20, ReservationPercent: 0.05},
	}
	cfg.Subscriptions = []subscriptionConfig{
		{AuthIndex: claudeIndex, Tariff: "x1"},
		{AuthIndex: codexIndex, Tariff: "x20"},
	}
	cfg.SmartKeys = []smartKeyConfig{{
		ID:             projectID,
		Name:           "Compact floor",
		SHA256:         strings.Repeat("a", 64),
		Enabled:        boolPointer(true),
		Status:         projectStatusActive,
		Models:         []string{"*"},
		AllowedAuthIDs: []string{claudeIndex, codexIndex},
	}}
	cfg.QuotaRefreshSeconds = 3600
	cfg.CompactBypassCooldownSeconds = 900
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-auth", AuthIndex: claudeIndex, Provider: "claude"},
		{ID: "codex-auth", AuthIndex: codexIndex, Provider: "codex"},
	}
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostLog:
			return json.RawMessage(`{}`), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	contract := requestCapabilityContract{Protocol: protocolClaude, Capabilities: newCapabilitySet(capabilityText)}
	normal := compactTestRequest("continue")
	normal.Metadata = compactProjectMetadata(projectID)
	normalPlan, errNormal := buildExecutionPlan(normal, "fallback-probe", cfg.Models["fallback-probe"], contract)
	if errNormal != nil {
		t.Fatal(errNormal)
	}
	if len(normalPlan) == 0 || normalizeProvider(normalPlan[0].Candidate.Provider) != "codex" {
		t.Fatalf("normal plan = %#v, want Codex after Claude floor", normalPlan)
	}
	if len(normalPlan[0].PreflightRejections) == 0 || normalPlan[0].PreflightRejections[0].Code != "bravo_allocator_reserve_floor" {
		t.Fatalf("normal plan lost allocator rejection: %#v", normalPlan)
	}

	compact := compactTestRequest(claudeCompactPromptFixture)
	compact.Metadata = compactProjectMetadata(projectID)
	compactPlan, errCompact := buildExecutionPlan(compact, "fallback-probe", cfg.Models["fallback-probe"], contract)
	if errCompact != nil {
		t.Fatal(errCompact)
	}
	if len(compactPlan) == 0 || normalizeProvider(compactPlan[0].Candidate.Provider) != "claude" || !compactPlan[0].CompactBypass {
		t.Fatalf("compact plan = %#v, want Claude reserve bypass first", compactPlan)
	}
	if compactPlan[0].Auth.AuthIndex != claudeIndex {
		t.Fatalf("compact bypass auth = %q, want project-allowed Claude auth", compactPlan[0].Auth.AuthIndex)
	}
}

func compactTestRequest(lastUserText string) rpcExecutorRequest {
	body, _ := json.Marshal(map[string]any{
		"model": "bravo/fable",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": lastUserText}}},
		},
		"stream": true,
	})
	return rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           "bravo/fallback-probe",
		Format:          protocolClaude,
		SourceFormat:    protocolClaude,
		OriginalRequest: body,
		Headers: http.Header{
			"User-Agent":               []string{"claude-cli/2.1.221 (external, cli)"},
			"X-Claude-Code-Session-Id": []string{"session-compact-test"},
		},
	}}
}

func compactProjectMetadata(projectID string) map[string]any {
	return map[string]any{"access_metadata": map[string]string{
		bravoAccessProviderMetadataKey: pluginIdentifier,
		bravoProjectIDMetadataKey:      projectID,
	}}
}

func isolateCompactBypassState(t *testing.T) {
	t.Helper()
	compactBypassRuntime.Lock()
	previousNext := compactBypassRuntime.NextAllowed
	previousInFlight := compactBypassRuntime.InFlight
	compactBypassRuntime.NextAllowed = make(map[string]time.Time)
	compactBypassRuntime.InFlight = make(map[string]bool)
	compactBypassRuntime.Unlock()
	t.Cleanup(func() {
		compactBypassRuntime.Lock()
		compactBypassRuntime.NextAllowed = previousNext
		compactBypassRuntime.InFlight = previousInFlight
		compactBypassRuntime.Unlock()
	})
}

func installCompactQuotaState(t *testing.T, quotas map[string]*credentialQuotaState) {
	t.Helper()
	bravoUsageState.mu.Lock()
	previous := bravoUsageState.state.Quotas
	bravoUsageState.state.Quotas = quotas
	bravoUsageState.mu.Unlock()
	t.Cleanup(func() {
		bravoUsageState.mu.Lock()
		bravoUsageState.state.Quotas = previous
		bravoUsageState.mu.Unlock()
	})
}
