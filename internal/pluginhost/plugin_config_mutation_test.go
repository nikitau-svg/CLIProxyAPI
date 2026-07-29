package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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

func TestHostPluginConfigListMutationDefersReloadUntilPluginCallCloses(t *testing.T) {
	host := New()
	afterCalls := 0
	host.SetPluginConfigListMutator(func(
		context.Context,
		string,
		PluginConfigListMutationRequest,
	) (PluginConfigListMutationResult, error) {
		return PluginConfigListMutationResult{
			Items: []json.RawMessage{json.RawMessage(`{"id":"prj_test"}`)},
			AfterPluginCall: func() {
				afterCalls++
			},
		}, nil
	})

	managementCtx := withPluginManagementMutation(context.Background())
	callbackID, closeCallback := host.openCallbackContextForPlugin(managementCtx, "bravo")
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
	if afterCalls != 0 {
		t.Fatalf("post-call reload ran %d times while the plugin call was active", afterCalls)
	}
	result, errDecode := decodeRPCEnvelope[PluginConfigListMutationResult](rawResponse)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(result.Items) != 1 {
		t.Fatalf("result = %#v", result)
	}

	closeCallback()
	if afterCalls != 1 {
		t.Fatalf("post-call reload ran %d times after plugin call close, want 1", afterCalls)
	}
	closeCallback()
	if afterCalls != 1 {
		t.Fatalf("post-call reload ran %d times after duplicate close, want 1", afterCalls)
	}
}

func TestRPCManagementMutationWaitsForPostCallReload(t *testing.T) {
	host := New()
	pluginCallExited := make(chan struct{})
	mutationReturned := make(chan struct{})
	reloadStarted := make(chan struct{})
	reloadRelease := make(chan struct{})
	host.SetPluginConfigListMutator(func(
		context.Context,
		string,
		PluginConfigListMutationRequest,
	) (PluginConfigListMutationResult, error) {
		return PluginConfigListMutationResult{
			Items: []json.RawMessage{json.RawMessage(`{"id":"prj_test"}`)},
			AfterPluginCall: func() {
				close(reloadStarted)
				<-reloadRelease
			},
		}, nil
	})

	client := &managementMutationPluginClient{
		host:             host,
		pluginCallExited: pluginCallExited,
		mutationReturned: mutationReturned,
	}
	adapter := &rpcPluginAdapter{
		id:     "bravo",
		host:   host,
		client: newGuardedPluginClient(client),
	}
	handleDone := make(chan error, 1)
	go func() {
		_, errHandle := adapter.HandleManagement(
			withPluginManagementMutation(context.Background()),
			pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   "/v0/management/bravo/projects",
			},
		)
		handleDone <- errHandle
	}()

	for name, signal := range map[string]<-chan struct{}{
		"plugin mutation":  mutationReturned,
		"post-call reload": reloadStarted,
	} {
		select {
		case <-signal:
		case <-time.After(time.Second):
			t.Fatalf("%s did not start", name)
		}
	}
	select {
	case <-pluginCallExited:
	default:
		t.Fatal("post-call reload started before the plugin management call exited")
	}
	select {
	case errHandle := <-handleDone:
		t.Fatalf("management call returned before post-call reload finished: %v", errHandle)
	default:
	}

	close(reloadRelease)
	select {
	case errHandle := <-handleDone:
		if errHandle != nil {
			t.Fatal(errHandle)
		}
	case <-time.After(time.Second):
		t.Fatal("management call did not return after post-call reload finished")
	}
}

type managementMutationPluginClient struct {
	host             *Host
	pluginCallExited chan struct{}
	mutationReturned chan struct{}
}

func (c *managementMutationPluginClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if method != pluginabi.MethodManagementHandle {
		return nil, fmt.Errorf("method = %s, want %s", method, pluginabi.MethodManagementHandle)
	}
	defer close(c.pluginCallExited)

	var managementRequest rpcManagementRequest
	if errUnmarshal := json.Unmarshal(request, &managementRequest); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	rawMutation, errMarshal := json.Marshal(rpcHostPluginConfigListMutationRequest{
		HostCallbackID: managementRequest.HostCallbackID,
		PluginConfigListMutationRequest: PluginConfigListMutationRequest{
			Field:      "smart_keys",
			Operation:  "append",
			MatchField: "id",
			MatchValue: "prj_test",
			Value:      json.RawMessage(`{"id":"prj_test"}`),
		},
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	if _, errCall := c.host.callFromPlugin(
		withHostCallbackPluginID(ctx, "bravo"),
		pluginabi.MethodHostPluginConfigListMutate,
		rawMutation,
	); errCall != nil {
		return nil, errCall
	}
	close(c.mutationReturned)
	return marshalRPCResult(pluginapi.ManagementResponse{StatusCode: http.StatusCreated})
}

func (c *managementMutationPluginClient) Shutdown() {}

func asPluginConfigMutationError(err error, target **PluginConfigMutationError) bool {
	current, ok := err.(*PluginConfigMutationError)
	if !ok {
		return false
	}
	*target = current
	return true
}
