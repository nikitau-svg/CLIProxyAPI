package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const compatibilitySchemaVersion = 1

const (
	compatibilitySupported = "supported"
	compatibilityCodeFix   = "code_fix"
	compatibilityYAMLFix   = "yaml_fix"
	compatibilityRouteFix  = "route_fix"
)

type compatibilityResponse struct {
	SchemaVersion int                          `json:"schema_version"`
	GeneratedAt   time.Time                    `json:"generated_at"`
	FailClosed    bool                         `json:"fail_closed"`
	Summary       compatibilitySummary         `json:"summary"`
	Models        []compatibilityModelResponse `json:"models"`
}

type compatibilitySummary struct {
	Total          int `json:"total"`
	Supported      int `json:"supported"`
	CodeFix        int `json:"code_fix"`
	YAMLFix        int `json:"yaml_fix"`
	RouteFix       int `json:"route_fix"`
	ActionRequired int `json:"action_required"`
}

type compatibilityModelResponse struct {
	Provider          string                      `json:"provider"`
	Model             string                      `json:"model"`
	DisplayName       string                      `json:"display_name,omitempty"`
	Source            string                      `json:"source"`
	DetectedInHost    bool                        `json:"detected_in_host"`
	Catalog           bool                        `json:"catalog"`
	Available         bool                        `json:"available"`
	HostMetadata      *compatibilityHostMetadata  `json:"host_metadata,omitempty"`
	Status            string                      `json:"status"`
	Classification    string                      `json:"classification"`
	RequiredFixes     []string                    `json:"required_fixes"`
	Detected          compatibilityDetected       `json:"detected"`
	Reasons           []compatibilityReason       `json:"reasons"`
	Targets           []compatibilityTarget       `json:"targets"`
	SuggestedFixes    []compatibilitySuggestedFix `json:"suggested_fixes"`
	RecommendedRoutes []string                    `json:"recommended_routes"`
	RouteIDs          []string                    `json:"route_ids"`
}

type compatibilityHostMetadata struct {
	Type                      string                         `json:"type,omitempty"`
	SupportedParameters       []string                       `json:"supported_parameters"`
	SupportedInputModalities  []string                       `json:"supported_input_modalities"`
	SupportedOutputModalities []string                       `json:"supported_output_modalities"`
	Thinking                  *compatibilityThinkingMetadata `json:"thinking,omitempty"`
}

type compatibilityThinkingMetadata struct {
	Min             int      `json:"min,omitempty"`
	Max             int      `json:"max,omitempty"`
	ZeroAllowed     bool     `json:"zero_allowed"`
	DynamicAllowed  bool     `json:"dynamic_allowed"`
	Levels          []string `json:"levels"`
	DefaultOn       bool     `json:"default_on"`
	MaxDisableLevel string   `json:"max_disable_level,omitempty"`
}

type compatibilityDetected struct {
	Host      bool `json:"host"`
	Catalog   bool `json:"catalog"`
	Available bool `json:"available"`
	Bravo     bool `json:"bravo"`
	Route     bool `json:"route"`
}

type compatibilityReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type compatibilityTarget struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Selector string `json:"selector,omitempty"`
}

type compatibilitySuggestedFix struct {
	Code                     string                `json:"code"`
	Kind                     string                `json:"kind"`
	Title                    string                `json:"title"`
	Reason                   string                `json:"reason,omitempty"`
	ReasonCodes              []string              `json:"reason_codes,omitempty"`
	Targets                  []compatibilityTarget `json:"targets,omitempty"`
	Format                   string                `json:"format"`
	Snippet                  string                `json:"snippet"`
	SafeToApplyAutomatically bool                  `json:"safe_to_apply_automatically"`
}

func handleCompatibilityManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != "/v0/management/bravo/compatibility" || req.Method != http.MethodGet {
		return nil, nil
	}

	raw, errCall := callHost(pluginabi.MethodHostModelList, pluginapi.HostModelListRequest{
		HostCallbackID: strings.TrimSpace(req.HostCallbackID),
	})
	if errCall != nil {
		return projectHostFailureJSON(errCall)
	}
	var hostModels pluginapi.HostModelListResponse
	if errUnmarshal := json.Unmarshal(raw, &hostModels); errUnmarshal != nil {
		return projectHostFailureJSON(fmt.Errorf("decode host model registry: %w", errUnmarshal))
	}
	response := collectCompatibilityResponse(loadedConfig(), hostModels.Models, time.Now().UTC())
	return managementJSON(http.StatusOK, response)
}

