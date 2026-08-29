package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	adaptiveShadowAuditSchemaVersion     = 1
	adaptiveShadowAuditQueueCapacity     = 1024
	adaptiveShadowAuditMemoryRecords     = 4096
	adaptiveShadowAuditAttemptsPerRecord = 16
	adaptiveShadowAuditFileBytes         = 4 * 1024 * 1024
	adaptiveShadowAuditTotalBytes        = 2 * adaptiveShadowAuditFileBytes
	adaptiveShadowAuditMaximumRecord     = 32 * 1024
	adaptiveShadowAuditFlushInterval     = time.Second
	adaptiveShadowAuditDefaultPeriod     = 24 * time.Hour
	adaptiveShadowAuditReviewRequests    = 100
	adaptiveShadowAuditReviewCoverage    = 6 * time.Hour
	adaptiveShadowAuditAggregatePeriod   = 7 * 24 * time.Hour
)

type adaptiveShadowAuditHour struct {
	Hour                     time.Time `json:"hour"`
	First                    time.Time `json:"first"`
	Last                     time.Time `json:"last"`
	Requests                 int       `json:"requests"`
	SuccessfulRequests       int       `json:"successful_requests"`
	FailedRequests           int       `json:"failed_requests"`
	ActualAttempts           int       `json:"actual_attempts"`
	FallbackRequests         int       `json:"fallback_requests"`
	RoutingEnforced          bool      `json:"routing_enforced"`
	RoutingChanges           int       `json:"routing_changes"`
	AdditionalRequests       int       `json:"additional_requests"`
	WouldAdmit               int       `json:"would_admit"`
	WouldWithhold            int       `json:"would_withhold"`
	LegacyAttempts           int       `json:"legacy_attempts"`
	Unknown                  int       `json:"unknown"`
	SuccessfulWithhold       int       `json:"successful_withhold"`
	QuotaFailuresAdmit       int       `json:"quota_failures_admit"`
	TokenAttempts            int       `json:"token_attempts"`
	TokenFirst               time.Time `json:"token_first,omitempty"`
	TokenLast                time.Time `json:"token_last,omitempty"`
	TokenWouldAdmit          int       `json:"token_would_admit"`
	TokenWouldWithhold       int       `json:"token_would_withhold"`
	TokenUnknown             int       `json:"token_unknown"`
	TokenSuccessfulWithhold  int       `json:"token_successful_withhold"`
	TokenQuotaFailuresAdmit  int       `json:"token_quota_failures_admit"`
	EdgeAttempts             int       `json:"edge_attempts"`
	EdgeFirst                time.Time `json:"edge_first,omitempty"`
	EdgeLast                 time.Time `json:"edge_last,omitempty"`
	EdgeGreen                int       `json:"edge_green"`
	EdgeGuarded              int       `json:"edge_guarded"`
	EdgeTripped              int       `json:"edge_tripped"`
	EdgeHalfOpen             int       `json:"edge_half_open"`
	EdgeDispatch             int       `json:"edge_dispatch"`
	EdgeProbe                int       `json:"edge_probe"`
	EdgeSkipBusy             int       `json:"edge_skip_busy"`
	EdgeSkipTripped          int       `json:"edge_skip_tripped"`
	EdgeSuccessfulSkip       int       `json:"edge_successful_skip"`
	EdgeQuotaFailureSkip     int       `json:"edge_quota_failure_skip"`
	EdgeQuotaFailureDispatch int       `json:"edge_quota_failure_dispatch"`
	EdgeTrips                int       `json:"edge_trips"`
	EdgeReopens              int       `json:"edge_reopens"`
	AssistActuallyDeferred   int       `json:"assist_actually_deferred"`
	AssistTailReached        int       `json:"assist_tail_reached"`
	AssistTailDispatched     int       `json:"assist_tail_dispatched"`
	AssistTailSuccess        int       `json:"assist_tail_success"`
	AssistNeighborSuccess    int       `json:"assist_neighbor_success"`
	AssistLostTail           int       `json:"assist_lost_tail"`
	AssistDuplicateTail      int       `json:"assist_duplicate_tail"`
	AssistPrimaryDeferred    int       `json:"assist_primary_deferred"`
	AssistStreamHedge        int       `json:"assist_stream_hedge"`
	AssistRequests           int       `json:"assist_requests"`
	AssistSuccessfulRequests int       `json:"assist_successful_requests"`
	AssistFailedRequests     int       `json:"assist_failed_requests"`
	AssistTailNotReached     int       `json:"assist_tail_not_reached"`
	AssistTerminalBeforeTail int       `json:"assist_terminal_before_tail"`
}

type adaptiveShadowAuditAttempt struct {
	Provider                      string  `json:"provider"`
	Model                         string  `json:"model"`
	Primary                       bool    `json:"primary"`
	Decision                      string  `json:"decision"`
	EstimateConfidence            string  `json:"estimate_confidence"`
	ReservationPercent            float64 `json:"reservation_percent"`
	SessionReservationPercent     float64 `json:"session_reservation_percent,omitempty"`
	WeeklyReservationPercent      float64 `json:"weekly_reservation_percent,omitempty"`
	ModelWeeklyReservationPercent float64 `json:"model_weekly_reservation_percent,omitempty"`
	ModelWeeklyName               string  `json:"model_weekly_name,omitempty"`
	PredictedTokens               float64 `json:"predicted_tokens,omitempty"`
	PendingPercent                float64 `json:"pending_percent"`
	SafeHeadroomBefore            float64 `json:"safe_headroom_before"`
	SafeHeadroomAfter             float64 `json:"safe_headroom_after"`
	Outcome                       string  `json:"outcome"`
	Status                        int     `json:"status"`
	Success                       bool    `json:"success"`
	ProviderAcceptance            string  `json:"provider_acceptance"`
	LatencyMilliseconds           int64   `json:"latency_ms"`
	ErrorCode                     string  `json:"error_code,omitempty"`
	EdgeGateState                 string  `json:"edge_gate_state,omitempty"`
	EdgeGateDecision              string  `json:"edge_gate_decision,omitempty"`
	EdgeGateReason                string  `json:"edge_gate_reason,omitempty"`
	EdgeGateQuotaConfirmed        bool    `json:"edge_gate_quota_confirmed,omitempty"`
	EdgeGateSessionHeadroom       float64 `json:"edge_gate_session_headroom_percent,omitempty"`
	EdgeGateWeeklyHeadroom        float64 `json:"edge_gate_weekly_headroom_percent,omitempty"`
	EdgeGateTripRemainingSeconds  int64   `json:"edge_gate_trip_remaining_seconds,omitempty"`
	EdgeGateOutcomeTransition     string  `json:"edge_gate_outcome_transition,omitempty"`
	AssistLifecycle               string  `json:"assist_lifecycle,omitempty"`
}

type adaptiveShadowAuditRecord struct {
	SchemaVersion              int                          `json:"schema_version"`
	Sequence                   uint64                       `json:"sequence,omitempty"`
	At                         time.Time                    `json:"at"`
	TraceID                    string                       `json:"trace_id"`
	LogicalModel               string                       `json:"logical_model"`
	Stream                     bool                         `json:"stream"`
	Success                    bool                         `json:"success"`
	Status                     int                          `json:"status"`
	ActualExecutionAttempts    int                          `json:"actual_execution_attempts"`
	OmittedAttempts            int                          `json:"omitted_attempts,omitempty"`
	FallbackUsed               bool                         `json:"fallback_used"`
	RoutingEnforced            bool                         `json:"routing_enforced"`
	RoutingChangesApplied      int                          `json:"routing_changes_applied"`
	AdditionalProviderRequests int                          `json:"additional_provider_requests"`
	Attempts                   []adaptiveShadowAuditAttempt `json:"attempts"`
	AssistActuallyDeferred     int                          `json:"assist_actually_deferred,omitempty"`
	AssistTailReached          int                          `json:"assist_tail_reached,omitempty"`
	AssistTailDispatched       int                          `json:"assist_tail_dispatched,omitempty"`
	AssistTailSuccess          int                          `json:"assist_tail_success,omitempty"`
	AssistNeighborSuccess      int                          `json:"assist_neighbor_success,omitempty"`
	AssistLostTail             int                          `json:"assist_lost_tail,omitempty"`
	AssistDuplicateTail        int                          `json:"assist_duplicate_tail,omitempty"`
	AssistPrimaryDeferred      int                          `json:"assist_primary_deferred,omitempty"`
	AssistStreamHedge          int                          `json:"assist_stream_hedge,omitempty"`
	AssistRequests             int                          `json:"assist_requests,omitempty"`
	AssistSuccessfulRequests   int                          `json:"assist_successful_requests,omitempty"`
	AssistFailedRequests       int                          `json:"assist_failed_requests,omitempty"`
	AssistTailNotReached       int                          `json:"assist_tail_not_reached,omitempty"`
	AssistTerminalBeforeTail   int                          `json:"assist_terminal_before_tail,omitempty"`
}

