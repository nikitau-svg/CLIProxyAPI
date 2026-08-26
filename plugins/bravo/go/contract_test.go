package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDetectRequestContractTextAcrossProtocols(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{name: "openai", protocol: protocolOpenAI, body: `{"messages":[{"role":"user","content":"hello"}]}`},
		{name: "responses", protocol: protocolOpenAIResponse, body: `{"input":"hello"}`},
		{name: "claude", protocol: protocolClaude, body: `{"messages":[{"role":"user","content":"hello"}]}`},
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
		})
	}
}

func TestDetectRequestContractOpenAI(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"stream": true,
		"background": true,
		"reasoning_effort": "high",
		"response_format": {"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}},
		"tools": [{"type":"web_search_preview"}],
		"tool_choice": {"type":"web_search_preview"},
		"messages": [
			{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},
			{"role":"tool","tool_call_id":"call_1","content":"done"}
		]
	}`)
	contract, errDetect := detectRequestContract(protocolOpenAI, body, false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract,
		capabilityText,
		capabilityTools,
		capabilityToolResult,
		capabilityVision,
		capabilityWebSearch,
		capabilityStructuredOutput,
		capabilityBackground,
		capabilityStream,
	)
	assertRequestEffort(t, contract, "high", true)
	if !contract.ForcedToolChoice {
		t.Fatal("ForcedToolChoice = false, want true")
	}
}

func TestOpenAIChatJSONModeIsAdvisoryButJSONSchemaRemainsStrict(t *testing.T) {
	t.Parallel()

	jsonObject, errDetect := detectRequestContract(
		protocolOpenAI,
		[]byte(`{"messages":[{"role":"user","content":"sync"}],"response_format":{"type":"json_object"}}`),
		false,
	)
	if errDetect != nil {
		t.Fatalf("detect json_object contract: %v", errDetect)
	}
	assertCapabilities(t, jsonObject, capabilityText)

	for _, provider := range []string{"claude", "codex"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			_, errPreflight := preflightCandidateContract(
				candidate{Provider: provider, Capabilities: []string{capabilityText}},
				protocolOpenAI,
				[]byte(`{"messages":[{"role":"user","content":"sync"}],"response_format":{"type":"json_object"}}`),
				false,
			)
			if errPreflight != nil {
				t.Fatalf("json_object preflight for %s: %v", provider, errPreflight)
			}
		})
	}

	jsonSchema, errDetect := detectRequestContract(
		protocolOpenAI,
		[]byte(`{"messages":[{"role":"user","content":"sync"}],"response_format":{"type":"json_schema","json_schema":{"name":"sync","schema":{"type":"object"}}}}`),
		false,
	)
	if errDetect != nil {
		t.Fatalf("detect json_schema contract: %v", errDetect)
	}
	assertCapabilities(t, jsonSchema, capabilityText, capabilityStructuredOutput)
}

func TestDetectRequestContractOpenAIResponse(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"reasoning":{"effort":"medium"},
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}},
		"tools":[{"type":"web_search","external_web_access":true}],
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},
			{"type":"function_call_output","call_id":"call_1","output":"done"},
			{"type":"reasoning","summary":[]}
		]
	}`)
	contract, errDetect := detectRequestContract(protocolOpenAIResponse, body, true)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract,
		capabilityText,
		capabilityTools,
		capabilityToolResult,
		capabilityReasoning,
		capabilityVision,
		capabilityWebSearch,
		capabilityStructuredOutput,
		capabilityStream,
	)
	assertRequestEffort(t, contract, "medium", true)
}

func TestDetectRequestContractClaude(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{
			"effort":"high",
			"format":{"type":"json_schema","schema":{"type":"object"}}
		},
		"tools":[{"type":"web_search_20260209","name":"web_search"}],
		"tool_choice":{"type":"any"},
		"messages":[{
			"role":"user",
			"content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},
				{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}
			]
		}]
	}`)
	contract, errDetect := detectRequestContract(protocolClaude, body, false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract,
		capabilityText,
		capabilityTools,
		capabilityToolResult,
		capabilityVision,
		capabilityWebSearch,
		capabilityStructuredOutput,
		capabilityStream,
	)
	assertRequestEffort(t, contract, "high", true)
	if !contract.ForcedToolChoice {
		t.Fatal("ForcedToolChoice = false, want true")
	}
}

func TestDetectRequestContractNestedToolResultImageRequiresVision(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"},
		"messages":[{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_1",
				"content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},
					{"type":"text","text":"screenshot"}
				]
			}]
		}]
	}`)
	contract, errDetect := detectRequestContract(protocolClaude, body, false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract,
		capabilityText,
		capabilityTools,
		capabilityToolResult,
		capabilityVision,
		capabilityStream,
	)
	assertRequestEffort(t, contract, "xhigh", true)
}

