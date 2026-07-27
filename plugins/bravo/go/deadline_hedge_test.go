package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamHedgeProbe struct {
	mu sync.Mutex

	claudeStarted  chan struct{}
	claudeReturned chan struct{}
	codexStarted   chan struct{}
	codexReturned  chan struct{}
	claudeRelease  chan struct{}

	claudeStartOnce  sync.Once
	claudeReturnOnce sync.Once
	codexStartOnce   sync.Once
	codexReturnOnce  sync.Once
	claudeCloseOnce  sync.Once

	claudeSucceeds bool
	codexFailure   *hostCallError
	modelCalls     []hostModelExecutionRequest
	committedIDs   []string
	closedIDs      []string
	emitted        [][]byte
	readCounts     map[string]int
	pluginClose    rpcStreamCloseRequest
	nextChild      int
	claudeScopeID  string
}

func newStreamHedgeProbe(claudeSucceeds bool) *streamHedgeProbe {
	return &streamHedgeProbe{
		claudeStarted:  make(chan struct{}),
		claudeReturned: make(chan struct{}),
		codexStarted:   make(chan struct{}),
		codexReturned:  make(chan struct{}),
		claudeRelease:  make(chan struct{}),
		claudeSucceeds: claudeSucceeds,
		readCounts:     make(map[string]int),
	}
}

func (p *streamHedgeProbe) call(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		return streamHedgeTestJSON(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
			{ID: "claude-slow", Name: "claude-slow.json", Provider: "claude"},
			{ID: "claude-slow-secondary", Name: "claude-slow-secondary.json", Provider: "claude"},
			{ID: "codex-fast", Name: "codex-fast.json", Provider: "codex"},
		}}), nil
	case pluginabi.MethodHostCallbackFork:
		var request pluginapi.HostCallbackScopeRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		if strings.TrimSpace(request.HostCallbackID) == "" {
			return nil, fmt.Errorf("empty parent callback id")
		}
		p.mu.Lock()
		p.nextChild++
		childID := fmt.Sprintf("hedge-child-%d", p.nextChild)
		p.mu.Unlock()
		return streamHedgeTestJSON(pluginapi.HostCallbackScopeResponse{HostCallbackID: childID}), nil
	case pluginabi.MethodHostModelExecuteStream:
		var request hostModelExecutionRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		p.modelCalls = append(p.modelCalls, request)
		p.mu.Unlock()

		switch request.ForcedProvider {
		case "claude":
			p.mu.Lock()
			p.claudeScopeID = request.HostCallbackID
			p.mu.Unlock()
			p.claudeStartOnce.Do(func() { close(p.claudeStarted) })
			<-p.claudeRelease
			p.claudeReturnOnce.Do(func() { close(p.claudeReturned) })
			if p.claudeSucceeds {
				return streamHedgeTestJSON(pluginapi.HostModelStreamResponse{
					StatusCode: http.StatusOK,
					StreamID:   "claude-success-stream",
				}), nil
			}
			return nil, &hostCallError{
				Code:       "attempt_superseded",
				Message:    "the pre-commit Claude hedge lost",
				HTTPStatus: 499,
			}
		case "codex":
			p.codexStartOnce.Do(func() { close(p.codexStarted) })
			if p.codexFailure != nil {
				p.codexReturnOnce.Do(func() { close(p.codexReturned) })
				return nil, p.codexFailure
			}
			return streamHedgeTestJSON(pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "codex-success-stream",
			}), nil
		default:
			return nil, fmt.Errorf("unexpected provider %q", request.ForcedProvider)
		}
	case pluginabi.MethodHostCallbackClose:
		var request pluginapi.HostCallbackScopeRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		p.closedIDs = append(p.closedIDs, request.HostCallbackID)
		releaseClaude := request.HostCallbackID == p.claudeScopeID
		p.mu.Unlock()
		if releaseClaude {
			p.releaseClaude()
		}
		return streamHedgeTestJSON(map[string]any{}), nil
	case pluginabi.MethodHostCallbackCommit:
		var request pluginapi.HostCallbackScopeRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		p.committedIDs = append(p.committedIDs, request.HostCallbackID)
		p.mu.Unlock()
		return streamHedgeTestJSON(map[string]any{}), nil
	case pluginabi.MethodHostModelStreamRead:
		var request pluginapi.HostModelStreamReadRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		readCount := p.readCounts[request.StreamID]
		p.readCounts[request.StreamID] = readCount + 1
		p.mu.Unlock()
		if readCount > 0 {
			return streamHedgeTestJSON(pluginapi.HostModelStreamReadResponse{Done: true}), nil
		}
		model := "gpt-5.6-terra"
		content := "codex"
		if request.StreamID == "claude-success-stream" {
			model = "claude-sonnet-5"
			content = "claude"
		}
		return streamHedgeTestJSON(pluginapi.HostModelStreamReadResponse{
			Payload: []byte(fmt.Sprintf(
				`{"model":%q,"choices":[{"delta":{"content":%q}}]}`,
				model,
				content,
			)),
		}), nil
	case pluginabi.MethodHostStreamEmit:
		var request rpcStreamEmitRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		p.emitted = append(p.emitted, append([]byte(nil), request.Payload...))
		p.mu.Unlock()
		return streamHedgeTestJSON(map[string]any{}), nil
	case pluginabi.MethodHostModelStreamClose:
		return streamHedgeTestJSON(map[string]any{}), nil
	case pluginabi.MethodHostStreamClose:
		var request rpcStreamCloseRequest
		if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
			return nil, errDecode
		}
		p.mu.Lock()
		p.pluginClose = request
		p.mu.Unlock()
		return streamHedgeTestJSON(map[string]any{}), nil
	default:
		return nil, fmt.Errorf("unexpected host callback %q", method)
	}
}

