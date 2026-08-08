package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCooldownStatePersistsAndReloadsConfigAuthModelScope(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}

	authID := "config-api-key-palantir"
	retryAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	setCooldownWithProviderError(
		"anthropic",
		authID,
		"claude-fable-5(xhigh)",
		"bravo_subscription_model_credits_exhausted",
		retryAt,
		&providererror.Detail{
			Type:             "rate_limit_error",
			Code:             "credits_required",
			Message:          `request_id=req_private payment_method=pm_private`,
			Model:            "claude-fable-5(xhigh)",
			ModelDisplayName: "Fable 5",
			NoticeTitle:      "You've hit your monthly spend limit",
			NoticeText:       "Switch models to continue.",
			DisabledReason:   "org_level_disabled_until",
			Scope:            "model",
			Reason:           "monthly_spend_limit",
		},
	)

	// A client may restart the container immediately after receiving the
	// fallback response. Cooldown persistence therefore cannot rely on the
	// ordinary 250 ms analytics debounce or plugin shutdown.
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	snapshot := string(raw)
	for _, required := range []string{
		`"cooldowns"`,
		`"provider": "claude"`,
		`"auth_id": "config-api-key-palantir"`,
		`"model": "claude-fable-5"`,
		`"code": "credits_required"`,
		`"notice_title": "You've hit your monthly spend limit"`,
	} {
		if !strings.Contains(snapshot, required) {
			t.Fatalf("snapshot lacks %q: %s", required, snapshot)
		}
	}
	for _, forbidden := range []string{
		"claude-fable-5(xhigh)",
		"req_private",
		"pm_private",
		"payment_method",
		"request_id",
	} {
		if strings.Contains(snapshot, forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, snapshot)
		}
	}

	simulateFreshBravoProcess(t, path)

	configAuth := pluginapi.HostAuthFileEntry{
		ID:        authID,
		AuthIndex: "config:claude:palantir",
		Name:      "configured Claude API key",
		Provider:  "claude",
		Source:    "config",
		// Config/API-key auth has no Core .cds model snapshot after restart.
		ModelStates: nil,
	}
	now := time.Now()
	if got := classifyBravoAuthHealthForModel("claude", configAuth, "claude-fable-5", now); got != bravoAuthCooldown {
		t.Fatalf("restored base Fable health = %q, want cooldown", got)
	}
	if got := classifyBravoAuthHealthForModel("claude", configAuth, "claude-fable-5(max)", now); got != bravoAuthCooldown {
		t.Fatalf("restored effort-qualified Fable health = %q, want cooldown", got)
	}
	if got := classifyBravoAuthHealthForModel("claude", configAuth, "claude-sonnet-5", now); got != bravoAuthReady {
		t.Fatalf("same-auth sibling Sonnet health = %q, want ready", got)
	}
	otherAuth := configAuth
	otherAuth.ID = "config-api-key-other"
	if got := classifyBravoAuthHealthForModel("claude", otherAuth, "claude-fable-5", now); got != bravoAuthReady {
		t.Fatalf("other config auth Fable health = %q, want ready", got)
	}

	entries := activeProviderModelCooldowns("anthropic", authID, now)
	if len(entries) != 1 {
		t.Fatalf("restored cooldowns = %#v, want one exact entry", entries)
	}
	entry := entries[0]
	if entry.Provider != "claude" || entry.AuthID != authID ||
		entry.Model != "claude-fable-5" || !entry.Until.Equal(retryAt) {
		t.Fatalf("restored cooldown = %#v", entry)
	}
	if entry.ProviderError.Code != "credits_required" ||
		entry.ProviderError.Model != "claude-fable-5" ||
		entry.ProviderError.Message != "" {
		t.Fatalf("restored provider detail = %#v", entry.ProviderError)
	}
}

