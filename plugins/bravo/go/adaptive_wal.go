package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	adaptiveWALVersion       = 1
	adaptiveWALMaxRecordSize = 1 << 20
	adaptiveWALScanBuffer    = adaptiveWALMaxRecordSize + 64*1024
	adaptiveWALMaxBatch      = 128
	adaptiveWALMaxPending    = 4096
	adaptiveWALMaxBytes      = 8 << 20
	adaptiveWALMaxRecords    = 16384
)

var errAdaptiveWALCapacity = fmt.Errorf("adaptive WAL reached its hard capacity")

type adaptiveWALRecord struct {
	Version           int                            `json:"version"`
	AuthIndex         string                         `json:"auth_index"`
	Revision          uint64                         `json:"revision"`
	Pending           *persistedAdaptivePendingState `json:"pending,omitempty"`
	Prepared          *persistedAdaptivePendingState `json:"prepared,omitempty"`
	Saturated         *bool                          `json:"saturated,omitempty"`
	OverflowAuthCount int                            `json:"overflow_auth_count,omitempty"`
	RecordedAt        time.Time                      `json:"recorded_at"`
}

type adaptiveWALEnvelope struct {
	Record   json.RawMessage `json:"record"`
	Checksum uint32          `json:"checksum"`
}

type adaptiveWALRequest struct {
	path    string
	line    []byte
	done    chan error
	wait    bool
	barrier bool
}

type adaptiveWALCommitter struct {
	mu       sync.Mutex
	ioMu     sync.Mutex
	pending  []*adaptiveWALRequest
	flushing bool
	flushes  atomic.Uint64
	bytes    atomic.Uint64
	maxBatch atomic.Uint64
	disk     map[string]adaptiveWALDiskUsage // guarded by ioMu
	// Non-zero overrides are test-only; production uses the hard defaults.
	maxBytes   int64
	maxRecords int64
}

type adaptiveWALDiskUsage struct {
	bytes   int64
	records int64
}

var adaptiveWALRuntime = &adaptiveWALCommitter{}
var adaptiveWALAppendAndSync = appendAndSyncAdaptiveWAL
var adaptiveSyncDirectory = syncAdaptiveDirectory

func adaptiveWALPath(statePath string) string {
	return strings.TrimSpace(statePath) + ".adaptive.wal"
}

func marshalAdaptiveWALRecord(record adaptiveWALRecord) ([]byte, error) {
	record = normalizeAdaptiveWALRecord(record)
	if record.AuthIndex == "" || record.Revision == 0 {
		return nil, fmt.Errorf("invalid adaptive WAL record identity")
	}
	payload, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode adaptive WAL record: %w", errMarshal)
	}
	if len(payload) > adaptiveWALMaxRecordSize {
		return nil, fmt.Errorf("adaptive WAL record exceeds %d bytes", adaptiveWALMaxRecordSize)
	}
	envelope, errEnvelope := json.Marshal(adaptiveWALEnvelope{
		Record: payload, Checksum: crc32.ChecksumIEEE(payload),
	})
	if errEnvelope != nil {
		return nil, fmt.Errorf("encode adaptive WAL envelope: %w", errEnvelope)
	}
	return append(envelope, '\n'), nil
}

func normalizeAdaptiveWALRecord(record adaptiveWALRecord) adaptiveWALRecord {
	record.Version = adaptiveWALVersion
	record.AuthIndex = strings.TrimSpace(record.AuthIndex)
	record.RecordedAt = record.RecordedAt.UTC()
	if record.Pending != nil {
		normalized := normalizePersistedAdaptivePending(map[string]*persistedAdaptivePendingState{
			record.AuthIndex: record.Pending,
		})
		record.Pending = normalized[record.AuthIndex]
	}
	if record.Prepared != nil {
		normalized := normalizePersistedAdaptivePending(map[string]*persistedAdaptivePendingState{
			record.AuthIndex: record.Prepared,
		})
		record.Prepared = normalized[record.AuthIndex]
	}
	if record.OverflowAuthCount < 0 {
		record.OverflowAuthCount = 0
	}
	return record
}

