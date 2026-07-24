package pluginhost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostModelListCallbackReturnsRedactedProviderSnapshot(t *testing.T) {
	const (
		clientID = "host-model-list-callback-test"
		modelID  = "claude-host-model-list-test"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                        modelID,
		DisplayName:               "Host Model List Test",
		Type:                      "claude",
		SupportedParameters:       []string{"tools", "stream"},
		SupportedInputModalities:  []string{"IMAGE", "TEXT"},
		SupportedOutputModalities: []string{"TEXT"},
		Thinking: &registry.ThinkingSupport{
			Levels:          []string{"high", "low"},
			DefaultOn:       true,
			MaxDisableLevel: "high",
		},
		Config: &registry.ModelConfig{
			OverrideHeader: map[string]string{"Authorization": "must-not-leak"},
		},
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	rawReq, errMarshal := json.Marshal(pluginapi.HostModelListRequest{HostCallbackID: "callback-id"})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	rawResp, errCall := New().callFromPlugin(context.Background(), pluginabi.MethodHostModelList, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelListResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	var found *pluginapi.HostModelListEntry
	for index := range resp.Models {
		if resp.Models[index].Provider == "claude" && resp.Models[index].ID == modelID {
			found = &resp.Models[index]
			break
		}
	}
	if found == nil {
		t.Fatalf("model %q not found in %#v", modelID, resp.Models)
	}
	if found.DisplayName != "Host Model List Test" || found.Type != "claude" {
		t.Fatalf("entry = %#v", found)
	}
	if found.Catalog || !found.Available {
		t.Fatalf("live-only flags = %#v", found)
	}
	if len(found.SupportedParameters) != 2 || found.SupportedParameters[0] != "stream" ||
		found.Thinking == nil || len(found.Thinking.Levels) != 2 || found.Thinking.Levels[0] != "high" ||
		!found.Thinking.DefaultOn || found.Thinking.MaxDisableLevel != "high" {
		t.Fatalf("entry metadata is not deterministic: %#v", found)
	}
	if encoded, errJSON := json.Marshal(found); errJSON != nil {
		t.Fatal(errJSON)
	} else if string(encoded) == "" || containsAny(string(encoded), "Authorization", "must-not-leak", "override_header") {
		t.Fatalf("redacted entry leaked registry transport config: %s", encoded)
	}

	var opus5 *pluginapi.HostModelListEntry
	for index := range resp.Models {
		if resp.Models[index].Provider == "claude" && resp.Models[index].ID == "claude-opus-5" {
			opus5 = &resp.Models[index]
			break
		}
	}
	if opus5 == nil || !opus5.Catalog {
		t.Fatalf("reviewed static Claude catalog entry missing: %#v", opus5)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
