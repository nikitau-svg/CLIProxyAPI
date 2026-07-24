package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDefaultConfigCoversConnectedGeneralModels(t *testing.T) {
	t.Parallel()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatalf("normalizeConfig() error = %v", errNormalize)
	}
	expected := []string{
		"frontier", "deep", "balanced", "fast", "auto",
		"claude-fable-5", "claude-sonnet-5", "claude-opus-4-8",
		"claude-3-5-haiku-20241022",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini",
	}
	for _, name := range expected {
		if _, ok := cfg.Models[name]; !ok {
			t.Errorf("default model %q is missing", name)
		}
	}
	for _, unsupported := range []string{"gpt-5.3-codex-spark", "codex-auto-review"} {
		if _, ok := cfg.Models[unsupported]; ok {
			t.Errorf("direct-only model %q must not be advertised as Bravo-capable", unsupported)
		}
	}
}

func TestDefaultTextCandidatesDeclareAnthropicVision(t *testing.T) {
	t.Parallel()

	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatalf("normalizeConfig() error = %v", errNormalize)
	}
	for name, model := range cfg.Models {
		for _, item := range model.Candidates {
			capabilities := newCapabilitySet(item.Capabilities...)
			if _, text := capabilities[capabilityText]; !text {
				continue
			}
			if _, vision := capabilities[capabilityVision]; !vision {
				t.Fatalf("default text candidate %s/%s/%s does not declare vision", name, item.Provider, item.Model)
			}
			if _, fileInput := capabilities[capabilityFileInput]; fileInput {
				t.Fatalf("default text candidate %s/%s/%s unexpectedly declares file_input", name, item.Provider, item.Model)
			}
		}
	}
}

func TestDefaultSmartRoutesPairOnlyCrossProviderEquivalents(t *testing.T) {
	t.Parallel()
	cfg := defaultPluginConfig()
	expected := map[string][]string{
		"opus":   {"claude/claude-opus-4-8", "codex/gpt-5.6-sol"},
		"sol":    {"codex/gpt-5.6-sol", "claude/claude-opus-4-8"},
		"sonnet": {"claude/claude-sonnet-5", "codex/gpt-5.6-terra"},
		"terra":  {"codex/gpt-5.6-terra", "claude/claude-sonnet-5"},
		"haiku":  {"claude/claude-haiku-4-5-20251001", "codex/gpt-5.6-luna"},
		"luna":   {"codex/gpt-5.6-luna", "claude/claude-haiku-4-5-20251001"},
	}
	for route, want := range expected {
		model := cfg.Models[route]
		if len(model.Candidates) != len(want) {
			t.Fatalf("%s candidates = %#v, want exactly %v", route, model.Candidates, want)
		}
		for index, item := range model.Candidates {
			got := normalizeProvider(item.Provider) + "/" + item.Model
			if got != want[index] {
				t.Fatalf("%s candidate %d = %q, want %q", route, index, got, want[index])
			}
		}
	}
}

func TestOlderClaudeExactAliasesKeepOneCodexFallback(t *testing.T) {
	t.Parallel()
	cfg := defaultPluginConfig()
	for _, name := range []string{
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-opus-4-5-20251101",
		"claude-sonnet-4-5-20250929",
		"claude-opus-4-1-20250805",
		"claude-opus-4-20250514",
		"claude-sonnet-4-20250514",
		"claude-3-7-sonnet-20250219",
		"claude-3-5-haiku-20241022",
	} {
		model, exists := cfg.Models[name]
		if !exists {
			t.Fatalf("older exact alias %s is missing", name)
		}
		if len(model.Candidates) != 2 ||
			normalizeProvider(model.Candidates[0].Provider) != "claude" ||
			model.Candidates[0].Model != name ||
			normalizeProvider(model.Candidates[1].Provider) != "codex" {
			t.Fatalf("older exact alias %s = %#v, want exact Claude then one Codex fallback", name, model.Candidates)
		}
	}
}

func TestEveryDefaultGeneralTextRouteIsOneCrossProviderPair(t *testing.T) {
	t.Parallel()
	cfg := defaultPluginConfig()
	for name, model := range cfg.Models {
		if routeClassCapability(model) == capabilityImageGeneration {
			continue
		}
		if len(model.Candidates) != 2 {
			t.Fatalf("default text route %s has %d candidates, want one Claude plus one Codex: %#v", name, len(model.Candidates), model.Candidates)
		}
		providers := map[string]int{}
		for _, item := range model.Candidates {
			providers[normalizeProvider(item.Provider)]++
		}
		if providers["claude"] != 1 || providers["codex"] != 1 || len(providers) != 2 {
			t.Fatalf("default text route %s providers = %#v, want exactly one Claude and one Codex", name, providers)
		}
	}
}

