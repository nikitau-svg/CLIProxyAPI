package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const statusClientClosedRequest = 499

type providerStatus499Error struct{}

func (providerStatus499Error) Error() string     { return "upstream closed request" }
func (providerStatus499Error) ErrorCode() string { return "upstream_closed_request" }
func (providerStatus499Error) StatusCode() int   { return statusClientClosedRequest }

func TestExecuteModelRootCancellationReturnsRequestCanceled499(t *testing.T) {
	model := "model-execution-root-cancel"
	started := make(chan struct{})
	executor := &modelExecutionCaptureExecutor{
		execute: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			close(started)
			<-ctx.Done()
			return coreexecutor.Response{}, ctx.Err()
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		_, errMsg := handler.ExecuteModel(ctx, cancellationModelExecutionRequest(model, false))
		result <- errMsg
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	cancel()

	select {
	case errMsg := <-result:
		assertRequestCanceledModelExecutionError(t, errMsg)
		assertProviderExecutionState(t, errMsg.Error, true, true)
	case <-time.After(time.Second):
		t.Fatal("ExecuteModel did not return after root cancellation")
	}
}

func TestCountModelTokensRootCancellationReturnsRequestCanceled499(t *testing.T) {
	model := "model-count-root-cancel"
	executor := &modelExecutionCaptureExecutor{}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errMsg := handler.CountModelTokens(ctx, cancellationModelExecutionRequest(model, false))
	assertRequestCanceledModelExecutionError(t, errMsg)
	assertProviderExecutionState(t, errMsg.Error, false, false)
}

func TestExecuteModelStreamBootstrapCancellationReturnsRequestCanceled499(t *testing.T) {
	model := "model-stream-bootstrap-root-cancel"
	started := make(chan struct{})
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			close(started)
			return &coreexecutor.StreamResult{Chunks: make(chan coreexecutor.StreamChunk)}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		_, errMsg := handler.ExecuteModelStream(ctx, cancellationModelExecutionRequest(model, true))
		result <- errMsg
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream executor did not start")
	}
	cancel()

	select {
	case errMsg := <-result:
		assertRequestCanceledModelExecutionError(t, errMsg)
		assertProviderExecutionState(t, errMsg.Error, true, true)
	case <-time.After(time.Second):
		t.Fatal("ExecuteModelStream did not return after bootstrap cancellation")
	}
}

func TestExecuteModelCancellationBeforeProviderDispatchIsProven(t *testing.T) {
	model := "model-execution-pre-dispatch-cancel"
	var providerCalls int
	executor := &modelExecutionCaptureExecutor{
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			providerCalls++
			return coreexecutor.Response{}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errMsg := handler.ExecuteModel(ctx, cancellationModelExecutionRequest(model, false))
	assertRequestCanceledModelExecutionError(t, errMsg)
	assertProviderExecutionState(t, errMsg.Error, false, false)
	if providerCalls != 0 {
		t.Fatalf("pre-dispatch cancellation made %d provider calls", providerCalls)
	}
}

func TestProvider499IsNotReclassifiedAsRootCancellation(t *testing.T) {
	model := "model-provider-status-499"
	executor := &modelExecutionCaptureExecutor{
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{}, providerStatus499Error{}
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})

	_, errMsg := handler.ExecuteModel(context.Background(), cancellationModelExecutionRequest(model, false))
	if errMsg == nil || errMsg.Error == nil {
		t.Fatal("provider 499 error = nil")
	}
	if errMsg.StatusCode != statusClientClosedRequest {
		t.Fatalf("provider status = %d, want %d", errMsg.StatusCode, statusClientClosedRequest)
	}
	if got := ErrorCodeFromError(errMsg.Error); got != "upstream_closed_request" {
		t.Fatalf("provider error code = %q, want upstream_closed_request", got)
	}
	var requestScoped coreexecutor.RequestScopedError
	if errors.As(errMsg.Error, &requestScoped) && requestScoped != nil && requestScoped.IsRequestScoped() {
		t.Fatalf("provider-originated 499 was reclassified as request-scoped: %T %v", errMsg.Error, errMsg.Error)
	}
}

func cancellationModelExecutionRequest(model string, stream bool) ModelExecutionRequest {
	return ModelExecutionRequest{
		EntryProtocol:  "openai",
		ExitProtocol:   "openai",
		ForcedProvider: "codex",
		AuthID:         "model-execution-" + model,
		SingleAttempt:  true,
		Model:          model,
		Stream:         stream,
		Body:           []byte(fmt.Sprintf(`{"model":%q,"stream":%t}`, model, stream)),
	}
}

func assertRequestCanceledModelExecutionError(t *testing.T, errMsg *interfaces.ErrorMessage) {
	t.Helper()
	if errMsg == nil || errMsg.Error == nil {
		t.Fatal("cancellation error = nil")
	}
	if errMsg.StatusCode != statusClientClosedRequest {
		t.Fatalf("cancellation status = %d, want %d: %v", errMsg.StatusCode, statusClientClosedRequest, errMsg.Error)
	}
	if got := ErrorCodeFromError(errMsg.Error); got != "request_canceled" {
		t.Fatalf("cancellation error code = %q, want request_canceled: %T %v", got, errMsg.Error, errMsg.Error)
	}
	var requestScoped coreexecutor.RequestScopedError
	if !errors.As(errMsg.Error, &requestScoped) || requestScoped == nil || !requestScoped.IsRequestScoped() {
		t.Fatalf("cancellation error is not request-scoped: %T %v", errMsg.Error, errMsg.Error)
	}
	var retryable interface{ Retryable() bool }
	if !errors.As(errMsg.Error, &retryable) || retryable == nil {
		t.Fatalf("cancellation error does not expose retryability: %T %v", errMsg.Error, errMsg.Error)
	}
	if retryable.Retryable() {
		t.Fatalf("cancellation error is retryable: %T %v", errMsg.Error, errMsg.Error)
	}
	if retryAfter := errMsg.Addon.Get("Retry-After"); retryAfter != "" {
		t.Fatalf("cancellation Retry-After = %q, want empty", retryAfter)
	}
	if !errors.Is(errMsg.Error, context.Canceled) {
		t.Fatalf("cancellation error does not unwrap context.Canceled: %T %v", errMsg.Error, errMsg.Error)
	}
	if errMsg.StatusCode >= http.StatusInternalServerError {
		t.Fatalf("cancellation surfaced as server failure: status %d", errMsg.StatusCode)
	}
}

func assertProviderExecutionState(t *testing.T, err error, wantStarted, wantAmbiguous bool) {
	t.Helper()
	var state interface {
		ProviderExecutionState() (started, known, ambiguous bool)
	}
	if !errors.As(err, &state) || state == nil {
		t.Fatalf("error does not expose provider execution state: %T %v", err, err)
	}
	started, known, ambiguous := state.ProviderExecutionState()
	if !known || started != wantStarted || ambiguous != wantAmbiguous {
		t.Fatalf("provider execution state = started %t known %t ambiguous %t, want %t/true/%t", started, known, ambiguous, wantStarted, wantAmbiguous)
	}
}
