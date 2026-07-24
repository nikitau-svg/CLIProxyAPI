package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

// requestEffort is the provider-neutral named effort requested by the client.
// "auto" is deliberately retained as an explicit value so the request fields
// can be removed while Bravo keeps the candidate's configured default.
type requestEffort struct {
	Value     string
	Specified bool
}

func detectRequestEffort(protocol string, root map[string]any) (requestEffort, error) {
	var values []namedEffort
	switch normalizeContractProtocol(protocol) {
	case protocolOpenAI, protocolOpenAIResponse:
		if value, exists := root["reasoning_effort"]; exists && valuePresent(value) {
			effort, errNormalize := normalizeClientEffort(value, "reasoning_effort")
			if errNormalize != nil {
				return requestEffort{}, errNormalize
			}
			values = append(values, namedEffort{Path: "reasoning_effort", Value: effort})
		}
		if rawReasoning, exists := root["reasoning"]; exists && rawReasoning != nil {
			reasoning, ok := rawReasoning.(map[string]any)
			if !ok {
				return requestEffort{}, unsupportedOpenAIReasoningControl()
			}
			for key := range reasoning {
				if key != "effort" {
					return requestEffort{}, unsupportedOpenAIReasoningControl()
				}
			}
			if value, exists := reasoning["effort"]; exists && valuePresent(value) {
				effort, errNormalize := normalizeClientEffort(value, "reasoning.effort")
				if errNormalize != nil {
					return requestEffort{}, errNormalize
				}
				values = append(values, namedEffort{Path: "reasoning.effort", Value: effort})
			}
		}
	case protocolClaude:
		if outputConfig, ok := root["output_config"].(map[string]any); ok {
			if value, exists := outputConfig["effort"]; exists && valuePresent(value) {
				effort, errNormalize := normalizeClientEffort(value, "output_config.effort")
				if errNormalize != nil {
					return requestEffort{}, errNormalize
				}
				values = append(values, namedEffort{Path: "output_config.effort", Value: effort})
			}
		}

		if rawThinking, exists := root["thinking"]; exists && rawThinking != nil {
			thinking, ok := rawThinking.(map[string]any)
			if !ok || lowerString(thinking["type"]) != "adaptive" {
				return requestEffort{}, unsupportedClaudeThinkingControl()
			}
			for key, value := range thinking {
				switch key {
				case "type":
					continue
				case "display":
					// Claude Code 2.1.218 sends display:"omitted" alongside
					// adaptive thinking. This is a presentation preference,
					// not a reasoning budget, and stripRequestEffort removes
					// the whole source thinking object before host execution.
					if lowerString(value) == "omitted" {
						continue
					}
				}
				if key != "type" {
					return requestEffort{}, unsupportedClaudeThinkingControl()
				}
			}
			// Adaptive is the mode marker used by Claude Code. Without an
			// explicit output_config.effort it means "use Bravo's mapped
			// candidate default", which is the common "auto" behavior.
			if !hasNamedEffortAt(values, "output_config.effort") {
				values = append(values, namedEffort{Path: "thinking.type", Value: "auto"})
			}
		}
	}

	if len(values) == 0 {
		return requestEffort{}, nil
	}
	requested := values[0]
	for _, value := range values[1:] {
		if value.Value == requested.Value {
			continue
		}
		return requestEffort{}, effortContractError(
			"bravo_effort_conflict",
			fmt.Sprintf("conflicting client effort controls: %s=%s and %s=%s", requested.Path, requested.Value, value.Path, value.Value),
		)
	}
	return requestEffort{Value: requested.Value, Specified: true}, nil
}

type namedEffort struct {
	Path  string
	Value string
}

func hasNamedEffortAt(values []namedEffort, path string) bool {
	for _, value := range values {
		if value.Path == path {
			return true
		}
	}
	return false
}

