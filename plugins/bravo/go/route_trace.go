package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	routeTraceSchemaVersion = 1
	defaultRouteTraceLimit  = 2000
	defaultRouteTraceTTL    = 30 * 24 * time.Hour
)

// routeTrace is a bounded diagnostic record. It deliberately contains no
// request/response bodies, headers, plaintext keys, OAuth tokens, or raw
// provider messages.
type routeTrace struct {
	TraceID        string              `json:"trace_id"`
	StartedAt      time.Time           `json:"started_at"`
	CompletedAt    time.Time           `json:"completed_at,omitempty"`
	ProjectID      string              `json:"project_id,omitempty"`
	LogicalModel   string              `json:"logical_model,omitempty"`
	SourceProtocol string              `json:"source_protocol,omitempty"`
	Stream         bool                `json:"stream,omitempty"`
	Status         int                 `json:"status,omitempty"`
	Success        bool                `json:"success"`
	Outcome        string              `json:"outcome,omitempty"`
	FinalCode      string              `json:"final_code,omitempty"`
	FinalMessage   string              `json:"final_message,omitempty"`
	ClientAction   string              `json:"client_action,omitempty"`
	TotalLatencyMS int64               `json:"total_latency_ms,omitempty"`
	Attempts       []routeTraceAttempt `json:"attempts,omitempty"`
}

type routeTraceAttempt struct {
	Ordinal              int       `json:"ordinal"`
	At                   time.Time `json:"at,omitempty"`
	Provider             string    `json:"provider,omitempty"`
	Model                string    `json:"model,omitempty"`
	SubscriptionID       string    `json:"subscription_id,omitempty"`
	SubscriptionLabel    string    `json:"subscription_label,omitempty"`
	Status               int       `json:"status,omitempty"`
	Success              bool      `json:"success"`
	Outcome              string    `json:"outcome,omitempty"`
	Decision             string    `json:"decision,omitempty"`
	Committed            bool      `json:"committed"`
	RequestedEffort      string    `json:"requested_effort,omitempty"`
	EffectiveEffort      string    `json:"effective_effort,omitempty"`
	LatencyMS            int64     `json:"latency_ms,omitempty"`
	TTFBMS               int64     `json:"ttfb_ms,omitempty"`
	FirstContentMS       int64     `json:"first_content_ms,omitempty"`
	ErrorCode            string    `json:"error_code,omitempty"`
	ErrorMessage         string    `json:"error_message,omitempty"`
	DiagnosticStage      string    `json:"diagnostic_stage,omitempty"`
	RequiredCapability   string    `json:"required_capability,omitempty"`
	ParameterPath        string    `json:"parameter_path,omitempty"`
	ProviderErrorType    string    `json:"provider_error_type,omitempty"`
	ProviderErrorCode    string    `json:"provider_error_code,omitempty"`
	ProviderErrorParam   string    `json:"provider_error_parameter,omitempty"`
	ProviderErrorScope   string    `json:"provider_error_scope,omitempty"`
	FailureClass         string    `json:"failure_class,omitempty"`
	RetryAfter           string    `json:"retry_after,omitempty"`
	RequiredInputTokens  int64     `json:"required_input_tokens,omitempty"`
	SupportedInputTokens int64     `json:"supported_input_tokens,omitempty"`
}

type routeTraceSnapshot struct {
	SchemaVersion int          `json:"schema_version"`
	UpdatedAt     time.Time    `json:"updated_at,omitempty"`
	Traces        []routeTrace `json:"traces"`
}

type routeTraceQuery struct {
	ProjectID  string
	TraceID    string
	ErrorsOnly bool
	Limit      int
}

type routeTraceStore struct {
	mu         sync.Mutex
	path       string
	loaded     bool
	traces     []routeTrace
	maxEntries int
	retention  time.Duration
	saveTimer  *time.Timer
	dirty      bool
	loadError  string
}

var bravoRouteTraces = newRouteTraceStore(defaultStatePath)

func newRouteTraceStore(statePath string) *routeTraceStore {
	return &routeTraceStore{
		path:       routeTracePath(statePath),
		maxEntries: defaultRouteTraceLimit,
		retention:  defaultRouteTraceTTL,
	}
}