type adaptiveShadowAuditReport struct {
	SchemaVersion                       int                         `json:"schema_version"`
	Status                              string                      `json:"status"`
	Verdict                             string                      `json:"verdict"`
	VerdictMessage                      string                      `json:"verdict_message"`
	Mode                                string                      `json:"mode"`
	Effect                              string                      `json:"effect"`
	RoutingEnforced                     bool                        `json:"routing_enforced"`
	From                                time.Time                   `json:"from"`
	To                                  time.Time                   `json:"to"`
	RequestsObserved                    int                         `json:"requests_observed"`
	SuccessfulRequests                  int                         `json:"successful_requests"`
	FailedRequests                      int                         `json:"failed_requests"`
	ActualExecutionAttempts             int                         `json:"actual_execution_attempts"`
	RequestsWithFallback                int                         `json:"requests_with_fallback"`
	CoverageSeconds                     int64                       `json:"coverage_seconds"`
	MinimumReviewRequests               int                         `json:"minimum_review_requests"`
	MinimumReviewCoverage               int64                       `json:"minimum_review_coverage_seconds"`
	WouldAdmitAttempts                  int                         `json:"would_admit_attempts"`
	WouldWithholdAttempts               int                         `json:"would_withhold_attempts"`
	UnknownDecisionAttempts             int                         `json:"unknown_decision_attempts"`
	SuccessfulWouldWithhold             int                         `json:"successful_would_withhold"`
	QuotaFailuresWouldAdmit             int                         `json:"quota_failures_would_admit"`
	TokenCalibratedAttempts             int                         `json:"token_calibrated_attempts"`
	TokenCalibratedWouldAdmit           int                         `json:"token_calibrated_would_admit"`
	TokenCalibratedWouldWithhold        int                         `json:"token_calibrated_would_withhold"`
	TokenCalibratedUnknown              int                         `json:"token_calibrated_unknown"`
	SuccessfulTokenCalibratedWithhold   int                         `json:"successful_token_calibrated_would_withhold"`
	TokenCalibratedQuotaFailuresOnAdmit int                         `json:"token_calibrated_quota_failures_would_admit"`
	TokenCalibratedCoverageSeconds      int64                       `json:"token_calibrated_coverage_seconds"`
	TokenCalibrationVerdict             string                      `json:"token_calibration_verdict"`
	TokenCalibrationVerdictMessage      string                      `json:"token_calibration_verdict_message"`
	LegacyShapeEstimateAttempts         int                         `json:"legacy_shape_estimate_attempts"`
	EdgeGateAttempts                    int                         `json:"edge_gate_attempts"`
	EdgeGateGreenAttempts               int                         `json:"edge_gate_green_attempts"`
	EdgeGateGuardedAttempts             int                         `json:"edge_gate_guarded_attempts"`
	EdgeGateTrippedAttempts             int                         `json:"edge_gate_tripped_attempts"`
	EdgeGateHalfOpenAttempts            int                         `json:"edge_gate_half_open_attempts"`
	EdgeGateWouldDispatch               int                         `json:"edge_gate_would_dispatch"`
	EdgeGateWouldProbe                  int                         `json:"edge_gate_would_probe"`
	EdgeGateWouldSkipBusy               int                         `json:"edge_gate_would_skip_busy"`
	EdgeGateWouldSkipTripped            int                         `json:"edge_gate_would_skip_tripped"`
	EdgeGateSuccessfulWouldSkip         int                         `json:"edge_gate_successful_would_skip"`
	EdgeGateQuotaFailuresWouldSkip      int                         `json:"edge_gate_quota_failures_would_skip"`
	EdgeGateQuotaFailuresWhileDispatch  int                         `json:"edge_gate_quota_failures_while_dispatching"`
	EdgeGateTripsObserved               int                         `json:"edge_gate_trips_observed"`
	EdgeGateReopensObserved             int                         `json:"edge_gate_reopens_observed"`
	EdgeGateCoverageSeconds             int64                       `json:"edge_gate_coverage_seconds"`
	EdgeGateVerdict                     string                      `json:"edge_gate_verdict"`
	EdgeGateVerdictMessage              string                      `json:"edge_gate_verdict_message"`
	AssistActuallyDeferred              int                         `json:"assist_actually_deferred"`
	AssistTailReached                   int                         `json:"assist_tail_reached"`
	AssistTailDispatched                int                         `json:"assist_tail_dispatched"`
	AssistTailSuccess                   int                         `json:"assist_tail_success"`
	AssistNeighborSuccess               int                         `json:"assist_neighbor_success"`
	AssistLostTail                      int                         `json:"assist_lost_tail"`
	AssistDuplicateTail                 int                         `json:"assist_duplicate_tail"`
	AssistPrimaryDeferred               int                         `json:"assist_primary_deferred"`
	AssistStreamHedge                   int                         `json:"assist_stream_hedge"`
	AssistRequests                      int                         `json:"assist_requests"`
	AssistSuccessfulRequests            int                         `json:"assist_successful_requests"`
	AssistFailedRequests                int                         `json:"assist_failed_requests"`
	AssistTailNotReached                int                         `json:"assist_tail_not_reached"`
	AssistTerminalBeforeTail            int                         `json:"assist_terminal_before_tail"`
	RoutingChangesApplied               int                         `json:"routing_changes_applied"`
	AdditionalProviderRequests          int                         `json:"additional_provider_requests"`
	QueueDepth                          int                         `json:"queue_depth"`
	QueueCapacity                       int                         `json:"queue_capacity"`
	DroppedRecords                      uint64                      `json:"dropped_records"`
	WriteFailures                       uint64                      `json:"write_failures"`
	RotationFailures                    uint64                      `json:"rotation_failures"`
	RecordsInMemory                     int                         `json:"records_in_memory"`
	DiskBytes                           int64                       `json:"disk_bytes"`
	DiskLimitBytes                      int64                       `json:"disk_limit_bytes"`
	LastEventAt                         time.Time                   `json:"last_event_at,omitempty"`
	OldestRetainedAt                    time.Time                   `json:"oldest_retained_at,omitempty"`
	RetainedHistorySpanSeconds          int64                       `json:"retained_history_span_seconds"`
	HistoryTruncated                    bool                        `json:"history_truncated"`
	HighRateTruncation                  bool                        `json:"high_rate_truncation"`
	ReadinessBlockers                   []string                    `json:"readiness_blockers,omitempty"`
	Warning                             string                      `json:"warning,omitempty"`
	Recent                              []adaptiveShadowAuditRecord `json:"recent,omitempty"`
}

type adaptiveShadowAuditStore struct {
	path        string
	rotatedPath string
	fileLimit   int64
	queue       chan adaptiveShadowAuditRecord
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	lifecycle   sync.RWMutex

	mu                  sync.RWMutex
	records             []adaptiveShadowAuditRecord
	hours               map[int64]*adaptiveShadowAuditHour
	nextSequence        uint64
	aggregateCheckpoint uint64
	warning             string

	dropped         atomic.Uint64
	writeFailures   atomic.Uint64
	rotationFailure atomic.Uint64
	currentBytes    atomic.Int64
	rotatedBytes    atomic.Int64
	diskDisabled    atomic.Bool
	closed          atomic.Bool
}

var adaptiveShadowAuditGlobal = struct {
	sync.RWMutex
	store *adaptiveShadowAuditStore
}{}

var adaptiveShadowAuditConfigureMu sync.Mutex

func adaptiveShadowAuditPath(statePath string) string {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		statePath = defaultStatePath
	}
	base := strings.TrimSuffix(statePath, filepath.Ext(statePath))
	return base + "-adaptive-shadow.jsonl"
}

func newAdaptiveShadowAuditStore(statePath string, queueCapacity int) *adaptiveShadowAuditStore {
	if queueCapacity <= 0 {
		queueCapacity = adaptiveShadowAuditQueueCapacity
	}
	path := adaptiveShadowAuditPath(statePath)
	return &adaptiveShadowAuditStore{
		path:        path,
		rotatedPath: path + ".1",
		fileLimit:   adaptiveShadowAuditFileBytes,
		queue:       make(chan adaptiveShadowAuditRecord, queueCapacity),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		hours:       make(map[int64]*adaptiveShadowAuditHour),
	}
}

func configureAdaptiveShadowAuditStore(statePath string) {
	adaptiveShadowAuditConfigureMu.Lock()
	defer adaptiveShadowAuditConfigureMu.Unlock()
	adaptiveShadowAuditGlobal.RLock()
	previous := adaptiveShadowAuditGlobal.store
	adaptiveShadowAuditGlobal.RUnlock()
	if previous != nil {
		// Reconfiguration is control-plane work. Flush the old worker before
		// loading the same files so two workers can never append concurrently.
		previous.close()
	}
	store := newAdaptiveShadowAuditStore(statePath, adaptiveShadowAuditQueueCapacity)
	store.loadBoundedHistory()
	go store.run()
	adaptiveShadowAuditGlobal.Lock()
	if previous != nil {
		// The global write lock prevents a new enqueue from selecting the old
		// store. Waiting for its lifecycle readers makes the counter hand-off
		// exact even for a request already finishing during reconfiguration.
		previous.lifecycle.Lock()
		store.dropped.Add(previous.dropped.Load())
		store.writeFailures.Add(previous.writeFailures.Load())
		store.rotationFailure.Add(previous.rotationFailure.Load())
		previous.mu.RLock()
		previousWarning := previous.warning
		previous.mu.RUnlock()
		previous.lifecycle.Unlock()
		if strings.TrimSpace(previousWarning) != "" && strings.TrimSpace(store.warning) == "" {
			store.setWarning(previousWarning)
		}
	}
	adaptiveShadowAuditGlobal.store = store
	adaptiveShadowAuditGlobal.Unlock()
}

