package main

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func registerModels(raw []byte) ([]byte, error) {
	var request pluginapi.ModelRegistrationRequest
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg := loadedConfig()
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)

	models := make([]pluginapi.ModelInfo, 0, len(names))
	for _, name := range names {
		item := cfg.Models[name]
		models = append(models, registeredLogicalModel(cfg.Prefix, name, item, request.HostModels))
	}
	return okEnvelope(pluginapi.ModelRegistrationResponse{
		Provider: pluginIdentifier,
		Models:   models,
	})
}

func registeredLogicalModel(prefix, name string, item logicalModel, snapshots ...[]pluginapi.HostModelListEntry) pluginapi.ModelInfo {
	id := prefix + name
	info := pluginapi.ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy:     pluginIdentifier,
		DisplayName: item.DisplayName,
		Name:        id,
		Version:     pluginVersion,
		Description: item.Description,
		UserDefined: true,
	}
	if logicalModelIsImageOnly(item) {
		info.Type = "openai-image"
		info.SupportedGenerationMethods = []string{"images.generate", "images.edit"}
		info.SupportedParameters = []string{
			"prompt",
			"image",
			"images",
			"mask",
			"n",
			"size",
			"quality",
			"background",
			"output_format",
			"output_compression",
			"input_fidelity",
			"moderation",
		}
		info.SupportedInputModalities = []string{"text", "image"}
		info.SupportedOutputModalities = []string{"image"}
		return info
	}

	info.Type = "smart-router"
	var hostModels []pluginapi.HostModelListEntry
	if len(snapshots) > 0 {
		hostModels = snapshots[0]
	}
	if anchor, ok := logicalModelCapacityAnchor(item, hostModels); ok {
		info.InputTokenLimit = anchor.InputTokenLimit
		info.OutputTokenLimit = anchor.MaxCompletionTokens
		info.ContextLength = anchor.ContextLength
		info.MaxCompletionTokens = anchor.MaxCompletionTokens
	}
	info.SupportedGenerationMethods = []string{"generateContent", "streamGenerateContent"}
	info.SupportedParameters = []string{"stream", "tools", "tool_choice", "reasoning_effort"}
	info.SupportedInputModalities = []string{"text"}
	info.SupportedOutputModalities = []string{"text"}
	info.Thinking = &pluginapi.ThinkingSupport{
		DynamicAllowed: true,
		Levels:         []string{"low", "medium", "high", "xhigh", "max"},
	}
	return info
}

func logicalModelIsImageOnly(item logicalModel) bool {
	if len(item.Candidates) == 0 {
		return false
	}
	for _, modelCandidate := range item.Candidates {
		capabilities := newCapabilitySet(modelCandidate.Capabilities...)
		if _, imageGeneration := capabilities[capabilityImageGeneration]; !imageGeneration {
			return false
		}
		if _, text := capabilities[capabilityText]; text {
			return false
		}
	}
	return true
}

func routeModel(raw []byte) ([]byte, error) {
	var rpcReq rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &rpcReq); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	req := rpcReq.ModelRouteRequest
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "bravo_disabled"})
	}
	_, smartKeyAuthenticated := smartKeyForRoute(req, cfg)
	logicalName, _, ok := resolveLogicalModel(cfg, req.RequestedModel)
	if !ok && smartKeyAuthenticated {
		logicalName, _, ok = resolveUnprefixedLogicalModel(cfg, req.RequestedModel)
	}
	if !ok {
		requested := strings.ToLower(strings.TrimSpace(req.RequestedModel))
		isBravoNamespace := strings.HasPrefix(requested, strings.ToLower(cfg.Prefix))
		if !isBravoNamespace && !smartKeyAuthenticated {
			return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
		}
		// A recognized Bravo project key is itself an authorization boundary.
		// Unknown prefixed models and unknown unprefixed models requested with
		// that key must reach prepareBravoExecution, which returns a fail-closed
		// bravo_model_unknown response. Declining here would let the native host
		// serve a newly discovered model outside the project's model scope,
		// allowed_auth_ids, allocator, analytics, and retry policy.
		reason := "bravo_unknown_prefixed_model"
		if !isBravoNamespace {
			reason = "bravo_project_model_gate"
		}
		return okEnvelope(pluginapi.ModelRouteResponse{
			Handled:    true,
			TargetKind: pluginapi.ModelRouteTargetSelf,
			Reason:     reason,
		})
	}
	// Route a recognized prefixed model to the executor even when the request
	// lacks a valid smart key or no candidate provider is currently available.
	// Model-router errors are treated as "decline" by the host, which would
	// otherwise fall through to native routing and turn a precise 401/403 into
	// a misleading provider 503. prepareBravoExecution is the authoritative
	// smart-key and model-scope gate; the executor reports pool exhaustion after
	// those fail-closed checks pass.
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "bravo_logical_model:" + logicalName,
	})
}

func smartKeyForRoute(req pluginapi.ModelRouteRequest, cfg pluginConfig) (smartKeyConfig, bool) {
	if key, ok := smartKeyFromMetadata(req.Metadata, cfg); ok {
		return key, true
	}
	if plaintext := requestCredential(req.Headers, req.Query); plaintext != "" {
		return matchSmartKey(cfg, plaintext)
	}
	return smartKeyConfig{}, false
}

func resolveLogicalModel(cfg pluginConfig, requested string) (string, logicalModel, bool) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	prefix := strings.ToLower(cfg.Prefix)
	if !strings.HasPrefix(requested, prefix) {
		return "", logicalModel{}, false
	}
	name := strings.Trim(strings.TrimPrefix(requested, prefix), "/")
	item, ok := cfg.Models[name]
	return name, item, ok
}

func resolveUnprefixedLogicalModel(cfg pluginConfig, requested string) (string, logicalModel, bool) {
	name := strings.ToLower(strings.Trim(strings.TrimSpace(requested), "/"))
	item, ok := cfg.Models[name]
	return name, item, ok
}

func pickPinnedAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	// Bravo pins concrete accounts on its nested host calls. It deliberately
	// declines global scheduling so ordinary non-Bravo traffic is untouched.
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return "claude"
	case "openai", "openai-codex":
		return "codex"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}
