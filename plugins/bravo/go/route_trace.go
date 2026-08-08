package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	routeTraceSchemaVersion        = 1
	defaultRouteTraceLimit         = 2000
	defaultRouteTraceTTL           = 30 * 24 * time.Hour
	maxPersistedRouteTraceAttempts = 64
)

// routeTrace is a bounded diagnostic record. It deliberately contains no
// request/response bodies, headers, plaintext keys, OAuth tokens, or raw
// provider messages.
type routeTrace struct {
	TraceID        string                   `json:"trace_id"`
	StartedAt      time.Time                `json:"started_at"`
	CompletedAt    time.Time                `json:"completed_at,omitempty"`
	ProjectID      string                   `json:"project_id,omitempty"`
	LogicalModel   string                   `json:"logical_model,omitempty"`
	SourceProtocol string                   `json:"source_protocol,omitempty"`
	Stream         bool                     `json:"stream,omitempty"`
	Status         int                      `json:"status,omitempty"`
	Success        bool                     `json:"success"`
	Outcome        string                   `json:"outcome,omitempty"`
	FinalCode      string                   `json:"final_code,omitempty"`
	FinalMessage   string                   `json:"final_message,omitempty"`
	ClientAction   string                   `json:"client_action,omitempty"`
	TotalLatencyMS int64                    `json:"total_latency_ms,omitempty"`
	Attempts       []routeTraceAttempt      `json:"attempts,omitempty"`
	AttemptSummary routeTraceAttemptSummary `json:"attempt_summary"`
}

type routeTraceAttemptSummary struct {
	Total     int `json:"total"`
	Persisted int `json:"persisted"`
	Omitted   int `json:"omitted"`
}

type routeTraceAttempt struct {
	Ordinal                     int       `json:"ordinal"`
	At                          time.Time `json:"at,omitempty"`
	Provider                    string    `json:"provider,omitempty"`
	Model                       string    `json:"model,omitempty"`
	SubscriptionID              string    `json:"subscription_id,omitempty"`
	SubscriptionLabel           string    `json:"subscription_label,omitempty"`
	Status                      int       `json:"status,omitempty"`
	Success                     bool      `json:"success"`
	Outcome                     string    `json:"outcome,omitempty"`
	Decision                    string    `json:"decision,omitempty"`
	Committed                   bool      `json:"committed"`
	RequestedEffort             string    `json:"requested_effort,omitempty"`
	EffectiveEffort             string    `json:"effective_effort,omitempty"`
	LatencyMS                   int64     `json:"latency_ms,omitempty"`
	TTFBMS                      int64     `json:"ttfb_ms,omitempty"`
	FirstContentMS              int64     `json:"first_content_ms,omitempty"`
	ErrorCode                   string    `json:"error_code,omitempty"`
	ErrorMessage                string    `json:"error_message,omitempty"`
	ProviderErrorType           string    `json:"provider_error_type,omitempty"`
	ProviderErrorCode           string    `json:"provider_error_code,omitempty"`
	ProviderErrorScope          string    `json:"provider_error_scope,omitempty"`
	FailureClass                string    `json:"failure_class,omitempty"`
	ProviderStarted             *bool     `json:"provider_started,omitempty"`
	ProviderExecutionAmbiguous  bool      `json:"provider_execution_ambiguous,omitempty"`
	RetryAfter                  string    `json:"retry_after,omitempty"`
	RequiredInputTokens         int64     `json:"required_input_tokens,omitempty"`
	SupportedInputTokens        int64     `json:"supported_input_tokens,omitempty"`
	ReservationPercent          float64   `json:"reservation_percent,omitempty"`
	ProjectRole                 string    `json:"project_role,omitempty"`
	AllocatorMode               string    `json:"allocator_mode,omitempty"`
	AdaptiveDecision            string    `json:"adaptive_decision,omitempty"`
	AdaptiveRejection           string    `json:"adaptive_rejection,omitempty"`
	AdmissionRejectionCause     string    `json:"admission_rejection_cause,omitempty"`
	AdaptiveFallback            string    `json:"adaptive_fallback,omitempty"`
	SessionHeadroomBefore       float64   `json:"session_headroom_before_percent,omitempty"`
	SessionHeadroomAfter        float64   `json:"session_headroom_after_percent,omitempty"`
	WeeklyHeadroomBefore        float64   `json:"weekly_headroom_before_percent,omitempty"`
	WeeklyHeadroomAfter         float64   `json:"weekly_headroom_after_percent,omitempty"`
	SessionExposureGuardPercent float64   `json:"session_exposure_guard_percent,omitempty"`
	WeeklyExposureGuardPercent  float64   `json:"weekly_exposure_guard_percent,omitempty"`
	DemandGuardPercent          float64   `json:"demand_guard_percent,omitempty"`
	PendingGuardPercent         float64   `json:"pending_guard_percent,omitempty"`
	InFlightGuardPercent        float64   `json:"in_flight_guard_percent,omitempty"`
	FallbackProvider            string    `json:"fallback_provider,omitempty"`
	FallbackModel               string    `json:"fallback_model,omitempty"`
}

