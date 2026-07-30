package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var crossProtocolCreditsProviderDetail = providererror.Detail{
	Type:             "rate_limit_error",
	Code:             "credits_required",
	Message:          "Usage credits are required for this model.",
	Model:            "claude-fable-5",
	ModelDisplayName: "Fable 5",
	NoticeTitle:      "You've hit your monthly spend limit",
	NoticeText:       "Ask your admin to raise your spend limit, or switch models to continue this chat.",
	DisabledReason:   "org_level_disabled_until",
	Scope:            "model",
	Reason:           "monthly_spend_limit",
}

type crossProtocolStreamFixture struct {
	name           string
	protocol       string
	requestBody    []byte
	failedPrelude  []byte
	content        []byte
	fallbackChunks []pluginapi.HostModelStreamReadResponse
}

type crossProtocolStreamObservation struct {
	calls       []pluginapi.HostModelExecutionRequest
	emitted     [][]byte
	pluginClose rpcStreamCloseRequest
}

func TestBravoStreamPreContentCreditsFallbackAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:          "credits_required",
							Message:       "Fable 5: You've hit your monthly spend limit",
							HTTPStatus:    http.StatusTooManyRequests,
							Retryable:     true,
							ProviderError: &crossProtocolCreditsProviderDetail,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude", "codex")
			if observation.pluginClose.Error != "" {
				t.Fatalf("plugin stream close = %#v, want successful fallback", observation.pluginClose)
			}
			visible := joinCrossProtocolPayloads(observation.emitted)
			if !strings.Contains(visible, "fallback-visible") {
				t.Fatalf("fallback output was not emitted: %q", visible)
			}
			if strings.Contains(visible, "failed-prelude") {
				t.Fatalf("failed Claude prelude was committed before fallback: %q", visible)
			}
			if strings.Contains(visible, "claude-fable-5") ||
				strings.Contains(visible, "gpt-5.6-sol") ||
				!strings.Contains(visible, "bravo/fallback-probe") {
				t.Fatalf("client-visible model identity was not preserved: %q", visible)
			}
			if fixture.protocol == protocolOpenAIResponse {
				assertCrossProtocolResponsesFallbackContract(t, observation.emitted)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
			assertCrossProtocolCreditsCooldown(t)
		})
	}
}

func TestBravoStreamPostContentCreditsNeverSplicesAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{Payload: fixture.content},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:          "credits_required",
							Message:       "Fable 5: You've hit your monthly spend limit",
							HTTPStatus:    http.StatusTooManyRequests,
							Retryable:     true,
							ProviderError: &crossProtocolCreditsProviderDetail,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude")
			visible := joinCrossProtocolPayloads(observation.emitted)
			if !strings.Contains(visible, "primary-visible") {
				t.Fatalf("committed primary content disappeared: %q", visible)
			}
			if strings.Contains(visible, "fallback-visible") {
				t.Fatalf("two provider streams were spliced together: %q", visible)
			}
			if !strings.Contains(observation.pluginClose.Error, "bravo_subscription_model_credits_exhausted") {
				t.Fatalf("plugin stream close = %#v, want safe terminal credits failure", observation.pluginClose)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
		})
	}
}

func TestBravoStreamPreContentBillingErrorFallsBackAcrossClientProtocols(t *testing.T) {
	billingDetail := providererror.Detail{
		Type:    "billing_error",
		Code:    "billing_error",
		Message: "The provider reported a billing restriction.",
		Scope:   "account",
	}
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:          "billing_error",
							Message:       "The provider reported a billing restriction.",
							HTTPStatus:    http.StatusPaymentRequired,
							Retryable:     true,
							ProviderError: &billingDetail,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude", "codex")
			if observation.pluginClose.Error != "" {
				t.Fatalf("plugin stream close = %#v, want successful fallback", observation.pluginClose)
			}
			visible := joinCrossProtocolPayloads(observation.emitted)
			if strings.Contains(visible, "failed-prelude") ||
				!strings.Contains(visible, "fallback-visible") {
				t.Fatalf("billing prelude leaked or fallback disappeared: %q", visible)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
			assertCrossProtocolAccountCooldown(t, "billing_error")
		})
	}
}