func closeAdaptiveShadowAuditStore() {
	adaptiveShadowAuditConfigureMu.Lock()
	defer adaptiveShadowAuditConfigureMu.Unlock()
	adaptiveShadowAuditGlobal.Lock()
	store := adaptiveShadowAuditGlobal.store
	adaptiveShadowAuditGlobal.store = nil
	adaptiveShadowAuditGlobal.Unlock()
	if store != nil {
		store.close()
	}
}

func currentAdaptiveShadowAuditStore() *adaptiveShadowAuditStore {
	adaptiveShadowAuditGlobal.RLock()
	store := adaptiveShadowAuditGlobal.store
	adaptiveShadowAuditGlobal.RUnlock()
	return store
}

func enqueueAdaptiveShadowAudit(record adaptiveShadowAuditRecord) {
	if len(record.Attempts) == 0 {
		return
	}
	adaptiveShadowAuditGlobal.RLock()
	store := adaptiveShadowAuditGlobal.store
	if store == nil {
		adaptiveShadowAuditGlobal.RUnlock()
		return
	}
	store.lifecycle.RLock()
	adaptiveShadowAuditGlobal.RUnlock()
	defer store.lifecycle.RUnlock()
	record = sanitizeAdaptiveShadowAuditRecord(record)
	if store.closed.Load() {
		store.dropped.Add(1)
		return
	}
	select {
	case store.queue <- record:
	default:
		store.dropped.Add(1)
	}
}

func (store *adaptiveShadowAuditStore) close() {
	if store == nil {
		return
	}
	store.closeOnce.Do(func() {
		store.lifecycle.Lock()
		store.closed.Store(true)
		close(store.stop)
		store.lifecycle.Unlock()
		<-store.done
	})
}

