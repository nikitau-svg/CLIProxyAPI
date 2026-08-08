package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const maxRouteOverrideCandidates = 12

var routeMutationMu sync.Mutex

var routeEditableEfforts = []string{"", "low", "medium", "high", "xhigh", "max"}

type routeCandidateInput struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type putRouteRequest struct {
	ID         string                `json:"id"`
	Candidates []routeCandidateInput `json:"candidates"`
	Preview    bool                  `json:"preview,omitempty"`
}

type resetRouteRequest struct {
	ID string `json:"id"`
}

type routeCandidateView struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort,omitempty"`
	Priority     int      `json:"priority"`
	Capabilities []string `json:"capabilities"`
}

type routeView struct {
	ID                string               `json:"id"`
	RequestModel      string               `json:"request_model"`
	DisplayName       string               `json:"display_name"`
	Description       string               `json:"description,omitempty"`
	Overridden        bool                 `json:"overridden"`
	Candidates        []routeCandidateView `json:"candidates"`
	DefaultCandidates []routeCandidateView `json:"default_candidates"`
}

func handleRoutesManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != "/v0/management/bravo/routes" && path != "/v0/management/bravo/routes/reset" {
		return nil, nil
	}
	switch {
	case path == "/v0/management/bravo/routes" && req.Method == http.MethodGet:
		cfg := loadedConfig()
		defaultView := strings.EqualFold(strings.TrimSpace(req.Query.Get("view")), "default") ||
			strings.EqualFold(strings.TrimSpace(req.Query.Get("defaults")), "true")
		return routeListJSON(http.StatusOK, cfg, defaultView, false)
	case path == "/v0/management/bravo/routes" && req.Method == http.MethodPut:
		routeMutationMu.Lock()
		defer routeMutationMu.Unlock()
		return putRouteOverride(req)
	case path == "/v0/management/bravo/routes/reset" && req.Method == http.MethodPost:
		routeMutationMu.Lock()
		defer routeMutationMu.Unlock()
		return resetRouteOverride(req)
	default:
		return nil, nil
	}
}

