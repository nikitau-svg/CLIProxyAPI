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
	runPluginMutationAfterCall(t, result, reloads)
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
	runPluginMutationAfterCall(t, result, reloads)
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
	runPluginMutationAfterCall(t, result, reloads)
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

func TestPluginConfigListMutationSerializesThroughPostCallReload(t *testing.T) {
	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg: &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Configs: map[string]config.PluginInstanceConfig{
					"bravo": pluginConfigFromYAML(t, "enabled: true\nsmart_keys: []\n"),
				},
			},
		},
		configFilePath: configPath,
	}
	reloads := make(chan *config.Config, 3)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) {
		h.SetConfig(cfg)
		reloads <- cfg
	})
	mutate := func(operation, id string, value json.RawMessage) (pluginhost.PluginConfigListMutationResult, error) {
		return h.mutatePluginConfigList(context.Background(), "bravo", pluginhost.PluginConfigListMutationRequest{
			Field:        "smart_keys",
			Operation:    operation,
			MatchField:   "id",
			MatchValue:   id,
			Value:        value,
			UniqueFields: []string{"id", "name", "sha256"},
		})
	}
	firstValue := json.RawMessage(`{
		"id":"prj_one",
		"name":"Project One",
		"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}`)
	secondValue := json.RawMessage(`{
		"id":"prj_two",
		"name":"Project Two",
		"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}`)

	first, errFirst := mutate("append", "prj_one", firstValue)
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	assertPluginMutationTransactionLocked(t, h)

	type mutationOutcome struct {
		result pluginhost.PluginConfigListMutationResult
		err    error
	}
	secondStarted := make(chan struct{})
	secondDone := make(chan mutationOutcome, 1)
	go func() {
		close(secondStarted)
		result, errMutate := mutate("append", "prj_two", secondValue)
		secondDone <- mutationOutcome{result: result, err: errMutate}
	}()
	<-secondStarted
	first.AfterPluginCall()
	waitForPluginMutationReload(t, reloads)

	var second mutationOutcome
	select {
	case second = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second mutation did not start after the first reload barrier")
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if len(second.result.Items) != 2 {
		t.Fatalf("second mutation items = %#v, want two projects", second.result.Items)
	}
	assertPluginMutationTransactionLocked(t, h)

	deleteStarted := make(chan struct{})
	deleteDone := make(chan mutationOutcome, 1)
	go func() {
		close(deleteStarted)
		result, errMutate := mutate("delete", "prj_one", nil)
		deleteDone <- mutationOutcome{result: result, err: errMutate}
	}()
	<-deleteStarted
	second.result.AfterPluginCall()
	waitForPluginMutationReload(t, reloads)

	var deleted mutationOutcome
	select {
	case deleted = <-deleteDone:
	case <-time.After(time.Second):
		t.Fatal("delete mutation did not start after the second reload barrier")
	}
	if deleted.err != nil {
		t.Fatal(deleted.err)
	}
	if len(deleted.result.Items) != 1 ||
		!strings.Contains(string(deleted.result.Items[0]), "prj_two") {
		t.Fatalf("delete mutation items = %#v, want only prj_two", deleted.result.Items)
	}
	deleted.result.AfterPluginCall()
	waitForPluginMutationReload(t, reloads)

	persisted, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(persisted), "prj_one") ||
		!strings.Contains(string(persisted), "prj_two") {
		t.Fatalf("final persisted smart keys are stale:\n%s", persisted)
	}
}

func assertPluginMutationTransactionLocked(t *testing.T, h *Handler) {
	t.Helper()
	if h.pluginConfigMutationMu.TryLock() {
		h.pluginConfigMutationMu.Unlock()
		t.Fatal("plugin config mutation transaction unlocked before its post-call reload")
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

func runPluginMutationAfterCall(
	t *testing.T,
	result pluginhost.PluginConfigListMutationResult,
	reloads <-chan *config.Config,
) {
	t.Helper()
	select {
	case <-reloads:
		t.Fatal("plugin config reloaded before the active plugin call returned")
	default:
	}
	if result.AfterPluginCall == nil {
		t.Fatal("plugin config mutation did not return a post-call reload barrier")
	}
	result.AfterPluginCall()
}