func TestDetectRequestContractKeepsFileInputsSeparateFromVision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "OpenAI chat file",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:text/plain;base64,QQ=="}}]}]}`,
		},
		{
			name:     "OpenAI Responses input file",
			protocol: protocolOpenAIResponse,
			body:     `{"input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,AA=="}]}]}`,
		},
		{
			name:     "Claude document",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="}}]}]}`,
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
			assertCapabilities(t, contract, capabilityText, capabilityFileInput)
		})
	}
}

func TestDetectRequestContractDoesNotTreatFunctionNamedWebSearchAsBuiltin(t *testing.T) {
	t.Parallel()

	contract, errDetect := detectRequestContract(protocolOpenAI, []byte(`{
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object"}}}]
	}`), false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract, capabilityText, capabilityTools)
}

func TestDetectRequestContractWebSearchFiltersFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "openai allowed domains",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_preview","allowed_domains":["example.com"]}]}`,
		},
		{
			name:     "responses filters",
			protocol: protocolOpenAIResponse,
			body:     `{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`,
		},
		{
			name:     "claude blocked domains",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","blocked_domains":["example.com"]}]}`,
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
			assertCapabilities(t, contract,
				capabilityText,
				capabilityTools,
				capabilityWebSearch,
				capabilityWebSearchFilters,
			)
		})
	}

	// Keep this assertion independent of whether ordinary web search has been
	// promoted in the live matrix: domain restrictions need their own evidence.
	contract := requestCapabilityContract{
		Protocol:     protocolOpenAI,
		Capabilities: newCapabilitySet(capabilityWebSearchFilters),
	}
	errVerify := verifyProviderContract("codex", []string{capabilityWebSearchFilters}, contract)
	assertContractError(t, errVerify, "bravo_contract_unverified", capabilityWebSearchFilters)
}

