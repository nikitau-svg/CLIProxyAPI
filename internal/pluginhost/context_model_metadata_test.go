package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostModelListEntryCarriesReviewedContextLimits(t *testing.T) {
	entry := hostModelListEntry("claude", &registry.ModelInfo{
		ID:                  "claude-context-test",
		InputTokenLimit:     900000,
		ContextLength:       1000000,
		MaxCompletionTokens: 100000,
	}, true, false)

	if entry.InputTokenLimit != 900000 || entry.ContextLength != 1000000 ||
		entry.MaxCompletionTokens != 100000 {
		t.Fatalf("context limits = %#v, want 900000/1000000/100000", entry)
	}
}

func TestModelRegistrationReceivesRedactedHostModelSnapshot(t *testing.T) {
	modelRegistry := newFakeModelRegistry()
	var snapshot []pluginapi.HostModelListEntry
	host := newHostWithRecords(capabilityRecord{
		id:   "bravo",
		meta: pluginapi.Metadata{Name: "Bravo", Version: "test"},
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ModelRegistrar: modelRegistrarFunc(func(_ context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
				snapshot = append([]pluginapi.HostModelListEntry(nil), req.HostModels...)
				return pluginapi.ModelRegistrationResponse{}, nil
			}),
		}},
	})

	host.RegisterModels(context.Background(), modelRegistry)

	var opus *pluginapi.HostModelListEntry
	for index := range snapshot {
		if snapshot[index].Provider == "claude" && snapshot[index].ID == "claude-opus-5" {
			opus = &snapshot[index]
			break
		}
	}
	if opus == nil {
		t.Fatalf("registration snapshot does not contain Claude Opus 5: %#v", snapshot)
	}
	if !opus.Catalog || opus.ContextLength != 1000000 || opus.MaxCompletionTokens != 128000 {
		t.Fatalf("Claude Opus 5 registration metadata = %#v", opus)
	}
	for _, entry := range snapshot {
		if entry.Provider == "bravo" {
			t.Fatalf("registration snapshot recursively contains Bravo model: %#v", entry)
		}
	}
}

func TestHostModelLimitMergeUsesConservativeReviewedValue(t *testing.T) {
	current := pluginapi.HostModelListEntry{
		Provider:            "claude",
		ID:                  "claude-conflict",
		ContextLength:       1000000,
		MaxCompletionTokens: 128000,
		Catalog:             true,
	}
	incoming := pluginapi.HostModelListEntry{
		Provider:            "claude",
		ID:                  "claude-conflict",
		ContextLength:       200000,
		MaxCompletionTokens: 64000,
		Catalog:             true,
		Available:           true,
	}

	merged := mergeHostModelListEntry(current, incoming)
	if merged.ContextLength != 200000 || merged.MaxCompletionTokens != 64000 || !merged.Available {
		t.Fatalf("merged limits = %#v, want conservative reviewed values", merged)
	}
}