func normalizeClientEffort(raw any, path string) (string, error) {
	value, ok := raw.(string)
	if !ok {
		return "", effortContractError("bravo_effort_invalid", fmt.Sprintf("%s must be a named effort string", path))
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "ultra" {
		value = "max"
	}
	switch value {
	case "auto", "low", "medium", "high", "xhigh", "max":
		return value, nil
	case "none", "minimal":
		return "", effortContractError(
			"bravo_contract_unverified",
			fmt.Sprintf("%s effort %q is not contract-preserving across the current Bravo fallback pool", path, value),
		)
	default:
		return "", effortContractError(
			"bravo_effort_invalid",
			fmt.Sprintf("%s has unsupported effort %q", path, strings.TrimSpace(value)),
		)
	}
}

func effortContractError(code, message string) error {
	return &capabilityContractError{
		Code:       code,
		Capability: capabilityReasoning,
		Message:    message,
	}
}

func unsupportedClaudeThinkingControl() error {
	return effortContractError(
		"bravo_contract_unverified",
		`thinking supports adaptive mode and Claude Code display:"omitted" under Bravo; manual budgets and other provider-specific thinking controls are not contract-preserving across the fallback pool`,
	)
}

func unsupportedOpenAIReasoningControl() error {
	return effortContractError(
		"bravo_contract_unverified",
		"reasoning supports only the named effort field under Bravo; summaries and provider-specific reasoning controls are not contract-preserving across the fallback pool",
	)
}

// resolveCandidateEffort converts an explicit logical effort into an effort
// that the concrete physical model can accept. The model registry is the
// authority for physical capabilities, while the core thinking package is the
// authority for comparing named effort levels.
//
// Explicit "auto" and an unspecified effort deliberately retain the
// candidate's configured default. For discrete-level models, an unsupported
// request is floored to the greatest advertised level that does not exceed the
// request. Budget-only models accept all core named levels because the host
// converts and clamps those levels to the model's registered budget range.
func resolveCandidateEffort(item candidate, requested requestEffort) (candidate, error) {
	item.Effort = normalizeEffort(item.Effort)
	if !requested.Specified || requested.Value == "auto" {
		return item, nil
	}

	requestedValue := normalizeEffort(requested.Value)
	requestedBudget, knownRequested := thinking.ConvertLevelToBudget(requestedValue)
	if !knownRequested || requestedBudget <= 0 {
		return candidate{}, candidateEffortUnavailable(item, requestedValue, "the requested effort is not a core named thinking level")
	}

	baseModel := strings.TrimSpace(thinking.ParseSuffix(item.Model).ModelName)
	modelInfo := registry.LookupModelInfo(baseModel, normalizeProvider(item.Provider))
	if modelInfo == nil || modelInfo.Thinking == nil {
		return candidate{}, candidateEffortUnavailable(item, requestedValue, "the physical model has no verified thinking capabilities")
	}

	support := modelInfo.Thinking
	if len(support.Levels) == 0 {
		if support.Min <= 0 && support.Max <= 0 {
			return candidate{}, candidateEffortUnavailable(item, requestedValue, "the physical model has no verified fixed-effort range")
		}
		item.Effort = requestedValue
		return item, nil
	}

	effective := ""
	effectiveBudget := -1
	for _, supported := range support.Levels {
		level := normalizeEffort(supported)
		if level == "" || level == "none" || level == "auto" {
			continue
		}
		budget, known := thinking.ConvertLevelToBudget(level)
		if !known || budget <= 0 || budget > requestedBudget || budget <= effectiveBudget {
			continue
		}
		effective = level
		effectiveBudget = budget
	}
	if effective == "" {
		return candidate{}, candidateEffortUnavailable(item, requestedValue, "the physical model has no supported effort at or below the request")
	}
	item.Effort = effective
	return item, nil
}

func candidateEffortUnavailable(item candidate, requested, reason string) error {
	provider := normalizeProvider(item.Provider)
	model := strings.TrimSpace(thinking.ParseSuffix(item.Model).ModelName)
	return &capabilityContractError{
		Code:       "bravo_effort_unavailable",
		Provider:   provider,
		Capability: capabilityReasoning,
		Message: fmt.Sprintf(
			"Bravo candidate %s/%s cannot safely apply requested effort %q: %s",
			provider,
			model,
			requested,
			reason,
		),
	}
}

func openAIHasUnhandledReasoning(root map[string]any) bool {
	value, exists := root["reasoning"]
	if !exists || value == nil {
		return false
	}
	reasoning, ok := value.(map[string]any)
	if !ok {
		return valuePresent(value)
	}
	for key, child := range reasoning {
		if key == "effort" || !valuePresent(child) {
			continue
		}
		return true
	}
	return false
}

func claudeHasUnhandledThinking(root map[string]any) bool {
	value, exists := root["thinking"]
	if !exists || value == nil {
		return false
	}
	thinking, ok := value.(map[string]any)
	if !ok {
		return valuePresent(value)
	}
	if len(thinking) == 0 {
		return false
	}
	for key, child := range thinking {
		if key == "type" || !valuePresent(child) {
			continue
		}
		if key == "display" && lowerString(child) == "omitted" {
			continue
		}
		// Numeric budget_tokens and all other manual/replay controls remain
		// fail-closed because reducing them to a named level loses semantics.
		return true
	}
	switch lowerString(thinking["type"]) {
	case "adaptive":
		return false
	default:
		return true
	}
}

// stripRequestEffort removes only the effort controls already represented by
// the physical-model suffix. This prevents source-provider fields, especially
// literal "auto", from being interpreted a second time by a fallback provider.
func stripRequestEffort(body []byte, protocol string, effort requestEffort) ([]byte, error) {
	if !effort.Specified {
		return body, nil
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, fmt.Errorf("decode client effort request: %w", errUnmarshal)
	}
	switch normalizeContractProtocol(protocol) {
	case protocolOpenAI, protocolOpenAIResponse:
		delete(root, "reasoning_effort")
		if reasoning, ok := root["reasoning"].(map[string]any); ok {
			delete(reasoning, "effort")
			if len(reasoning) == 0 {
				delete(root, "reasoning")
			}
		}
	case protocolClaude:
		if outputConfig, ok := root["output_config"].(map[string]any); ok {
			delete(outputConfig, "effort")
			if len(outputConfig) == 0 {
				delete(root, "output_config")
			}
		}
		delete(root, "thinking")
	}
	return json.Marshal(root)
}
