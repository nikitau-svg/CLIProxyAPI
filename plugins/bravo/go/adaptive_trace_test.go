package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveRouteTraceRecordsAdmissionRejectionAndFallback(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{
		ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50,
		Multiplier: 1, ReservationPercent: 1,
	}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })

	green := adaptiveTraceAttempt("trace-green-auth", "claude", "claude-fable-5", 1)
	installAdaptiveTestQuota(t, green.Auth.AuthIndex, 80, 80)
	releaseGreen, greenAcquired, green := acquireAttemptLeaseDetailed(green)
	if !greenAcquired {
		t.Fatal("green adaptive trace fixture was not admitted")
	}
	releaseGreen(true)
	recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.success(green, time.Now().Add(-time.Millisecond), http.StatusOK)
	greenTrace := recorder.trace.Attempts[0]
	if greenTrace.AdaptiveDecision != "adaptive_green_admitted" || greenTrace.ProjectRole != "secondary" ||
		greenTrace.AllocatorMode != "enforce" || greenTrace.ReservationPercent != 1 ||
		greenTrace.SessionHeadroomBefore <= greenTrace.SessionHeadroomAfter {
		t.Fatalf("green adaptive trace = %#v", greenTrace)
	}

	blocked := adaptiveTraceAttempt("trace-blocked-auth", "claude", "claude-fable-5", 1)
	installAdaptiveTestQuota(t, blocked.Auth.AuthIndex, 50.5, 50.5)
	_, acquired, failure, blocked := acquireExecutionAttemptLeaseDetailed(blocked)
	if acquired || failure == nil {
		t.Fatalf("protected secondary acquired=%t failure=%#v", acquired, failure)
	}
	recorder = &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.failure(blocked, time.Now(), failure.Status, *failure)

	fallback := adaptiveTraceAttempt("trace-fallback-auth", "codex", "gpt-5.6-sol", 1)
	fallback.Primary = true
	installAdaptiveTestQuota(t, fallback.Auth.AuthIndex, 80, 80)
	releaseFallback, fallbackAcquired, fallback := acquireAttemptLeaseDetailed(fallback)
	if !fallbackAcquired {
		t.Fatal("fallback adaptive trace fixture was not admitted")
	}
	releaseFallback(true)
	recorder.success(fallback, time.Now().Add(-time.Millisecond), http.StatusOK)

	blockedTrace := recorder.trace.Attempts[0]
	if blockedTrace.AdaptiveRejection != "adaptive_secondary_floor_protected" ||
		blockedTrace.AdaptiveFallback != "adaptive_failover_selected" ||
		blockedTrace.FallbackProvider != "codex" || blockedTrace.FallbackModel != "gpt-5.6-sol" {
		t.Fatalf("rejection/fallback adaptive trace = %#v", blockedTrace)
	}
	if recorder.trace.Attempts[1].AdaptiveDecision == "" {
		t.Fatalf("fallback admission has no adaptive decision: %#v", recorder.trace.Attempts[1])
	}
}

func TestAdaptiveRouteTraceClassifiesGlobalLedgerSaturation(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 10, WeeklyFloorPercent: 10, ReservationPercent: 1}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := adaptiveTraceAttempt("ledger-saturated-trace-auth", "claude", "claude-fable-5", 1)
	installAdaptiveTestQuota(t, attempt.Auth.AuthIndex, 100, 100)
	adaptiveRoutingSaturated.Store(true)
	_, acquired, failure, effective := acquireExecutionAttemptLeaseDetailed(attempt)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_ledger_saturated" {
		t.Fatalf("ledger saturation admission = acquired %t failure %#v", acquired, failure)
	}
	if effective.AdaptiveTrace.rejectionCause != adaptiveRejectionLedgerSaturated {
		t.Fatalf("ledger rejection cause = %q", effective.AdaptiveTrace.rejectionCause)
	}
	recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.failure(effective, time.Now(), failure.Status, *failure)
	if got := recorder.trace.Attempts[0].AdmissionRejectionCause; got != "ledger_saturated" {
		t.Fatalf("persisted ledger rejection cause = %q", got)
	}
}