func routeTracePath(statePath string) string {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		statePath = defaultStatePath
	}
	base := strings.TrimSuffix(statePath, filepath.Ext(statePath))
	return base + "-route-traces.json"
}

func configureRouteTraceStore(statePath string) error {
	if bravoRouteTraces != nil {
		if errFlush := bravoRouteTraces.flush(); errFlush != nil {
			return errFlush
		}
	}
	store := newRouteTraceStore(statePath)
	if errLoad := store.load(); errLoad != nil {
		// Observability must never make model execution unavailable. Keep the
		// reviewed error for the authenticated management response and recover
		// with a fresh bounded snapshot on the next completed route.
		store.loadError = "Предыдущий файл трасс повреждён или несовместим; новые трассы записываются в свежий безопасный снимок."
		store.loaded = true
		store.traces = nil
	}
	bravoRouteTraces = store
	return nil
}

func (store *routeTraceStore) warning() string {
	if store == nil {
		return ""
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return strings.TrimSpace(store.loadError)
}

func (store *routeTraceStore) append(trace routeTrace) {
	store.appendWithDurability(trace, false)
}

// appendDurable persists a terminal diagnostic before returning. Successful
// traces remain batched because rewriting the bounded snapshot on every
// request would add avoidable disk and CPU load. A failed route is rare and
// operationally important enough to require restart-safe durability.
func (store *routeTraceStore) appendDurable(trace routeTrace) error {
	return store.appendWithDurability(trace, true)
}

func (store *routeTraceStore) appendWithDurability(trace routeTrace, durable bool) error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	_ = store.loadLocked()
	trace = sanitizeRouteTrace(trace)
	if trace.TraceID == "" {
		trace.TraceID = newRouteTraceID()
	}
	if trace.StartedAt.IsZero() {
		trace.StartedAt = time.Now().UTC()
	}
	store.traces = append(store.traces, trace)
	store.pruneLocked(time.Now().UTC())
	store.dirty = true
	if durable {
		if store.saveTimer != nil {
			store.saveTimer.Stop()
			store.saveTimer = nil
		}
		if errSave := store.saveLocked(); errSave != nil {
			store.loadError = "Не удалось сохранить аварийную трассу на диск; она остаётся доступна в памяти до перезапуска."
			store.scheduleSaveLocked()
			return errSave
		}
		store.dirty = false
		return nil
	}
	store.scheduleSaveLocked()
	return nil
}

func (store *routeTraceStore) scheduleSaveLocked() {
	if store.saveTimer == nil {
		store.saveTimer = time.AfterFunc(250*time.Millisecond, func() {
			_ = store.flush()
		})
	}
}

func (store *routeTraceStore) list(query routeTraceQuery, now time.Time) ([]routeTrace, error) {
	if store == nil {
		return nil, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if errLoad := store.loadLocked(); errLoad != nil {
		return nil, errLoad
	}
	store.pruneLocked(now.UTC())
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]routeTrace, 0, limit)
	for index := len(store.traces) - 1; index >= 0 && len(out) < limit; index-- {
		trace := store.traces[index]
		if query.ProjectID != "" && trace.ProjectID != query.ProjectID {
			continue
		}
		if query.TraceID != "" && trace.TraceID != query.TraceID {
			continue
		}
		if query.ErrorsOnly && trace.Success {
			continue
		}
		out = append(out, trace)
	}
	return out, nil
}

func (store *routeTraceStore) load() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadLocked()
}

func (store *routeTraceStore) loadLocked() error {
	if store.loaded {
		return nil
	}
	store.loaded = true
	raw, errRead := os.ReadFile(store.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read Bravo route traces: %w", errRead)
	}
	var snapshot routeTraceSnapshot
	if errDecode := json.Unmarshal(raw, &snapshot); errDecode != nil {
		return fmt.Errorf("decode Bravo route traces: %w", errDecode)
	}
	if snapshot.SchemaVersion != routeTraceSchemaVersion {
		return fmt.Errorf("unsupported Bravo route trace schema %d", snapshot.SchemaVersion)
	}
	store.traces = make([]routeTrace, 0, len(snapshot.Traces))
	for _, trace := range snapshot.Traces {
		store.traces = append(store.traces, sanitizeRouteTrace(trace))
	}
	return nil
}