func (writer *adaptiveWALCommitter) append(path string, record adaptiveWALRecord) error {
	request, errEnqueue := writer.enqueue(path, record, true)
	if errEnqueue != nil {
		return errEnqueue
	}
	return <-request.done
}

func (writer *adaptiveWALCommitter) appendAsync(path string, record adaptiveWALRecord) error {
	_, errEnqueue := writer.enqueue(path, record, false)
	if errEnqueue != nil {
		return errEnqueue
	}
	return nil
}

// barrier waits until every record queued before it has completed its ordered
// write. Callers must prevent new ledger mutations while waiting when they need
// a stable revision epoch (the reconciliation path holds adaptiveAdmissionMu).
func (writer *adaptiveWALCommitter) barrier() error {
	request := &adaptiveWALRequest{wait: true, barrier: true, done: make(chan error, 1)}
	writer.mu.Lock()
	// This one control record is allowed beyond the data queue bound: it carries
	// no payload and is itself the mechanism that drains a saturated queue.
	writer.pending = append(writer.pending, request)
	if !writer.flushing {
		writer.flushing = true
		go writer.flushLoop()
	}
	writer.mu.Unlock()
	return <-request.done
}

func (writer *adaptiveWALCommitter) enqueue(path string, record adaptiveWALRecord, wait bool) (*adaptiveWALRequest, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("adaptive WAL path is empty")
	}
	line, errMarshal := marshalAdaptiveWALRecord(record)
	if errMarshal != nil {
		return nil, errMarshal
	}
	request := &adaptiveWALRequest{path: path, line: line, wait: wait}
	if wait {
		request.done = make(chan error, 1)
	}

	writer.mu.Lock()
	if len(writer.pending) >= adaptiveWALMaxPending {
		writer.mu.Unlock()
		return nil, fmt.Errorf("adaptive WAL queue is saturated")
	}
	writer.pending = append(writer.pending, request)
	if !writer.flushing {
		writer.flushing = true
		go writer.flushLoop()
	}
	writer.mu.Unlock()
	return request, nil
}

func (writer *adaptiveWALCommitter) flushLoop() {
	for {
		writer.mu.Lock()
		if len(writer.pending) == 0 {
			writer.flushing = false
			writer.mu.Unlock()
			return
		}
		batchSize := len(writer.pending)
		if batchSize > adaptiveWALMaxBatch {
			batchSize = adaptiveWALMaxBatch
		}
		batch := append([]*adaptiveWALRequest(nil), writer.pending[:batchSize]...)
		writer.pending = writer.pending[batchSize:]
		writer.mu.Unlock()

		errorsByRequest := writer.writeBatch(batch)
		asyncWriteFailed := false
		for index, item := range batch {
			if item.wait {
				item.done <- errorsByRequest[index]
			} else if errorsByRequest[index] != nil {
				asyncWriteFailed = true
			}
		}
		if asyncWriteFailed {
			_ = persistAdaptiveWALFallback()
		}

	}
}

func (writer *adaptiveWALCommitter) writeBatch(batch []*adaptiveWALRequest) []error {
	for observed := writer.maxBatch.Load(); uint64(len(batch)) > observed; observed = writer.maxBatch.Load() {
		if writer.maxBatch.CompareAndSwap(observed, uint64(len(batch))) {
			break
		}
	}
	errorsByRequest := make([]error, len(batch))
	indexesByPath := make(map[string][]int)
	for index, request := range batch {
		if request.barrier {
			continue
		}
		indexesByPath[request.path] = append(indexesByPath[request.path], index)
	}
	paths := make([]string, 0, len(indexesByPath))
	for path := range indexesByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	writer.ioMu.Lock()
	defer writer.ioMu.Unlock()
	for _, path := range paths {
		indexes := indexesByPath[path]
		var payload bytes.Buffer
		for _, index := range indexes {
			payload.Write(batch[index].line)
		}
		usage, errUsage := writer.diskUsageLocked(path)
		errWrite := errUsage
		maxBytes, maxRecords := writer.diskLimitsLocked()
		if errWrite == nil && (usage.bytes+int64(payload.Len()) > maxBytes || usage.records+int64(len(indexes)) > maxRecords) {
			errWrite = errAdaptiveWALCapacity
		}
		if errWrite == nil {
			errWrite = adaptiveWALAppendAndSync(path, payload.Bytes())
		}
		if errWrite == nil {
			usage.bytes += int64(payload.Len())
			usage.records += int64(len(indexes))
			writer.disk[path] = usage
		} else {
			// Append may fail after a partial write or after file fsync (for
			// example directory fsync). Re-read the actual file on the next
			// attempt so the hard cap cannot be bypassed by late errors.
			delete(writer.disk, path)
		}
		writer.flushes.Add(1)
		writer.bytes.Add(uint64(payload.Len()))
		for _, index := range indexes {
			errorsByRequest[index] = errWrite
		}
	}
	return errorsByRequest
}