func TestAdaptiveTerminalFailurePreservesTypedCauseAndPriority(t *testing.T) {
	failure := func(code string) executionFailure {
		return executionFailure{Code: code, Message: "unsafe raw provider text", Status: 503, Retryable: true, RouteFallback: true}
	}
	tests := []struct {
		name   string
		codes  []string
		want   string
		action string
	}{
		{name: "all estimator", codes: []string{"bravo_adaptive_estimator_saturated", "bravo_adaptive_estimator_saturated"}, want: "bravo_adaptive_estimator_saturated", action: "reconcile_limits"},
		{name: "durability wins", codes: []string{"bravo_adaptive_demand_saturated", "bravo_adaptive_durability_unavailable", "bravo_allocator_reserve_floor"}, want: "bravo_adaptive_durability_unavailable", action: "check_storage"},
		{name: "context wins", codes: []string{"bravo_adaptive_durability_unavailable", "bravo_context_window_exceeded"}, want: "bravo_context_window_exceeded", action: "compact"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			traces := make([]executionFailureTrace, 0, len(testCase.codes))
			attempts := make([]routeTraceAttempt, 0, len(testCase.codes))
			for _, code := range testCase.codes {
				item := failure(code)
				if code == "bravo_context_window_exceeded" {
					item.Status = http.StatusBadRequest
				}
				traces = append(traces, executionFailureTrace{Provider: "claude", Model: "claude-fable-5", Failure: item})
				attempts = append(attempts, routeTraceAttempt{Provider: "claude", Model: "claude-fable-5", ErrorCode: code})
			}
			got := finalExecutionFailure(traces, failure("bravo_route_temporarily_unavailable"))
			if got.Code != testCase.want {
				t.Fatalf("terminal code = %q, want %q", got.Code, testCase.want)
			}
			trace := sanitizeRouteTrace(routeTrace{FinalCode: "bravo_route_temporarily_unavailable", Attempts: attempts})
			if trace.FinalCode != testCase.want || trace.ClientAction != testCase.action {
				t.Fatalf("typed terminal trace = code %q action %q", trace.FinalCode, trace.ClientAction)
			}
			if strings.Contains(trace.FinalMessage, "unsafe") || strings.Contains(got.Message, "unsafe") {
				t.Fatalf("typed terminal output leaked raw message: trace=%q failure=%q", trace.FinalMessage, got.Message)
			}
		})
	}
}

func TestAdaptiveMixedLocalAndProviderFailureKeepsCompositeTerminalCause(t *testing.T) {
	for _, testCase := range []struct {
		name, localCode, wantRU string
	}{
		{name: "floor", localCode: "bravo_allocator_reserve_floor", wantRU: "резерв"},
		{name: "stale quota", localCode: "bravo_adaptive_quota_stale", wantRU: "квота подписки устарела"},
		{name: "primary zero", localCode: "bravo_adaptive_primary_zero", wantRU: "нулевого остатка"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			traces := []executionFailureTrace{
				{Provider: "claude", Model: "claude-fable-5", Failure: executionFailure{
					Code: testCase.localCode, Message: "unsafe local payload", Status: 503, Retryable: true, RouteFallback: true,
				}},
				{Provider: "codex", Model: "gpt-5.6-sol", Failure: executionFailure{
					Code: "server_error", Message: "unsafe provider payload", Status: 502, Retryable: true, RouteFallback: true,
				}},
			}
			got := finalExecutionFailure(traces, executionFailure{
				Code: "server_error", Message: "unsafe provider payload", Status: 502, Retryable: true, RouteFallback: true,
			})
			if got.Code != "bravo_route_temporarily_unavailable" {
				t.Fatalf("mixed terminal code = %q, want generic exhausted route", got.Code)
			}
			if !strings.Contains(got.Message, testCase.wantRU) || !strings.Contains(got.Message, "внутренняя ошибка") ||
				strings.Contains(got.Message, "bravo_adaptive_") || strings.Contains(got.Message, "unsafe") {
				t.Fatalf("mixed terminal message is not safe/actionable: %q", got.Message)
			}
			trace := sanitizeRouteTrace(routeTrace{FinalCode: "bravo_route_temporarily_unavailable", Attempts: []routeTraceAttempt{
				{ErrorCode: testCase.localCode, Outcome: "skipped"},
				{ErrorCode: "server_error", Outcome: "failed"},
			}})
			if trace.FinalCode != "bravo_route_temporarily_unavailable" || trace.ClientAction != "retry" {
				t.Fatalf("mixed sanitized terminal = code %q action %q", trace.FinalCode, trace.ClientAction)
			}
		})
	}
}

