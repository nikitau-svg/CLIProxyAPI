package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type cooldownEntry struct {
	Until  time.Time
	Reason string
}

var runtimeState = struct {
	sync.RWMutex
	Cooldowns map[string]cooldownEntry
	Attempts  []attemptRecord
}{
	Cooldowns: make(map[string]cooldownEntry),
}

func buildExecutionPlan(req rpcExecutorRequest, logicalName string, model logicalModel, contract requestCapabilityContract) ([]executionAttempt, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{
		"host_callback_id": req.HostCallbackID,
	})
	if errCall != nil {
		return nil, errCall
	}
	var authResp hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &authResp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host auth list: %w", errUnmarshal)
	}

	sticky := executionStickyKey(req.ExecutorRequest)
	now := time.Now()
	cfg := loadedConfig()
	project, authenticatedProject := authenticatedExecutionProject(req, cfg)
	if authenticatedProject {
		// allowed_auth_ids is an authorization boundary, not an allocator hint.
		// Apply it before provider eligibility, quota observation, primary
		// ordering, and every off/observe/enforce allocator branch.
		authResp.Files = filterProjectAllowedAuths(project, authResp.Files)
	}
	if authenticatedProject && cfg.AllocatorMode != "off" {
		// Refresh stale provider windows concurrently once per plan. Individual
		// allocation passes then consume the cached snapshots without turning a
		// 20-account pool into 20 serial network round trips.
		refreshQuotaSnapshots(req.HostCallbackID, authResp.Files, false)
	}
	plan := make([]executionAttempt, 0)
	for _, item := range model.Candidates {
		resolved, errContract := resolveCandidateContract(item, contract)
		if errContract != nil {
			continue
		}
		eligible := eligibleAuths(resolved, authResp.Files, now)
		orderAuths(eligible, sticky, resolved)
		attempts := make([]executionAttempt, 0, len(eligible))
		if authenticatedProject && cfg.AllocatorMode != "off" {
			allocated := allocateCandidateAuths(req, cfg, project, resolved, eligible, sticky)
			if cfg.AllocatorMode == "enforce" {
				attempts = allocated
			} else {
				// Observe mode executes the pre-v0.4 order while still refreshing
				// quota and calculating the allocation decision.
				for _, auth := range eligible {
					attempts = append(attempts, executionAttempt{Candidate: resolved, Auth: auth})
				}
			}
		} else {
			for _, auth := range eligible {
				attempts = append(attempts, executionAttempt{Candidate: resolved, Auth: auth})
			}
		}
		for _, allocated := range attempts {
			allocated.LogicalModel = logicalName
			allocated.RequestedEffort = requestedEffortValue(contract.Effort)
			allocated.EffectiveEffort = normalizeEffort(resolved.Effort)
			plan = append(plan, allocated)
			if cfg.MaxAttempts > 0 && len(plan) >= cfg.MaxAttempts {
				return plan, nil
			}
		}
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("Bravo has no healthy account for logical model %s", logicalName)
	}
	return plan, nil
}

func filterProjectAllowedAuths(project smartKeyConfig, auths []pluginapi.HostAuthFileEntry) []pluginapi.HostAuthFileEntry {
	if len(project.AllowedAuthIDs) == 0 {
		return auths
	}
	allowed := resolvedProjectAuthIndexes(project.AllowedAuthIDs, auths)
	out := make([]pluginapi.HostAuthFileEntry, 0, len(allowed))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if authIndex == "" {
			continue
		}
		if _, exists := allowed[authIndex]; exists {
			out = append(out, auth)
		}
	}
	return out
}

func resolvedProjectAuthIndexes(configured []string, auths []pluginapi.HostAuthFileEntry) map[string]struct{} {
	resolved := make(map[string]struct{}, len(configured))
	for _, value := range normalizeOpaqueStrings(configured) {
		if authIndex, ok := resolvePrimaryAuthIndex(value, auths); ok && strings.TrimSpace(authIndex) != "" {
			resolved[strings.TrimSpace(authIndex)] = struct{}{}
		}
	}
	return resolved
}