func collectCompatibilityResponse(cfg pluginConfig, hostModels []pluginapi.HostModelListEntry, generatedAt time.Time) compatibilityResponse {
	profiles := compatibilityProfiles()
	models := append([]pluginapi.HostModelListEntry(nil), hostModels...)
	sort.Slice(models, func(i, j int) bool {
		leftProvider := normalizeProvider(models[i].Provider)
		rightProvider := normalizeProvider(models[j].Provider)
		if leftProvider != rightProvider {
			return leftProvider < rightProvider
		}
		leftModel := normalizeCompatibilityModel(models[i].ID)
		rightModel := normalizeCompatibilityModel(models[j].ID)
		if leftModel != rightModel {
			return leftModel < rightModel
		}
		return strings.TrimSpace(models[i].DisplayName) < strings.TrimSpace(models[j].DisplayName)
	})

	type compatibilityInput struct {
		hostModel      pluginapi.HostModelListEntry
		detectedInHost bool
		profile        compatibilityProfile
	}
	inputs := make(map[string]compatibilityInput, len(models)+len(profiles))
	for _, hostModel := range models {
		provider := normalizeProvider(hostModel.Provider)
		model := normalizeCompatibilityModel(hostModel.ID)
		if !compatibilityProviderInScope(provider) || model == "" || compatibilityModelExcluded(provider, model) {
			continue
		}
		key := routeCandidateKey(provider, model)
		if _, duplicate := inputs[key]; duplicate {
			continue
		}
		hostModel.Provider = provider
		hostModel.ID = model
		inputs[key] = compatibilityInput{
			hostModel:      hostModel,
			detectedInHost: true,
			profile:        profiles[key],
		}
	}
	for key, profile := range profiles {
		if _, exists := inputs[key]; exists {
			continue
		}
		inputs[key] = compatibilityInput{
			hostModel: pluginapi.HostModelListEntry{
				Provider: profile.Provider,
				ID:       profile.Model,
			},
			profile: profile,
		}
	}
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	response := compatibilityResponse{
		SchemaVersion: compatibilitySchemaVersion,
		GeneratedAt:   generatedAt,
		FailClosed:    true,
		Models:        make([]compatibilityModelResponse, 0, len(keys)),
	}
	for _, key := range keys {
		input := inputs[key]
		item := classifyCompatibilityModel(cfg, input.hostModel, input.detectedInHost, input.profile)
		response.Models = append(response.Models, item)
		switch item.Classification {
		case compatibilitySupported:
			response.Summary.Supported++
		case compatibilityCodeFix:
			response.Summary.CodeFix++
		case compatibilityYAMLFix:
			response.Summary.YAMLFix++
		case compatibilityRouteFix:
			response.Summary.RouteFix++
		}
	}
	response.Summary.Total = len(response.Models)
	response.Summary.ActionRequired = response.Summary.CodeFix + response.Summary.YAMLFix + response.Summary.RouteFix
	return response
}

