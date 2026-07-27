package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	testMethodHostCallbackFork   = "host.callback.fork"
	testMethodHostCallbackCommit = "host.callback.commit"
	testMethodHostCallbackClose  = "host.callback.close"
)

type testHostCallbackScopeRequest struct {
	HostCallbackID string `json:"host_callback_id"`
}

func TestHostCallbackCommitAppliesWinnerAndCloseDiscardsLoser(t *testing.T) {
	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "deferred-child-account",
		Provider: "claude",
		Status:   coreauth.StatusActive,
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeParent()

	winnerID := forkTestHostCallback(t, host, "bravo", parentID)
	winnerCtx := host.resolveCallbackContext(winnerID, context.Background())
	manager.MarkResult(winnerCtx, coreauth.Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  true,
	})
	if got, _ := manager.GetByID(auth.ID); got.Success != 0 {
		t.Fatalf("uncommitted winner success = %d, want 0", got.Success)
	}
	commitTestHostCallback(t, host, "bravo", winnerID)
	if got, _ := manager.GetByID(auth.ID); got.Success != 1 || got.Failed != 0 {
		t.Fatalf("committed winner accounting = %#v", got)
	}
	closeTestHostCallback(t, host, "bravo", winnerID)

	loserID := forkTestHostCallback(t, host, "bravo", parentID)
	loserCtx := host.resolveCallbackContext(loserID, context.Background())
	manager.MarkResult(loserCtx, coreauth.Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  true,
	})
	closeTestHostCallback(t, host, "bravo", loserID)
	if got, _ := manager.GetByID(auth.ID); got.Success != 1 || got.Failed != 0 {
		t.Fatalf("closed loser changed accounting: %#v", got)
	}
}

func TestHostCallbackCommitRejectsForeignAndParentCleanupPreservesCanceledTombstone(t *testing.T) {
	host := New()
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	childID := forkTestHostCallback(t, host, "bravo", parentID)

	_, errForeign := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "other"),
		testMethodHostCallbackCommit,
		marshalTestHostCallbackRequest(t, childID),
	)
	assertHostCallbackScopeError(t, errForeign, "host_callback_forbidden", http.StatusForbidden)

	closeTestHostCallback(t, host, "bravo", childID)
	closeParent()
	_, errLateCommit := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		testMethodHostCallbackCommit,
		marshalTestHostCallbackRequest(t, childID),
	)
	assertHostCallbackScopeError(t, errLateCommit, "request_canceled", statusClientClosedRequest)
}

func TestHostCallbackParentCloseAtomicallyInvalidatesChildCommit(t *testing.T) {
	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "parent-close-child-race",
		Provider: "claude",
		Status:   coreauth.StatusActive,
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	if !host.addCallbackCleanup(parentID, func() {
		close(cleanupEntered)
		<-releaseCleanup
	}) {
		t.Fatal("failed to register deterministic parent cleanup blocker")
	}
	childID := forkTestHostCallback(t, host, "bravo", parentID)
	childCtx := host.resolveCallbackContext(childID, context.Background())
	manager.MarkResult(childCtx, coreauth.Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  true,
	})

	parentClosed := make(chan struct{})
	go func() {
		defer close(parentClosed)
		closeParent()
	}()
	assertSignalReceived(t, cleanupEntered, "parent cleanup blocker")

	_, errCommit := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		testMethodHostCallbackCommit,
		marshalTestHostCallbackRequest(t, childID),
	)
	assertHostCallbackScopeError(t, errCommit, "request_canceled", statusClientClosedRequest)
	if got, _ := manager.GetByID(auth.ID); got.Success != 0 || got.Failed != 0 {
		t.Fatalf("child accounting applied after parent close: %#v", got)
	}

	close(releaseCleanup)
	assertSignalReceived(t, parentClosed, "parent close completion")
}

type testHostCallbackScopeResponse struct {
	HostCallbackID string `json:"host_callback_id"`
}

type testHostCallbackValueKey struct{}

