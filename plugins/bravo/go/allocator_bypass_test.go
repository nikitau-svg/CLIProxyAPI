package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// textContract is the contract a plain text request produces: the Claude
// Messages protocol asking only for text. An empty contract has no protocol and
// is rejected by resolveCandidateContract before the allocator is ever reached.
func textContract() requestCapabilityContract {
	return requestCapabilityContract{
		Protocol:     protocolClaude,
		Capabilities: capabilitySet{capabilityText: {}},
	}
}

// U26: an allocator rejection is an execution boundary. Generic planning must
// never manufacture an unmanaged attempt around unknown quota or reserve debt.
func TestAllocatorBypassFailsClosedForWithheldCredential(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
	}}
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", AuthIndex: "idx-a", Name: "claude-a.json", Provider: "claude"},
	}
	rejections := []candidateRejection{{
		Provider: "claude",
		Model:    "claude-opus-5",
		Stage:    "allocator",
		Code:     "bravo_allocator_withheld",
		Reason:   "allocator released none of 1 eligible credentials",
	}}

	plan := allocatorBypassPlan("opus", model, textContract(), auths, rejections, "", now)
	if len(plan) != 0 {
		t.Fatalf("allocator rejection produced %d unmanaged attempts, want fail-closed", len(plan))
	}
}

// Only allocator verdicts are bypassed. A candidate the request genuinely cannot
// run on — wrong capabilities, or no healthy credential — must stay rejected.
func TestAllocatorBypassIgnoresNonAllocatorRejections(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
	}}
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", AuthIndex: "idx-a", Name: "claude-a.json", Provider: "claude"},
	}

	for _, stage := range []string{"contract", "eligibility"} {
		rejections := []candidateRejection{{
			Provider: "claude",
			Model:    "claude-opus-5",
			Stage:    stage,
			Code:     "bravo_" + stage,
			Reason:   stage + " rejection",
		}}
		if plan := allocatorBypassPlan("opus", model, textContract(), auths, rejections, "", now); len(plan) != 0 {
			t.Fatalf("stage %q was bypassed; plan length = %d, want 0", stage, len(plan))
		}
	}
}

// The bypass must not reach a credential the project is not allowed to use. It
// re-runs eligibility over the caller's already project-filtered list, so an auth
// outside that list is unreachable.
func TestAllocatorBypassCannotWidenTheAuthorizationBoundary(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
	}}
	project := smartKeyConfig{ID: "prj-test", AllowedAuthIDs: []string{"claude-allowed"}}
	all := []pluginapi.HostAuthFileEntry{
		{ID: "claude-allowed", AuthIndex: "idx-allowed", Name: "claude-allowed.json", Provider: "claude"},
		{ID: "claude-forbidden", AuthIndex: "idx-forbidden", Name: "claude-forbidden.json", Provider: "claude"},
	}
	// Mirror the caller: allowed_auth_ids is applied before planning.
	allowed := filterProjectAllowedAuths(project, all)
	if len(allowed) != 1 {
		t.Fatalf("project filter kept %d auths, want 1", len(allowed))
	}
	rejections := []candidateRejection{{
		Provider: "claude",
		Model:    "claude-opus-5",
		Stage:    "allocator",
		Code:     "bravo_allocator_withheld",
		Reason:   "withheld",
	}}

	plan := allocatorBypassPlan("opus", model, textContract(), allowed, rejections, "", now)
	for _, attempt := range plan {
		if attempt.Auth.ID == "claude-forbidden" {
			t.Fatal("bypass reached a credential outside the project's allowed_auth_ids")
		}
	}
	if len(plan) != 0 {
		t.Fatalf("authorization-filtered allocator rejection produced %d unmanaged attempts", len(plan))
	}
}

// A credential that is actually cooling is not a budget decision, so the bypass
// must leave it alone: retrying a rate-limited model only earns another 429.
func TestAllocatorBypassSkipsUnhealthyCredentials(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Priority: 100, Capabilities: []string{capabilityText}},
	}}
	auths := []pluginapi.HostAuthFileEntry{{
		ID:        "claude-cooling",
		AuthIndex: "idx-cooling",
		Name:      "claude-cooling.json",
		Provider:  "claude",
		ModelStates: map[string]pluginapi.HostAuthModelState{
			"claude-opus-5": {Status: "error", Unavailable: true, NextRetryAfter: now.Add(time.Minute)},
		},
	}}
	rejections := []candidateRejection{{
		Provider: "claude",
		Model:    "claude-opus-5",
		Stage:    "allocator",
		Code:     "bravo_allocator_withheld",
		Reason:   "withheld",
	}}

	if plan := allocatorBypassPlan("opus", model, textContract(), auths, rejections, "", now); len(plan) != 0 {
		t.Fatalf("bypass used a cooling credential; plan length = %d, want 0", len(plan))
	}
}

// With nothing withheld by the allocator there is nothing to bypass, and the
// caller must fall through to its normal error.
func TestAllocatorBypassIsInertWithoutAllocatorRejections(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Capabilities: []string{capabilityText}},
	}}
	auths := []pluginapi.HostAuthFileEntry{{ID: "claude-a", AuthIndex: "idx-a", Provider: "claude"}}
	if plan := allocatorBypassPlan("opus", model, textContract(), auths, nil, "", now); len(plan) != 0 {
		t.Fatalf("bypass produced %d attempts with no allocator rejections", len(plan))
	}
}

// U27: provider call budget cannot revive an allocator-rejected credential.
func TestAllocatorBypassCannotUseProviderBudgetToReviveRejectedCredential(t *testing.T) {
	isolateBravoCooldowns(t)
	now := time.Now()
	previous := loadedConfig()
	cfg := previous
	cfg.MaxAttempts = 1
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })

	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-opus-5", Capabilities: []string{capabilityText}},
	}}
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "claude-a", AuthIndex: "idx-a", Provider: "claude"},
		{ID: "claude-b", AuthIndex: "idx-b", Provider: "claude"},
	}
	rejections := []candidateRejection{{
		Provider: "claude",
		Model:    "claude-opus-5",
		Stage:    "allocator",
		Code:     "bravo_allocator_withheld",
		Reason:   "withheld",
	}}
	plan := allocatorBypassPlan("opus", model, textContract(), auths, rejections, "", now)
	if len(plan) != 0 {
		t.Fatalf("provider budget revived %d allocator-rejected attempts", len(plan))
	}
}

// The bypass overspends a budget the operator configured, so it must be visible
// in the log rather than silent.
func TestAllocatorBypassIsLogged(t *testing.T) {
	isolateBravoCooldowns(t)
	var logged []map[string]any
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostLog {
			var entry map[string]any
			decodeBravoPayload(t, payload, &entry)
			logged = append(logged, entry)
		}
		return json.RawMessage(`{}`), nil
	})

	logAllocatorBypass("opus", []candidateRejection{{
		Provider: "claude",
		Model:    "claude-opus-5",
		Stage:    "allocator",
		Code:     "bravo_allocator_withheld",
		Reason:   "withheld",
	}})

	if len(logged) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logged))
	}
	message, _ := logged[0]["message"].(string)
	if !strings.Contains(message, "резервного порога") {
		t.Fatalf("log message = %q, want it to name the allocator override", message)
	}
}
