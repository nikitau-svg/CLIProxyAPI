package pluginhost

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type callbackContextRegistry struct {
	next         atomic.Uint64
	mu           sync.RWMutex
	contexts     map[string]callbackContextEntry
	closedOwners map[string]closedCallbackContextEntry
	lastPrune    time.Time
}

type callbackContextEntry struct {
	ctx      context.Context
	pluginID string
	parentID string
	cancel   context.CancelFunc
	cleanup  []func()
	account  *coreauth.DeferredResultAccounting
}

type closedCallbackContextEntry struct {
	pluginID string
	canceled bool
	closedAt time.Time
}

const (
	maxClosedCallbackContexts = 4096
	closedCallbackContextTTL  = 5 * time.Minute
	closedCallbackPruneEvery  = time.Minute
)

func newCallbackContextRegistry() *callbackContextRegistry {
	return &callbackContextRegistry{
		contexts:     make(map[string]callbackContextEntry),
		closedOwners: make(map[string]closedCallbackContextEntry),
	}
}

func (r *callbackContextRegistry) open(ctx context.Context, pluginID string) (string, func()) {
	if r == nil {
		return "", func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pluginID = strings.TrimSpace(pluginID)
	ctx = withHostCallbackPluginID(ctx, pluginID)
	id := strconv.FormatUint(r.next.Add(1), 10)
	r.mu.Lock()
	r.pruneClosedLocked(time.Now())
	r.contexts[id] = callbackContextEntry{ctx: ctx, pluginID: pluginID}
	r.mu.Unlock()

	var once sync.Once
	return id, func() {
		once.Do(func() {
			r.closeInternal(id, true)
		})
	}
}

func (r *callbackContextRegistry) fork(parentID, callerPluginID string) (string, bool, bool, bool) {
	if r == nil || strings.TrimSpace(parentID) == "" {
		return "", false, false, false
	}
	parentID = strings.TrimSpace(parentID)
	callerPluginID = strings.TrimSpace(callerPluginID)

	r.mu.Lock()
	parent, ok := r.contexts[parentID]
	if !ok || parent.ctx == nil {
		closed := r.closedOwners[parentID]
		r.mu.Unlock()
		if closed.pluginID != "" && closed.pluginID != callerPluginID {
			return "", false, true, false
		}
		if closed.canceled {
			return "", false, false, true
		}
		return "", false, false, false
	}
	if parent.pluginID != callerPluginID {
		r.mu.Unlock()
		return "", false, true, false
	}
	if parent.ctx.Err() != nil {
		r.mu.Unlock()
		return "", false, false, true
	}
	childCtx, cancel := context.WithCancel(parent.ctx)
	childCtx, account := coreauth.WithDeferredResultAccounting(childCtx)
	childID := strconv.FormatUint(r.next.Add(1), 10)
	r.contexts[childID] = callbackContextEntry{
		ctx:      childCtx,
		pluginID: parent.pluginID,
		parentID: parentID,
		cancel:   cancel,
		account:  account,
	}
	parent.cleanup = append(parent.cleanup, func() {
		r.closeInternal(childID, true)
	})
	r.contexts[parentID] = parent
	r.mu.Unlock()
	return childID, true, false, false
}

func (r *callbackContextRegistry) closeOwned(id, callerPluginID string) (bool, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return false, false
	}
	id = strings.TrimSpace(id)
	callerPluginID = strings.TrimSpace(callerPluginID)

	r.mu.Lock()
	if active, ok := r.contexts[id]; ok {
		if active.pluginID != callerPluginID {
			r.mu.Unlock()
			return false, true
		}
		entries := r.detachSubtreeLocked(id, true, time.Now())
		r.mu.Unlock()
		closeCallbackContextEntries(entries)
		return true, false
	}
	if closed, ok := r.closedOwners[id]; ok {
		r.mu.Unlock()
		if closed.pluginID != callerPluginID {
			return false, true
		}
		return true, false
	}
	r.mu.Unlock()
	return false, false
}