func TestHostCallbackForkCloseOwnsCancelableChildScope(t *testing.T) {
	host := New()
	parentValue := "request-value"
	parentCtx, cancelParent := context.WithTimeout(
		context.WithValue(context.Background(), testHostCallbackValueKey{}, parentValue),
		time.Minute,
	)
	defer cancelParent()
	parentID, closeParent := host.openCallbackContextForPlugin(parentCtx, "bravo")
	defer closeParent()

	childID := forkTestHostCallback(t, host, "bravo", parentID)
	if childID == "" || childID == parentID {
		t.Fatalf("forked callback id = %q, parent = %q", childID, parentID)
	}
	if got := host.callbackContextPluginID(childID); got != "bravo" {
		t.Fatalf("child plugin id = %q, want bravo", got)
	}
	childCtx := host.resolveCallbackContext(childID, context.Background())
	if got := childCtx.Value(testHostCallbackValueKey{}); got != parentValue {
		t.Fatalf("child context value = %#v, want %q", got, parentValue)
	}
	parentDeadline, parentHasDeadline := parentCtx.Deadline()
	childDeadline, childHasDeadline := childCtx.Deadline()
	if !parentHasDeadline || !childHasDeadline || !childDeadline.Equal(parentDeadline) {
		t.Fatalf(
			"child deadline = %v/%v, parent = %v/%v",
			childDeadline,
			childHasDeadline,
			parentDeadline,
			parentHasDeadline,
		)
	}

	closeTestHostCallback(t, host, "bravo", childID)
	select {
	case <-childCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("child callback context was not canceled")
	}
	if errParent := parentCtx.Err(); errParent != nil {
		t.Fatalf("closing child canceled parent: %v", errParent)
	}
	if testCallbackContextExists(host, childID) {
		t.Fatal("closed child callback remains registered")
	}

	// Closing an owned child is deliberately idempotent so winner/cleanup races
	// cannot turn into a second host-side failure.
	closeTestHostCallback(t, host, "bravo", childID)
}

func TestHostCallbackForkCloseRejectsAnotherPlugin(t *testing.T) {
	host := New()
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeParent()

	rawFork := marshalTestHostCallbackRequest(t, parentID)
	_, errFork := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "other"),
		testMethodHostCallbackFork,
		rawFork,
	)
	assertHostCallbackScopeError(t, errFork, "host_callback_forbidden", http.StatusForbidden)

	childID := forkTestHostCallback(t, host, "bravo", parentID)
	childCtx := host.resolveCallbackContext(childID, context.Background())
	rawClose := marshalTestHostCallbackRequest(t, childID)
	_, errClose := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "other"),
		testMethodHostCallbackClose,
		rawClose,
	)
	assertHostCallbackScopeError(t, errClose, "host_callback_forbidden", http.StatusForbidden)
	select {
	case <-childCtx.Done():
		t.Fatal("foreign plugin canceled another plugin's callback")
	default:
	}

	closeTestHostCallback(t, host, "bravo", childID)
}

func TestHostCallbackParentCloseCascadesToChildren(t *testing.T) {
	host := New()
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	childOneID := forkTestHostCallback(t, host, "bravo", parentID)
	childTwoID := forkTestHostCallback(t, host, "bravo", parentID)
	childOneCtx := host.resolveCallbackContext(childOneID, context.Background())
	childTwoCtx := host.resolveCallbackContext(childTwoID, context.Background())

	childOneCleanup := make(chan struct{})
	childTwoCleanup := make(chan struct{})
	if !host.addCallbackCleanup(childOneID, func() { close(childOneCleanup) }) {
		t.Fatal("failed to register first child cleanup")
	}
	if !host.addCallbackCleanup(childTwoID, func() { close(childTwoCleanup) }) {
		t.Fatal("failed to register second child cleanup")
	}

	closeParent()
	assertContextCanceled(t, childOneCtx, "first child")
	assertContextCanceled(t, childTwoCtx, "second child")
	assertSignalReceived(t, childOneCleanup, "first child cleanup")
	assertSignalReceived(t, childTwoCleanup, "second child cleanup")
	if testCallbackContextExists(host, childOneID) || testCallbackContextExists(host, childTwoID) {
		t.Fatal("parent close left child callback scopes registered")
	}
}

func TestHostCallbackForkAfterCanceledParentReturnsRequestCanceled(t *testing.T) {
	host := New()
	parentCtx, cancelParent := context.WithCancel(context.Background())
	parentID, closeParent := host.openCallbackContextForPlugin(parentCtx, "bravo")
	cancelParent()
	closeParent()

	_, errFork := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostCallbackFork,
		marshalTestHostCallbackRequest(t, parentID),
	)
	assertHostCallbackScopeError(t, errFork, "request_canceled", statusClientClosedRequest)
}

