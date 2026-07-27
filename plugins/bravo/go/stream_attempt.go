package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type bravoStreamAttemptRun struct {
	attempt       executionAttempt
	physicalModel string
	candidateBody []byte
	callbackID    string
	ownsScope     bool
	started       time.Time
	results       chan bravoStreamBootstrapResult
	releaseLease  func(bool)
	releaseOnce   sync.Once
	finalized     atomic.Bool
}

type bravoStreamBootstrapResult struct {
	response *pluginapi.HostModelStreamResponse
	failure  *executionFailure
	accepted bool
}

func launchBravoStreamAttempt(
	req rpcExecutorRequest,
	attempt executionAttempt,
	protocol, physicalModel string,
	candidateBody []byte,
	callbackID string,
	ownsScope bool,
	releaseLease func(bool),
) *bravoStreamAttemptRun {
	run := &bravoStreamAttemptRun{
		attempt:       attempt,
		physicalModel: physicalModel,
		candidateBody: candidateBody,
		callbackID:    callbackID,
		ownsScope:     ownsScope,
		started:       time.Now(),
		results:       make(chan bravoStreamBootstrapResult, 1),
		releaseLease:  releaseLease,
	}
	go run.execute(req, protocol)
	return run
}

func (r *bravoStreamAttemptRun) execute(req rpcExecutorRequest, protocol string) {
	completed := false
	defer func() {
		if completed {
			return
		}
		recovered := recover()
		r.release(true)
		r.closeScope()
		message := "provider bootstrap goroutine stopped without a result"
		if recovered != nil {
			message = fmt.Sprintf("provider bootstrap panic: %v", recovered)
		}
		failure := executionFailure{
			Code:    "bravo_stream_attempt_aborted",
			Message: message,
			Status:  http.StatusInternalServerError,
		}
		r.results <- bravoStreamBootstrapResult{failure: &failure, accepted: true}
	}()

	rawResponse, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: nestedHostModelRequest(
			req,
			r.attempt,
			protocol,
			r.physicalModel,
			r.candidateBody,
			true,
		),
		HostCallbackID: r.callbackID,
	})
	if errCall != nil {
		failure := classifyExecutionError(errCall)
		completed = true
		r.results <- bravoStreamBootstrapResult{failure: &failure}
		return
	}

	var response pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(rawResponse, &response); errDecode != nil {
		failure := executionFailure{
			Code:      "bravo_host_stream_invalid",
			Message:   errDecode.Error(),
			Status:    http.StatusBadGateway,
			Retryable: true,
		}
		completed = true
		r.results <- bravoStreamBootstrapResult{failure: &failure, accepted: true}
		return
	}
	if strings.TrimSpace(response.StreamID) == "" {
		failure := executionFailure{
			Code:      "bravo_host_stream_missing",
			Message:   "host returned an empty stream_id",
			Status:    http.StatusBadGateway,
			Retryable: true,
		}
		completed = true
		r.results <- bravoStreamBootstrapResult{failure: &failure, accepted: true}
		return
	}
	completed = true
	r.results <- bravoStreamBootstrapResult{response: &response}
}

func (r *bravoStreamAttemptRun) release(accepted bool) {
	if r == nil {
		return
	}
	r.releaseOnce.Do(func() {
		if r.releaseLease != nil {
			r.releaseLease(accepted)
		}
	})
}

func (r *bravoStreamAttemptRun) closeScope() {
	if r == nil || !r.ownsScope {
		return
	}
	_ = closeHostCallbackScope(r.callbackID)
}

func (r *bravoStreamAttemptRun) commitScope() error {
	if r == nil || !r.ownsScope {
		return nil
	}
	return commitHostCallbackScope(r.callbackID)
}

func (r *bravoStreamAttemptRun) supersede() {
	if r == nil || !r.finalized.CompareAndSwap(false, true) {
		return
	}
	// A hedged request may already have reached the provider even when its host
	// callback has not returned. Preserve the reservation until quota refresh.
	r.release(true)
	r.closeScope()
	failure := executionFailure{
		Code:    "bravo_attempt_superseded",
		Message: "A pre-commit provider attempt lost the Bravo hedge.",
		Status:  499,
	}
	recordExecutionAttempt(r.attempt, r.started, failure.Status, false, failure)
}

func (r *bravoStreamAttemptRun) cancelWithRequest(failure executionFailure) {
	if r == nil || !r.finalized.CompareAndSwap(false, true) {
		return
	}
	r.release(true)
	r.closeScope()
	failure.Code = "request_canceled"
	failure.Status = 499
	failure.Retryable = false
	failure.AccountWide = false
	failure.RetryAfter = ""
	recordExecutionAttempt(r.attempt, r.started, failure.Status, false, failure)
}

func (r *bravoStreamAttemptRun) abort(failure executionFailure) {
	if r == nil || !r.finalized.CompareAndSwap(false, true) {
		return
	}
	r.release(true)
	r.closeScope()
	recordExecutionAttempt(r.attempt, r.started, failure.Status, false, failure)
}

func settleBravoCompetingAttempt(
	run *bravoStreamAttemptRun,
	results <-chan bravoStreamBootstrapResult,
	requestFailure *executionFailure,
) {
	if run == nil {
		return
	}
	select {
	case result := <-results:
		if result.response != nil {
			if requestFailure != nil && requestFailure.Code == "request_canceled" {
				run.cancelWithRequest(*requestFailure)
			} else {
				run.supersede()
			}
			return
		}
		if result.failure == nil {
			result.failure = &executionFailure{
				Code:      "bravo_host_stream_invalid",
				Message:   "host returned an empty competing stream result",
				Status:    http.StatusBadGateway,
				Retryable: true,
			}
		}
		finishBravoStreamAttemptFailure(run, *result.failure, result.accepted)
	default:
		if requestFailure != nil && requestFailure.Code == "request_canceled" {
			run.cancelWithRequest(*requestFailure)
		} else {
			run.supersede()
		}
	}
}

func finishBravoStreamAttemptFailure(run *bravoStreamAttemptRun, failure executionFailure, accepted bool) {
	if run == nil || !run.finalized.CompareAndSwap(false, true) {
		return
	}
	applyCooldown := true
	if shouldCommitBravoCoreAccounting(failure) {
		if errCommit := run.commitScope(); errCommit != nil {
			failure = classifyExecutionError(errCommit)
			applyCooldown = false
		}
	}
	run.release(accepted)
	run.closeScope()
	recordExecutionAttempt(run.attempt, run.started, failure.Status, false, failure)
	if applyCooldown && failure.Code != "request_canceled" {
		applyFailureCooldown(run.attempt, failure)
	}
}

func shouldCommitBravoCoreAccounting(failure executionFailure) bool {
	code := strings.ToLower(strings.TrimSpace(failure.Code))
	switch code {
	case "bravo_subscription_auth_unavailable",
		"bravo_subscription_access_denied",
		"bravo_subscription_quota_exhausted",
		"bravo_subscription_model_unavailable":
		// These reviewed Bravo codes are normalized from real provider/Core
		// results and must retain Core availability and quota accounting.
		return true
	}
	if code == "" ||
		code == "request_canceled" ||
		code == "host_call_failed" ||
		strings.HasPrefix(code, "bravo_") ||
		strings.HasPrefix(code, "host_callback_") {
		return false
	}
	return true
}