type routeTraceSnapshot struct {
	SchemaVersion int          `json:"schema_version"`
	Revision      uint64       `json:"revision,omitempty"`
	UpdatedAt     time.Time    `json:"updated_at,omitempty"`
	Traces        []routeTrace `json:"traces"`
}

type routeTraceQuery struct {
	ProjectID  string
	TraceID    string
	ErrorsOnly bool
	Limit      int
}

type routeTraceStorageStatus struct {
	QueueDepth          int    `json:"queue_depth"`
	QueueCapacity       int    `json:"queue_capacity"`
	PersistenceDrops    uint64 `json:"persistence_drops"`
	PersistenceFailures uint64 `json:"persistence_failures"`
	WALRecords          int    `json:"wal_records"`
	WALBytes            int64  `json:"wal_bytes"`
}

type routeTraceStore struct {
	appendMu             sync.Mutex
	mu                   sync.Mutex
	path                 string
	walPath              string
	loaded               bool
	closed               bool
	traces               []routeTrace
	maxEntries           int
	retention            time.Duration
	loadError            string
	nextRevision         uint64
	snapshotRevision     uint64
	walRecords           int
	walBytes             int64
	maxWALRecords        int
	maxWALBytes          int64
	compactAfterRecords  int
	persistQueue         chan routeTracePersistRequest
	persistDone          chan struct{}
	persistenceDrops     uint64
	persistenceFailures  uint64
	terminalWaitTimeout  time.Duration
	terminalQueueTimeout time.Duration
	beforePersist        func()
	beforeSnapshot       func() error
	beforeWALReset       func() error
	memoryOnly           bool
}

var bravoRouteTraceStores = struct {
	sync.RWMutex
	store *routeTraceStore
}{store: newRouteTraceStore(defaultStatePath)}

var routeTraceConfigureMu sync.Mutex

func newRouteTraceStore(statePath string) *routeTraceStore {
	store := &routeTraceStore{
		path:                 routeTracePath(statePath),
		walPath:              routeTraceWALPath(statePath),
		maxEntries:           defaultRouteTraceLimit,
		retention:            defaultRouteTraceTTL,
		compactAfterRecords:  128,
		maxWALRecords:        1024,
		maxWALBytes:          16 << 20,
		persistQueue:         make(chan routeTracePersistRequest, 128),
		persistDone:          make(chan struct{}),
		terminalWaitTimeout:  500 * time.Millisecond,
		terminalQueueTimeout: 250 * time.Millisecond,
	}
	go store.persistenceLoop()
	return store
}

