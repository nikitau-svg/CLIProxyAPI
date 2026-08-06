package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const compactBypassWarningRU = "Команда /compact временно использовала резерв Claude ниже внутреннего порога Bravo и могла уменьшить доступный лимит подписки."

type claudeRequestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeRequestContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type compactBypassLeaseState struct {
	sync.Mutex
	NextAllowed map[string]time.Time
	InFlight    map[string]bool
}

var compactBypassRuntime = compactBypassLeaseState{
	NextAllowed: make(map[string]time.Time),
	InFlight:    make(map[string]bool),
}

func claudeCLICompactBypassKey(req rpcExecutorRequest, project smartKeyConfig) (string, bool) {
	if normalizeContractProtocol(requestProtocol(req.ExecutorRequest)) != protocolClaude {
		return "", false
	}
	if !strings.Contains(strings.ToLower(req.Headers.Get("User-Agent")), "claude-cli/") {
		return "", false
	}
	sessionID := strings.TrimSpace(req.Headers.Get("X-Claude-Code-Session-Id"))
	projectID := strings.TrimSpace(project.ID)
	if sessionID == "" || projectID == "" || !isClaudeCLICompactPrompt(executionBody(req)) {
		return "", false
	}
	return projectID + "\x00" + sessionID, true
}

func isClaudeCLICompactPrompt(body []byte) bool {
	var payload struct {
		Messages []claudeRequestMessage `json:"messages"`
	}
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return false
	}
	for index := len(payload.Messages) - 1; index >= 0; index-- {
		message := payload.Messages[index]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		text := strings.TrimSpace(claudeMessageText(message.Content))
		if text == "" {
			return false
		}
		return strings.HasPrefix(text, "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.") &&
			strings.Contains(text, "an <analysis> block followed by a <summary> block") &&
			strings.Contains(text, "Tool calls will be rejected and you will fail the task")
	}
	return false
}

func claudeMessageText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []claudeRequestContent
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.EqualFold(strings.TrimSpace(block.Type), "text") && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func compactBypassCandidateAttempts(
	req rpcExecutorRequest,
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	sticky string,
) []executionAttempt {
	if cfg.CompactBypassCooldownSeconds <= 0 || normalizeProvider(item.Provider) != "claude" {
		return nil
	}
	key, compact := claudeCLICompactBypassKey(req, project)
	if !compact {
		return nil
	}

	attempts := make([]executionAttempt, 0, len(auths))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		if authIndex == "" {
			continue
		}
		subscription := subscriptionPolicy(cfg, authIndex)
		if !subscriptionEnabled(subscription) {
			continue
		}
		quota := quotaSnapshot(authIndex)
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
		if !compactBypassQuotaEligible(quota, item.Model, authIndex, tariff.ReservationPercent) {
			continue
		}
		attempts = append(attempts, executionAttempt{
			Candidate:                    item,
			Auth:                         auth,
			ProjectID:                    project.ID,
			ReservationPercent:           tariff.ReservationPercent,
			TariffID:                     tariff.ID,
			CompactBypass:                true,
			CompactBypassKey:             key,
			CompactBypassCooldownSeconds: cfg.CompactBypassCooldownSeconds,
		})
	}

	sort.SliceStable(attempts, func(i, j int) bool {
		left := allocatorStress(cfg, attempts[i])
		right := allocatorStress(cfg, attempts[j])
		if math.Abs(left-right) > 0.000001 {
			return left < right
		}
		leftTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(attempts[i].Auth))
		rightTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(attempts[j].Auth))
		return leftTie > rightTie
	})
	return attempts
}

func compactBypassQuotaEligible(
	quota credentialQuotaState,
	model, authIndex string,
	reservation float64,
) bool {
	if quotaConfidence(quota) != "confirmed" {
		return false
	}
	session, weekly := effectiveQuotaWindows(quota, model)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[strings.TrimSpace(authIndex)] +
		allocatorRuntime.PendingPercent[strings.TrimSpace(authIndex)]
	allocatorRuntime.Unlock()
	return session.RemainingPercent-reserved-reservation > 0 &&
		weekly.RemainingPercent-reserved-reservation > 0
}

func acquireExecutionAttemptLease(attempt executionAttempt) (func(bool), bool, *executionFailure) {
	if !attempt.CompactBypass {
		release, acquired := acquireAttemptLease(attempt)
		return release, acquired, nil
	}
	cooldown := time.Duration(attempt.CompactBypassCooldownSeconds) * time.Second
	release, wait, acquired := reserveCompactBypass(attempt.CompactBypassKey, cooldown, time.Now())
	if !acquired {
		seconds := int(math.Ceil(wait.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		failure := executionFailure{
			Code:          "bravo_compact_bypass_cooldown",
			Message:       fmt.Sprintf("Повторный доступ /compact к резерву Claude будет доступен через %d сек.", seconds),
			Status:        http.StatusTooManyRequests,
			Retryable:     true,
			RouteFallback: true,
			RetryAfter:    strconv.Itoa(seconds),
		}
		return func(bool) {}, false, &failure
	}
	return func(commit bool) {
		release(commit, time.Now())
		if commit {
			logCompactBypassUsage(attempt)
		}
	}, true, nil
}

func reserveCompactBypass(
	key string,
	cooldown time.Duration,
	now time.Time,
) (func(bool, time.Time), time.Duration, bool) {
	key = strings.TrimSpace(key)
	if key == "" || cooldown <= 0 {
		return func(bool, time.Time) {}, cooldown, false
	}
	compactBypassRuntime.Lock()
	for expiredKey, next := range compactBypassRuntime.NextAllowed {
		if !compactBypassRuntime.InFlight[expiredKey] && !next.After(now) {
			delete(compactBypassRuntime.NextAllowed, expiredKey)
		}
	}
	if compactBypassRuntime.InFlight[key] {
		compactBypassRuntime.Unlock()
		return func(bool, time.Time) {}, cooldown, false
	}
	if next := compactBypassRuntime.NextAllowed[key]; next.After(now) {
		wait := next.Sub(now)
		compactBypassRuntime.Unlock()
		return func(bool, time.Time) {}, wait, false
	}
	compactBypassRuntime.InFlight[key] = true
	compactBypassRuntime.Unlock()

	var once sync.Once
	return func(commit bool, completedAt time.Time) {
		once.Do(func() {
			compactBypassRuntime.Lock()
			delete(compactBypassRuntime.InFlight, key)
			if commit {
				compactBypassRuntime.NextAllowed[key] = completedAt.Add(cooldown)
			}
			compactBypassRuntime.Unlock()
		})
	}, 0, true
}

func compactBypassResponseWarning(headers http.Header, metadata map[string]any, attempt executionAttempt) {
	if !attempt.CompactBypass {
		return
	}
	if headers != nil {
		headers.Set("X-Bravo-Warning-Code", "compact-bypass-consumed-claude-reserve")
		headers.Set("X-Bravo-Warning", compactBypassWarningRU)
	}
	if metadata != nil {
		metadata["bravo_compact_bypass"] = true
		metadata["bravo_warning"] = compactBypassWarningRU
	}
}

func logCompactBypassUsage(attempt executionAttempt) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   "warn",
		"message": "Bravo: /compact использовал резерв Claude ниже внутреннего порога",
		"fields": map[string]any{
			"project_id":     attempt.ProjectID,
			"logical_model":  attempt.LogicalModel,
			"physical_model": attempt.Candidate.Model,
			"subscription":   stableAuthIndex(attempt.Auth),
			"tariff":         attempt.TariffID,
			"warning":        compactBypassWarningRU,
		},
	})
}