func classifyCompatibilityModel(
	cfg pluginConfig,
	hostModel pluginapi.HostModelListEntry,
	detectedInHost bool,
	profile compatibilityProfile,
) compatibilityModelResponse {
	provider := normalizeProvider(hostModel.Provider)
	model := normalizeCompatibilityModel(hostModel.ID)
	key := routeCandidateKey(provider, model)
	baseRoutes := compatibilityRoutesUsing(cfg.BaseModels, key)
	effectiveRoutes := compatibilityRoutesUsing(cfg.Models, key)
	item := compatibilityModelResponse{
		Provider:       provider,
		Model:          model,
		DisplayName:    strings.TrimSpace(hostModel.DisplayName),
		DetectedInHost: detectedInHost,
		Catalog:        hostModel.Catalog,
		Available:      hostModel.Available,
		RequiredFixes:  []string{},
		Detected: compatibilityDetected{
			Host:      detectedInHost,
			Catalog:   hostModel.Catalog,
			Available: hostModel.Available,
			Bravo:     len(baseRoutes) > 0,
		},
		Reasons:        []compatibilityReason{},
		Targets:        []compatibilityTarget{},
		SuggestedFixes: []compatibilitySuggestedFix{},
		RouteIDs:       effectiveRoutes,
	}
	switch {
	case detectedInHost && profile.Model != "":
		item.Source = "host_and_profile"
	case detectedInHost:
		item.Source = "host"
	default:
		item.Source = "profile"
	}
	if detectedInHost {
		item.HostMetadata = compatibilityHostMetadataFromEntry(hostModel)
	}

	if profile.Model == "" {
		item.Classification = compatibilityCodeFix
		item.Status = compatibilityCodeFix
		item.RequiredFixes = []string{compatibilityCodeFix}
		reason := compatibilityReason{
			Code:    "bravo_compatibility_profile_missing",
			Message: fmt.Sprintf("The host registry discovered %s/%s, but Bravo has no reviewed compatibility profile for it.", provider, model),
		}
		targets := []compatibilityTarget{
			compatibilityTarget{Kind: "code", Path: "plugins/bravo/go/compatibility_profiles.go", Selector: provider + "/" + model},
			compatibilityTarget{Kind: "code", Path: "plugins/bravo/go/contract_test.go", Selector: provider + "/" + model},
		}
		item.Reasons = append(item.Reasons, reason)
		item.Targets = append(item.Targets, targets...)
		item.SuggestedFixes = append(
			item.SuggestedFixes,
			withCompatibilitySuggestionContext(
				compatibilityCodeSuggestion(provider, model),
				[]compatibilityReason{reason},
				targets,
			),
		)
		return normalizeCompatibilityItem(item)
	}

	item.RecommendedRoutes = append([]string(nil), profile.RecommendedRoutes...)
	baseCandidate, profileMatches := compatibilityBaseCandidate(cfg.BaseModels, key, profile.Capabilities)
	needsBravoYAML := len(baseRoutes) == 0 || !profileMatches
	needsHostCatalog := !hostModel.Catalog
	needsHostMetadata := hostModel.Catalog && compatibilityHostThinkingMismatch(hostModel, profile)
	needsHostCode := needsHostCatalog || needsHostMetadata
	needsRoute := false
	missingRouteDefinitions := make([]string, 0)
	missingRouteAssignments := make([]string, 0)
	if len(profile.RecommendedRoutes) == 0 {
		needsRoute = len(effectiveRoutes) == 0
		if needsRoute && len(baseRoutes) > 0 {
			missingRouteAssignments = append(missingRouteAssignments, baseRoutes[0])
		}
	} else {
		for _, routeID := range profile.RecommendedRoutes {
			if _, exists := cfg.BaseModels[routeID]; !exists {
				missingRouteDefinitions = append(missingRouteDefinitions, routeID)
				needsBravoYAML = true
				needsRoute = true
				continue
			}
			if !compatibilityRouteUses(cfg.Models[routeID], key) {
				missingRouteAssignments = append(missingRouteAssignments, routeID)
				needsRoute = true
			}
		}
	}
	item.Detected.Route = !needsRoute

	switch {
	case needsHostCode:
		item.Classification = compatibilityCodeFix
	case needsBravoYAML:
		item.Classification = compatibilityYAMLFix
	case needsRoute:
		item.Classification = compatibilityRouteFix
	default:
		item.Classification = compatibilitySupported
	}
	item.Status = item.Classification

	if needsHostCode {
		item.RequiredFixes = append(item.RequiredFixes, compatibilityCodeFix)
		fixReasons := make([]compatibilityReason, 0, 2)
		if needsHostCatalog {
			fixReasons = append(fixReasons, compatibilityReason{
				Code:    "host_catalog_entry_missing",
				Message: fmt.Sprintf("Bravo has reviewed %s/%s, but the host static model catalog does not contain it.", provider, model),
			})
		}
		if needsHostMetadata {
			fixReasons = append(fixReasons, compatibilityReason{
				Code:    "host_thinking_profile_mismatch",
				Message: fmt.Sprintf("The host registry thinking metadata for %s/%s does not match the reviewed Bravo contract.", provider, model),
			})
		}
		target := compatibilityTarget{
			Kind:     "code",
			Path:     "internal/registry/models/models.json",
			Selector: provider + "/" + model,
		}
		item.Reasons = append(item.Reasons, fixReasons...)
		item.Targets = append(item.Targets, target)
		item.SuggestedFixes = append(
			item.SuggestedFixes,
			withCompatibilitySuggestionContext(
				compatibilityHostManifestSuggestion(profile),
				fixReasons,
				[]compatibilityTarget{target},
			),
		)
	}
	if needsBravoYAML {
		item.RequiredFixes = append(item.RequiredFixes, compatibilityYAMLFix)
		fixReasons := make([]compatibilityReason, 0, 1+len(missingRouteDefinitions))
		fixTargets := []compatibilityTarget{{
			Kind:     "yaml",
			Path:     "plugins.configs.bravo.models",
			Selector: provider + "/" + model,
		}}
		switch {
		case len(baseRoutes) == 0:
			fixReasons = append(fixReasons, compatibilityReason{
				Code:    "bravo_candidate_manifest_missing",
				Message: fmt.Sprintf("A reviewed profile exists for %s/%s, but the effective Bravo base model map does not declare this candidate.", provider, model),
			})
		case !profileMatches:
			fixReasons = append(fixReasons, compatibilityReason{
				Code:    "bravo_candidate_profile_mismatch",
				Message: fmt.Sprintf("Bravo declares %s/%s, but its candidate capabilities do not satisfy the reviewed profile.", provider, model),
			})
		}
		for _, routeID := range missingRouteDefinitions {
			fixReasons = append(fixReasons, compatibilityReason{
				Code:    "bravo_route_definition_missing",
				Message: fmt.Sprintf("Recommended logical route %q is absent from the Bravo base model map.", routeID),
			})
			fixTargets = append(fixTargets, compatibilityTarget{
				Kind:     "yaml",
				Path:     "plugins.configs.bravo.models",
				Selector: routeID,
			})
		}
		item.Reasons = append(item.Reasons, fixReasons...)
		item.Targets = append(item.Targets, fixTargets...)
		item.SuggestedFixes = append(
			item.SuggestedFixes,
			withCompatibilitySuggestionContext(
				compatibilityYAMLSuggestion(profile),
				fixReasons,
				fixTargets,
			),
		)
	}
	if needsRoute {
		item.RequiredFixes = append(item.RequiredFixes, compatibilityRouteFix)
		if len(profile.RecommendedRoutes) == 0 {
			item.Reasons = append(item.Reasons, compatibilityReason{
				Code:    "bravo_effective_route_missing",
				Message: fmt.Sprintf("Bravo knows %s/%s, but no effective logical route currently uses it.", provider, model),
			})
		}
		for _, routeID := range missingRouteAssignments {
			reason := compatibilityReason{
				Code:    "bravo_route_assignment_missing",
				Message: fmt.Sprintf("Recommended logical route %q does not currently use %s/%s.", routeID, provider, model),
			}
			target := compatibilityTarget{
				Kind:     "route",
				Path:     "/v0/management/bravo/routes",
				Selector: routeID,
			}
			item.Reasons = append(item.Reasons, reason)
			item.Targets = append(item.Targets, target)
			if profileMatches {
				if suggestion, ok := compatibilityRouteSuggestion(cfg, routeID, baseCandidate, profile); ok {
					item.SuggestedFixes = append(
						item.SuggestedFixes,
						withCompatibilitySuggestionContext(
							suggestion,
							[]compatibilityReason{reason},
							[]compatibilityTarget{target},
						),
					)
				}
			}
		}
	}
	if item.Classification == compatibilitySupported {
		item.Reasons = append(item.Reasons, compatibilityReason{
			Code:    "bravo_model_supported",
			Message: fmt.Sprintf("%s/%s has a reviewed profile, a Bravo base candidate, and the required effective route coverage.", provider, model),
		})
	}
	if !detectedInHost {
		item.Reasons = append(item.Reasons, compatibilityReason{
			Code:    "host_model_not_detected",
			Message: fmt.Sprintf("%s/%s is reviewed by Bravo but is absent from the host catalog and live registry.", provider, model),
		})
	} else if !hostModel.Available {
		item.Reasons = append(item.Reasons, compatibilityReason{
			Code:    "host_model_not_available",
			Message: fmt.Sprintf("%s/%s is supported by the architecture, but no connected subscription currently advertises it.", provider, model),
		})
	}
	return normalizeCompatibilityItem(item)
}

