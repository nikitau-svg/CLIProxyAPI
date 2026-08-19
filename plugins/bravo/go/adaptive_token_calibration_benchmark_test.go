package main

import (
	"runtime"
	"strconv"
	"testing"
	"time"
)

func BenchmarkAdaptiveTokenLookupAtProfileCapacity(b *testing.B) {
	bravoUsageState.mu.Lock()
	previous := bravoUsageState.state
	state := newPersistedUsageState()
	now := time.Now().UTC()
	for index := 0; index < adaptiveTokenMaximumUsageProfiles-1; index++ {
		model := "model-" + strconv.Itoa(index)
		key := adaptiveTokenUsageProfileKey("auth", "claude", model, "", "x1")
		state.AdaptiveTokenUsageProfiles[key] = adaptiveTokenBenchmarkUsageProfile("auth", model, now)
	}
	usageKey := adaptiveTokenUsageProfileKey("target", "claude", "claude-sonnet-5", "", "x1")
	state.AdaptiveTokenUsageProfiles[usageKey] = adaptiveTokenBenchmarkUsageProfile("target", "claude-sonnet-5", now)
	for index, kind := range []string{"session", "weekly"} {
		key := adaptiveTokenWindowProfileKey("target", "claude", "claude-sonnet-5", "", "x1", kind, "")
		state.AdaptiveTokenWindowProfiles[key] = &persistedAdaptiveTokenWindowProfile{
			AuthIndex: "target", Provider: "claude", Model: "claude-sonnet-5", WindowKind: kind,
			IntervalSamples: 8, EffectiveIntervals: 8, CoverageSeconds: 7200,
			EffectiveTokenUnits: 100000, AttributedDropPercent: float64(index + 1), UpdatedAt: now,
		}
	}
	bravoUsageState.state = state
	bravoUsageState.mu.Unlock()
	b.Cleanup(func() {
		bravoUsageState.mu.Lock()
		bravoUsageState.state = previous
		bravoUsageState.mu.Unlock()
	})
	features := adaptiveShadowRequestFeatures{InputTokens: 4096, DeclaredOutputTokens: 8192, OutputTrusted: true}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := adaptiveTokenCalibrationFor("target", "claude", "claude-sonnet-5", "", "x1", credentialQuotaState{}, features, now)
		if !result.Session.Available || !result.Weekly.Available {
			b.Fatal("calibration unexpectedly unavailable")
		}
	}
}

func BenchmarkUsageStateCloneWithAdaptiveTokenCapacity(b *testing.B) {
	state := newPersistedUsageState()
	now := time.Now().UTC()
	for index := 0; index < adaptiveTokenMaximumUsageProfiles; index++ {
		model := "model-" + strconv.Itoa(index)
		usageKey := adaptiveTokenUsageProfileKey("auth", "claude", model, "", "x1")
		state.AdaptiveTokenUsageProfiles[usageKey] = adaptiveTokenBenchmarkUsageProfile("auth", model, now)
		windowKey := adaptiveTokenWindowProfileKey("auth", "claude", model, "", "x1", "weekly", "")
		state.AdaptiveTokenWindowProfiles[windowKey] = &persistedAdaptiveTokenWindowProfile{
			AuthIndex: "auth", Provider: "claude", Model: model, WindowKind: "weekly", UpdatedAt: now,
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		cloned := clonePersistedUsageState(state)
		runtime.KeepAlive(cloned)
	}
}

func adaptiveTokenBenchmarkUsageProfile(authIndex, model string, now time.Time) *persistedAdaptiveTokenUsageProfile {
	buckets := make([]float64, len(adaptiveTokenCompletionBuckets)+1)
	buckets[5] = 8
	return &persistedAdaptiveTokenUsageProfile{
		AuthIndex: authIndex, Provider: "claude", Model: model,
		SampleCount: 8, Samples: 8, InputTokens: 32000, OutputTokens: 16000,
		CompletionBuckets: buckets, UpdatedAt: now,
	}
}
