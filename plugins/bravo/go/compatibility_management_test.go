package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCompatibilityUnknownModelFailsClosedAndSuppressesOutOfScopeModels(t *testing.T) {
	retiredProfile := compatibilityProfiles()[routeCandidateKey("claude", "claude-opus-4-8")]
	retiredCandidate := candidateFromCompatibilityProfile(retiredProfile)
	cfg := pluginConfig{
		BaseModels: map[string]logicalModel{
			"claude-opus-4-8": {Candidates: []candidate{retiredCandidate}},
		},
		Models: map[string]logicalModel{
			"claude-opus-4-8": {Candidates: []candidate{retiredCandidate}},
		},
	}
	hostModels := []pluginapi.HostModelListEntry{
		{Provider: "gemini", ID: "gemini-future"},
		{Provider: "codex", ID: "codex-auto-review"},
		{Provider: "codex", ID: "gpt-5.3-codex-spark"},
		{Provider: "claude", ID: "claude-opus-6", DisplayName: "Claude Opus 6", Available: true},
	}
	for _, profile := range compatibilityProfiles() {
		entry := hostEntryFromCompatibilityProfile(profile)
		if profile.Model == "claude-opus-4-8" {
			entry.Provider = "anthropic"
			entry.DisplayName = "Claude Opus 4.8"
			entry.Available = true
		}
		hostModels = append(hostModels, entry)
	}
	response := collectCompatibilityResponse(cfg, hostModels, time.Unix(10, 0).UTC())

	if response.Summary.Total != len(compatibilityProfiles())+1 || response.Summary.Supported != 1 ||
		response.Summary.CodeFix != 1 || response.Summary.ActionRequired != response.Summary.Total-1 {
		t.Fatalf("summary = %#v", response.Summary)
	}
	for _, item := range response.Models {
		if item.Provider == "gemini" || item.Model == "codex-auto-review" || item.Model == "gpt-5.3-codex-spark" {
			t.Fatalf("out-of-scope model was returned: %#v", item)
		}
	}
	unknown := compatibilityModelByID(t, response, "claude", "claude-opus-6")
	if unknown.Status != compatibilityCodeFix || unknown.Classification != compatibilityCodeFix ||
		len(unknown.RequiredFixes) != 1 || unknown.RequiredFixes[0] != compatibilityCodeFix {
		t.Fatalf("unknown classification = %#v", unknown)
	}
	if !unknown.Detected.Host || unknown.Detected.Bravo || unknown.Detected.Route {
		t.Fatalf("unknown detection = %#v", unknown.Detected)
	}
	if len(unknown.SuggestedFixes) != 1 || unknown.SuggestedFixes[0].SafeToApplyAutomatically ||
		!strings.Contains(unknown.SuggestedFixes[0].Snippet, "live provider-contract tests pass") {
		t.Fatalf("unknown suggestions = %#v", unknown.SuggestedFixes)
	}
	if len(unknown.SuggestedFixes[0].ReasonCodes) != 1 ||
		unknown.SuggestedFixes[0].ReasonCodes[0] != "bravo_compatibility_profile_missing" ||
		len(unknown.SuggestedFixes[0].Targets) != 2 {
		t.Fatalf("unknown suggestion context = %#v", unknown.SuggestedFixes[0])
	}
}

