package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRouteTraceStorePersistsSafeBoundedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(path)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	secret := "sk-bravo-secret-sentinel"

	store.append(routeTrace{
		TraceID:        "trace-safe",
		StartedAt:      now,
		CompletedAt:    now.Add(2 * time.Second),
		ProjectID:      "prj_alpha",
		LogicalModel:   "bravo/fable",
		SourceProtocol: "claude",
		Status:         503,
		Success:        false,
		FinalCode:      "bravo_context_window_exceeded",
		FinalMessage:   "Контекст не помещается; " + secret,
		Attempts: []routeTraceAttempt{{
			Provider:          "claude",
			Model:             "claude-fable-5",
			SubscriptionID:    analyticsSubscriptionID("auth-" + secret),
			SubscriptionLabel: "Рабочая подписка " + secret,
			Status:            400,
			ErrorCode:         "bravo_context_window_exceeded",
			ErrorMessage:      "provider said " + secret,
		}},
	})
	if err := store.flush(); err != nil {
		t.Fatalf("flush route traces: %v", err)
	}

	raw, errRead := os.ReadFile(routeTracePath(path))
	if errRead != nil {
		t.Fatalf("read route traces: %v", errRead)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("persisted route trace exposed a request or credential secret")
	}
	if info, errStat := os.Stat(routeTracePath(path)); errStat != nil {
		t.Fatalf("stat route traces: %v", errStat)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("route trace permissions = %o, want 600", info.Mode().Perm())
	}

	reloaded := newRouteTraceStore(path)
	traces, errList := reloaded.list(routeTraceQuery{ProjectID: "prj_alpha", Limit: 10}, now.Add(3*time.Second))
	if errList != nil {
		t.Fatalf("list route traces: %v", errList)
	}
	if len(traces) != 1 || traces[0].TraceID != "trace-safe" {
		t.Fatalf("reloaded route traces = %#v", traces)
	}
	if strings.Contains(traces[0].FinalMessage, secret) || strings.Contains(traces[0].Attempts[0].SubscriptionLabel, secret) {
		t.Fatal("route trace API exposed unsafe provider text")
	}
}