func putRouteOverride(req rpcManagementRequest) ([]byte, error) {
	var input putRouteRequest
	if failure := decodeRouteManagementBody(req.Body, &input); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	override, errOverride := routeOverrideFromInput(cfg, input)
	if errOverride != nil {
		return routeValidationFailureJSON(errOverride)
	}
	candidateCfg := cfg
	candidateCfg.RouteOverrides = upsertRouteOverride(candidateCfg.RouteOverrides, override)
	if errNormalize := normalizeConfig(&candidateCfg); errNormalize != nil {
		return routeValidationFailureJSON(errNormalize)
	}
	if input.Preview {
		return routeListJSON(http.StatusOK, candidateCfg, false, true)
	}

	items, errPersist := persistRouteOverride(
		req.HostCallbackID,
		override,
		routeOverrideConfigured(cfg, override.ID),
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedRouteOverrides(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return routeListJSON(http.StatusOK, loadedConfig(), false, false)
}

func resetRouteOverride(req rpcManagementRequest) ([]byte, error) {
	var input resetRouteRequest
	if failure := decodeRouteManagementBody(req.Body, &input); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	id := normalizeRequestedRouteID(cfg, input.ID)
	if _, exists := cfg.BaseModels[id]; !exists {
		return routeValidationFailureJSON(fmt.Errorf("unknown route id %q", id))
	}
	if !routeOverrideConfigured(cfg, id) {
		return routeListJSON(http.StatusOK, cfg, false, false)
	}
	raw, errCall := callHost(pluginabi.MethodHostPluginConfigListMutate, hostPluginConfigListMutationRequest{
		HostCallbackID: strings.TrimSpace(req.HostCallbackID),
		Field:          "route_overrides",
		Operation:      "delete",
		MatchField:     "id",
		MatchValue:     id,
		UniqueFields:   []string{"id"},
	})
	if errCall != nil {
		return projectHostFailureJSON(errCall)
	}
	items, errItems := decodeRouteOverrideMutationResult(raw)
	if errItems != nil {
		return projectHostFailureJSON(errItems)
	}
	if errInstall := installPersistedRouteOverrides(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return routeListJSON(http.StatusOK, loadedConfig(), false, false)
}

func routeOverrideFromInput(cfg pluginConfig, input putRouteRequest) (routeOverrideConfig, error) {
	id := normalizeRequestedRouteID(cfg, input.ID)
	if _, exists := cfg.BaseModels[id]; !exists {
		return routeOverrideConfig{}, fmt.Errorf("unknown route id %q", id)
	}
	if len(input.Candidates) == 0 {
		return routeOverrideConfig{}, fmt.Errorf("route %s must keep at least one candidate", id)
	}
	if len(input.Candidates) > maxRouteOverrideCandidates {
		return routeOverrideConfig{}, fmt.Errorf("route %s exceeds the %d-candidate limit", id, maxRouteOverrideCandidates)
	}
	withPriority := 0
	for _, item := range input.Candidates {
		if item.Priority != nil {
			withPriority++
		}
	}
	if withPriority != 0 && withPriority != len(input.Candidates) {
		return routeOverrideConfig{}, fmt.Errorf("route %s must provide priority for every candidate or omit all priorities", id)
	}
	candidates := make([]candidate, 0, len(input.Candidates))
	for index, item := range input.Candidates {
		priority := (len(input.Candidates) - index) * 10
		if item.Priority != nil {
			priority = *item.Priority
		}
		candidates = append(candidates, candidate{
			Provider:     item.Provider,
			Model:        item.Model,
			Effort:       item.Effort,
			Priority:     priority,
			Capabilities: item.Capabilities,
		})
	}
	override := routeOverrideConfig{ID: id, Candidates: candidates}
	return canonicalizeRouteOverride(cfg.BaseModels, override)
}

func normalizeAndApplyRouteOverrides(cfg *pluginConfig) error {
	if cfg == nil {
		return fmt.Errorf("Bravo config is nil")
	}
	normalized := make([]routeOverrideConfig, 0, len(cfg.RouteOverrides))
	seen := make(map[string]struct{}, len(cfg.RouteOverrides))
	for index, item := range cfg.RouteOverrides {
		override, errCanonical := canonicalizeRouteOverride(cfg.BaseModels, item)
		if errCanonical != nil {
			return fmt.Errorf("route_overrides[%d]: %w", index, errCanonical)
		}
		if _, exists := seen[override.ID]; exists {
			return fmt.Errorf("duplicate route override id %q", override.ID)
		}
		seen[override.ID] = struct{}{}
		normalized = append(normalized, override)
		model := cfg.Models[override.ID]
		model.Candidates = cloneCandidates(override.Candidates)
		cfg.Models[override.ID] = model
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	cfg.RouteOverrides = normalized
	return nil
}

func canonicalizeRouteOverride(baseModels map[string]logicalModel, override routeOverrideConfig) (routeOverrideConfig, error) {
	override.ID = normalizeRouteID(override.ID)
	base, exists := baseModels[override.ID]
	if !exists {
		return routeOverrideConfig{}, fmt.Errorf("unknown route id %q", override.ID)
	}
	if len(override.Candidates) == 0 {
		return routeOverrideConfig{}, fmt.Errorf("route %s must keep at least one candidate", override.ID)
	}
	if len(override.Candidates) > maxRouteOverrideCandidates {
		return routeOverrideConfig{}, fmt.Errorf("route %s exceeds the %d-candidate limit", override.ID, maxRouteOverrideCandidates)
	}
	catalog := routeCandidateCatalog(baseModels)
	requiredCapability := routeClassCapability(base)
	seenCandidates := make(map[string]struct{}, len(override.Candidates))
	allZeroPriority := true
	for _, item := range override.Candidates {
		if item.Priority != 0 {
			allZeroPriority = false
			break
		}
	}
	for index := range override.Candidates {
		item := &override.Candidates[index]
		if allZeroPriority {
			item.Priority = (len(override.Candidates) - index) * 10
		}
		if item.Priority <= 0 || item.Priority > 1_000_000 {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d priority must be in [1,1000000]", override.ID, index)
		}
		if index > 0 && override.Candidates[index-1].Priority <= item.Priority {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate priorities must be strictly descending and match their order", override.ID)
		}
		if len(item.AuthIDs) > 0 {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d cannot override auth_ids", override.ID, index)
		}
		provider := normalizeProvider(item.Provider)
		model := strings.ToLower(strings.TrimSpace(item.Model))
		key := routeCandidateKey(provider, model)
		canonical, known := catalog[key]
		if !known {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d uses unverified provider/model %s/%s", override.ID, index, provider, model)
		}
		if _, duplicate := seenCandidates[key]; duplicate {
			return routeOverrideConfig{}, fmt.Errorf("route %s repeats provider/model %s/%s", override.ID, provider, model)
		}
		seenCandidates[key] = struct{}{}

		effort := normalizeEffort(item.Effort)
		if !editableRouteEffort(effort) {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d has unsupported editable effort %q", override.ID, index, effort)
		}
		canonical.Provider = provider
		canonical.Model = model
		canonical.Priority = item.Priority
		canonical.Effort = effort
		canonical.AuthIDs = nil
		if effort != "" {
			resolved, errEffort := resolveCandidateEffort(canonical, requestEffort{Value: effort, Specified: true})
			if errEffort != nil || normalizeEffort(resolved.Effort) != effort {
				return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d effort %q is not verified for %s/%s", override.ID, index, effort, provider, model)
			}
		}

		verifiedCaps := newCapabilitySet(canonical.Capabilities...)
		requestedCaps := item.Capabilities
		if requestedCaps == nil {
			requestedCaps = append([]string(nil), canonical.Capabilities...)
		} else {
			requestedCaps = normalizeStrings(requestedCaps)
			if len(requestedCaps) == 0 {
				return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d cannot declare an empty capability set", override.ID, index)
			}
			for _, capability := range requestedCaps {
				if _, verified := verifiedCaps[capability]; !verified {
					return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d cannot promote unverified capability %q", override.ID, index, capability)
				}
			}
		}
		if _, compatible := newCapabilitySet(requestedCaps...)[requiredCapability]; !compatible {
			return routeOverrideConfig{}, fmt.Errorf("route %s candidate %d must preserve %s", override.ID, index, requiredCapability)
		}
		canonical.Capabilities = requestedCaps
		*item = canonical
	}
	return override, nil
}

func routeCandidateCatalog(models map[string]logicalModel) map[string]candidate {
	catalog := make(map[string]candidate)
	for _, model := range models {
		for _, item := range model.Candidates {
			provider := normalizeProvider(item.Provider)
			modelName := strings.ToLower(strings.TrimSpace(item.Model))
			key := routeCandidateKey(provider, modelName)
			if _, exists := catalog[key]; exists {
				continue
			}
			item.Provider = provider
			item.Model = modelName
			item.Capabilities = normalizeStrings(item.Capabilities)
			item.AuthIDs = nil
			catalog[key] = item
		}
	}
	return catalog
}

func routeClassCapability(model logicalModel) string {
	for _, item := range model.Candidates {
		if _, image := newCapabilitySet(item.Capabilities...)[capabilityImageGeneration]; image {
			return capabilityImageGeneration
		}
	}
	return capabilityText
}

func editableRouteEffort(value string) bool {
	for _, allowed := range routeEditableEfforts {
		if value == allowed {
			return true
		}
	}
	return false
}

func normalizeRouteID(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
}

func normalizeRequestedRouteID(cfg pluginConfig, value string) string {
	id := normalizeRouteID(value)
	prefix := normalizeRouteID(cfg.Prefix)
	if prefix != "" {
		id = strings.TrimPrefix(id, prefix+"/")
	}
	return id
}

func routeCandidateKey(provider, model string) string {
	return normalizeProvider(provider) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func upsertRouteOverride(items []routeOverrideConfig, value routeOverrideConfig) []routeOverrideConfig {
	out := make([]routeOverrideConfig, 0, len(items)+1)
	replaced := false
	for _, item := range items {
		if normalizeRouteID(item.ID) == value.ID {
			out = append(out, value)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, value)
	}
	return out
}

func routeOverrideConfigured(cfg pluginConfig, id string) bool {
	id = normalizeRouteID(id)
	for _, item := range cfg.RouteOverrides {
		if normalizeRouteID(item.ID) == id {
			return true
		}
	}
	return false
}

func persistRouteOverride(hostCallbackID string, value routeOverrideConfig, replace bool) ([]routeOverrideConfig, error) {
	rawValue, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	operation := "append"
	if replace {
		operation = "replace"
	}
	raw, errCall := callHost(pluginabi.MethodHostPluginConfigListMutate, hostPluginConfigListMutationRequest{
		HostCallbackID: strings.TrimSpace(hostCallbackID),
		Field:          "route_overrides",
		Operation:      operation,
		MatchField:     "id",
		MatchValue:     value.ID,
		Value:          rawValue,
		UniqueFields:   []string{"id"},
	})
	if errCall != nil {
		return nil, errCall
	}
	return decodeRouteOverrideMutationResult(raw)
}

func decodeRouteOverrideMutationResult(raw json.RawMessage) ([]routeOverrideConfig, error) {
	var response hostPluginConfigListMutationResult
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode persisted route overrides: %w", errUnmarshal)
	}
	items := make([]routeOverrideConfig, 0, len(response.Items))
	for index, rawItem := range response.Items {
		var item routeOverrideConfig
		if errUnmarshal := json.Unmarshal(rawItem, &item); errUnmarshal != nil {
			return nil, fmt.Errorf("decode persisted route override %d: %w", index, errUnmarshal)
		}
		items = append(items, item)
	}
	return items, nil
}

func installPersistedRouteOverrides(items []routeOverrideConfig) error {
	cfg := loadedConfig()
	cfg.RouteOverrides = append([]routeOverrideConfig(nil), items...)
	cfg.Models = cloneLogicalModels(cfg.BaseModels)
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		return fmt.Errorf("validate persisted route overrides: %w", errNormalize)
	}
	currentConfig.Store(cfg)
	resetAdaptiveStatusRuntime()
	return nil
}

func routeListJSON(status int, cfg pluginConfig, defaultsOnly, preview bool) ([]byte, error) {
	models := cfg.Models
	if defaultsOnly {
		models = cfg.BaseModels
	}
	overridden := make(map[string]struct{}, len(cfg.RouteOverrides))
	for _, item := range cfg.RouteOverrides {
		overridden[item.ID] = struct{}{}
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	routes := make([]routeView, 0, len(names))
	for _, name := range names {
		model := models[name]
		base := cfg.BaseModels[name]
		_, hasOverride := overridden[name]
		if defaultsOnly {
			hasOverride = false
		}
		routes = append(routes, routeView{
			ID:                name,
			RequestModel:      cfg.Prefix + name,
			DisplayName:       model.DisplayName,
			Description:       model.Description,
			Overridden:        hasOverride,
			Candidates:        routeCandidateViews(model.Candidates),
			DefaultCandidates: routeCandidateViews(base.Candidates),
		})
	}
	return managementJSON(status, map[string]any{
		"routes":  routes,
		"efforts": append([]string(nil), routeEditableEfforts...),
		"view":    map[bool]string{true: "default", false: "effective"}[defaultsOnly],
		"preview": preview,
	})
}

func routeCandidateViews(items []candidate) []routeCandidateView {
	out := make([]routeCandidateView, 0, len(items))
	for _, item := range items {
		out = append(out, routeCandidateView{
			Provider:     normalizeProvider(item.Provider),
			Model:        item.Model,
			Effort:       item.Effort,
			Priority:     item.Priority,
			Capabilities: append([]string(nil), item.Capabilities...),
		})
	}
	return out
}

func cloneLogicalModels(models map[string]logicalModel) map[string]logicalModel {
	out := make(map[string]logicalModel, len(models))
	for name, model := range models {
		model.Candidates = cloneCandidates(model.Candidates)
		out[name] = model
	}
	return out
}

func cloneCandidates(items []candidate) []candidate {
	out := make([]candidate, len(items))
	for index, item := range items {
		item.Capabilities = append([]string(nil), item.Capabilities...)
		item.AuthIDs = append([]string(nil), item.AuthIDs...)
		out[index] = item
	}
	return out
}

func decodeRouteManagementBody(body []byte, value any) *projectFailure {
	if len(bytes.TrimSpace(body)) == 0 {
		return &projectFailure{Code: "bravo_route_body_required", Message: "A JSON request body is required.", Status: http.StatusBadRequest}
	}
	if len(body) > maxProjectManagementBodyBytes {
		return &projectFailure{Code: "bravo_route_body_too_large", Message: "The request body is too large.", Status: http.StatusRequestEntityTooLarge}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(value); errDecode != nil {
		return &projectFailure{Code: "bravo_route_body_invalid", Message: "The request body must be a valid JSON object with known fields.", Status: http.StatusBadRequest}
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return &projectFailure{Code: "bravo_route_body_invalid", Message: "The request body must contain exactly one JSON object.", Status: http.StatusBadRequest}
	}
	return nil
}

func routeValidationFailureJSON(err error) ([]byte, error) {
	message := "Invalid Bravo route override."
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	return projectFailureJSON(projectFailure{
		Code:    "bravo_route_invalid",
		Message: message,
		Status:  http.StatusBadRequest,
	})
}
