package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
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
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	routeRecorder := newRouteTraceRecorder(req, strings.TrimSpace(req.Model), protocol, true)
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		failure := executionFailure{
			Code:    "bravo_stream_id_required",
			Message: "stream_id is required for executor.execute_stream",
			Status:  http.StatusBadRequest,
		}
		routeRecorder.preflightFailure("stream_preflight", failure, nil)
		return failureEnvelopeWithRouteTrace(routeRecorder, failure), nil
	}
	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		routeRecorder.preflightFailure("routing_preflight", *failure, nil)
		return failureEnvelopeWithRouteTrace(routeRecorder, *failure), nil
	}
	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	routeRecorder.setLogicalModel(logicalModelID)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		failure := contractFailure(errDetect)
		routeRecorder.preflightFailure("contract_detection", failure, errDetect)
		return failureEnvelopeWithRouteTrace(routeRecorder, failure), nil
	}
	if errPreflight := verifyLogicalModelContract(model, contract); errPreflight != nil {
		failure := contractFailure(errPreflight)
		routeRecorder.preflightFailure("logical_contract", failure, errPreflight)
		return failureEnvelopeWithRouteTrace(routeRecorder, failure), nil
	}
	go runBravoStreamWithTrace(req, streamID, routeRecorder)
	return okEnvelope(map[string]any{
		"headers": http.Header{
			"Content-Type":     []string{"text/event-stream"},
			"Cache-Control":    []string{"no-cache"},
			bravoTraceIDHeader: []string{routeRecorder.trace.TraceID},
		},
	})
}

func runBravoStream(req rpcExecutorRequest, pluginStreamID string) {
	runBravoStreamWithTrace(req, pluginStreamID, nil)
}