func (store *adaptiveShadowAuditStore) run() {
	defer close(store.done)
	ticker := time.NewTicker(adaptiveShadowAuditFlushInterval)
	defer ticker.Stop()
	var file *os.File
	var writer *bufio.Writer
	flush := func(syncDisk bool) {
		if writer != nil {
			if errFlush := writer.Flush(); errFlush != nil {
				store.noteWriteFailure(errFlush)
			}
		}
		if syncDisk && file != nil {
			if errSync := file.Sync(); errSync != nil {
				store.noteWriteFailure(errSync)
			}
		}
	}
	closeFile := func() {
		flush(true)
		if file != nil {
			_ = file.Close()
		}
		file, writer = nil, nil
	}
	writeRecord := func(record adaptiveShadowAuditRecord) {
		record = store.appendMemory(record)
		if store.diskDisabled.Load() {
			return
		}
		raw, errMarshal := json.Marshal(record)
		maximumRecord := minInt64(adaptiveShadowAuditMaximumRecord, store.fileLimit)
		if errMarshal != nil || int64(len(raw)+1) > maximumRecord {
			store.noteWriteFailure(firstNonNil(errMarshal, fmt.Errorf("adaptive shadow audit record exceeds %d bytes", maximumRecord)))
			return
		}
		raw = append(raw, '\n')
		if store.currentBytes.Load()+int64(len(raw)) > store.fileLimit {
			closeFile()
			if errRotate := store.rotate(); errRotate != nil {
				store.rotationFailure.Add(1)
				store.setWarning("Не удалось ротировать shadow-журнал; новые записи временно остаются только в памяти.")
				return
			}
		}
		if file == nil {
			if errMkdir := os.MkdirAll(filepath.Dir(store.path), 0o700); errMkdir != nil {
				store.noteWriteFailure(errMkdir)
				return
			}
			var errOpen error
			file, errOpen = os.OpenFile(store.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if errOpen != nil {
				store.noteWriteFailure(errOpen)
				file = nil
				return
			}
			writer = bufio.NewWriterSize(file, 64*1024)
		}
		written, errWrite := writer.Write(raw)
		if errWrite != nil || written != len(raw) {
			store.noteWriteFailure(firstNonNil(errWrite, fmt.Errorf("short adaptive shadow audit write: %d/%d", written, len(raw))))
			closeFile()
			return
		}
		store.currentBytes.Add(int64(written))
	}
	for {
		select {
		case record := <-store.queue:
			writeRecord(record)
		case <-ticker.C:
			flush(true)
			store.saveHours()
		case <-store.stop:
			for {
				select {
				case record := <-store.queue:
					writeRecord(record)
				default:
					closeFile()
					store.saveHours()
					return
				}
			}
		}
	}
}

func (store *adaptiveShadowAuditStore) rotate() error {
	if store == nil {
		return nil
	}
	// Rotation is destructive: a second rotation removes the only older JSONL
	// generation. Durably checkpoint every record already accepted by the
	// single writer before deleting it, rather than relying on the 1s ticker.
	if errPersist := store.persistHours(); errPersist != nil {
		return fmt.Errorf("persist adaptive audit checkpoint before rotation: %w", errPersist)
	}
	if errRemove := os.Remove(store.rotatedPath); errRemove != nil && !os.IsNotExist(errRemove) {
		return errRemove
	}
	if errRename := os.Rename(store.path, store.rotatedPath); errRename != nil && !os.IsNotExist(errRename) {
		return errRename
	}
	store.rotatedBytes.Store(minInt64(store.currentBytes.Load(), store.fileLimit))
	store.currentBytes.Store(0)
	return nil
}

func (store *adaptiveShadowAuditStore) appendMemory(record adaptiveShadowAuditRecord) adaptiveShadowAuditRecord {
	store.mu.Lock()
	if record.Sequence == 0 {
		store.nextSequence++
		record.Sequence = store.nextSequence
	} else if record.Sequence > store.nextSequence {
		store.nextSequence = record.Sequence
	}
	store.appendHourLocked(record)
	if record.Sequence > store.aggregateCheckpoint {
		store.aggregateCheckpoint = record.Sequence
	}
	store.records = append(store.records, record)
	if excess := len(store.records) - adaptiveShadowAuditMemoryRecords; excess > 0 {
		copy(store.records, store.records[excess:])
		store.records = store.records[:adaptiveShadowAuditMemoryRecords]
	}
	store.mu.Unlock()
	return record
}

func (store *adaptiveShadowAuditStore) appendHourLocked(record adaptiveShadowAuditRecord) {
	if store.hours == nil {
		store.hours = make(map[int64]*adaptiveShadowAuditHour)
	}
	hour := record.At.UTC().Truncate(time.Hour)
	key := hour.Unix()
	bucket := store.hours[key]
	if bucket == nil {
		bucket = &adaptiveShadowAuditHour{Hour: hour}
		store.hours[key] = bucket
	}
	if bucket.First.IsZero() || record.At.Before(bucket.First) {
		bucket.First = record.At
	}
	if record.At.After(bucket.Last) {
		bucket.Last = record.At
	}
	bucket.Requests++
	if record.Success {
		bucket.SuccessfulRequests++
	} else {
		bucket.FailedRequests++
	}
	bucket.ActualAttempts += record.ActualExecutionAttempts
	if record.FallbackUsed {
		bucket.FallbackRequests++
	}
	bucket.RoutingEnforced = bucket.RoutingEnforced || record.RoutingEnforced
	bucket.RoutingChanges += record.RoutingChangesApplied
	bucket.AdditionalRequests += record.AdditionalProviderRequests
	bucket.AssistActuallyDeferred += record.AssistActuallyDeferred
	bucket.AssistTailReached += record.AssistTailReached
	bucket.AssistTailDispatched += record.AssistTailDispatched
	bucket.AssistTailSuccess += record.AssistTailSuccess
	bucket.AssistNeighborSuccess += record.AssistNeighborSuccess
	bucket.AssistLostTail += record.AssistLostTail
	bucket.AssistDuplicateTail += record.AssistDuplicateTail
	bucket.AssistPrimaryDeferred += record.AssistPrimaryDeferred
	bucket.AssistStreamHedge += record.AssistStreamHedge
	bucket.AssistRequests += record.AssistRequests
	bucket.AssistSuccessfulRequests += record.AssistSuccessfulRequests
	bucket.AssistFailedRequests += record.AssistFailedRequests
	bucket.AssistTailNotReached += record.AssistTailNotReached
	bucket.AssistTerminalBeforeTail += record.AssistTerminalBeforeTail
	for _, attempt := range record.Attempts {
		if attempt.AssistLifecycle != "" {
			continue
		}
		token := strings.HasPrefix(attempt.EstimateConfidence, "token_calibrated_")
		if token {
			bucket.TokenAttempts++
			if bucket.TokenFirst.IsZero() || record.At.Before(bucket.TokenFirst) {
				bucket.TokenFirst = record.At
			}
			if record.At.After(bucket.TokenLast) {
				bucket.TokenLast = record.At
			}
		} else {
			bucket.LegacyAttempts++
		}
		switch attempt.Decision {
		case adaptiveShadowDecisionAdmit:
			bucket.WouldAdmit++
			if token {
				bucket.TokenWouldAdmit++
			}
		case adaptiveShadowDecisionWithhold:
			bucket.WouldWithhold++
			if token {
				bucket.TokenWouldWithhold++
			}
		}
		if attempt.Decision != adaptiveShadowDecisionAdmit && attempt.Decision != adaptiveShadowDecisionWithhold {
			bucket.Unknown++
			if token {
				bucket.TokenUnknown++
			}
		}
		if attempt.Success && attempt.Decision == adaptiveShadowDecisionWithhold {
			bucket.SuccessfulWithhold++
			if token {
				bucket.TokenSuccessfulWithhold++
			}
		}
		if attempt.Decision == adaptiveShadowDecisionAdmit && adaptiveShadowAuditQuotaFailure(attempt.ErrorCode) {
			bucket.QuotaFailuresAdmit++
			if token {
				bucket.TokenQuotaFailuresAdmit++
			}
		}
		if attempt.EdgeGateState != "" {
			bucket.EdgeAttempts++
			if bucket.EdgeFirst.IsZero() || record.At.Before(bucket.EdgeFirst) {
				bucket.EdgeFirst = record.At
			}
			if record.At.After(bucket.EdgeLast) {
				bucket.EdgeLast = record.At
			}
			switch attempt.EdgeGateState {
			case adaptiveEdgeGateStateGreen:
				bucket.EdgeGreen++
			case adaptiveEdgeGateStateGuarded:
				bucket.EdgeGuarded++
			case adaptiveEdgeGateStateTripped:
				bucket.EdgeTripped++
			case adaptiveEdgeGateStateHalfOpen:
				bucket.EdgeHalfOpen++
			}
			skipped := false
			switch attempt.EdgeGateDecision {
			case adaptiveEdgeGateDecisionDispatch:
				bucket.EdgeDispatch++
			case adaptiveEdgeGateDecisionProbe:
				bucket.EdgeProbe++
			case adaptiveEdgeGateDecisionSkipBusy:
				bucket.EdgeSkipBusy++
				skipped = true
			case adaptiveEdgeGateDecisionSkipTripped:
				bucket.EdgeSkipTripped++
				skipped = true
			}
			quotaFailure := adaptiveEdgeGateAuditQuotaFailure(attempt)
			if skipped && attempt.Success {
				bucket.EdgeSuccessfulSkip++
			}
			if skipped && quotaFailure {
				bucket.EdgeQuotaFailureSkip++
			} else if !skipped && quotaFailure {
				bucket.EdgeQuotaFailureDispatch++
			}
			if strings.HasPrefix(attempt.EdgeGateOutcomeTransition, "tripped_") {
				bucket.EdgeTrips++
			}
			if attempt.EdgeGateOutcomeTransition == "reopened" {
				bucket.EdgeReopens++
			}
		}
	}
	cutoff := record.At.UTC().Add(-adaptiveShadowAuditAggregatePeriod).Truncate(time.Hour).Unix()
	for item := range store.hours {
		if item < cutoff {
			delete(store.hours, item)
		}
	}
}

func (store *adaptiveShadowAuditStore) aggregatePath() string { return store.path + ".hours.json" }

type adaptiveShadowAuditAggregateFile struct {
	SchemaVersion int                       `json:"schema_version"`
	Checkpoint    uint64                    `json:"checkpoint"`
	Hours         []adaptiveShadowAuditHour `json:"hours"`
}

func mergeAdaptiveShadowAuditHour(total *adaptiveShadowAuditHour, item adaptiveShadowAuditHour) {
	if total == nil {
		return
	}
	total.Requests += item.Requests
	total.SuccessfulRequests += item.SuccessfulRequests
	total.FailedRequests += item.FailedRequests
	total.ActualAttempts += item.ActualAttempts
	total.FallbackRequests += item.FallbackRequests
	total.RoutingEnforced = total.RoutingEnforced || item.RoutingEnforced
	total.RoutingChanges += item.RoutingChanges
	total.AdditionalRequests += item.AdditionalRequests
	total.WouldAdmit += item.WouldAdmit
	total.WouldWithhold += item.WouldWithhold
	total.LegacyAttempts += item.LegacyAttempts
	total.Unknown += item.Unknown
	total.SuccessfulWithhold += item.SuccessfulWithhold
	total.QuotaFailuresAdmit += item.QuotaFailuresAdmit
	total.TokenAttempts += item.TokenAttempts
	total.TokenWouldAdmit += item.TokenWouldAdmit
	total.TokenWouldWithhold += item.TokenWouldWithhold
	total.TokenUnknown += item.TokenUnknown
	total.TokenSuccessfulWithhold += item.TokenSuccessfulWithhold
	total.TokenQuotaFailuresAdmit += item.TokenQuotaFailuresAdmit
	total.EdgeAttempts += item.EdgeAttempts
	total.EdgeGreen += item.EdgeGreen
	total.EdgeGuarded += item.EdgeGuarded
	total.EdgeTripped += item.EdgeTripped
	total.EdgeHalfOpen += item.EdgeHalfOpen
	total.EdgeDispatch += item.EdgeDispatch
	total.EdgeProbe += item.EdgeProbe
	total.EdgeSkipBusy += item.EdgeSkipBusy
	total.EdgeSkipTripped += item.EdgeSkipTripped
	total.EdgeSuccessfulSkip += item.EdgeSuccessfulSkip
	total.EdgeQuotaFailureSkip += item.EdgeQuotaFailureSkip
	total.EdgeQuotaFailureDispatch += item.EdgeQuotaFailureDispatch
	total.EdgeTrips += item.EdgeTrips
	total.EdgeReopens += item.EdgeReopens
	total.AssistActuallyDeferred += item.AssistActuallyDeferred
	total.AssistTailReached += item.AssistTailReached
	total.AssistTailDispatched += item.AssistTailDispatched
	total.AssistTailSuccess += item.AssistTailSuccess
	total.AssistNeighborSuccess += item.AssistNeighborSuccess
	total.AssistLostTail += item.AssistLostTail
	total.AssistDuplicateTail += item.AssistDuplicateTail
	total.AssistPrimaryDeferred += item.AssistPrimaryDeferred
	total.AssistStreamHedge += item.AssistStreamHedge
	total.AssistRequests += item.AssistRequests
	total.AssistSuccessfulRequests += item.AssistSuccessfulRequests
	total.AssistFailedRequests += item.AssistFailedRequests
	total.AssistTailNotReached += item.AssistTailNotReached
	total.AssistTerminalBeforeTail += item.AssistTerminalBeforeTail
}

func (store *adaptiveShadowAuditStore) saveHours() {
	if err := store.persistHours(); err != nil {
		store.noteWriteFailure(err)
	}
}

func (store *adaptiveShadowAuditStore) persistHours() error {
	store.mu.RLock()
	hours := make([]adaptiveShadowAuditHour, 0, len(store.hours))
	for _, bucket := range store.hours {
		if bucket != nil {
			hours = append(hours, *bucket)
		}
	}
	checkpoint := store.aggregateCheckpoint
	store.mu.RUnlock()
	raw, err := json.Marshal(adaptiveShadowAuditAggregateFile{SchemaVersion: 1, Checkpoint: checkpoint, Hours: hours})
	if err != nil {
		return err
	}
	path := store.aggregatePath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (store *adaptiveShadowAuditStore) loadHours() {
	raw, err := os.ReadFile(store.aggregatePath())
	if err != nil {
		return
	}
	var persisted adaptiveShadowAuditAggregateFile
	if json.Unmarshal(raw, &persisted) != nil || persisted.SchemaVersion != 1 {
		// preview.13 development snapshots used a bare array; accept it once.
		if json.Unmarshal(raw, &persisted.Hours) != nil {
			return
		}
	}
	cutoff := time.Now().UTC().Add(-adaptiveShadowAuditAggregatePeriod).Truncate(time.Hour)
	store.mu.Lock()
	for index := range persisted.Hours {
		bucket := persisted.Hours[index]
		if !bucket.Hour.Before(cutoff) {
			copyBucket := bucket
			store.hours[bucket.Hour.Unix()] = &copyBucket
		}
	}
	store.aggregateCheckpoint = persisted.Checkpoint
	store.nextSequence = persisted.Checkpoint
	store.mu.Unlock()
}

func (store *adaptiveShadowAuditStore) loadBoundedHistory() {
	if store == nil {
		return
	}
	store.loadHours()
	hadAggregates := len(store.hours) > 0
	checkpoint := store.aggregateCheckpoint
	for _, path := range []string{store.rotatedPath, store.path} {
		info, errStat := os.Stat(path)
		if errStat != nil {
			if !os.IsNotExist(errStat) {
				store.setWarning("Не удалось прочитать размер shadow-журнала; новая телеметрия продолжит работу в памяти.")
			}
			continue
		}
		if info.Size() > store.fileLimit {
			if errTruncate := os.Truncate(path, store.fileLimit); errTruncate != nil {
				// The audit files contain telemetry only. If an old oversized file
				// cannot be shortened, remove that exact owned file rather than let
				// an optional observer consume unbounded disk space.
				if errRemove := os.Remove(path); errRemove == nil || os.IsNotExist(errRemove) {
					store.setWarning("Предыдущий oversized shadow-журнал удалён; сбор начат заново без влияния на маршрутизацию.")
					continue
				}
				store.setWarning("Shadow-журнал превысил лимит и не может быть сокращён; новые записи временно остаются только в памяти.")
				store.diskDisabled.Store(true)
				if path == store.path {
					store.currentBytes.Store(info.Size())
				} else {
					store.rotatedBytes.Store(info.Size())
				}
				continue
			}
			store.setWarning("Предыдущий shadow-журнал был сокращён до безопасного лимита размера.")
		}
		size := minInt64(info.Size(), store.fileLimit)
		if path == store.path {
			store.currentBytes.Store(size)
		} else {
			store.rotatedBytes.Store(size)
		}
		file, errOpen := os.Open(path)
		if errOpen != nil {
			store.setWarning("Не удалось восстановить предыдущий shadow-журнал; новые записи продолжаются отдельно.")
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), adaptiveShadowAuditMaximumRecord)
		for scanner.Scan() {
			var record adaptiveShadowAuditRecord
			if errDecode := json.Unmarshal(scanner.Bytes(), &record); errDecode == nil && record.SchemaVersion == adaptiveShadowAuditSchemaVersion {
				record = sanitizeAdaptiveShadowAuditRecord(record)
				// Sequence zero is legacy JSONL. Once a sidecar exists those rows
				// formed its bootstrap cohort; all preview.13 crash-tail rows carry
				// a writer-assigned sequence and can be compared exactly.
				alreadyAggregated := hadAggregates && (record.Sequence == 0 || record.Sequence <= checkpoint)
				if alreadyAggregated {
					store.mu.Lock()
					store.records = append(store.records, record)
					if excess := len(store.records) - adaptiveShadowAuditMemoryRecords; excess > 0 {
						copy(store.records, store.records[excess:])
						store.records = store.records[:adaptiveShadowAuditMemoryRecords]
					}
					store.mu.Unlock()
				} else {
					store.appendMemory(record)
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			store.setWarning("Часть предыдущего shadow-журнала повреждена; отчёт построен по валидным ограниченным записям.")
		}
		_ = file.Close()
	}
}

func (store *adaptiveShadowAuditStore) report(cfg pluginConfig, period time.Duration, recentLimit int, now time.Time) adaptiveShadowAuditReport {
	if store == nil {
		return currentAdaptiveShadowAuditReport(cfg, period, recentLimit, now)
	}
	if period <= 0 {
		period = adaptiveShadowAuditDefaultPeriod
	}
	if period > 7*24*time.Hour {
		period = 7 * 24 * time.Hour
	}
	if recentLimit < 0 {
		recentLimit = 0
	}
	if recentLimit > 100 {
		recentLimit = 100
	}
	now = now.UTC()
	report := adaptiveShadowAuditReport{
		SchemaVersion:         adaptiveShadowAuditSchemaVersion,
		Status:                "ok",
		Verdict:               "collecting",
		VerdictMessage:        "Наблюдений пока недостаточно; маршрутизация остаётся прежней.",
		Mode:                  cfg.AdaptiveAllocatorMode,
		Effect:                adaptiveShadowEffect(cfg),
		RoutingEnforced:       adaptiveRoutingEnforced(cfg),
		From:                  now.Add(-period),
		To:                    now,
		QueueCapacity:         cap(store.queue),
		DiskLimitBytes:        2 * store.fileLimit,
		MinimumReviewRequests: adaptiveShadowAuditReviewRequests,
		MinimumReviewCoverage: int64(adaptiveShadowAuditReviewCoverage / time.Second),
	}
	store.mu.RLock()
	records := append([]adaptiveShadowAuditRecord(nil), store.records...)
	hours := make([]adaptiveShadowAuditHour, 0, len(store.hours))
	for _, bucket := range store.hours {
		if bucket != nil {
			hours = append(hours, *bucket)
		}
	}
	report.Warning = strings.TrimSpace(store.warning)
	store.mu.RUnlock()
	report.QueueDepth = len(store.queue)
	report.DroppedRecords = store.dropped.Load()
	report.WriteFailures = store.writeFailures.Load()
	report.RotationFailures = store.rotationFailure.Load()
	report.RecordsInMemory = len(records)
	report.DiskBytes = store.currentBytes.Load() + store.rotatedBytes.Load()
	var firstEventAt time.Time
	var firstTokenCalibratedAt time.Time
	var lastTokenCalibratedAt time.Time
	var firstEdgeGateAt time.Time
	var lastEdgeGateAt time.Time
	for _, record := range records {
		if record.At.Before(report.From) || record.At.After(now.Add(time.Minute)) {
			continue
		}
		report.RequestsObserved++
		// The report covers a historical window which may span a hot reload.
		// Preserve evidence that routing was enforced for any included request,
		// even when the current mode has since returned to observe.
		report.RoutingEnforced = report.RoutingEnforced || record.RoutingEnforced
		if record.Success {
			report.SuccessfulRequests++
		} else {
			report.FailedRequests++
		}
		report.ActualExecutionAttempts += record.ActualExecutionAttempts
		report.RoutingChangesApplied += record.RoutingChangesApplied
		if record.FallbackUsed {
			report.RequestsWithFallback++
		}
		if record.At.After(report.LastEventAt) {
			report.LastEventAt = record.At
		}
		if firstEventAt.IsZero() || record.At.Before(firstEventAt) {
			firstEventAt = record.At
		}
		for _, attempt := range record.Attempts {
			if attempt.AssistLifecycle != "" {
				continue
			}
			tokenCalibrated := strings.HasPrefix(attempt.EstimateConfidence, "token_calibrated_")
			if tokenCalibrated {
				report.TokenCalibratedAttempts++
				if firstTokenCalibratedAt.IsZero() || record.At.Before(firstTokenCalibratedAt) {
					firstTokenCalibratedAt = record.At
				}
				if record.At.After(lastTokenCalibratedAt) {
					lastTokenCalibratedAt = record.At
				}
			} else {
				report.LegacyShapeEstimateAttempts++
			}
			switch attempt.Decision {
			case adaptiveShadowDecisionAdmit:
				report.WouldAdmitAttempts++
				if tokenCalibrated {
					report.TokenCalibratedWouldAdmit++
				}
			case adaptiveShadowDecisionWithhold:
				report.WouldWithholdAttempts++
				if tokenCalibrated {
					report.TokenCalibratedWouldWithhold++
				}
			default:
				report.UnknownDecisionAttempts++
				if tokenCalibrated {
					report.TokenCalibratedUnknown++
				}
			}
			if attempt.Success && attempt.Decision == adaptiveShadowDecisionWithhold {
				report.SuccessfulWouldWithhold++
				if tokenCalibrated {
					report.SuccessfulTokenCalibratedWithhold++
				}
			}
			if attempt.Decision == adaptiveShadowDecisionAdmit && adaptiveShadowAuditQuotaFailure(attempt.ErrorCode) {
				report.QuotaFailuresWouldAdmit++
				if tokenCalibrated {
					report.TokenCalibratedQuotaFailuresOnAdmit++
				}
			}
			if attempt.EdgeGateState != "" {
				report.EdgeGateAttempts++
				if firstEdgeGateAt.IsZero() || record.At.Before(firstEdgeGateAt) {
					firstEdgeGateAt = record.At
				}
				if record.At.After(lastEdgeGateAt) {
					lastEdgeGateAt = record.At
				}
				switch attempt.EdgeGateState {
				case adaptiveEdgeGateStateGreen:
					report.EdgeGateGreenAttempts++
				case adaptiveEdgeGateStateGuarded:
					report.EdgeGateGuardedAttempts++
				case adaptiveEdgeGateStateTripped:
					report.EdgeGateTrippedAttempts++
				case adaptiveEdgeGateStateHalfOpen:
					report.EdgeGateHalfOpenAttempts++
				}
				skipped := false
				switch attempt.EdgeGateDecision {
				case adaptiveEdgeGateDecisionDispatch:
					report.EdgeGateWouldDispatch++
				case adaptiveEdgeGateDecisionProbe:
					report.EdgeGateWouldProbe++
				case adaptiveEdgeGateDecisionSkipBusy:
					report.EdgeGateWouldSkipBusy++
					skipped = true
				case adaptiveEdgeGateDecisionSkipTripped:
					report.EdgeGateWouldSkipTripped++
					skipped = true
				}
				quotaFailure := adaptiveEdgeGateAuditQuotaFailure(attempt)
				if skipped && attempt.Success {
					report.EdgeGateSuccessfulWouldSkip++
				}
				if skipped && quotaFailure {
					report.EdgeGateQuotaFailuresWouldSkip++
				} else if !skipped && quotaFailure {
					report.EdgeGateQuotaFailuresWhileDispatch++
				}
				if strings.HasPrefix(attempt.EdgeGateOutcomeTransition, "tripped_") {
					report.EdgeGateTripsObserved++
				}
				if attempt.EdgeGateOutcomeTransition == "reopened" {
					report.EdgeGateReopensObserved++
				}
			}
		}
		if recentLimit > 0 {
			report.Recent = append(report.Recent, record)
			if len(report.Recent) > recentLimit {
				copy(report.Recent, report.Recent[len(report.Recent)-recentLimit:])
				report.Recent = report.Recent[:recentLimit]
			}
		}
	}
	if !firstEventAt.IsZero() && report.LastEventAt.After(firstEventAt) {
		report.CoverageSeconds = int64(report.LastEventAt.Sub(firstEventAt) / time.Second)
	}
	if !firstTokenCalibratedAt.IsZero() && lastTokenCalibratedAt.After(firstTokenCalibratedAt) {
		report.TokenCalibratedCoverageSeconds = int64(lastTokenCalibratedAt.Sub(firstTokenCalibratedAt) / time.Second)
	}
	if !firstEdgeGateAt.IsZero() && lastEdgeGateAt.After(firstEdgeGateAt) {
		report.EdgeGateCoverageSeconds = int64(lastEdgeGateAt.Sub(firstEdgeGateAt) / time.Second)
	}
	// Detailed records are intentionally capped, but readiness uses the compact
	// seven-day hourly cohort so high request rates cannot collapse six hours of
	// evidence into a few minutes. The aggregate contains no trace or identity.
	var aggregateFirst, aggregateLast, tokenFirst, tokenLast, edgeFirst, edgeLast time.Time
	var total adaptiveShadowAuditHour
	for _, bucket := range hours {
		// Hour buckets are indivisible counters. Exclude a partial leading hour
		// instead of attributing requests from before the requested period.
		if bucket.First.Before(report.From) || bucket.Last.After(now) {
			continue
		}
		bucketFirst := bucket.First
		if bucketFirst.Before(report.From) {
			bucketFirst = report.From
		}
		bucketLast := bucket.Last
		if bucketLast.After(now) {
			bucketLast = now
		}
		mergeAdaptiveShadowAuditHour(&total, bucket)
		if aggregateFirst.IsZero() || bucketFirst.Before(aggregateFirst) {
			aggregateFirst = bucketFirst
		}
		if bucketLast.After(aggregateLast) {
			aggregateLast = bucketLast
		}
		if bucket.TokenAttempts > 0 {
			itemFirst := bucket.TokenFirst
			if itemFirst.IsZero() {
				itemFirst = bucketFirst
			}
			if itemFirst.Before(report.From) {
				itemFirst = report.From
			}
			itemLast := bucket.TokenLast
			if itemLast.IsZero() {
				itemLast = bucketLast
			}
			if itemLast.After(now) {
				itemLast = now
			}
			if tokenFirst.IsZero() || itemFirst.Before(tokenFirst) {
				tokenFirst = itemFirst
			}
			if itemLast.After(tokenLast) {
				tokenLast = itemLast
			}
		}
		if bucket.EdgeAttempts > 0 {
			itemFirst := bucket.EdgeFirst
			if itemFirst.IsZero() {
				itemFirst = bucketFirst
			}
			if itemFirst.Before(report.From) {
				itemFirst = report.From
			}
			itemLast := bucket.EdgeLast
			if itemLast.IsZero() {
				itemLast = bucketLast
			}
			if itemLast.After(now) {
				itemLast = now
			}
			if edgeFirst.IsZero() || itemFirst.Before(edgeFirst) {
				edgeFirst = itemFirst
			}
			if itemLast.After(edgeLast) {
				edgeLast = itemLast
			}
		}
	}
	if total.Requests > 0 {
		report.RequestsObserved = total.Requests
		report.SuccessfulRequests = total.SuccessfulRequests
		report.FailedRequests = total.FailedRequests
		report.ActualExecutionAttempts = total.ActualAttempts
		report.RequestsWithFallback = total.FallbackRequests
		report.RoutingEnforced = report.RoutingEnforced || total.RoutingEnforced
		report.RoutingChangesApplied = total.RoutingChanges
		report.AdditionalProviderRequests = total.AdditionalRequests
		report.WouldAdmitAttempts = total.WouldAdmit
		report.WouldWithholdAttempts = total.WouldWithhold
		report.UnknownDecisionAttempts = total.Unknown
		report.LegacyShapeEstimateAttempts = total.LegacyAttempts
		report.SuccessfulWouldWithhold = total.SuccessfulWithhold
		report.QuotaFailuresWouldAdmit = total.QuotaFailuresAdmit
		report.TokenCalibratedAttempts = total.TokenAttempts
		report.TokenCalibratedWouldAdmit = total.TokenWouldAdmit
		report.TokenCalibratedWouldWithhold = total.TokenWouldWithhold
		report.TokenCalibratedUnknown = total.TokenUnknown
		report.SuccessfulTokenCalibratedWithhold = total.TokenSuccessfulWithhold
		report.TokenCalibratedQuotaFailuresOnAdmit = total.TokenQuotaFailuresAdmit
		report.EdgeGateAttempts = total.EdgeAttempts
		report.EdgeGateGreenAttempts = total.EdgeGreen
		report.EdgeGateGuardedAttempts = total.EdgeGuarded
		report.EdgeGateTrippedAttempts = total.EdgeTripped
		report.EdgeGateHalfOpenAttempts = total.EdgeHalfOpen
		report.EdgeGateWouldDispatch = total.EdgeDispatch
		report.EdgeGateWouldProbe = total.EdgeProbe
		report.EdgeGateWouldSkipBusy = total.EdgeSkipBusy
		report.EdgeGateWouldSkipTripped = total.EdgeSkipTripped
		report.EdgeGateSuccessfulWouldSkip = total.EdgeSuccessfulSkip
		report.EdgeGateQuotaFailuresWouldSkip = total.EdgeQuotaFailureSkip
		report.EdgeGateQuotaFailuresWhileDispatch = total.EdgeQuotaFailureDispatch
		report.EdgeGateTripsObserved = total.EdgeTrips
		report.EdgeGateReopensObserved = total.EdgeReopens
		report.AssistActuallyDeferred = total.AssistActuallyDeferred
		report.AssistTailReached = total.AssistTailReached
		report.AssistTailDispatched = total.AssistTailDispatched
		report.AssistTailSuccess = total.AssistTailSuccess
		report.AssistNeighborSuccess = total.AssistNeighborSuccess
		report.AssistLostTail = total.AssistLostTail
		report.AssistDuplicateTail = total.AssistDuplicateTail
		report.AssistPrimaryDeferred = total.AssistPrimaryDeferred
		report.AssistStreamHedge = total.AssistStreamHedge
		report.AssistRequests = total.AssistRequests
		report.AssistSuccessfulRequests = total.AssistSuccessfulRequests
		report.AssistFailedRequests = total.AssistFailedRequests
		report.AssistTailNotReached = total.AssistTailNotReached
		report.AssistTerminalBeforeTail = total.AssistTerminalBeforeTail
		report.OldestRetainedAt = aggregateFirst
		report.LastEventAt = aggregateLast
		if aggregateLast.After(aggregateFirst) {
			report.CoverageSeconds = int64(aggregateLast.Sub(aggregateFirst) / time.Second)
		}
		if tokenLast.After(tokenFirst) {
			report.TokenCalibratedCoverageSeconds = int64(tokenLast.Sub(tokenFirst) / time.Second)
		}
		if edgeLast.After(edgeFirst) {
			report.EdgeGateCoverageSeconds = int64(edgeLast.Sub(edgeFirst) / time.Second)
		}
	} else if len(records) > 0 {
		report.OldestRetainedAt = firstEventAt
	}
	report.RetainedHistorySpanSeconds = report.CoverageSeconds
	if len(records) == adaptiveShadowAuditMemoryRecords && !aggregateFirst.IsZero() && !firstEventAt.IsZero() && aggregateFirst.Before(firstEventAt) {
		report.HistoryTruncated = true
		detailSpan := report.LastEventAt.Sub(firstEventAt)
		report.HighRateTruncation = detailSpan < adaptiveShadowAuditReviewCoverage
	}
	report.TokenCalibrationVerdict = "collecting"
	report.TokenCalibrationVerdictMessage = "Полностью токен-калиброванных наблюдений пока недостаточно; маршрутизация остаётся прежней."
	if report.DroppedRecords > 0 || report.WriteFailures > 0 || report.RotationFailures > 0 ||
		store.diskDisabled.Load() || report.Warning != "" {
		report.Status = "warning"
		report.Verdict = "telemetry_degraded"
		report.VerdictMessage = "Часть наблюдений потеряна; перед включением влияния нужен новый чистый период сбора."
		if report.Warning == "" {
			report.Warning = "Часть shadow-телеметрии не сохранена; маршрутизация и запросы моделей не затронуты."
		}
	} else if report.SuccessfulWouldWithhold > 0 || report.QuotaFailuresWouldAdmit > 0 {
		report.Verdict = "needs_review"
		report.VerdictMessage = "Обнаружены расхождения shadow-решений с фактическим результатом; включать влияние нельзя."
	} else if report.UnknownDecisionAttempts > 0 {
		report.VerdictMessage = "Есть попытки без подтверждённых квот; сбор продолжается, влияние на маршрутизацию включать нельзя."
	} else if report.RequestsObserved >= adaptiveShadowAuditReviewRequests &&
		report.CoverageSeconds >= int64(adaptiveShadowAuditReviewCoverage/time.Second) {
		report.Verdict = "ready_for_review"
		report.VerdictMessage = "Расхождений и потерь телеметрии в выбранном периоде не обнаружено; можно проводить ручной аудит."
	} else if report.RequestsObserved > 0 {
		report.VerdictMessage = fmt.Sprintf(
			"Сбор продолжается: для ручного аудита нужно не менее %d запросов и %d часов наблюдений.",
			adaptiveShadowAuditReviewRequests,
			int(adaptiveShadowAuditReviewCoverage/time.Hour),
		)
	}
	if report.Status == "warning" {
		report.TokenCalibrationVerdict = "telemetry_degraded"
		report.TokenCalibrationVerdictMessage = "Часть наблюдений потеряна; для новой формулы нужен новый чистый период сбора."
	} else if report.SuccessfulTokenCalibratedWithhold > 0 || report.TokenCalibratedQuotaFailuresOnAdmit > 0 {
		report.TokenCalibrationVerdict = "needs_review"
		report.TokenCalibrationVerdictMessage = "Полностью токен-калиброванные решения расходятся с фактическим результатом; включать влияние нельзя."
	} else if report.TokenCalibratedUnknown > 0 {
		report.TokenCalibrationVerdictMessage = "Есть токен-калиброванные попытки без подтверждённой квоты; сбор продолжается."
	} else if report.TokenCalibratedAttempts >= adaptiveShadowAuditReviewRequests &&
		report.TokenCalibratedCoverageSeconds >= int64(adaptiveShadowAuditReviewCoverage/time.Second) {
		report.TokenCalibrationVerdict = "ready_for_review"
		report.TokenCalibrationVerdictMessage = "Расхождений новой формулы не обнаружено; можно проводить ручной аудит перед отдельной канарейкой."
	} else if report.TokenCalibratedAttempts > 0 {
		report.TokenCalibrationVerdictMessage = fmt.Sprintf(
			"Сбор новой формулы продолжается: нужно не менее %d полностью калиброванных попыток за %d часов.",
			adaptiveShadowAuditReviewRequests,
			int(adaptiveShadowAuditReviewCoverage/time.Hour),
		)
	}
	report.EdgeGateVerdict = "collecting"
	report.EdgeGateVerdictMessage = "Новый турникет пока только наблюдает; маршрутизация остаётся прежней."
	if report.Status == "warning" {
		report.EdgeGateVerdict = "telemetry_degraded"
		report.EdgeGateVerdictMessage = "Часть наблюдений потеряна; для оценки турникета нужен новый чистый период сбора."
	} else if report.EdgeGateAttempts >= adaptiveShadowAuditReviewRequests &&
		report.EdgeGateCoverageSeconds >= int64(adaptiveShadowAuditReviewCoverage/time.Second) {
		report.EdgeGateVerdict = "ready_for_review"
		report.EdgeGateVerdictMessage = fmt.Sprintf(
			"Турникет набрал достаточную shadow-выборку: успешных контрфактических пропусков %d, quota-ошибок на пропущенных попытках %d; требуется ручная проверка перед отдельной канарейкой.",
			report.EdgeGateSuccessfulWouldSkip,
			report.EdgeGateQuotaFailuresWouldSkip,
		)
	} else if report.EdgeGateAttempts > 0 {
		report.EdgeGateVerdictMessage = fmt.Sprintf(
			"Сбор турникета продолжается: нужно не менее %d попыток за %d часов; сейчас %d попыток.",
			adaptiveShadowAuditReviewRequests,
			int(adaptiveShadowAuditReviewCoverage/time.Hour),
			report.EdgeGateAttempts,
		)
	}
	if report.RoutingEnforced {
		report.VerdictMessage = adaptiveEnforcedAuditVerdictMessage(report.Verdict, report.RequestsObserved)
		report.TokenCalibrationVerdictMessage = adaptiveEnforcedAuditComponentMessage(
			"токен-калибровка", report.TokenCalibrationVerdict, report.TokenCalibratedAttempts,
		)
		report.EdgeGateVerdictMessage = adaptiveEnforcedAuditComponentMessage(
			"турникет", report.EdgeGateVerdict, report.EdgeGateAttempts,
		)
	}
	if cfg.AdaptiveAllocatorMode == "breaker" {
		// Forecast verdicts remain counterfactual in breaker mode. A poor
		// forecast is useful calibration evidence, not a reason to disable the
		// independently evidence-backed breaker.
		report.VerdictMessage = adaptiveBreakerForecastAuditMessage(report.Verdict, report.RequestsObserved)
		report.TokenCalibrationVerdictMessage = adaptiveBreakerForecastComponentMessage(
			report.TokenCalibrationVerdict, report.TokenCalibratedAttempts,
		)
		report.EdgeGateVerdictMessage = adaptiveEnforcedAuditComponentMessage(
			"breaker", report.EdgeGateVerdict, report.EdgeGateAttempts,
		)
	}
	if report.Status == "warning" {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "telemetry_degraded")
	}
	if report.RequestsObserved < adaptiveShadowAuditReviewRequests {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "minimum_requests")
	}
	if report.CoverageSeconds < int64(adaptiveShadowAuditReviewCoverage/time.Second) {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "minimum_coverage")
	}
	if report.UnknownDecisionAttempts > 0 {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "unknown_quota")
	}
	if report.SuccessfulWouldWithhold > 0 {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "successful_would_withhold")
	}
	if report.QuotaFailuresWouldAdmit > 0 {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "quota_failure_would_admit")
	}
	if report.AssistLostTail > 0 || report.AssistDuplicateTail > 0 || report.AssistPrimaryDeferred > 0 || report.AssistStreamHedge > 0 {
		report.ReadinessBlockers = append(report.ReadinessBlockers, "assist_lifecycle_invariant")
		report.Verdict = "needs_review"
		report.VerdictMessage = "Assist lifecycle нарушил инвариант сохранения хвостовой попытки; требуется ручная проверка перед продолжением canary."
	}
	return report
}

