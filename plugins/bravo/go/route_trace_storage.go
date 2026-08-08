package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	routeTraceWALMagic         = uint32(0x42525431) // BRT1
	routeTraceWALHeaderSize    = 12
	routeTraceWALMaxPayload    = 256 << 10
	routeTraceWALBatchSize     = 64
	routeTraceWALGroupDelay    = 500 * time.Microsecond
	routeTraceSnapshotMaxBytes = 64 << 20
)

var (
	errRouteTraceStoreClosed        = errors.New("Bravo route trace store is closed")
	errRouteTracePersistenceQueue   = errors.New("Bravo route trace persistence queue is full")
	errRouteTracePersistenceTimeout = errors.New("Bravo route trace persistence timed out")
)

type routeTraceWALRecord struct {
	SchemaVersion int        `json:"schema_version"`
	Revision      uint64     `json:"revision"`
	Trace         routeTrace `json:"trace"`
}

type routeTracePersistKind uint8

const (
	routeTracePersistAppend routeTracePersistKind = iota + 1
	routeTracePersistFlush
	routeTracePersistClose
)

type routeTracePersistRequest struct {
	kind   routeTracePersistKind
	record routeTraceWALRecord
	ack    chan error
}

func (store *routeTraceStore) enqueueDurable(request routeTracePersistRequest) error {
	timer := time.NewTimer(store.terminalQueueTimeout)
	defer timer.Stop()
	select {
	case store.persistQueue <- request:
		return nil
	case <-store.persistDone:
		return errRouteTraceStoreClosed
	case <-timer.C:
		return errRouteTracePersistenceQueue
	}
}

func (store *routeTraceStore) setPersistenceWarning() {
	store.mu.Lock()
	store.persistenceFailures++
	store.loadError = "Не удалось сохранить аварийную трассу на диск; она остаётся доступна в памяти до перезапуска."
	store.mu.Unlock()
}

