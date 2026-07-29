package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	log "github.com/sirupsen/logrus"
)

const maxPluginConfigMutationValueBytes = 1 << 20

func (h *Handler) mutatePluginConfigList(
	ctx context.Context,
	pluginID string,
	req pluginhost.PluginConfigListMutationRequest,
) (pluginhost.PluginConfigListMutationResult, error) {
	if h == nil {
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_persistence_unavailable",
			"management handler is unavailable",
			http.StatusServiceUnavailable,
		)
	}
	pluginID = strings.TrimSpace(pluginID)
	if !pluginhost.ValidatePluginID(pluginID) {
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_invalid_plugin",
			"invalid calling plugin identifier",
			http.StatusBadRequest,
		)
	}
	if errValidate := validatePluginConfigListMutation(req); errValidate != nil {
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_invalid_mutation",
			errValidate.Error(),
			http.StatusBadRequest,
		)
	}

	h.pluginConfigMutationMu.Lock()
	releaseMutationLock := true
	defer func() {
		if releaseMutationLock {
			h.pluginConfigMutationMu.Unlock()
		}
	}()

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_persistence_unavailable",
			"runtime configuration is unavailable",
			http.StatusServiceUnavailable,
		)
	}
	item, exists := h.cfg.Plugins.Configs[pluginID]
	if !exists {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_not_found",
			"plugin configuration was not found",
			http.StatusNotFound,
		)
	}
	body, errBody := pluginConfigJSONObject(item)
	if errBody != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_encode_failed",
			errBody.Error(),
			http.StatusInternalServerError,
		)
	}
	current, errList := pluginConfigListValue(body[req.Field])
	if errList != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_field_not_array",
			errList.Error(),
			http.StatusConflict,
		)
	}
	next, errMutate := applyPluginConfigListMutation(current, req)
	if errMutate != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, errMutate
	}
	if errUnique := validatePluginConfigListUniqueFields(next, req.UniqueFields); errUnique != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, errUnique
	}

	node := pluginConfigNode(item)
	valueNode, errNode := yamlNodeFromJSONValue(next)
	if errNode != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_invalid_value",
			errNode.Error(),
			http.StatusBadRequest,
		)
	}
	setYAMLMappingValue(node, req.Field, valueNode)
	updated, errConfig := pluginInstanceConfigFromNode(node)
	if errConfig != nil {
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_invalid_value",
			errConfig.Error(),
			http.StatusBadRequest,
		)
	}

	h.cfg.Plugins.Configs[pluginID] = updated
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		h.cfg.Plugins.Configs[pluginID] = item
		h.mu.Unlock()
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_save_failed",
			"failed to save plugin configuration",
			http.StatusInternalServerError,
		)
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()

	result, errResult := pluginConfigListMutationResult(next)
	if errResult != nil {
		return pluginhost.PluginConfigListMutationResult{}, pluginConfigMutationFailure(
			"plugin_config_encode_failed",
			errResult.Error(),
			http.StatusInternalServerError,
		)
	}
	reloadCtx := context.Background()
	if ctx != nil {
		reloadCtx = context.WithoutCancel(ctx)
	}
	var finishOnce sync.Once
	finishMutation := func(applyReload bool) {
		finishOnce.Do(func() {
			defer h.pluginConfigMutationMu.Unlock()
			if !applyReload {
				return
			}
			defer func() {
				if recovered := recover(); recovered != nil {
					log.WithField("panic", recovered).Error("management: deferred plugin config reload panicked")
				}
			}()
			h.reloadConfigAfterManagementSave(reloadCtx, snapshot)
		})
	}
	result.AfterPluginCall = func() { finishMutation(true) }
	result.AbortPluginCall = func() { finishMutation(false) }
	releaseMutationLock = false
	return result, nil
}