func adaptiveBreakerForecastAuditMessage(verdict string, requests int) string {
	switch verdict {
	case "telemetry_degraded":
		return "Телеметрия теневого прогноза частично потеряна; breaker продолжает опираться только на фактические доверенные ошибки квоты."
	case "needs_review":
		return "Теневой прогноз обнаружил расхождения и требует калибровки; он не блокирует маршруты и не влияет на evidence-based breaker."
	case "ready_for_review":
		return "Теневой прогноз набрал достаточную выборку для ручной проверки; маршруты закрывает только evidence-based breaker."
	default:
		return fmt.Sprintf("Теневой прогноз продолжает сбор (%d запросов) и не блокирует маршруты; breaker реагирует только на фактические доверенные ошибки квоты.", requests)
	}
}

func adaptiveBreakerForecastComponentMessage(verdict string, attempts int) string {
	if verdict == "needs_review" {
		return "Теневая токен-калибровка обнаружила расхождения; она не блокирует маршруты и не требует выключать breaker."
	}
	return fmt.Sprintf("Токен-калибровка остаётся теневой в breaker-режиме (%d попыток; вердикт %s).", attempts, verdict)
}

func adaptiveEnforcedAuditVerdictMessage(verdict string, requests int) string {
	switch verdict {
	case "telemetry_degraded":
		return "Телеметрия боевого режима частично потеряна; сам маршрутизатор продолжает работать fail-open на неизвестных данных."
	case "needs_review":
		return "Боевой режим обнаружил расхождения прогноза с фактическим результатом; требуется проверить пороги или временно вернуть observe."
	case "ready_for_review":
		return "Боевой режим работает без обнаруженных расхождений в выбранном окне; аудит можно зафиксировать."
	default:
		return fmt.Sprintf("Боевой режим включён; аудит продолжает накапливать подтверждения (%d запросов), не добавляя обращений к провайдеру.", requests)
	}
}