func compatibilityRoutesUsing(models map[string]logicalModel, key string) []string {
	routes := make([]string, 0)
	for routeID, model := range models {
		if compatibilityRouteUses(model, key) {
			routes = append(routes, normalizeRouteID(routeID))
		}
	}
	sort.Strings(routes)
	return routes
}

func compatibilityRouteUses(model logicalModel, key string) bool {
	for _, item := range model.Candidates {
		if routeCandidateKey(item.Provider, item.Model) == key {
			return true
		}
	}
	return false
}

func compatibilityBaseCandidate(
	models map[string]logicalModel,
	key string,
	requiredCapabilities []string,
) (candidate, bool) {
	routeIDs := make([]string, 0, len(models))
	for routeID := range models {
		routeIDs = append(routeIDs, routeID)
	}
	sort.Strings(routeIDs)
	var first candidate
	found := false
	for _, routeID := range routeIDs {
		for _, item := range models[routeID].Candidates {
			if routeCandidateKey(item.Provider, item.Model) != key {
				continue
			}
			if !found {
				first = item
				found = true
			}
			if capabilitySetContains(item.Capabilities, requiredCapabilities) {
				return item, true
			}
		}
	}
	return first, false
}

func capabilitySetContains(actual, required []string) bool {
	actualSet := newCapabilitySet(actual...)
	for _, capability := range required {
		if _, exists := actualSet[capability]; !exists {
			return false
		}
	}
	return true
}