func TestDetectRequestContractUnknownProviderToolsFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "openai file search",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"file_search"}]}`,
		},
		{
			name:     "responses code interpreter",
			protocol: protocolOpenAIResponse,
			body:     `{"input":"run","tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`,
		},
		{
			name:     "claude computer",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":"click"}],"tools":[{"type":"computer_20250124","name":"computer"}]}`,
		},
		{
			name:     "openai missing type",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"run"}],"tools":[{"name":"unknown"}]}`,
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
			assertCapabilities(t, contract, capabilityText, capabilityTools, capabilityProviderTool)

			item := candidate{
				Provider:     "codex",
				Capabilities: []string{capabilityText, capabilityTools, capabilityProviderTool},
			}
			_, errPreflight := preflightCandidateContract(item, testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errPreflight, "bravo_contract_unverified", capabilityProviderTool)
		})
	}
}

func TestDetectRequestContractAllowsNamespaceAndClaudeCustomTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "openai namespace",
			protocol: protocolOpenAI,
			body:     `{"messages":[{"role":"user","content":"run"}],"tools":[{"type":"namespace","name":"repo","tools":[{"type":"function","function":{"name":"status"}}]}]}`,
		},
		{
			name:     "claude custom function",
			protocol: protocolClaude,
			body:     `{"messages":[{"role":"user","content":"run"}],"tools":[{"name":"status","input_schema":{"type":"object"}}]}`,
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
			assertCapabilities(t, contract, capabilityText, capabilityTools)
		})
	}
}

func TestDetectRequestContractImageToolRequiresImageEndpoint(t *testing.T) {
	t.Parallel()

	body := []byte(`{"input":"draw","tools":[{"type":"image_generation"}]}`)
	contract, errDetect := detectRequestContract(protocolOpenAIResponse, body, false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract, capabilityText, capabilityTools, capabilityImageGeneration)

	item := candidate{
		Provider:     "codex",
		Capabilities: []string{capabilityText, capabilityTools, capabilityImageGeneration},
	}
	_, errPreflight := preflightCandidateContract(item, protocolOpenAIResponse, body, false)
	assertContractError(t, errPreflight, "bravo_contract_unverified", capabilityImageGeneration)
}

func TestDetectRequestContractPlainTextFormatIsNotStructured(t *testing.T) {
	t.Parallel()

	contract, errDetect := detectRequestContract(protocolOpenAIResponse, []byte(`{
		"input":"hello",
		"text":{"format":{"type":"text"}}
	}`), false)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, contract, capabilityText)
}

func TestPreflightCandidateContractAllowsLiveCells(t *testing.T) {
	t.Parallel()

	protocolBodies := map[string]string{
		protocolOpenAI: `{
			"stream":true,
			"tools":[{"type":"function","function":{"name":"lookup"}}],
			"messages":[{"role":"tool","tool_call_id":"call_1","content":"done"}]
		}`,
		protocolOpenAIResponse: `{
			"stream":true,
			"tools":[{"type":"function","name":"lookup"}],
			"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]
		}`,
		protocolClaude: `{
			"stream":true,
			"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]
		}`,
	}
	for _, provider := range []string{"claude", "codex"} {
		provider := provider
		for protocol, body := range protocolBodies {
			protocol, body := protocol, body
			t.Run(provider+"/"+protocol, func(t *testing.T) {
				t.Parallel()
				item := candidate{
					Provider: provider,
					Capabilities: []string{
						capabilityText,
						capabilityTools,
						capabilityToolResult,
						capabilityStream,
					},
				}
				contract, errPreflight := preflightCandidateContract(item, protocol, []byte(body), false)
				if errPreflight != nil {
					t.Fatalf("preflightCandidateContract() error = %v", errPreflight)
				}
				assertCapabilities(t, contract,
					capabilityText,
					capabilityTools,
					capabilityToolResult,
					capabilityStream,
				)
			})
		}
	}
}

func TestPreflightCandidateContractAllowsLiveWebSearch(t *testing.T) {
	t.Parallel()

	protocolBodies := map[string]string{
		protocolOpenAI:         `{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search"}]}`,
		protocolOpenAIResponse: `{"input":"search","tools":[{"type":"web_search"}]}`,
		protocolClaude:         `{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
	}
	for _, provider := range []string{"claude", "codex"} {
		provider := provider
		for protocol, body := range protocolBodies {
			protocol, body := protocol, body
			t.Run(provider+"/"+protocol, func(t *testing.T) {
				t.Parallel()
				item := candidate{
					Provider: provider,
					Capabilities: []string{
						capabilityText,
						capabilityTools,
						capabilityWebSearch,
					},
				}
				contract, errPreflight := preflightCandidateContract(item, protocol, []byte(body), false)
				if errPreflight != nil {
					t.Fatalf("preflightCandidateContract() error = %v", errPreflight)
				}
				assertCapabilities(t, contract, capabilityText, capabilityTools, capabilityWebSearch)
			})
		}
	}
}

func TestPreflightCandidateContractAllowsLiveAnthropicVision(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"},
		"messages":[{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_1",
				"content":[
					{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AA=="}},
					{"type":"text","text":"screenshot"}
				]
			}]
		}]
	}`)
	for _, provider := range []string{"claude", "codex"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			item := candidate{
				Provider: provider,
				Model:    map[string]string{"claude": "claude-opus-4-8", "codex": "gpt-5.6-sol"}[provider],
				Capabilities: []string{
					capabilityText,
					capabilityTools,
					capabilityToolResult,
					capabilityVision,
					capabilityStream,
				},
			}
			contract, errPreflight := preflightCandidateContract(item, protocolClaude, body, true)
			if errPreflight != nil {
				t.Fatalf("preflightCandidateContract() error = %v", errPreflight)
			}
			assertCapabilities(t, contract,
				capabilityText,
				capabilityTools,
				capabilityToolResult,
				capabilityVision,
				capabilityStream,
			)
			assertRequestEffort(t, contract, "xhigh", true)
		})
	}
}