func TestAllocatorAllWithheldCapturesStableAdaptiveCauses(t *testing.T) {
	tests := []struct {
		name      string
		remaining float64
		prepare   func(string)
		wantCode  string
		wantCause adaptiveAdmissionRejectionCause
	}{
		{name: "floor", remaining: 50.5, wantCode: "bravo_allocator_reserve_floor", wantCause: adaptiveRejectionFloor},
		{name: "stale", remaining: -1, wantCode: "bravo_adaptive_quota_stale", wantCause: adaptiveRejectionQuotaStale},
		{name: "ledger", remaining: 90, prepare: func(string) { adaptiveRoutingSaturated.Store(true) }, wantCode: "bravo_adaptive_ledger_saturated", wantCause: adaptiveRejectionLedgerSaturated},
		{name: "estimator", remaining: 90, prepare: func(authIndex string) {
			adaptiveReserveRuntime.Lock()
			adaptiveReserveRuntime.Saturated[authIndex] = time.Now().UTC()
			adaptiveReserveRuntime.Unlock()
		}, wantCode: "bravo_adaptive_estimator_saturated", wantCause: adaptiveRejectionEstimatorSaturated},
		{name: "demand", remaining: 90, prepare: func(authIndex string) {
			bravoProjectDemand.mu.Lock()
			bravoProjectDemand.projectBlocked[authIndex] = time.Now().UTC()
			bravoProjectDemand.mu.Unlock()
		}, wantCode: "bravo_adaptive_demand_saturated", wantCause: adaptiveRejectionDemandSaturated},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetAdaptiveReserveForTest()
			t.Cleanup(resetAdaptiveReserveForTest)
			bravoProjectDemand = newProjectDemandTracker(defaultProjectDemandHalfLife)
			authIndex := "all-withheld-" + testCase.name
			cfg := defaultPluginConfig()
			cfg.AllocatorMode = "enforce"
			cfg.Tariffs = []tariffConfig{{
				ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, ReservationPercent: 1,
			}}
			cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			t.Cleanup(func() { currentConfig.Store(previousConfig) })
			if testCase.remaining >= 0 {
				installAdaptiveTestQuota(t, authIndex, testCase.remaining, testCase.remaining)
			}
			if testCase.prepare != nil {
				testCase.prepare(authIndex)
			}
			item := candidate{Provider: "claude", Model: "claude-fable-5"}
			rejections := allocatorCandidateRejections(cfg, smartKeyConfig{ID: "withheld-project"}, item,
				[]pluginapi.HostAuthFileEntry{{ID: "auth-id", AuthIndex: authIndex, Provider: "claude"}}, adaptiveRequestFeatures{})
			if len(rejections) != 1 {
				t.Fatalf("rejections = %#v", rejections)
			}
			got := rejections[0]
			if got.Code != testCase.wantCode || got.AdaptiveTrace.rejectionCause != testCase.wantCause {
				t.Fatalf("rejection = code %q cause %q, want %q/%q", got.Code, got.AdaptiveTrace.rejectionCause, testCase.wantCode, testCase.wantCause)
			}
			failure, retained := executionPlanFailure(&executionPlanError{Cause: errors.New("withheld"), Rejections: rejections})
			if failure.Code != testCase.wantCode || len(retained) != 1 || strings.TrimSpace(failure.Message) == "" {
				t.Fatalf("typed plan failure = %#v retained=%#v", failure, retained)
			}
			recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
			recorder.preflight(rejections)
			if len(recorder.trace.Attempts) != 1 || recorder.trace.Attempts[0].AdmissionRejectionCause != string(testCase.wantCause) ||
				recorder.trace.Attempts[0].SubscriptionID == "" {
				t.Fatalf("preflight trace = %#v", recorder.trace.Attempts)
			}
		})
	}
}

