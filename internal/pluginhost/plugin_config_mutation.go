package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// PluginConfigListMutationRequest describes an atomic mutation of one array
// field in a plugin's own persisted configuration.
type PluginConfigListMutationRequest struct {
	Field        string          `json:"field"`
	Operation    string          `json:"operation"`
	MatchField   string          `json:"match_field"`
	MatchValue   string          `json:"match_value"`
	Value        json.RawMessage `json:"value,omitempty"`
	UniqueFields []string        `json:"unique_fields,omitempty"`
}

// PluginConfigListMutationResult returns the complete persisted value of the
// mutated list so the caller can immediately synchronize its runtime state.
type PluginConfigListMutationResult struct {
	Items []json.RawMessage `json:"items"`
}

// PluginConfigListMutator persists an atomic list mutation scoped to pluginID.
type PluginConfigListMutator func(
	context.Context,
	string,
	PluginConfigListMutationRequest,
) (PluginConfigListMutationResult, error)

// PluginConfigMutationError is returned to a plugin as a structured host
// callback error.
type PluginConfigMutationError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *PluginConfigMutationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *PluginConfigMutationError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Code)
}

func (e *PluginConfigMutationError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

type rpcHostPluginConfigListMutationRequest struct {
	HostCallbackID string `json:"host_callback_id"`
	PluginConfigListMutationRequest
}

type pluginManagementMutationContextKey struct{}

func withPluginManagementMutation(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pluginManagementMutationContextKey{}, true)
}

func pluginManagementMutationAllowed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	allowed, _ := ctx.Value(pluginManagementMutationContextKey{}).(bool)
	return allowed
}

// SetPluginConfigListMutator installs the host-owned persistence callback used
// by authenticated plugin Management API handlers.
func (h *Host) SetPluginConfigListMutator(mutator PluginConfigListMutator) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.pluginConfigListMutator = mutator
	h.mu.Unlock()
}

func (h *Host) callHostPluginConfigListMutate(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostPluginConfigListMutationRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, pluginConfigMutationError(
			"plugin_config_invalid_request",
			"decode plugin config list mutation: "+errUnmarshal.Error(),
			http.StatusBadRequest,
		)
	}

	callerPluginID := hostCallbackPluginIDFromContext(ctx)
	callbackCtx, errContext := h.requiredModelCallbackContext(ctx, req.HostCallbackID)
	if errContext != nil || callerPluginID == "" {
		return nil, pluginConfigMutationError(
			"plugin_config_callback_forbidden",
			"plugin config mutation requires a callback owned by the calling plugin",
			http.StatusForbidden,
		)
	}
	if !pluginManagementMutationAllowed(callbackCtx) {
		return nil, pluginConfigMutationError(
			"plugin_config_management_required",
			"plugin config mutation is available only during an authenticated management request",
			http.StatusForbidden,
		)
	}

	h.mu.Lock()
	mutator := h.pluginConfigListMutator
	h.mu.Unlock()
	if mutator == nil {
		return nil, pluginConfigMutationError(
			"plugin_config_persistence_unavailable",
			"plugin config persistence is unavailable",
			http.StatusServiceUnavailable,
		)
	}

	result, errMutate := mutator(callbackCtx, callerPluginID, req.PluginConfigListMutationRequest)
	if errMutate != nil {
		return nil, errMutate
	}
	return marshalRPCResult(result)
}

func pluginConfigMutationError(code, message string, status int) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	return &PluginConfigMutationError{
		Code:       strings.TrimSpace(code),
		Message:    strings.TrimSpace(message),
		HTTPStatus: status,
	}
}
