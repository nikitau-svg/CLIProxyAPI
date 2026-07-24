package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

const (
	capabilityText             = "text"
	capabilityTools            = "tools"
	capabilityToolResult       = "tool_result"
	capabilityReasoning        = "reasoning"
	capabilityVision           = "vision"
	capabilityWebSearch        = "web_search"
	capabilityWebSearchFilters = "web_search_filters"
	capabilityProviderTool     = "provider_tool"
	capabilityStructuredOutput = "structured_output"
	capabilityBackground       = "background"
	capabilityFileInput        = "file_input"
	capabilityImageGeneration  = "image_generation"
	capabilityStream           = "stream"
)

const (
	protocolOpenAI         = "openai"
	protocolOpenAIResponse = "openai-response"
	protocolClaude         = "claude"
	protocolOpenAIImage    = "openai-image"
)

type providerProtocol struct {
	Provider string
	Protocol string
}

// liveCapabilityMatrix contains only contracts exercised against a live
// upstream. Capabilities are promoted independently so unrelated CODE evidence
// cannot widen a provider contract.
var liveCapabilityMatrix = map[providerProtocol]capabilitySet{
	{Provider: "claude", Protocol: protocolOpenAI}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "claude", Protocol: protocolOpenAIResponse}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "claude", Protocol: protocolClaude}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityReasoning:  {},
		capabilityVision:     {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "codex", Protocol: protocolOpenAI}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "codex", Protocol: protocolOpenAIResponse}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "codex", Protocol: protocolClaude}: {
		capabilityText:       {},
		capabilityTools:      {},
		capabilityToolResult: {},
		capabilityVision:     {},
		capabilityWebSearch:  {},
		capabilityStream:     {},
	},
	{Provider: "codex", Protocol: protocolOpenAIImage}: {
		capabilityImageGeneration: {},
	},
}

// Vision through Anthropic Messages is live-verified for Claude and Codex,
// including nested tool-result images, adaptive effort, and streaming. OpenAI
// image generation and edit are also live-verified. Image-generation streaming
// remains absent until its upstream response framing is verified as valid SSE.

var capabilityOrder = []string{
	capabilityText,
	capabilityTools,
	capabilityToolResult,
	capabilityProviderTool,
	capabilityReasoning,
	capabilityVision,
	capabilityWebSearch,
	capabilityWebSearchFilters,
	capabilityStructuredOutput,
	capabilityBackground,
	capabilityFileInput,
	capabilityImageGeneration,
	capabilityStream,
}

type requestCapabilityContract struct {
	Protocol         string
	Capabilities     capabilitySet
	ForcedToolChoice bool
	Effort           requestEffort
}

func (c requestCapabilityContract) RequiredCapabilities() []string {
	return orderedCapabilities(c.Capabilities)
}

type capabilityContractError struct {
	Code       string
	Provider   string
	Protocol   string
	Capability string
	Message    string
}

func (e *capabilityContractError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// detectRequestContract derives the semantic contract from a client request.
// streaming is supplied by the caller because stream execution can be selected
// by the endpoint even when a translated body omits the stream field.
func detectRequestContract(protocol string, body []byte, streaming bool) (requestCapabilityContract, error) {
	protocol = normalizeContractProtocol(protocol)
	if protocol == "" {
		return requestCapabilityContract{}, &capabilityContractError{
			Code:     "bravo_protocol_unsupported",
			Protocol: strings.TrimSpace(protocol),
			Message:  "Bravo does not support the requested protocol",
		}
	}
	if protocol == protocolOpenAIImage {
		return detectOpenAIImageContract(body, streaming)
	}

	var root map[string]any
	if len(body) == 0 {
		return requestCapabilityContract{}, invalidContractPayload(protocol, "request body is empty")
	}
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return requestCapabilityContract{}, invalidContractPayload(protocol, "request body is not a JSON object")
	}
	if root == nil {
		return requestCapabilityContract{}, invalidContractPayload(protocol, "request body is not a JSON object")
	}

	contract := requestCapabilityContract{
		Protocol:     protocol,
		Capabilities: newCapabilitySet(capabilityText),
	}
	effort, errEffort := detectRequestEffort(protocol, root)
	if errEffort != nil {
		return requestCapabilityContract{}, errEffort
	}
	contract.Effort = effort
	if streaming || boolField(root, "stream") {
		contract.Capabilities[capabilityStream] = struct{}{}
	}
	if boolField(root, "background") {
		contract.Capabilities[capabilityBackground] = struct{}{}
	}

	inspectTools(root["tools"], protocol, contract.Capabilities)
	if choice, exists := root["tool_choice"]; exists && meaningfulToolChoice(choice) {
		contract.Capabilities[capabilityTools] = struct{}{}
		if toolChoiceIsWebSearch(choice) {
			contract.Capabilities[capabilityWebSearch] = struct{}{}
		}
		contract.ForcedToolChoice = forcedToolChoice(protocol, choice)
	}

	switch protocol {
	case protocolOpenAI:
		inspectOpenAIChat(root, contract.Capabilities)
	case protocolOpenAIResponse:
		inspectOpenAIResponse(root, contract.Capabilities)
	case protocolClaude:
		inspectClaude(root, contract.Capabilities)
	}

	if _, hasResult := contract.Capabilities[capabilityToolResult]; hasResult {
		contract.Capabilities[capabilityTools] = struct{}{}
	}
	return contract, nil
}