func TestCompatibilityClassifiesYAMLAndRouteFixesInStages(t *testing.T) {
	profile := compatibilityProfiles()[routeCandidateKey("claude", "claude-opus-5")]
	host := pluginapi.HostModelListEntry{
		Provider:  "claude",
		ID:        "claude-opus-5",
		Catalog:   true,
		Available: true,
		Thinking: &pluginapi.ThinkingSupport{
			ZeroAllowed:     true,
			DefaultOn:       true,
			MaxDisableLevel: "high",
		},
	}
	fallback := candidate{
		Provider:     "codex",
		Model:        "gpt-5.6-sol",
		Effort:       "high",
		Priority:     90,
		Capabilities: []string{capabilityText},
	}

	t.Run("default Opus 5 overlay is supported", func(t *testing.T) {
		cfg := defaultPluginConfig()
		if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
			t.Fatal(errNormalize)
		}
		response := collectCompatibilityResponse(cfg, []pluginapi.HostModelListEntry{host}, time.Time{})
		item := compatibilityModelByID(t, response, "claude", "claude-opus-5")
		if item.Classification != compatibilitySupported || !item.Detected.Host ||
			!item.Detected.Bravo || !item.Detected.Route || item.Source != "host_and_profile" {
			t.Fatalf("item = %#v", item)
		}
		if item.HostMetadata == nil || item.HostMetadata.Thinking == nil ||
			!item.HostMetadata.Thinking.DefaultOn || item.HostMetadata.Thinking.MaxDisableLevel != "high" {
			t.Fatalf("host thinking metadata = %#v", item.HostMetadata)
		}
		profileOnly := compatibilityModelByID(t, response, "claude", "claude-fable-5")
		if profileOnly.DetectedInHost || profileOnly.Detected.Host || profileOnly.Source != "profile" {
			t.Fatalf("profile-only source = %#v", profileOnly)
		}
		if profileOnly.Classification != compatibilityCodeFix ||
			!compatibilityHasReason(profileOnly, "host_catalog_entry_missing") {
			t.Fatalf("profile-only host catalog fix = %#v", profileOnly)
		}
	})

	t.Run("profile without base candidate needs manual YAML", func(t *testing.T) {
		cfg := pluginConfig{
			BaseModels: map[string]logicalModel{"opus": {Candidates: []candidate{fallback}}},
			Models:     map[string]logicalModel{"opus": {Candidates: []candidate{fallback}}},
		}
		response := collectCompatibilityResponse(cfg, []pluginapi.HostModelListEntry{host}, time.Time{})
		item := compatibilityModelByID(t, response, "claude", "claude-opus-5")
		if item.Classification != compatibilityYAMLFix || item.Detected.Bravo || item.Detected.Route {
			t.Fatalf("item = %#v", item)
		}
		if len(item.RequiredFixes) != 2 || item.RequiredFixes[0] != compatibilityYAMLFix ||
			item.RequiredFixes[1] != compatibilityRouteFix {
			t.Fatalf("required fixes = %#v", item.RequiredFixes)
		}
		if len(item.SuggestedFixes) != 1 || item.SuggestedFixes[0].Kind != compatibilityYAMLFix ||
			item.SuggestedFixes[0].SafeToApplyAutomatically ||
			!strings.Contains(item.SuggestedFixes[0].Snippet, "MANUAL FULL-MAP MERGE ONLY") {
			t.Fatalf("YAML suggestion = %#v", item.SuggestedFixes)
		}
	})

	t.Run("exact alias does not hide missing family route", func(t *testing.T) {
		opus := candidateFromCompatibilityProfile(profile)
		cfg := pluginConfig{
			BaseModels: map[string]logicalModel{
				"opus":          {Candidates: []candidate{opus, fallback}},
				"claude-opus-5": {Candidates: []candidate{opus}},
			},
			Models: map[string]logicalModel{
				"opus":          {Candidates: []candidate{fallback}},
				"claude-opus-5": {Candidates: []candidate{opus}},
			},
		}
		response := collectCompatibilityResponse(cfg, []pluginapi.HostModelListEntry{host}, time.Time{})
		item := compatibilityModelByID(t, response, "claude", "claude-opus-5")
		if item.Classification != compatibilityRouteFix || !item.Detected.Bravo || item.Detected.Route {
			t.Fatalf("item = %#v", item)
		}
		if len(item.RouteIDs) != 1 || item.RouteIDs[0] != "claude-opus-5" {
			t.Fatalf("effective route ids = %#v", item.RouteIDs)
		}
		if len(item.SuggestedFixes) != 1 || item.SuggestedFixes[0].Kind != compatibilityRouteFix ||
			item.SuggestedFixes[0].SafeToApplyAutomatically {
			t.Fatalf("route suggestion = %#v", item.SuggestedFixes)
		}
		var preview putRouteRequest
		if errUnmarshal := json.Unmarshal([]byte(item.SuggestedFixes[0].Snippet), &preview); errUnmarshal != nil {
			t.Fatalf("route suggestion is not JSON: %v", errUnmarshal)
		}
		if !preview.Preview || preview.ID != "opus" || len(preview.Candidates) != 2 ||
			preview.Candidates[0].Model != "claude-opus-5" {
			t.Fatalf("route preview = %#v", preview)
		}
	})

	t.Run("each route suggestion carries its exact reason and target", func(t *testing.T) {
		multiRouteProfile := profile
		multiRouteProfile.RecommendedRoutes = []string{"deep", "opus"}
		opus := candidateFromCompatibilityProfile(multiRouteProfile)
		cfg := pluginConfig{
			BaseModels: map[string]logicalModel{
				"deep": {Candidates: []candidate{opus, fallback}},
				"opus": {Candidates: []candidate{opus, fallback}},
			},
			Models: map[string]logicalModel{
				"deep": {Candidates: []candidate{fallback}},
				"opus": {Candidates: []candidate{fallback}},
			},
		}

		item := classifyCompatibilityModel(cfg, host, true, multiRouteProfile)
		if item.Classification != compatibilityRouteFix || len(item.SuggestedFixes) != 2 {
			t.Fatalf("item = %#v", item)
		}
		for _, suggestion := range item.SuggestedFixes {
			var preview putRouteRequest
			if errUnmarshal := json.Unmarshal([]byte(suggestion.Snippet), &preview); errUnmarshal != nil {
				t.Fatalf("route suggestion is not JSON: %v", errUnmarshal)
			}
			if len(suggestion.ReasonCodes) != 1 ||
				suggestion.ReasonCodes[0] != "bravo_route_assignment_missing" ||
				!strings.Contains(suggestion.Reason, strconv.Quote(preview.ID)) ||
				len(suggestion.Targets) != 1 ||
				suggestion.Targets[0].Kind != "route" ||
				suggestion.Targets[0].Selector != preview.ID {
				t.Fatalf("suggestion %q context = %#v", preview.ID, suggestion)
			}
		}
	})
}