func TestBravoStreamPreContentOverloadedErrorFallsBackAcrossClientProtocols(t *testing.T) {
	overloadedDetail := providererror.Detail{
		Type:    "overloaded_error",
		Code:    "overloaded_error",
		Message: "The provider is temporarily overloaded.",
		Scope:   "model",
	}
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:          "overloaded_error",
							Message:       "The provider is temporarily overloaded.",
							HTTPStatus:    529,
							Retryable:     true,
							ProviderError: &overloadedDetail,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude", "codex")
			if observation.pluginClose.Error != "" {
				t.Fatalf("plugin stream close = %#v, want successful fallback", observation.pluginClose)
			}
			visible := joinCrossProtocolPayloads(observation.emitted)
			if strings.Contains(visible, "failed-prelude") ||
				!strings.Contains(visible, "fallback-visible") {
				t.Fatalf("overload prelude leaked or fallback disappeared: %q", visible)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
			assertCrossProtocolModelCooldown(t, "overloaded_error")
		})
	}
}

func TestBravoStreamPreContentUnknownStructuredErrorFailsClosedAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "provider_stream_error",
							Message:    "The provider returned an unrecognized structured stream error.",
							HTTPStatus: http.StatusBadGateway,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude")
			if len(observation.emitted) != 0 {
				t.Fatalf("pre-content provider bytes were emitted: %q", observation.emitted)
			}
			if observation.pluginClose.ErrorStatus != http.StatusBadGateway ||
				observation.pluginClose.ErrorCode != "bravo_provider_stream_error" {
				t.Fatalf("plugin stream close = %#v, want safe fail-closed 502", observation.pluginClose)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
			assertCrossProtocolNoCooldown(t)
		})
	}
}

func TestBravoStreamPreContentContextOverflowIsRequestScopedAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "invalid_request_error",
							Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
							HTTPStatus: http.StatusBadRequest,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude")
			if len(observation.emitted) != 0 {
				t.Fatalf("pre-content provider bytes were emitted: %q", observation.emitted)
			}
			if observation.pluginClose.ErrorStatus != http.StatusBadRequest ||
				observation.pluginClose.ErrorCode != "bravo_context_window_exceeded" ||
				!strings.Contains(observation.pluginClose.Error, "context window") {
				t.Fatalf("plugin stream close = %#v, want request-scoped context failure", observation.pluginClose)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
			assertCrossProtocolNoCooldown(t)
		})
	}
}

func TestBravoStreamIncompleteBeforeContentFallsBackAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "provider_stream_incomplete",
							Message:    "The provider returned an incomplete stream before message completion.",
							HTTPStatus: http.StatusBadGateway,
							Retryable:  true,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude", "codex")
			if observation.pluginClose.Error != "" {
				t.Fatalf("plugin stream close = %#v, want successful fallback", observation.pluginClose)
			}
			visible := joinCrossProtocolPayloads(observation.emitted)
			if strings.Contains(visible, "failed-prelude") ||
				!strings.Contains(visible, "fallback-visible") {
				t.Fatalf("incomplete prelude leaked or fallback disappeared: %q", visible)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
		})
	}
}

func TestBravoStreamIncompleteAfterContentNeverSplicesAcrossClientProtocols(t *testing.T) {
	for _, fixture := range crossProtocolStreamFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			observation := runCrossProtocolStreamScenario(
				t,
				fixture,
				[]pluginapi.HostModelStreamReadResponse{
					{Payload: fixture.failedPrelude},
					{Payload: fixture.content},
					{
						ErrorDetail: &pluginapi.HostModelExecutionError{
							Code:       "provider_stream_incomplete",
							Message:    "The provider returned an incomplete stream before message completion.",
							HTTPStatus: http.StatusBadGateway,
							Retryable:  true,
						},
						Done: true,
					},
				},
				fixture.fallbackChunks,
			)

			assertCrossProtocolProviderOrder(t, observation.calls, "claude")
			visible := joinCrossProtocolPayloads(observation.emitted)
			if !strings.Contains(visible, "primary-visible") ||
				strings.Contains(visible, "fallback-visible") {
				t.Fatalf("committed response was lost or spliced: %q", visible)
			}
			if !strings.Contains(observation.pluginClose.Error, "provider_stream_incomplete") {
				t.Fatalf("plugin stream close = %#v, want safe incomplete terminal error", observation.pluginClose)
			}
			assertCrossProtocolSafeDiagnostics(t, observation)
		})
	}
}