func detectOpenAIImageContract(body []byte, streaming bool) (requestCapabilityContract, error) {
	if len(body) == 0 {
		return requestCapabilityContract{}, invalidContractPayload(protocolOpenAIImage, "request body is empty")
	}

	contract := requestCapabilityContract{
		Protocol:     protocolOpenAIImage,
		Capabilities: newCapabilitySet(capabilityImageGeneration),
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal == nil {
		if root == nil {
			return requestCapabilityContract{}, invalidContractPayload(protocolOpenAIImage, "request body is not a JSON object")
		}
		if boolField(root, "stream") {
			streaming = true
		}
	} else if len(body) < 2 || body[0] != '-' || body[1] != '-' {
		return requestCapabilityContract{}, invalidContractPayload(protocolOpenAIImage, "request body is neither a JSON object nor multipart form data")
	}
	if streaming {
		contract.Capabilities[capabilityStream] = struct{}{}
	}
	return contract, nil
}

// preflightCandidateContract is the execution-facing pure preflight. A
// capability must be both declared by the candidate and live-verified for the
// provider/protocol pair.
func preflightCandidateContract(item candidate, protocol string, body []byte, streaming bool) (requestCapabilityContract, error) {
	contract, errDetect := detectRequestContract(protocol, body, streaming)
	if errDetect != nil {
		return requestCapabilityContract{}, errDetect
	}
	if _, errVerify := resolveCandidateContract(item, contract); errVerify != nil {
		return contract, errVerify
	}
	return contract, nil
}

func preflightLogicalModelContract(model logicalModel, protocol string, body []byte, streaming bool) error {
	contract, errDetect := detectRequestContract(protocol, body, streaming)
	if errDetect != nil {
		return errDetect
	}
	return verifyLogicalModelContract(model, contract)
}

func verifyLogicalModelContract(model logicalModel, contract requestCapabilityContract) error {
	failures := make([]*capabilityContractError, 0, len(model.Candidates))
	providers := make([]string, 0, len(model.Candidates))
	for _, item := range model.Candidates {
		if _, errPreflight := resolveCandidateContract(item, contract); errPreflight == nil {
			return nil
		} else {
			var typed *capabilityContractError
			if errors.As(errPreflight, &typed) {
				failures = append(failures, typed)
			}
			if provider := normalizeContractProvider(item.Provider); provider != "" {
				providers = append(providers, provider)
			}
		}
	}
	if len(failures) > 0 {
		code := failures[0].Code
		capability := failures[0].Capability
		for _, failure := range failures[1:] {
			if failure.Code != code {
				code = "bravo_contract_unavailable"
			}
			if failure.Capability != capability {
				capability = ""
			}
		}
		providers = normalizeStrings(providers)
		message := "Bravo logical model has no contract-preserving candidate"
		if capability != "" {
			message = fmt.Sprintf("Bravo logical model cannot preserve required capability %s", capability)
		}
		if len(providers) > 0 {
			message += fmt.Sprintf(" across configured providers (%s)", strings.Join(providers, ", "))
		}
		return &capabilityContractError{
			Code:       code,
			Protocol:   contract.Protocol,
			Capability: capability,
			Message:    message,
		}
	}
	return &capabilityContractError{
		Code:     "bravo_contract_unavailable",
		Protocol: contract.Protocol,
		Message:  "Bravo logical model has no contract-preserving candidate",
	}
}

func resolveCandidateContract(item candidate, contract requestCapabilityContract) (candidate, error) {
	// A mapped effort is Bravo policy, not part of the client contract. Claude
	// cannot combine thinking with a forced tool choice, so an otherwise
	// compatible function-tool request may use Claude without the internal
	// default. Explicit effort (including adaptive "auto") remains binding and
	// is rejected below rather than silently downgraded.
	if normalizeContractProvider(item.Provider) == "claude" &&
		contract.ForcedToolChoice &&
		!contract.Effort.Specified {
		item.Effort = ""
	}
	if errVerify := verifyCandidateContract(item, contract); errVerify != nil {
		return candidate{}, errVerify
	}
	return resolveCandidateEffort(item, contract.Effort)
}

func verifyCandidateContract(item candidate, contract requestCapabilityContract) error {
	provider := normalizeContractProvider(item.Provider)
	if provider == "claude" && contract.ForcedToolChoice && effortEnablesThinking(effectiveCandidateEffort(item, contract.Effort)) {
		return &capabilityContractError{
			Code:       "bravo_capability_conflict",
			Provider:   provider,
			Protocol:   contract.Protocol,
			Capability: capabilityReasoning,
			Message:    "Claude cannot preserve reasoning with a forced tool choice",
		}
	}
	if errVerify := verifyProviderContract(item.Provider, item.Capabilities, contract); errVerify != nil {
		return errVerify
	}
	if _, reasoning := contract.Capabilities[capabilityReasoning]; reasoning {
		baseModel := strings.TrimSpace(thinking.ParseSuffix(item.Model).ModelName)
		modelInfo := registry.LookupModelInfo(baseModel, normalizeProvider(item.Provider))
		if modelInfo == nil || modelInfo.Thinking == nil {
			return &capabilityContractError{
				Code:       "bravo_contract_unverified",
				Provider:   provider,
				Protocol:   contract.Protocol,
				Capability: capabilityReasoning,
				Message:    fmt.Sprintf("candidate %s/%s has no verified reasoning support", provider, baseModel),
			}
		}
	}
	return nil
}

func verifyProviderContract(provider string, declared []string, contract requestCapabilityContract) error {
	provider = normalizeContractProvider(provider)
	if provider == "" {
		return &capabilityContractError{
			Code:     "bravo_provider_unsupported",
			Protocol: contract.Protocol,
			Message:  "Bravo does not support the candidate provider",
		}
	}

	if provider == "claude" && contract.ForcedToolChoice {
		_, reasoning := contract.Capabilities[capabilityReasoning]
		if reasoning {
			return &capabilityContractError{
				Code:       "bravo_capability_conflict",
				Provider:   provider,
				Protocol:   contract.Protocol,
				Capability: capabilityReasoning,
				Message:    "Claude cannot preserve reasoning with a forced tool choice",
			}
		}
	}

	declaredSet := newCapabilitySet(declared...)
	verified, routeKnown := verifiedCapabilities(provider, contract.Protocol)
	if !routeKnown {
		return &capabilityContractError{
			Code:     "bravo_contract_unverified",
			Provider: provider,
			Protocol: contract.Protocol,
			Message:  fmt.Sprintf("Bravo has no verified contract for provider %s via %s", provider, contract.Protocol),
		}
	}

	for _, required := range contract.RequiredCapabilities() {
		if _, ok := declaredSet[required]; !ok {
			return &capabilityContractError{
				Code:       "bravo_capability_undeclared",
				Provider:   provider,
				Protocol:   contract.Protocol,
				Capability: required,
				Message:    fmt.Sprintf("candidate %s does not declare required capability %s", provider, required),
			}
		}
		if _, ok := verified[required]; !ok {
			return &capabilityContractError{
				Code:       "bravo_contract_unverified",
				Provider:   provider,
				Protocol:   contract.Protocol,
				Capability: required,
				Message:    fmt.Sprintf("capability %s is not live-verified for provider %s via %s", required, provider, contract.Protocol),
			}
		}
	}
	return nil
}

func verifiedCapabilities(provider, protocol string) (capabilitySet, bool) {
	key := providerProtocol{
		Provider: normalizeContractProvider(provider),
		Protocol: normalizeContractProtocol(protocol),
	}
	live, liveKnown := liveCapabilityMatrix[key]
	if !liveKnown {
		return nil, false
	}
	return cloneCapabilitySet(live), true
}

func inspectOpenAIChat(root map[string]any, capabilities capabilitySet) {
	if openAIHasUnhandledReasoning(root) {
		capabilities[capabilityReasoning] = struct{}{}
	}
	if structuredFormat(root["response_format"]) || structuredTextConfig(root["text"]) {
		capabilities[capabilityStructuredOutput] = struct{}{}
	}
	inspectConversationValue(root["messages"], protocolOpenAI, capabilities)
}

func inspectOpenAIResponse(root map[string]any, capabilities capabilitySet) {
	if openAIHasUnhandledReasoning(root) {
		capabilities[capabilityReasoning] = struct{}{}
	}
	if structuredTextConfig(root["text"]) || structuredFormat(root["response_format"]) {
		capabilities[capabilityStructuredOutput] = struct{}{}
	}
	inspectConversationValue(root["input"], protocolOpenAIResponse, capabilities)
}

func inspectClaude(root map[string]any, capabilities capabilitySet) {
	if claudeHasUnhandledThinking(root) {
		capabilities[capabilityReasoning] = struct{}{}
	}
	if nestedStructuredFormat(root["output_config"], "format") || structuredFormat(root["output_format"]) {
		capabilities[capabilityStructuredOutput] = struct{}{}
	}
	inspectConversationValue(root["messages"], protocolClaude, capabilities)
}

func inspectConversationValue(value any, protocol string, capabilities capabilitySet) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			inspectConversationValue(item, protocol, capabilities)
		}
	case map[string]any:
		itemType := lowerString(typed["type"])
		role := lowerString(typed["role"])

		if role == "tool" {
			capabilities[capabilityToolResult] = struct{}{}
		}
		if valuePresent(typed["reasoning_content"]) {
			capabilities[capabilityReasoning] = struct{}{}
		}

		switch itemType {
		case "image", "image_url", "input_image":
			capabilities[capabilityVision] = struct{}{}
		case "document", "file", "input_file":
			capabilities[capabilityFileInput] = struct{}{}
		case "tool_result", "function_call_output", "custom_tool_call_output", "computer_call_output":
			capabilities[capabilityToolResult] = struct{}{}
		case "tool_use", "function_call", "custom_tool_call", "computer_call", "additional_tools":
			capabilities[capabilityTools] = struct{}{}
		case "reasoning", "thinking", "redacted_thinking":
			capabilities[capabilityReasoning] = struct{}{}
		case "web_search_call", "web_search_tool_result":
			capabilities[capabilityTools] = struct{}{}
			capabilities[capabilityWebSearch] = struct{}{}
		case "server_tool_use":
			capabilities[capabilityTools] = struct{}{}
			if lowerString(typed["name"]) == "web_search" {
				capabilities[capabilityWebSearch] = struct{}{}
			}
		}

		if calls, ok := typed["tool_calls"].([]any); ok && len(calls) > 0 {
			capabilities[capabilityTools] = struct{}{}
		}
		if protocol == protocolClaude && strings.HasPrefix(itemType, "web_search_") {
			capabilities[capabilityTools] = struct{}{}
			capabilities[capabilityWebSearch] = struct{}{}
		}

		if content, exists := typed["content"]; exists {
			inspectConversationValue(content, protocol, capabilities)
		}
		if message, exists := typed["message"]; exists {
			inspectConversationValue(message, protocol, capabilities)
		}
	}
}

