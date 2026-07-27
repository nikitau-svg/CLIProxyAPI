package auth

import (
	"context"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type deferredResultContextHook struct {
	NoopHook
	mu     sync.Mutex
	ctxErr error
	called bool
}

func (h *deferredResultContextHook) OnResult(ctx context.Context, _ Result) {
	h.mu.Lock()
	h.called = true
	h.ctxErr = ctx.Err()
	h.mu.Unlock()
}

func (h *deferredResultContextHook) snapshot() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.called, h.ctxErr
}

func TestManagerMarkResultIgnoresCanceledRequestContext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "canceled-result-auth",
		Provider: "claude",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  false,
		Error: &Error{
			Code:       "request_canceled",
			Message:    "client request was canceled",
			HTTPStatus: 499,
		},
	})
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  true,
	})

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("registered auth disappeared")
	}
	if got.Success != 0 || got.Failed != 0 {
		t.Fatalf("canceled results changed totals: success=%d failed=%d", got.Success, got.Failed)
	}
	if got.Unavailable || got.Status != StatusActive || got.LastError != nil || len(got.ModelStates) != 0 {
		t.Fatalf("canceled results changed provider availability: %#v", got)
	}
	if got.Quota.Exceeded || got.Quota.NextRecoverAt.Unix() > 0 {
		t.Fatalf("canceled results changed quota state: %#v", got.Quota)
	}
}

func TestDeferredResultAccountingDiscardsFastCompletedStreamLoser(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "fast-stream-loser",
		Provider: "claude",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	ctx, gate := WithDeferredResultAccounting(context.Background())
	remaining := make(chan cliproxyexecutor.StreamChunk)
	close(remaining)
	result := manager.wrapStreamResult(
		ctx,
		auth,
		auth.Provider,
		"claude-sonnet",
		nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("one-and-done")}},
		remaining,
		OAuthModelAliasResult{},
	)
	for range result.Chunks {
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("registered auth disappeared")
	}
	if got.Success != 0 || got.Failed != 0 || len(got.ModelStates) != 0 {
		t.Fatalf("uncommitted fast stream changed accounting: %#v", got)
	}

	gate.Discard()
	got, _ = manager.GetByID(auth.ID)
	if got.Success != 0 || got.Failed != 0 || len(got.ModelStates) != 0 {
		t.Fatalf("discarded fast stream changed accounting: %#v", got)
	}
}

func TestDeferredResultAccountingCommitsWinnerButIgnoresLateCancellation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "committed-stream-winner",
		Provider: "codex",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	parentCtx, cancelParent := context.WithCancel(context.Background())
	ctx, gate := WithDeferredResultAccounting(parentCtx)
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.6-terra",
		Success:  true,
	})
	gate.Commit()

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil || got.Success != 1 || got.Failed != 0 {
		t.Fatalf("committed winner accounting = %#v, want one success", got)
	}

	cancelParent()
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.6-terra",
		Success:  false,
		Error: &Error{
			Code:       "request_canceled",
			Message:    "late child cancellation",
			HTTPStatus: 499,
		},
	})
	got, _ = manager.GetByID(auth.ID)
	if got.Success != 1 || got.Failed != 0 || got.Unavailable || got.LastError != nil {
		t.Fatalf("late cancellation poisoned committed winner: %#v", got)
	}
}

func TestDeferredResultAccountingCommitUsesLiveDetachedApplyContext(t *testing.T) {
	hook := &deferredResultContextHook{}
	manager := NewManager(nil, nil, hook)
	auth := &Auth{
		ID:       "deferred-result-hook-context",
		Provider: "claude",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	childCtx, gate := WithDeferredResultAccounting(context.Background())
	streamCtx, cancelStream := context.WithCancel(childCtx)
	manager.MarkResult(streamCtx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet",
		Success:  true,
	})
	cancelStream()
	gate.Commit()

	called, ctxErr := hook.snapshot()
	if !called || ctxErr != nil {
		t.Fatalf("deferred result hook called=%v ctxErr=%v, want a live detached context", called, ctxErr)
	}
	if got, _ := manager.GetByID(auth.ID); got.Success != 1 || got.Failed != 0 {
		t.Fatalf("deferred result accounting = %#v, want one committed success", got)
	}
}

func TestDeferredResultAccountingCommitPreservesPendingOrder(t *testing.T) {
	gate := &DeferredResultAccounting{}
	var order []int
	gate.deferApply(func() { order = append(order, 1) })
	apply, ok := gate.PrepareCommit()
	if !ok || apply == nil {
		t.Fatal("PrepareCommit did not return a pending-result runner")
	}
	gate.deferApply(func() { order = append(order, 2) })
	apply()
	gate.deferApply(func() { order = append(order, 3) })
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("deferred result order = %v, want [1 2 3]", order)
	}
}

func TestDeferredResultAccountingAlsoGatesAvailabilityNeutralResults(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "deferred-neutral-result",
		Provider: "claude",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	ctx, gate := WithDeferredResultAccounting(context.Background())
	manager.recordAvailabilityNeutralResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  true,
	})
	if got, _ := manager.GetByID(auth.ID); got.Success != 0 || got.Failed != 0 {
		t.Fatalf("uncommitted neutral result changed accounting: %#v", got)
	}
	gate.Discard()
	if got, _ := manager.GetByID(auth.ID); got.Success != 0 || got.Failed != 0 {
		t.Fatalf("discarded neutral result changed accounting: %#v", got)
	}
}