func (writer *adaptiveWALCommitter) diskLimitsLocked() (int64, int64) {
	maxBytes, maxRecords := writer.maxBytes, writer.maxRecords
	if maxBytes <= 0 {
		maxBytes = adaptiveWALMaxBytes
	}
	if maxRecords <= 0 {
		maxRecords = adaptiveWALMaxRecords
	}
	return maxBytes, maxRecords
}

func (writer *adaptiveWALCommitter) diskUsageLocked(path string) (adaptiveWALDiskUsage, error) {
	if writer.disk == nil {
		writer.disk = make(map[string]adaptiveWALDiskUsage)
	}
	if usage, exists := writer.disk[path]; exists {
		return usage, nil
	}
	info, errStat := os.Stat(path)
	if os.IsNotExist(errStat) {
		usage := adaptiveWALDiskUsage{}
		writer.disk[path] = usage
		return usage, nil
	}
	if errStat != nil {
		return adaptiveWALDiskUsage{}, errStat
	}
	usage := adaptiveWALDiskUsage{bytes: info.Size()}
	maxBytes, _ := writer.diskLimitsLocked()
	if info.Size() <= maxBytes {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return adaptiveWALDiskUsage{}, errRead
		}
		usage.records = int64(bytes.Count(raw, []byte{'\n'}))
	}
	writer.disk[path] = usage
	return usage, nil
}

func (writer *adaptiveWALCommitter) overCapacity(path string) (bool, error) {
	writer.ioMu.Lock()
	defer writer.ioMu.Unlock()
	usage, errUsage := writer.diskUsageLocked(path)
	if errUsage != nil {
		return false, errUsage
	}
	maxBytes, maxRecords := writer.diskLimitsLocked()
	return usage.bytes > maxBytes || usage.records > maxRecords, nil
}

func appendAndSyncAdaptiveWAL(path string, payload []byte) error {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create adaptive WAL directory: %w", errMkdir)
	}
	_, errStat := os.Stat(path)
	created := os.IsNotExist(errStat)
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open adaptive WAL: %w", errOpen)
	}
	if _, errWrite := file.Write(payload); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("append adaptive WAL: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return fmt.Errorf("sync adaptive WAL: %w", errSync)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close adaptive WAL: %w", errClose)
	}
	if created {
		if errSyncDir := adaptiveSyncDirectory(filepath.Dir(path)); errSyncDir != nil {
			return fmt.Errorf("sync adaptive WAL directory: %w", errSyncDir)
		}
	}
	return nil
}

func (writer *adaptiveWALCommitter) compact(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	writer.ioMu.Lock()
	defer writer.ioMu.Unlock()
	if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
		return errRemove
	} else if errRemove == nil {
		if errSyncDir := adaptiveSyncDirectory(filepath.Dir(path)); errSyncDir != nil {
			return errSyncDir
		}
	}
	if writer.disk != nil {
		delete(writer.disk, path)
	}
	return nil
}