func runCrossProtocolStreamScenario(
	t *testing.T,
	fixture crossProtocolStreamFixture,
	claudeChunks, codexChunks []pluginapi.HostModelStreamReadResponse,
) crossProtocolStreamObservation {
	t.Helper()
	isolateBravoFallbackTestState(t)
	installBravoTestConfig(t, logicalModel{
		Candidates: []candidate{
			{
				Provider:     "claude",
				Model:        "claude-fable-5",
				Priority:     100,
				Capabilities: []string{capabilityText, capabilityStream},
			},
			{
				Provider:     "codex",
				Model:        "gpt-5.6-sol",
				Priority:     90,
				Capabilities: []string{capabilityText, capabilityStream},
			},
		},
	})

	auths := []pluginapi.HostAuthFileEntry{
		{ID: "palantir", Name: "palantir.json", Provider: "claude", Note: "Palantir"},
		{ID: "codex-x20", Name: "codex-x20.json", Provider: "codex", Note: "Codex x20"},
	}
	observation := crossProtocolStreamObservation{}
	readIndexes := map[string]int{}
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var request hostModelExecutionRequest
			decodeBravoPayload(t, payload, &request)
			observation.calls = append(observation.calls, request.HostModelExecutionRequest)
			streamID := request.ForcedProvider + "-translated-stream"
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
				StatusCode: http.StatusOK,
				StreamID:   streamID,
			}), nil
		case pluginabi.MethodHostModelStreamRead:
			var request pluginapi.HostModelStreamReadRequest
			decodeBravoPayload(t, payload, &request)
			chunks := claudeChunks
			if strings.HasPrefix(request.StreamID, "codex-") {
				chunks = codexChunks
			}
			index := readIndexes[request.StreamID]
			if index >= len(chunks) {
				return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
			}
			readIndexes[request.StreamID] = index + 1
			return mustBravoJSON(t, chunks[index]), nil
		case pluginabi.MethodHostModelStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamEmit:
			var request rpcStreamEmitRequest
			decodeBravoPayload(t, payload, &request)
			observation.emitted = append(
				observation.emitted,
				append([]byte(nil), request.Payload...),
			)
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &observation.pluginClose)
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	})

	runBravoStream(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/fallback-probe",
			Format:          fixture.protocol,
			SourceFormat:    fixture.protocol,
			OriginalRequest: fixture.requestBody,
			Metadata:        map[string]any{"request_id": "cross-protocol-" + fixture.name},
		},
		HostCallbackID: "cross-protocol-" + fixture.name + "-callback",
	}, "cross-protocol-"+fixture.name+"-client-stream")
	return observation
}