func TestExecuteAndStreamAllWithheldRecordPreflightWithoutProviderCalls(t *testing.T) {
	isolateBravoFallbackTestState(t)
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	bravoProjectDemand = newProjectDemandTracker(defaultProjectDemandHalfLife)
	authIndex := "all-withheld-execution-auth"
	cfg := defaultPluginConfig()
	cfg.Enabled = true
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 1}}
	cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "all-withheld-project", Name: "All withheld", SHA256: strings.Repeat("a", 64), Enabled: boolPointer(true), Status: projectStatusActive, Models: []string{"*"},
	}}
	cfg.Models = map[string]logicalModel{"all-withheld": {Candidates: []candidate{{
		Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText, capabilityStream},
	}}}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	installAdaptiveTestQuota(t, authIndex, 50.5, 50.5)
	auth := pluginapi.HostAuthFileEntry{ID: "all-withheld-auth-id", AuthIndex: authIndex, Provider: "claude"}
	providerCalls := 0
	var streamClose rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{auth}}), nil
		case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
			providerCalls++
			return nil, errors.New("provider must not be called")
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &streamClose)
			return json.RawMessage(`{}`), nil
		case pluginabi.MethodHostLog:
			return json.RawMessage(`{}`), nil
		default:
			return json.RawMessage(`{}`), nil
		}
	})
	request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/all-withheld", Format: protocolClaude, SourceFormat: protocolClaude,
		OriginalRequest: []byte(`{"model":"bravo/all-withheld","messages":[{"role":"user","content":"continue"}]}`),
		Metadata:        compactProjectMetadata("all-withheld-project"),
	}, HostCallbackID: "all-withheld-callback"}
	raw, errExecute := execute(mustJSONValue(t, request))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.OK || env.Error == nil || env.Error.Code != "bravo_allocator_reserve_floor" {
		t.Fatalf("execute response = %s error=%v", raw, errUnmarshal)
	}
	request.OriginalRequest = []byte(`{"model":"bravo/all-withheld","messages":[{"role":"user","content":"continue"}],"stream":true}`)
	runBravoStream(request, "all-withheld-stream")
	if providerCalls != 0 {
		t.Fatalf("all-withheld made %d provider calls", providerCalls)
	}
	if streamClose.ErrorCode != "bravo_allocator_reserve_floor" {
		t.Fatalf("stream close = %#v", streamClose)
	}
	traces, _, errList := listCurrentRouteTraces(routeTraceQuery{ProjectID: "all-withheld-project", ErrorsOnly: true, Limit: 10}, time.Now().UTC())
	if errList != nil || len(traces) != 2 {
		t.Fatalf("route traces = %#v warning error=%v", traces, errList)
	}
	for _, trace := range traces {
		if trace.FinalCode != "bravo_allocator_reserve_floor" || len(trace.Attempts) != 1 ||
			trace.Attempts[0].AdmissionRejectionCause != "floor" || trace.Attempts[0].SubscriptionID == "" {
			t.Fatalf("all-withheld trace = %#v", trace)
		}
	}
}