func inspectTools(value any, protocol string, capabilities capabilitySet) {
	tools, ok := value.([]any)
	if !ok || len(tools) == 0 {
		return
	}
	capabilities[capabilityTools] = struct{}{}
	for _, rawTool := range tools {
		tool, okTool := rawTool.(map[string]any)
		if !okTool {
			capabilities[capabilityProviderTool] = struct{}{}
			continue
		}
		toolType := lowerString(tool["type"])
		webSearch := strings.HasPrefix(toolType, "web_search") ||
			(protocol == protocolClaude && toolType == "server_tool_use" && lowerString(tool["name"]) == "web_search")
		switch {
		case webSearch:
			capabilities[capabilityWebSearch] = struct{}{}
			if webSearchFiltersPresent(tool) {
				capabilities[capabilityWebSearchFilters] = struct{}{}
			}
		case toolType == "image_generation":
			// Image generation is supported only by the dedicated image
			// endpoint. Treating this as a generic text tool could silently
			// discard the generated image or its endpoint-specific options.
			capabilities[capabilityImageGeneration] = struct{}{}
		case toolType == "function", toolType == "namespace":
			// These are provider-neutral tool schemas covered by tools.
		case toolType == "" && protocol == protocolClaude:
			// Claude's ordinary client-defined tools omit the type field.
		default:
			// Built-in provider tools (file_search, computer, code
			// interpreter, MCP, and future types) need their own verified
			// translation contract. They must not inherit the generic
			// function-tools contract.
			capabilities[capabilityProviderTool] = struct{}{}
		}
		if children, exists := tool["tools"]; exists {
			inspectTools(children, protocol, capabilities)
		}
	}
}