func (r *callbackContextRegistry) closeInternal(id string, forget bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	id = strings.TrimSpace(id)
	r.mu.Lock()
	if active, ok := r.contexts[id]; ok {
		tombstoneRoot := !forget || (active.ctx != nil && active.ctx.Err() != nil)
		entries := r.detachSubtreeLocked(id, tombstoneRoot, time.Now())
		r.mu.Unlock()
		closeCallbackContextEntries(entries)
		return
	}
	r.pruneClosedLocked(time.Now())
	r.mu.Unlock()
}

// detachSubtreeLocked makes parent closure and every descendant cancellation
// linearizable with callback commit. Deferred accounting is discarded while
// registry membership is still protected; arbitrary cancel/cleanup functions
// run only after the caller releases r.mu.
func (r *callbackContextRegistry) detachSubtreeLocked(rootID string, tombstoneRoot bool, now time.Time) []callbackContextEntry {
	if r == nil {
		return nil
	}
	if _, ok := r.contexts[rootID]; !ok {
		return nil
	}
	entries := make([]callbackContextEntry, 0, 2)
	queue := []string{rootID}
	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]
		entry, active := r.contexts[currentID]
		if !active {
			continue
		}
		for childID, child := range r.contexts {
			if child.parentID == currentID {
				queue = append(queue, childID)
			}
		}
		delete(r.contexts, currentID)
		if entry.account != nil {
			entry.account.Discard()
		}
		if currentID != rootID || tombstoneRoot {
			r.closedOwners[currentID] = closedCallbackContextEntry{
				pluginID: entry.pluginID,
				canceled: true,
				closedAt: now,
			}
		}
		entries = append(entries, entry)
	}
	r.pruneClosedLocked(now)
	return entries
}

func (r *callbackContextRegistry) pruneClosedLocked(now time.Time) {
	if r == nil || len(r.closedOwners) == 0 {
		return
	}
	if len(r.closedOwners) <= maxClosedCallbackContexts &&
		!r.lastPrune.IsZero() &&
		now.Sub(r.lastPrune) < closedCallbackPruneEvery {
		return
	}
	r.lastPrune = now
	cutoff := now.Add(-closedCallbackContextTTL)
	for id, entry := range r.closedOwners {
		if entry.closedAt.Before(cutoff) {
			delete(r.closedOwners, id)
		}
	}
	excess := len(r.closedOwners) - maxClosedCallbackContexts
	if excess <= 0 {
		return
	}
	type closedItem struct {
		id       string
		closedAt time.Time
	}
	items := make([]closedItem, 0, len(r.closedOwners))
	for id, entry := range r.closedOwners {
		items = append(items, closedItem{id: id, closedAt: entry.closedAt})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].closedAt.Before(items[j].closedAt)
	})
	for index := 0; index < excess; index++ {
		delete(r.closedOwners, items[index].id)
	}
}

func closeCallbackContextEntry(entry callbackContextEntry) {
	if entry.account != nil {
		entry.account.Discard()
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	for _, fn := range entry.cleanup {
		if fn != nil {
			fn()
		}
	}
}

func closeCallbackContextEntries(entries []callbackContextEntry) {
	for index := len(entries) - 1; index >= 0; index-- {
		closeCallbackContextEntry(entries[index])
	}
}

func (r *callbackContextRegistry) commitOwned(id, callerPluginID string) (bool, bool, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return false, false, false
	}
	id = strings.TrimSpace(id)
	callerPluginID = strings.TrimSpace(callerPluginID)

	r.mu.Lock()
	entry, active := r.contexts[id]
	closed := r.closedOwners[id]
	if !active {
		r.mu.Unlock()
		if closed.pluginID != "" && closed.pluginID != callerPluginID {
			return false, true, false
		}
		return false, false, closed.canceled
	}
	if entry.pluginID != callerPluginID {
		r.mu.Unlock()
		return false, true, false
	}
	if entry.ctx == nil || entry.ctx.Err() != nil {
		r.mu.Unlock()
		return false, false, true
	}
	if entry.account == nil {
		r.mu.Unlock()
		return false, false, false
	}
	apply, committed := entry.account.PrepareCommit()
	r.mu.Unlock()
	if !committed {
		return false, false, true
	}
	if apply != nil {
		apply()
	}
	return true, false, false
}

func (r *callbackContextRegistry) pluginID(id string) string {
	if r == nil || id == "" {
		return ""
	}
	r.mu.RLock()
	entry := r.contexts[id]
	r.mu.RUnlock()
	return strings.TrimSpace(entry.pluginID)
}

