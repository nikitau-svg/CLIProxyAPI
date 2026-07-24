package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestRouteOverrideManagementPreviewPersistAndReset(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	var stored []json.RawMessage
	mutationCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostPluginConfigListMutate {
			t.Fatalf("unexpected host method %q", method)
		}
		mutationCalls++
		var request hostPluginConfigListMutationRequest
		decodeBravoPayload(t, payload, &request)
		if request.Field != "route_overrides" || request.MatchField != "id" || request.MatchValue != "opus" {
			t.Fatalf("route mutation = %#v", request)
		}
		switch request.Operation {
		case "append":
			stored = append(stored, append(json.RawMessage(nil), request.Value...))
		case "replace":
			if len(stored) != 1 {
				t.Fatalf("replace stored items = %d", len(stored))
			}
			stored[0] = append(json.RawMessage(nil), request.Value...)
		case "delete":
			stored = nil
		default:
			t.Fatalf("unexpected route mutation operation %q", request.Operation)
		}
		return mustBravoJSON(t, hostPluginConfigListMutationResult{Items: stored}), nil
	})

	body := `{
		"id":"bravo/opus",
		"preview":true,
		"candidates":[
			{"provider":"openai","model":"gpt-5.6-sol"},
			{"provider":"anthropic","model":"claude-opus-4-8"}
		]
	}`
	status, preview := callProjectManagement(t, http.MethodPut, "/v0/management/bravo/routes", body)
	if status != http.StatusOK || preview["preview"] != true || mutationCalls != 0 {
		t.Fatalf("preview status/body/calls = %d %#v %d", status, preview, mutationCalls)
	}
	previewRoute := routeMapByID(t, preview, "opus")
	if previewRoute["overridden"] != true {
		t.Fatalf("preview route = %#v", previewRoute)
	}
	if got := loadedConfig().Models["opus"].Candidates[0].Provider; normalizeProvider(got) != "claude" {
		t.Fatalf("preview mutated runtime route: %#v", loadedConfig().Models["opus"].Candidates)
	}

	body = strings.Replace(body, `"preview":true,`, "", 1)
	status, persisted := callProjectManagement(t, http.MethodPut, "/v0/management/bravo/routes", body)
	if status != http.StatusOK || mutationCalls != 1 || len(stored) != 1 {
		t.Fatalf("persist status/body/calls = %d %#v %d", status, persisted, mutationCalls)
	}
	effective := loadedConfig().Models["opus"].Candidates
	if len(effective) != 2 || normalizeProvider(effective[0].Provider) != "codex" || effective[0].Model != "gpt-5.6-sol" {
		t.Fatalf("effective override = %#v", effective)
	}
	if _, promoted := newCapabilitySet(effective[0].Capabilities...)[capabilityStructuredOutput]; promoted {
		t.Fatalf("derived route capabilities unexpectedly promoted schema support: %#v", effective[0])
	}

	status, reset := callProjectManagement(t, http.MethodPost, "/v0/management/bravo/routes/reset", `{"id":"opus"}`)
	if status != http.StatusOK || mutationCalls != 2 || len(stored) != 0 {
		t.Fatalf("reset status/body/calls = %d %#v %d", status, reset, mutationCalls)
	}
	restored := loadedConfig().Models["opus"].Candidates
	if len(restored) != 2 || normalizeProvider(restored[0].Provider) != "claude" || restored[0].Model != "claude-opus-5" {
		t.Fatalf("reset route = %#v", restored)
	}
}

func TestRouteOverrideValidationFailsClosed(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		t.Fatalf("invalid route unexpectedly called host %q with %#v", method, payload)
		return nil, nil
	})

	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: `{"id":"opus","candidates":[]}`},
		{name: "unknown route", body: `{"id":"missing","candidates":[{"provider":"claude","model":"claude-opus-4-8"}]}`},
		{name: "unknown physical model", body: `{"id":"opus","candidates":[{"provider":"claude","model":"invented"}]}`},
		{name: "invalid effort", body: `{"id":"opus","candidates":[{"provider":"claude","model":"claude-opus-4-8","effort":"turbo"}]}`},
		{name: "duplicate candidate", body: `{"id":"opus","candidates":[{"provider":"claude","model":"claude-opus-4-8"},{"provider":"anthropic","model":"claude-opus-4-8"}]}`},
		{name: "invalid order", body: `{"id":"opus","candidates":[{"provider":"claude","model":"claude-opus-4-8","priority":10},{"provider":"codex","model":"gpt-5.6-sol","priority":20}]}`},
		{name: "capability promotion", body: `{"id":"opus","candidates":[{"provider":"claude","model":"claude-opus-4-8","capabilities":["text","structured_output"]}]}`},
		{name: "wrong route class", body: `{"id":"opus","candidates":[{"provider":"codex","model":"gpt-image-2"}]}`},
		{name: "auth ids forbidden", body: `{"id":"opus","candidates":[{"provider":"claude","model":"claude-opus-4-8","auth_ids":["secret"]}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, response := callProjectManagement(t, http.MethodPut, "/v0/management/bravo/routes", test.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status/body = %d %#v", status, response)
			}
			errorBody := projectMap(t, response["error"])
			if code, _ := errorBody["code"].(string); !strings.HasPrefix(code, "bravo_route_") {
				t.Fatalf("error = %#v", errorBody)
			}
		})
	}
}

func TestRouteManagementRoutesAreProtectedAndExposeDefaults(t *testing.T) {
	raw, errRegister := registerManagement()
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var registration managementRegistrationResponse
	if errUnmarshal := json.Unmarshal(env.Result, &registration); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	expected := map[string]bool{
		http.MethodGet + " /bravo/routes":        false,
		http.MethodPut + " /bravo/routes":        false,
		http.MethodPost + " /bravo/routes/reset": false,
	}
	for _, route := range registration.Routes {
		if _, exists := expected[route.Method+" "+route.Path]; exists {
			expected[route.Method+" "+route.Path] = true
		}
	}
	for route, found := range expected {
		if !found {
			t.Errorf("missing protected route %s", route)
		}
	}
	for _, resource := range registration.Resources {
		if strings.Contains(resource.Path, "routes") {
			t.Fatalf("route mutation was exposed as an unauthenticated resource: %#v", resource)
		}
	}

	status, response := callProjectManagement(t, http.MethodGet, "/v0/management/bravo/routes", "")
	if status != http.StatusOK {
		t.Fatalf("GET routes status/body = %d %#v", status, response)
	}
	route := routeMapByID(t, response, "opus")
	if route["request_model"] != "bravo/opus" || route["overridden"] != false {
		t.Fatalf("default route = %#v", route)
	}
	efforts, _ := response["efforts"].([]any)
	if len(efforts) == 0 {
		t.Fatalf("route effort options = %#v", response["efforts"])
	}
}

func routeMapByID(t *testing.T, response map[string]any, id string) map[string]any {
	t.Helper()
	routes, _ := response["routes"].([]any)
	for _, raw := range routes {
		route := projectMap(t, raw)
		if route["id"] == id {
			return route
		}
	}
	t.Fatalf("route %q not found in %#v", id, response)
	return nil
}