func (store *routeTraceStore) persistenceLoop() {
	defer close(store.persistDone)
	var deferred *routeTracePersistRequest
	for {
		var request routeTracePersistRequest
		if deferred != nil {
			request = *deferred
			deferred = nil
		} else {
			request = <-store.persistQueue
		}
		switch request.kind {
		case routeTracePersistAppend:
			batch := []routeTracePersistRequest{request}
			timer := time.NewTimer(routeTraceWALGroupDelay)
		collect:
			for len(batch) < routeTraceWALBatchSize {
				select {
				case next := <-store.persistQueue:
					if next.kind != routeTracePersistAppend {
						deferred = &next
						break collect
					}
					batch = append(batch, next)
				case <-timer.C:
					break collect
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			errWrite := store.appendWALBatch(batch)
			for index := range batch {
				if batch[index].ack != nil {
					batch[index].ack <- errWrite
				}
			}
			if errWrite != nil {
				store.setPersistenceWarning()
				continue
			}
			if store.shouldCompact() {
				if errCompact := store.writeSnapshotAndResetWAL(); errCompact != nil {
					store.setPersistenceWarning()
				}
			}
		case routeTracePersistFlush:
			errFlush := store.writeSnapshotAndResetWAL()
			if request.ack != nil {
				request.ack <- errFlush
			}
		case routeTracePersistClose:
			errClose := store.writeSnapshotAndResetWAL()
			if request.ack != nil {
				request.ack <- errClose
			}
			return
		}
	}
}

func (store *routeTraceStore) appendWALBatch(batch []routeTracePersistRequest) error {
	if len(batch) == 0 {
		return nil
	}
	if store.beforePersist != nil {
		store.beforePersist()
	}
	frames := make([]byte, 0, len(batch)*1024)
	for _, request := range batch {
		frame, errFrame := marshalRouteTraceWALFrame(request.record)
		if errFrame != nil {
			return errFrame
		}
		frames = append(frames, frame...)
	}
	store.mu.Lock()
	wouldExceed := (store.maxWALRecords > 0 && store.walRecords+len(batch) > store.maxWALRecords) ||
		(store.maxWALBytes > 0 && store.walBytes+int64(len(frames)) > store.maxWALBytes)
	store.mu.Unlock()
	if wouldExceed {
		// The snapshot already contains these in-memory records. Publishing it
		// durably is therefore a safe substitute for appending this batch.
		if errCompact := store.writeSnapshotAndResetWAL(); errCompact != nil {
			return fmt.Errorf("Bravo route trace WAL hard limit reached: %w", errCompact)
		}
		return nil
	}
	if errMkdir := os.MkdirAll(filepath.Dir(store.walPath), 0o700); errMkdir != nil {
		return fmt.Errorf("create Bravo route trace WAL directory: %w", errMkdir)
	}
	wal, errOpen := os.OpenFile(store.walPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open Bravo route trace WAL: %w", errOpen)
	}
	if errChmod := wal.Chmod(0o600); errChmod != nil {
		_ = wal.Close()
		return fmt.Errorf("protect Bravo route trace WAL: %w", errChmod)
	}
	if _, errWrite := wal.Write(frames); errWrite != nil {
		_ = wal.Close()
		return fmt.Errorf("append Bravo route trace WAL: %w", errWrite)
	}
	if errSync := wal.Sync(); errSync != nil {
		_ = wal.Close()
		return fmt.Errorf("sync Bravo route trace WAL: %w", errSync)
	}
	if errClose := wal.Close(); errClose != nil {
		return fmt.Errorf("close Bravo route trace WAL: %w", errClose)
	}
	if errSyncDir := syncRouteTraceDirectory(filepath.Dir(store.walPath)); errSyncDir != nil {
		return errSyncDir
	}
	store.mu.Lock()
	store.walRecords += len(batch)
	store.walBytes += int64(len(frames))
	store.mu.Unlock()
	return nil
}

func marshalRouteTraceWALFrame(record routeTraceWALRecord) ([]byte, error) {
	payload, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode Bravo route trace WAL record: %w", errMarshal)
	}
	if len(payload) == 0 || len(payload) > routeTraceWALMaxPayload {
		return nil, fmt.Errorf("Bravo route trace WAL record size %d exceeds limit", len(payload))
	}
	frame := make([]byte, routeTraceWALHeaderSize+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], routeTraceWALMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(payload))
	copy(frame[routeTraceWALHeaderSize:], payload)
	return frame, nil
}

func (store *routeTraceStore) loadLocked() error {
	if store.loaded {
		return nil
	}
	store.loaded = true
	raw, oversized, errRead := readRouteTraceFileBounded(store.path, routeTraceSnapshotMaxBytes)
	if errRead == nil {
		if oversized {
			return fmt.Errorf("Bravo route trace snapshot exceeds %d bytes", routeTraceSnapshotMaxBytes)
		}
		snapshot, errDecode := decodeRouteTraceSnapshot(raw, store.maxEntries)
		if errDecode != nil {
			return fmt.Errorf("decode Bravo route traces: %w", errDecode)
		}
		if snapshot.SchemaVersion != routeTraceSchemaVersion {
			return fmt.Errorf("unsupported Bravo route trace schema %d", snapshot.SchemaVersion)
		}
		store.snapshotRevision = snapshot.Revision
		store.nextRevision = snapshot.Revision
		store.traces = make([]routeTrace, 0, len(snapshot.Traces))
		for _, trace := range snapshot.Traces {
			store.traces = append(store.traces, sanitizeRouteTrace(trace))
		}
	} else if !errors.Is(errRead, os.ErrNotExist) {
		return fmt.Errorf("read Bravo route traces: %w", errRead)
	}
	if errReplay := store.replayWALLocked(); errReplay != nil {
		return errReplay
	}
	store.pruneLocked(time.Now().UTC())
	return nil
}

