package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", http.StatusBadRequest))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), http.StatusInternalServerError))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	flushUsageState()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return registerModels()
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodFrontendAuthAuthenticate:
		return authenticateSmartKey(request)
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodSchedulerPick:
		return pickPinnedAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return countTokens(request)
	case pluginabi.MethodManagementRegister:
		return registerManagement()
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodUsageHandle:
		return handleUsageRecord(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Bravo Smart Router",
			Version:          pluginVersion,
			Author:           "CLIProxyAPI Bravo",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable Bravo logical models and execution."},
				{Name: "prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Client-visible logical model prefix."},
				{Name: "require_smart_key", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Require a Bravo smart key for logical models."},
				{Name: "max_attempts", Type: pluginapi.ConfigFieldTypeInteger, Description: "Global provider-call budget. Zero means every eligible configured account."},
				{Name: "cooldown_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Fallback cooldown when Retry-After is absent."},
				{Name: "fallback_hedge_delay_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Delay before one cross-provider streaming-bootstrap hedge. Zero disables hedging."},
				{Name: "state_path", Type: pluginapi.ConfigFieldTypeString, Description: "Private persistent Bravo usage and quota snapshot."},
				{Name: "allocator_mode", Type: pluginapi.ConfigFieldTypeString, Description: "Allocator mode: off, observe, or enforce."},
				{Name: "quota_refresh_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Confirmed quota cache lifetime."},
				{Name: "unknown_secondary_policy", Type: pluginapi.ConfigFieldTypeString, Description: "Unknown secondary quota policy: block or allow."},
				{Name: "tariffs", Type: pluginapi.ConfigFieldTypeArray, Description: "Allocator tariff floors and reservation policy."},
				{Name: "subscriptions", Type: pluginapi.ConfigFieldTypeArray, Description: "Auth-index subscription policy."},
				{Name: "smart_keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Smart key SHA-256 records; plaintext keys are never stored."},
				{Name: "route_overrides", Type: pluginapi.ConfigFieldTypeArray, Description: "Validated provider/model route overrides managed through the Bravo API."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeObject, Description: "Logical models and ordered provider equivalents."},
			},
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			FrontendAuthProvider:  true,
			ModelRouter:           true,
			Scheduler:             false,
			UsagePlugin:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude", "openai-image"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude", "openai-image"},
			ManagementAPI:         true,
		},
	}
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, status int) []byte {
	return detailedErrorEnvelope(envelopeError{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
	})
}

func detailedErrorEnvelope(detail envelopeError) []byte {
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &detail,
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

type hostCallFunc func(method string, payload any) (json.RawMessage, error)

var hostCall hostCallFunc = callHostABI

func callHost(method string, payload any) (json.RawMessage, error) {
	return hostCall(method, payload)
}

func callHostABI(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, &hostCallError{
			Code:       "host_callback_empty",
			Message:    fmt.Sprintf("host callback %s returned no response, code=%d", method, int(callCode)),
			Retryable:  true,
			HTTPStatus: http.StatusBadGateway,
		}
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostCallError{
				Code:       env.Error.Code,
				Message:    env.Error.Message,
				Retryable:  env.Error.Retryable,
				HTTPStatus: env.Error.HTTPStatus,
				Headers:    cloneHeader(env.Error.Headers),
				RetryAfter: strings.TrimSpace(env.Error.RetryAfter),
			}
		}
		return nil, &hostCallError{Code: "host_callback_failed", Message: "host callback failed", HTTPStatus: http.StatusBadGateway}
	}
	if callCode != 0 {
		return nil, &hostCallError{
			Code:       "host_callback_code",
			Message:    fmt.Sprintf("host callback %s returned code=%d", method, int(callCode)),
			Retryable:  true,
			HTTPStatus: http.StatusBadGateway,
		}
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func cloneHeader(source http.Header) http.Header {
	if source == nil {
		return nil
	}
	out := make(http.Header, len(source))
	for key, values := range source {
		out[key] = append([]string(nil), values...)
	}
	return out
}
