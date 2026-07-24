package management

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
)

func TestPluginConfigListMutationPersistsAndHotReloads(t *testing.T) {
	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Configs: map[string]config.PluginInstanceConfig{
					"bravo": pluginConfigFromYAML(t, "enabled: true\nkeep: retained\nsmart_keys: []\n"),
				},
			},
		},
		configFilePath: configPath,
	}
	reloads := make(chan *config.Config, 3)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) {
		reloads <- cfg
	})

	created := json.RawMessage(`{
		"id":"prj_one",
		"name":"Project One",
		"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"models":["*"]
	}`)
	result, errCreate := h.mutatePluginConfigList(context.Background(), "bravo", pluginhost.PluginConfigListMutationRequest{
		Field:        "smart_keys",
		Operation:    "append",
		MatchField:   "id",
		MatchValue:   "prj_one",
		Value:        created,
		UniqueFields: []string{"id", "name", "sha256"},
	})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if len(result.Items) != 1 {
		t.Fatalf("created result = %#v", result)
	}
	waitForPluginMutationReload(t, reloads)
	persisted, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	text := string(persisted)
	if !strings.Contains(text, "prj_one") ||
		!strings.Contains(text, "keep: retained") ||
		!strings.Contains(text, strings.Repeat("a", 64)) {
		t.Fatalf("persisted config is incomplete:\n%s", text)
	}

	replaced := json.RawMessage(`{
		"id":"prj_one",
		"name":"Renamed",
		"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"models":["frontier"]
	}`)
	result, errReplace := h.mutatePluginConfigList(context.Background(), "bravo", pluginhost.PluginConfigListMutationRequest{
		Field:        "smart_keys",
		Operation:    "replace",
		MatchField:   "id",
		MatchValue:   "prj_one",
		Value:        replaced,
		UniqueFields: []string{"id", "name", "sha256"},
	})
	if errReplace != nil {
		t.Fatal(errReplace)
	}
	if len(result.Items) != 1 || !strings.Contains(string(result.Items[0]), "Renamed") {
		t.Fatalf("replace result = %#v", result)
	}
	waitForPluginMutationReload(t, reloads)

	result, errDelete := h.mutatePluginConfigList(context.Background(), "bravo", pluginhost.PluginConfigListMutationRequest{
		Field:      "smart_keys",
		Operation:  "delete",
		MatchField: "id",
		MatchValue: "prj_one",
	})
	if errDelete != nil {
		t.Fatal(errDelete)
	}
	if len(result.Items) != 0 {
		t.Fatalf("delete result = %#v", result)
	}
	waitForPluginMutationReload(t, reloads)
}

func TestPluginConfigListMutationRejectsDuplicateUniqueValue(t *testing.T) {
	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Configs: map[string]config.PluginInstanceConfig{
					"bravo": pluginConfigFromYAML(t, `enabled: true
smart_keys:
  - id: prj_one
    name: Same
    sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`),
				},
			},
		},
		configFilePath: configPath,
	}
	_, errMutate := h.mutatePluginConfigList(context.Background(), "bravo", pluginhost.PluginConfigListMutationRequest{
		Field:      "smart_keys",
		Operation:  "append",
		MatchField: "id",
		MatchValue: "prj_two",
		Value: json.RawMessage(`{
			"id":"prj_two",
			"name":"Same",
			"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}`),
		UniqueFields: []string{"id", "name", "sha256"},
	})
	mutationErr, ok := errMutate.(*pluginhost.PluginConfigMutationError)
	if !ok || mutationErr.Code != "plugin_config_duplicate_value" {
		t.Fatalf("error = %#v", errMutate)
	}
}

func waitForPluginMutationReload(t *testing.T, reloads <-chan *config.Config) {
	t.Helper()
	select {
	case <-reloads:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for plugin config reload")
	}
}