func webSearchFiltersPresent(tool map[string]any) bool {
	for _, field := range []string{"allowed_domains", "blocked_domains", "filters"} {
		if _, exists := tool[field]; exists {
			return true
		}
	}
	return false
}

func forcedToolChoice(protocol string, value any) bool {
	switch typed := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "", "auto", "none":
			return false
		default:
			return true
		}
	case map[string]any:
		choiceType := lowerString(typed["type"])
		if protocol == protocolClaude {
			return choiceType == "any" || choiceType == "tool"
		}
		switch choiceType {
		case "", "auto", "none":
			return false
		default:
			return true
		}
	default:
		return false
	}
}

func meaningfulToolChoice(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(typed)) != "none" && strings.TrimSpace(typed) != ""
	case map[string]any:
		return lowerString(typed["type"]) != "none" && len(typed) > 0
	default:
		return false
	}
}

func toolChoiceIsWebSearch(value any) bool {
	choice, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return strings.HasPrefix(lowerString(choice["type"]), "web_search")
}

func structuredTextConfig(value any) bool {
	text, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return structuredFormat(text["format"])
}

func nestedStructuredFormat(value any, key string) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return structuredFormat(object[key])
}

func structuredFormat(value any) bool {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return false
	}
	formatType := lowerString(object["type"])
	if formatType == "text" {
		return false
	}
	return formatType != "" || valuePresent(object["schema"]) || valuePresent(object["json_schema"])
}

