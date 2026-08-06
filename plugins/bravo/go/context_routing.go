package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type contextCountKind string

const (
	contextCountUnknown    contextCountKind = "unknown"
	contextCountExact      contextCountKind = "exact"
	contextCountUpperBound contextCountKind = "upper_bound"
)

type contextCountScope string

const (
	contextCountScopeUnknown contextCountScope = "unknown"
	contextCountTargetModel  contextCountScope = "target_model"
)

type contextCompatibility uint8

const (
	contextCompatibilityUnknown contextCompatibility = iota
	contextCompatibilityFits
	contextCompatibilityDoesNotFit
)

type contextRequirement struct {
	RequiredInputTokens   int64
	RequestedOutputTokens int64
	CountKind             contextCountKind
	CountScope            contextCountScope
	Provider              string
	Model                 string
}

type hostModelLimitIndex map[string]pluginapi.HostModelListEntry

func newHostModelLimitIndex(models []pluginapi.HostModelListEntry) hostModelLimitIndex {
	index := make(hostModelLimitIndex, len(models))
	for _, model := range models {
		provider := normalizeProvider(model.Provider)
		modelID := strings.TrimSpace(model.ID)
		if provider == "" || modelID == "" {
			continue
		}
		model.Provider = provider
		model.ID = modelID
		if model.InputTokenLimit < 0 {
			model.InputTokenLimit = 0
		}
		if model.ContextLength < 0 {
			model.ContextLength = 0
		}
		if model.MaxCompletionTokens < 0 {
			model.MaxCompletionTokens = 0
		}
		index[contextModelKey(provider, modelID)] = model
	}
	return index
}

