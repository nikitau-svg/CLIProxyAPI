package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProjectAllowedPoolIsHardBoundaryInEveryAllocatorMode(t *testing.T) {
	for modeIndex, mode := range []string{"off", "observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			plaintext := "brv_allowed_pool_" + mode
			sum := sha256.Sum256([]byte(plaintext))
			claudeAllowed := fmt.Sprintf("%016x", 100+modeIndex*10)
			codexAllowed := fmt.Sprintf("%016x", 101+modeIndex*10)
			claudeOutside := fmt.Sprintf("%016x", 102+modeIndex*10)
			codexOutside := fmt.Sprintf("%016x", 103+modeIndex*10)
			auths := []pluginapi.HostAuthFileEntry{
				{ID: "claude-allowed-" + mode, AuthIndex: claudeAllowed, Name: "claude-allowed.json", Provider: "claude"},
				{ID: "claude-outside-" + mode, AuthIndex: claudeOutside, Name: "claude-outside.json", Provider: "claude"},
				{ID: "codex-allowed-" + mode, AuthIndex: codexAllowed, Name: "codex-allowed.json", Provider: "codex"},
				{ID: "codex-outside-" + mode, AuthIndex: codexOutside, Name: "codex-outside.json", Provider: "codex"},
			}
			cfg := defaultPluginConfig()
			cfg.AllocatorMode = mode
			cfg.UnknownSecondaryPolicy = "allow"
			cfg.Models = map[string]logicalModel{
				"pool-probe": {
					Candidates: []candidate{
						{Provider: "claude", Model: "claude-opus-4-8", Effort: "high", Priority: 100, Capabilities: []string{capabilityText}},
						{Provider: "codex", Model: "gpt-5.6-sol", Effort: "max", Priority: 90, Capabilities: []string{capabilityText}},
					},
				},
			}
			cfg.SmartKeys = []smartKeyConfig{{
				ID:             "prj_pool_" + mode,
				Name:           "Pool " + mode,
				SHA256:         hex.EncodeToString(sum[:]),
				Enabled:        boolPointer(true),
				Status:         projectStatusActive,
				Models:         []string{"*"},
				PrimaryAuthIDs: []string{claudeAllowed},
				AllowedAuthIDs: []string{claudeAllowed, codexAllowed},
			}}
			if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
				t.Fatal(errNormalize)
			}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			t.Cleanup(func() { currentConfig.Store(previousConfig) })
			if mode == "enforce" {
				// Enforce is fail-closed for unknown secondary quota even when a
				// legacy config says allow. This test is about the authorization pool,
				// so install confirmed snapshots instead of relying on async polling.
				installAdaptiveTestQuota(t, claudeAllowed, 90, 90)
				installAdaptiveTestQuota(t, codexAllowed, 90, 90)
			}

			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostAuthQuotaGet:
					var request pluginapi.HostAuthQuotaRequest
					decodeBravoPayload(t, payload, &request)
					provider := "claude"
					if request.AuthIndex == codexAllowed {
						provider = "codex"
					}
					now := time.Now().UTC()
					return mustBravoJSON(t, pluginapi.HostAuthQuotaResponse{
						AuthIndex:  request.AuthIndex,
						Provider:   provider,
						PlanLabel:  "pro",
						ObservedAt: now,
						Confidence: pluginapi.HostAuthQuotaConfidenceConfirmed,
						Windows: []pluginapi.HostAuthQuotaWindow{
							{ID: "session", Kind: pluginapi.HostAuthQuotaWindowKindSession, UsedPercent: 10, RemainingPercent: 90, ResetAt: now.Add(time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
							{ID: "weekly", Kind: pluginapi.HostAuthQuotaWindowKindWeekly, UsedPercent: 10, RemainingPercent: 90, ResetAt: now.Add(24 * time.Hour), ResetMode: pluginapi.HostAuthQuotaResetModeScheduled},
						},
					}), nil
				default:
					t.Fatalf("unexpected host callback %q", method)
					return nil, nil
				}
			})

			model := cfg.Models["pool-probe"]
			plan, errPlan := buildExecutionPlan(
				rpcExecutorRequest{
					ExecutorRequest: pluginapi.ExecutorRequest{
						Model:   "bravo/pool-probe",
						Headers: http.Header{"Authorization": []string{"Bearer " + plaintext}},
					},
					HostCallbackID: "allowed-pool-test",
				},
				"pool-probe",
				model,
				requestCapabilityContract{
					Protocol:     protocolOpenAI,
					Capabilities: newCapabilitySet(capabilityText),
				},
			)
			if errPlan != nil {
				t.Fatal(errPlan)
			}
			got := make([]string, 0, len(plan))
			for _, attempt := range plan {
				got = append(got, attempt.Auth.AuthIndex)
				if attempt.Auth.AuthIndex == claudeOutside || attempt.Auth.AuthIndex == codexOutside {
					t.Fatalf("allocator mode %s escaped allowed_auth_ids: %#v", mode, plan)
				}
			}
			sort.Strings(got)
			want := []string{claudeAllowed, codexAllowed}
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("allocator mode %s plan auths = %v, want %v", mode, got, want)
			}
		})
	}
}