func (p *streamHedgeProbe) releaseClaude() {
	p.claudeCloseOnce.Do(func() { close(p.claudeRelease) })
}

func (p *streamHedgeProbe) snapshot() ([]hostModelExecutionRequest, []string, [][]byte, rpcStreamCloseRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	emitted := make([][]byte, len(p.emitted))
	for index := range p.emitted {
		emitted[index] = append([]byte(nil), p.emitted[index]...)
	}
	return append([]hostModelExecutionRequest(nil), p.modelCalls...),
		append([]string(nil), p.closedIDs...),
		emitted,
		p.pluginClose
}

func (p *streamHedgeProbe) committedSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.committedIDs...)
}

func TestBravoStreamHedgesSlowClaudeBootstrapWithFastCodex(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)
	probe := newStreamHedgeProbe(false)
	installBravoHostCall(t, probe.call)

	request := streamHedgeExecutorRequest("slow-claude-fast-codex")
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(request, "hedged-client-stream")
	}()

	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "slow Claude stream bootstrap did not start")
	select {
	case <-probe.codexStarted:
		// The fast cross-provider hedge started while Claude was still blocked.
	case <-time.After(2500 * time.Millisecond):
		probe.releaseClaude()
		waitStreamHedgeSignal(t, done, time.Second, "Bravo stream did not stop after RED cleanup")
		t.Fatal("Codex did not start while the Claude stream bootstrap was still pending")
	}

	waitStreamHedgeSignal(t, probe.claudeReturned, time.Second, "losing Claude child callback was not closed")
	waitStreamHedgeSignal(t, done, time.Second, "hedged Bravo stream did not finish")

	modelCalls, closedIDs, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 2 {
		t.Fatalf("model calls = %#v, want one Claude bootstrap and one Codex bootstrap", modelCalls)
	}
	if modelCalls[0].ForcedProvider != "claude" || modelCalls[1].ForcedProvider != "codex" {
		t.Fatalf("provider order = %q, %q, want Claude then cross-provider Codex without the second Claude credential", modelCalls[0].ForcedProvider, modelCalls[1].ForcedProvider)
	}
	primaryScopeID := strings.TrimSpace(modelCalls[0].HostCallbackID)
	hedgeScopeID := strings.TrimSpace(modelCalls[1].HostCallbackID)
	if primaryScopeID == "" || hedgeScopeID == "" || primaryScopeID == hedgeScopeID {
		t.Fatalf("callback scopes = %q, %q, want distinct child scopes", primaryScopeID, hedgeScopeID)
	}
	if !streamHedgeTestContains(closedIDs, primaryScopeID) {
		t.Fatalf("closed callback scopes = %v, want losing Claude scope %q", closedIDs, primaryScopeID)
	}
	if !streamHedgeTestContains(closedIDs, hedgeScopeID) {
		t.Fatalf("closed callback scopes = %v, want completed Codex scope %q", closedIDs, hedgeScopeID)
	}
	committedIDs := probe.committedSnapshot()
	if streamHedgeTestContains(committedIDs, primaryScopeID) ||
		!streamHedgeTestContains(committedIDs, hedgeScopeID) {
		t.Fatalf("committed callback scopes = %v, want only winning Codex scope %q", committedIDs, hedgeScopeID)
	}
	if len(emitted) == 0 {
		t.Fatal("hedged stream emitted no Codex payload")
	}
	for _, payload := range emitted {
		text := string(payload)
		if strings.Contains(text, "claude") || strings.Contains(text, "claude-sonnet-5") {
			t.Fatalf("losing Claude bytes reached the client: %s", payload)
		}
	}
	if !strings.Contains(string(emitted[0]), `"model":"bravo/fallback-probe"`) ||
		!strings.Contains(string(emitted[0]), `"content":"codex"`) {
		t.Fatalf("emitted payload = %s, want logical model served by Codex", emitted[0])
	}
	if pluginClose.StreamID != "hedged-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin close = %#v, want successful hedged stream close", pluginClose)
	}
	runtimeState.RLock()
	cooldownCount := len(runtimeState.Cooldowns)
	runtimeState.RUnlock()
	if cooldownCount != 0 {
		t.Fatalf("superseded Claude hedge created %d Bravo cooldown entries", cooldownCount)
	}
}