func TestHostModelCallbacksFailClosedForUnknownOrClosedScope(t *testing.T) {
	callbacks := []struct {
		name   string
		method string
		stream bool
	}{
		{name: "execute", method: pluginabi.MethodHostModelExecute},
		{name: "count", method: pluginabi.MethodHostModelCountTokens},
		{name: "stream bootstrap", method: pluginabi.MethodHostModelExecuteStream, stream: true},
	}
	scopes := []struct {
		name string
		id   func(*Host) string
	}{
		{
			name: "unknown",
			id: func(*Host) string {
				return "unknown-callback-id"
			},
		},
		{
			name: "closed",
			id: func(host *Host) string {
				id, closeCallback := host.openCallbackContextForPlugin(context.Background(), "bravo")
				closeCallback()
				return id
			},
		},
	}

	for _, callback := range callbacks {
		callback := callback
		for _, scope := range scopes {
			scope := scope
			t.Run(callback.name+"/"+scope.name, func(t *testing.T) {
				host := New()
				var called atomic.Int32
				host.SetModelExecutor(&fakeHostModelExecutor{
					executeModel: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
						called.Add(1)
						return handlers.ModelExecutionResponse{}, &interfaces.ErrorMessage{
							StatusCode: http.StatusTeapot,
							Error:      errors.New("execute must not run for an invalid callback"),
						}
					},
					countModelTokens: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
						called.Add(1)
						return handlers.ModelExecutionResponse{}, &interfaces.ErrorMessage{
							StatusCode: http.StatusTeapot,
							Error:      errors.New("count must not run for an invalid callback"),
						}
					},
					executeModelStream: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
						called.Add(1)
						return handlers.ModelExecutionStream{}, &interfaces.ErrorMessage{
							StatusCode: http.StatusTeapot,
							Error:      errors.New("stream must not run for an invalid callback"),
						}
					},
				})

				rawRequest, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
					HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
						EntryProtocol: "openai",
						ExitProtocol:  "openai",
						Model:         "model-1",
						Stream:        callback.stream,
						Body:          []byte(`{"model":"model-1"}`),
					},
					HostCallbackID: scope.id(host),
				})
				if errMarshal != nil {
					t.Fatalf("marshal model callback request: %v", errMarshal)
				}
				_, errCall := host.callFromPlugin(
					withHostCallbackPluginID(context.Background(), "bravo"),
					callback.method,
					rawRequest,
				)
				assertHostCallbackScopeError(t, errCall, "host_callback_invalid", http.StatusBadRequest)
				if got := called.Load(); got != 0 {
					t.Fatalf("model executor calls = %d, want 0", got)
				}
			})
		}
	}
}

func TestHostModelStreamReadCancellationReturnsRequestCanceled(t *testing.T) {
	host := New()
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeParent()
	childID := forkTestHostCallback(t, host, "bravo", parentID)
	streamID := openOwnedHostModelStreamForTest(t, host, "bravo", childID)

	readRequest, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{
		StreamID:       streamID,
		HostCallbackID: childID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	readResult := make(chan error, 1)
	go func() {
		_, errRead := host.callFromPlugin(
			withHostCallbackPluginID(context.Background(), "bravo"),
			pluginabi.MethodHostModelStreamRead,
			readRequest,
		)
		readResult <- errRead
	}()

	closeTestHostCallback(t, host, "bravo", childID)
	select {
	case errRead := <-readResult:
		assertHostCallbackScopeError(t, errRead, "request_canceled", statusClientClosedRequest)
	case <-time.After(time.Second):
		t.Fatal("stream read did not stop after child callback cancellation")
	}
}

func TestHostModelExecuteStreamRegistrationFailureClosesOpenedBridge(t *testing.T) {
	host := New()
	parentID, closeParent := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeParent()
	callbackID := forkTestHostCallback(t, host, "bravo", parentID)
	streamContext := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			streamContext <- ctx
			// Deterministically close the callback after the initial cancel
			// cleanup was registered but before the host opens and registers
			// cleanup for the returned stream bridge.
			closeTestHostCallback(t, host, "bravo", callbackID)
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})

	request, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"model":"model-1","stream":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostModelExecuteStream,
		request,
	)
	assertHostCallbackScopeError(t, errCall, "request_canceled", statusClientClosedRequest)

	var startedContext context.Context
	select {
	case startedContext = <-streamContext:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	assertContextCanceled(t, startedContext, "stream registration failure")
	if count := hostModelStreamCountForTest(t, host); count != 0 {
		t.Fatalf("model stream count = %d, want 0 after cleanup registration failure", count)
	}
}