func (r *callbackContextRegistry) resolveRequired(id string) (context.Context, string, bool, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return nil, "", false, false
	}
	r.mu.RLock()
	entry, ok := r.contexts[strings.TrimSpace(id)]
	closed := r.closedOwners[strings.TrimSpace(id)]
	r.mu.RUnlock()
	if ok && entry.ctx != nil {
		return entry.ctx, strings.TrimSpace(entry.pluginID), true, entry.ctx.Err() != nil
	}
	return nil, strings.TrimSpace(closed.pluginID), false, closed.canceled
}

func (r *callbackContextRegistry) addCleanup(id string, cleanup func()) bool {
	if r == nil || id == "" || cleanup == nil {
		return false
	}
	r.mu.Lock()
	entry, ok := r.contexts[id]
	if ok {
		entry.cleanup = append(entry.cleanup, cleanup)
		r.contexts[id] = entry
	}
	r.mu.Unlock()
	if !ok {
		cleanup()
		return false
	}
	return true
}

func (r *callbackContextRegistry) deferCleanup(id string, cleanup func()) bool {
	if r == nil || id == "" || cleanup == nil {
		return false
	}
	r.mu.Lock()
	entry, ok := r.contexts[id]
	if ok {
		entry.cleanup = append(entry.cleanup, cleanup)
		r.contexts[id] = entry
	}
	r.mu.Unlock()
	return ok
}

func (r *callbackContextRegistry) resolve(id string, fallback context.Context) context.Context {
	if fallback == nil {
		fallback = context.Background()
	}
	if r == nil || id == "" {
		return fallback
	}
	r.mu.RLock()
	ctx := r.contexts[id].ctx
	r.mu.RUnlock()
	if ctx == nil {
		return fallback
	}
	return ctx
}

func (h *Host) openCallbackContext(ctx context.Context) (string, func()) {
	return h.openCallbackContextForPlugin(ctx, "")
}

func (h *Host) openCallbackContextForPlugin(ctx context.Context, pluginID string) (string, func()) {
	if h == nil || h.callbackContexts == nil {
		return "", func() {}
	}
	return h.callbackContexts.open(ctx, pluginID)
}

func (h *Host) addCallbackCleanup(id string, cleanup func()) bool {
	if h == nil || h.callbackContexts == nil {
		if id != "" && cleanup != nil {
			cleanup()
		}
		return false
	}
	return h.callbackContexts.addCleanup(id, cleanup)
}

func (h *Host) deferCallbackCleanup(id string, cleanup func()) bool {
	if h == nil || h.callbackContexts == nil {
		return false
	}
	return h.callbackContexts.deferCleanup(id, cleanup)
}

func (h *Host) resolveCallbackContext(id string, fallback context.Context) context.Context {
	if h == nil || h.callbackContexts == nil {
		if fallback == nil {
			return context.Background()
		}
		return fallback
	}
	return h.callbackContexts.resolve(id, fallback)
}

func (h *Host) callbackContextPluginID(id string) string {
	if h == nil || h.callbackContexts == nil {
		return ""
	}
	return h.callbackContexts.pluginID(id)
}

func (h *Host) forkCallbackContext(parentID, callerPluginID string) (string, bool, bool, bool) {
	if h == nil || h.callbackContexts == nil {
		return "", false, false, false
	}
	return h.callbackContexts.fork(parentID, callerPluginID)
}

func (h *Host) closeOwnedCallbackContext(id, callerPluginID string) (bool, bool) {
	if h == nil || h.callbackContexts == nil {
		return false, false
	}
	return h.callbackContexts.closeOwned(id, callerPluginID)
}

func (h *Host) commitOwnedCallbackContext(id, callerPluginID string) (bool, bool, bool) {
	if h == nil || h.callbackContexts == nil {
		return false, false, false
	}
	return h.callbackContexts.commitOwned(id, callerPluginID)
}

func (h *Host) resolveRequiredCallbackContext(id string) (context.Context, string, bool, bool) {
	if h == nil || h.callbackContexts == nil {
		return nil, "", false, false
	}
	return h.callbackContexts.resolveRequired(id)
}
