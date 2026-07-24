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
	_ = h.resolveCallbackContext(req.HostCallbackID, ctx)

	modelRegistry := registry.GetGlobalRegistry()
	entriesByKey := make(map[string]pluginapi.HostModelListEntry)
	upsert := func(entry pluginapi.HostModelListEntry) {
		entry.Provider = strings.ToLower(strings.TrimSpace(entry.Provider))
		entry.ID = strings.TrimSpace(entry.ID)
		if entry.Provider == "" || entry.ID == "" {
			return
		}
		key := entry.Provider + "\x00" + strings.ToLower(entry.ID)
		current, exists := entriesByKey[key]
		if !exists {
			entriesByKey[key] = entry
			return
		}
		current.Catalog = current.Catalog || entry.Catalog
		current.Available = current.Available || entry.Available
		if current.DisplayName == "" {
			current.DisplayName = entry.DisplayName
		}
		if current.Type == "" {
			current.Type = entry.Type
		}
		if len(current.SupportedParameters) == 0 {
			current.SupportedParameters = append([]string(nil), entry.SupportedParameters...)
		}
		if len(current.SupportedInputModalities) == 0 {
			current.SupportedInputModalities = append([]string(nil), entry.SupportedInputModalities...)
		}
		if len(current.SupportedOutputModalities) == 0 {
			current.SupportedOutputModalities = append([]string(nil), entry.SupportedOutputModalities...)
		}
		if current.Thinking == nil {
			current.Thinking = entry.Thinking
		}
		entriesByKey[key] = current
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
	return marshalRPCResult(pluginapi.HostModelListResponse{Models: entries})
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