func newMemoryRouteTraceStore() *routeTraceStore {
	return &routeTraceStore{
		loaded:     true,
		memoryOnly: true,
		maxEntries: defaultRouteTraceLimit,
		retention:  defaultRouteTraceTTL,
		loadError:  "Хранилище трасс переключается; завершённые трассы временно удерживаются в ограниченной памяти.",
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

func routeTraceWALPath(statePath string) string {
	return routeTracePath(statePath) + ".wal"
}

func configureRouteTraceStore(statePath string) error {
	routeTraceConfigureMu.Lock()
	defer routeTraceConfigureMu.Unlock()

	// Publish an in-memory handoff before filesystem I/O. Requests remain
	// available while the previous store fsyncs or a legacy file is recovered.
	transition := newMemoryRouteTraceStore()
	bravoRouteTraceStores.Lock()
	previous := bravoRouteTraceStores.store
	bravoRouteTraceStores.store = transition
	bravoRouteTraceStores.Unlock()

	closeWarning := ""
	var closeFallback []routeTrace
	if previous != nil {
		previous.mu.Lock()
		_ = previous.loadLocked()
		closeFallback = cloneRouteTraces(previous.traces)
		previous.mu.Unlock()
		if errClose := previous.close(); errClose != nil {
			closeWarning = "Предыдущее хранилище трасс не удалось полностью сбросить; проверьте доступность каталога состояния."
		}
	}
	store := newRouteTraceStore(statePath)
	if errLoad := store.load(); errLoad != nil {
		// Observability must never make model execution unavailable. Keep the
		// reviewed error for the authenticated management response and recover
		// with a fresh bounded snapshot on the next completed route.
		_ = store.recoverAfterLoadFailure()
	}
	if closeWarning != "" {
		store.mu.Lock()
		store.mergeFallbackTracesLocked(closeFallback)
		if store.loadError == "" {
			store.loadError = closeWarning
		}
		store.mu.Unlock()
		_ = store.flush()
	}

	// Freeze the bounded handoff only for the short in-memory merge and pointer
	// swap. No filesystem operation runs under the global request barrier.
	bravoRouteTraceStores.Lock()
	transition.mu.Lock()
	transition.closed = true
	handoff := cloneRouteTraces(transition.traces)
	transition.mu.Unlock()
	store.mu.Lock()
	store.mergeFallbackTracesLocked(handoff)
	store.mu.Unlock()
	bravoRouteTraceStores.store = store
	bravoRouteTraceStores.Unlock()
	if len(handoff) > 0 {
		if errFlush := store.flush(); errFlush != nil {
			store.setPersistenceWarning()
		}
	}
	return nil
}

func withRouteTraceStore(fn func(*routeTraceStore)) {
	bravoRouteTraceStores.RLock()
	defer bravoRouteTraceStores.RUnlock()
	if store := bravoRouteTraceStores.store; store != nil {
		fn(store)
	}
}

func readRouteTraceStore[T any](fn func(*routeTraceStore) T) (out T) {
	bravoRouteTraceStores.RLock()
	defer bravoRouteTraceStores.RUnlock()
	if store := bravoRouteTraceStores.store; store != nil {
		return fn(store)
	}
	return out
}

func appendCurrentRouteTrace(trace routeTrace, durable bool) error {
	bravoRouteTraceStores.RLock()
	defer bravoRouteTraceStores.RUnlock()
	store := bravoRouteTraceStores.store
	if store == nil {
		return nil
	}
	if durable {
		return store.appendDurable(trace)
	}
	store.append(trace)
	return nil
}

func listCurrentRouteTraces(query routeTraceQuery, now time.Time) ([]routeTrace, string, error) {
	bravoRouteTraceStores.RLock()
	defer bravoRouteTraceStores.RUnlock()
	store := bravoRouteTraceStores.store
	if store == nil {
		return nil, "", nil
	}
	traces, errList := store.list(query, now)
	return traces, store.warning(), errList
}

func currentRouteTraceStorageStatus() routeTraceStorageStatus {
	return readRouteTraceStore(func(store *routeTraceStore) routeTraceStorageStatus {
		store.mu.Lock()
		defer store.mu.Unlock()
		return routeTraceStorageStatus{
			QueueDepth:          len(store.persistQueue),
			QueueCapacity:       cap(store.persistQueue),
			PersistenceDrops:    store.persistenceDrops,
			PersistenceFailures: store.persistenceFailures,
			WALRecords:          store.walRecords,
			WALBytes:            store.walBytes,
		}
	})
}

func replaceRouteTraceStoreForTest(store *routeTraceStore) func() {
	bravoRouteTraceStores.Lock()
	previous := bravoRouteTraceStores.store
	bravoRouteTraceStores.store = store
	bravoRouteTraceStores.Unlock()
	return func() {
		bravoRouteTraceStores.Lock()
		bravoRouteTraceStores.store = previous
		bravoRouteTraceStores.Unlock()
		_ = store.close()
	}
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
	// Serialize revision assignment with queue admission so WAL records are
	// always ordered even when requests complete concurrently.
	store.appendMu.Lock()
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		store.appendMu.Unlock()
		return errRouteTraceStoreClosed
	}
	if errLoad := store.loadLocked(); errLoad != nil {
		// Diagnostics remain fail-soft: a broken backing path must not discard
		// the terminal trace from bounded memory or affect provider execution.
		store.loaded = true
		store.traces = nil
		store.nextRevision = 0
		store.snapshotRevision = 0
		store.walRecords = 0
		store.walBytes = 0
		store.loadError = "Хранилище трасс недоступно; новые трассы остаются в ограниченной памяти до восстановления диска."
	}
	trace = sanitizeRouteTrace(trace)
	if trace.TraceID == "" {
		trace.TraceID = newRouteTraceID()
	}
	if trace.StartedAt.IsZero() {
		trace.StartedAt = time.Now().UTC()
	}
	store.traces = append(store.traces, trace)
	store.trimCountLocked()
	store.nextRevision++
	record := routeTraceWALRecord{SchemaVersion: routeTraceSchemaVersion, Revision: store.nextRevision, Trace: trace}
	store.mu.Unlock()
	if store.memoryOnly {
		store.appendMu.Unlock()
		return nil
	}

	request := routeTracePersistRequest{kind: routeTracePersistAppend, record: record}
	if durable {
		request.ack = make(chan error, 1)
		if errQueue := store.enqueueDurable(request); errQueue != nil {
			store.appendMu.Unlock()
			store.setPersistenceWarning()
			return errQueue
		}
		store.appendMu.Unlock()
		select {
		case errPersist := <-request.ack:
			if errPersist != nil {
				store.setPersistenceWarning()
			}
			return errPersist
		case <-time.After(store.terminalWaitTimeout):
			store.setPersistenceWarning()
			return errRouteTracePersistenceTimeout
		}
	}
	select {
	case store.persistQueue <- request:
	default:
		store.mu.Lock()
		store.persistenceDrops++
		store.loadError = "Очередь сохранения трасс перегружена; часть успешных трасс доступна только в памяти до следующего снимка."
		store.mu.Unlock()
	}
	store.appendMu.Unlock()
	return nil
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

func sanitizeRouteTrace(trace routeTrace) routeTrace {
	totalAttempts := len(trace.Attempts)
	if trace.AttemptSummary.Total > totalAttempts {
		totalAttempts = trace.AttemptSummary.Total
	}
	if len(trace.Attempts) > maxPersistedRouteTraceAttempts {
		last := trace.Attempts[len(trace.Attempts)-1]
		trace.Attempts = append([]routeTraceAttempt(nil), trace.Attempts[:maxPersistedRouteTraceAttempts-1]...)
		trace.Attempts = append(trace.Attempts, last)
	} else {
		trace.Attempts = append([]routeTraceAttempt(nil), trace.Attempts...)
	}
	trace.AttemptSummary = routeTraceAttemptSummary{
		Total:     totalAttempts,
		Persisted: len(trace.Attempts),
		Omitted:   totalAttempts - len(trace.Attempts),
	}
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
		if attempt.Ordinal <= 0 {
			attempt.Ordinal = index + 1
		}
		attempt.Provider = safeRouteTraceIdentifier(attempt.Provider)
		attempt.Model = safeRouteTraceModel(attempt.Model)
		attempt.SubscriptionID = safeRouteTraceIdentifier(attempt.SubscriptionID)
		// Presentation labels are joined from the live, authenticated account
		// list. Persisting notes or email addresses would make the trace file an
		// unnecessary second source of personal data.
		attempt.SubscriptionLabel = ""
		attempt.ErrorCode = safeRouteTraceIdentifier(attempt.ErrorCode)
		attempt.ErrorMessage = routeTraceMessageRU(attempt.ErrorCode)
		attempt.ProviderErrorType = safeRouteTraceIdentifier(attempt.ProviderErrorType)
		attempt.ProviderErrorCode = safeRouteTraceIdentifier(attempt.ProviderErrorCode)
		attempt.ProviderErrorScope = safeRouteTraceIdentifier(attempt.ProviderErrorScope)
		attempt.Outcome = safeRouteTraceIdentifier(attempt.Outcome)
		attempt.Decision = safeRouteTraceIdentifier(attempt.Decision)
		attempt.RequestedEffort = safeRouteTraceIdentifier(attempt.RequestedEffort)
		attempt.EffectiveEffort = safeRouteTraceIdentifier(attempt.EffectiveEffort)
		attempt.FailureClass = safeRouteTraceIdentifier(attempt.FailureClass)
		attempt.RetryAfter = safeRouteTraceIdentifier(attempt.RetryAfter)
		attempt.ProjectRole = safeRouteTraceIdentifier(attempt.ProjectRole)
		attempt.AllocatorMode = safeRouteTraceIdentifier(attempt.AllocatorMode)
		attempt.AdaptiveDecision = safeRouteTraceIdentifier(attempt.AdaptiveDecision)
		attempt.AdaptiveRejection = safeRouteTraceIdentifier(attempt.AdaptiveRejection)
		attempt.AdmissionRejectionCause = safeRouteTraceIdentifier(attempt.AdmissionRejectionCause)
		attempt.AdaptiveFallback = safeRouteTraceIdentifier(attempt.AdaptiveFallback)
		attempt.FallbackProvider = safeRouteTraceIdentifier(attempt.FallbackProvider)
		attempt.FallbackModel = safeRouteTraceModel(attempt.FallbackModel)
		attempt.ReservationPercent = safeRouteTracePercent(attempt.ReservationPercent)
		attempt.SessionHeadroomBefore = safeRouteTraceSignedPercent(attempt.SessionHeadroomBefore)
		attempt.SessionHeadroomAfter = safeRouteTraceSignedPercent(attempt.SessionHeadroomAfter)
		attempt.WeeklyHeadroomBefore = safeRouteTraceSignedPercent(attempt.WeeklyHeadroomBefore)
		attempt.WeeklyHeadroomAfter = safeRouteTraceSignedPercent(attempt.WeeklyHeadroomAfter)
		attempt.SessionExposureGuardPercent = safeRouteTracePercent(attempt.SessionExposureGuardPercent)
		attempt.WeeklyExposureGuardPercent = safeRouteTracePercent(attempt.WeeklyExposureGuardPercent)
		attempt.DemandGuardPercent = safeRouteTracePercent(attempt.DemandGuardPercent)
		attempt.PendingGuardPercent = safeRouteTracePercent(attempt.PendingGuardPercent)
		attempt.InFlightGuardPercent = safeRouteTracePercent(attempt.InFlightGuardPercent)
		if attempt.LatencyMS < 0 {
			attempt.LatencyMS = 0
		}
	}
	trace.FinalCode = dominantAdaptiveRouteTraceCode(trace)
	trace.FinalMessage = routeTraceActionRU(trace)
	trace.ClientAction = routeTraceClientAction(trace)
	return trace
}

func dominantAdaptiveRouteTraceCode(trace routeTrace) string {
	if trace.Success {
		return trace.FinalCode
	}
	if trace.FinalCode != "" && trace.FinalCode != "bravo_route_temporarily_unavailable" {
		return trace.FinalCode
	}
	for _, attempt := range trace.Attempts {
		if attempt.ErrorCode == "bravo_context_window_exceeded" {
			return "bravo_context_window_exceeded"
		}
	}
	bestCode := ""
	bestPriority := 0
	allAdaptive := len(trace.Attempts) > 0
	for _, attempt := range trace.Attempts {
		code := strings.TrimSpace(attempt.ErrorCode)
		if code == "" {
			switch attempt.AdmissionRejectionCause {
			case "durability_unavailable":
				code = "bravo_adaptive_durability_unavailable"
			case "ledger_saturated":
				code = "bravo_adaptive_ledger_saturated"
			case "estimator_saturated":
				code = "bravo_adaptive_estimator_saturated"
			case "demand_saturated":
				code = "bravo_adaptive_demand_saturated"
			case "floor":
				code = "bravo_allocator_reserve_floor"
			case "concurrency":
				code = "bravo_adaptive_concurrency_recheck"
			}
		}
		if priority := adaptiveFailureCodePriority(code); priority > bestPriority {
			bestCode, bestPriority = code, priority
		} else if priority == 0 {
			allAdaptive = false
		}
	}
	if allAdaptive && bestCode != "" {
		return bestCode
	}
	return trace.FinalCode
}

func safeRouteTracePercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	value = math.Min(math.Max(value, 0), 100)
	return math.Round(value*1000) / 1000
}

func safeRouteTraceSignedPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	value = math.Min(math.Max(value, -100), 100)
	return math.Round(value*1000) / 1000
}

func routeTraceClientAction(trace routeTrace) string {
	if trace.Success {
		return "none"
	}
	for _, attempt := range trace.Attempts {
		if attempt.ErrorCode == "bravo_context_window_exceeded" ||
			attempt.ErrorCode == "bravo_context_target_incompatible" {
			return "compact"
		}
	}
	switch trace.FinalCode {
	case "bravo_subscription_quota_exhausted", "bravo_subscription_model_credits_exhausted":
		return "raise_quota"
	case "bravo_adaptive_ledger_saturated", "bravo_adaptive_estimator_saturated", "bravo_adaptive_demand_saturated":
		return "reconcile_limits"
	case "bravo_adaptive_durability_unavailable":
		return "check_storage"
	case "bravo_adaptive_quota_stale":
		return "refresh_quota"
	case "bravo_adaptive_primary_zero":
		return "raise_quota"
	case "bravo_allocator_reserve_floor":
		return "adjust_reserve"
	case "bravo_subscription_auth_unavailable", "authentication_error":
		return "reauth"
	case "bravo_route_temporarily_unavailable", "bravo_adaptive_concurrency_recheck", "overloaded_error", "server_error":
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
	case "bravo_adaptive_ledger_saturated":
		return adaptiveLedgerSaturatedMessageRU
	case "bravo_adaptive_estimator_saturated":
		return "Оценщик расхода Bravo переполнен; выполните сверку лимитов в админке Bravo."
	case "bravo_adaptive_demand_saturated":
		return "Учёт спроса проектов Bravo переполнен; выполните сверку лимитов в админке Bravo."
	case "bravo_adaptive_durability_unavailable":
		return "Bravo не смог надёжно записать резерв на диск; проверьте каталог состояния."
	case "bravo_adaptive_concurrency_recheck":
		return "Параллельный запрос занял доступный резерв; Bravo выберет следующий безопасный маршрут."
	case "bravo_adaptive_quota_stale":
		return "Квота подписки устарела или ещё не подтверждена; обновите квоты в админке Bravo."
	case "bravo_adaptive_primary_zero":
		return "Основная подписка достигла подтверждённого нулевого остатка."
	case "bravo_subscription_auth_unavailable", "authentication_error":
		return "Авторизация подписки недоступна."
	case "bravo_subscription_access_denied", "permission_error":
		return "Подписка не имеет доступа к этому запросу."
	case "overloaded_error":
		return "Провайдер временно перегружен."
	case "server_error", "bravo_provider_stream_error":
		return "Провайдер завершил запрос внутренней ошибкой."
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
	var claudeLimit *routeTraceAttempt
	var claudeModelCredits *routeTraceAttempt
	var claudeReserve *routeTraceAttempt
	for index := range trace.Attempts {
		attempt := &trace.Attempts[index]
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