func TestNormalizeConfigMapsUltraToMaxAndRejectsUnknownEffort(t *testing.T) {
	t.Parallel()
	cfg := pluginConfig{
		Models: map[string]logicalModel{
			"test": {Candidates: []candidate{{
				Provider: "claude",
				Model:    "claude-fable-5",
				Effort:   "ultra",
			}}},
		},
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatalf("normalizeConfig() error = %v", errNormalize)
	}
	if got := cfg.Models["test"].Candidates[0].Effort; got != "max" {
		t.Fatalf("effort = %q, want max", got)
	}

	cfg.Models["test"] = logicalModel{Candidates: []candidate{{
		Provider: "claude",
		Model:    "claude-fable-5",
		Effort:   "turbo",
	}}}
	// BaseModels is runtime-only. A caller intentionally replacing Models in a
	// fresh config clears it before asking normalization to establish a new
	// baseline.
	cfg.BaseModels = nil
	if errNormalize := normalizeConfig(&cfg); errNormalize == nil {
		t.Fatal("normalizeConfig() accepted an unknown effort")
	}
}

func TestSmartKeyMatchingAndModelScope(t *testing.T) {
	t.Parallel()
	const plaintext = "brv_test_only_once"
	sum := sha256.Sum256([]byte(plaintext))
	cfg := pluginConfig{SmartKeys: []smartKeyConfig{{
		Name:   "project-a",
		SHA256: hex.EncodeToString(sum[:]),
		Models: []string{"frontier"},
	}}}
	key, ok := matchSmartKey(cfg, plaintext)
	if !ok || key.Name != "project-a" {
		t.Fatalf("matchSmartKey() = %#v/%v", key, ok)
	}
	if _, okWrong := matchSmartKey(cfg, "wrong"); okWrong {
		t.Fatal("matchSmartKey() accepted the wrong key")
	}
	if !smartKeyAllowsModel(key, "frontier") || smartKeyAllowsModel(key, "fast") {
		t.Fatal("smart key model scope is not enforced")
	}
}

func TestRequestCredentialSupportsOpenAIAndClaudeHeaders(t *testing.T) {
	t.Parallel()
	if got := requestCredential(http.Header{"Authorization": []string{"Bearer brv_openai"}}, nil); got != "brv_openai" {
		t.Fatalf("Authorization credential = %q", got)
	}
	if got := requestCredential(http.Header{"X-Api-Key": []string{"brv_claude"}}, nil); got != "brv_claude" {
		t.Fatalf("x-api-key credential = %q", got)
	}
	if got := requestCredential(nil, url.Values{"key": []string{"brv_query"}}); got != "brv_query" {
		t.Fatalf("query credential = %q", got)
	}
}

func TestSmartKeyRoutesUnprefixedExactModel(t *testing.T) {
	const plaintext = "brv_route_exact"
	sum := sha256.Sum256([]byte(plaintext))
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{{
		Name:   "project-route",
		SHA256: hex.EncodeToString(sum[:]),
		Models: []string{"claude-opus-4-8"},
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		fallback := defaultPluginConfig()
		_ = normalizeConfig(&fallback)
		currentConfig.Store(fallback)
	})

	raw, errRoute := routeModel(mustJSONValue(t, rpcModelRouteRequest{
		ModelRouteRequest: pluginapi.ModelRouteRequest{
			RequestedModel:     "claude-opus-4-8",
			Headers:            http.Header{"Authorization": []string{"Bearer " + plaintext}},
			AvailableProviders: []string{"claude", "codex"},
		},
	}))
	if errRoute != nil {
		t.Fatal(errRoute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var response pluginapi.ModelRouteResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !response.Handled || response.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route response = %#v", response)
	}
}

func TestPrefixedModelRoutesToExecutorWithoutSmartKeyForPreciseAuthError(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.RequireSmartKey = true
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		fallback := defaultPluginConfig()
		_ = normalizeConfig(&fallback)
		currentConfig.Store(fallback)
	})

	raw, errRoute := routeModel(mustJSONValue(t, rpcModelRouteRequest{
		ModelRouteRequest: pluginapi.ModelRouteRequest{
			RequestedModel:     "bravo/haiku",
			AvailableProviders: []string{"claude", "codex"},
		},
	}))
	if errRoute != nil {
		t.Fatal(errRoute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("router returned an RPC error instead of delegating auth to executor: %#v", env.Error)
	}
	var response pluginapi.ModelRouteResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !response.Handled || response.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route response = %#v", response)
	}
}

func TestNestedHeadersStripClientSecrets(t *testing.T) {
	t.Parallel()
	headers := sanitizedNestedHeaders(http.Header{
		"Authorization":     []string{"Bearer brv_secret"},
		"X-Api-Key":         []string{"brv_secret"},
		"Anthropic-Version": []string{"2023-06-01"},
	})
	if headers.Get("Authorization") != "" || headers.Get("X-Api-Key") != "" {
		t.Fatal("client credentials survived nested header sanitization")
	}
	if headers.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("protocol header was removed")
	}
}