func (store *routeTraceStore) replayWALLocked() error {
	limit := store.maxWALBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	raw, oversized, errRead := readRouteTraceFileBounded(store.walPath, limit)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read Bravo route trace WAL: %w", errRead)
	}
	offset := 0
	validOffset := 0
	replayed := 0
	for offset < len(raw) {
		if len(raw)-offset < routeTraceWALHeaderSize {
			break
		}
		header := raw[offset : offset+routeTraceWALHeaderSize]
		if binary.BigEndian.Uint32(header[0:4]) != routeTraceWALMagic {
			break
		}
		payloadSize := int(binary.BigEndian.Uint32(header[4:8]))
		if payloadSize <= 0 || payloadSize > routeTraceWALMaxPayload || len(raw)-offset-routeTraceWALHeaderSize < payloadSize {
			break
		}
		payload := raw[offset+routeTraceWALHeaderSize : offset+routeTraceWALHeaderSize+payloadSize]
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[8:12]) {
			break
		}
		var record routeTraceWALRecord
		if errDecode := json.Unmarshal(payload, &record); errDecode != nil || record.SchemaVersion != routeTraceSchemaVersion || record.Revision == 0 {
			break
		}
		validOffset = offset + routeTraceWALHeaderSize + payloadSize
		offset = validOffset
		if record.Revision > store.nextRevision {
			store.nextRevision = record.Revision
		}
		if record.Revision <= store.snapshotRevision {
			continue
		}
		store.traces = append(store.traces, sanitizeRouteTrace(record.Trace))
		replayed++
	}
	store.walRecords = replayed
	store.walBytes = int64(validOffset)
	if validOffset == len(raw) && !oversized {
		return nil
	}
	store.loadError = "Хвост журнала трасс был повреждён или записан не полностью; подтверждённые записи восстановлены."
	if errTruncate := truncateRouteTraceWAL(store.walPath, int64(validOffset)); errTruncate != nil {
		return errTruncate
	}
	return nil
}

func (store *routeTraceStore) shouldCompact() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	threshold := store.compactAfterRecords
	if threshold <= 0 {
		threshold = 128
	}
	return store.walRecords >= threshold
}

func (store *routeTraceStore) writeSnapshotAndResetWAL() error {
	if store.beforeSnapshot != nil {
		if errHook := store.beforeSnapshot(); errHook != nil {
			return errHook
		}
	}
	store.mu.Lock()
	if errLoad := store.loadLocked(); errLoad != nil {
		store.mu.Unlock()
		return errLoad
	}
	store.pruneLocked(time.Now().UTC())
	snapshot := routeTraceSnapshot{
		SchemaVersion: routeTraceSchemaVersion,
		Revision:      store.nextRevision,
		UpdatedAt:     time.Now().UTC(),
		Traces:        cloneRouteTraces(store.traces),
	}
	store.mu.Unlock()

	directory := filepath.Dir(store.path)
	if errMkdir := os.MkdirAll(directory, 0o700); errMkdir != nil {
		return fmt.Errorf("create Bravo route trace directory: %w", errMkdir)
	}
	temporary := store.path + ".tmp"
	file, errOpen := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open Bravo route trace snapshot: %w", errOpen)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if errWrite := writeRouteTraceSnapshot(file, snapshot); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write Bravo route trace snapshot: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return fmt.Errorf("sync Bravo route trace snapshot: %w", errSync)
	}
	if errChmod := file.Chmod(0o600); errChmod != nil {
		_ = file.Close()
		return fmt.Errorf("protect Bravo route trace snapshot: %w", errChmod)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close Bravo route trace snapshot: %w", errClose)
	}
	if errRename := os.Rename(temporary, store.path); errRename != nil {
		return fmt.Errorf("replace Bravo route trace snapshot: %w", errRename)
	}
	removeTemporary = false
	if errSyncDir := syncRouteTraceDirectory(directory); errSyncDir != nil {
		return errSyncDir
	}
	if store.beforeWALReset != nil {
		if errHook := store.beforeWALReset(); errHook != nil {
			return errHook
		}
	}
	if errTruncate := truncateRouteTraceWAL(store.walPath, 0); errTruncate != nil {
		return errTruncate
	}
	store.mu.Lock()
	store.snapshotRevision = snapshot.Revision
	store.walRecords = 0
	store.walBytes = 0
	store.mu.Unlock()
	return nil
}

