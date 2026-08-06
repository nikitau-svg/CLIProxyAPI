package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNestedHostModelRequestSeparatesLogicalAndPhysicalModels(t *testing.T) {
	req := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "bravo/opus"},
	}
	attempt := executionAttempt{
		LogicalModel: "opus",
		Candidate: candidate{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
		},
		Auth: pluginapi.HostAuthFileEntry{
			ID:        "codex-account-1",
			AuthIndex: "codex-index-1",
		},
	}

	got := nestedHostModelRequest(req, attempt, protocolOpenAI, "gpt-5.6-sol", []byte(`{}`), false)

	if got.Model != "gpt-5.6-sol" {
		t.Fatalf("physical model = %q, want gpt-5.6-sol", got.Model)
	}
	if got.UsageAlias != "bravo/opus" {
		t.Fatalf("usage alias = %q, want bravo/opus", got.UsageAlias)
	}
	if got.ForcedProvider != "codex" || got.AuthID != "codex-account-1" {
		t.Fatalf("provider/auth = %q/%q, want codex/codex-account-1", got.ForcedProvider, got.AuthID)
	}
}

func TestUsageStateKeepsLogicalRouteAlongsidePhysicalDimensions(t *testing.T) {
	store := usageStateStore{state: newPersistedUsageState()}
	at := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store.record(pluginapi.UsageRecord{
		Provider:    "openai",
		Model:       "gpt-5.6-sol",
		Alias:       "bravo/opus",
		APIKey:      "bravo:prj_alpha",
		AuthIndex:   "codex-index-1",
		RequestedAt: at,
		Detail:      pluginapi.UsageDetail{TotalTokens: 42},
	})

	key := usageDimensionKey("prj_alpha", "codex-index-1", "codex", "gpt-5.6-sol", "bravo/opus")
	got := store.state.ProjectSubscriptionModelTotals[key]
	if got == nil {
		t.Fatalf("missing project/subscription/model dimensions for key %q", key)
	}
	if got.ProjectID != "prj_alpha" ||
		got.AuthIndex != "codex-index-1" ||
		got.Provider != "codex" ||
		got.Model != "gpt-5.6-sol" ||
		got.LogicalModel != "bravo/opus" {
		t.Fatalf("usage dimensions = %#v", got)
	}
	if got.Usage.Total.TotalTokens != 42 {
		t.Fatalf("total tokens = %d, want 42", got.Usage.Total.TotalTokens)
	}
}

func TestUsageStateSeparatesFullDurationFromStreamingTTFT(t *testing.T) {
	store := usageStateStore{state: newPersistedUsageState()}
	store.record(pluginapi.UsageRecord{
		Provider:    "anthropic",
		Model:       "claude-fable-5",
		Alias:       "bravo/fable",
		APIKey:      "bravo:prj_alpha",
		AuthIndex:   "claude-index-1",
		RequestedAt: time.Now().UTC(),
		Latency:     11 * time.Second,
		TTFT:        850 * time.Millisecond,
		Detail:      pluginapi.UsageDetail{TotalTokens: 10},
	})
	store.record(pluginapi.UsageRecord{
		Provider:    "anthropic",
		Model:       "claude-fable-5",
		Alias:       "bravo/fable",
		APIKey:      "bravo:prj_alpha",
		AuthIndex:   "claude-index-1",
		RequestedAt: time.Now().UTC(),
		Latency:     9 * time.Second,
		Detail:      pluginapi.UsageDetail{TotalTokens: 5},
	})

	summary := usageSummary(store.state.ProjectTotals["prj_alpha"], time.Now().UTC())
	if summary.AverageLatencyMS != 10_000 {
		t.Fatalf("average full duration = %v, want 10000ms", summary.AverageLatencyMS)
	}
	if summary.AverageTTFTMS != 850 {
		t.Fatalf("average TTFT = %v, want 850ms from streaming samples only", summary.AverageTTFTMS)
	}
}