func TestBravoStreamPrimarySuccessBeforeDelayDoesNotStartHedge(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)
	probe := newStreamHedgeProbe(true)
	installBravoHostCall(t, probe.call)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("fast-primary"), "fast-primary-client-stream")
	}()

	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "primary Claude stream bootstrap did not start")
	probe.releaseClaude()
	waitStreamHedgeSignal(t, done, time.Second, "fast primary Bravo stream did not finish")

	modelCalls, closedIDs, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 1 || modelCalls[0].ForcedProvider != "claude" {
		t.Fatalf("model calls = %#v, want only the fast primary Claude stream", modelCalls)
	}
	select {
	case <-probe.codexStarted:
		t.Fatal("Codex hedge started after the primary had already succeeded")
	default:
	}
	primaryScopeID := strings.TrimSpace(modelCalls[0].HostCallbackID)
	if primaryScopeID == "" || !streamHedgeTestContains(closedIDs, primaryScopeID) {
		t.Fatalf("closed callback scopes = %v, want completed primary scope %q", closedIDs, primaryScopeID)
	}
	if len(emitted) == 0 || !strings.Contains(string(emitted[0]), `"content":"claude"`) {
		t.Fatalf("emitted payloads = %q, want the primary Claude stream", emitted)
	}
	if pluginClose.StreamID != "fast-primary-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin close = %#v, want successful primary stream close", pluginClose)
	}
}

func TestBravoStreamTerminalHedgeDoesNotKillPendingPrimary(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)
	probe := newStreamHedgeProbe(true)
	probe.codexFailure = &hostCallError{
		Code:       "invalid_request",
		Message:    "Codex rejected only its mapped candidate",
		HTTPStatus: http.StatusBadRequest,
	}
	installBravoHostCall(t, probe.call)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("terminal-hedge"), "terminal-hedge-client-stream")
	}()

	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "primary Claude stream bootstrap did not start")
	waitStreamHedgeSignal(t, probe.codexStarted, 2500*time.Millisecond, "Codex hedge did not start")
	waitStreamHedgeSignal(t, probe.codexReturned, time.Second, "Codex hedge did not return its terminal failure")
	waitBravoAttemptCode(t, "invalid_request", time.Second)
	probe.releaseClaude()
	waitStreamHedgeSignal(t, done, time.Second, "primary did not recover after terminal hedge failure")

	modelCalls, _, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 2 || modelCalls[0].ForcedProvider != "claude" || modelCalls[1].ForcedProvider != "codex" {
		t.Fatalf("model calls = %#v, want Claude primary and Codex hedge", modelCalls)
	}
	if len(emitted) == 0 || !strings.Contains(string(emitted[0]), `"content":"claude"`) {
		t.Fatalf("emitted payloads = %q, want the still-healthy Claude primary", emitted)
	}
	if pluginClose.StreamID != "terminal-hedge-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin close = %#v, want successful primary close", pluginClose)
	}
}