// compactThrough removes only absolute records already represented by the
// checkpoint snapshot. Records appended after snapshot capture have higher
// per-auth revisions and survive, so checkpoint I/O never needs to hold the
// live usage-state lock.
func (writer *adaptiveWALCommitter) compactThrough(path string, revisions map[string]uint64) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	writer.ioMu.Lock()
	defer writer.ioMu.Unlock()
	raw, errRead := os.ReadFile(path)
	if os.IsNotExist(errRead) {
		if writer.disk != nil {
			delete(writer.disk, path)
		}
		return nil
	}
	if errRead != nil {
		return errRead
	}
	var kept bytes.Buffer
	keptRecords := int64(0)
	for len(raw) > 0 {
		newline := bytes.IndexByte(raw, '\n')
		if newline < 0 {
			break // interrupted suffix
		}
		line := bytes.TrimSpace(raw[:newline])
		raw = raw[newline+1:]
		if len(line) == 0 {
			continue
		}
		var envelope adaptiveWALEnvelope
		if errDecode := json.Unmarshal(line, &envelope); errDecode != nil ||
			len(envelope.Record) == 0 || crc32.ChecksumIEEE(envelope.Record) != envelope.Checksum {
			break
		}
		var record adaptiveWALRecord
		if errDecode := json.Unmarshal(envelope.Record, &record); errDecode != nil || record.Version != adaptiveWALVersion {
			break
		}
		if record.Revision <= revisions[strings.TrimSpace(record.AuthIndex)] {
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
		keptRecords++
	}
	if kept.Len() == 0 {
		if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
			return errRemove
		} else if errRemove == nil {
			if errSyncDir := adaptiveSyncDirectory(filepath.Dir(path)); errSyncDir != nil {
				return errSyncDir
			}
		}
		if writer.disk != nil {
			delete(writer.disk, path)
		}
		return nil
	}
	dir := filepath.Dir(path)
	temp, errCreate := os.CreateTemp(dir, ".adaptive-wal-*.tmp")
	if errCreate != nil {
		return errCreate
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		_ = temp.Close()
		return errChmod
	}
	if _, errWrite := temp.Write(kept.Bytes()); errWrite != nil {
		_ = temp.Close()
		return errWrite
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return errSync
	}
	if errClose := temp.Close(); errClose != nil {
		return errClose
	}
	if errRename := os.Rename(tempName, path); errRename != nil {
		return errRename
	}
	if errSyncDir := adaptiveSyncDirectory(dir); errSyncDir != nil {
		return errSyncDir
	}
	if writer.disk == nil {
		writer.disk = make(map[string]adaptiveWALDiskUsage)
	}
	writer.disk[path] = adaptiveWALDiskUsage{bytes: int64(kept.Len()), records: keptRecords}
	return nil
}

func syncAdaptiveDirectory(path string) error {
	directory, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func replayAdaptiveWALFile(path string, state *persistedUsageState) error {
	if state == nil {
		return nil
	}
	// Configure holds the admission write gate and drains the ordered queue
	// before replay. ioMu additionally prevents a direct/test replay from racing
	// append or compaction while a corrupt suffix is being healed.
	adaptiveWALRuntime.ioMu.Lock()
	defer adaptiveWALRuntime.ioMu.Unlock()
	file, errOpen := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(errOpen) {
		if adaptiveWALRuntime.disk != nil {
			delete(adaptiveWALRuntime.disk, path)
		}
		return nil
	}
	if errOpen != nil {
		return fmt.Errorf("open adaptive WAL: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), adaptiveWALScanBuffer)
	lastAdvance := 0
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		lastAdvance = advance
		return advance, token, err
	})
	validOffset := int64(0)
	validRecords := int64(0)
	corruptSuffix := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			validOffset += int64(lastAdvance)
			continue
		}
		var envelope adaptiveWALEnvelope
		if errDecode := json.Unmarshal(line, &envelope); errDecode != nil ||
			len(envelope.Record) == 0 || crc32.ChecksumIEEE(envelope.Record) != envelope.Checksum {
			// An invalid suffix is an interrupted append. The valid prefix contains
			// only absolute, revisioned snapshots and remains safe to replay.
			corruptSuffix = true
			break
		}
		var record adaptiveWALRecord
		if errDecode := json.Unmarshal(envelope.Record, &record); errDecode != nil || record.Version != adaptiveWALVersion {
			corruptSuffix = true
			break
		}
		applyAdaptiveWALRecord(state, record)
		validOffset += int64(lastAdvance)
		validRecords++
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("scan adaptive WAL: %w", errScan)
	}
	if corruptSuffix {
		if errTruncate := file.Truncate(validOffset); errTruncate != nil {
			return fmt.Errorf("truncate corrupt adaptive WAL suffix: %w", errTruncate)
		}
		if errSync := file.Sync(); errSync != nil {
			return fmt.Errorf("sync healed adaptive WAL: %w", errSync)
		}
		if errSyncDir := adaptiveSyncDirectory(filepath.Dir(path)); errSyncDir != nil {
			return fmt.Errorf("sync healed adaptive WAL directory: %w", errSyncDir)
		}
	}
	if adaptiveWALRuntime.disk == nil {
		adaptiveWALRuntime.disk = make(map[string]adaptiveWALDiskUsage)
	}
	adaptiveWALRuntime.disk[path] = adaptiveWALDiskUsage{bytes: validOffset, records: validRecords}
	normalizePersistedUsageState(state)
	return nil
}