func contextModelKey(provider, model string) string {
	return normalizeProvider(provider) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func logicalModelCapacityAnchor(item logicalModel, hostModels []pluginapi.HostModelListEntry) (pluginapi.HostModelListEntry, bool) {
	index := newHostModelLimitIndex(hostModels)
	type rankedAnchor struct {
		entry    pluginapi.HostModelListEntry
		capacity int64
		priority int
		key      string
	}
	anchors := make([]rankedAnchor, 0, len(item.Candidates))
	for _, modelCandidate := range item.Candidates {
		capabilities := newCapabilitySet(modelCandidate.Capabilities...)
		if _, imageOnly := capabilities[capabilityImageGeneration]; imageOnly {
			if _, text := capabilities[capabilityText]; !text {
				continue
			}
		}
		key := contextModelKey(modelCandidate.Provider, modelCandidate.Model)
		entry, exists := index[key]
		if !exists {
			continue
		}
		capacity, known := effectiveInputCapacity(entry, entry.MaxCompletionTokens)
		if !known {
			continue
		}
		anchors = append(anchors, rankedAnchor{
			entry:    entry,
			capacity: capacity,
			priority: modelCandidate.Priority,
			key:      key,
		})
	}
	if len(anchors) == 0 {
		return pluginapi.HostModelListEntry{}, false
	}
	sort.SliceStable(anchors, func(i, j int) bool {
		left, right := anchors[i], anchors[j]
		if left.capacity != right.capacity {
			return left.capacity > right.capacity
		}
		if left.entry.ContextLength != right.entry.ContextLength {
			return left.entry.ContextLength > right.entry.ContextLength
		}
		if left.entry.MaxCompletionTokens != right.entry.MaxCompletionTokens {
			return left.entry.MaxCompletionTokens > right.entry.MaxCompletionTokens
		}
		if left.priority != right.priority {
			return left.priority > right.priority
		}
		return left.key < right.key
	})
	return anchors[0].entry, true
}

func effectiveInputCapacity(entry pluginapi.HostModelListEntry, completionReservation int64) (int64, bool) {
	capacity := int64(0)
	known := false
	if entry.InputTokenLimit > 0 {
		capacity = entry.InputTokenLimit
		known = true
	}
	if entry.ContextLength > 0 && completionReservation > 0 && completionReservation <= entry.ContextLength {
		combinedCapacity := entry.ContextLength - completionReservation
		if !known || combinedCapacity < capacity {
			capacity = combinedCapacity
		}
		known = true
	}
	return capacity, known && capacity >= 0
}

func contextCandidateCompatibility(
	modelCandidate candidate,
	requirement contextRequirement,
	completionReservation int64,
	limits hostModelLimitIndex,
) contextCompatibility {
	if requirement.RequiredInputTokens <= 0 ||
		(requirement.CountKind != contextCountExact && requirement.CountKind != contextCountUpperBound) ||
		requirement.CountScope != contextCountTargetModel ||
		contextModelKey(requirement.Provider, requirement.Model) != contextModelKey(modelCandidate.Provider, modelCandidate.Model) {
		return contextCompatibilityUnknown
	}
	entry, exists := limits[contextModelKey(modelCandidate.Provider, modelCandidate.Model)]
	if !exists {
		return contextCompatibilityUnknown
	}
	if completionReservation <= 0 {
		completionReservation = entry.MaxCompletionTokens
	}
	if entry.InputTokenLimit <= 0 && (entry.ContextLength <= 0 || completionReservation <= 0) {
		return contextCompatibilityUnknown
	}
	if entry.InputTokenLimit > 0 && requirement.RequiredInputTokens > entry.InputTokenLimit {
		return contextCompatibilityDoesNotFit
	}
	if entry.MaxCompletionTokens > 0 && completionReservation > entry.MaxCompletionTokens {
		return contextCompatibilityDoesNotFit
	}
	if entry.ContextLength > 0 {
		if completionReservation <= 0 || requirement.RequiredInputTokens > entry.ContextLength-completionReservation {
			return contextCompatibilityDoesNotFit
		}
	}
	return contextCompatibilityFits
}

type contextRoutingState struct {
	hostCallbackID string
	requirement    contextRequirement
	limits         hostModelLimitIndex
	limitsLoaded   bool
	proofs         map[string]contextCompatibility
}

func newContextRoutingState(hostCallbackID string) *contextRoutingState {
	return &contextRoutingState{
		hostCallbackID: strings.TrimSpace(hostCallbackID),
		proofs:         make(map[string]contextCompatibility),
	}
}

func (state *contextRoutingState) observeFailure(attempt executionAttempt, failure executionFailure) bool {
	if state == nil || failure.Code != "bravo_context_window_exceeded" || failure.Provider == nil ||
		failure.Provider.RequiredTokens <= 0 {
		return false
	}
	state.requirement = contextRequirement{
		RequiredInputTokens: failure.Provider.RequiredTokens,
		CountKind:           contextCountExact,
		CountScope:          contextCountTargetModel,
		Provider:            normalizeProvider(attempt.Candidate.Provider),
		Model:               strings.TrimSpace(attempt.Candidate.Model),
	}
	return true
}

func (state *contextRoutingState) active() bool {
	return state != nil && state.requirement.RequiredInputTokens > 0
}

func (state *contextRoutingState) proveCandidate(
	req rpcExecutorRequest,
	attempt executionAttempt,
	protocol, physicalModel string,
	candidateBody []byte,
) bool {
	if state == nil || !state.active() {
		return true
	}
	key := contextModelKey(attempt.Candidate.Provider, attempt.Candidate.Model)
	if result, attempted := state.proofs[key]; attempted {
		return result == contextCompatibilityFits
	}
	// Allocate the proof slot before any host call. Multiple credentials for the
	// same physical model must never multiply count probes.
	state.proofs[key] = contextCompatibilityUnknown
	if key == contextModelKey(state.requirement.Provider, state.requirement.Model) {
		state.proofs[key] = contextCompatibilityDoesNotFit
		return false
	}
	if !state.loadLimits() {
		return false
	}
	count, ok := state.countTarget(req, attempt, protocol, physicalModel, candidateBody)
	if !ok {
		return false
	}
	requirement := contextRequirement{
		RequiredInputTokens:   count,
		RequestedOutputTokens: requestedCompletionTokens(candidateBody),
		CountKind:             contextCountExact,
		CountScope:            contextCountTargetModel,
		Provider:              normalizeProvider(attempt.Candidate.Provider),
		Model:                 strings.TrimSpace(attempt.Candidate.Model),
	}
	result := contextCandidateCompatibility(
		attempt.Candidate,
		requirement,
		requirement.RequestedOutputTokens,
		state.limits,
	)
	state.proofs[key] = result
	return result == contextCompatibilityFits
}

func (state *contextRoutingState) loadLimits() bool {
	if state.limitsLoaded {
		return len(state.limits) > 0
	}
	state.limitsLoaded = true
	raw, errCall := callHost(pluginabi.MethodHostModelList, pluginapi.HostModelListRequest{
		HostCallbackID: state.hostCallbackID,
	})
	if errCall != nil {
		return false
	}
	var response pluginapi.HostModelListResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return false
	}
	state.limits = newHostModelLimitIndex(response.Models)
	return len(state.limits) > 0
}