func TestRouteTraceManagementFiltersErrorsByProject(t *testing.T) {
	store := newRouteTraceStore(filepath.Join(t.TempDir(), "bravo-state.json"))
	restore := replaceRouteTraceStoreForTest(store)
	t.Cleanup(restore)
	now := time.Now().UTC()
	store.append(routeTrace{TraceID: "trc_ok", StartedAt: now, ProjectID: "prj_alpha", Success: true, Status: 200})
	store.append(routeTrace{
		TraceID:      "trc_failed",
		StartedAt:    now.Add(time.Second),
		ProjectID:    "prj_alpha",
		Status:       503,
		FinalCode:    "bravo_route_temporarily_unavailable",
		FinalMessage: "unsafe raw provider message",
	})
	store.append(routeTrace{TraceID: "trc_other", StartedAt: now.Add(2 * time.Second), ProjectID: "prj_beta", Status: 503})

	rawRequest, errMarshal := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/bravo/traces",
		Query: url.Values{
			"project_id":  []string{"prj_alpha"},
			"errors_only": []string{"true"},
		},
	}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errHandle := handleManagement(rawRequest)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("route trace status = %d body=%s", response.StatusCode, response.Body)
	}
	var body struct {
		Traces []routeTrace `json:"traces"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(body.Traces) != 1 || body.Traces[0].TraceID != "trc_failed" {
		t.Fatalf("filtered route traces = %#v", body.Traces)
	}
	if body.Traces[0].FinalMessage != routeTraceMessageRU("bravo_route_temporarily_unavailable") {
		t.Fatalf("unsafe route message escaped: %q", body.Traces[0].FinalMessage)
	}
}

func TestRouteTraceStorePrunesRetentionAndCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(path)
	now := time.Now().UTC()
	store.maxEntries = 2
	store.retention = 24 * time.Hour

	store.append(routeTrace{TraceID: "expired", StartedAt: now.Add(-48 * time.Hour)})
	store.append(routeTrace{TraceID: "one", StartedAt: now.Add(-2 * time.Hour)})
	store.append(routeTrace{TraceID: "two", StartedAt: now.Add(-time.Hour)})
	store.append(routeTrace{TraceID: "three", StartedAt: now})

	traces, errList := store.list(routeTraceQuery{Limit: 10}, now)
	if errList != nil {
		t.Fatalf("list route traces: %v", errList)
	}
	if len(traces) != 2 || traces[0].TraceID != "three" || traces[1].TraceID != "two" {
		t.Fatalf("pruned route traces = %#v", traces)
	}
}

func TestRouteTraceRecorderPersistsProviderPathWithoutRawAuthID(t *testing.T) {
	store := newRouteTraceStore(filepath.Join(t.TempDir(), "bravo-state.json"))
	restore := replaceRouteTraceStoreForTest(store)
	t.Cleanup(restore)
	const rawAuthID = "claude-secret-auth-id"
	recorder := newRouteTraceRecorder(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "bravo/fable"},
	}, "bravo/fable", protocolClaude, false)
	attempt := executionAttempt{
		Candidate: candidate{Provider: "claude", Model: "claude-fable-5"},
		Auth: pluginapi.HostAuthFileEntry{
			ID:        "runtime-auth-id-differs",
			AuthIndex: rawAuthID,
		},
	}
	detail := providererror.Detail{
		Type:            "invalid_request_error",
		Code:            "context_window_exceeded",
		Scope:           providererror.ScopeRequest,
		Class:           providererror.ClassContextWindow,
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		RequiredTokens:  1_003_466,
		LimitTokens:     1_000_000,
	}
	failure := contextExecutionFailure(detail)
	recorder.failure(attempt, time.Now().Add(-time.Second), http.StatusBadRequest, failure)
	traceID := recorder.finish(false, failure.Status, failure)

	traces, errList := store.list(routeTraceQuery{TraceID: traceID}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 1 || len(traces[0].Attempts) != 1 {
		t.Fatalf("recorded route traces = %#v", traces)
	}
	got := traces[0].Attempts[0]
	if got.Ordinal != 1 || got.Outcome != "failed" || got.Decision != "stop" || got.Committed {
		t.Fatalf("attempt routing decision = %#v", got)
	}
	if got.SubscriptionID == "" || got.SubscriptionID == rawAuthID {
		t.Fatalf("subscription id was not pseudonymized: %q", got.SubscriptionID)
	}
	if got.SubscriptionID != analyticsSubscriptionID(rawAuthID) {
		t.Fatalf("subscription id = %q, want stable auth-index identity", got.SubscriptionID)
	}
	if got.RequiredInputTokens != 1_003_466 || got.SupportedInputTokens != 1_000_000 {
		t.Fatalf("context evidence = %d/%d", got.RequiredInputTokens, got.SupportedInputTokens)
	}
	if traces[0].Outcome != "failed" || traces[0].ClientAction != "compact" {
		t.Fatalf("route outcome/action = %q/%q", traces[0].Outcome, traces[0].ClientAction)
	}
	encoded, errMarshal := json.Marshal(traces)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), rawAuthID) {
		t.Fatal("route trace exposed the raw auth id")
	}
}

func TestFailedRouteTraceSurvivesImmediateReload(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "bravo-state.json")
	store := newRouteTraceStore(statePath)
	restore := replaceRouteTraceStoreForTest(store)
	t.Cleanup(restore)

	recorder := newRouteTraceRecorder(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "bravo/fable"},
	}, "bravo/fable", protocolClaude, false)
	failure := executionFailure{
		Status:  http.StatusServiceUnavailable,
		Code:    "bravo_route_temporarily_unavailable",
		Message: "unsafe provider diagnostic",
	}
	traceID := recorder.finish(false, failure.Status, failure)

	// Do not call flush and do not wait for the batching timer: a new process
	// must be able to read a terminal trace as soon as finish returns.
	reloaded := newRouteTraceStore(statePath)
	traces, errList := reloaded.list(routeTraceQuery{TraceID: traceID}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 1 || traces[0].TraceID != traceID || traces[0].Success {
		t.Fatalf("immediately reloaded terminal traces = %#v", traces)
	}
}

func TestFailedRouteTracePersistenceFailureDoesNotLoseInMemoryDiagnostic(t *testing.T) {
	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	if errWrite := os.WriteFile(blockedParent, []byte("blocked"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := newRouteTraceStore(filepath.Join(blockedParent, "bravo-state.json"))
	trace := routeTrace{
		TraceID:   "trc_persistence_failure",
		StartedAt: time.Now().UTC(),
		Status:    http.StatusBadGateway,
		Success:   false,
		FinalCode: "bravo_route_temporarily_unavailable",
	}
	if errAppend := store.appendDurable(trace); errAppend == nil {
		t.Fatal("appendDurable unexpectedly succeeded with a non-directory parent")
	}
	if !strings.Contains(store.warning(), "остаётся доступна в памяти") {
		t.Fatalf("safe persistence warning = %q", store.warning())
	}
	traces, errList := store.list(routeTraceQuery{TraceID: trace.TraceID}, time.Now().UTC())
	if errList != nil {
		t.Fatal(errList)
	}
	if len(traces) != 1 || traces[0].TraceID != trace.TraceID {
		t.Fatalf("in-memory terminal trace = %#v", traces)
	}
	_ = store.close()
}

func TestRouteTraceExplainsModelCreditsBeforeContextFallback(t *testing.T) {
	trace := sanitizeRouteTrace(routeTrace{
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		Status:      http.StatusBadRequest,
		Success:     false,
		FinalCode:   "bravo_context_window_exceeded",
		Attempts: []routeTraceAttempt{
			{
				Provider:  "claude",
				Model:     "claude-fable-5",
				Status:    http.StatusTooManyRequests,
				ErrorCode: "bravo_subscription_model_credits_exhausted",
				Outcome:   "failed",
				Decision:  "fallback",
			},
			{
				Provider:  "codex",
				Model:     "gpt-5.6-sol",
				Status:    http.StatusBadRequest,
				ErrorCode: "bravo_context_window_exceeded",
				Outcome:   "failed",
				Decision:  "stop",
			},
		},
	})

	for _, want := range []string{
		"Fable 5",
		"лимит расходов",
		"Sol",
		"не вместил контекст",
		"/compact",
	} {
		if !strings.Contains(trace.FinalMessage, want) {
			t.Errorf("route trace message = %q, missing %q", trace.FinalMessage, want)
		}
	}
	if trace.ClientAction != "compact" {
		t.Fatalf("route trace client action = %q, want compact", trace.ClientAction)
	}
}
