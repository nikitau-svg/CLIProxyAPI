package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectRequestEffortNamedControls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
		want     string
	}{
		{
			name:     "openai chat",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low"}`,
			want:     "low",
		},
		{
			name:     "openai chat nested",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"hello"}],"reasoning":{"effort":"high"}}`,
			want:     "high",
		},
		{
			name:     "responses",
			protocol: protocolOpenAIResponse,
			body:     `{"input":"hello","reasoning":{"effort":"xhigh"}}`,
			want:     "xhigh",
		},
		{
			name:     "claude code",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"max"}}`,
			want:     "max",
		},
		{
			name:     "claude adaptive uses mapped default",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive"}}`,
			want:     "auto",
		},
		{
			name:     "ultra aliases max",
			protocol: protocolOpenAIResponse,
			body:     `{"input":"hello","reasoning":{"effort":"ultra"}}`,
			want:     "max",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			contract, errDetect := detectRequestContract(testCase.protocol, []byte(testCase.body), false)
			if errDetect != nil {
				t.Fatalf("detectRequestContract() error = %v", errDetect)
			}
			assertCapabilities(t, contract, capabilityText)
			assertRequestEffort(t, contract, testCase.want, true)
		})
	}
}

func TestDetectRequestEffortRejectsInvalidAndConflictingControls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
		code     string
	}{
		{
			name:     "invalid type",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":42}`,
			code:     "bravo_effort_invalid",
		},
		{
			name:     "unknown value",
			protocol: protocolOpenAIResponse,
			body:     `{"reasoning":{"effort":"turbo"}}`,
			code:     "bravo_effort_invalid",
		},
		{
			name:     "none remains unverified",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":"none"}`,
			code:     "bravo_contract_unverified",
		},
		{
			name:     "minimal remains unverified",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":"minimal"}`,
			code:     "bravo_contract_unverified",
		},
		{
			name:     "conflicting openai fields",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":"low","reasoning":{"effort":"high"}}`,
			code:     "bravo_effort_conflict",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, errDetect := detectRequestContract(testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errDetect, testCase.code, capabilityReasoning)
		})
	}
}

func TestManualAndReplayReasoningRemainFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		protocol      string
		body          string
		rejectedEarly bool
	}{
		{
			name:          "claude numeric budget",
			protocol:      protocolClaude,
			body:          `{"thinking":{"type":"enabled","budget_tokens":8192}}`,
			rejectedEarly: true,
		},
		{
			name:          "claude enabled without budget",
			protocol:      protocolClaude,
			body:          `{"thinking":{"type":"enabled"}}`,
			rejectedEarly: true,
		},
		{
			name:          "claude disabled",
			protocol:      protocolClaude,
			body:          `{"thinking":{"type":"disabled"}}`,
			rejectedEarly: true,
		},
		{
			name:          "claude adaptive with manual budget",
			protocol:      protocolClaude,
			body:          `{"thinking":{"type":"adaptive","budget_tokens":8192}}`,
			rejectedEarly: true,
		},
		{
			name:          "claude adaptive with unverified display mode",
			protocol:      protocolClaude,
			body:          `{"thinking":{"type":"adaptive","display":"summarized"}}`,
			rejectedEarly: true,
		},
		{
			name:          "claude non-object control",
			protocol:      protocolClaude,
			body:          `{"thinking":"adaptive"}`,
			rejectedEarly: true,
		},
		{
			name:          "responses summary option",
			protocol:      protocolOpenAIResponse,
			body:          `{"reasoning":{"effort":"high","summary":"auto"}}`,
			rejectedEarly: true,
		},
		{
			name:     "reasoning replay item",
			protocol: protocolOpenAIResponse,
			body:     `{"input":[{"type":"reasoning","summary":[]}]}`,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			contract, errDetect := detectRequestContract(testCase.protocol, []byte(testCase.body), false)
			if testCase.rejectedEarly {
				assertContractError(t, errDetect, "bravo_contract_unverified", capabilityReasoning)
				return
			}
			if errDetect != nil {
				t.Fatalf("detectRequestContract() error = %v", errDetect)
			}
			if _, ok := contract.Capabilities[capabilityReasoning]; !ok {
				t.Fatalf("capabilities = %v, want reasoning fail-closed", contract.RequiredCapabilities())
			}
			item := candidate{
				Provider:     "codex",
				Capabilities: append([]string(nil), capabilityOrder...),
			}
			_, errPreflight := preflightCandidateContract(item, testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errPreflight, "bravo_contract_unverified", capabilityReasoning)
		})
	}
}