func mustJSONValue(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func TestRewriteRequestAndResponseModel(t *testing.T) {
	t.Parallel()
	request, errRewrite := rewriteRequestModel(
		[]byte(`{"model":"bravo/frontier","messages":[{"role":"user","content":"hello"}]}`),
		"gpt-5.6-sol(max)",
		false,
	)
	if errRewrite != nil {
		t.Fatal(errRewrite)
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(request, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if root["model"] != "gpt-5.6-sol(max)" {
		t.Fatalf("request model = %#v", root["model"])
	}

	response := rewriteResponseModel(
		[]byte(`{"id":"x","model":"gpt-5.6-sol","nested":{"model":"gpt-5.6-sol"}}`),
		"gpt-5.6-sol(max)",
		"bravo/frontier",
	)
	if strings.Count(string(response), `"model":"bravo/frontier"`) != 2 {
		t.Fatalf("rewritten response = %s", response)
	}
}

func TestRewriteCandidateRequestNormalizesResponsesStringInput(t *testing.T) {
	t.Parallel()
	rewritten, errRewrite := rewriteCandidateRequest(
		[]byte(`{"model":"bravo/fast","input":"hello"}`),
		protocolOpenAIResponse,
		"claude-haiku-4-5-20251001(low)",
		false,
	)
	if errRewrite != nil {
		t.Fatal(errRewrite)
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(rewritten, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	input, ok := root["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("normalized input = %#v", root["input"])
	}
	message, _ := input[0].(map[string]any)
	if message["type"] != "message" {
		t.Fatalf("normalized message type = %#v", message["type"])
	}
	if message["role"] != "user" {
		t.Fatalf("normalized message = %#v", message)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("normalized content = %#v", message["content"])
	}
	textPart, _ := content[0].(map[string]any)
	if textPart["type"] != "input_text" || textPart["text"] != "hello" {
		t.Fatalf("normalized text part = %#v", textPart)
	}
}

func TestStreamModelRewriterHandlesSplitSSEFrame(t *testing.T) {
	t.Parallel()
	rewriter := streamModelRewriter{
		physical: "claude-opus-4-8(high)",
		logical:  "bravo/deep",
	}
	if chunks := rewriter.Push([]byte("event: message_start\ndata: {\"message\":{\"model\":\"claude-")); len(chunks) != 0 {
		t.Fatalf("first partial push emitted %d chunks", len(chunks))
	}
	chunks := rewriter.Push([]byte("opus-4-8\"}}\n\n"))
	if len(chunks) != 1 || !strings.Contains(string(chunks[0]), `"model":"bravo/deep"`) {
		t.Fatalf("rewritten stream chunks = %q", chunks)
	}
}

func TestStreamModelRewriterPreservesOpenAIChatChunkBoundaries(t *testing.T) {
	t.Parallel()
	rewriter := streamModelRewriter{
		physical: "claude-haiku-4-5-20251001(low)",
		logical:  "bravo/haiku",
		protocol: protocolOpenAI,
	}
	first := rewriter.Push([]byte(`{"model":"claude-haiku-4-5-20251001","choices":[{"delta":{"content":"A"}}]}`))
	second := rewriter.Push([]byte(`{"model":"claude-haiku-4-5-20251001","choices":[{"delta":{"content":"B"}}]}`))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("chunk counts = %d, %d", len(first), len(second))
	}
	for _, chunk := range append(first, second...) {
		if !json.Valid(chunk) {
			t.Fatalf("invalid JSON chunk: %q", chunk)
		}
		if strings.Contains(string(chunk), "claude-haiku") || !strings.Contains(string(chunk), `"model":"bravo/haiku"`) {
			t.Fatalf("unexpected rewritten chunk: %s", chunk)
		}
	}
	if tail := rewriter.Flush(); len(tail) != 0 {
		t.Fatalf("unexpected tail: %q", tail)
	}
}

func TestStreamModelRewriterPreservesResponsesChunksWithoutBlankLine(t *testing.T) {
	t.Parallel()
	rewriter := streamModelRewriter{
		physical: "claude-haiku-4-5-20251001(low)",
		logical:  "bravo/haiku",
		protocol: protocolOpenAIResponse,
	}
	first := rewriter.Push([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"claude-haiku-4-5-20251001\"}}\n"))
	second := rewriter.Push([]byte("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"model\":\"claude-haiku-4-5-20251001\"}}\n"))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("chunk counts = %d, %d", len(first), len(second))
	}
	for _, chunk := range append(first, second...) {
		if strings.Contains(string(chunk), "claude-haiku") || !strings.Contains(string(chunk), `"model":"bravo/haiku"`) {
			t.Fatalf("unexpected rewritten chunk: %s", chunk)
		}
		if strings.Contains(string(chunk), "}event:") {
			t.Fatalf("joined events: %s", chunk)
		}
	}
}