func TestBravoStreamMaxAttemptsOneDoesNotHedge(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 1, 1)
	probe := newStreamHedgeProbe(true)
	installBravoHostCall(t, probe.call)

	request := streamHedgeExecutorRequest("max-attempts-one")
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(request, "single-attempt-client-stream")
	}()

	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "Claude stream bootstrap did not start")
	select {
	case <-probe.codexStarted:
		probe.releaseClaude()
		waitStreamHedgeSignal(t, done, time.Second, "Bravo stream did not stop after invalid hedge")
		t.Fatal("Codex hedge started even though max_attempts is 1")
	case <-time.After(1250 * time.Millisecond):
		probe.releaseClaude()
	}
	waitStreamHedgeSignal(t, probe.claudeReturned, time.Second, "Claude stream bootstrap did not return")
	waitStreamHedgeSignal(t, done, time.Second, "single-attempt Bravo stream did not finish")

	modelCalls, closedIDs, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 1 || modelCalls[0].ForcedProvider != "claude" {
		t.Fatalf("model calls = %#v, want only the primary Claude stream", modelCalls)
	}
	if len(closedIDs) != 0 {
		t.Fatalf("closed callback scopes = %v, want none without a hedge", closedIDs)
	}
	if len(emitted) == 0 || !strings.Contains(string(emitted[0]), `"content":"claude"`) {
		t.Fatalf("emitted payloads = %q, want the primary Claude stream", emitted)
	}
	if pluginClose.StreamID != "single-attempt-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin close = %#v, want successful primary stream close", pluginClose)
	}
}

func TestBravoStreamZeroHedgeDelayDisablesHedge(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 0)
	probe := newStreamHedgeProbe(true)
	installBravoHostCall(t, probe.call)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("zero-delay-disabled"), "zero-delay-client-stream")
	}()

	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "primary Claude stream bootstrap did not start")
	select {
	case <-probe.codexStarted:
		probe.releaseClaude()
		waitStreamHedgeSignal(t, done, time.Second, "Bravo stream did not stop after an invalid zero-delay hedge")
		t.Fatal("Codex hedge started even though fallback_hedge_delay_seconds is 0")
	case <-time.After(250 * time.Millisecond):
		probe.releaseClaude()
	}
	waitStreamHedgeSignal(t, done, time.Second, "zero-delay-disabled primary stream did not finish")

	modelCalls, closedIDs, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 1 || modelCalls[0].ForcedProvider != "claude" {
		t.Fatalf("model calls = %#v, want only the sequential Claude primary", modelCalls)
	}
	if len(closedIDs) != 0 {
		t.Fatalf("closed child scopes = %v, want none when hedging is disabled", closedIDs)
	}
	if len(emitted) == 0 || !strings.Contains(string(emitted[0]), `"content":"claude"`) {
		t.Fatalf("emitted payloads = %q, want the primary Claude stream", emitted)
	}
	if pluginClose.StreamID != "zero-delay-client-stream" || pluginClose.Error != "" {
		t.Fatalf("plugin close = %#v, want successful sequential close", pluginClose)
	}
}

