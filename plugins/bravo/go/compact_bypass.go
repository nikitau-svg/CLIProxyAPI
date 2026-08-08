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
const adaptiveLedgerSaturatedMessageRU = "Защитный журнал Bravo переполнен: часть незавершённого расхода сохранена как общий долг. Вторичные маршруты и /compact заблокированы до сверки лимитов и сброса аварийного состояния."

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
	requestShape adaptiveRequestShape,
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
		reservation := adaptiveReservationForShape(auth, tariff, requestShape, time.Now())
		if !compactBypassQuotaEligible(quota, item.Model, authIndex, reservation) {
			continue
		}
		attempts = append(attempts, executionAttempt{
			Candidate:                    item,
			Auth:                         auth,
			ProjectID:                    project.ID,
			AllocatorManaged:             true,
			ReservationPercent:           reservation,
			AdaptiveReserveKey:           adaptiveProfileKey(authIndex, requestShape),
			AdaptiveRequestShape:         requestShape,
			AdaptiveBaselinePercent:      tariff.ReservationPercent,
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
	if adaptiveRoutingSaturated.Load() || adaptiveEstimatorIdentitySaturated(authIndex) {
		return false
	}
	if quotaRoutingConfidenceAt(quota, model, loadedConfig(), time.Now()) != "confirmed" {
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
	release, acquired, failure, _ := acquireExecutionAttemptLeaseDetailed(attempt)
	return release, acquired, failure
}

func acquireExecutionAttemptLeaseDetailed(attempt executionAttempt) (func(bool), bool, *executionFailure, executionAttempt) {
	if attempt.AllocatorObserve && !attempt.CompactBypass {
		release, effectiveAttempt := acquireObserveShadowLease(attempt)
		return release, true, nil, effectiveAttempt
	}
	if !attempt.CompactBypass {
		release, acquired, effectiveAttempt := acquireAttemptLeaseDetailed(attempt)
		if !acquired && attempt.AllocatorManaged {
			decision := effectiveAttempt.AdaptiveTrace
			if failure := adaptiveAdmissionFailure(decision.rejectionCause); failure != nil {
				return release, false, failure, effectiveAttempt
			}
			message := "Подписка временно удерживается адаптивным резервом Bravo; запрос будет направлен в следующий безопасный маршрут."
			if decision.rejection == "adaptive_primary_zero" {
				message = "Основная подписка достигла подтверждённого нулевого остатка; Bravo выбирает следующий безопасный маршрут."
			}
			failure := executionFailure{
				Code:          "bravo_allocator_reserve_floor",
				Message:       message,
				Status:        http.StatusServiceUnavailable,
				Retryable:     true,
				RouteFallback: true,
			}
			return release, false, &failure, effectiveAttempt
		}
		return release, acquired, nil, effectiveAttempt
	}
	if adaptiveRoutingSaturated.Load() {
		failure := executionFailure{
			Code:          "bravo_adaptive_ledger_saturated",
			Message:       adaptiveLedgerSaturatedMessageRU,
			Status:        http.StatusServiceUnavailable,
			Retryable:     false,
			RouteFallback: true,
		}
		return func(bool) {}, false, &failure, attempt
	}
	if adaptiveEstimatorIdentitySaturated(attempt.Auth.AuthIndex) {
		failure := executionFailure{
			Code:          "bravo_adaptive_estimator_saturated",
			Message:       "Оценка расхода этой подписки переполнена неопределёнными данными; /compact заблокирован до подтверждённой сверки лимитов.",
			Status:        http.StatusServiceUnavailable,
			Retryable:     false,
			RouteFallback: true,
		}
		return func(bool) {}, false, &failure, attempt
	}
	cooldown := time.Duration(attempt.CompactBypassCooldownSeconds) * time.Second
	cooldownRelease, wait, acquired := reserveCompactBypass(attempt.CompactBypassKey, cooldown, time.Now())
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
		return func(bool) {}, false, &failure, attempt
	}
	adaptiveRelease, adaptiveAcquired, effectiveAttempt := acquireAttemptLeaseDetailed(attempt)
	if !adaptiveAcquired {
		cooldownRelease(false, time.Now())
		if failure := adaptiveAdmissionFailure(effectiveAttempt.AdaptiveTrace.rejectionCause); failure != nil {
			return func(bool) {}, false, failure, effectiveAttempt
		}
		code := "bravo_compact_adaptive_reserve"
		message := "Подтверждённого остатка подписки недостаточно для безопасного /compact; Bravo попробует следующий маршрут."
		if adaptiveRoutingSaturated.Load() {
			code, message = "bravo_adaptive_ledger_saturated", adaptiveLedgerSaturatedMessageRU
		} else if adaptiveEstimatorIdentitySaturated(attempt.Auth.AuthIndex) {
			code = "bravo_adaptive_estimator_saturated"
			message = "Оценка расхода этой подписки переполнена неопределёнными данными; /compact заблокирован до подтверждённой сверки лимитов."
		}
		failure := executionFailure{
			Code:          code,
			Message:       message,
			Status:        http.StatusServiceUnavailable,
			Retryable:     code == "bravo_compact_adaptive_reserve",
			RouteFallback: true,
		}
		return func(bool) {}, false, &failure, effectiveAttempt
	}
	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			adaptiveRelease(commit)
			cooldownRelease(commit, time.Now())
			if commit {
				logCompactBypassUsage(effectiveAttempt)
			}
		})
	}, true, nil, effectiveAttempt
}

func adaptiveAdmissionFailure(cause adaptiveAdmissionRejectionCause) *executionFailure {
	failure := executionFailure{
		Status:        http.StatusServiceUnavailable,
		Retryable:     true,
		RouteFallback: true,
	}
	switch cause {
	case adaptiveRejectionLedgerSaturated:
		failure.Code = "bravo_adaptive_ledger_saturated"
		failure.Message = adaptiveLedgerSaturatedMessageRU
		failure.Retryable = false
	case adaptiveRejectionEstimatorSaturated:
		failure.Code = "bravo_adaptive_estimator_saturated"
		failure.Message = "Оценщик расхода Bravo переполнен и временно закрыл эту подписку для безопасного вторичного маршрута. Выполните сверку лимитов в админке Bravo."
	case adaptiveRejectionDemandSaturated:
		failure.Code = "bravo_adaptive_demand_saturated"
		failure.Message = "Учёт спроса проектов Bravo переполнен и временно защищает подписку от вторичного расхода. Выполните сверку лимитов в админке Bravo."
	case adaptiveRejectionDurabilityUnavailable:
		failure.Code = "bravo_adaptive_durability_unavailable"
		failure.Message = "Bravo не смог надёжно записать резерв запроса на диск и не отправил запрос провайдеру. Проверьте доступность каталога состояния."
	case adaptiveRejectionQuotaStale:
		failure.Code = "bravo_adaptive_quota_stale"
		failure.Message = "Квота подписки устарела или ещё не подтверждена; запрос провайдеру не отправлен. Обновите квоты в админке Bravo."
	case adaptiveRejectionPrimaryZero:
		failure.Code = "bravo_adaptive_primary_zero"
		failure.Message = "Основная подписка достигла подтверждённого нулевого остатка; запрос провайдеру не отправлен. Дождитесь сброса или увеличьте лимит."
	case adaptiveRejectionConcurrency:
		failure.Code = "bravo_adaptive_concurrency_recheck"
		failure.Message = "Параллельный запрос занял доступный резерв подписки раньше текущего; Bravo попробует следующий безопасный маршрут."
	default:
		return nil
	}
	return &failure
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