func TestCooldownStateLoadPrunesExpiredAndResanitizesPersistedDetail(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	now := time.Now().UTC().Truncate(time.Second)
	fixture := fmt.Sprintf(`{
		"schema_version": 2,
		"cooldowns": {
			"untrusted-active-key": {
				"until": %q,
				"observed_at": %q,
				"reason": "monthly_spend_limit",
				"provider": "anthropic",
				"auth_id": "config-active",
				"model": "claude-fable-5(xhigh)",
				"provider_error": {
					"type": "rate_limit_error",
					"code": "credits_required",
					"message": "request_id=req_fixture password=hunter2",
					"model": "claude-fable-5(xhigh)",
					"model_display_name": "Fable 5",
					"notice_title": "You've hit your monthly spend limit",
					"notice_text": "payment_method=pm_fixture",
					"scope": "model",
					"reason": "monthly_spend_limit",
					"request_id": "req_top_level_should_be_ignored"
				}
			},
			"untrusted-expired-key": {
				"until": %q,
				"provider": "claude",
				"auth_id": "config-expired",
				"model": "claude-fable-5"
			}
		}
	}`,
		now.Add(time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339),
		now.Add(-time.Hour).Format(time.RFC3339),
	)
	if errWrite := os.WriteFile(path, []byte(fixture), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if !cooldownActive("claude", "config-active", "claude-fable-5", now) {
		t.Fatal("active persisted cooldown was not restored")
	}
	if cooldownActive("claude", "config-expired", "claude-fable-5", now) {
		t.Fatal("expired persisted cooldown was restored")
	}
	flushUsageState()

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	snapshot := string(raw)
	if !strings.Contains(snapshot, `"auth_id": "config-active"`) ||
		!strings.Contains(snapshot, `"model": "claude-fable-5"`) {
		t.Fatalf("active canonical cooldown missing after rewrite: %s", snapshot)
	}
	for _, forbidden := range []string{
		"config-expired",
		"xhigh",
		"req_fixture",
		"req_top_level_should_be_ignored",
		"hunter2",
		"pm_fixture",
		"payment_method",
		"request_id",
	} {
		if strings.Contains(snapshot, forbidden) {
			t.Fatalf("rewritten snapshot retained %q: %s", forbidden, snapshot)
		}
	}
}

func TestCooldownSamePathReconfigureMergesConcurrentRuntimeBarrier(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setCooldown(
		"claude",
		"persisted-auth",
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		time.Now().Add(time.Hour),
	)

	// This is the deterministic interleaving of setCooldown's runtime update
	// happening before it obtains usageStateStore.mu. A same-path reconfigure
	// may restore persisted state while that setter waits, but must merge rather
	// than erase the newer live barrier.
	concurrent := cooldownEntry{
		Until:      time.Now().Add(time.Hour),
		ObservedAt: time.Now().UTC(),
		Reason:     "bravo_subscription_model_credits_exhausted",
		Provider:   "claude",
		AuthID:     "concurrent-config-auth",
		Model:      "claude-fable-5",
	}
	runtimeState.Lock()
	runtimeState.Cooldowns[cooldownKey(concurrent.Provider, concurrent.AuthID, concurrent.Model)] = concurrent
	runtimeState.Unlock()

	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if !cooldownActive("claude", concurrent.AuthID, concurrent.Model, time.Now()) {
		t.Fatal("same-path reconfigure erased a concurrent runtime cooldown")
	}
	if !cooldownActive("claude", "persisted-auth", "claude-fable-5", time.Now()) {
		t.Fatal("same-path reconfigure lost the persisted cooldown")
	}
}

func TestCooldownDifferentStatePathReplacesRuntimeBarriers(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	root := t.TempDir()
	firstPath := filepath.Join(root, "first-state.json")
	secondPath := filepath.Join(root, "second-state.json")
	if errConfigure := configureUsageState(firstPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	setCooldown(
		"claude",
		"first-deployment-auth",
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		time.Now().Add(time.Hour),
	)
	if errConfigure := configureUsageState(secondPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if cooldownActive("claude", "first-deployment-auth", "claude-fable-5", time.Now()) {
		t.Fatal("different state_path retained a barrier from the previous deployment")
	}
}

func TestCooldownPersistenceReassertsBarrierAfterStoreMutexInterleaving(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}

	const authID = "concurrent-config-setter"
	bravoUsageState.mu.Lock()
	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		setCooldown(
			"claude",
			authID,
			"claude-fable-5(xhigh)",
			"bravo_subscription_model_credits_exhausted",
			time.Now().Add(time.Hour),
		)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !cooldownActive("claude", authID, "claude-fable-5", time.Now()) &&
		time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cooldownActive("claude", authID, "claude-fable-5", time.Now()) {
		bravoUsageState.mu.Unlock()
		t.Fatal("setter did not install its runtime barrier before waiting for store mutex")
	}

	// Emulate the replacement portion of a new-path reload while the setter is
	// blocked on usageStateStore.mu. Once the mutex is released, the setter must
	// synchronously persist and reassert its barrier.
	runtimeState.Lock()
	runtimeState.Cooldowns = make(map[string]cooldownEntry)
	runtimeState.Unlock()
	bravoUsageState.mu.Unlock()

	select {
	case <-setDone:
	case <-time.After(2 * time.Second):
		t.Fatal("setter deadlocked after the store-mutex interleaving")
	}
	if !cooldownActive("claude", authID, "claude-fable-5", time.Now()) {
		t.Fatal("durable setter did not reassert the runtime barrier after replacement")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(raw), `"auth_id": "concurrent-config-setter"`) {
		t.Fatalf("setter returned before durable snapshot update: %s", raw)
	}
}

func TestCooldownPersistenceRejectsInvertedStaleSameKeyWriteAcrossRestart(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "inverted-cooldown-state.json")
	if err := configureUsageState(path); err != nil {
		t.Fatal(err)
	}
	generation := bravoUsageState.generation.Load()
	now := time.Now().UTC().Truncate(time.Millisecond)
	newer := cooldownEntry{
		Provider:   "claude",
		AuthID:     "same-key-auth",
		Model:      "claude-fable-5",
		Reason:     "newer-observation",
		ObservedAt: now,
		Until:      now.Add(2 * time.Hour),
	}
	older := cooldownEntry{
		Provider:   newer.Provider,
		AuthID:     newer.AuthID,
		Model:      newer.Model,
		Reason:     "older-observation",
		ObservedAt: now.Add(-time.Minute),
		Until:      now.Add(time.Hour),
	}

	// Reproduce the completion inversion directly: the newer setter reaches
	// durable state first, then the older waiter obtains store.mu and receives a
	// higher snapshot sequence. The latter must become an idempotent snapshot of
	// the newer barrier, not a shortening write.
	restoreRuntimeCooldowns(
		map[string]*persistedCooldownEntry{
			cooldownKey(newer.Provider, newer.AuthID, newer.Model): persistedCooldownFromRuntime(newer),
		},
		now,
		false,
	)
	persistRuntimeCooldown(newer, generation)
	persistRuntimeCooldown(older, generation)

	simulateFreshBravoProcess(t, path)
	key := cooldownKey(newer.Provider, newer.AuthID, newer.Model)
	runtimeState.RLock()
	restored, ok := runtimeState.Cooldowns[key]
	runtimeState.RUnlock()
	if !ok {
		t.Fatal("same-key cooldown was lost after restart")
	}
	if !restored.Until.Equal(newer.Until) || !restored.ObservedAt.Equal(newer.ObservedAt) ||
		restored.Reason != newer.Reason {
		t.Fatalf("restart restored stale cooldown %#v, want newer %#v", restored, newer)
	}
}

func TestStaleExpiryCleanupKeepsRefreshedSameKeyBarrier(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	path := filepath.Join(t.TempDir(), "bravo-state.json")
	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}

	const authID = "refreshed-config-auth"
	key := cooldownKey("claude", authID, "claude-fable-5")
	base := time.Now().UTC()
	setCooldown(
		"claude",
		authID,
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		base.Add(time.Hour),
	)
	runtimeState.RLock()
	expiredObservation := runtimeState.Cooldowns[key]
	runtimeState.RUnlock()

	refreshedUntil := base.Add(3 * time.Hour)
	setCooldown(
		"claude",
		authID,
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		refreshedUntil,
	)

	// Cleanup began from the first entry at a clock where that entry is stale.
	// Both cleanup call sites must compare the observed instance, not only its
	// provider/auth/model key, after the same key has been refreshed.
	cleanupNow := base.Add(2 * time.Hour)
	removeExpiredCooldownIfCurrent(key, expiredObservation, cleanupNow)
	removePersistedCooldown(expiredObservation)

	if !cooldownActive("claude", authID, "claude-fable-5", cleanupNow) {
		t.Fatal("stale expiry cleanup deleted the refreshed runtime barrier")
	}
	bravoUsageState.mu.Lock()
	persisted := bravoUsageState.state.Cooldowns[key]
	bravoUsageState.mu.Unlock()
	current, ok := runtimeCooldownFromPersisted(persisted)
	if !ok || !current.Until.Equal(refreshedUntil) {
		t.Fatalf("stale expiry cleanup deleted/refolded refreshed persisted barrier: %#v", persisted)
	}
}

