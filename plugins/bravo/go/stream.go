package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcStreamEmitRequest struct {
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
	Payload        []byte `json:"payload,omitempty"`
	Error          string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID    string `json:"stream_id"`
	Error       string `json:"error,omitempty"`
	ErrorStatus int    `json:"error_status,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	RetryAfter  string `json:"retry_after,omitempty"`
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return failureEnvelope(executionFailure{
			Code:    "bravo_stream_id_required",
			Message: "stream_id is required for executor.execute_stream",
			Status:  http.StatusBadRequest,
		}), nil
	}
	_, model, _, failure := prepareBravoExecution(req)
	if failure != nil {
		return failureEnvelope(*failure), nil
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		return failureEnvelope(contractFailure(errDetect)), nil
	}
	if errPreflight := verifyLogicalModelContract(model, contract); errPreflight != nil {
		return failureEnvelope(contractFailure(errPreflight)), nil
	}
	go runBravoStream(req, streamID)
	return okEnvelope(map[string]any{
		"headers": http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
	})
}

func runBravoStream(req rpcExecutorRequest, pluginStreamID string) {
	launchedRuns := make([]*bravoStreamAttemptRun, 0, 2)
	defer func() {
		if recovered := recover(); recovered != nil {
			failure := executionFailure{
				Code:    "bravo_stream_panic",
				Message: fmt.Sprintf("Bravo stream coordinator panic: %v", recovered),
				Status:  http.StatusInternalServerError,
			}
			for _, run := range launchedRuns {
				run.abort(failure)
			}
			closePluginStream(pluginStreamID, fmt.Sprintf("bravo_stream_panic: %v", recovered))
		}
	}()

	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		closePluginStreamFailure(pluginStreamID, *failure)
		return
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		closePluginStreamFailure(pluginStreamID, contractFailure(errDetect))
		return
	}
	candidateSourceBody, errStrip := stripRequestEffort(body, protocol, contract.Effort)
	if errStrip != nil {
		closePluginStream(pluginStreamID, "bravo_request_invalid: "+errStrip.Error())
		return
	}
	hedgeDelay := time.Duration(cfg.FallbackHedgeDelaySeconds) * time.Second
	hedgeAt := time.Time{}
	plan, errPlan := buildExecutionPlan(req, logicalName, model, contract)
	if errPlan != nil {
		closePluginStreamFailure(pluginStreamID, executionFailure{
			Code:      "bravo_no_eligible_account",
			Message:   errPlan.Error(),
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		})
		return
	}
	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	var lastFailure executionFailure
	providerCalls := 0
	attempted := make(map[int]bool, len(plan))
	hedgeUsed := false

	canHedgeFrom := func(index int) bool {
		if hedgeUsed ||
			hedgeDelay <= 0 ||
			strings.TrimSpace(req.HostCallbackID) == "" ||
			providerCallBudgetExhausted(cfg.MaxAttempts, providerCalls+1) {
			return false
		}
		provider := normalizeProvider(plan[index].Candidate.Provider)
		for next := index + 1; next < len(plan); next++ {
			if attempted[next] ||
				normalizeProvider(plan[next].Candidate.Provider) == provider ||
				verifyCandidateContract(plan[next].Candidate, contract) != nil {
				continue
			}
			return true
		}
		return false
	}

	startAttempt := func(index int, childScope bool) (*bravoStreamAttemptRun, *executionFailure, bool) {
		attempt := plan[index]
		if skipCoolingExecutionAttempt(attempt, &lastFailure) {
			return nil, nil, false
		}
		if errPreflight := verifyCandidateContract(attempt.Candidate, contract); errPreflight != nil {
			failure := contractFailure(errPreflight)
			lastFailure = failure
			return nil, &failure, false
		}
		physicalModel := candidateModelName(attempt.Candidate)
		candidateBody, errRewrite := rewriteCandidateRequest(candidateSourceBody, protocol, physicalModel, true, req.Headers.Get("Content-Type"))
		if errRewrite != nil {
			failure := executionFailure{
				Code:    "bravo_request_invalid",
				Message: errRewrite.Error(),
				Status:  http.StatusBadRequest,
			}
			return nil, &failure, false
		}
		if providerCallBudgetExhausted(cfg.MaxAttempts, providerCalls) {
			return nil, nil, false
		}
		callbackID := req.HostCallbackID
		ownsScope := false
		if childScope {
			childID, errFork := forkHostCallbackScope(req.HostCallbackID)
			if errFork != nil {
				failure := classifyExecutionError(errFork)
				return nil, &failure, false
			}
			callbackID = childID
			ownsScope = true
		}
		releaseLease, acquired := acquireAttemptLease(attempt)
		if !acquired {
			if ownsScope {
				_ = closeHostCallbackScope(callbackID)
			}
			return nil, nil, false
		}
		providerCalls++
		run := launchBravoStreamAttempt(
			req,
			attempt,
			protocol,
			physicalModel,
			candidateBody,
			callbackID,
			ownsScope,
			releaseLease,
		)
		launchedRuns = append(launchedRuns, run)
		return run, nil, true
	}

	findHedge := func(primaryIndex int) (*bravoStreamAttemptRun, *executionFailure, bool) {
		primaryProvider := normalizeProvider(plan[primaryIndex].Candidate.Provider)
		for index := primaryIndex + 1; index < len(plan); index++ {
			if attempted[index] || normalizeProvider(plan[index].Candidate.Provider) == primaryProvider {
				continue
			}
			run, failure, launched := startAttempt(index, true)
			if launched {
				attempted[index] = true
				return run, nil, true
			}
			if failure != nil {
				if failure.Status == http.StatusUnprocessableEntity {
					attempted[index] = true
					lastFailure = *failure
					continue
				}
				return nil, failure, false
			}
			// The account entered cooldown or lost its local reservation after
			// the plan was built. A same-request retry cannot make it eligible.
			attempted[index] = true
		}
		return nil, nil, false
	}

	for index := 0; index < len(plan); index++ {
		if attempted[index] {
			continue
		}
		attempted[index] = true
		primaryCanHedge := canHedgeFrom(index)
		primary, preflightFailure, launched := startAttempt(index, primaryCanHedge)
		if preflightFailure != nil {
			lastFailure = *preflightFailure
			if preflightFailure.Status == http.StatusUnprocessableEntity {
				continue
			}
			if !preflightFailure.Retryable {
				closePluginStreamFailure(pluginStreamID, *preflightFailure)
				return
			}
			continue
		}
		if !launched || primary == nil {
			if providerCallBudgetExhausted(cfg.MaxAttempts, providerCalls) {
				break
			}
			continue
		}
		if primaryCanHedge && hedgeAt.IsZero() {
			// Planning and quota discovery do not consume the primary's head
			// start. The request-wide hedge clock begins with the first real
			// provider attempt.
			hedgeAt = primary.started.Add(hedgeDelay)
		}

		primaryResults := (<-chan bravoStreamBootstrapResult)(primary.results)
		var hedge *bravoStreamAttemptRun
		var hedgeResults <-chan bravoStreamBootstrapResult
		var hedgeTimer *time.Timer
		var hedgeTimerC <-chan time.Time
		if primaryCanHedge {
			delay := time.Until(hedgeAt)
			if delay < 0 {
				delay = 0
			}
			hedgeTimer = time.NewTimer(delay)
			hedgeTimerC = hedgeTimer.C
		}
		stopHedgeTimer := func() {
			hedgeTimerC = nil
			if hedgeTimer == nil {
				return
			}
			if !hedgeTimer.Stop() {
				select {
				case <-hedgeTimer.C:
				default:
				}
			}
			hedgeTimer = nil
		}
		var deferredHedgeTerminal *executionFailure

		for primaryResults != nil || hedgeResults != nil {
			select {
			case result := <-primaryResults:
				primaryResults = nil
				if result.response != nil {
					stopHedgeTimer()
					finished, winnerFailure := forwardBravoStreamWinner(
						primary,
						result.response,
						pluginStreamID,
						protocol,
						logicalModelID,
						func() error {
							if hedgeResults != nil {
								settleBravoCompetingAttempt(hedge, hedgeResults, nil)
								hedgeResults = nil
							}
							return primary.commitScope()
						},
					)
					if finished {
						if hedgeResults != nil {
							settleBravoCompetingAttempt(hedge, hedgeResults, nil)
							hedgeResults = nil
						}
						return
					}
					if winnerFailure != nil {
						lastFailure = *winnerFailure
						if !winnerFailure.Retryable {
							if hedgeResults != nil {
								settleBravoCompetingAttempt(hedge, hedgeResults, winnerFailure)
								hedgeResults = nil
							}
							closePluginStreamFailure(pluginStreamID, *winnerFailure)
							return
						}
					}
					if hedgeResults == nil && deferredHedgeTerminal != nil {
						closePluginStreamFailure(pluginStreamID, *deferredHedgeTerminal)
						return
					}
					continue
				}
				if result.failure == nil {
					result.failure = &executionFailure{
						Code:      "bravo_host_stream_invalid",
						Message:   "host returned an empty stream bootstrap result",
						Status:    http.StatusBadGateway,
						Retryable: true,
					}
				}
				finishBravoStreamAttemptFailure(primary, *result.failure, result.accepted)
				lastFailure = *result.failure
				if !result.failure.Retryable {
					stopHedgeTimer()
					if hedgeResults != nil {
						settleBravoCompetingAttempt(hedge, hedgeResults, result.failure)
						hedgeResults = nil
					}
					closePluginStreamFailure(pluginStreamID, *result.failure)
					return
				}
				if hedgeResults == nil && deferredHedgeTerminal != nil {
					closePluginStreamFailure(pluginStreamID, *deferredHedgeTerminal)
					return
				}
			case result := <-hedgeResults:
				hedgeResults = nil
				if result.response != nil {
					finished, winnerFailure := forwardBravoStreamWinner(
						hedge,
						result.response,
						pluginStreamID,
						protocol,
						logicalModelID,
						func() error {
							if primaryResults != nil {
								settleBravoCompetingAttempt(primary, primaryResults, nil)
								primaryResults = nil
							}
							return hedge.commitScope()
						},
					)
					if finished {
						if primaryResults != nil {
							settleBravoCompetingAttempt(primary, primaryResults, nil)
							primaryResults = nil
						}
						return
					}
					if winnerFailure != nil {
						lastFailure = *winnerFailure
						if !winnerFailure.Retryable {
							if winnerFailure.Code == "request_canceled" {
								if primaryResults != nil {
									settleBravoCompetingAttempt(primary, primaryResults, winnerFailure)
									primaryResults = nil
								}
								closePluginStreamFailure(pluginStreamID, *winnerFailure)
								return
							}
							if primaryResults != nil {
								failureCopy := *winnerFailure
								deferredHedgeTerminal = &failureCopy
							} else {
								closePluginStreamFailure(pluginStreamID, *winnerFailure)
								return
							}
						}
					}
					continue
				}
				if result.failure == nil {
					result.failure = &executionFailure{
						Code:      "bravo_host_stream_invalid",
						Message:   "host returned an empty hedge bootstrap result",
						Status:    http.StatusBadGateway,
						Retryable: true,
					}
				}
				finishBravoStreamAttemptFailure(hedge, *result.failure, result.accepted)
				lastFailure = *result.failure
				if !result.failure.Retryable {
					if result.failure.Code == "request_canceled" {
						if primaryResults != nil {
							settleBravoCompetingAttempt(primary, primaryResults, result.failure)
							primaryResults = nil
						}
						closePluginStreamFailure(pluginStreamID, *result.failure)
						return
					}
					if primaryResults != nil {
						failureCopy := *result.failure
						deferredHedgeTerminal = &failureCopy
					} else {
						closePluginStreamFailure(pluginStreamID, *result.failure)
						return
					}
				}
			case <-hedgeTimerC:
				hedgeTimerC = nil
				hedgeUsed = true
				var hedgeFailure *executionFailure
				var hedgeLaunched bool
				hedge, hedgeFailure, hedgeLaunched = findHedge(index)
				if hedgeFailure != nil {
					lastFailure = *hedgeFailure
					if !hedgeFailure.Retryable {
						if hedgeFailure.Code == "request_canceled" {
							if primaryResults != nil {
								settleBravoCompetingAttempt(primary, primaryResults, hedgeFailure)
								primaryResults = nil
							}
							closePluginStreamFailure(pluginStreamID, *hedgeFailure)
							return
						}
						if primaryResults == nil {
							closePluginStreamFailure(pluginStreamID, *hedgeFailure)
							return
						}
						failureCopy := *hedgeFailure
						deferredHedgeTerminal = &failureCopy
					}
				}
				if hedgeLaunched && hedge != nil {
					hedgeResults = hedge.results
				}
			}
		}
		stopHedgeTimer()
		if deferredHedgeTerminal != nil {
			closePluginStreamFailure(pluginStreamID, *deferredHedgeTerminal)
			return
		}
	}

	if lastFailure.Code == "" {
		lastFailure = executionFailure{
			Code:    "bravo_contract_unavailable",
			Message: "No configured candidate can preserve this streaming request contract.",
			Status:  http.StatusUnprocessableEntity,
		}
	}
	closePluginStreamFailure(pluginStreamID, lastFailure)
}

func forwardBravoStreamWinner(
	run *bravoStreamAttemptRun,
	response *pluginapi.HostModelStreamResponse,
	pluginStreamID, protocol, logicalModelID string,
	onCommit func() error,
) (bool, *executionFailure) {
	if run == nil || response == nil {
		failure := executionFailure{
			Code:      "bravo_host_stream_invalid",
			Message:   "host returned an empty winning stream",
			Status:    http.StatusBadGateway,
			Retryable: true,
		}
		return false, &failure
	}
	committed, streamFailure := forwardCandidateStream(
		context.Background(),
		response.StreamID,
		pluginStreamID,
		protocol,
		run.physicalModel,
		logicalModelID,
		run.callbackID,
		onCommit,
	)
	if !committed && streamFailure == nil {
		if errCommit := run.commitScope(); errCommit != nil {
			failure := classifyExecutionError(errCommit)
			streamFailure = &failure
		}
	} else if !committed && streamFailure != nil && shouldCommitBravoCoreAccounting(*streamFailure) {
		if errCommit := run.commitScope(); errCommit != nil {
			failure := classifyExecutionError(errCommit)
			streamFailure = &failure
		}
	}
	_ = closeHostModelStream(response.StreamID, run.callbackID)
	run.closeScope()
	run.release(true)
	if !run.finalized.CompareAndSwap(false, true) {
		return true, nil
	}
	if streamFailure == nil {
		recordExecutionAttempt(run.attempt, run.started, http.StatusOK, true, executionFailure{})
		closePluginStream(pluginStreamID, "")
		return true, nil
	}
	recordExecutionAttempt(run.attempt, run.started, streamFailure.Status, false, *streamFailure)
	if shouldCommitBravoCoreAccounting(*streamFailure) {
		applyFailureCooldown(run.attempt, *streamFailure)
	}
	if committed {
		// Switching provider after the client observed bytes would splice two
		// unrelated streams. Fail closed and let the caller retry explicitly.
		closePluginStream(pluginStreamID, streamFailure.Code+": "+streamFailure.Message)
		return true, streamFailure
	}
	return false, streamFailure
}

func forwardCandidateStream(
	ctx context.Context,
	hostStreamID, pluginStreamID, protocol, physicalModel, logicalModel, hostCallbackID string,
	onCommit func() error,
) (bool, *executionFailure) {
	rewriter := streamModelRewriter{
		physical: physicalModel,
		logical:  logicalModel,
		protocol: protocol,
	}
	committed := false
	for {
		rawChunk, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{
			StreamID:       hostStreamID,
			HostCallbackID: hostCallbackID,
		})
		if errRead != nil {
			failure := classifyExecutionError(errRead)
			return committed, &failure
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if errDecode := json.Unmarshal(rawChunk, &chunk); errDecode != nil {
			return committed, &executionFailure{
				Code:      "bravo_host_stream_chunk_invalid",
				Message:   errDecode.Error(),
				Status:    http.StatusBadGateway,
				Retryable: !committed,
			}
		}
		if chunk.ErrorDetail != nil || strings.TrimSpace(chunk.Error) != "" {
			failure := streamChunkFailure(chunk)
			if committed {
				failure.Retryable = false
			}
			return committed, &failure
		}
		for _, payload := range rewriter.Push(chunk.Payload) {
			if len(payload) == 0 {
				continue
			}
			if errEmit := emitPluginStreamChunk(ctx, pluginStreamID, hostCallbackID, payload); errEmit != nil {
				failure := classifyExecutionError(errEmit)
				failure.Retryable = false
				return committed, &failure
			}
			if !committed {
				committed = true
				if onCommit != nil {
					if errCommit := onCommit(); errCommit != nil {
						failure := classifyExecutionError(errCommit)
						failure.Retryable = false
						return true, &failure
					}
				}
			}
		}
		if chunk.Done {
			if tail := rewriter.Flush(); len(tail) > 0 {
				if errEmit := emitPluginStreamChunk(ctx, pluginStreamID, hostCallbackID, tail); errEmit != nil {
					failure := classifyExecutionError(errEmit)
					failure.Retryable = false
					return committed, &failure
				}
				if !committed {
					committed = true
					if onCommit != nil {
						if errCommit := onCommit(); errCommit != nil {
							failure := classifyExecutionError(errCommit)
							failure.Retryable = false
							return true, &failure
						}
					}
				}
			}
			return committed, nil
		}
	}
}

func streamChunkFailure(chunk pluginapi.HostModelStreamReadResponse) executionFailure {
	if detail := chunk.ErrorDetail; detail != nil {
		failure := executionFailure{
			Code:       firstNonEmpty(detail.Code, "bravo_host_stream_error"),
			Message:    firstNonEmpty(detail.Message, chunk.Error),
			Status:     firstPositive(detail.HTTPStatus, http.StatusBadGateway),
			Retryable:  detail.Retryable || retryableHTTPStatus(detail.HTTPStatus),
			Headers:    cloneHeader(detail.Headers),
			RetryAfter: firstNonEmpty(detail.RetryAfter, detail.Headers.Get("Retry-After")),
		}
		return classifyProviderFailureSignal(failure, detail.Code, detail.Message, chunk.Error)
	}
	return classifyProviderFailureSignal(executionFailure{
		Code:      "bravo_host_stream_error",
		Message:   chunk.Error,
		Status:    http.StatusBadGateway,
		Retryable: true,
	}, chunk.Error)
}

func emitPluginStreamChunk(ctx context.Context, streamID, hostCallbackID string, payload []byte) error {
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID:       streamID,
		HostCallbackID: hostCallbackID,
		Payload:        payload,
	})
	return errCall
}

func closePluginStream(streamID, errorMessage string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errorMessage),
	})
}

// closePluginStreamFailure closes a stream that never produced bytes, carrying
// the HTTP semantics the client needs. Streaming is the dominant traffic shape,
// so without this a pool exhaustion reaches the SDK as a bare 500 and triggers
// an immediate retry into the same exhausted pool.
func closePluginStreamFailure(streamID string, failure executionFailure) {
	status := failure.Status
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}
	status = normalizedFailureStatus(status)
	retryAfter := strings.TrimSpace(failure.RetryAfter)
	if retryAfter == "" && status == http.StatusServiceUnavailable {
		retryAfter = strconv.Itoa(defaultRetryAfterSeconds(failure))
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID:    streamID,
		Error:       strings.TrimSpace(failure.Code + ": " + failure.Message),
		ErrorStatus: status,
		ErrorCode:   strings.TrimSpace(failure.Code),
		RetryAfter:  retryAfter,
	})
}

// normalizedFailureStatus reports pool-level exhaustion as 503 rather than 500.
// A 500 reads as "the server is broken, retry now"; 503 with Retry-After tells
// the client the pool is temporarily saturated and when to come back.
func normalizedFailureStatus(status int) int {
	if status == http.StatusInternalServerError {
		return http.StatusServiceUnavailable
	}
	return status
}

// defaultRetryAfterSeconds derives a backoff hint from the configured cooldown
// so clients wait roughly as long as the credential is actually parked.
func defaultRetryAfterSeconds(failure executionFailure) int {
	seconds := loadedConfig().CooldownSeconds
	if seconds <= 0 {
		seconds = 30
	}
	if seconds > 300 {
		seconds = 300
	}
	return seconds
}

func closeHostModelStream(streamID, hostCallbackID string) error {
	_, errCall := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{
		StreamID:       streamID,
		HostCallbackID: hostCallbackID,
	})
	return errCall
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