func requestedEffortValue(requested requestEffort) string {
	if !requested.Specified {
		return ""
	}
	return normalizeEffort(requested.Value)
}

func eligibleAuths(item candidate, auths []pluginapi.HostAuthFileEntry, now time.Time) []pluginapi.HostAuthFileEntry {
	provider := normalizeProvider(item.Provider)
	allowedIDs := make(map[string]struct{}, len(item.AuthIDs))
	for _, id := range item.AuthIDs {
		allowedIDs[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		authProvider := normalizeProvider(auth.Provider)
		if authProvider == "" {
			authProvider = normalizeProvider(auth.Type)
		}
		if authProvider != provider {
			continue
		}
		if classifyBravoAuthHealth(provider, auth, now) != bravoAuthReady {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if len(allowedIDs) > 0 {
			if _, ok := allowedIDs[id]; !ok {
				if _, ok = allowedIDs[auth.AuthIndex]; !ok {
					if _, ok = allowedIDs[auth.Name]; !ok {
						continue
					}
				}
			}
		}
		out = append(out, auth)
	}
	return out
}

func orderAuths(auths []pluginapi.HostAuthFileEntry, sticky string, item candidate) {
	sort.SliceStable(auths, func(i, j int) bool {
		if auths[i].Priority != auths[j].Priority {
			return auths[i].Priority > auths[j].Priority
		}
		left := rendezvousScore(sticky, item.Provider, item.Model, authIdentifier(auths[i]))
		right := rendezvousScore(sticky, item.Provider, item.Model, authIdentifier(auths[j]))
		if left == right {
			return authIdentifier(auths[i]) < authIdentifier(auths[j])
		}
		return left > right
	})
}

func executionStickyKey(req pluginapi.ExecutorRequest) string {
	if req.Metadata != nil {
		for _, key := range []string{"idempotency_key", "execution_session_id", "request_id"} {
			if value, ok := req.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	sum := sha256.Sum256(req.OriginalRequest)
	return fmt.Sprintf("%x", sum[:12])
}

func rendezvousScore(parts ...string) uint64 {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return binary.BigEndian.Uint64(sum[:8])
}

func authIdentifier(auth pluginapi.HostAuthFileEntry) string {
	for _, value := range []string{auth.ID, auth.AuthIndex, auth.Name} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func pinnedAuthID(auth pluginapi.HostAuthFileEntry) string {
	return strings.TrimSpace(auth.ID)
}

func cooldownKey(provider, authID string) string {
	return normalizeProvider(provider) + "\x00" + strings.TrimSpace(authID)
}

func cooldownActive(provider, authID string, now time.Time) bool {
	key := cooldownKey(provider, authID)
	runtimeState.RLock()
	entry, ok := runtimeState.Cooldowns[key]
	runtimeState.RUnlock()
	if !ok {
		return false
	}
	if entry.Until.After(now) {
		return true
	}
	runtimeState.Lock()
	delete(runtimeState.Cooldowns, key)
	runtimeState.Unlock()
	return false
}

func setCooldown(provider, authID, reason string, until time.Time) {
	if until.IsZero() || !until.After(time.Now()) {
		return
	}
	runtimeState.Lock()
	runtimeState.Cooldowns[cooldownKey(provider, authID)] = cooldownEntry{Until: until, Reason: reason}
	runtimeState.Unlock()
}

func appendAttempt(record attemptRecord) {
	const maxAttemptRecords = 250
	runtimeState.Lock()
	runtimeState.Attempts = append(runtimeState.Attempts, record)
	if excess := len(runtimeState.Attempts) - maxAttemptRecords; excess > 0 {
		copy(runtimeState.Attempts, runtimeState.Attempts[excess:])
		runtimeState.Attempts = runtimeState.Attempts[:maxAttemptRecords]
	}
	runtimeState.Unlock()
}