func TestHostModelStreamReadAndCloseRejectAnotherPlugin(t *testing.T) {
	host := New()
	chunks := make(chan handlers.ModelExecutionChunk, 1)
	chunks <- handlers.ModelExecutionChunk{Payload: []byte("owned")}
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     chunks,
			}, nil
		},
	})
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "bravo")
	defer closeCallback()
	streamID := openOwnedHostModelStreamForTest(t, host, "bravo", callbackID)
	foreignCtx := withHostCallbackPluginID(context.Background(), "other")

	readRequest, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errRead := host.callFromPlugin(foreignCtx, pluginabi.MethodHostModelStreamRead, readRequest)
	assertHostCallbackScopeError(t, errRead, "host_callback_forbidden", http.StatusForbidden)

	closeRequest, errMarshal := json.Marshal(pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errClose := host.callFromPlugin(foreignCtx, pluginabi.MethodHostModelStreamClose, closeRequest)
	assertHostCallbackScopeError(t, errClose, "host_callback_forbidden", http.StatusForbidden)

	readRequest, errMarshal = json.Marshal(pluginapi.HostModelStreamReadRequest{
		StreamID:       streamID,
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errRead = host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), "bravo"),
		pluginabi.MethodHostModelStreamRead,
		readRequest,
	); errRead != nil {
		t.Fatalf("owner read failed after foreign access: %v", errRead)
	}
}

func openOwnedHostModelStreamForTest(t *testing.T, host *Host, pluginID, callbackID string) string {
	t.Helper()
	request, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"model":"model-1","stream":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	rawResponse, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), pluginID),
		pluginabi.MethodHostModelExecuteStream,
		request,
	)
	if errCall != nil {
		t.Fatalf("open owned model stream: %v", errCall)
	}
	response, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResponse)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.StreamID == "" {
		t.Fatal("open owned model stream returned empty stream id")
	}
	return response.StreamID
}

func forkTestHostCallback(t *testing.T, host *Host, pluginID, parentID string) string {
	t.Helper()
	rawResponse, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), pluginID),
		testMethodHostCallbackFork,
		marshalTestHostCallbackRequest(t, parentID),
	)
	if errCall != nil {
		t.Fatalf("fork callback: %v", errCall)
	}
	response, errDecode := decodeRPCEnvelope[testHostCallbackScopeResponse](rawResponse)
	if errDecode != nil {
		t.Fatalf("decode fork callback response: %v", errDecode)
	}
	if response.HostCallbackID == "" {
		t.Fatal("fork callback returned an empty host_callback_id")
	}
	return response.HostCallbackID
}

func closeTestHostCallback(t *testing.T, host *Host, pluginID, callbackID string) {
	t.Helper()
	_, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), pluginID),
		testMethodHostCallbackClose,
		marshalTestHostCallbackRequest(t, callbackID),
	)
	if errCall != nil {
		t.Fatalf("close callback %q: %v", callbackID, errCall)
	}
}

func commitTestHostCallback(t *testing.T, host *Host, pluginID, callbackID string) {
	t.Helper()
	_, errCall := host.callFromPlugin(
		withHostCallbackPluginID(context.Background(), pluginID),
		testMethodHostCallbackCommit,
		marshalTestHostCallbackRequest(t, callbackID),
	)
	if errCall != nil {
		t.Fatalf("commit callback %q: %v", callbackID, errCall)
	}
}

func marshalTestHostCallbackRequest(t *testing.T, callbackID string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(testHostCallbackScopeRequest{HostCallbackID: callbackID})
	if errMarshal != nil {
		t.Fatalf("marshal callback scope request: %v", errMarshal)
	}
	return raw
}

func assertHostCallbackScopeError(t *testing.T, err error, wantCode string, wantStatus int) {
	t.Helper()
	if err == nil {
		t.Fatalf("callback error = nil, want %s/%d", wantCode, wantStatus)
	}
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded == nil || coded.ErrorCode() != wantCode {
		t.Fatalf("callback error = %T %v, want code %q", err, err, wantCode)
	}
	var statusProvider interface{ StatusCode() int }
	if !errors.As(err, &statusProvider) || statusProvider == nil || statusProvider.StatusCode() != wantStatus {
		t.Fatalf("callback error = %T %v, want status %d", err, err, wantStatus)
	}
}

func assertContextCanceled(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("%s context was not canceled", label)
	}
}

func assertSignalReceived(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("%s was not invoked", label)
	}
}

func testCallbackContextExists(host *Host, callbackID string) bool {
	if host == nil || host.callbackContexts == nil {
		return false
	}
	host.callbackContexts.mu.RLock()
	_, exists := host.callbackContexts.contexts[callbackID]
	host.callbackContexts.mu.RUnlock()
	return exists
}
