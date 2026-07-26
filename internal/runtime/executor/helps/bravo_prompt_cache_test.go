package helps

import (
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestBravoPromptCacheClaudeTTLRequiresTrustedBravoMetadata(t *testing.T) {
	trusted := map[string]any{
		coreexecutor.AccessProviderMetadataKey: "plugin:bravo:bravo",
		coreexecutor.AccessMetadataMetadataKey: map[string]string{
			"bravo_access_provider":         "bravo",
			"bravo_prompt_cache_claude_ttl": "1h",
		},
	}
	if got := BravoPromptCacheClaudeTTL(trusted); got != "1h" {
		t.Fatalf("trusted TTL = %q, want 1h", got)
	}

	for name, metadata := range map[string]map[string]any{
		"other provider": {
			coreexecutor.AccessProviderMetadataKey: "config-inline",
			coreexecutor.AccessMetadataMetadataKey: trusted[coreexecutor.AccessMetadataMetadataKey],
		},
		"lookalike bravo metadata": {
			coreexecutor.AccessProviderMetadataKey: "plugin:bravo:bravo",
			coreexecutor.AccessMetadataMetadataKey: map[string]string{
				"bravo_access_provider":         "other",
				"bravo_prompt_cache_claude_ttl": "1h",
			},
		},
		"invalid ttl": {
			coreexecutor.AccessProviderMetadataKey: "plugin:bravo:bravo",
			coreexecutor.AccessMetadataMetadataKey: map[string]string{
				"bravo_access_provider":         "bravo",
				"bravo_prompt_cache_claude_ttl": "24h",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BravoPromptCacheClaudeTTL(metadata); got != "" {
				t.Fatalf("untrusted TTL = %q, want empty", got)
			}
		})
	}
}
