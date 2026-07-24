package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestHostPluginConfigListMutationRequiresManagementCallback(t *testing.T) {
	host := New()
	host.SetPluginConfigListMutator(func(
		context.Context,
		string,
		PluginConfigListMutationRequest,
	) (PluginConfigListMutationResult, error) {
		t.Fatal("mutator was called outside a management request")
		return PluginConfigListMutationResult{}, nil
	})
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeCallback()

	rawRequest, errMarshal := json.Marshal(rpcHostPluginConfigListMutationRequest{
		HostCallbackID: callbackID,
		PluginConfigListMutationRequest: PluginConfigListMutationRequest{
			Field:      "smart_keys",
			Operation:  "append",
			MatchField: "id",
			MatchValue: "prj_test",
			Value:      json.RawMessage(`{"id":"prj_test"}`),
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostPluginConfigListMutate,
		rawRequest,
	)
	var mutationErr *PluginConfigMutationError
	if errCall == nil || !asPluginConfigMutationError(errCall, &mutationErr) {
		t.Fatalf("error = %v, want PluginConfigMutationError", errCall)
	}
	if mutationErr.HTTPStatus != http.StatusForbidden ||
		mutationErr.Code != "plugin_config_management_required" {
		t.Fatalf("error = %#v", mutationErr)
	}
}

func TestHostPluginConfigListMutationIsScopedToCallbackPlugin(t *testing.T) {
	host := New()
	called := false
	host.SetPluginConfigListMutator(func(
		_ context.Context,
		pluginID string,
		req PluginConfigListMutationRequest,
	) (PluginConfigListMutationResult, error) {
		called = true
		if pluginID != "bravo" {
			t.Fatalf("pluginID = %q", pluginID)
		}
		if req.Field != "smart_keys" || req.MatchValue != "prj_test" {
			t.Fatalf("request = %#v", req)
		}
		return PluginConfigListMutationResult{
			Items: []json.RawMessage{json.RawMessage(`{"id":"prj_test"}`)},
		}, nil
	})
	managementCtx := withPluginManagementMutation(context.Background())
	callbackID, closeCallback := host.openCallbackContextForPlugin(managementCtx, "bravo")
	defer closeCallback()
	rawRequest, errMarshal := json.Marshal(rpcHostPluginConfigListMutationRequest{
		HostCallbackID: callbackID,
		PluginConfigListMutationRequest: PluginConfigListMutationRequest{
			Field:      "smart_keys",
			Operation:  "append",
			MatchField: "id",
			MatchValue: "prj_test",
			Value:      json.RawMessage(`{"id":"prj_test"}`),
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	rawResponse, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostPluginConfigListMutate,
		rawRequest,
	)
	if errCall != nil {
		t.Fatal(errCall)
	}
	if !called {
		t.Fatal("mutator was not called")
	}
	result, errDecode := decodeRPCEnvelope[PluginConfigListMutationResult](rawResponse)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(result.Items) != 1 {
		t.Fatalf("result = %#v", result)
	}

	_, errWrongPlugin := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "other"),
		pluginabi.MethodHostPluginConfigListMutate,
		rawRequest,
	)
	var mutationErr *PluginConfigMutationError
	if errWrongPlugin == nil || !asPluginConfigMutationError(errWrongPlugin, &mutationErr) ||
		mutationErr.Code != "plugin_config_callback_forbidden" {
		t.Fatalf("wrong-plugin error = %v", errWrongPlugin)
	}
}

func asPluginConfigMutationError(err error, target **PluginConfigMutationError) bool {
	current, ok := err.(*PluginConfigMutationError)
	if !ok {
		return false
	}
	*target = current
	return true
}
