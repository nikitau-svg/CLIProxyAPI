package pluginhost

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelExecutionRequestFromPluginPreservesUsageAlias(t *testing.T) {
	got := modelExecutionRequestFromPlugin(pluginapi.HostModelExecutionRequest{
		ForcedProvider: "codex",
		AuthID:         "codex-account-1",
		Model:          "gpt-5.6-sol",
		UsageAlias:     "bravo/opus",
	}, "bravo")

	if got.Model != "gpt-5.6-sol" {
		t.Fatalf("physical model = %q, want gpt-5.6-sol", got.Model)
	}
	if got.UsageAlias != "bravo/opus" {
		t.Fatalf("usage alias = %q, want bravo/opus", got.UsageAlias)
	}
	if got.ForcedProvider != "codex" || got.AuthID != "codex-account-1" {
		t.Fatalf("provider/auth = %q/%q, want codex/codex-account-1", got.ForcedProvider, got.AuthID)
	}
}