func TestCompatibilityManagementEndpointUsesHostRegistryCallback(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostModelList {
			t.Fatalf("unexpected host method %q", method)
		}
		var request pluginapi.HostModelListRequest
		decodeBravoPayload(t, payload, &request)
		if request.HostCallbackID != "management-callback" {
			t.Fatalf("host callback id = %q, want management-callback", request.HostCallbackID)
		}
		return mustBravoJSON(t, pluginapi.HostModelListResponse{Models: []pluginapi.HostModelListEntry{
			{Provider: "claude", ID: "claude-opus-6", Available: true},
			{Provider: "codex", ID: "codex-auto-review"},
		}}), nil
	})

	status, body := callProjectManagement(t, http.MethodGet, "/v0/management/bravo/compatibility", "")
	if status != http.StatusOK || body["schema_version"] != float64(compatibilitySchemaVersion) ||
		body["fail_closed"] != true {
		t.Fatalf("status/body = %d %#v", status, body)
	}
	models, _ := body["models"].([]any)
	foundUnknown := false
	for _, rawModel := range models {
		model := projectMap(t, rawModel)
		if model["provider"] == "claude" && model["model"] == "claude-opus-6" {
			foundUnknown = model["classification"] == compatibilityCodeFix
		}
		if model["model"] == "codex-auto-review" {
			t.Fatalf("direct-only model was returned: %#v", model)
		}
	}
	if !foundUnknown {
		t.Fatalf("unknown model classification missing from %#v", body["models"])
	}

	rawRegistration, errRegister := registerManagement()
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawRegistration, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var registration managementRegistrationResponse
	if errUnmarshal := json.Unmarshal(env.Result, &registration); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, route := range registration.Routes {
		if route.Method == http.MethodGet && route.Path == "/bravo/compatibility" {
			return
		}
	}
	t.Fatal("authenticated compatibility route is not registered")
}

