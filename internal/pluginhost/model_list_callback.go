package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (h *Host) callHostModelList(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelListRequest
	if len(bytesTrimSpace(request)) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host model list request: %w", errUnmarshal)
		}
	}
	if _, errContext := h.requiredModelCallbackContext(ctx, req.HostCallbackID); errContext != nil {
		return nil, errContext
	}
	return marshalRPCResult(pluginapi.HostModelListResponse{Models: h.hostModelListSnapshot("")})
}

func (h *Host) hostModelListSnapshot(excludedProvider string) []pluginapi.HostModelListEntry {
	modelRegistry := registry.GetGlobalRegistry()
	excludedProvider = strings.ToLower(strings.TrimSpace(excludedProvider))
	entriesByKey := make(map[string]pluginapi.HostModelListEntry)
	upsert := func(entry pluginapi.HostModelListEntry) {
		entry.Provider = strings.ToLower(strings.TrimSpace(entry.Provider))
		entry.ID = strings.TrimSpace(entry.ID)
		if entry.Provider == "" || entry.ID == "" || entry.Provider == excludedProvider {
			return
		}
		key := entry.Provider + "\x00" + strings.ToLower(entry.ID)
		current, exists := entriesByKey[key]
		if !exists {
			entriesByKey[key] = entry
			return
		}
		entriesByKey[key] = mergeHostModelListEntry(current, entry)
	}

	for _, info := range registry.GetClaudeModels() {
		upsert(hostModelListEntry("claude", info, true, false))
	}
	codexCatalogs := [][]*registry.ModelInfo{
		registry.GetCodexFreeModels(),
		registry.GetCodexTeamModels(),
		registry.GetCodexPlusModels(),
		registry.GetCodexProModels(),
	}
	for _, catalog := range codexCatalogs {
		for _, info := range catalog {
			upsert(hostModelListEntry("codex", info, true, false))
		}
	}

	available := modelRegistry.GetAvailableModels("openai")
	for _, item := range available {
		modelID, _ := item["id"].(string)
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		for _, provider := range modelRegistry.GetModelProviders(modelID) {
			provider = strings.ToLower(strings.TrimSpace(provider))
			if provider == "" {
				continue
			}
			info := modelRegistry.GetModelInfo(modelID, provider)
			if info == nil {
				continue
			}
			upsert(hostModelListEntry(provider, info, false, true))
		}
	}

	entries := make([]pluginapi.HostModelListEntry, 0, len(entriesByKey))
	for _, entry := range entriesByKey {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider == entries[j].Provider {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Provider < entries[j].Provider
	})
	return entries
}

func mergeHostModelListEntry(current, incoming pluginapi.HostModelListEntry) pluginapi.HostModelListEntry {
	// A reviewed catalog entry owns descriptive metadata. Live metadata only
	// fills catalog gaps. If two reviewed sources disagree, preserve the lower
	// non-zero capacity rather than advertising an unverified larger window.
	if incoming.Catalog && !current.Catalog {
		current, incoming = incoming, current
	}
	bothReviewed := current.Catalog && incoming.Catalog
	current.Catalog = current.Catalog || incoming.Catalog
	current.Available = current.Available || incoming.Available
	if current.DisplayName == "" {
		current.DisplayName = incoming.DisplayName
	}
	if current.Type == "" {
		current.Type = incoming.Type
	}
	if len(current.SupportedParameters) == 0 {
		current.SupportedParameters = append([]string(nil), incoming.SupportedParameters...)
	}
	if len(current.SupportedInputModalities) == 0 {
		current.SupportedInputModalities = append([]string(nil), incoming.SupportedInputModalities...)
	}
	if len(current.SupportedOutputModalities) == 0 {
		current.SupportedOutputModalities = append([]string(nil), incoming.SupportedOutputModalities...)
	}
	if current.Thinking == nil {
		current.Thinking = incoming.Thinking
	}
	current.InputTokenLimit = mergeReviewedModelLimit(current.InputTokenLimit, incoming.InputTokenLimit, bothReviewed)
	current.ContextLength = mergeReviewedModelLimit(current.ContextLength, incoming.ContextLength, bothReviewed)
	current.MaxCompletionTokens = mergeReviewedModelLimit(current.MaxCompletionTokens, incoming.MaxCompletionTokens, bothReviewed)
	return current
}

func mergeReviewedModelLimit(current, incoming int64, bothReviewed bool) int64 {
	if current <= 0 {
		if incoming > 0 {
			return incoming
		}
		return 0
	}
	if incoming <= 0 || !bothReviewed {
		return current
	}
	if incoming < current {
		return incoming
	}
	return current
}

func hostModelListEntry(provider string, info *registry.ModelInfo, catalog, available bool) pluginapi.HostModelListEntry {
	if info == nil {
		return pluginapi.HostModelListEntry{}
	}
	entry := pluginapi.HostModelListEntry{
		Provider:                  provider,
		ID:                        strings.TrimSpace(info.ID),
		DisplayName:               strings.TrimSpace(info.DisplayName),
		Type:                      strings.TrimSpace(info.Type),
		InputTokenLimit:           positiveModelLimit(info.InputTokenLimit),
		ContextLength:             positiveModelLimit(info.ContextLength),
		MaxCompletionTokens:       positiveModelLimit(info.MaxCompletionTokens),
		SupportedParameters:       append([]string(nil), info.SupportedParameters...),
		SupportedInputModalities:  append([]string(nil), info.SupportedInputModalities...),
		SupportedOutputModalities: append([]string(nil), info.SupportedOutputModalities...),
		Catalog:                   catalog,
		Available:                 available,
	}
	sort.Strings(entry.SupportedParameters)
	sort.Strings(entry.SupportedInputModalities)
	sort.Strings(entry.SupportedOutputModalities)
	if info.Thinking != nil {
		entry.Thinking = &pluginapi.ThinkingSupport{
			Min:            info.Thinking.Min,
			Max:            info.Thinking.Max,
			ZeroAllowed:    info.Thinking.ZeroAllowed,
			DynamicAllowed: info.Thinking.DynamicAllowed,
			Levels:         append([]string(nil), info.Thinking.Levels...),
			DefaultOn:      info.Thinking.DefaultOn,
			MaxDisableLevel: strings.TrimSpace(
				info.Thinking.MaxDisableLevel,
			),
		}
		sort.Strings(entry.Thinking.Levels)
	}
	return entry
}

func positiveModelLimit(value int) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value)
}