func applyAdaptiveWALRecord(state *persistedUsageState, record adaptiveWALRecord) {
	if state == nil {
		return
	}
	record = normalizeAdaptiveWALRecord(record)
	if record.AuthIndex == "" || record.Revision == 0 {
		return
	}
	if state.AdaptiveQuota.Revisions == nil {
		state.AdaptiveQuota.Revisions = make(map[string]uint64)
	}
	if record.Revision <= state.AdaptiveQuota.Revisions[record.AuthIndex] {
		return
	}
	if record.Saturated != nil {
		state.AdaptiveQuota.Saturated = *record.Saturated
		state.AdaptiveQuota.OverflowAuthCount = record.OverflowAuthCount
	}
	_, pendingExists := state.AdaptiveQuota.Pending[record.AuthIndex]
	_, preparedExists := state.AdaptiveQuota.Prepared[record.AuthIndex]
	_, revisionExists := state.AdaptiveQuota.Revisions[record.AuthIndex]
	introducesUnresolved := (record.Pending != nil || record.Prepared != nil) && !pendingExists && !preparedExists
	if (introducesUnresolved && adaptivePersistedLedgerAuthCount(state.AdaptiveQuota) >= adaptiveMaximumPersistedAuthRecords) ||
		(!revisionExists && len(state.AdaptiveQuota.Revisions) >= adaptiveMaximumPersistedAuthRecords) {
		state.AdaptiveQuota.Saturated = true
		state.AdaptiveQuota.OverflowAuthCount++
		return
	}
	if state.AdaptiveQuota.Pending == nil {
		state.AdaptiveQuota.Pending = make(map[string]*persistedAdaptivePendingState)
	}
	if record.Pending == nil {
		delete(state.AdaptiveQuota.Pending, record.AuthIndex)
	} else {
		copyPending := *record.Pending
		state.AdaptiveQuota.Pending[record.AuthIndex] = &copyPending
	}
	if state.AdaptiveQuota.Prepared == nil {
		state.AdaptiveQuota.Prepared = make(map[string]*persistedAdaptivePendingState)
	}
	if record.Prepared == nil {
		delete(state.AdaptiveQuota.Prepared, record.AuthIndex)
	} else {
		copyPrepared := *record.Prepared
		state.AdaptiveQuota.Prepared[record.AuthIndex] = &copyPrepared
	}
	state.AdaptiveQuota.Revisions[record.AuthIndex] = record.Revision
}

func adaptivePersistedLedgerAuthCount(state persistedAdaptiveQuotaState) int {
	keys := make(map[string]struct{}, len(state.Pending)+len(state.Prepared))
	for authIndex := range state.Pending {
		keys[authIndex] = struct{}{}
	}
	for authIndex := range state.Prepared {
		keys[authIndex] = struct{}{}
	}
	return len(keys)
}
