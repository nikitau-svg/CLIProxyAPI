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
)

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
}

type adaptiveShadowAuditRecord struct {
	SchemaVersion              int                          `json:"schema_version"`
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

	mu      sync.RWMutex
	records []adaptiveShadowAuditRecord
	warning string

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
		store.appendMemory(record)
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
		case <-store.stop:
			for {
				select {
				case record := <-store.queue:
					writeRecord(record)
				default:
					closeFile()
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

func (store *adaptiveShadowAuditStore) appendMemory(record adaptiveShadowAuditRecord) {
	store.mu.Lock()
	store.records = append(store.records, record)
	if excess := len(store.records) - adaptiveShadowAuditMemoryRecords; excess > 0 {
		copy(store.records, store.records[excess:])
		store.records = store.records[:adaptiveShadowAuditMemoryRecords]
	}
	store.mu.Unlock()
}

func (store *adaptiveShadowAuditStore) loadBoundedHistory() {
	if store == nil {
		return
	}
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
				store.appendMemory(sanitizeAdaptiveShadowAuditRecord(record))
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
		attempt.ErrorCode = adaptiveShadowAuditToken(attempt.ErrorCode, 96)
		attempt.EdgeGateState = adaptiveShadowAuditToken(attempt.EdgeGateState, 24)
		attempt.EdgeGateDecision = adaptiveShadowAuditToken(attempt.EdgeGateDecision, 32)
		attempt.EdgeGateReason = adaptiveShadowAuditToken(attempt.EdgeGateReason, 64)
		attempt.EdgeGateOutcomeTransition = adaptiveShadowAuditToken(attempt.EdgeGateOutcomeTransition, 48)
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