func TestAtomicLeaseRecheckPreservesQuotaStaleAndPrimaryZeroCauses(t *testing.T) {
	tests := []struct {
		name, wantCode, wantCause, wantAction string
		primary                               bool
		mutate                                func(string)
	}{
		{
			name: "fresh secondary expires", wantCode: "bravo_adaptive_quota_stale",
			wantCause: "quota_stale", wantAction: "refresh_quota",
			mutate: func(authIndex string) {
				storeQuotaSnapshot(authIndex, credentialQuotaState{
					Confidence: "confirmed", ConfirmedAt: time.Now().Add(-2 * time.Hour).UTC(),
					Session: quotaWindowState{RemainingPercent: 90}, Weekly: quotaWindowState{RemainingPercent: 90},
				})
			},
		},
		{
			name: "positive primary reaches zero", wantCode: "bravo_adaptive_primary_zero",
			wantCause: "primary_zero", wantAction: "raise_quota", primary: true,
			mutate: func(authIndex string) {
				storeQuotaSnapshot(authIndex, credentialQuotaState{
					Confidence: "confirmed", ConfirmedAt: time.Now().UTC(),
					Session: quotaWindowState{RemainingPercent: 0}, Weekly: quotaWindowState{RemainingPercent: 0},
				})
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			isolateBravoFallbackTestState(t)
			resetAdaptiveReserveForTest()
			t.Cleanup(resetAdaptiveReserveForTest)
			bravoProjectDemand = newProjectDemandTracker(defaultProjectDemandHalfLife)
			authIndex := "atomic-recheck-" + strings.ReplaceAll(testCase.name, " ", "-")
			cfg := defaultPluginConfig()
			cfg.Enabled = true
			cfg.AllocatorMode = "enforce"
			cfg.Tariffs = []tariffConfig{{
				ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1,
			}}
			cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
			project := smartKeyConfig{
				ID: "atomic-recheck-project", Name: "Atomic recheck", SHA256: strings.Repeat("a", 64),
				Enabled: boolPointer(true), Status: projectStatusActive, Models: []string{"*"},
			}
			if testCase.primary {
				project.PrimaryAuthIDs = []string{authIndex}
			}
			cfg.SmartKeys = []smartKeyConfig{project}
			cfg.Models = map[string]logicalModel{"atomic-recheck": {Candidates: []candidate{{
				Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText},
			}}}}
			if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
				t.Fatal(errNormalize)
			}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			t.Cleanup(func() { currentConfig.Store(previousConfig) })
			installAdaptiveTestQuota(t, authIndex, 90, 90)
			providerCalls := 0
			installBravoHostCall(t, func(method string, _ any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
						ID: "atomic-recheck-auth-id", AuthIndex: authIndex, Provider: "claude",
					}}}), nil
				case pluginabi.MethodHostModelExecute:
					providerCalls++
					return nil, errors.New("provider must not be called after failed atomic recheck")
				case pluginabi.MethodHostLog:
					return json.RawMessage(`{}`), nil
				default:
					return json.RawMessage(`{}`), nil
				}
			})
			acquireAttemptLeaseBeforeAdmissionHook = func() {
				acquireAttemptLeaseBeforeAdmissionHook = nil
				testCase.mutate(authIndex)
			}
			t.Cleanup(func() { acquireAttemptLeaseBeforeAdmissionHook = nil })
			raw, errExecute := execute(mustJSONValue(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "bravo/atomic-recheck", Format: protocolClaude, SourceFormat: protocolClaude,
				OriginalRequest: []byte(`{"model":"bravo/atomic-recheck","messages":[{"role":"user","content":"continue"}]}`),
				Metadata:        compactProjectMetadata("atomic-recheck-project"),
			}, HostCallbackID: "atomic-recheck-callback"}))
			if errExecute != nil {
				t.Fatal(errExecute)
			}
			var env envelope
			if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.OK || env.Error == nil || env.Error.Code != testCase.wantCode {
				t.Fatalf("atomic recheck response = %s error=%v", raw, errUnmarshal)
			}
			if providerCalls != 0 {
				t.Fatalf("atomic recheck made %d provider calls", providerCalls)
			}
			traces, _, errList := listCurrentRouteTraces(routeTraceQuery{ProjectID: "atomic-recheck-project", ErrorsOnly: true, Limit: 2}, time.Now().UTC())
			if errList != nil || len(traces) != 1 {
				t.Fatalf("atomic recheck traces = %#v error=%v", traces, errList)
			}
			trace := traces[0]
			if trace.FinalCode != testCase.wantCode || trace.ClientAction != testCase.wantAction || len(trace.Attempts) != 1 ||
				trace.Attempts[0].AdmissionRejectionCause != testCase.wantCause || trace.Attempts[0].ErrorCode != testCase.wantCode {
				t.Fatalf("atomic recheck trace = %#v", trace)
			}
		})
	}
}

