package helps

import (
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const bravoPromptCacheClaudeTTLMetadataKey = "bravo_prompt_cache_claude_ttl"

// BravoPromptCacheClaudeTTL returns a validated provider-native TTL only for
// execution metadata produced by Bravo frontend authentication. Plain request
// headers and metadata from every other access provider are ignored.
func BravoPromptCacheClaudeTTL(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	accessProvider, _ := metadata[coreexecutor.AccessProviderMetadataKey].(string)
	if !strings.EqualFold(strings.TrimSpace(accessProvider), "plugin:bravo:bravo") {
		return ""
	}
	accessMetadata := bravoAccessMetadataStrings(metadata[coreexecutor.AccessMetadataMetadataKey])
	if accessMetadata["bravo_access_provider"] != "bravo" {
		return ""
	}
	switch value := strings.ToLower(strings.TrimSpace(accessMetadata[bravoPromptCacheClaudeTTLMetadataKey])); value {
	case "auto", "5m", "1h":
		return value
	default:
		return ""
	}
}

func bravoAccessMetadataStrings(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			text, ok := raw.(string)
			if ok {
				out[key] = text
			}
		}
		return out
	default:
		return nil
	}
}