func runBravoStreamWithTrace(req rpcExecutorRequest, pluginStreamID string, initialRecorder *routeTraceRecorder) {
	launchedRuns := make([]*bravoStreamAttemptRun, 0, 2)
	routeRecorder := initialRecorder
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
			if routeRecorder != nil {
				routeRecorder.finish(false, failure.Status, failure)
			}
			closePluginStreamFailure(pluginStreamID, failure)
		}
	}()

	logicalName, model, cfg, failure := prepareBravoExecution(req)
	if failure != nil {
		if routeRecorder != nil {
			routeRecorder.finish(false, failure.Status, *failure)
		}
		closePluginStreamFailure(pluginStreamID, *failure)
		return
	}
	body := executionBody(req)
	protocol := requestProtocol(req.ExecutorRequest)
	contract, errDetect := detectRequestContract(protocol, body, true)
	if errDetect != nil {
		failure := contractFailure(errDetect)
		if routeRecorder != nil {
			routeRecorder.finish(false, failure.Status, failure)
		}
		closePluginStreamFailure(pluginStreamID, failure)
		return
	}
	candidateSourceBody, errStrip := stripRequestEffort(body, protocol, contract.Effort)
	if errStrip != nil {
		failure := executionFailure{
			Code:    "bravo_request_invalid",
			Message: errStrip.Error(),
			Status:  http.StatusBadRequest,
		}
		if routeRecorder != nil {
			routeRecorder.finish(false, failure.Status, failure)
		}
		closePluginStreamFailure(pluginStreamID, failure)
		return
	}
	hedgeDelay := time.Duration(cfg.FallbackHedgeDelaySeconds) * time.Second
	if project, authenticated := authenticatedExecutionProject(req, cfg); authenticated {
		if _, compact := claudeCLICompactBypassKey(req, project); compact {
			hedgeDelay = 0
		}
	}
	hedgeAt := time.Time{}
	logicalModelID := clientLogicalModelID(req.Model, cfg.Prefix+logicalName)
	if routeRecorder == nil {
		routeRecorder = newRouteTraceRecorder(req, logicalModelID, protocol, true)
	}
	plan, errPlan := buildExecutionPlan(req, logicalName, model, contract)
	if errPlan != nil {
		failure := executionFailure{
			Code:      "bravo_no_eligible_account",
			Message:   errPlan.Error(),
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		}
		routeRecorder.finish(false, failure.Status, failure)
		closePluginStreamFailure(pluginStreamID, failure)
		return
	}
	if len(plan) > 0 {
		routeRecorder.preflight(plan[0].PreflightRejections)
	}
	var lastFailure executionFailure
	failureTraces := initialExecutionFailureTraces(plan)
	contextRouting := newContextRoutingState(req.HostCallbackID)
	providerCalls := 0
	attempted := make(map[int]bool, len(plan))
	blockedModels := make(map[string]bool)
	hedgeUsed := false

	rememberFailure := func(run *bravoStreamAttemptRun, failure executionFailure) {
		lastFailure = failure
		if run == nil {
			return
		}
		contextRouting.observeFailure(run.attempt, failure)
		failureTraces = appendExecutionFailureTrace(failureTraces, run.attempt, failure)
		routeRecorder.failure(run.attempt, run.started, failure.Status, failure)
		if executionFailureBlocksPhysicalModel(failure) {
			blockedModels[executionFailureModelKey(run.attempt)] = true
		}
	}
	closeTerminalFailure := func(failure executionFailure) {
		final := finalExecutionFailureForRequest(req, failureTraces, failure)
		routeRecorder.finish(false, final.Status, final)
		closePluginStreamFailure(pluginStreamID, final)
	}
	canContinueStreamingRoute := func(failure executionFailure) bool {
		return executionFailureCanContinueRoute(failure)
	}

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
				blockedModels[executionFailureModelKey(plan[next])] ||
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
		if blockedModels[executionFailureModelKey(attempt)] {
			return nil, nil, false
		}
		if skipCoolingExecutionAttempt(attempt, &lastFailure) {
			return nil, nil, false
		}
		if errPreflight := verifyCandidateContract(attempt.Candidate, contract); errPreflight != nil {
			failure := contractFailure(errPreflight)
			lastFailure = failure
			routeRecorder.failure(attempt, time.Now(), failure.Status, failure)
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
			routeRecorder.failure(attempt, time.Now(), failure.Status, failure)
			return nil, &failure, false
		}
		if contextRouting.active() && !contextRouting.proveCandidate(
			req,
			attempt,
			protocol,
			physicalModel,
			candidateBody,
		) {
			routeRecorder.failure(attempt, time.Now(), http.StatusUnprocessableEntity, executionFailure{
				Code:    "bravo_context_target_incompatible",
				Message: "Целевая модель не прошла доказательную проверку вместимости контекста.",
				Status:  http.StatusUnprocessableEntity,
			})
			return nil, nil, false
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
				routeRecorder.failure(attempt, time.Now(), failure.Status, failure)
				return nil, &failure, false
			}
			callbackID = childID
			ownsScope = true
		}
		releaseLease, acquired, leaseFailure := acquireExecutionAttemptLease(attempt)
		if leaseFailure != nil {
			if ownsScope {
				_ = closeHostCallbackScope(callbackID)
			}
			failureTraces = appendExecutionFailureTrace(failureTraces, attempt, *leaseFailure)
			routeRecorder.failure(attempt, time.Now(), leaseFailure.Status, *leaseFailure)
			return nil, leaseFailure, false
		}
		if !acquired {
			if ownsScope {
				_ = closeHostCallbackScope(callbackID)
			}
			return nil, nil, false
		}
		attempt.AdaptiveProviderDispatched = true
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
			routeRecorder,
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
			if !canContinueStreamingRoute(*preflightFailure) {
				closeTerminalFailure(*preflightFailure)
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
					primary.attempt.AdaptiveProviderAccepted = true
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
						if winnerFailure == nil {
							routeRecorder.success(primary.attempt, primary.started, http.StatusOK)
							routeRecorder.finish(true, http.StatusOK, executionFailure{})
						} else {
							lastFailure = *winnerFailure
							contextRouting.observeFailure(primary.attempt, *winnerFailure)
							failureTraces = appendExecutionFailureTrace(failureTraces, primary.attempt, *winnerFailure)
							routeRecorder.failureWithCommit(primary.attempt, primary.started, winnerFailure.Status, *winnerFailure, true)
							routeRecorder.finish(false, winnerFailure.Status, *winnerFailure)
						}
						return
					}
					if winnerFailure != nil {
						rememberFailure(primary, *winnerFailure)
						if !canContinueStreamingRoute(*winnerFailure) {
							if hedgeResults != nil {
								settleBravoCompetingAttempt(hedge, hedgeResults, winnerFailure)
								hedgeResults = nil
							}
							closeTerminalFailure(*winnerFailure)
							return
						}
					}
					if hedgeResults == nil && deferredHedgeTerminal != nil {
						closeTerminalFailure(*deferredHedgeTerminal)
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
				rememberFailure(primary, *result.failure)
				if !canContinueStreamingRoute(*result.failure) {
					stopHedgeTimer()
					if hedgeResults != nil {
						settleBravoCompetingAttempt(hedge, hedgeResults, result.failure)
						hedgeResults = nil
					}
					closeTerminalFailure(*result.failure)
					return
				}
				if hedgeResults == nil && deferredHedgeTerminal != nil {
					closeTerminalFailure(*deferredHedgeTerminal)
					return
				}
			case result := <-hedgeResults:
				hedgeResults = nil
				if result.response != nil {
					hedge.attempt.AdaptiveProviderAccepted = true
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
						if winnerFailure == nil {
							routeRecorder.success(hedge.attempt, hedge.started, http.StatusOK)
							routeRecorder.finish(true, http.StatusOK, executionFailure{})
						} else {
							lastFailure = *winnerFailure
							contextRouting.observeFailure(hedge.attempt, *winnerFailure)
							failureTraces = appendExecutionFailureTrace(failureTraces, hedge.attempt, *winnerFailure)
							routeRecorder.failureWithCommit(hedge.attempt, hedge.started, winnerFailure.Status, *winnerFailure, true)
							routeRecorder.finish(false, winnerFailure.Status, *winnerFailure)
						}
						return
					}
					if winnerFailure != nil {
						rememberFailure(hedge, *winnerFailure)
						if !canContinueStreamingRoute(*winnerFailure) {
							if winnerFailure.Code == "request_canceled" {
								if primaryResults != nil {
									settleBravoCompetingAttempt(primary, primaryResults, winnerFailure)
									primaryResults = nil
								}
								closeTerminalFailure(*winnerFailure)
								return
							}
							if primaryResults != nil {
								failureCopy := *winnerFailure
								deferredHedgeTerminal = &failureCopy
							} else {
								closeTerminalFailure(*winnerFailure)
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
				rememberFailure(hedge, *result.failure)
				if !canContinueStreamingRoute(*result.failure) {
					if result.failure.Code == "request_canceled" {
						if primaryResults != nil {
							settleBravoCompetingAttempt(primary, primaryResults, result.failure)
							primaryResults = nil
						}
						closeTerminalFailure(*result.failure)
						return
					}
					if primaryResults != nil {
						failureCopy := *result.failure
						deferredHedgeTerminal = &failureCopy
					} else {
						closeTerminalFailure(*result.failure)
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
					if !canContinueStreamingRoute(*hedgeFailure) {
						if hedgeFailure.Code == "request_canceled" {
							if primaryResults != nil {
								settleBravoCompetingAttempt(primary, primaryResults, hedgeFailure)
								primaryResults = nil
							}
							closeTerminalFailure(*hedgeFailure)
							return
						}
						if primaryResults == nil {
							closeTerminalFailure(*hedgeFailure)
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
			closeTerminalFailure(*deferredHedgeTerminal)
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
	closeTerminalFailure(lastFailure)
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
		run.attempt.Candidate.Provider,
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
	run.attempt.AdaptiveProviderAccepted = true
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
		localized := clientExecutionFailureRU(*streamFailure)
		closePluginStream(pluginStreamID, localized.Code+": "+localized.Message)
		return true, streamFailure
	}
	return false, streamFailure
}

func forwardCandidateStream(
	ctx context.Context,
	hostStreamID, pluginStreamID, protocol, provider, physicalModel, logicalModel, hostCallbackID string,
	onCommit func() error,
) (bool, *executionFailure) {
	const maxBufferedPreludeBytes = 256 << 10

	rewriter := streamModelRewriter{
		physical: physicalModel,
		logical:  logicalModel,
		protocol: protocol,
	}
	committed := false
	claudeProviderStream := normalizeProvider(provider) == "claude"
	pendingPrelude := make([][]byte, 0, 2)
	pendingPreludeBytes := 0

	commitBeforeEmit := func() *executionFailure {
		if committed {
			return nil
		}
		// Once a substantive block is observed, fallback would splice two
		// provider streams even if the downstream emit itself fails.
		committed = true
		if onCommit == nil {
			return nil
		}
		if errCommit := onCommit(); errCommit != nil {
			failure := classifyExecutionError(errCommit)
			failure.Retryable = false
			return &failure
		}
		return nil
	}
	emitPayload := func(payload []byte) *executionFailure {
		if len(payload) == 0 {
			return nil
		}
		if errEmit := emitPluginStreamChunk(ctx, pluginStreamID, hostCallbackID, payload); errEmit != nil {
			failure := classifyExecutionError(errEmit)
			failure.Retryable = false
			return &failure
		}
		return nil
	}
	flushPendingPrelude := func(payload []byte) *executionFailure {
		if failure := commitBeforeEmit(); failure != nil {
			return failure
		}
		buffered := pendingPrelude
		pendingPrelude = nil
		pendingPreludeBytes = 0
		for _, prelude := range buffered {
			if failure := emitPayload(prelude); failure != nil {
				return failure
			}
		}
		return emitPayload(payload)
	}

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
			return committed, &failure
		}
		if claudeProviderStream && len(chunk.Payload) > 0 {
			// Claude may deliver a provider error as an ordinary SSE payload
			// instead of ErrorDetail. Intercept reviewed credits_required and
			// fail closed on other structured error envelopes so neither raw
			// request IDs nor payment details reach the client.
			if failure := claudeStreamPayloadFailure(chunk.Payload); failure != nil {
				return committed, failure
			}
		}
		for _, payload := range rewriter.Push(chunk.Payload) {
			if len(payload) == 0 {
				continue
			}
			if !committed {
				// Host streams are already translated to the client protocol.
				// Buffer its recognized non-substantive prelude regardless of
				// which physical provider produced it, so a terminal failure
				// cannot commit an otherwise invisible attempt.
				contentful := providerStreamPayloadContainsContent(protocol, payload)
				if !contentful && pendingPreludeBytes+len(payload) <= maxBufferedPreludeBytes {
					pendingPrelude = append(pendingPrelude, append([]byte(nil), payload...))
					pendingPreludeBytes += len(payload)
					continue
				}
				if len(pendingPrelude) > 0 {
					if failure := flushPendingPrelude(payload); failure != nil {
						return true, failure
					}
					continue
				}
			}
			if failure := emitPayload(payload); failure != nil {
				return committed, failure
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
			if !committed && len(pendingPrelude) > 0 {
				if failure := flushPendingPrelude(nil); failure != nil {
					return true, failure
				}
			}
			if tail := rewriter.Flush(); len(tail) > 0 {
				if failure := emitPayload(tail); failure != nil {
					return committed, failure
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

func claudeStreamPayloadFailure(payload []byte) *executionFailure {
	raw, ok := firstStreamJSONObject(payload)
	if !ok {
		return nil
	}
	if _, reviewed := creditsRequiredProviderDetail(raw); reviewed {
		failure := classifyProviderFailureSignal(executionFailure{
			Code:      "bravo_host_stream_error",
			Message:   "Provider model credits are exhausted.",
			Status:    http.StatusTooManyRequests,
			Retryable: true,
		}, raw)
		return &failure
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(raw), &envelope); errUnmarshal != nil ||
		!strings.EqualFold(strings.TrimSpace(envelope.Type), "error") {
		return nil
	}
	if classification, reviewed := providererror.ParseAnthropicStandard(raw); reviewed {
		failure := classifyProviderFailureDetail(executionFailure{
			Code:      classification.Detail.Code,
			Message:   classification.Detail.Message,
			Status:    classification.Status,
			Retryable: classification.Retryable,
		}, classification.Detail)
		return &failure
	}
	if providerContextWindowSignal(envelope.Error.Type, envelope.Error.Message) {
		failure := classifyProviderFailureSignal(executionFailure{
			Code:    firstNonEmpty(envelope.Error.Type, "invalid_request_error"),
			Message: "Input exceeds this model's context window.",
			Status:  http.StatusBadRequest,
		}, envelope.Error.Type, envelope.Error.Message)
		return &failure
	}
	return &executionFailure{
		Code:    "bravo_provider_stream_error",
		Message: "Provider returned an unrecognized structured error before completing the response.",
		Status:  http.StatusBadGateway,
	}
}

func providerStreamPayloadContainsContent(protocol string, payload []byte) bool {
	switch normalizeContractProtocol(protocol) {
	case protocolClaude:
		return claudeStreamPayloadContainsContent(payload)
	case protocolOpenAI:
		return !openAIChatStreamPrelude(payload)
	case protocolOpenAIResponse:
		return !openAIResponsesStreamPrelude(payload)
	default:
		// Unknown translated shapes are client-visible by definition. Commit
		// rather than widening the fallback window by guessing.
		return true
	}
}

func claudeStreamPayloadContainsContent(payload []byte) bool {
	raw, ok := firstStreamJSONObject(payload)
	if !ok {
		// Unknown bytes may already be meaningful to the client. Preserve the
		// original fail-closed boundary instead of treating them as prelude.
		return true
	}
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			Partial  string `json:"partial_json"`
		} `json:"delta"`
	}
	if errUnmarshal := json.Unmarshal([]byte(raw), &event); errUnmarshal != nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(event.Type)) {
	case "message_start", "ping", "message_delta", "message_stop":
		return false
	case "content_block_start":
		// An empty text block is protocol scaffolding. Tool, image and other
		// semantic block starts alter client state immediately and commit.
		return !strings.EqualFold(strings.TrimSpace(event.ContentBlock.Type), "text") ||
			event.ContentBlock.Text != ""
	case "content_block_delta":
		switch strings.ToLower(strings.TrimSpace(event.Delta.Type)) {
		case "text_delta":
			return event.Delta.Text != ""
		case "thinking_delta":
			return event.Delta.Thinking != ""
		case "input_json_delta":
			return event.Delta.Partial != ""
		default:
			return true
		}
	case "content_block_stop":
		return false
	case "error":
		return true
	default:
		return true
	}
}

func openAIChatStreamPrelude(payload []byte) bool {
	raw, ok := firstStreamJSONObject(payload)
	if !ok {
		return false
	}
	var chunk struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta        map[string]json.RawMessage `json:"delta"`
			FinishReason json.RawMessage            `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal([]byte(raw), &chunk); errUnmarshal != nil ||
		chunk.Object != "chat.completion.chunk" ||
		len(chunk.Choices) == 0 {
		return false
	}
	for _, choice := range chunk.Choices {
		if value := strings.TrimSpace(string(choice.FinishReason)); value != "" && value != "null" {
			return false
		}
		if len(choice.Delta) != 1 {
			return false
		}
		role, okRole := choice.Delta["role"]
		if !okRole {
			return false
		}
		var roleValue string
		if errRole := json.Unmarshal(role, &roleValue); errRole != nil || roleValue != "assistant" {
			return false
		}
	}
	return true
}

func openAIResponsesStreamPrelude(payload []byte) bool {
	raw, ok := firstStreamJSONObject(payload)
	if !ok {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal([]byte(raw), &event); errUnmarshal != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(event.Type)) {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func firstStreamJSONObject(payload []byte) (string, bool) {
	value := strings.TrimSpace(string(payload))
	objectStart := strings.IndexByte(value, '{')
	if objectStart < 0 {
		return "", false
	}
	decoder := json.NewDecoder(strings.NewReader(value[objectStart:]))
	var raw json.RawMessage
	if errDecode := decoder.Decode(&raw); errDecode != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
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
		if detail.ProviderError != nil {
			return classifyProviderFailureDetail(failure, *detail.ProviderError)
		}
		if terminalProviderStreamErrorCode(detail.Code) {
			failure.Code = "bravo_provider_stream_error"
			failure.Message = "Provider returned an unrecognized structured error before completing the response."
			failure.Retryable = false
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

func terminalProviderStreamErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "provider_stream_error", "bravo_provider_stream_error":
		return true
	default:
		return false
	}
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
	failure = clientExecutionFailureRU(failure)
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