func TestSuccessfulFallbackDoesNotInheritEarlierTerminalAction(t *testing.T) {
	trace := sanitizeRouteTrace(routeTrace{
		Success: true,
		Attempts: []routeTraceAttempt{
			{ErrorCode: "bravo_context_window_exceeded", Outcome: "failed"},
			{Provider: "claude", Model: "claude-fable-5", Success: true, Outcome: "succeeded"},
		},
	})
	if trace.FinalCode != "" || trace.ClientAction != "none" {
		t.Fatalf("successful fallback inherited terminal state: code=%q action=%q", trace.FinalCode, trace.ClientAction)
	}
}

func TestAdaptiveRouteTraceSanitizationRejectsSecretsAndNonFiniteValues(t *testing.T) {
	const sentinel = "sk-secret-plaintext-must-not-survive"
	trace := sanitizeRouteTrace(routeTrace{
		TraceID:   "trc_privacy",
		StartedAt: time.Now().UTC(),
		Attempts: []routeTraceAttempt{{
			Provider:             "claude",
			Model:                sentinel + " invalid space",
			SubscriptionLabel:    sentinel,
			ErrorCode:            "bravo_allocator_reserve_floor",
			ErrorMessage:         sentinel,
			AllocatorMode:        "enforce",
			ProjectRole:          "secondary",
			AdaptiveDecision:     "adaptive_green_admitted",
			AdaptiveRejection:    "adaptive_secondary_floor_protected",
			AdaptiveFallback:     "adaptive_failover_selected",
			FallbackProvider:     "codex",
			FallbackModel:        sentinel + " invalid space",
			ReservationPercent:   101,
			PendingGuardPercent:  148,
			SessionHeadroomAfter: -1000,
		}},
	})
	raw, errMarshal := json.Marshal(trace)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("adaptive route trace leaked privacy sentinel: %s", raw)
	}
	if trace.Attempts[0].ReservationPercent != 100 || trace.Attempts[0].PendingGuardPercent != 100 || trace.Attempts[0].SessionHeadroomAfter != -100 {
		t.Fatalf("unsafe adaptive numeric values survived: %#v", trace.Attempts[0])
	}
}

func TestAdaptiveManagementCountsLeaseLifecycle(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{
		ID: "x1", SessionFloorPercent: 10, WeeklyFloorPercent: 10,
		Multiplier: 1, ReservationPercent: 1,
	}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	auth := pluginapi.HostAuthFileEntry{AuthIndex: "trace-count-auth", Provider: "claude"}
	installAdaptiveTestQuota(t, auth.AuthIndex, 90, 90)
	attempt := adaptiveTraceAttempt(auth.AuthIndex, "claude", "claude-fable-5", 1)
	first, firstOK := acquireAttemptLease(attempt)
	second, secondOK := acquireAttemptLease(attempt)
	if !firstOK || !secondOK {
		t.Fatal("count lifecycle fixture did not acquire both leases")
	}
	view := adaptiveSubscriptionRuntimeView(cfg, auth, cfg.Tariffs[0], quotaSnapshot(auth.AuthIndex), time.Now().UTC())
	if view.InFlightRequestCount != 2 || view.PendingRequestCount != 0 {
		t.Fatalf("in-flight request count view = %#v", view)
	}
	first(true)
	second(true)
	view = adaptiveSubscriptionRuntimeView(cfg, auth, cfg.Tariffs[0], quotaSnapshot(auth.AuthIndex), time.Now().UTC())
	if view.InFlightRequestCount != 0 || view.PendingRequestCount != 2 {
		t.Fatalf("pending request count view = %#v", view)
	}
}

