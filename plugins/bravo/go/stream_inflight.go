package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const duplicateStreamRetryAfterSeconds = 15

type bravoActiveStream struct {
	callbackID string
	streamID   string
}

type bravoActiveStreamRegistry struct {
	sync.Mutex
	streams map[string]bravoActiveStream
}

type bravoActiveStreamLease struct {
	key      string
	streamID string
}

var activeBravoStreams = bravoActiveStreamRegistry{
	streams: make(map[string]bravoActiveStream),
}

// acquireBravoActiveStream prevents an application-level retry from sending a
// second byte-identical request to a provider while the first stream is still
// active. Unique agent-team requests remain independent because their request
// bodies produce different fingerprints.
func acquireBravoActiveStream(
	req rpcExecutorRequest,
	cfg pluginConfig,
	logicalModel, protocol string,
	body []byte,
) (*bravoActiveStreamLease, *executionFailure) {
	project, authenticated := authenticatedExecutionProject(req, cfg)
	callbackID := strings.TrimSpace(req.HostCallbackID)
	streamID := strings.TrimSpace(req.StreamID)
	if !authenticated || strings.TrimSpace(project.ID) == "" || callbackID == "" || streamID == "" || len(body) == 0 {
		return nil, nil
	}

	key := bravoActiveStreamKey(project.ID, logicalModel, protocol, body)
	activeBravoStreams.Lock()
	defer activeBravoStreams.Unlock()
	if current, exists := activeBravoStreams.streams[key]; exists {
		failure := executionFailure{
			Code:       "bravo_duplicate_stream_in_flight",
			Message:    "An identical streaming request is already active for this project.",
			Status:     http.StatusTooManyRequests,
			Retryable:  true,
			RetryAfter: fmt.Sprintf("%d", duplicateStreamRetryAfterSeconds),
		}
		if current.callbackID == callbackID && current.streamID == streamID {
			failure.Retryable = false
		}
		return nil, &failure
	}

	activeBravoStreams.streams[key] = bravoActiveStream{
		callbackID: callbackID,
		streamID:   streamID,
	}
	return &bravoActiveStreamLease{key: key, streamID: streamID}, nil
}

func (lease *bravoActiveStreamLease) release() {
	if lease == nil || lease.key == "" || lease.streamID == "" {
		return
	}
	activeBravoStreams.Lock()
	if current, exists := activeBravoStreams.streams[lease.key]; exists && current.streamID == lease.streamID {
		delete(activeBravoStreams.streams, lease.key)
	}
	activeBravoStreams.Unlock()
}

func bravoActiveStreamKey(projectID, logicalModel, protocol string, body []byte) string {
	digest := sha256.Sum256(body)
	return strings.Join([]string{
		strings.TrimSpace(projectID),
		strings.ToLower(strings.TrimSpace(logicalModel)),
		strings.ToLower(strings.TrimSpace(protocol)),
		fmt.Sprintf("%x", digest),
	}, "\x00")
}