func crossProtocolStreamFixtures() []crossProtocolStreamFixture {
	return []crossProtocolStreamFixture{
		{
			name:        "claude_messages",
			protocol:    protocolClaude,
			requestBody: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			failedPrelude: []byte(
				"event: message_start\n" +
					`data: {"type":"message_start","message":{"id":"failed-prelude","model":"claude-fable-5","content":[]}}` +
					"\n\n",
			),
			content: []byte(
				"event: content_block_delta\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"primary-visible"}}` +
					"\n\n",
			),
			fallbackChunks: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						"event: message_start\n" +
							`data: {"type":"message_start","message":{"id":"fallback-start","model":"gpt-5.6-sol","content":[]}}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: content_block_delta\n" +
							`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fallback-visible"}}` +
							"\n\n",
					),
					Done: true,
				},
			},
		},
		{
			name:        "openai_chat_completions",
			protocol:    protocolOpenAI,
			requestBody: []byte(`{"model":"bravo/fallback-probe","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			failedPrelude: []byte(
				`{"id":"failed-prelude","object":"chat.completion.chunk","created":1,"model":"claude-fable-5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			),
			content: []byte(
				`{"id":"primary-content","object":"chat.completion.chunk","created":1,"model":"claude-fable-5","choices":[{"index":0,"delta":{"content":"primary-visible"},"finish_reason":null}]}`,
			),
			fallbackChunks: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						`{"id":"fallback-content","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"content":"fallback-visible"},"finish_reason":null}]}`,
					),
					Done: true,
				},
			},
		},
		{
			name:        "openai_responses",
			protocol:    protocolOpenAIResponse,
			requestBody: []byte(`{"model":"bravo/fallback-probe","input":"hello","stream":true}`),
			failedPrelude: []byte(
				"event: response.created\n" +
					`data: {"type":"response.created","sequence_number":1,"response":{"id":"failed-prelude","model":"claude-fable-5","status":"in_progress","output":[]}}` +
					"\n\n",
			),
			content: []byte(
				"event: response.output_text.delta\n" +
					`data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg-primary","output_index":0,"content_index":0,"delta":"primary-visible"}` +
					"\n\n",
			),
			fallbackChunks: []pluginapi.HostModelStreamReadResponse{
				{
					Payload: []byte(
						"event: response.created\n" +
							`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp-fallback","object":"response","created_at":1,"status":"in_progress","model":"gpt-5.6-sol","output":[]}}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: response.output_text.delta\n" +
							`data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"msg-fallback","output_index":0,"content_index":0,"delta":"fallback-visible"}` +
							"\n\n",
					),
				},
				{
					Payload: []byte(
						"event: response.completed\n" +
							`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp-fallback","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg-fallback","type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback-visible","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}` +
							"\n\n",
					),
					Done: true,
				},
			},
		},
	}
}

type crossProtocolResponsesEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	Delta          string `json:"delta"`
	Response       struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	} `json:"response"`
}

func assertCrossProtocolResponsesFallbackContract(t *testing.T, payloads [][]byte) {
	t.Helper()
	events := parseCrossProtocolResponsesEvents(t, payloads)
	if len(events) != 3 {
		t.Fatalf("Responses fallback emitted %d frames, want exactly 3: %#v", len(events), events)
	}

	expectedBySequence := map[int]string{
		0: "response.created",
		1: "response.output_text.delta",
		2: "response.completed",
	}
	counts := make(map[string]int, len(expectedBySequence))
	sequenceCounts := make(map[int]int, len(expectedBySequence))
	var deltaText strings.Builder
	completedText := ""
	for _, event := range events {
		counts[event.Type]++
		sequenceCounts[event.SequenceNumber]++
		expectedType, ok := expectedBySequence[event.SequenceNumber]
		if !ok || event.Type != expectedType {
			t.Fatalf(
				"Responses fallback frame sequence=%d type=%q, want %q",
				event.SequenceNumber,
				event.Type,
				expectedType,
			)
		}
		switch event.Type {
		case "response.output_text.delta":
			deltaText.WriteString(event.Delta)
		case "response.completed":
			for _, output := range event.Response.Output {
				for _, content := range output.Content {
					if content.Type == "output_text" {
						completedText += content.Text
					}
				}
			}
		}
	}
	for sequence, eventType := range expectedBySequence {
		if counts[eventType] != 1 || sequenceCounts[sequence] != 1 {
			t.Fatalf(
				"Responses fallback frame %q sequence=%d counts are event=%d sequence=%d, want each exactly once",
				eventType,
				sequence,
				counts[eventType],
				sequenceCounts[sequence],
			)
		}
	}
	if got := deltaText.String(); got != "fallback-visible" {
		t.Fatalf("Responses delta reconstruction = %q, want %q", got, "fallback-visible")
	}
	if completedText != deltaText.String() {
		t.Fatalf(
			"Responses completed aggregate text = %q, reconstructed delta text = %q",
			completedText,
			deltaText.String(),
		)
	}

	visible := joinCrossProtocolPayloads(payloads)
	for _, forbidden := range []string{
		"failed-prelude",
		"claude-fable-5",
		"credits_required",
		"Usage credits are required",
		"monthly spend limit",
		"rate_limit_error",
		"org_level_disabled_until",
		`"type":"error"`,
	} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("Responses fallback leaked failed-provider diagnostic %q: %s", forbidden, visible)
		}
	}
}

func parseCrossProtocolResponsesEvents(
	t *testing.T,
	payloads [][]byte,
) []crossProtocolResponsesEvent {
	t.Helper()
	stream := strings.ReplaceAll(joinCrossProtocolPayloads(payloads), "\r\n", "\n")
	rawFrames := strings.Split(stream, "\n\n")
	events := make([]crossProtocolResponsesEvent, 0, len(rawFrames))
	for _, rawFrame := range rawFrames {
		if strings.TrimSpace(rawFrame) == "" {
			continue
		}
		eventName := ""
		dataLines := make([]string, 0, 1)
		for _, line := range strings.Split(rawFrame, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(dataLines) == 0 {
			t.Fatalf("Responses fallback frame has no data field: %q", rawFrame)
		}
		var event crossProtocolResponsesEvent
		if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &event); err != nil {
			t.Fatalf("decode Responses fallback frame %q: %v", rawFrame, err)
		}
		if eventName == "" {
			eventName = event.Type
		}
		if event.Type == "" || eventName != event.Type {
			t.Fatalf(
				"Responses fallback event/data type mismatch: event=%q data.type=%q",
				eventName,
				event.Type,
			)
		}
		events = append(events, event)
	}
	return events
}

func assertCrossProtocolProviderOrder(
	t *testing.T,
	calls []pluginapi.HostModelExecutionRequest,
	providers ...string,
) {
	t.Helper()
	if len(calls) != len(providers) {
		t.Fatalf("provider calls = %#v, want providers %v", calls, providers)
	}
	for index, provider := range providers {
		call := calls[index]
		if call.ForcedProvider != provider || !call.SingleAttempt || call.AuthID == "" {
			t.Fatalf("provider call %d = %#v, want pinned single-attempt %s", index, call, provider)
		}
		if provider == "claude" &&
			(call.Model != "claude-fable-5" || call.AuthID != "palantir") {
			t.Fatalf("Claude call = %#v, want Palantir Fable", call)
		}
		if provider == "codex" &&
			(call.Model != "gpt-5.6-sol" || call.AuthID != "codex-x20") {
			t.Fatalf("Codex call = %#v, want x20 Sol", call)
		}
	}
}

func assertCrossProtocolSafeDiagnostics(
	t *testing.T,
	observation crossProtocolStreamObservation,
) {
	t.Helper()
	serialized := joinCrossProtocolPayloads(observation.emitted) +
		string(mustJSONValue(t, observation.pluginClose))
	runtimeState.RLock()
	serialized += string(mustJSONValue(t, runtimeState.Attempts))
	runtimeState.RUnlock()
	for _, forbidden := range []string{
		"req_bravo_credits_private",
		"req_private",
		"pm_private",
		"private diagnostic",
		"payment_method",
		"has_chargeable_saved_payment_method",
		"can_user_purchase_credits",
		`{"type":"error"`,
	} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("stream diagnostics leaked %q: %s", forbidden, serialized)
		}
	}
}

func assertCrossProtocolCreditsCooldown(t *testing.T) {
	t.Helper()
	now := time.Now()
	if !cooldownActive("claude", "palantir", "claude-fable-5", now) {
		t.Fatal("Fable credits failure did not create a model-scoped cooldown")
	}
	if cooldownActive("claude", "palantir", "", now) {
		t.Fatal("Fable credits failure created an account-wide cooldown")
	}
	if cooldownActive("claude", "palantir", "claude-sonnet-5", now) {
		t.Fatal("Fable credits failure cooled a sibling Claude model")
	}
	if cooldownActive("codex", "codex-x20", "gpt-5.6-sol", now) {
		t.Fatal("successful fallback received a cooldown")
	}
}

func assertCrossProtocolAccountCooldown(t *testing.T, wantProviderCode string) {
	t.Helper()
	now := time.Now()
	if !cooldownActive("claude", "palantir", "", now) {
		t.Fatal("account-scoped provider failure did not create an account-wide cooldown")
	}
	runtimeState.RLock()
	defer runtimeState.RUnlock()
	entry, ok := runtimeState.Cooldowns[cooldownKey("claude", "palantir", "")]
	if !ok {
		t.Fatal("account-scoped cooldown entry is missing")
	}
	if entry.ProviderError.Type != wantProviderCode ||
		entry.ProviderError.Code != wantProviderCode ||
		entry.ProviderError.Scope != "account" {
		t.Fatalf("account-scoped cooldown provider detail = %#v, want %s", entry.ProviderError, wantProviderCode)
	}
}

func assertCrossProtocolModelCooldown(t *testing.T, wantProviderCode string) {
	t.Helper()
	now := time.Now()
	if !cooldownActive("claude", "palantir", "claude-fable-5", now) {
		t.Fatal("model-scoped provider failure did not create a model cooldown")
	}
	if cooldownActive("claude", "palantir", "", now) {
		t.Fatal("model-scoped provider failure created an account-wide cooldown")
	}
	runtimeState.RLock()
	defer runtimeState.RUnlock()
	entry, ok := runtimeState.Cooldowns[cooldownKey("claude", "palantir", "claude-fable-5")]
	if !ok {
		t.Fatal("model-scoped cooldown entry is missing")
	}
	if entry.ProviderError.Type != wantProviderCode ||
		entry.ProviderError.Code != wantProviderCode ||
		entry.ProviderError.Scope != "model" {
		t.Fatalf("model-scoped cooldown provider detail = %#v, want %s", entry.ProviderError, wantProviderCode)
	}
}

func assertCrossProtocolNoCooldown(t *testing.T) {
	t.Helper()
	now := time.Now()
	for _, key := range []struct {
		provider string
		authID   string
		model    string
	}{
		{provider: "claude", authID: "palantir"},
		{provider: "claude", authID: "palantir", model: "claude-fable-5"},
		{provider: "claude", authID: "palantir", model: "claude-sonnet-5"},
		{provider: "codex", authID: "codex-x20", model: "gpt-5.6-sol"},
	} {
		if cooldownActive(key.provider, key.authID, key.model, now) {
			t.Fatalf("unexpected cooldown for provider=%s auth=%s model=%s", key.provider, key.authID, key.model)
		}
	}
}

func joinCrossProtocolPayloads(payloads [][]byte) string {
	var builder strings.Builder
	for _, payload := range payloads {
		builder.Write(payload)
	}
	return builder.String()
}
