package pluginhost

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamBridgeNotifyContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *streamBridgeNotifyContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestStreamBridgeCloseUnblocksPendingEmit(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, _ := bridge.open(context.Background())

	for range streamBridgeBufferSize {
		if err := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")}); err != nil {
			t.Fatalf("fill stream buffer: %v", err)
		}
	}

	emitCtx := &streamBridgeNotifyContext{
		Context: context.Background(),
		ready:   make(chan struct{}),
	}
	emitDone := make(chan error, 1)
	go func() {
		emitDone <- bridge.emit(emitCtx, streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("blocked")})
	}()

	select {
	case <-emitCtx.ready:
	case <-time.After(time.Second):
		t.Fatal("emit did not reach the blocked send")
	}
	select {
	case err := <-emitDone:
		t.Fatalf("emit returned while the stream buffer was full: %v", err)
	default:
	}

	bridge.close(streamID, "", 0, "", "")

	select {
	case err := <-emitDone:
		if err == nil || !strings.Contains(err.Error(), "is not open") {
			t.Fatalf("emit error = %v, want stream-not-open error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock the pending emit")
	}

	chunkCount := 0
	for range chunks {
		chunkCount++
	}
	if chunkCount != streamBridgeBufferSize {
		t.Fatalf("delivered chunks = %d, want %d buffered chunks without the rejected emit", chunkCount, streamBridgeBufferSize)
	}
}

func TestStreamBridgeEmitUsesAcceptedPumpResultAfterContextCancellation(t *testing.T) {
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		stream := &streamBridgeStream{
			emits:  make(chan streamBridgeEmit),
			closed: make(chan struct{}),
		}
		go func() {
			request := <-stream.emits
			cancel()
			request.done <- nil
		}()

		if err := stream.emit(ctx, pluginapi.ExecutorStreamChunk{Payload: []byte("accepted")}); err != nil {
			t.Fatalf("accepted emit returned error: %v", err)
		}
	}
}

func TestStreamBridgeAbortClosesSaturatedStreamWithoutConsumer(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	bridge.mu.Lock()
	stream := bridge.streams[streamID]
	bridge.mu.Unlock()

	for range streamBridgeBufferSize {
		if err := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")}); err != nil {
			t.Fatalf("fill stream buffer: %v", err)
		}
	}

	cleanup()

	select {
	case <-stream.finished:
	case <-time.After(time.Second):
		t.Fatal("abort left the saturated stream pump running")
	}
	if _, ok := <-chunks; ok {
		t.Fatal("aborted stream retained buffered chunks")
	}
}

func TestStreamBridgeCleanupAbortsPendingGracefulClose(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	bridge.mu.Lock()
	stream := bridge.streams[streamID]
	bridge.mu.Unlock()

	for range streamBridgeBufferSize {
		if err := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")}); err != nil {
			t.Fatalf("fill stream buffer: %v", err)
		}
	}
	bridge.close(streamID, "plugin stream failed", 0, "", "")

	cleanup()

	select {
	case <-stream.finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not abort the graceful close after the stream was removed")
	}
	if _, ok := <-chunks; ok {
		t.Fatal("cleanup retained queued chunks after aborting the graceful close")
	}
}

func TestStreamBridgeCloseDeliversTerminalError(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, _ := bridge.open(context.Background())

	bridge.close(streamID, "plugin stream failed", 0, "", "")

	chunk, ok := <-chunks
	if !ok {
		t.Fatal("stream closed before terminal error")
	}
	if chunk.Err == nil || chunk.Err.Error() != "plugin stream failed" {
		t.Fatalf("terminal error = %v, want plugin stream failed", chunk.Err)
	}
	if _, ok = <-chunks; ok {
		t.Fatal("stream remains open after terminal error")
	}
}

// A plugin that closes a stream with an upstream status must surface that
// status downstream. Without it the terminal error carries no status and the
// caller renders a bare 500 with no Retry-After, hiding a retryable outage.
func TestStreamBridgeCloseCarriesStatusMetadata(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, _ := bridge.open(context.Background())

	bridge.close(streamID, "bravo_no_eligible_account: no healthy account", http.StatusServiceUnavailable, "bravo_no_eligible_account", "30")

	chunk, ok := <-chunks
	if !ok {
		t.Fatal("stream closed before terminal error")
	}
	var typed *streamCloseError
	if !errors.As(chunk.Err, &typed) {
		t.Fatalf("terminal error = %T, want *streamCloseError", chunk.Err)
	}
	if typed.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode() = %d, want %d", typed.StatusCode(), http.StatusServiceUnavailable)
	}
	if typed.ErrorCode() != "bravo_no_eligible_account" {
		t.Fatalf("ErrorCode() = %q, want bravo_no_eligible_account", typed.ErrorCode())
	}
	if typed.RetryAfterValue() != "30" {
		t.Fatalf("RetryAfterValue() = %q, want 30", typed.RetryAfterValue())
	}
}

func TestStreamBridgeClosePreservesTerminalErrorWhenBufferIsFull(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, _ := bridge.open(context.Background())

	for range streamBridgeBufferSize {
		if err := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")}); err != nil {
			t.Fatalf("fill stream buffer: %v", err)
		}
	}

	closeDone := make(chan struct{})
	go func() {
		bridge.close(streamID, "plugin stream failed", 0, "", "")
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("close blocked on the saturated stream")
	}

	chunkCount := 0
	var terminalErr error
	for chunk := range chunks {
		chunkCount++
		if chunk.Err != nil {
			terminalErr = chunk.Err
		}
	}
	if chunkCount != streamBridgeBufferSize+1 {
		t.Fatalf("delivered chunks = %d, want %d buffered chunks plus terminal error", chunkCount, streamBridgeBufferSize+1)
	}
	if terminalErr == nil || terminalErr.Error() != "plugin stream failed" {
		t.Fatalf("terminal error = %v, want plugin stream failed", terminalErr)
	}
}