func (store *routeTraceStore) flush() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if errLoad := store.loadLocked(); errLoad != nil {
		return errLoad
	}
	if store.saveTimer != nil {
		store.saveTimer.Stop()
		store.saveTimer = nil
	}
	if !store.dirty {
		return nil
	}
	if errSave := store.saveLocked(); errSave != nil {
		return errSave
	}
	store.dirty = false
	return nil
}

func (store *routeTraceStore) saveLocked() error {
	store.pruneLocked(time.Now().UTC())
	if errMkdir := os.MkdirAll(filepath.Dir(store.path), 0o700); errMkdir != nil {
		return fmt.Errorf("create Bravo route trace directory: %w", errMkdir)
	}
	snapshot := routeTraceSnapshot{
		SchemaVersion: routeTraceSchemaVersion,
		UpdatedAt:     time.Now().UTC(),
		Traces:        store.traces,
	}
	raw, errEncode := json.Marshal(snapshot)
	if errEncode != nil {
		return fmt.Errorf("encode Bravo route traces: %w", errEncode)
	}
	temporary := store.path + ".tmp"
	if errWrite := os.WriteFile(temporary, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write Bravo route traces: %w", errWrite)
	}
	if errChmod := os.Chmod(temporary, 0o600); errChmod != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("protect Bravo route traces: %w", errChmod)
	}
	if errRename := os.Rename(temporary, store.path); errRename != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace Bravo route traces: %w", errRename)
	}
	return nil
}

func (store *routeTraceStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-store.retention)
	kept := store.traces[:0]
	for _, trace := range store.traces {
		if !trace.StartedAt.IsZero() && trace.StartedAt.Before(cutoff) {
			continue
		}
		kept = append(kept, trace)
	}
	store.traces = kept
	sort.SliceStable(store.traces, func(i, j int) bool {
		return store.traces[i].StartedAt.Before(store.traces[j].StartedAt)
	})
	if excess := len(store.traces) - store.maxEntries; excess > 0 {
		store.traces = append([]routeTrace(nil), store.traces[excess:]...)
	}
}

func sanitizeRouteTrace(trace routeTrace) routeTrace {
	trace.TraceID = safeRouteTraceIdentifier(trace.TraceID)
	trace.ProjectID = safeRouteTraceIdentifier(trace.ProjectID)
	trace.LogicalModel = safeRouteTraceModel(trace.LogicalModel)
	trace.SourceProtocol = safeRouteTraceIdentifier(trace.SourceProtocol)
	trace.FinalCode = safeRouteTraceIdentifier(trace.FinalCode)
	trace.Outcome = safeRouteTraceIdentifier(trace.Outcome)
	trace.ClientAction = safeRouteTraceIdentifier(trace.ClientAction)
	if trace.CompletedAt.Before(trace.StartedAt) {
		trace.CompletedAt = time.Time{}
	}
	if trace.TotalLatencyMS < 0 {
		trace.TotalLatencyMS = 0
	}
	for index := range trace.Attempts {
		attempt := &trace.Attempts[index]
		attempt.Ordinal = index + 1
		attempt.Provider = safeRouteTraceIdentifier(attempt.Provider)
		attempt.Model = safeRouteTraceModel(attempt.Model)
		attempt.SubscriptionID = safeRouteTraceIdentifier(attempt.SubscriptionID)
		// Presentation labels are joined from the live, authenticated account
		// list. Persisting notes or email addresses would make the trace file an
		// unnecessary second source of personal data.
		attempt.SubscriptionLabel = ""
		attempt.ErrorCode = safeRouteTraceIdentifier(attempt.ErrorCode)
		attempt.DiagnosticStage = safeRouteTraceIdentifier(attempt.DiagnosticStage)
		attempt.RequiredCapability = safeRouteTraceIdentifier(attempt.RequiredCapability)
		attempt.ParameterPath = safeRouteTraceParameter(attempt.ParameterPath)
		attempt.ProviderErrorType = safeRouteTraceIdentifier(attempt.ProviderErrorType)
		attempt.ProviderErrorCode = safeRouteTraceIdentifier(attempt.ProviderErrorCode)
		attempt.ProviderErrorParam = safeRouteTraceParameter(attempt.ProviderErrorParam)
		attempt.ProviderErrorScope = safeRouteTraceIdentifier(attempt.ProviderErrorScope)
		attempt.Outcome = safeRouteTraceIdentifier(attempt.Outcome)
		attempt.Decision = safeRouteTraceIdentifier(attempt.Decision)
		attempt.RequestedEffort = safeRouteTraceIdentifier(attempt.RequestedEffort)
		attempt.EffectiveEffort = safeRouteTraceIdentifier(attempt.EffectiveEffort)
		attempt.FailureClass = safeRouteTraceIdentifier(attempt.FailureClass)
		attempt.RetryAfter = safeRouteTraceIdentifier(attempt.RetryAfter)
		if attempt.LatencyMS < 0 {
			attempt.LatencyMS = 0
		}
		attempt.ErrorMessage = routeTraceAttemptMessageRU(*attempt)
	}
	trace.FinalMessage = routeTraceActionRU(trace)
	trace.ClientAction = routeTraceClientAction(trace)
	return trace
}

