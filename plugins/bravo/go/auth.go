package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	bravoAccessProviderMetadataKey = "bravo_access_provider"
	bravoProjectIDMetadataKey      = "bravo_project_id"
	bravoKeyNameMetadataKey        = "bravo_key_name"
	bravoAllowedModelsMetadataKey  = "bravo_allowed_models"
)

func authenticateSmartKey(raw []byte) ([]byte, error) {
	var req pluginapi.FrontendAuthRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	key := requestCredential(req.Headers, req.Query)
	if key == "" {
		return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: false})
	}
	matched, ok := matchSmartKey(loadedConfig(), key)
	if !ok {
		return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: false})
	}
	return okEnvelope(pluginapi.FrontendAuthResponse{
		Authenticated: true,
		Principal:     "bravo:" + matched.ID,
		Metadata: map[string]string{
			bravoAccessProviderMetadataKey: pluginIdentifier,
			bravoProjectIDMetadataKey:      matched.ID,
			bravoKeyNameMetadataKey:        matched.Name,
			bravoAllowedModelsMetadataKey:  strings.Join(matched.Models, ","),
		},
	})
}

func requestCredential(headers http.Header, query map[string][]string) string {
	if headers != nil {
		if authorization := strings.TrimSpace(headers.Get("Authorization")); authorization != "" {
			fields := strings.Fields(authorization)
			if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
				return strings.TrimSpace(fields[1])
			}
			if len(fields) == 1 {
				return strings.TrimSpace(fields[0])
			}
		}
		for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
			if value := strings.TrimSpace(headers.Get(name)); value != "" {
				return value
			}
		}
	}
	for _, name := range []string{"key", "auth_token"} {
		if values := query[name]; len(values) > 0 {
			if value := strings.TrimSpace(values[0]); value != "" {
				return value
			}
		}
	}
	return ""
}

func matchSmartKey(cfg pluginConfig, plaintext string) (smartKeyConfig, bool) {
	sum := sha256.Sum256([]byte(plaintext))
	for _, item := range cfg.SmartKeys {
		if !smartKeyActive(item) {
			continue
		}
		expected, errDecode := hex.DecodeString(item.SHA256)
		if errDecode != nil || len(expected) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(sum[:], expected) == 1 {
			return item, true
		}
	}
	return smartKeyConfig{}, false
}

func smartKeyAllowsModel(key smartKeyConfig, logicalName string) bool {
	if len(key.Models) == 0 {
		return true
	}
	logicalName = strings.ToLower(strings.TrimSpace(logicalName))
	for _, allowed := range key.Models {
		allowed = strings.ToLower(strings.Trim(strings.TrimSpace(allowed), "/"))
		if allowed == "*" || allowed == logicalName {
			return true
		}
	}
	return false
}

func smartKeyFromMetadata(meta map[string]any, cfg pluginConfig) (smartKeyConfig, bool) {
	if len(meta) == 0 {
		return smartKeyConfig{}, false
	}
	access := metadataStringMap(meta["access_metadata"])
	if access[bravoAccessProviderMetadataKey] != pluginIdentifier {
		return smartKeyConfig{}, false
	}
	projectID := strings.TrimSpace(access[bravoProjectIDMetadataKey])
	name := strings.TrimSpace(access[bravoKeyNameMetadataKey])
	for _, item := range cfg.SmartKeys {
		if !smartKeyActive(item) {
			continue
		}
		if projectID != "" && item.ID == projectID {
			return item, true
		}
		if projectID == "" && item.Name == name {
			return item, true
		}
	}
	return smartKeyConfig{}, false
}

func metadataStringMap(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			if text, ok := raw.(string); ok {
				out[key] = text
			}
		}
		return out
	default:
		return nil
	}
}
