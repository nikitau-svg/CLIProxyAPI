package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyDisabledThinkingPreservesEffortForDefaultOnModel(t *testing.T) {
	model := &registry.ModelInfo{
		ID:   "claude-opus-5",
		Type: "claude",
		Thinking: &registry.ThinkingSupport{
			ZeroAllowed:     true,
			DefaultOn:       true,
			MaxDisableLevel: "high",
			Levels:          []string{"low", "medium", "high", "xhigh", "max"},
		},
	}
	body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`)

	out, err := NewApplier().Apply(body, thinking.ThinkingConfig{Mode: thinking.ModeNone}, model)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "medium" {
		t.Fatalf("output_config.effort = %q, want medium; body=%s", got, out)
	}
}

func TestApplyDisabledThinkingRemovesEffortForOffByDefaultModel(t *testing.T) {
	model := &registry.ModelInfo{
		ID:   "claude-opus-4-8",
		Type: "claude",
		Thinking: &registry.ThinkingSupport{
			ZeroAllowed: true,
			Levels:      []string{"low", "medium", "high", "xhigh", "max"},
		},
	}
	body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)

	out, err := NewApplier().Apply(body, thinking.ThinkingConfig{Mode: thinking.ModeNone}, model)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, out)
	}
	if gjson.GetBytes(out, "output_config.effort").Exists() {
		t.Fatalf("output_config.effort should be removed for off-by-default model; body=%s", out)
	}
}