func nestedValuePresent(value any, key string) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return valuePresent(object[key])
}

func nonEmptyObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && len(object) > 0
}

func valuePresent(value any) bool {
	if value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	return true
}

func boolField(root map[string]any, key string) bool {
	value, ok := root[key].(bool)
	return ok && value
}

func lowerString(value any) string {
	text, _ := value.(string)
	return strings.ToLower(strings.TrimSpace(text))
}

func normalizeContractProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case protocolOpenAI:
		return protocolOpenAI
	case protocolOpenAIResponse:
		return protocolOpenAIResponse
	case protocolClaude:
		return protocolClaude
	case protocolOpenAIImage:
		return protocolOpenAIImage
	default:
		return ""
	}
}

func normalizeContractProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic":
		return "claude"
	case "codex", "openai":
		return "codex"
	default:
		return ""
	}
}

func newCapabilitySet(values ...string) capabilitySet {
	out := make(capabilitySet, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func cloneCapabilitySet(source capabilitySet) capabilitySet {
	out := make(capabilitySet, len(source))
	for capability := range source {
		out[capability] = struct{}{}
	}
	return out
}

func orderedCapabilities(capabilities capabilitySet) []string {
	out := make([]string, 0, len(capabilities))
	seen := make(capabilitySet, len(capabilities))
	for _, capability := range capabilityOrder {
		if _, exists := capabilities[capability]; !exists {
			continue
		}
		out = append(out, capability)
		seen[capability] = struct{}{}
	}
	var extra []string
	for capability := range capabilities {
		if _, exists := seen[capability]; !exists {
			extra = append(extra, capability)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func invalidContractPayload(protocol, message string) error {
	return &capabilityContractError{
		Code:     "bravo_request_invalid",
		Protocol: protocol,
		Message:  message,
	}
}
