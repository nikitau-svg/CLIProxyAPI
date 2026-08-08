package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAdaptiveOneMegabyteSaturatedShapePathAllocatesNoHeap(t *testing.T) {
	body := append([]byte(`{"messages":[{"role":"user","content":"`), bytes.Repeat([]byte("x"), 1024*1024)...)
	body = append(body, []byte(`"}],"max_tokens":8192}`)...)
	tracker := saturatedDemandTrackerForPerfTest()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	items := []candidate{{Model: "claude-fable-5", Effort: "xhigh"}, {Model: "gpt-5.6-sol", Effort: "xhigh"}}
	allocations := testing.AllocsPerRun(50, func() {
		features := adaptiveRequestFeaturesFor(body)
		for _, item := range items {
			_ = adaptiveRequestShapeFromFeatures(features, item)
		}
		tracker.maintain(now.Add(30 * time.Second))
	})
	if allocations != 0 {
		t.Fatalf("1MB saturated allocation shape path allocations = %.1f, want 0", allocations)
	}
}

func BenchmarkAdaptiveAllocationShapeOneMegabyteSaturatedDemand(b *testing.B) {
	body := append([]byte(`{"messages":[{"role":"user","content":"`), bytes.Repeat([]byte("x"), 1024*1024)...)
	body = append(body, []byte(`"}],"max_tokens":8192}`)...)
	tracker := saturatedDemandTrackerForPerfTest()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	candidates := []candidate{
		{Model: "claude-fable-5", Effort: "xhigh"},
		{Model: "gpt-5.6-sol", Effort: "xhigh"},
		{Model: "claude-haiku-4-5", Effort: "low"},
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		features := adaptiveRequestFeaturesFor(body)
		for _, item := range candidates {
			_ = adaptiveRequestShapeFromFeatures(features, item)
		}
		tracker.maintain(now.Add(30 * time.Second))
	}
}

func TestAdaptiveFourMegabyteFullBuildPlanP95BelowTwoMilliseconds(t *testing.T) {
	req, model := adaptiveFourMegabytePlanFixture(t)
	durations := make([]time.Duration, 60)
	for index := range durations {
		started := time.Now()
		plan, errPlan := buildExecutionPlan(req, "perf-route", model, textContract())
		durations[index] = time.Since(started)
		if errPlan != nil || len(plan) == 0 {
			t.Fatalf("full build plan failed: attempts=%d error=%v", len(plan), errPlan)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95-1)/100]
	limit := 2 * time.Millisecond
	if adaptiveRaceEnabled {
		// Race instrumentation deliberately taxes every map/lock access; retain
		// the functional fixture there but enforce the production compute SLO on
		// the ordinary binary.
		limit = 10 * time.Millisecond
	}
	if p95 > limit {
		t.Fatalf("4MB max-context full build-plan p95 = %s, want <=%s", p95, limit)
	}
}

func BenchmarkAdaptiveBuildPlanFourMegabyteMaxContext(b *testing.B) {
	req, model := adaptiveFourMegabytePlanFixture(b)
	b.SetBytes(int64(len(req.OriginalRequest)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, errPlan := buildExecutionPlan(req, "perf-route", model, textContract())
		if errPlan != nil || len(plan) == 0 {
			b.Fatalf("full build plan failed: attempts=%d error=%v", len(plan), errPlan)
		}
	}
}

func BenchmarkAdaptiveLeaseWALP95(b *testing.B) {
	restore := isolateBravoUsageState(b)
	b.Cleanup(restore)
	resetAdaptiveReserveForTest()
	b.Cleanup(resetAdaptiveReserveForTest)
	if errConfigure := configureUsageState(filepath.Join(b.TempDir(), "lease-wal.json")); errConfigure != nil {
		b.Fatal(errConfigure)
	}
	authIndex := "lease-wal-perf"
	setAdaptivePersistenceQuota(b, authIndex, 100)
	attempt := adaptivePersistenceAttempt(authIndex, 0.5)
	attempt.Primary = true
	durations := make([]time.Duration, b.N)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		release, acquired := acquireAttemptLease(attempt)
		if !acquired {
			b.Fatal("lease/WAL benchmark admission failed")
		}
		release(false)
		durations[index] = time.Since(started)
	}
	b.StopTimer()
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95 := durations[(len(durations)*95-1)/100]
		b.ReportMetric(float64(p95.Nanoseconds()), "p95-ns/op")
	}
}