func TestAdaptiveTraceUsesEffectiveLateLearnedReservation(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 10, WeeklyFloorPercent: 10, Multiplier: 1, ReservationPercent: 0.1}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := adaptiveTraceAttempt("late-learning-auth", "claude", "claude-fable-5", 0.1)
	attempt.AdaptiveBaselinePercent = 0.1
	attempt.AdaptiveRequestShape = adaptiveRequestShape{
		Provider: "claude", PhysicalModel: "claude-fable-5", ModelFamily: "fable",
		EffortBucket: "xhigh", Multiplier: 5, ContextBucket: "large", CostMode: "unknown",
	}
	attempt.AdaptiveReserveKey = adaptiveProfileKey(attempt.Auth.AuthIndex, attempt.AdaptiveRequestShape)
	installAdaptiveTestQuota(t, attempt.Auth.AuthIndex, 90, 90)
	pendingBefore := pendingReservationPercent(attempt.Auth.AuthIndex)
	release, acquired, effectiveAttempt := acquireAttemptLeaseDetailed(attempt)
	if !acquired || effectiveAttempt.ReservationPercent <= attempt.ReservationPercent {
		t.Fatalf("late learned admission = acquired %t plan %.3f effective %.3f", acquired, attempt.ReservationPercent, effectiveAttempt.ReservationPercent)
	}
	release(true)
	pendingDelta := pendingReservationPercent(attempt.Auth.AuthIndex) - pendingBefore
	recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.success(effectiveAttempt, time.Now(), http.StatusOK)
	traced := recorder.trace.Attempts[0].ReservationPercent
	if traced != effectiveAttempt.ReservationPercent || traced != pendingDelta {
		t.Fatalf("reservation plan/effective/pending/trace = %.3f/%.3f/%.3f/%.3f", attempt.ReservationPercent, effectiveAttempt.ReservationPercent, pendingDelta, traced)
	}
}

func TestAdaptiveTraceAdmissionSnapshotDoesNotMoveAfterQuotaRefresh(t *testing.T) {
	resetAdaptiveReserveForTest()
	t.Cleanup(resetAdaptiveReserveForTest)
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previousConfig) })
	attempt := adaptiveTraceAttempt("immutable-trace-auth", "claude", "claude-fable-5", 1)
	installAdaptiveTestQuota(t, attempt.Auth.AuthIndex, 80, 80)
	release, acquired, effectiveAttempt := acquireAttemptLeaseDetailed(attempt)
	if !acquired {
		t.Fatal("immutable trace fixture was not admitted")
	}
	wantBefore := effectiveAttempt.AdaptiveTrace.sessionBefore
	wantAfter := effectiveAttempt.AdaptiveTrace.sessionAfter
	refreshed := make(chan struct{})
	go func() {
		storeQuotaSnapshot(attempt.Auth.AuthIndex, credentialQuotaState{
			Confidence: "confirmed", ConfirmedAt: time.Now().UTC(),
			Session: quotaWindowState{RemainingPercent: 40}, Weekly: quotaWindowState{RemainingPercent: 40},
		})
		close(refreshed)
	}()
	<-refreshed
	release(true)
	recorder := &routeTraceRecorder{trace: routeTrace{StartedAt: time.Now().UTC()}}
	recorder.success(effectiveAttempt, time.Now(), http.StatusOK)
	got := recorder.trace.Attempts[0]
	if got.SessionHeadroomBefore != wantBefore || got.SessionHeadroomAfter != wantAfter {
		t.Fatalf("immutable snapshot moved after refresh: got %.3f/%.3f want %.3f/%.3f", got.SessionHeadroomBefore, got.SessionHeadroomAfter, wantBefore, wantAfter)
	}
	if got.SessionHeadroomBefore <= 0 {
		t.Fatalf("trace was recomputed from post-admission quota: %#v", got)
	}
}

func adaptiveTraceAttempt(authIndex, provider, model string, reservation float64) executionAttempt {
	return executionAttempt{
		Candidate: candidate{Provider: provider, Model: model},
		Auth: pluginapi.HostAuthFileEntry{
			AuthIndex: authIndex,
			Provider:  provider,
		},
		ProjectID:          "trace-project",
		AllocatorManaged:   true,
		ReservationPercent: reservation,
		TariffID:           "x1",
	}
}