func decodeRouteTraceSnapshot(raw []byte, maxEntries int) (routeTraceSnapshot, error) {
	if maxEntries <= 0 {
		maxEntries = defaultRouteTraceLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, errToken := decoder.Token()
	if errToken != nil || opening != json.Delim('{') {
		return routeTraceSnapshot{}, errors.New("snapshot is not a JSON object")
	}
	snapshot := routeTraceSnapshot{}
	ring := make([]routeTrace, maxEntries)
	traceCount := 0
	for decoder.More() {
		keyToken, errKey := decoder.Token()
		if errKey != nil {
			return routeTraceSnapshot{}, errKey
		}
		key, ok := keyToken.(string)
		if !ok {
			return routeTraceSnapshot{}, errors.New("snapshot field name is invalid")
		}
		switch key {
		case "schema_version":
			if errDecode := decoder.Decode(&snapshot.SchemaVersion); errDecode != nil {
				return routeTraceSnapshot{}, errDecode
			}
		case "revision":
			if errDecode := decoder.Decode(&snapshot.Revision); errDecode != nil {
				return routeTraceSnapshot{}, errDecode
			}
		case "updated_at":
			if errDecode := decoder.Decode(&snapshot.UpdatedAt); errDecode != nil {
				return routeTraceSnapshot{}, errDecode
			}
		case "traces":
			arrayStart, errArray := decoder.Token()
			if errArray != nil || arrayStart != json.Delim('[') {
				return routeTraceSnapshot{}, errors.New("snapshot traces is not an array")
			}
			for decoder.More() {
				var encoded json.RawMessage
				if errDecode := decoder.Decode(&encoded); errDecode != nil {
					return routeTraceSnapshot{}, errDecode
				}
				if len(encoded) > routeTraceWALMaxPayload {
					return routeTraceSnapshot{}, fmt.Errorf("snapshot trace size %d exceeds limit", len(encoded))
				}
				var trace routeTrace
				if errDecode := json.Unmarshal(encoded, &trace); errDecode != nil {
					return routeTraceSnapshot{}, errDecode
				}
				ring[traceCount%maxEntries] = sanitizeRouteTrace(trace)
				traceCount++
			}
			if _, errArray := decoder.Token(); errArray != nil {
				return routeTraceSnapshot{}, errArray
			}
		default:
			var ignored json.RawMessage
			if errDecode := decoder.Decode(&ignored); errDecode != nil {
				return routeTraceSnapshot{}, errDecode
			}
		}
	}
	if _, errToken := decoder.Token(); errToken != nil {
		return routeTraceSnapshot{}, errToken
	}
	kept := traceCount
	if kept > maxEntries {
		kept = maxEntries
	}
	snapshot.Traces = make([]routeTrace, 0, kept)
	start := traceCount - kept
	for index := start; index < traceCount; index++ {
		snapshot.Traces = append(snapshot.Traces, ring[index%maxEntries])
	}
	return snapshot, nil
}

func writeRouteTraceSnapshot(file *os.File, snapshot routeTraceSnapshot) error {
	metadata := struct {
		SchemaVersion int       `json:"schema_version"`
		Revision      uint64    `json:"revision,omitempty"`
		UpdatedAt     time.Time `json:"updated_at,omitempty"`
	}{snapshot.SchemaVersion, snapshot.Revision, snapshot.UpdatedAt}
	prefix, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return errMarshal
	}
	if len(prefix) == 0 || prefix[len(prefix)-1] != '}' {
		return errors.New("invalid snapshot metadata encoding")
	}
	prefix = append(prefix[:len(prefix)-1], []byte(`,"traces":[`)...)
	written := int64(0)
	writePart := func(part []byte) error {
		if written+int64(len(part)) > routeTraceSnapshotMaxBytes {
			return fmt.Errorf("snapshot exceeds %d bytes", routeTraceSnapshotMaxBytes)
		}
		for len(part) > 0 {
			count, errWrite := file.Write(part)
			if errWrite != nil {
				return errWrite
			}
			if count <= 0 {
				return io.ErrShortWrite
			}
			written += int64(count)
			part = part[count:]
		}
		return nil
	}
	if errWrite := writePart(prefix); errWrite != nil {
		return errWrite
	}
	for index, trace := range snapshot.Traces {
		encoded, errMarshal := json.Marshal(trace)
		if errMarshal != nil {
			return errMarshal
		}
		if len(encoded) > routeTraceWALMaxPayload {
			return fmt.Errorf("snapshot trace size %d exceeds limit", len(encoded))
		}
		if index > 0 {
			if errWrite := writePart([]byte{','}); errWrite != nil {
				return errWrite
			}
		}
		if errWrite := writePart(encoded); errWrite != nil {
			return errWrite
		}
	}
	return writePart([]byte("]}\n"))
}