func adaptiveFourMegabytePlanFixture(tb testing.TB) (rpcExecutorRequest, logicalModel) {
	tb.Helper()
	restoreUsage := isolateBravoUsageState(tb)
	tb.Cleanup(restoreUsage)
	resetAdaptiveReserveForTest()
	tb.Cleanup(resetAdaptiveReserveForTest)
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "enforce"
	auths := make([]pluginapi.HostAuthFileEntry, 0, 20)
	allowed := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		provider := "claude"
		if index >= 10 {
			provider = "codex"
		}
		authIndex := fmt.Sprintf("perf-auth-%02d", index)
		auths = append(auths, pluginapi.HostAuthFileEntry{ID: authIndex + "-id", AuthIndex: authIndex, Provider: provider})
		allowed = append(allowed, authIndex)
		cfg.Subscriptions = append(cfg.Subscriptions, subscriptionConfig{AuthIndex: authIndex, Tariff: "x5"})
		setAdaptivePersistenceQuota(tb, authIndex, 100)
	}
	cfg.SmartKeys = append(cfg.SmartKeys, smartKeyConfig{
		ID: "perf-project", Name: "Performance project", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: projectStatusActive, Models: []string{"*"},
		PrimaryAuthIDs: []string{auths[10].AuthIndex}, AllowedAuthIDs: allowed,
	})
	for index := 1; index < 30; index++ {
		cfg.SmartKeys = append(cfg.SmartKeys, smartKeyConfig{
			ID: fmt.Sprintf("perf-owner-%02d", index), Name: fmt.Sprintf("Performance owner %02d", index),
			SHA256: fmt.Sprintf("%064x", index+1), Status: projectStatusActive, Models: []string{"*"},
			PrimaryAuthIDs: []string{auths[index%len(auths)].AuthIndex}, AllowedAuthIDs: allowed,
		})
	}
	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Effort: "xhigh", Priority: 100, Capabilities: []string{capabilityText}},
		{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Priority: 90, Capabilities: []string{capabilityText}},
		{Provider: "claude", Model: "claude-haiku-4-5", Effort: "low", Priority: 80, Capabilities: []string{capabilityText}},
	}}
	cfg.Models = map[string]logicalModel{"perf-route": model}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		tb.Fatal(errNormalize)
	}
	previousConfig := loadedConfig()
	currentConfig.Store(cfg)
	tb.Cleanup(func() { currentConfig.Store(previousConfig) })
	authListRaw, errMarshal := json.Marshal(hostAuthListResponse{Files: auths})
	if errMarshal != nil {
		tb.Fatal(errMarshal)
	}
	previousHost := swapHostCall(func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return authListRaw, nil
		case pluginabi.MethodHostLog:
			return json.RawMessage(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected perf host callback %q", method)
		}
	})
	tb.Cleanup(func() { swapHostCall(previousHost) })
	body := append([]byte(`{"messages":[{"role":"user","content":"`), bytes.Repeat([]byte("x"), 4*1024*1024)...)
	body = append(body, []byte(`"}],"max_completion_tokens":65536}`)...)
	metadata := compactProjectMetadata("perf-project")
	metadata["request_id"] = "perf-sticky-id"
	return rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/perf-route", Format: protocolClaude, SourceFormat: protocolClaude,
		OriginalRequest: body, Metadata: metadata,
	}}, model
}

func saturatedDemandTrackerForPerfTest() *projectDemandTracker {
	tracker := newProjectDemandTracker(time.Minute)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tracker.mu.Lock()
	for index := 0; index < projectDemandMaximumEntries; index++ {
		key := projectDemandLoanKey{projectID: fmt.Sprintf("project-%05d", index), authIndex: fmt.Sprintf("auth-%05d", index)}
		tracker.projects[key] = &projectDemandSample{at: now, lastActivity: now}
		tracker.loans[key] = &projectDemandSample{at: now, lastActivity: now}
	}
	tracker.lastPrune = now
	tracker.mu.Unlock()
	return tracker
}