func adaptiveEnforcedAuditComponentMessage(component, verdict string, attempts int) string {
	switch verdict {
	case "telemetry_degraded":
		return fmt.Sprintf("Телеметрия компонента «%s» деградировала; неизвестные данные остаются fail-open.", component)
	case "needs_review":
		return fmt.Sprintf("Компонент «%s» обнаружил расхождения; требуется ручная проверка.", component)
	case "ready_for_review":
		return fmt.Sprintf("Компонент «%s» набрал достаточную чистую выборку в боевом режиме.", component)
	default:
		return fmt.Sprintf("Компонент «%s» активен; аудит продолжается (%d попыток).", component, attempts)
	}
}

func currentAdaptiveShadowAuditReport(cfg pluginConfig, period time.Duration, recentLimit int, now time.Time) adaptiveShadowAuditReport {
	if period <= 0 {
		period = adaptiveShadowAuditDefaultPeriod
	}
	store := currentAdaptiveShadowAuditStore()
	if store == nil {
		return adaptiveShadowAuditReport{
			SchemaVersion:                  adaptiveShadowAuditSchemaVersion,
			Status:                         "disabled",
			Verdict:                        "collecting",
			VerdictMessage:                 "Shadow-журнал ещё не инициализирован.",
			TokenCalibrationVerdict:        "collecting",
			TokenCalibrationVerdictMessage: "Токен-калибровка ещё не инициализирована.",
			EdgeGateVerdict:                "collecting",
			EdgeGateVerdictMessage:         "Shadow-турникет ещё не инициализирован.",
			Mode:                           cfg.AdaptiveAllocatorMode,
			Effect:                         adaptiveShadowEffect(cfg),
			RoutingEnforced:                adaptiveRoutingEnforced(cfg),
			From:                           now.UTC().Add(-period),
			To:                             now.UTC(),
			QueueCapacity:                  adaptiveShadowAuditQueueCapacity,
			DiskLimitBytes:                 adaptiveShadowAuditTotalBytes,
			MinimumReviewRequests:          adaptiveShadowAuditReviewRequests,
			MinimumReviewCoverage:          int64(adaptiveShadowAuditReviewCoverage / time.Second),
		}
	}
	return store.report(cfg, period, recentLimit, now)
}

