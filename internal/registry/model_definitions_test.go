package registry

import "testing"

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	got := ModelOverrideHeaders("gpt-5.6-luna")
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestGeminiVertexModelsUseFlashLiteReleaseID(t *testing.T) {
	const releaseID = "gemini-3.1-flash-lite"
	const previewID = releaseID + "-preview"

	for _, model := range GetGeminiVertexModels() {
		if model == nil {
			continue
		}
		if model.ID == previewID {
			t.Fatalf("Vertex model ID = %q, want release ID %q", model.ID, releaseID)
		}
		if model.ID == releaseID {
			return
		}
	}

	t.Fatalf("Vertex models do not contain %q", releaseID)
}

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}

func TestWithClaudeBuiltinsReplacesStaleContractMetadataWithoutDuplicates(t *testing.T) {
	models := WithClaudeBuiltins([]*ModelInfo{
		{ID: "remote-claude-model", Type: "claude"},
		{
			ID:                  claudeBuiltinOpus5ModelID,
			Type:                "claude",
			ContextLength:       1,
			MaxCompletionTokens: 1,
			Thinking:            &ThinkingSupport{},
		},
	})

	seenOpus5 := 0
	for _, model := range models {
		if model == nil || model.ID != claudeBuiltinOpus5ModelID {
			continue
		}
		seenOpus5++
		if model.Created != 0 {
			t.Fatalf("Claude Opus 5 created = %d, want 0 until an official release timestamp is recorded", model.Created)
		}
		if model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 {
			t.Fatalf("Claude Opus 5 limits = context %d output %d, want 1000000/128000", model.ContextLength, model.MaxCompletionTokens)
		}
		if model.Thinking == nil || !model.Thinking.DefaultOn || !model.Thinking.ZeroAllowed ||
			!model.Thinking.DynamicAllowed || model.Thinking.MaxDisableLevel != "high" {
			t.Fatalf("Claude Opus 5 thinking policy = %+v", model.Thinking)
		}
	}
	if seenOpus5 != 1 {
		t.Fatalf("Claude Opus 5 entries = %d, want 1", seenOpus5)
	}
}

func TestClaudeBuiltinsSurviveRemoteCatalogWithoutOpus5(t *testing.T) {
	original := getModels()
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &staticModelsJSON{
		Claude: []*ModelInfo{{
			ID:   "remote-only-claude-model",
			Type: "claude",
		}},
	}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	found := false
	for _, model := range GetClaudeModels() {
		if model != nil && model.ID == claudeBuiltinOpus5ModelID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("GetClaudeModels omitted pinned Claude Opus 5 after remote catalog replacement")
	}

	model := LookupStaticModelInfo(claudeBuiltinOpus5ModelID)
	if model == nil || model.Thinking == nil || !model.Thinking.DefaultOn || model.Thinking.MaxDisableLevel != "high" {
		t.Fatalf("LookupStaticModelInfo Claude Opus 5 = %+v", model)
	}
}

func TestClaude5BuiltinThinkingPolicies(t *testing.T) {
	tests := []struct {
		modelID        string
		zeroAllowed    bool
		maxDisable     string
		dynamicAllowed bool
	}{
		{modelID: claudeBuiltinOpus5ModelID, zeroAllowed: true, maxDisable: "high", dynamicAllowed: true},
		{modelID: claudeBuiltinSonnet5ModelID, zeroAllowed: true, dynamicAllowed: true},
		{modelID: claudeBuiltinFable5ModelID, zeroAllowed: false, dynamicAllowed: true},
	}

	for _, testCase := range tests {
		model := LookupStaticModelInfo(testCase.modelID)
		if model == nil || model.Thinking == nil {
			t.Fatalf("%s static thinking metadata is missing", testCase.modelID)
		}
		if !model.Thinking.DefaultOn || model.Thinking.ZeroAllowed != testCase.zeroAllowed ||
			model.Thinking.DynamicAllowed != testCase.dynamicAllowed ||
			model.Thinking.MaxDisableLevel != testCase.maxDisable {
			t.Fatalf("%s thinking policy = %+v", testCase.modelID, model.Thinking)
		}
	}
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}
