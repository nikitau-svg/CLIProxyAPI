package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var quotaPollingConfigured atomic.Bool

// quotaPollingRuntime owns discovery cadence. Inference only publishes the
// host's already-loaded, secret-free auth view here; provider I/O is performed
// by this worker and never awaited by routing or allocation.
var quotaPollingRuntime = struct {
	sync.Mutex
	running        bool
	hostCallbackID string
	auths          map[string]pluginapi.HostAuthFileEntry
	wake           chan struct{}
	stop           chan struct{}
	done           chan struct{}
}{
	auths: make(map[string]pluginapi.HostAuthFileEntry),
}

func observeQuotaPolling(hostCallbackID string, auths []pluginapi.HostAuthFileEntry) {
	if !quotaPollingConfigured.Load() {
		return
	}
	hostCallbackID = strings.TrimSpace(hostCallbackID)
	if hostCallbackID == "" {
		return
	}
	quotaPollingRuntime.Lock()
	nextAuths := make(map[string]pluginapi.HostAuthFileEntry, len(auths))
	quotaPollingRuntime.hostCallbackID = hostCallbackID
	for _, auth := range auths {
		provider := normalizeProvider(firstNonEmpty(auth.Provider, auth.Type))
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if authIndex == "" || (provider != "claude" && provider != "codex") {
			continue
		}
		nextAuths[authIndex] = auth
	}
	changed := quotaPollingAuthsChanged(quotaPollingRuntime.auths, nextAuths)
	quotaPollingRuntime.auths = nextAuths
	if !quotaPollingRuntime.running && len(nextAuths) == 0 {
		quotaPollingRuntime.Unlock()
		return
	}
	if !quotaPollingRuntime.running {
		quotaPollingRuntime.running = true
		quotaPollingRuntime.wake = make(chan struct{}, 1)
		quotaPollingRuntime.stop = make(chan struct{})
		quotaPollingRuntime.done = make(chan struct{})
		go runQuotaPolling(quotaPollingRuntime.wake, quotaPollingRuntime.stop, quotaPollingRuntime.done)
		changed = true
	}
	wake := quotaPollingRuntime.wake
	quotaPollingRuntime.Unlock()
	if changed {
		nonBlockingQuotaPollingWake(wake)
	}
}

func quotaPollingAuthsChanged(current, next map[string]pluginapi.HostAuthFileEntry) bool {
	if len(current) != len(next) {
		return true
	}
	for authIndex, nextAuth := range next {
		currentAuth, exists := current[authIndex]
		if !exists || currentAuth.ID != nextAuth.ID || currentAuth.Provider != nextAuth.Provider ||
			currentAuth.Type != nextAuth.Type || currentAuth.EgressID != nextAuth.EgressID ||
			currentAuth.Status != nextAuth.Status || currentAuth.Disabled != nextAuth.Disabled ||
			currentAuth.Unavailable != nextAuth.Unavailable {
			return true
		}
	}
	return false
}

func wakeQuotaPolling() {
	quotaPollingRuntime.Lock()
	wake := quotaPollingRuntime.wake
	running := quotaPollingRuntime.running
	quotaPollingRuntime.Unlock()
	if running {
		nonBlockingQuotaPollingWake(wake)
	}
}

func nonBlockingQuotaPollingWake(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func runQuotaPolling(wake <-chan struct{}, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	timer := time.NewTimer(quotaPollingCheckInterval(loadedConfig()))
	defer timer.Stop()
	for {
		select {
		case <-wake:
			runQuotaPollingCycle()
		case <-timer.C:
			runQuotaPollingCycle()
		case <-stop:
			return
		}
		timer.Reset(quotaPollingCheckInterval(loadedConfig()))
	}
}

func runQuotaPollingCycle() {
	quotaPollingRuntime.Lock()
	hostCallbackID := quotaPollingRuntime.hostCallbackID
	auths := make([]pluginapi.HostAuthFileEntry, 0, len(quotaPollingRuntime.auths))
	for _, auth := range quotaPollingRuntime.auths {
		auths = append(auths, auth)
	}
	quotaPollingRuntime.Unlock()
	refreshQuotaSnapshots(hostCallbackID, auths, false)
}

func quotaPollingCheckInterval(cfg pluginConfig) time.Duration {
	usage := time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	if usage <= 0 {
		usage = defaultQuotaUsageRefreshSeconds * time.Second
	}
	interval := usage / 4
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

func stopQuotaPolling() {
	quotaPollingRuntime.Lock()
	if !quotaPollingRuntime.running {
		quotaPollingRuntime.Unlock()
		return
	}
	stop := quotaPollingRuntime.stop
	done := quotaPollingRuntime.done
	quotaPollingRuntime.running = false
	quotaPollingRuntime.stop = nil
	quotaPollingRuntime.done = nil
	quotaPollingRuntime.wake = nil
	quotaPollingRuntime.Unlock()
	close(stop)
	<-done
}

func resetQuotaPollingForTest() {
	stopQuotaPolling()
	quotaPollingRuntime.Lock()
	quotaPollingRuntime.hostCallbackID = ""
	quotaPollingRuntime.auths = make(map[string]pluginapi.HostAuthFileEntry)
	quotaPollingRuntime.Unlock()
	quotaPollingConfigured.Store(false)
}