func TestResolveCandidateEffortUsesPhysicalModelRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		item          candidate
		requested     requestEffort
		wantEffort    string
		wantModel     string
		wantErrorCode string
	}{
		{
			name:       "claude xhigh floors below max gap",
			item:       candidate{Provider: "claude", Model: "claude-sonnet-4-6", Effort: "medium"},
			requested:  requestEffort{Value: "xhigh", Specified: true},
			wantEffort: "high",
			wantModel:  "claude-sonnet-4-6(high)",
		},
		{
			name:       "codex max floors to xhigh",
			item:       candidate{Provider: "codex", Model: "gpt-5.5", Effort: "medium"},
			requested:  requestEffort{Value: "max", Specified: true},
			wantEffort: "xhigh",
			wantModel:  "gpt-5.5(xhigh)",
		},
		{
			name:       "max remains max when registered",
			item:       candidate{Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium"},
			requested:  requestEffort{Value: "max", Specified: true},
			wantEffort: "max",
			wantModel:  "gpt-5.6-sol(max)",
		},
		{
			name:       "budget model delegates named conversion to core",
			item:       candidate{Provider: "claude", Model: "claude-haiku-4-5-20251001", Effort: "low"},
			requested:  requestEffort{Value: "max", Specified: true},
			wantEffort: "max",
			wantModel:  "claude-haiku-4-5-20251001(max)",
		},
		{
			name:          "unknown physical model fails closed",
			item:          candidate{Provider: "codex", Model: "unverified-model", Effort: "medium"},
			requested:     requestEffort{Value: "high", Specified: true},
			wantErrorCode: "bravo_effort_unavailable",
		},
		{
			name:          "known no-thinking model fails closed",
			item:          candidate{Provider: "claude", Model: "claude-3-5-haiku-20241022"},
			requested:     requestEffort{Value: "low", Specified: true},
			wantErrorCode: "bravo_effort_unavailable",
		},
		{
			name:       "auto keeps candidate default without registry lookup",
			item:       candidate{Provider: "codex", Model: "unverified-model", Effort: "medium"},
			requested:  requestEffort{Value: "auto", Specified: true},
			wantEffort: "medium",
			wantModel:  "unverified-model(medium)",
		},
		{
			name:       "unspecified keeps candidate default",
			item:       candidate{Provider: "codex", Model: "unverified-model", Effort: "medium"},
			wantEffort: "medium",
			wantModel:  "unverified-model(medium)",
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolved, errResolve := resolveCandidateEffort(testCase.item, testCase.requested)
			if testCase.wantErrorCode != "" {
				assertContractError(t, errResolve, testCase.wantErrorCode, capabilityReasoning)
				return
			}
			if errResolve != nil {
				t.Fatalf("resolveCandidateEffort() error = %v", errResolve)
			}
			if resolved.Effort != testCase.wantEffort {
				t.Fatalf("resolved effort = %q, want %q", resolved.Effort, testCase.wantEffort)
			}
			if got := candidateModelName(resolved); got != testCase.wantModel {
				t.Fatalf("candidateModelName() = %q, want %q", got, testCase.wantModel)
			}
		})
	}
}

func TestRegisteredLogicalModelAdvertisesOnlyContractPreservingEfforts(t *testing.T) {
	t.Parallel()

	info := registeredLogicalModel(defaultPrefix, "test", logicalModel{
		Candidates: []candidate{{Provider: "codex", Model: "physical-model"}},
	})
	if info.Thinking == nil {
		t.Fatal("registered logical model has no thinking metadata")
	}
	if info.Thinking.Min != 0 || info.Thinking.Max != 0 || info.Thinking.ZeroAllowed {
		t.Fatalf("registered budget/none support = %#v, want named effort only", info.Thinking)
	}
	if !info.Thinking.DynamicAllowed {
		t.Fatal("registered logical model does not advertise Bravo auto effort")
	}
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if len(info.Thinking.Levels) != len(want) {
		t.Fatalf("registered effort levels = %v, want %v", info.Thinking.Levels, want)
	}
	for index := range want {
		if info.Thinking.Levels[index] != want[index] {
			t.Fatalf("registered effort levels = %v, want %v", info.Thinking.Levels, want)
		}
	}
}

func TestStripRequestEffortLeavesOnlySuffixAuthority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
		absent   []string
		present  []string
	}{
		{
			name:     "responses",
			protocol: protocolOpenAIResponse,
			body:     `{"input":"hello","reasoning":{"effort":"high"}}`,
			absent:   []string{`"reasoning_effort"`, `"reasoning"`},
			present:  []string{`"input":"hello"`},
		},
		{
			name:     "claude code",
			protocol: protocolClaude,
			body:     `{"messages":[],"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"high"}}`,
			absent:   []string{`"thinking"`, `"output_config"`},
			present:  []string{`"messages":[]`},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var root map[string]any
			if errUnmarshal := json.Unmarshal([]byte(testCase.body), &root); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			effort, errDetect := detectRequestEffort(testCase.protocol, root)
			if errDetect != nil {
				t.Fatal(errDetect)
			}
			rewritten, errStrip := stripRequestEffort([]byte(testCase.body), testCase.protocol, effort)
			if errStrip != nil {
				t.Fatal(errStrip)
			}
			for _, value := range testCase.absent {
				if strings.Contains(string(rewritten), value) {
					t.Fatalf("rewritten body still contains %s: %s", value, rewritten)
				}
			}
			for _, value := range testCase.present {
				if !strings.Contains(string(rewritten), value) {
					t.Fatalf("rewritten body is missing %s: %s", value, rewritten)
				}
			}
		})
	}
}