func TestBravoStreamRootRequestCanceledIsTerminalWithoutCooldown(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 0)

	var mu sync.Mutex
	var providers []string
	var pluginClose rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return streamHedgeTestJSON(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "claude-canceled", Name: "claude-canceled.json", Provider: "claude"},
				{ID: "codex-must-not-run", Name: "codex-must-not-run.json", Provider: "codex"},
			}}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			mu.Lock()
			providers = append(providers, request.ForcedProvider)
			mu.Unlock()
			return nil, &hostCallError{
				Code:       "request_canceled",
				Message:    "root client request was canceled",
				HTTPStatus: 499,
			}
		case pluginabi.MethodHostStreamClose:
			if errDecode := decodeStreamHedgeTestPayload(payload, &pluginClose); errDecode != nil {
				return nil, errDecode
			}
			return streamHedgeTestJSON(map[string]any{}), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %q", method)
		}
	})

	runBravoStream(streamHedgeExecutorRequest("root-request-canceled"), "canceled-client-stream")

	mu.Lock()
	gotProviders := append([]string(nil), providers...)
	mu.Unlock()
	if len(gotProviders) != 1 || gotProviders[0] != "claude" {
		t.Fatalf("providers = %v, want terminal cancellation after Claude only", gotProviders)
	}
	if pluginClose.StreamID != "canceled-client-stream" ||
		pluginClose.ErrorCode != "request_canceled" ||
		pluginClose.ErrorStatus != 499 {
		t.Fatalf("plugin close = %#v, want request_canceled/499", pluginClose)
	}
	runtimeState.RLock()
	cooldownCount := len(runtimeState.Cooldowns)
	runtimeState.RUnlock()
	if cooldownCount != 0 {
		t.Fatalf("root request cancellation created %d Bravo cooldown entries", cooldownCount)
	}
}

func TestBravoStreamRootCancellationAtFirstEmitIsLocalAndTerminal(t *testing.T) {
	runBravoFirstEmitFailureTest(t, &hostCallError{
		Code:       "request_canceled",
		Message:    "root client request was canceled before first emit",
		HTTPStatus: 499,
	}, "request_canceled", 499)
}

func TestBravoStreamLocalBridgeFailureDoesNotCoolOrRetryProvider(t *testing.T) {
	runBravoFirstEmitFailureTest(t, &hostCallError{
		Code:       "host_call_failed",
		Message:    "stream is not open",
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
	}, "host_call_failed", http.StatusBadGateway)
}

func runBravoFirstEmitFailureTest(
	t *testing.T,
	emitFailure *hostCallError,
	wantCode string,
	wantStatus int,
) {
	t.Helper()
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)

	const childID = "first-emit-child"
	var mu sync.Mutex
	var providers []string
	var closedIDs []string
	var committedIDs []string
	var emitCallbackID string
	var pluginClose rpcStreamCloseRequest
	readCount := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return streamHedgeTestJSON(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "claude-first-emit", Name: "claude-first-emit.json", Provider: "claude"},
				{ID: "codex-must-not-run", Name: "codex-must-not-run.json", Provider: "codex"},
			}}), nil
		case pluginabi.MethodHostCallbackFork:
			return streamHedgeTestJSON(pluginapi.HostCallbackScopeResponse{HostCallbackID: childID}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			mu.Lock()
			providers = append(providers, request.ForcedProvider)
			mu.Unlock()
			return streamHedgeTestJSON(pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   "first-emit-host-stream",
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			readCount++
			if readCount > 1 {
				return streamHedgeTestJSON(pluginapi.HostModelStreamReadResponse{Done: true}), nil
			}
			return streamHedgeTestJSON(pluginapi.HostModelStreamReadResponse{
				Payload: []byte(`{"model":"claude-sonnet-5","choices":[{"delta":{"content":"never-visible"}}]}`),
			}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			emitCallbackID = request.HostCallbackID
			return nil, emitFailure
		case pluginabi.MethodHostCallbackCommit:
			var request pluginapi.HostCallbackScopeRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			committedIDs = append(committedIDs, request.HostCallbackID)
			return streamHedgeTestJSON(map[string]any{}), nil
		case pluginabi.MethodHostCallbackClose:
			var request pluginapi.HostCallbackScopeRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			closedIDs = append(closedIDs, request.HostCallbackID)
			return streamHedgeTestJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamHedgeTestJSON(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			if errDecode := decodeStreamHedgeTestPayload(payload, &pluginClose); errDecode != nil {
				return nil, errDecode
			}
			return streamHedgeTestJSON(map[string]any{}), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %q", method)
		}
	})

	runBravoStream(streamHedgeExecutorRequest("first-emit-failure"), "first-emit-client-stream")

	mu.Lock()
	gotProviders := append([]string(nil), providers...)
	mu.Unlock()
	if len(gotProviders) != 1 || gotProviders[0] != "claude" {
		t.Fatalf("providers = %v, want one terminal Claude attempt", gotProviders)
	}
	if emitCallbackID != childID {
		t.Fatalf("emit callback id = %q, want %q", emitCallbackID, childID)
	}
	if !streamHedgeTestContains(closedIDs, childID) {
		t.Fatalf("closed callback ids = %v, want %q", closedIDs, childID)
	}
	if len(committedIDs) != 0 {
		t.Fatalf("committed callback ids = %v, want none", committedIDs)
	}
	if pluginClose.StreamID != "first-emit-client-stream" ||
		pluginClose.ErrorCode != wantCode ||
		pluginClose.ErrorStatus != wantStatus {
		t.Fatalf("plugin close = %#v, want %s/%d", pluginClose, wantCode, wantStatus)
	}
	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	cooldownCount := len(runtimeState.Cooldowns)
	runtimeState.RUnlock()
	if len(attempts) != 1 ||
		attempts[0].ErrorCode != wantCode ||
		attempts[0].Status != wantStatus ||
		attempts[0].Success ||
		attempts[0].Retryable {
		t.Fatalf("attempts = %#v, want one terminal local %s/%d", attempts, wantCode, wantStatus)
	}
	if cooldownCount != 0 {
		t.Fatalf("local first-emit failure created %d cooldown entries", cooldownCount)
	}
}