func sanitizeAdaptiveShadowAuditRecord(record adaptiveShadowAuditRecord) adaptiveShadowAuditRecord {
	record.SchemaVersion = adaptiveShadowAuditSchemaVersion
	record.At = record.At.UTC()
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	record.TraceID = adaptiveShadowAuditToken(record.TraceID, 96)
	record.LogicalModel = adaptiveShadowAuditToken(record.LogicalModel, 160)
	record.AdditionalProviderRequests = 0
	if record.RoutingChangesApplied < 0 {
		record.RoutingChangesApplied = 0
	}
	if !record.RoutingEnforced {
		record.RoutingChangesApplied = 0
	}
	if record.ActualExecutionAttempts < 0 {
		record.ActualExecutionAttempts = 0
	}
	if len(record.Attempts) > adaptiveShadowAuditAttemptsPerRecord {
		record.OmittedAttempts += len(record.Attempts) - adaptiveShadowAuditAttemptsPerRecord
		record.Attempts = record.Attempts[:adaptiveShadowAuditAttemptsPerRecord]
	}
	for index := range record.Attempts {
		attempt := &record.Attempts[index]
		attempt.Provider = adaptiveShadowAuditToken(attempt.Provider, 48)
		attempt.Model = adaptiveShadowAuditToken(attempt.Model, 160)
		attempt.Decision = adaptiveShadowAuditToken(attempt.Decision, 32)
		attempt.EstimateConfidence = adaptiveShadowAuditToken(attempt.EstimateConfidence, 96)
		attempt.ModelWeeklyName = adaptiveShadowAuditToken(attempt.ModelWeeklyName, 96)
		attempt.Outcome = adaptiveShadowAuditToken(attempt.Outcome, 32)
		attempt.ProviderAcceptance = adaptiveShadowAuditToken(attempt.ProviderAcceptance, 16)
		attempt.ErrorCode = adaptiveShadowAuditErrorCategory(attempt.ErrorCode)
		attempt.EdgeGateState = adaptiveShadowAuditToken(attempt.EdgeGateState, 24)
		attempt.EdgeGateDecision = adaptiveShadowAuditToken(attempt.EdgeGateDecision, 32)
		attempt.EdgeGateReason = adaptiveShadowAuditToken(attempt.EdgeGateReason, 64)
		attempt.EdgeGateOutcomeTransition = adaptiveShadowAuditToken(attempt.EdgeGateOutcomeTransition, 48)
		attempt.AssistLifecycle = adaptiveShadowAuditToken(attempt.AssistLifecycle, 32)
		attempt.ReservationPercent = adaptiveShadowAuditNumber(attempt.ReservationPercent, 0, 100)
		attempt.SessionReservationPercent = adaptiveShadowAuditNumber(attempt.SessionReservationPercent, 0, 100)
		attempt.WeeklyReservationPercent = adaptiveShadowAuditNumber(attempt.WeeklyReservationPercent, 0, 100)
		attempt.ModelWeeklyReservationPercent = adaptiveShadowAuditNumber(attempt.ModelWeeklyReservationPercent, 0, 100)
		attempt.PredictedTokens = adaptiveShadowAuditNumber(attempt.PredictedTokens, 0, 2*adaptiveShadowMaximumOutputTokens)
		attempt.PendingPercent = adaptiveShadowAuditNumber(attempt.PendingPercent, 0, 1_000_000)
		attempt.SafeHeadroomBefore = adaptiveShadowAuditNumber(attempt.SafeHeadroomBefore, -1_000_000, 1_000_000)
		attempt.SafeHeadroomAfter = adaptiveShadowAuditNumber(attempt.SafeHeadroomAfter, -1_000_000, 1_000_000)
		attempt.EdgeGateSessionHeadroom = adaptiveShadowAuditNumber(attempt.EdgeGateSessionHeadroom, -100, 100)
		attempt.EdgeGateWeeklyHeadroom = adaptiveShadowAuditNumber(attempt.EdgeGateWeeklyHeadroom, -100, 100)
		if attempt.EdgeGateTripRemainingSeconds < 0 {
			attempt.EdgeGateTripRemainingSeconds = 0
		}
		if attempt.LatencyMilliseconds < 0 {
			attempt.LatencyMilliseconds = 0
		}
	}
	return record
}