func TestPreflightCandidateContractAllowsLiveOpenAIChatVision(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"inspect"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}
		]}]
	}`)
	for _, provider := range []string{"claude", "codex"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			item := candidate{
				Provider: provider,
				Capabilities: []string{
					capabilityText,
					capabilityVision,
				},
			}
			contract, errPreflight := preflightCandidateContract(item, protocolOpenAI, body, false)
			if errPreflight != nil {
				t.Fatalf("preflightCandidateContract() error = %v", errPreflight)
			}
			assertCapabilities(t, contract, capabilityText, capabilityVision)
		})
	}
}

func TestPreflightCandidateContractRejectsUndeclaredCapability(t *testing.T) {
	t.Parallel()

	item := candidate{Provider: "claude", Capabilities: []string{capabilityText}}
	_, errPreflight := preflightCandidateContract(
		item,
		protocolOpenAI,
		[]byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`),
		false,
	)
	assertContractError(t, errPreflight, "bravo_capability_undeclared", capabilityTools)
}

func TestPreflightCandidateContractRejectsUnverifiedAdvancedCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		protocol   string
		body       string
		capability string
	}{
		{name: "reasoning replay", protocol: protocolOpenAI, body: `{"messages":[{"role":"assistant","reasoning_content":"prior trace"}]}`, capability: capabilityReasoning},
		{name: "vision", protocol: protocolOpenAIResponse, body: `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`, capability: capabilityVision},
		{name: "background", protocol: protocolOpenAIResponse, body: `{"input":"hello","background":true}`, capability: capabilityBackground},
	}
	allCapabilities := append([]string(nil), capabilityOrder...)
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			item := candidate{Provider: "claude", Capabilities: allCapabilities}
			_, errPreflight := preflightCandidateContract(item, testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errPreflight, "bravo_contract_unverified", testCase.capability)
		})
	}
}

func TestPreflightLogicalHaikuAllowsReasoningExtractionStructuredOutput(t *testing.T) {
	t.Parallel()

	model := defaultPluginConfig().Models["haiku"]
	body := []byte(`{
		"model":"bravo/haiku",
		"stream":true,
		"messages":[{"role":"user","content":"extract"}],
		"output_config":{
			"format":{
				"type":"json_schema",
				"schema":{
					"type":"object",
					"properties":{"answer":{"type":"string"}},
					"required":["answer"],
					"additionalProperties":false
				}
			}
		}
	}`)
	if errPreflight := preflightLogicalModelContract(model, protocolClaude, body, true); errPreflight != nil {
		t.Fatalf("reasoning-extraction logical preflight: %v", errPreflight)
	}
	contract, errDetect := detectRequestContract(protocolClaude, body, true)
	if errDetect != nil {
		t.Fatalf("detect reasoning-extraction contract: %v", errDetect)
	}
	for _, item := range model.Candidates {
		if _, errResolve := resolveCandidateContract(item, contract); errResolve != nil {
			t.Errorf("%s/%s cannot preserve structured output: %v", item.Provider, item.Model, errResolve)
		}
	}
}

func TestDetectClaudeStructuredOutputRejectsUnverifiedFormatType(t *testing.T) {
	t.Parallel()

	_, errDetect := detectRequestContract(protocolClaude, []byte(`{
		"messages":[{"role":"user","content":"extract"}],
		"output_config":{"format":{"type":"future_schema","schema":{"type":"object"}}}
	}`), false)
	assertContractError(t, errDetect, "bravo_contract_unverified", capabilityStructuredOutput)
}

func TestDetectClaudeStructuredOutputRequiresSchemaObject(t *testing.T) {
	t.Parallel()

	_, errDetect := detectRequestContract(protocolClaude, []byte(`{
		"messages":[{"role":"user","content":"extract"}],
		"output_config":{"format":{"type":"json_schema","schema":"not-an-object"}}
	}`), false)
	assertContractError(t, errDetect, "bravo_request_invalid", "")
}