func TestOldStatePathSetterCannotCrossReconfigureGeneration(t *testing.T) {
	restoreUsage := isolateBravoUsageState(t)
	defer restoreUsage()
	isolateBravoFallbackTestState(t)

	root := t.TempDir()
	oldPath := filepath.Join(root, "old-state.json")
	newPath := filepath.Join(root, "new-state.json")
	if errConfigure := configureUsageState(oldPath); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	oldGeneration := bravoUsageState.generation.Load()

	const authID = "old-generation-setter"
	bravoUsageState.mu.Lock()
	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		setCooldown(
			"claude",
			authID,
			"claude-fable-5",
			"bravo_subscription_model_credits_exhausted",
			time.Now().Add(time.Hour),
		)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !cooldownActive("claude", authID, "claude-fable-5", time.Now()) &&
		time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cooldownActive("claude", authID, "claude-fable-5", time.Now()) {
		bravoUsageState.mu.Unlock()
		t.Fatal("old-generation setter did not install runtime state before waiting on store mutex")
	}
	if current := bravoUsageState.generation.Load(); current != oldGeneration {
		bravoUsageState.mu.Unlock()
		t.Fatalf("generation changed before simulated path switch: %d != %d", current, oldGeneration)
	}

	// Deterministically perform the state replacement portion of configure
	// while the old setter waits on this mutex.
	bravoUsageState.path = newPath
	bravoUsageState.state = newPersistedUsageState()
	bravoUsageState.generation.Add(1)
	restoreRuntimeCooldowns(bravoUsageState.state.Cooldowns, time.Now().UTC(), true)
	bravoUsageState.mu.Unlock()

	select {
	case <-setDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation setter deadlocked after path replacement")
	}
	if cooldownActive("claude", authID, "claude-fable-5", time.Now()) {
		t.Fatal("old-generation setter reasserted its barrier into the new state path")
	}
	bravoUsageState.mu.Lock()
	leaked := len(bravoUsageState.state.Cooldowns)
	bravoUsageState.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("old-generation setter wrote %d cooldown(s) into the new state path", leaked)
	}
	if _, errStat := os.Stat(newPath); !os.IsNotExist(errStat) {
		t.Fatalf("old-generation setter created the new snapshot: %v", errStat)
	}

	// Sequential work that starts after the path switch belongs to the new
	// generation and must persist normally.
	setCooldown(
		"claude",
		"new-generation-setter",
		"claude-fable-5",
		"bravo_subscription_model_credits_exhausted",
		time.Now().Add(time.Hour),
	)
	if !cooldownActive("claude", "new-generation-setter", "claude-fable-5", time.Now()) {
		t.Fatal("new-generation setter did not install its runtime barrier")
	}
	if _, errStat := os.Stat(newPath); errStat != nil {
		t.Fatalf("new-generation setter did not persist to switched state path: %v", errStat)
	}
}

func simulateFreshBravoProcess(t *testing.T, path string) {
	t.Helper()
	runtimeState.Lock()
	runtimeState.Cooldowns = make(map[string]cooldownEntry)
	runtimeState.Unlock()

	bravoUsageState.mu.Lock()
	if bravoUsageState.saveTimer != nil {
		bravoUsageState.saveTimer.Stop()
	}
	bravoUsageState.path = ""
	bravoUsageState.state = newPersistedUsageState()
	bravoUsageState.saveTimer = nil
	bravoUsageState.savePendingSince = time.Time{}
	bravoUsageState.mu.Unlock()

	if errConfigure := configureUsageState(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
}