func TestBravoStreamRootCancellationLabelsBothActiveAttempts(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)

	var mu sync.Mutex
	nextChild := 0
	providersStarted := make(chan string, 2)
	cancelAll := make(chan struct{})
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return streamHedgeTestJSON(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "claude-canceled", Name: "claude-canceled.json", Provider: "claude"},
				{ID: "codex-canceled", Name: "codex-canceled.json", Provider: "codex"},
			}}), nil
		case pluginabi.MethodHostCallbackFork:
			mu.Lock()
			nextChild++
			childID := fmt.Sprintf("root-cancel-child-%d", nextChild)
			mu.Unlock()
			return streamHedgeTestJSON(pluginapi.HostCallbackScopeResponse{HostCallbackID: childID}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			providersStarted <- request.ForcedProvider
			<-cancelAll
			return nil, &hostCallError{
				Code:       "request_canceled",
				Message:    "root client request was canceled",
				HTTPStatus: 499,
			}
		case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostStreamClose:
			return streamHedgeTestJSON(map[string]any{}), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %q", method)
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("both-active-root-cancel"), "both-active-canceled-stream")
	}()

	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case provider := <-providersStarted:
			started[provider] = true
		case <-time.After(2500 * time.Millisecond):
			t.Fatalf("active providers before root cancellation = %v, want Claude and Codex", started)
		}
	}
	close(cancelAll)
	waitStreamHedgeSignal(t, done, time.Second, "Bravo stream did not finish after root cancellation")

	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	cooldownCount := len(runtimeState.Cooldowns)
	runtimeState.RUnlock()
	if len(attempts) != 2 {
		t.Fatalf("attempt records = %#v, want exactly two active canceled attempts", attempts)
	}
	for _, attempt := range attempts {
		if attempt.ErrorCode != "request_canceled" || attempt.Status != 499 || attempt.Success || attempt.Retryable {
			t.Fatalf("canceled attempt record = %#v, want request_canceled/499/nonretryable", attempt)
		}
	}
	if cooldownCount != 0 {
		t.Fatalf("root cancellation created %d Bravo cooldown entries", cooldownCount)
	}
}