func routeTraceClientAction(trace routeTrace) string {
	if trace.Success {
		return "none"
	}
	for _, attempt := range trace.Attempts {
		if attempt.DiagnosticStage != "" || attempt.FailureClass == "invalid_request" {
			return "fix_request"
		}
		if attempt.ErrorCode == "bravo_context_window_exceeded" ||
			attempt.ErrorCode == "bravo_context_target_incompatible" {
			return "compact"
		}
	}
	switch trace.FinalCode {
	case "bravo_provider_invalid_request", "invalid_request_error", "invalid_tool_parameters", "invalid_function_parameters",
		"bravo_contract_unavailable", "bravo_contract_unverified", "bravo_capability_conflict", "bravo_capability_undeclared":
		return "fix_request"
	case "bravo_subscription_quota_exhausted", "bravo_subscription_model_credits_exhausted":
		return "raise_quota"
	case "bravo_subscription_auth_unavailable", "authentication_error":
		return "reauth"
	case "bravo_route_temporarily_unavailable", "overloaded_error", "server_error":
		return "retry"
	default:
		return "none"
	}
}

func safeRouteTraceIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		value = value[:160]
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:/-", char) {
			continue
		}
		return ""
	}
	return value
}

func safeRouteTraceModel(value string) string {
	return safeRouteTraceIdentifier(value)
}

func safeRouteTraceParameter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return ""
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-',
			char == '.',
			char == '[',
			char == ']',
			char == '$':
		default:
			return ""
		}
	}
	return value
}

func routeTraceAttemptMessageRU(attempt routeTraceAttempt) string {
	if attempt.DiagnosticStage != "" {
		path := strings.TrimSpace(attempt.ParameterPath)
		capability := strings.TrimSpace(attempt.RequiredCapability)
		switch {
		case path != "" && capability != "":
			return fmt.Sprintf("Локальная проверка запроса отклонила capability %s в поле %s до обращения к провайдеру.", capability, path)
		case capability != "":
			return fmt.Sprintf("Локальная проверка запроса отклонила capability %s до обращения к провайдеру.", capability)
		default:
			return "Локальная проверка отклонила параметры запроса до обращения к провайдеру."
		}
	}
	if attempt.ProviderErrorParam != "" &&
		(attempt.FailureClass == "invalid_request" || attempt.ProviderErrorType == "invalid_request_error") {
		return fmt.Sprintf("Провайдер отклонил параметр запроса %s. Проверьте объявление инструмента и переданные аргументы.", attempt.ProviderErrorParam)
	}
	return routeTraceMessageRU(attempt.ErrorCode)
}