func compatibilityHostMetadataFromEntry(hostModel pluginapi.HostModelListEntry) *compatibilityHostMetadata {
	metadata := &compatibilityHostMetadata{
		Type:                      strings.TrimSpace(hostModel.Type),
		SupportedParameters:       normalizeStrings(hostModel.SupportedParameters),
		SupportedInputModalities:  normalizeStrings(hostModel.SupportedInputModalities),
		SupportedOutputModalities: normalizeStrings(hostModel.SupportedOutputModalities),
	}
	sort.Strings(metadata.SupportedParameters)
	sort.Strings(metadata.SupportedInputModalities)
	sort.Strings(metadata.SupportedOutputModalities)
	if hostModel.Thinking != nil {
		metadata.Thinking = &compatibilityThinkingMetadata{
			Min:             hostModel.Thinking.Min,
			Max:             hostModel.Thinking.Max,
			ZeroAllowed:     hostModel.Thinking.ZeroAllowed,
			DynamicAllowed:  hostModel.Thinking.DynamicAllowed,
			Levels:          normalizeStrings(hostModel.Thinking.Levels),
			DefaultOn:       hostModel.Thinking.DefaultOn,
			MaxDisableLevel: normalizeEffort(hostModel.Thinking.MaxDisableLevel),
		}
		sort.Strings(metadata.Thinking.Levels)
	}
	return metadata
}

func compatibilityHostThinkingMismatch(hostModel pluginapi.HostModelListEntry, profile compatibilityProfile) bool {
	if !profile.HasThinkingPolicy {
		return false
	}
	if hostModel.Thinking == nil {
		return true
	}
	if profile.ThinkingDefaultOn != hostModel.Thinking.DefaultOn {
		return true
	}
	if profile.ThinkingZeroAllowed != hostModel.Thinking.ZeroAllowed {
		return true
	}
	return profile.MaxDisableLevel != normalizeEffort(hostModel.Thinking.MaxDisableLevel)
}