func TestBravoStreamHedgeCancellationStopsNoncooperativePrimary(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)

	var mu sync.Mutex
	nextChild := 0
	claudeScopeID := ""
	claudeStarted := make(chan struct{})
	codexStarted := make(chan struct{})
	claudeRelease := make(chan struct{})
	var claudeStartOnce sync.Once
	var codexStartOnce sync.Once
	var claudeReleaseOnce sync.Once
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return streamHedgeTestJSON(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "claude-stuck", Name: "claude-stuck.json", Provider: "claude"},
				{ID: "codex-root-canceled", Name: "codex-root-canceled.json", Provider: "codex"},
			}}), nil
		case pluginabi.MethodHostCallbackFork:
			mu.Lock()
			nextChild++
			childID := fmt.Sprintf("asymmetric-cancel-child-%d", nextChild)
			mu.Unlock()
			return streamHedgeTestJSON(pluginapi.HostCallbackScopeResponse{HostCallbackID: childID}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			if request.ForcedProvider == "claude" {
				mu.Lock()
				claudeScopeID = request.HostCallbackID
				mu.Unlock()
				claudeStartOnce.Do(func() { close(claudeStarted) })
				<-claudeRelease
			} else {
				codexStartOnce.Do(func() { close(codexStarted) })
			}
			return nil, &hostCallError{
				Code:       "request_canceled",
				Message:    "root client request was canceled",
				HTTPStatus: 499,
			}
		case pluginabi.MethodHostCallbackClose:
			var request pluginapi.HostCallbackScopeRequest
			if errDecode := decodeStreamHedgeTestPayload(payload, &request); errDecode != nil {
				return nil, errDecode
			}
			mu.Lock()
			releaseClaude := request.HostCallbackID == claudeScopeID
			mu.Unlock()
			if releaseClaude {
				claudeReleaseOnce.Do(func() { close(claudeRelease) })
			}
			return streamHedgeTestJSON(map[string]any{}), nil
		case pluginabi.MethodHostCallbackCommit:
			return streamHedgeTestJSON(map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			return streamHedgeTestJSON(map[string]any{}), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %q", method)
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("asymmetric-root-cancel"), "asymmetric-canceled-stream")
	}()
	waitStreamHedgeSignal(t, claudeStarted, time.Second, "noncooperative Claude primary did not start")
	waitStreamHedgeSignal(t, codexStarted, 2500*time.Millisecond, "canceling Codex hedge did not start")
	waitStreamHedgeSignal(t, done, time.Second, "hedge request cancellation waited for the stuck primary")

	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(attempts) != 2 {
		t.Fatalf("attempt records = %#v, want both active attempts canceled", attempts)
	}
	for _, attempt := range attempts {
		if attempt.ErrorCode != "request_canceled" || attempt.Status != 499 {
			t.Fatalf("attempt record = %#v, want request_canceled/499", attempt)
		}
	}
}

func TestBravoSupersededHedgeReleasesInFlightAndKeepsConservativePendingReservation(t *testing.T) {
	isolateBravoFallbackTestState(t)
	const authIndex = "hedge-reservation-auth"
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	allocatorRuntime.Lock()
	delete(allocatorRuntime.InFlightPercent, authIndex)
	delete(allocatorRuntime.PendingPercent, authIndex)
	allocatorRuntime.Unlock()
	t.Cleanup(func() {
		allocatorRuntime.Lock()
		delete(allocatorRuntime.InFlightPercent, authIndex)
		delete(allocatorRuntime.PendingPercent, authIndex)
		allocatorRuntime.Unlock()
	})

	attempt := executionAttempt{
		LogicalModel: "fallback-probe",
		Candidate: candidate{
			Provider: "claude",
			Model:    "claude-sonnet-5",
		},
		Auth: pluginapi.HostAuthFileEntry{
			ID:        "hedge-reservation-auth-id",
			AuthIndex: authIndex,
			Provider:  "claude",
		},
		Primary:            true,
		AllocatorManaged:   true,
		ReservationPercent: 0.25,
		TariffID:           "x1",
	}
	release, acquired := acquireAttemptLease(attempt)
	if !acquired {
		t.Fatal("primary hedge reservation was not acquired")
	}
	run := &bravoStreamAttemptRun{
		attempt:      attempt,
		started:      time.Now(),
		releaseLease: release,
	}
	run.supersede()
	run.supersede()

	allocatorRuntime.Lock()
	inFlight := allocatorRuntime.InFlightPercent[authIndex]
	pending := allocatorRuntime.PendingPercent[authIndex]
	allocatorRuntime.Unlock()
	if inFlight != 0 || pending != 0.25 {
		t.Fatalf("allocator reservations after supersede: in_flight=%v pending=%v, want 0/0.25", inFlight, pending)
	}
	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	cooldownCount := len(runtimeState.Cooldowns)
	runtimeState.RUnlock()
	if len(attempts) != 1 ||
		attempts[0].ErrorCode != "bravo_attempt_superseded" ||
		attempts[0].Status != 499 ||
		attempts[0].Success {
		t.Fatalf("superseded attempt records = %#v, want exactly one neutral 499", attempts)
	}
	if cooldownCount != 0 {
		t.Fatalf("superseded attempt created %d cooldown entries", cooldownCount)
	}

	clearPendingReservation(authIndex, 0.25)
	if got := pendingReservationPercent(authIndex); got != 0 {
		t.Fatalf("pending reservation after confirmed refresh cleanup = %v, want 0", got)
	}
}