func TestCompatibilityChecksDefaultOnClaudeThinkingPolicies(t *testing.T) {
	profiles := compatibilityProfiles()
	tests := []struct {
		model            string
		zeroAllowed      bool
		maxDisableLevel  string
		mismatchedPolicy pluginapi.ThinkingSupport
	}{
		{
			model:           "claude-opus-5",
			zeroAllowed:     true,
			maxDisableLevel: "high",
			mismatchedPolicy: pluginapi.ThinkingSupport{
				DefaultOn:       true,
				ZeroAllowed:     true,
				MaxDisableLevel: "xhigh",
			},
		},
		{
			model:       "claude-sonnet-5",
			zeroAllowed: true,
			mismatchedPolicy: pluginapi.ThinkingSupport{
				DefaultOn:   true,
				ZeroAllowed: false,
			},
		},
		{
			model:       "claude-fable-5",
			zeroAllowed: false,
			mismatchedPolicy: pluginapi.ThinkingSupport{
				DefaultOn:   true,
				ZeroAllowed: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			profile := profiles[routeCandidateKey("claude", test.model)]
			if !profile.HasThinkingPolicy || profile.ThinkingZeroAllowed != test.zeroAllowed ||
				profile.MaxDisableLevel != test.maxDisableLevel {
				t.Fatalf("profile = %#v", profile)
			}
			matching := pluginapi.HostModelListEntry{Thinking: &pluginapi.ThinkingSupport{
				DefaultOn:       true,
				ZeroAllowed:     test.zeroAllowed,
				MaxDisableLevel: test.maxDisableLevel,
			}}
			if compatibilityHostThinkingMismatch(matching, profile) {
				t.Fatalf("matching policy was rejected: %#v", matching.Thinking)
			}
			mismatched := pluginapi.HostModelListEntry{Thinking: &test.mismatchedPolicy}
			if !compatibilityHostThinkingMismatch(mismatched, profile) {
				t.Fatalf("mismatched policy was accepted: %#v", mismatched.Thinking)
			}
		})
	}
}

func candidateFromCompatibilityProfile(profile compatibilityProfile) candidate {
	return candidate{
		Provider:     profile.Provider,
		Model:        profile.Model,
		Effort:       profile.DefaultEffort,
		Priority:     100,
		Capabilities: append([]string(nil), profile.Capabilities...),
	}
}

func hostEntryFromCompatibilityProfile(profile compatibilityProfile) pluginapi.HostModelListEntry {
	entry := pluginapi.HostModelListEntry{
		Provider: profile.Provider,
		ID:       profile.Model,
		Catalog:  true,
	}
	if profile.HasThinkingPolicy {
		entry.Thinking = &pluginapi.ThinkingSupport{
			DefaultOn:       profile.ThinkingDefaultOn,
			ZeroAllowed:     profile.ThinkingZeroAllowed,
			MaxDisableLevel: profile.MaxDisableLevel,
		}
	}
	return entry
}

func compatibilityModelByID(
	t *testing.T,
	response compatibilityResponse,
	provider string,
	model string,
) compatibilityModelResponse {
	t.Helper()
	for _, item := range response.Models {
		if item.Provider == provider && item.Model == model {
			return item
		}
	}
	t.Fatalf("compatibility model %s/%s not found", provider, model)
	return compatibilityModelResponse{}
}

func compatibilityHasReason(item compatibilityModelResponse, code string) bool {
	for _, reason := range item.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
