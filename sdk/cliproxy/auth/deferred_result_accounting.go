package auth

import (
	"context"
	"sync"
)

type deferredResultAccountingContextKey struct{}

type deferredResultAccountingState uint8

const (
	deferredResultAccountingOpen deferredResultAccountingState = iota
	deferredResultAccountingCommitting
	deferredResultAccountingCommitted
	deferredResultAccountingDiscarded
)

// DeferredResultAccounting holds Core auth-result mutations until the owner of
// a speculative attempt explicitly commits or discards them. It is intentionally
// request-local and never persisted.
type DeferredResultAccounting struct {
	mu      sync.Mutex
	state   deferredResultAccountingState
	pending []func()
}

// WithDeferredResultAccounting attaches a fresh accounting gate to ctx.
func WithDeferredResultAccounting(ctx context.Context) (context.Context, *DeferredResultAccounting) {
	if ctx == nil {
		ctx = context.Background()
	}
	gate := &DeferredResultAccounting{}
	return context.WithValue(ctx, deferredResultAccountingContextKey{}, gate), gate
}

func deferredResultAccountingFromContext(ctx context.Context) *DeferredResultAccounting {
	if ctx == nil {
		return nil
	}
	gate, _ := ctx.Value(deferredResultAccountingContextKey{}).(*DeferredResultAccounting)
	return gate
}

func (g *DeferredResultAccounting) deferApply(apply func()) bool {
	if g == nil || apply == nil {
		return false
	}
	g.mu.Lock()
	switch g.state {
	case deferredResultAccountingOpen, deferredResultAccountingCommitting:
		g.pending = append(g.pending, apply)
		g.mu.Unlock()
	case deferredResultAccountingCommitted:
		g.mu.Unlock()
		apply()
	case deferredResultAccountingDiscarded:
		g.mu.Unlock()
	}
	return true
}

// Commit applies every result already observed and lets later results apply
// immediately. Commit is idempotent and cannot reverse a prior discard.
func (g *DeferredResultAccounting) Commit() {
	apply, ok := g.PrepareCommit()
	if ok && apply != nil {
		apply()
	}
}

// PrepareCommit atomically transitions the gate to committed and returns the
// pending work as a runner. Callers that coordinate another ownership lock can
// make the state transition under that lock, then run mutations after unlock.
func (g *DeferredResultAccounting) PrepareCommit() (func(), bool) {
	if g == nil {
		return nil, false
	}
	g.mu.Lock()
	switch g.state {
	case deferredResultAccountingCommitting:
		g.mu.Unlock()
		return nil, true
	case deferredResultAccountingCommitted:
		g.mu.Unlock()
		return nil, true
	case deferredResultAccountingDiscarded:
		g.mu.Unlock()
		return nil, false
	}
	g.state = deferredResultAccountingCommitting
	g.mu.Unlock()
	return func() {
		for {
			g.mu.Lock()
			pending := append([]func(){}, g.pending...)
			g.pending = nil
			if len(pending) == 0 {
				g.state = deferredResultAccountingCommitted
				g.mu.Unlock()
				return
			}
			g.mu.Unlock()
			for _, apply := range pending {
				if apply != nil {
					apply()
				}
			}
		}
	}, true
}

// Discard drops every deferred result and suppresses later results. Discard is
// idempotent and cannot reverse a prior commit.
func (g *DeferredResultAccounting) Discard() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.state == deferredResultAccountingOpen {
		g.state = deferredResultAccountingDiscarded
		g.pending = nil
	}
	g.mu.Unlock()
}