func routeTraceMessageRU(code string) string {
	switch strings.TrimSpace(code) {
	case "bravo_context_window_exceeded":
		return "Контекст запроса не помещается в окно выбранной модели. Выполните /compact или начните новую сессию."
	case "bravo_subscription_quota_exhausted", "rate_limit_error", "rate_limited":
		return "Подписка достигла подтверждённого лимита провайдера."
	case "bravo_subscription_model_credits_exhausted":
		return "Для этой модели исчерпан отдельный лимит расходов у провайдера."
	case "bravo_allocator_reserve_floor":
		return "Подписка достигла внутреннего резервного порога проекта Bravo."
	case "bravo_subscription_auth_unavailable", "authentication_error":
		return "Авторизация подписки недоступна."
	case "bravo_subscription_access_denied", "permission_error":
		return "Подписка не имеет доступа к этому запросу."
	case "overloaded_error":
		return "Провайдер временно перегружен."
	case "server_error", "bravo_provider_stream_error":
		return "Провайдер завершил запрос внутренней ошибкой."
	case "bravo_provider_invalid_request", "invalid_request_error", "invalid_tool_parameters", "invalid_function_parameters":
		return "Провайдер отклонил параметры запроса или инструмента. Проверьте JSON-схему и аргументы tool-вызова."
	case "bravo_capability_conflict", "bravo_capability_undeclared", "bravo_contract_unverified", "bravo_contract_unavailable":
		return "Локальная матрица совместимости отклонила контракт запроса до обращения к провайдеру."
	case "bravo_route_temporarily_unavailable":
		return "Все безопасные варианты маршрута временно недоступны."
	case "":
		return ""
	default:
		return "Запрос завершился классифицированной ошибкой; откройте попытки маршрута для деталей."
	}
}

func routeTraceActionRU(trace routeTrace) string {
	var contextAttempt *routeTraceAttempt
	var requestDiagnostic *routeTraceAttempt
	var claudeLimit *routeTraceAttempt
	var claudeModelCredits *routeTraceAttempt
	var claudeReserve *routeTraceAttempt
	for index := range trace.Attempts {
		attempt := &trace.Attempts[index]
		if requestDiagnostic == nil && (attempt.DiagnosticStage != "" ||
			attempt.FailureClass == "invalid_request" ||
			attempt.ProviderErrorType == "invalid_request_error") {
			requestDiagnostic = attempt
		}
		switch attempt.ErrorCode {
		case "bravo_context_window_exceeded":
			contextAttempt = attempt
		case "bravo_allocator_reserve_floor", "bravo_allocator_withheld":
			if attempt.Provider == "claude" {
				claudeReserve = attempt
			}
		case "bravo_subscription_quota_exhausted", "rate_limit_error", "rate_limited":
			if attempt.Provider == "claude" {
				claudeLimit = attempt
			}
		case "bravo_subscription_model_credits_exhausted":
			if attempt.Provider == "claude" {
				claudeModelCredits = attempt
			}
		}
	}
	if requestDiagnostic != nil && contextAttempt == nil {
		return requestDiagnostic.ErrorMessage
	}
	if contextAttempt != nil {
		target := friendlyModelName(contextAttempt.Model)
		switch {
		case claudeReserve != nil:
			return fmt.Sprintf(
				"Claude не был вызван из-за внутреннего резервного порога Bravo; fallback в %s не вместил контекст. Поднимите внутренний порог, выполните /compact или начните новую сессию.",
				target,
			)
		case claudeModelCredits != nil:
			return fmt.Sprintf(
				"У модели %s исчерпан отдельный лимит расходов у Claude; fallback в %s не вместил контекст. Увеличьте лимит расходов или смените модель Claude, выполните /compact либо начните новую сессию.",
				friendlyModelName(claudeModelCredits.Model),
				target,
			)
		case claudeLimit != nil:
			return fmt.Sprintf(
				"Claude подтвердил лимит подписки; fallback в %s не вместил контекст. Дождитесь сброса Claude, выполните /compact или начните новую сессию.",
				target,
			)
		default:
			return fmt.Sprintf(
				"Модель %s не вместила контекст, а совместимый маршрут с доказанно большим окном не найден. Выполните /compact или начните новую сессию.",
				target,
			)
		}
	}
	return routeTraceMessageRU(trace.FinalCode)
}

func newRouteTraceID() string {
	var data [12]byte
	if _, errRead := rand.Read(data[:]); errRead == nil {
		return "trc_" + hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("trc_%d", time.Now().UTC().UnixNano())
}