func compatibilityCodeSuggestion(provider, model string) compatibilitySuggestedFix {
	snippet := fmt.Sprintf(
		"// Add only after live provider-contract tests pass.\ncompatibilityProfile{Provider: %s, Model: %s, DefaultEffort: \"<verified-effort>\", Capabilities: []string{\"<verified-capability>\"}, RecommendedRoutes: []string{\"<verified-logical-route>\"}},",
		strconv.Quote(provider),
		strconv.Quote(model),
	)
	return compatibilitySuggestedFix{
		Code:                     "add_compatibility_profile",
		Kind:                     compatibilityCodeFix,
		Title:                    "Add a reviewed compatibility profile and conformance tests",
		Format:                   "go",
		Snippet:                  snippet,
		SafeToApplyAutomatically: false,
	}
}

func compatibilityHostManifestSuggestion(profile compatibilityProfile) compatibilitySuggestedFix {
	payload := map[string]any{
		"id": profile.Model,
	}
	if profile.HasThinkingPolicy {
		payload["thinking"] = map[string]any{
			"default_on":        profile.ThinkingDefaultOn,
			"zero_allowed":      profile.ThinkingZeroAllowed,
			"max_disable_level": profile.MaxDisableLevel,
		}
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	return compatibilitySuggestedFix{
		Code:                     "update_host_registry_manifest",
		Kind:                     compatibilityCodeFix,
		Title:                    "Review and merge the model thinking metadata into the host registry manifest",
		Format:                   "json",
		Snippet:                  string(raw),
		SafeToApplyAutomatically: false,
	}
}

func compatibilityYAMLSuggestion(profile compatibilityProfile) compatibilitySuggestedFix {
	capabilities, _ := json.Marshal(profile.Capabilities)
	routeID := "<logical-route>"
	if len(profile.RecommendedRoutes) > 0 {
		routeID = profile.RecommendedRoutes[0]
	}
	snippet := fmt.Sprintf(
		"# MANUAL FULL-MAP MERGE ONLY. A partial models: value replaces the map.\n# plugins:\n#   configs:\n#     bravo:\n#       models:\n#         %s:\n#           candidates:\n#             - provider: %s\n#               model: %s\n#               effort: %s\n#               capabilities: %s",
		routeID,
		strconv.Quote(profile.Provider),
		strconv.Quote(profile.Model),
		strconv.Quote(profile.DefaultEffort),
		string(capabilities),
	)
	return compatibilitySuggestedFix{
		Code:                     "merge_bravo_candidate",
		Kind:                     compatibilityYAMLFix,
		Title:                    "Manually merge the reviewed candidate into the complete Bravo model map",
		Format:                   "yaml",
		Snippet:                  snippet,
		SafeToApplyAutomatically: false,
	}
}

func compatibilityRouteSuggestion(
	cfg pluginConfig,
	routeID string,
	baseCandidate candidate,
	profile compatibilityProfile,
) (compatibilitySuggestedFix, bool) {
	route, exists := cfg.Models[routeID]
	if !exists || strings.TrimSpace(baseCandidate.Model) == "" || len(route.Candidates) >= maxRouteOverrideCandidates {
		return compatibilitySuggestedFix{}, false
	}
	candidates := make([]routeCandidateInput, 0, len(route.Candidates)+1)
	candidates = append(candidates, routeCandidateInput{
		Provider:     profile.Provider,
		Model:        profile.Model,
		Effort:       firstNonEmpty(profile.DefaultEffort, baseCandidate.Effort),
		Capabilities: append([]string(nil), profile.Capabilities...),
	})
	for _, item := range route.Candidates {
		candidates = append(candidates, routeCandidateInput{
			Provider:     normalizeProvider(item.Provider),
			Model:        normalizeCompatibilityModel(item.Model),
			Effort:       item.Effort,
			Capabilities: append([]string(nil), item.Capabilities...),
		})
	}
	payload := putRouteRequest{
		ID:         routeID,
		Candidates: candidates,
		Preview:    true,
	}
	raw, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return compatibilitySuggestedFix{}, false
	}
	return compatibilitySuggestedFix{
		Code:                     "preview_route_assignment",
		Kind:                     compatibilityRouteFix,
		Title:                    fmt.Sprintf("Preview adding %s/%s to %s", profile.Provider, profile.Model, routeID),
		Format:                   "json",
		Snippet:                  string(raw),
		SafeToApplyAutomatically: false,
	}, true
}