func validatePluginConfigListMutation(req pluginhost.PluginConfigListMutationRequest) error {
	if !pluginConfigMutationIdentifier(req.Field) {
		return fmt.Errorf("field must contain only letters, digits, underscore, or hyphen")
	}
	switch req.Field {
	case "enabled", "priority", "store":
		return fmt.Errorf("host-owned field %q cannot be mutated through the plugin callback", req.Field)
	}
	operation := strings.ToLower(strings.TrimSpace(req.Operation))
	switch operation {
	case "append", "replace", "delete":
	default:
		return fmt.Errorf("operation must be append, replace, or delete")
	}
	if !pluginConfigMutationIdentifier(req.MatchField) {
		return fmt.Errorf("match_field must contain only letters, digits, underscore, or hyphen")
	}
	if strings.TrimSpace(req.MatchValue) == "" {
		return fmt.Errorf("match_value is required")
	}
	if operation != "delete" {
		if len(req.Value) == 0 {
			return fmt.Errorf("value is required")
		}
		if len(req.Value) > maxPluginConfigMutationValueBytes {
			return fmt.Errorf("value is too large")
		}
		var value map[string]any
		if errUnmarshal := json.Unmarshal(req.Value, &value); errUnmarshal != nil || value == nil {
			return fmt.Errorf("value must be a JSON object")
		}
	}
	for _, field := range req.UniqueFields {
		if !pluginConfigMutationIdentifier(field) {
			return fmt.Errorf("unique_fields contains an invalid field")
		}
	}
	return nil
}

func pluginConfigListValue(value any) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("configured field is not an array")
	}
	return append([]any(nil), items...), nil
}

func applyPluginConfigListMutation(
	current []any,
	req pluginhost.PluginConfigListMutationRequest,
) ([]any, error) {
	operation := strings.ToLower(strings.TrimSpace(req.Operation))
	matchIndex := -1
	for index, rawItem := range current {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, pluginConfigMutationFailure(
				"plugin_config_invalid_list",
				"configured list contains a non-object entry",
				http.StatusConflict,
			)
		}
		if pluginConfigMutationScalar(item[req.MatchField]) == req.MatchValue {
			if matchIndex >= 0 {
				return nil, pluginConfigMutationFailure(
					"plugin_config_ambiguous_match",
					"more than one configured item matches the requested identity",
					http.StatusConflict,
				)
			}
			matchIndex = index
		}
	}

	switch operation {
	case "append":
		if matchIndex >= 0 {
			return nil, pluginConfigMutationFailure(
				"plugin_config_item_exists",
				"configured item already exists",
				http.StatusConflict,
			)
		}
	case "replace", "delete":
		if matchIndex < 0 {
			return nil, pluginConfigMutationFailure(
				"plugin_config_item_not_found",
				"configured item was not found",
				http.StatusNotFound,
			)
		}
	}

	next := append([]any(nil), current...)
	if operation == "delete" {
		return append(next[:matchIndex], next[matchIndex+1:]...), nil
	}
	var value map[string]any
	if errUnmarshal := json.Unmarshal(req.Value, &value); errUnmarshal != nil || value == nil {
		return nil, pluginConfigMutationFailure(
			"plugin_config_invalid_value",
			"value must be a JSON object",
			http.StatusBadRequest,
		)
	}
	if operation == "append" && pluginConfigMutationScalar(value[req.MatchField]) != req.MatchValue {
		return nil, pluginConfigMutationFailure(
			"plugin_config_identity_mismatch",
			"value identity does not match match_value",
			http.StatusConflict,
		)
	}
	if operation == "append" {
		return append(next, value), nil
	}
	next[matchIndex] = value
	return next, nil
}

func validatePluginConfigListUniqueFields(items []any, fields []string) error {
	for _, field := range fields {
		seen := make(map[string]struct{}, len(items))
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok {
				return pluginConfigMutationFailure(
					"plugin_config_invalid_list",
					"configured list contains a non-object entry",
					http.StatusConflict,
				)
			}
			value := pluginConfigMutationScalar(item[field])
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				return pluginConfigMutationFailure(
					"plugin_config_duplicate_value",
					fmt.Sprintf("configured list contains duplicate %s", field),
					http.StatusConflict,
				)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func pluginConfigListMutationResult(items []any) (pluginhost.PluginConfigListMutationResult, error) {
	out := pluginhost.PluginConfigListMutationResult{
		Items: make([]json.RawMessage, 0, len(items)),
	}
	for _, item := range items {
		raw, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			return pluginhost.PluginConfigListMutationResult{}, errMarshal
		}
		out.Items = append(out.Items, raw)
	}
	return out, nil
}

func pluginConfigMutationIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func pluginConfigMutationScalar(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func pluginConfigMutationFailure(code, message string, status int) error {
	return &pluginhost.PluginConfigMutationError{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
	}
}