func TestProjectAllowedPoolSeparatesPersonalAndWorkAndFailsClosedOnStaleIDs(t *testing.T) {
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "personal-id", AuthIndex: "aaaaaaaaaaaaaaaa", Provider: "claude"},
		{ID: "work-id", AuthIndex: "bbbbbbbbbbbbbbbb", Provider: "claude"},
	}
	personal := filterProjectAllowedAuths(smartKeyConfig{AllowedAuthIDs: []string{"personal-id"}}, auths)
	work := filterProjectAllowedAuths(smartKeyConfig{AllowedAuthIDs: []string{"bbbbbbbbbbbbbbbb"}}, auths)
	all := filterProjectAllowedAuths(smartKeyConfig{}, auths)
	stale := filterProjectAllowedAuths(smartKeyConfig{AllowedAuthIDs: []string{"missing-id"}}, auths)
	if len(personal) != 1 || personal[0].AuthIndex != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("personal pool = %#v", personal)
	}
	if len(work) != 1 || work[0].AuthIndex != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("work pool = %#v", work)
	}
	if len(all) != len(auths) {
		t.Fatalf("empty allowed_auth_ids did not preserve backward-compatible all-pool behavior: %#v", all)
	}
	if len(stale) != 0 {
		t.Fatalf("stale allowed_auth_ids did not fail closed: %#v", stale)
	}
}

func TestProjectAllowedPoolValidationCanonicalizesAndRequiresPrimarySubset(t *testing.T) {
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "personal-id", AuthIndex: "aaaaaaaaaaaaaaaa", Name: "personal.json", Provider: "claude"},
		{ID: "work-id", AuthIndex: "bbbbbbbbbbbbbbbb", Name: "work.json", Provider: "claude"},
	}
	installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("unexpected host callback %q", method)
		}
		return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
	})

	project := smartKeyConfig{
		ID:             "prj_scope",
		Enabled:        boolPointer(true),
		Status:         projectStatusActive,
		AllowedAuthIDs: []string{"personal-id"},
		PrimaryAuthIDs: []string{"personal.json"},
	}
	if failure := validateAndCanonicalizeProjectPrimaries("callback", pluginConfig{}, nil, &project); failure != nil {
		t.Fatalf("valid scoped project failed: %#v", failure)
	}
	if len(project.AllowedAuthIDs) != 1 || project.AllowedAuthIDs[0] != "aaaaaaaaaaaaaaaa" ||
		len(project.PrimaryAuthIDs) != 1 || project.PrimaryAuthIDs[0] != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("canonical project scope = %#v", project)
	}

	outside := project
	outside.PrimaryAuthIDs = []string{"work-id"}
	if failure := validateAndCanonicalizeProjectPrimaries("callback", pluginConfig{}, nil, &outside); failure == nil ||
		failure.Code != "bravo_primary_auth_outside_allowed_pool" ||
		failure.Status != http.StatusConflict {
		t.Fatalf("outside primary failure = %#v", failure)
	}

	unknown := project
	unknown.AllowedAuthIDs = []string{"missing-id"}
	unknown.PrimaryAuthIDs = nil
	if failure := validateAndCanonicalizeProjectPrimaries("callback", pluginConfig{}, nil, &unknown); failure == nil ||
		failure.Code != "bravo_allowed_auth_not_found" ||
		failure.Status != http.StatusConflict {
		t.Fatalf("unknown allowed auth failure = %#v", failure)
	}
}
