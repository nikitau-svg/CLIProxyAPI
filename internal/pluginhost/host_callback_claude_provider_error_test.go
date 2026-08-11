package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostModelExecutePreservesReviewedClaudeMaxTokensErrorEndToEnd(t *testing.T) {
	t.Parallel()

	const (
		authID = "host-callback-claude-max-tokens"
		model  = "claude-sonnet-5"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(
			`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must not exceed 128000; request_id=req_private"}}`,
		))
	}))
	defer upstream.Close()

	cfg := &internalconfig.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": upstream.URL,
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register Claude auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	host := New()
	host.SetModelExecutor(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	rawRequest, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol:  "openai",
		ExitProtocol:   "openai",
		ForcedProvider: "claude",
		AuthID:         authID,
		SingleAttempt:  true,
		Model:          model,
		Body:           []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"ok"}],"max_tokens":32}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawRequest)
	if errCall == nil {
		t.Fatal("host callback error = nil")
	}

	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(marshalHostCallbackError(errCall), &envelope); errUnmarshal != nil {
		t.Fatalf("decode host callback error: %v", errUnmarshal)
	}
	if envelope.Error == nil || envelope.Error.ProviderError == nil {
		t.Fatalf("host callback envelope = %#v, want safe provider detail", envelope)
	}
	detail := providererror.Sanitize(*envelope.Error.ProviderError)
	if detail.Type != "invalid_request_error" || detail.Code != "invalid_parameter" ||
		detail.Parameter != "max_tokens" || detail.Scope != providererror.ScopeRequest {
		t.Fatalf("provider detail = %#v, want reviewed max_tokens failure", detail)
	}
	for _, forbidden := range []string{"128000", "request_id", "req_private"} {
		if strings.Contains(strings.ToLower(string(marshalHostCallbackError(errCall))), forbidden) {
			t.Fatalf("host callback leaks %q", forbidden)
		}
	}
}