func TestPreflightCandidateContractRejectsClaudeReasoningWithForcedToolChoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "openai required",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":"high","tool_choice":"required","tools":[{"type":"function","function":{"name":"lookup"}}]}`,
		},
		{
			name:     "responses named function",
			protocol: protocolOpenAIResponse,
			body:     `{"reasoning":{"effort":"high"},"tool_choice":{"type":"function","name":"lookup"},"tools":[{"type":"function","name":"lookup"}]}`,
		},
		{
			name:     "claude any",
			protocol: protocolClaude,
			body:     `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tool_choice":{"type":"any"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			item := candidate{Provider: "claude", Capabilities: append([]string(nil), capabilityOrder...)}
			_, errPreflight := preflightCandidateContract(item, testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errPreflight, "bravo_capability_conflict", capabilityReasoning)
		})
	}
}

func TestPreflightCandidateContractAutoToolChoiceHasNoClaudeConflict(t *testing.T) {
	t.Parallel()

	item := candidate{Provider: "claude", Capabilities: append([]string(nil), capabilityOrder...)}
	contract, errPreflight := preflightCandidateContract(
		item,
		protocolClaude,
		[]byte(`{"thinking":{"type":"adaptive"},"tool_choice":{"type":"auto"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`),
		false,
	)
	if errPreflight != nil {
		t.Fatalf("preflightCandidateContract() error = %v", errPreflight)
	}
	assertCapabilities(t, contract, capabilityText, capabilityTools)
	assertRequestEffort(t, contract, "auto", true)
}

func TestResolveCandidateContractDropsImplicitClaudeEffortForForcedFunctionTools(t *testing.T) {
	t.Parallel()

	item := candidate{
		Provider:     "claude",
		Model:        "claude-haiku-4-5-20251001",
		Effort:       "low",
		Capabilities: []string{capabilityText, capabilityTools},
	}
	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "OpenAI named function",
			protocol: protocolOpenAI,
			body:     `{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		},
		{
			name:     "Anthropic named tool",
			protocol: protocolClaude,
			body:     `{"tool_choice":{"type":"tool","name":"lookup"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			contract, errDetect := detectRequestContract(
				testCase.protocol,
				[]byte(testCase.body),
				false,
			)
			if errDetect != nil {
				t.Fatalf("detectRequestContract() error = %v", errDetect)
			}
			resolved, errResolve := resolveCandidateContract(item, contract)
			if errResolve != nil {
				t.Fatalf("resolveCandidateContract() error = %v", errResolve)
			}
			if resolved.Effort != "" {
				t.Fatalf("resolved effort = %q, want empty implicit effort", resolved.Effort)
			}
		})
	}
}

func TestResolveCandidateContractHandlesDefaultOnClaudeForcedTools(t *testing.T) {
	t.Parallel()

	contract, errDetect := detectRequestContract(
		protocolClaude,
		[]byte(`{"tool_choice":{"type":"any"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`),
		false,
	)
	if errDetect != nil {
		t.Fatal(errDetect)
	}

	t.Run("Opus 5 may explicitly disable implicit thinking", func(t *testing.T) {
		item := candidate{
			Provider:     "claude",
			Model:        "claude-opus-5",
			Effort:       "high",
			Capabilities: []string{capabilityText, capabilityTools},
		}
		resolved, errResolve := resolveCandidateContract(item, contract)
		if errResolve != nil {
			t.Fatalf("resolveCandidateContract() error = %v", errResolve)
		}
		if resolved.Effort != "" {
			t.Fatalf("resolved effort = %q, want implicit thinking disabled", resolved.Effort)
		}
	})

	t.Run("Fable 5 fails closed because thinking cannot be disabled", func(t *testing.T) {
		item := candidate{
			Provider:     "claude",
			Model:        "claude-fable-5",
			Effort:       "max",
			Capabilities: []string{capabilityText, capabilityTools},
		}
		_, errResolve := resolveCandidateContract(item, contract)
		assertContractError(t, errResolve, "bravo_capability_conflict", capabilityTools)
	})

	t.Run("unknown Claude policy fails closed", func(t *testing.T) {
		item := candidate{
			Provider:     "claude",
			Model:        "claude-future-unverified",
			Effort:       "high",
			Capabilities: []string{capabilityText, capabilityTools},
		}
		_, errResolve := resolveCandidateContract(item, contract)
		assertContractError(t, errResolve, "bravo_contract_unverified", capabilityTools)
	})
}

func TestResolveCandidateContractKeepsExplicitClaudeForcedToolConflict(t *testing.T) {
	t.Parallel()

	item := candidate{
		Provider:     "claude",
		Model:        "claude-haiku-4-5-20251001",
		Effort:       "low",
		Capabilities: []string{capabilityText, capabilityTools, capabilityReasoning},
	}
	tests := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "OpenAI explicit effort",
			protocol: protocolOpenAI,
			body:     `{"reasoning_effort":"low","tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		},
		{
			name:     "Anthropic adaptive effort",
			protocol: protocolClaude,
			body:     `{"thinking":{"type":"adaptive"},"tool_choice":{"type":"tool","name":"lookup"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			contract, errDetect := detectRequestContract(
				testCase.protocol,
				[]byte(testCase.body),
				false,
			)
			if errDetect != nil {
				t.Fatalf("detectRequestContract() error = %v", errDetect)
			}
			_, errResolve := resolveCandidateContract(item, contract)
			assertContractError(t, errResolve, "bravo_capability_conflict", capabilityReasoning)
		})
	}
}

func TestPreflightCandidateContractAdaptiveAutoUsesMappedEffortForClaudeForcedToolConflict(t *testing.T) {
	t.Parallel()

	item := candidate{
		Provider:     "claude",
		Effort:       "high",
		Capabilities: []string{capabilityText, capabilityTools},
	}
	for _, body := range []string{
		`{"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
	} {
		_, errPreflight := preflightCandidateContract(item, protocolClaude, []byte(body), false)
		assertContractError(t, errPreflight, "bravo_capability_conflict", capabilityReasoning)
	}
}

func TestDetectRequestContractFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		body     string
		code     string
	}{
		{name: "unknown protocol", protocol: "gemini", body: `{}`, code: "bravo_protocol_unsupported"},
		{name: "empty body", protocol: protocolOpenAI, body: "", code: "bravo_request_invalid"},
		{name: "invalid json", protocol: protocolClaude, body: `{`, code: "bravo_request_invalid"},
		{name: "json null", protocol: protocolOpenAIResponse, body: `null`, code: "bravo_request_invalid"},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, errDetect := detectRequestContract(testCase.protocol, []byte(testCase.body), false)
			assertContractError(t, errDetect, testCase.code, "")
		})
	}
}

func TestVerifyProviderContractRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	contract := requestCapabilityContract{
		Protocol:     protocolOpenAI,
		Capabilities: newCapabilitySet(capabilityText),
	}
	errVerify := verifyProviderContract("gemini", []string{capabilityText}, contract)
	assertContractError(t, errVerify, "bravo_provider_unsupported", "")
}

func TestVerifyLogicalModelContractAggregatesCandidateFailures(t *testing.T) {
	t.Parallel()

	model := logicalModel{Candidates: []candidate{
		{Provider: "claude", Capabilities: []string{capabilityText}},
		{Provider: "codex", Capabilities: []string{capabilityText}},
	}}
	contract := requestCapabilityContract{
		Protocol:     protocolClaude,
		Capabilities: newCapabilitySet(capabilityText, capabilityFileInput),
	}
	errVerify := verifyLogicalModelContract(model, contract)
	var contractErr *capabilityContractError
	if !errors.As(errVerify, &contractErr) {
		t.Fatalf("error type = %T, want *capabilityContractError: %v", errVerify, errVerify)
	}
	if contractErr.Code != "bravo_capability_undeclared" || contractErr.Capability != capabilityFileInput {
		t.Fatalf("error = %#v, want aggregated file_input capability error", contractErr)
	}
	if contractErr.Provider != "" {
		t.Fatalf("aggregated error provider = %q, want empty", contractErr.Provider)
	}
	if strings.Contains(contractErr.Error(), "candidate ") ||
		!strings.Contains(contractErr.Error(), "claude, codex") {
		t.Fatalf("aggregated error message = %q", contractErr.Error())
	}
}

func assertCapabilities(t *testing.T, contract requestCapabilityContract, expected ...string) {
	t.Helper()
	actual := contract.RequiredCapabilities()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("capabilities = %v, want %v", actual, expected)
	}
}

func assertRequestEffort(t *testing.T, contract requestCapabilityContract, value string, specified bool) {
	t.Helper()
	if contract.Effort.Value != value || contract.Effort.Specified != specified {
		t.Fatalf("effort = %#v, want value=%q specified=%t", contract.Effort, value, specified)
	}
}

func assertContractError(t *testing.T, err error, code, capability string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", code)
	}
	var contractErr *capabilityContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("error type = %T, want *capabilityContractError: %v", err, err)
	}
	if contractErr.Code != code {
		t.Fatalf("error code = %q, want %q: %v", contractErr.Code, code, err)
	}
	if capability != "" && contractErr.Capability != capability {
		t.Fatalf("error capability = %q, want %q: %v", contractErr.Capability, capability, err)
	}
}