func readRouteTraceFileBounded(path string, limit int64) ([]byte, bool, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, false, errOpen
	}
	defer func() { _ = file.Close() }()
	if limit <= 0 {
		return nil, false, fmt.Errorf("invalid Bravo route trace file limit %d", limit)
	}
	reader := io.LimitReader(file, limit+1)
	raw, errRead := io.ReadAll(reader)
	if errRead != nil {
		return nil, false, errRead
	}
	if int64(len(raw)) > limit {
		return raw[:limit], true, nil
	}
	return raw, false, nil
}

func maxRouteTraceWALRevision(path string, limit int64) uint64 {
	raw, _, errRead := readRouteTraceFileBounded(path, limit)
	if errRead != nil {
		return 0
	}
	var maximum uint64
	for offset := 0; offset+routeTraceWALHeaderSize <= len(raw); {
		header := raw[offset : offset+routeTraceWALHeaderSize]
		if binary.BigEndian.Uint32(header[0:4]) != routeTraceWALMagic {
			break
		}
		payloadSize := int(binary.BigEndian.Uint32(header[4:8]))
		if payloadSize <= 0 || payloadSize > routeTraceWALMaxPayload || len(raw)-offset-routeTraceWALHeaderSize < payloadSize {
			break
		}
		payload := raw[offset+routeTraceWALHeaderSize : offset+routeTraceWALHeaderSize+payloadSize]
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[8:12]) {
			break
		}
		var record routeTraceWALRecord
		if errDecode := json.Unmarshal(payload, &record); errDecode != nil || record.SchemaVersion != routeTraceSchemaVersion {
			break
		}
		if record.Revision > maximum {
			maximum = record.Revision
		}
		offset += routeTraceWALHeaderSize + payloadSize
	}
	return maximum
}

func (store *routeTraceStore) recoverAfterLoadFailure() error {
	recoveryRevision := maxRouteTraceWALRevision(store.walPath, store.maxWALBytes)
	store.mu.Lock()
	store.loadError = "Предыдущий файл трасс повреждён или несовместим; новые трассы записываются в свежий безопасный снимок."
	store.loaded = true
	store.traces = nil
	store.nextRevision = recoveryRevision
	store.snapshotRevision = recoveryRevision
	store.walRecords = 0
	store.walBytes = 0
	store.mu.Unlock()
	if errReset := store.flush(); errReset != nil {
		store.mu.Lock()
		store.loadError = "Хранилище трасс повреждено, и свежий безопасный снимок пока не удалось создать; новые трассы доступны только в памяти."
		// Force the next persistence attempt through snapshot recovery rather
		// than appending behind an unreset or oversized legacy WAL.
		store.walRecords = store.maxWALRecords
		store.walBytes = store.maxWALBytes
		store.mu.Unlock()
		return errReset
	}
	return nil
}

func cloneRouteTraces(traces []routeTrace) []routeTrace {
	out := append([]routeTrace(nil), traces...)
	for index := range out {
		out[index].Attempts = append([]routeTraceAttempt(nil), out[index].Attempts...)
	}
	return out
}