func withCompatibilitySuggestionContext(
	suggestion compatibilitySuggestedFix,
	reasons []compatibilityReason,
	targets []compatibilityTarget,
) compatibilitySuggestedFix {
	messages := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if code := strings.TrimSpace(reason.Code); code != "" {
			suggestion.ReasonCodes = append(suggestion.ReasonCodes, code)
		}
		if message := strings.TrimSpace(reason.Message); message != "" {
			messages = append(messages, message)
		}
	}
	suggestion.Reason = strings.Join(messages, " ")
	suggestion.Targets = append(suggestion.Targets, targets...)
	return suggestion
}

func normalizeCompatibilityItem(item compatibilityModelResponse) compatibilityModelResponse {
	item.RequiredFixes = normalizeFixes(item.RequiredFixes)
	item.RecommendedRoutes = normalizeStrings(item.RecommendedRoutes)
	item.RouteIDs = normalizeStrings(item.RouteIDs)
	sort.Slice(item.Reasons, func(i, j int) bool {
		if item.Reasons[i].Code == item.Reasons[j].Code {
			return item.Reasons[i].Message < item.Reasons[j].Message
		}
		return item.Reasons[i].Code < item.Reasons[j].Code
	})
	sort.Slice(item.Targets, func(i, j int) bool {
		if item.Targets[i].Kind != item.Targets[j].Kind {
			return item.Targets[i].Kind < item.Targets[j].Kind
		}
		if item.Targets[i].Path != item.Targets[j].Path {
			return item.Targets[i].Path < item.Targets[j].Path
		}
		return item.Targets[i].Selector < item.Targets[j].Selector
	})
	item.Targets = deduplicateCompatibilityTargets(item.Targets)
	for index := range item.SuggestedFixes {
		item.SuggestedFixes[index].ReasonCodes = normalizeStrings(item.SuggestedFixes[index].ReasonCodes)
		sort.Slice(item.SuggestedFixes[index].Targets, func(i, j int) bool {
			if item.SuggestedFixes[index].Targets[i].Kind != item.SuggestedFixes[index].Targets[j].Kind {
				return item.SuggestedFixes[index].Targets[i].Kind < item.SuggestedFixes[index].Targets[j].Kind
			}
			if item.SuggestedFixes[index].Targets[i].Path != item.SuggestedFixes[index].Targets[j].Path {
				return item.SuggestedFixes[index].Targets[i].Path < item.SuggestedFixes[index].Targets[j].Path
			}
			return item.SuggestedFixes[index].Targets[i].Selector < item.SuggestedFixes[index].Targets[j].Selector
		})
		item.SuggestedFixes[index].Targets = deduplicateCompatibilityTargets(item.SuggestedFixes[index].Targets)
	}
	sort.Slice(item.SuggestedFixes, func(i, j int) bool {
		left := compatibilityFixRank(item.SuggestedFixes[i].Kind)
		right := compatibilityFixRank(item.SuggestedFixes[j].Kind)
		if left != right {
			return left < right
		}
		return item.SuggestedFixes[i].Title < item.SuggestedFixes[j].Title
	})
	return item
}

func normalizeFixes(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		switch value {
		case compatibilityCodeFix, compatibilityYAMLFix, compatibilityRouteFix:
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for _, value := range []string{compatibilityCodeFix, compatibilityYAMLFix, compatibilityRouteFix} {
		if _, exists := set[value]; exists {
			result = append(result, value)
		}
	}
	return result
}

func compatibilityFixRank(value string) int {
	switch value {
	case compatibilityCodeFix:
		return 0
	case compatibilityYAMLFix:
		return 1
	case compatibilityRouteFix:
		return 2
	default:
		return 3
	}
}

func deduplicateCompatibilityTargets(values []compatibilityTarget) []compatibilityTarget {
	result := make([]compatibilityTarget, 0, len(values))
	var previous compatibilityTarget
	for index, value := range values {
		if index > 0 && value == previous {
			continue
		}
		result = append(result, value)
		previous = value
	}
	return result
}