func (state *contextRoutingState) countTarget(
	req rpcExecutorRequest,
	attempt executionAttempt,
	protocol, physicalModel string,
	candidateBody []byte,
) (int64, bool) {
	proofBody := nonStreamingCountBody(candidateBody)
	raw, errCall := callHost(pluginabi.MethodHostModelCountTokens, hostModelExecutionRequest{
		HostModelExecutionRequest: nestedHostModelRequest(
			req,
			attempt,
			protocol,
			physicalModel,
			proofBody,
			false,
		),
		HostCallbackID: state.hostCallbackID,
	})
	if errCall != nil {
		return 0, false
	}
	var response pluginapi.HostModelExecutionResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil ||
		response.StatusCode >= http.StatusBadRequest {
		return 0, false
	}
	return exactInputTokenCount(response.Body)
}

func nonStreamingCountBody(body []byte) []byte {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || payload == nil {
		return body
	}
	payload["stream"] = json.RawMessage("false")
	delete(payload, "stream_options")
	normalized, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return normalized
}

func exactInputTokenCount(body []byte) (int64, bool) {
	var payload struct {
		InputTokens  int64 `json:"input_tokens"`
		PromptTokens int64 `json:"prompt_tokens"`
		Usage        struct {
			InputTokens  int64 `json:"input_tokens"`
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
		Response struct {
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				PromptTokens int64 `json:"prompt_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0, false
	}
	values := []int64{
		payload.InputTokens,
		payload.PromptTokens,
		payload.Usage.InputTokens,
		payload.Usage.PromptTokens,
		payload.Response.Usage.InputTokens,
		payload.Response.Usage.PromptTokens,
	}
	count := int64(0)
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if count != 0 && count != value {
			return 0, false
		}
		count = value
	}
	return count, count > 0
}

func requestedCompletionTokens(body []byte) int64 {
	var payload struct {
		MaxTokens       int64 `json:"max_tokens"`
		MaxOutputTokens int64 `json:"max_output_tokens"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0
	}
	if payload.MaxTokens > 0 {
		return payload.MaxTokens
	}
	if payload.MaxOutputTokens > 0 {
		return payload.MaxOutputTokens
	}
	return 0
}

func contextExecutionFailure(detail providererror.Detail) executionFailure {
	detail = providererror.Sanitize(detail)
	detail.Code = "context_window_exceeded"
	detail.Class = "context_window"
	detail.Scope = "request"
	failure := executionFailure{
		Code:          "bravo_context_window_exceeded",
		Message:       "Контекст переписки превышает окно выбранной модели.",
		Status:        http.StatusBadRequest,
		Retryable:     false,
		RouteFallback: detail.RequiredTokens > 0,
		AccountWide:   false,
		Provider:      &detail,
	}
	if detail.RequiredTokens > 0 && detail.LimitTokens > 0 {
		failure.Message = fmt.Sprintf(
			"Контекст содержит %s токенов и превышает лимит модели %s токенов.",
			formatContextTokenCount(detail.RequiredTokens),
			formatContextTokenCount(detail.LimitTokens),
		)
	}
	return failure
}

func formatContextTokenCount(value int64) string {
	raw := strconv.FormatInt(value, 10)
	if len(raw) <= 3 {
		return raw
	}
	var builder strings.Builder
	first := len(raw) % 3
	if first == 0 {
		first = 3
	}
	builder.WriteString(raw[:first])
	for index := first; index < len(raw); index += 3 {
		builder.WriteByte(' ')
		builder.WriteString(raw[index : index+3])
	}
	return builder.String()
}