func TestBravoStreamCoordinatorPanicClosesLaunchedChildAttempt(t *testing.T) {
	isolateBravoFallbackTestState(t)
	installBravoStreamHedgeTestConfig(t, 2, 1)
	probe := newStreamHedgeProbe(true)
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostStreamEmit {
			panic("forced stream emit panic")
		}
		return probe.call(method, payload)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBravoStream(streamHedgeExecutorRequest("coordinator-panic"), "coordinator-panic-stream")
	}()
	waitStreamHedgeSignal(t, probe.claudeStarted, time.Second, "panic-test primary did not start")
	probe.releaseClaude()
	waitStreamHedgeSignal(t, done, time.Second, "stream coordinator did not recover from panic")

	modelCalls, closedIDs, emitted, pluginClose := probe.snapshot()
	if len(modelCalls) != 1 {
		t.Fatalf("model calls = %#v, want one launched primary", modelCalls)
	}
	primaryScopeID := strings.TrimSpace(modelCalls[0].HostCallbackID)
	if primaryScopeID == "" || !streamHedgeTestContains(closedIDs, primaryScopeID) {
		t.Fatalf("closed callback scopes = %v, want panicked primary scope %q", closedIDs, primaryScopeID)
	}
	if len(emitted) != 0 {
		t.Fatalf("panic-test emitted payloads = %q, want none", emitted)
	}
	if pluginClose.StreamID != "coordinator-panic-stream" ||
		!strings.Contains(pluginClose.Error, "bravo_stream_panic") {
		t.Fatalf("plugin close = %#v, want a recovered coordinator panic", pluginClose)
	}
	runtimeState.RLock()
	attempts := append([]attemptRecord(nil), runtimeState.Attempts...)
	runtimeState.RUnlock()
	if len(attempts) != 1 || attempts[0].ErrorCode != "bravo_stream_panic" || attempts[0].Success {
		t.Fatalf("panic attempt records = %#v, want one failed cleanup record", attempts)
	}
}

func installBravoStreamHedgeTestConfig(t *testing.T, maxAttempts, hedgeDelaySeconds int) {
	t.Helper()
	previous := loadedConfig()
	cfg := pluginConfig{
		Enabled:                   true,
		Prefix:                    defaultPrefix,
		RequireSmartKey:           false,
		MaxAttempts:               maxAttempts,
		CooldownSeconds:           30,
		FallbackHedgeDelaySeconds: hedgeDelaySeconds,
		Models: map[string]logicalModel{
			"fallback-probe": {
				Candidates: []candidate{
					{
						Provider:     "claude",
						Model:        "claude-sonnet-5",
						Priority:     100,
						Capabilities: []string{capabilityText, capabilityStream},
					},
					{
						Provider:     "codex",
						Model:        "gpt-5.6-terra",
						Priority:     90,
						Capabilities: []string{capabilityText, capabilityStream},
					},
				},
			},
		},
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previous)
	})
}

func streamHedgeExecutorRequest(requestID string) rpcExecutorRequest {
	return rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          protocolOpenAI,
			SourceFormat:    protocolOpenAI,
			Stream:          true,
			OriginalRequest: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"classify"}],"stream":true}`),
			Metadata:        map[string]any{"request_id": requestID},
		},
		HostCallbackID: requestID + "-callback",
	}
}

func waitStreamHedgeSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatal(failure)
	}
}

func waitBravoAttemptCode(t *testing.T, code string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		runtimeState.RLock()
		found := false
		for _, attempt := range runtimeState.Attempts {
			if attempt.ErrorCode == code {
				found = true
				break
			}
		}
		runtimeState.RUnlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt code %q was not recorded", code)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func streamHedgeTestContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func decodeStreamHedgeTestPayload(payload any, target any) error {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return errMarshal
	}
	return json.Unmarshal(raw, target)
}

func streamHedgeTestJSON(value any) json.RawMessage {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}