func (store *routeTraceStore) mergeFallbackTracesLocked(traces []routeTrace) {
	seen := make(map[string]struct{}, len(store.traces))
	for _, trace := range store.traces {
		if trace.TraceID != "" {
			seen[trace.TraceID] = struct{}{}
		}
	}
	for _, trace := range traces {
		trace = sanitizeRouteTrace(trace)
		if trace.TraceID != "" {
			if _, exists := seen[trace.TraceID]; exists {
				continue
			}
			seen[trace.TraceID] = struct{}{}
		}
		store.traces = append(store.traces, trace)
		store.nextRevision++
	}
	store.pruneLocked(time.Now().UTC())
}

func truncateRouteTraceWAL(path string, size int64) error {
	directory := filepath.Dir(path)
	if errMkdir := os.MkdirAll(directory, 0o700); errMkdir != nil {
		return fmt.Errorf("create Bravo route trace WAL directory: %w", errMkdir)
	}
	wal, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open Bravo route trace WAL for truncation: %w", errOpen)
	}
	if errTruncate := wal.Truncate(size); errTruncate != nil {
		_ = wal.Close()
		return fmt.Errorf("truncate Bravo route trace WAL: %w", errTruncate)
	}
	if _, errSeek := wal.Seek(size, io.SeekStart); errSeek != nil {
		_ = wal.Close()
		return fmt.Errorf("seek Bravo route trace WAL: %w", errSeek)
	}
	if errSync := wal.Sync(); errSync != nil {
		_ = wal.Close()
		return fmt.Errorf("sync Bravo route trace WAL truncation: %w", errSync)
	}
	if errClose := wal.Close(); errClose != nil {
		return fmt.Errorf("close Bravo route trace WAL after truncation: %w", errClose)
	}
	return syncRouteTraceDirectory(directory)
}

func syncRouteTraceDirectory(path string) error {
	directory, errOpen := os.Open(path)
	if errOpen != nil {
		return fmt.Errorf("open Bravo route trace directory: %w", errOpen)
	}
	defer func() { _ = directory.Close() }()
	if errSync := directory.Sync(); errSync != nil {
		return fmt.Errorf("sync Bravo route trace directory: %w", errSync)
	}
	return nil
}

func (store *routeTraceStore) flush() error {
	if store == nil {
		return nil
	}
	if store.memoryOnly {
		return nil
	}
	ack := make(chan error, 1)
	request := routeTracePersistRequest{kind: routeTracePersistFlush, ack: ack}
	select {
	case store.persistQueue <- request:
	case <-store.persistDone:
		return errRouteTraceStoreClosed
	}
	select {
	case errFlush := <-ack:
		return errFlush
	case <-store.persistDone:
		return errRouteTraceStoreClosed
	}
}

func (store *routeTraceStore) close() error {
	if store == nil {
		return nil
	}
	if store.memoryOnly {
		store.appendMu.Lock()
		store.mu.Lock()
		store.closed = true
		store.mu.Unlock()
		store.appendMu.Unlock()
		return nil
	}
	store.appendMu.Lock()
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		store.appendMu.Unlock()
		<-store.persistDone
		return nil
	}
	store.closed = true
	store.mu.Unlock()
	store.appendMu.Unlock()

	ack := make(chan error, 1)
	store.persistQueue <- routeTracePersistRequest{kind: routeTracePersistClose, ack: ack}
	errClose := <-ack
	<-store.persistDone
	return errClose
}

func (store *routeTraceStore) trimCountLocked() {
	if store.maxEntries <= 0 {
		store.maxEntries = defaultRouteTraceLimit
	}
	if excess := len(store.traces) - store.maxEntries; excess > 0 {
		for index := 0; index < excess; index++ {
			store.traces[index] = routeTrace{}
		}
		store.traces = store.traces[excess:]
	}
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
	for index := len(kept); index < len(store.traces); index++ {
		store.traces[index] = routeTrace{}
	}
	store.traces = kept
	store.trimCountLocked()
}