func adaptiveShadowAuditErrorCategory(code string) string {
	// Persisted audit uses a closed taxonomy. Host/provider error codes are
	// untrusted and may contain credentials or tenant identifiers even when they
	// look like a valid bravo_* token.
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "":
		return ""
	case "credits_required", "billing_error":
		return "credits_exhausted"
	case "usage limit has been reached":
		return "usage_limit"
	case "bravo_adaptive_quota_withheld", "bravo_adaptive_edge_tripped", "bravo_adaptive_edge_busy",
		"bravo_subscription_quota_exhausted", "bravo_subscription_model_credits_exhausted",
		"rate_limit_error", "rate_limited", "quota_unavailable",
		"request_canceled", "bravo_attempt_superseded", "bravo_stream_attempt_aborted",
		"bravo_request_invalid", "invalid_request", "invalid_request_error", "invalid_tool_parameters",
		"bravo_context_window_exceeded", "bravo_context_target_incompatible", "context_window_exceeded",
		"bravo_capability_conflict", "bravo_capability_undeclared", "bravo_capability_unsupported",
		"bravo_no_eligible_account", "bravo_route_temporarily_unavailable", "bravo_subscription_cooling_down",
		"bravo_provider_stream_error", "provider_stream_error", "provider_stream_incomplete",
		"provider_error", "provider_failed", "upstream_failed", "overloaded_error", "server_error", "timeout":
		return strings.ToLower(strings.TrimSpace(code))
	default:
		return "unclassified_provider_error"
	}
}

func adaptiveShadowAuditNumber(value, minimum, maximum float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return adaptiveShadowRound(math.Min(math.Max(value, minimum), maximum))
}

func adaptiveShadowAuditToken(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		value = value[:maximum]
	}
	valid := true
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("._:/+-", char)) {
			valid = false
			break
		}
	}
	if valid {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("._:/+-", char) {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func adaptiveShadowAuditQuotaFailure(code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	return strings.Contains(code, "quota") || strings.Contains(code, "rate_limit") ||
		strings.Contains(code, "credits_exhausted") || strings.Contains(code, "usage_limit")
}

func adaptiveEdgeGateAuditQuotaFailure(attempt adaptiveShadowAuditAttempt) bool {
	return attempt.Status == http.StatusTooManyRequests || adaptiveShadowAuditQuotaFailure(attempt.ErrorCode)
}

func (store *adaptiveShadowAuditStore) noteWriteFailure(err error) {
	if store == nil || err == nil {
		return
	}
	store.writeFailures.Add(1)
	store.setWarning("Не удалось записать часть shadow-телеметрии на диск; запросы моделей продолжаются без изменений.")
}

func (store *adaptiveShadowAuditStore) setWarning(message string) {
	if store == nil {
		return
	}
	store.mu.Lock()
	store.warning = strings.TrimSpace(message)
	store.mu.Unlock()
}

func firstNonNil(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}
