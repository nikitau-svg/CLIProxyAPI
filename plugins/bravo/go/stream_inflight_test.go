package main

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBravoActiveStreamRejectsIdenticalConcurrentRetry(t *testing.T) {
	resetBravoActiveStreamsForTest(t)
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{{ID: "prj_stream_guard", Name: "guard"}}}
	first := bravoStreamGuardRequest("callback-1", "stream-1")
	lease, failure := acquireBravoActiveStream(first, cfg, "bravo/opus", protocolClaude, []byte(`{"model":"bravo/opus","stream":true}`))
	if failure != nil || lease == nil {
		t.Fatalf("first acquire = lease:%#v failure:%#v", lease, failure)
	}

	second := bravoStreamGuardRequest("callback-2", "stream-2")
	duplicateLease, duplicateFailure := acquireBravoActiveStream(second, cfg, "bravo/opus", protocolClaude, []byte(`{"model":"bravo/opus","stream":true}`))
	if duplicateLease != nil || duplicateFailure == nil {
		t.Fatalf("duplicate acquire = lease:%#v failure:%#v", duplicateLease, duplicateFailure)
	}
	if duplicateFailure.Code != "bravo_duplicate_stream_in_flight" ||
		duplicateFailure.Status != http.StatusTooManyRequests ||
		duplicateFailure.RetryAfter != "15" {
		t.Fatalf("duplicate failure = %#v", duplicateFailure)
	}

	lease.release()
	thirdLease, thirdFailure := acquireBravoActiveStream(second, cfg, "bravo/opus", protocolClaude, []byte(`{"model":"bravo/opus","stream":true}`))
	if thirdFailure != nil || thirdLease == nil {
		t.Fatalf("acquire after release = lease:%#v failure:%#v", thirdLease, thirdFailure)
	}
	thirdLease.release()
}

func TestBravoActiveStreamAllowsDistinctAgentRequests(t *testing.T) {
	resetBravoActiveStreamsForTest(t)
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{{ID: "prj_stream_guard", Name: "guard"}}}
	first := bravoStreamGuardRequest("callback-1", "stream-1")
	second := bravoStreamGuardRequest("callback-2", "stream-2")

	firstLease, firstFailure := acquireBravoActiveStream(first, cfg, "bravo/opus", protocolClaude, []byte(`{"messages":[{"content":"one"}]}`))
	if firstFailure != nil || firstLease == nil {
		t.Fatalf("first acquire = lease:%#v failure:%#v", firstLease, firstFailure)
	}
	defer firstLease.release()
	secondLease, secondFailure := acquireBravoActiveStream(second, cfg, "bravo/opus", protocolClaude, []byte(`{"messages":[{"content":"two"}]}`))
	if secondFailure != nil || secondLease == nil {
		t.Fatalf("distinct acquire = lease:%#v failure:%#v", secondLease, secondFailure)
	}
	secondLease.release()
}

func bravoStreamGuardRequest(callbackID, streamID string) rpcExecutorRequest {
	return rpcExecutorRequest{
		StreamID:       streamID,
		HostCallbackID: callbackID,
		ExecutorRequest: pluginapi.ExecutorRequest{Metadata: map[string]any{
			"access_metadata": map[string]string{
				bravoAccessProviderMetadataKey: pluginIdentifier,
				bravoProjectIDMetadataKey:      "prj_stream_guard",
				bravoKeyNameMetadataKey:        "guard",
			},
		}},
	}
}

func resetBravoActiveStreamsForTest(t *testing.T) {
	t.Helper()
	activeBravoStreams.Lock()
	previous := activeBravoStreams.streams
	activeBravoStreams.streams = make(map[string]bravoActiveStream)
	activeBravoStreams.Unlock()
	t.Cleanup(func() {
		activeBravoStreams.Lock()
		activeBravoStreams.streams = previous
		activeBravoStreams.Unlock()
	})
}
