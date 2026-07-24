package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type repeatedHomeAuthDispatcher struct {
	calls  atomic.Int32
	authID string
}

func (d *repeatedHomeAuthDispatcher) HeartbeatOK() bool {
	return true
}

func (d *repeatedHomeAuthDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	d.calls.Add(1)
	authID := d.authID
	if authID == "" {
		authID = "home-auth-1"
	}
	raw, _ := json.Marshal(homeAuthDispatchResponse{
		Auth: Auth{
			ID:       authID,
			Provider: "home-loop-test",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "loop@example.com"},
		},
	})
	return raw, nil
}

type unauthorizedHomeExecutor struct {
	calls atomic.Int32
}

func (e *unauthorizedHomeExecutor) Identifier() string { return "home-loop-test" }

func (e *unauthorizedHomeExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "missing access token"}
}

func (e *unauthorizedHomeExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "missing access token"}
}

func (e *unauthorizedHomeExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "missing access token"}
}

func (e *unauthorizedHomeExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "missing access token"}
}

func (e *unauthorizedHomeExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "missing access token"}
}

func TestManagerExecuteHomeStopsWhenDispatchRepeatsTriedAuth(t *testing.T) {
	dispatcher := &repeatedHomeAuthDispatcher{}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	executor := &unauthorizedHomeExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(executor)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := manager.Execute(ctx, []string{"home-loop-test"}, cliproxyexecutor.Request{Model: "gemini-3.5-flash-low"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("Execute error = nil, want missing access token")
	}
	if statusCodeFromError(err) != http.StatusUnauthorized {
		t.Fatalf("Execute error status = %d, want 401 (%v)", statusCodeFromError(err), err)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d, want 1", got)
	}
	if got := dispatcher.calls.Load(); got != 2 {
		t.Fatalf("home dispatch calls = %d, want 2", got)
	}
}

func TestManagerExecuteHomePinnedAuthMismatchFailsClosed(t *testing.T) {
	dispatcher := &repeatedHomeAuthDispatcher{authID: "home-auth-other"}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	executor := &unauthorizedHomeExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(executor)

	_, err := manager.Execute(
		context.Background(),
		[]string{"home-loop-test"},
		cliproxyexecutor.Request{Model: "gemini-3.5-flash-low"},
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey:    "home-auth-pinned",
			cliproxyexecutor.SingleAttemptMetadataKey: true,
		}},
	)
	if err == nil {
		t.Fatal("Execute error = nil, want pinned auth mismatch")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "pinned_auth_mismatch" {
		t.Fatalf("Execute error = %v, want pinned_auth_mismatch", err)
	}
	if got := executor.calls.Load(); got != 0 {
		t.Fatalf("executor calls = %d, want 0", got)
	}
	if got := dispatcher.calls.Load(); got != 1 {
		t.Fatalf("home dispatch calls = %d, want 1", got)
	}
}

func TestManagerExecuteHomeSingleAttemptDoesNotRefreshOrRedispatch(t *testing.T) {
	dispatcher := &repeatedHomeAuthDispatcher{authID: "home-auth-pinned"}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	executor := &unauthorizedHomeExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(executor)

	_, err := manager.Execute(
		context.Background(),
		[]string{"home-loop-test"},
		cliproxyexecutor.Request{Model: "gemini-3.5-flash-low"},
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey:    "home-auth-pinned",
			cliproxyexecutor.SingleAttemptMetadataKey: true,
		}},
	)
	if err == nil || statusCodeFromError(err) != http.StatusUnauthorized {
		t.Fatalf("Execute error = %v, want unauthorized", err)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d, want 1", got)
	}
	if got := dispatcher.calls.Load(); got != 1 {
		t.Fatalf("home dispatch calls = %d, want 1", got)
	}
}
