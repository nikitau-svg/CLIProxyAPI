package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func installAdaptiveAssistTestState(t *testing.T, quotas map[string]credentialQuotaState) {
	t.Helper()
	installAdaptiveEnforcementTestState(t, quotas)
	cfg := loadedConfig()
	cfg.AdaptiveAllocatorMode = "assist"
	currentConfig.Store(cfg)
}

func TestAdaptiveAssistCoordinatorDefersExactSecondaryUntilNeighborsFail(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			req, auths := installAdaptiveAssistCoordinatorTest(t, stream)
			var providers []string
			var closed rpcStreamCloseRequest
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostModelExecute:
					var call hostModelExecutionRequest
					decodeBravoPayload(t, payload, &call)
					providers = append(providers, call.ForcedProvider)
					if call.ForcedProvider == "codex" {
						return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusServiceUnavailable}), nil
					}
					return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)}), nil
				case pluginabi.MethodHostModelExecuteStream:
					var call hostModelExecutionRequest
					decodeBravoPayload(t, payload, &call)
					providers = append(providers, call.ForcedProvider)
					if call.ForcedProvider == "codex" {
						return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
					}
					return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "assist-tail"}), nil
				case pluginabi.MethodHostModelStreamRead:
					return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
				case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
					return mustBravoJSON(t, map[string]any{}), nil
				case pluginabi.MethodHostStreamClose:
					decodeBravoPayload(t, payload, &closed)
					return mustBravoJSON(t, map[string]any{}), nil
				default:
					return mustBravoJSON(t, map[string]any{}), nil
				}
			})
			if stream {
				runBravoStream(req, "assist-client")
				if closed.ErrorCode != "" {
					t.Fatalf("stream tail failed: %#v", closed)
				}
			} else {
				raw, err := execute(mustJSONValue(t, req))
				if err != nil {
					t.Fatal(err)
				}
				var env envelope
				if json.Unmarshal(raw, &env) != nil || !env.OK {
					t.Fatalf("nonstream tail failed: %s", raw)
				}
			}
			if got := strings.Join(providers, ","); got != "codex,claude" {
				t.Fatalf("provider calls=%s, want neighbor then sequential exact tail", got)
			}
		})
	}
}

func TestAdaptiveAssistTailSurvivesTerminalNeighborButNotCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []string{"terminal_tail_success", "terminal_tail_failure", "request_canceled"} {
			t.Run(fmt.Sprintf("%v/%s", stream, scenario), func(t *testing.T) {
				req, auths := installAdaptiveAssistCoordinatorTest(t, stream)
				var providers []string
				var closed rpcStreamCloseRequest
				installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
					switch method {
					case pluginabi.MethodHostAuthList:
						return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
					case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
						var call hostModelExecutionRequest
						decodeBravoPayload(t, payload, &call)
						providers = append(providers, call.ForcedProvider)
						if call.ForcedProvider == "codex" && scenario == "request_canceled" {
							return nil, &hostCallError{Code: "request_canceled", Message: "client canceled", HTTPStatus: 499}
						}
						status := http.StatusBadRequest
						if call.ForcedProvider == "claude" && scenario == "terminal_tail_success" {
							status = http.StatusOK
						}
						if stream {
							response := pluginapi.HostModelStreamResponse{StatusCode: status}
							if status == http.StatusOK {
								response.StreamID = "assist-terminal-tail"
							}
							return mustBravoJSON(t, response), nil
						}
						return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: status,
							Body: []byte(`{"error":{"message":"terminal"}}`)}), nil
					case pluginabi.MethodHostModelStreamRead:
						return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
					case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
						return mustBravoJSON(t, map[string]any{}), nil
					case pluginabi.MethodHostStreamClose:
						decodeBravoPayload(t, payload, &closed)
						return mustBravoJSON(t, map[string]any{}), nil
					default:
						return mustBravoJSON(t, map[string]any{}), nil
					}
				})
				if stream {
					runBravoStream(req, "assist-terminal-client")
					if scenario == "terminal_tail_success" && closed.ErrorCode != "" {
						t.Fatalf("tail success closed with error: %#v", closed)
					}
					if scenario != "terminal_tail_success" && (closed.ErrorCode == "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_")) {
						t.Fatalf("terminal stream failure was not sanitized: %#v", closed)
					}
				} else {
					raw, err := execute(mustJSONValue(t, req))
					if err != nil {
						t.Fatal(err)
					}
					var env envelope
					_ = json.Unmarshal(raw, &env)
					if scenario == "terminal_tail_success" && !env.OK {
						t.Fatalf("tail success failed: %s", raw)
					}
					if scenario != "terminal_tail_success" && (env.Error == nil || strings.Contains(env.Error.Code, "bravo_adaptive_")) {
						t.Fatalf("terminal failure was not sanitized: %s", raw)
					}
				}
				want := "codex,claude"
				if scenario == "request_canceled" {
					want = "codex"
				}
				if got := strings.Join(providers, ","); got != want {
					t.Fatalf("provider calls=%s want=%s", got, want)
				}
			})
		}
	}
}

func TestAdaptiveAssistOpenedStreamPreCommitFailureReachesTail(t *testing.T) {
	for _, scenario := range []string{"tail_success", "tail_failure", "request_canceled", "committed_failure"} {
		t.Run(scenario, func(t *testing.T) {
			req, auths := installAdaptiveAssistCoordinatorTest(t, true)
			var providers []string
			reads := make(map[string]int)
			var closed rpcStreamCloseRequest
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostModelExecuteStream:
					var call hostModelExecutionRequest
					decodeBravoPayload(t, payload, &call)
					providers = append(providers, call.ForcedProvider)
					return mustBravoJSON(t, pluginapi.HostModelStreamResponse{
						StatusCode: http.StatusOK, StreamID: call.ForcedProvider + "-opened",
					}), nil
				case pluginabi.MethodHostModelStreamRead:
					var read pluginapi.HostModelStreamReadRequest
					decodeBravoPayload(t, payload, &read)
					reads[read.StreamID]++
					provider := strings.TrimSuffix(read.StreamID, "-opened")
					if provider == "claude" && scenario == "tail_success" {
						return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
					}
					if provider == "codex" && scenario == "request_canceled" {
						return nil, &hostCallError{Code: "request_canceled", Message: "client canceled", HTTPStatus: 499}
					}
					if provider == "codex" && scenario == "committed_failure" && reads[read.StreamID] == 1 {
						return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{
							Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"),
						}), nil
					}
					return nil, &hostCallError{Code: "invalid_request_error", Message: "terminal stream failure", HTTPStatus: http.StatusBadRequest}
				case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
					return mustBravoJSON(t, map[string]any{}), nil
				case pluginabi.MethodHostStreamClose:
					decodeBravoPayload(t, payload, &closed)
					return mustBravoJSON(t, map[string]any{}), nil
				default:
					return mustBravoJSON(t, map[string]any{}), nil
				}
			})
			runBravoStream(req, "assist-opened-client")
			want := "codex,claude"
			if scenario == "request_canceled" || scenario == "committed_failure" {
				want = "codex"
			}
			if got := strings.Join(providers, ","); got != want {
				t.Fatalf("opened stream calls=%s want=%s close=%#v", got, want, closed)
			}
			if scenario == "tail_success" && closed.ErrorCode != "" {
				t.Fatalf("pre-commit tail did not recover: %#v", closed)
			}
			if scenario == "tail_failure" && (closed.ErrorCode == "" || strings.Contains(closed.ErrorCode, "bravo_adaptive_")) {
				t.Fatalf("tail terminal failure leaked local state: %#v", closed)
			}
		})
	}
}

func TestAdaptiveAssistCoordinatorSnapshotsModeAndBudgetAcrossReload(t *testing.T) {
	t.Run("assist max1 to max0 stays inert", func(t *testing.T) {
		req, auths := installAdaptiveAssistCoordinatorTest(t, false)
		cfg := loadedConfig()
		cfg.MaxAttempts = 1
		currentConfig.Store(cfg)
		providers := runAdaptiveAssistReloadCoordinator(t, req, auths, false, func(cfg pluginConfig) pluginConfig {
			cfg.MaxAttempts = 0
			return cfg
		})
		if got := strings.Join(providers, ","); got != "claude" {
			t.Fatalf("max_attempts snapshot changed request: %s", got)
		}
	})
	t.Run("observe stream to assist stays observe", func(t *testing.T) {
		req, auths := installAdaptiveAssistCoordinatorTest(t, true)
		cfg := loadedConfig()
		cfg.AdaptiveAllocatorMode = "observe"
		currentConfig.Store(cfg)
		providers := runAdaptiveAssistReloadCoordinator(t, req, auths, true, func(cfg pluginConfig) pluginConfig {
			cfg.AdaptiveAllocatorMode = "assist"
			return cfg
		})
		if got := strings.Join(providers, ","); got != "claude" {
			t.Fatalf("observe request gained assist/hedge after reload: %s", got)
		}
	})
	t.Run("assist stream to observe stays sequential assist", func(t *testing.T) {
		req, auths := installAdaptiveAssistCoordinatorTest(t, true)
		providers := runAdaptiveAssistReloadCoordinator(t, req, auths, true, func(cfg pluginConfig) pluginConfig {
			cfg.AdaptiveAllocatorMode = "observe"
			return cfg
		})
		if got := strings.Join(providers, ","); got != "codex,claude" {
			t.Fatalf("assist request lost sequential tail after reload: %s", got)
		}
	})
	t.Run("project kill switch makes global assist breaker-only", func(t *testing.T) {
		req, auths := installAdaptiveAssistCoordinatorTest(t, false)
		cfg := loadedConfig()
		cfg.SmartKeys[0].AdaptiveAssist = false
		currentConfig.Store(cfg)
		providers := runAdaptiveAssistReloadCoordinator(t, req, auths, false, func(cfg pluginConfig) pluginConfig { return cfg })
		if got := strings.Join(providers, ","); got != "claude" {
			t.Fatalf("unlisted project was affected by global assist: %s", got)
		}
	})
}

func TestAdaptiveStreamACKFreezesProjectRoutingSnapshot(t *testing.T) {
	for _, test := range []struct {
		name          string
		acceptedMode  string
		acceptedOptIn bool
		acceptedMax   int
		reloadedMode  string
		reloadedOptIn bool
		reloadedMax   int
		wantProviders string
		wantForks     int
	}{
		{name: "assist to breaker opt-out", acceptedMode: "assist", acceptedOptIn: true, acceptedMax: 0,
			reloadedMode: "breaker", reloadedOptIn: false, reloadedMax: 1, wantProviders: "codex,claude", wantForks: 0},
		{name: "breaker opt-out to assist", acceptedMode: "breaker", acceptedOptIn: false, acceptedMax: 0,
			reloadedMode: "assist", reloadedOptIn: true, reloadedMax: 1, wantProviders: "claude", wantForks: 1},
		{name: "bounded breaker remains bounded", acceptedMode: "breaker", acceptedOptIn: false, acceptedMax: 1,
			reloadedMode: "assist", reloadedOptIn: true, reloadedMax: 0, wantProviders: "claude", wantForks: 0},
		{name: "bounded assist keeps baseline hedge", acceptedMode: "assist", acceptedOptIn: true, acceptedMax: 2,
			reloadedMode: "breaker", reloadedOptIn: false, reloadedMax: 0, wantProviders: "claude", wantForks: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetBravoActiveStreamsForTest(t)
			req, auths := installAdaptiveAssistCoordinatorTest(t, true)
			req.StreamID = "ack-snapshot-" + strings.ReplaceAll(test.name, " ", "-")
			cfg := loadedConfig()
			cfg.AdaptiveAllocatorMode = test.acceptedMode
			cfg.MaxAttempts = test.acceptedMax
			cfg.FallbackHedgeDelaySeconds = 60
			cfg.SmartKeys[0].AdaptiveAssist = test.acceptedOptIn
			currentConfig.Store(cfg)

			originalRunner := runBravoStreamAsync
			started := make(chan bravoStreamExecutionSnapshot, 1)
			release := make(chan struct{})
			runBravoStreamAsync = func(req rpcExecutorRequest, id string, recorder *routeTraceRecorder, snapshot bravoStreamExecutionSnapshot) {
				started <- snapshot
				<-release
				originalRunner(req, id, recorder, snapshot)
			}
			t.Cleanup(func() { runBravoStreamAsync = originalRunner })

			var providers []string
			forks := 0
			done := make(chan struct{})
			var doneOnce sync.Once
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
				case pluginabi.MethodHostCallbackFork:
					forks++
					return mustBravoJSON(t, pluginapi.HostCallbackScopeResponse{HostCallbackID: "ack-child"}), nil
				case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostCallbackCommit:
					return mustBravoJSON(t, map[string]any{}), nil
				case pluginabi.MethodHostModelExecuteStream:
					var call hostModelExecutionRequest
					decodeBravoPayload(t, payload, &call)
					providers = append(providers, call.ForcedProvider)
					if call.ForcedProvider == "codex" {
						return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
					}
					return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "ack-upstream"}), nil
				case pluginabi.MethodHostModelStreamRead:
					return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
				case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
					return mustBravoJSON(t, map[string]any{}), nil
				case pluginabi.MethodHostStreamClose:
					doneOnce.Do(func() { close(done) })
					return mustBravoJSON(t, map[string]any{}), nil
				default:
					return mustBravoJSON(t, map[string]any{}), nil
				}
			})

			raw, err := executeStream(mustJSONValue(t, req))
			if err != nil {
				t.Fatal(err)
			}
			var ack envelope
			if json.Unmarshal(raw, &ack) != nil || !ack.OK {
				t.Fatalf("stream ACK failed: %s", raw)
			}
			snapshot := <-started
			wantEffectiveMode := test.acceptedMode
			if wantEffectiveMode == "assist" && !test.acceptedOptIn {
				wantEffectiveMode = "breaker"
			}
			if snapshot.cfg.AdaptiveAllocatorMode != wantEffectiveMode || snapshot.cfg.MaxAttempts != test.acceptedMax {
				t.Fatalf("ACK snapshot=%s/%d want=%s/%d", snapshot.cfg.AdaptiveAllocatorMode, snapshot.cfg.MaxAttempts, wantEffectiveMode, test.acceptedMax)
			}
			reloaded := loadedConfig()
			reloaded.AdaptiveAllocatorMode = test.reloadedMode
			reloaded.MaxAttempts = test.reloadedMax
			reloaded.FallbackHedgeDelaySeconds = 0
			reloaded.SmartKeys[0].AdaptiveAssist = test.reloadedOptIn
			currentConfig.Store(reloaded)
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("accepted stream did not finish")
			}
			if got := strings.Join(providers, ","); got != test.wantProviders || forks != test.wantForks {
				t.Fatalf("post-ACK routing providers=%s forks=%d want=%s/%d", got, forks, test.wantProviders, test.wantForks)
			}
		})
	}
}

func TestGlobalAssistIsInertWithoutAuthenticatedOptInProject(t *testing.T) {
	isolateBravoFallbackTestState(t)
	cfg := defaultPluginConfig()
	cfg.RequireSmartKey = false
	cfg.AllocatorMode = "off"
	cfg.AdaptiveAllocatorMode = "assist"
	cfg.FallbackHedgeDelaySeconds = 60
	cfg.Models = map[string]logicalModel{"public-assist": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Priority: 100, Capabilities: []string{capabilityText, capabilityStream}},
		{Provider: "codex", Model: "gpt-5.6-luna", Priority: 90, Capabilities: []string{capabilityText, capabilityStream}},
	}}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	previous := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
	auths := []pluginapi.HostAuthFileEntry{{ID: "public-claude", Provider: "claude"}, {ID: "public-codex", Provider: "codex"}}
	providers := []string{}
	forks := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostCallbackFork:
			forks++
			return mustBravoJSON(t, pluginapi.HostCallbackScopeResponse{HostCallbackID: "public-child"}), nil
		case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostCallbackCommit:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "public-upstream"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamClose:
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/public-assist", Format: protocolOpenAI, SourceFormat: protocolOpenAI,
		OriginalRequest: []byte(`{"model":"bravo/public-assist","messages":[{"role":"user","content":"ok"}],"stream":true}`),
	}, HostCallbackID: "public-callback"}
	runBravoStream(req, "public-client")
	if got := strings.Join(providers, ","); got != "claude" || forks != 1 {
		t.Fatalf("unauthenticated global assist changed baseline: providers=%s forks=%d", got, forks)
	}
}

func runAdaptiveAssistReloadCoordinator(
	t *testing.T,
	req rpcExecutorRequest,
	auths []pluginapi.HostAuthFileEntry,
	stream bool,
	reload func(pluginConfig) pluginConfig,
) []string {
	t.Helper()
	var providers []string
	var closed rpcStreamCloseRequest
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			currentConfig.Store(reload(loadedConfig()))
			return mustBravoJSON(t, hostAuthListResponse{Files: auths}), nil
		case pluginabi.MethodHostCallbackFork:
			return mustBravoJSON(t, pluginapi.HostCallbackScopeResponse{HostCallbackID: "assist-reload-child"}), nil
		case pluginabi.MethodHostCallbackClose, pluginabi.MethodHostCallbackCommit:
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostModelExecute:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			if call.ForcedProvider == "codex" {
				return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)}), nil
		case pluginabi.MethodHostModelExecuteStream:
			var call hostModelExecutionRequest
			decodeBravoPayload(t, payload, &call)
			providers = append(providers, call.ForcedProvider)
			if call.ForcedProvider == "codex" {
				return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			return mustBravoJSON(t, pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "assist-reload"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return mustBravoJSON(t, pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostStreamClose:
			decodeBravoPayload(t, payload, &closed)
			return mustBravoJSON(t, map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit:
			return mustBravoJSON(t, map[string]any{}), nil
		default:
			return mustBravoJSON(t, map[string]any{}), nil
		}
	})
	if stream {
		runBravoStream(req, "assist-reload-client")
		if closed.ErrorCode != "" {
			t.Fatalf("reload stream failed: %#v", closed)
		}
	} else {
		raw, err := execute(mustJSONValue(t, req))
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if json.Unmarshal(raw, &env) != nil || !env.OK {
			t.Fatalf("reload request failed: %s", raw)
		}
	}
	return providers
}

func installAdaptiveAssistCoordinatorTest(t *testing.T, stream bool) (rpcExecutorRequest, []pluginapi.HostAuthFileEntry) {
	t.Helper()
	isolateBravoFallbackTestState(t)
	restoreUsage := isolateBravoUsageState(t)
	t.Cleanup(restoreUsage)
	resetAdaptiveShadowForTest()
	t.Cleanup(resetAdaptiveShadowForTest)
	resetAdaptiveEdgeGateForTest()
	t.Cleanup(resetAdaptiveEdgeGateForTest)
	const plaintext = "brv_assist_coordinator"
	sum := sha256.Sum256([]byte(plaintext))
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "assist-secondary", AuthIndex: "assist-secondary", Provider: "claude"},
		{ID: "assist-primary", AuthIndex: "assist-primary", Provider: "codex"},
	}
	capabilities := []string{capabilityText}
	if stream {
		capabilities = append(capabilities, capabilityStream)
	}
	cfg := defaultPluginConfig()
	cfg.AllocatorMode = "off"
	cfg.AdaptiveAllocatorMode = "assist"
	cfg.MaxAttempts = 0
	cfg.FallbackHedgeDelaySeconds = 60
	cfg.Models = map[string]logicalModel{"assist-coordinator": {Candidates: []candidate{
		{Provider: "claude", Model: "claude-fable-5", Effort: "max", Priority: 100, Capabilities: capabilities},
		{Provider: "codex", Model: "gpt-5.6-luna", Effort: "max", Priority: 90, Capabilities: capabilities},
	}}}
	cfg.SmartKeys = []smartKeyConfig{{
		ID: "assist-project", Name: "Assist", SHA256: hex.EncodeToString(sum[:]), Models: []string{"*"},
		AdaptiveAssist: true,
		AllowedAuthIDs: []string{"assist-secondary", "assist-primary"}, PrimaryAuthIDs: []string{"assist-primary"},
	}}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	previous := loadedConfig()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
	now := time.Now().UTC()
	storeQuotaSnapshot("assist-secondary", adaptiveEnforcementQuota(now, 50))
	storeQuotaSnapshot("assist-primary", adaptiveEnforcementQuota(now, 80))
	seedAdaptiveAssistCalibration("assist-secondary", "claude", "claude-fable-5", "max", "x1", now)
	body := []byte(`{"model":"bravo/assist-coordinator","messages":[{"role":"user","content":"ok"}]}`)
	if stream {
		body = []byte(`{"model":"bravo/assist-coordinator","messages":[{"role":"user","content":"ok"}],"stream":true}`)
	}
	return rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "bravo/assist-coordinator", Format: protocolOpenAI, SourceFormat: protocolOpenAI,
		Headers: http.Header{"Authorization": []string{"Bearer " + plaintext}}, OriginalRequest: body,
	}, HostCallbackID: "assist-coordinator-callback"}, auths
}

func seedAdaptiveAssistCalibration(auth, provider, model, effort, tariff string, now time.Time) {
	bravoUsageState.mu.Lock()
	defer bravoUsageState.mu.Unlock()
	if bravoUsageState.state.AdaptiveTokenUsageProfiles == nil {
		bravoUsageState.state.AdaptiveTokenUsageProfiles = make(map[string]*persistedAdaptiveTokenUsageProfile)
	}
	if bravoUsageState.state.AdaptiveTokenWindowProfiles == nil {
		bravoUsageState.state.AdaptiveTokenWindowProfiles = make(map[string]*persistedAdaptiveTokenWindowProfile)
	}
	bravoUsageState.state.AdaptiveTokenUsageProfiles[adaptiveTokenUsageProfileKey(auth, provider, model, effort, tariff)] = &persistedAdaptiveTokenUsageProfile{
		AuthIndex: auth, Provider: provider, Model: model, Effort: effort, TariffID: tariff,
		SampleCount: 8, Samples: 8, InputTokens: 8000, OutputTokens: 512, CompletionBuckets: []float64{8}, UpdatedAt: now,
	}
	for _, kind := range []string{pluginapi.HostAuthQuotaWindowKindSession, pluginapi.HostAuthQuotaWindowKindWeekly} {
		bravoUsageState.state.AdaptiveTokenWindowProfiles[adaptiveTokenWindowProfileKey(auth, provider, model, effort, tariff, kind, "")] = &persistedAdaptiveTokenWindowProfile{
			AuthIndex: auth, Provider: provider, Model: model, Effort: effort, TariffID: tariff, WindowKind: kind,
			IntervalSamples: 4, EffectiveIntervals: 4, CoverageSeconds: 3600, EffectiveTokenUnits: 10000,
			AttributedDropPercent: 100, UpdatedAt: now,
		}
	}
}

func adaptiveAssistAttempt(auth string, primary bool, reservation float64, quota credentialQuotaState, now time.Time) executionAttempt {
	attempt := adaptiveEnforcementAttempt(auth, primary, reservation, quota, now)
	attempt.AdaptiveAllocatorMode = "assist"
	attempt.AdaptiveSessionTokenCalibrated = true
	attempt.AdaptiveWeeklyTokenCalibrated = true
	cfg := loadedConfig()
	cfg.AdaptiveAllocatorMode = "assist"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(cfg, attempt, normalizedQuotaState(quota), tariffByID(cfg, "x1"), now)
	return attempt
}

func TestAdaptiveAssistPublicTruthAndFailOpenBoundaries(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 5)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"assist": quota})
	view := adaptiveShadowSummary(loadedConfig(), []string{"assist"}, now)
	if view.Mode != "assist" || view.Effect != "soft_assist_routing_enforced" || !view.SoftAssistEnabled ||
		view.ForecastRoutingEnforced || !view.RoutingEnforced || view.AdditionalProviderRequests {
		t.Fatalf("assist public view=%#v", view)
	}

	for _, test := range []struct {
		name   string
		mutate func(*executionAttempt)
	}{
		{name: "primary", mutate: func(a *executionAttempt) { a.Primary = true }},
		{name: "partial", mutate: func(a *executionAttempt) { a.AdaptiveWeeklyTokenCalibrated = false }},
		{name: "tail", mutate: func(a *executionAttempt) { a.AdaptiveAssistTail = true }},
		{name: "allocator bypass", mutate: func(a *executionAttempt) { a.AllocatorBypass = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := adaptiveAssistAttempt("assist", false, 5, quota, now)
			test.mutate(&attempt)
			release, acquired, failure := acquireAdaptiveEnforcementLease(attempt, now)
			if !acquired || failure != nil {
				t.Fatalf("boundary did not fail open: acquired=%v failure=%#v", acquired, failure)
			}
			release(false)
		})
	}
}

func TestAdaptiveAssistMaxAttemptsIsForecastInert(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 5)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"bounded": quota})
	for _, maxAttempts := range []int{1, 2} {
		cfg := loadedConfig()
		cfg.MaxAttempts = maxAttempts
		currentConfig.Store(cfg)
		attempt := adaptiveAssistAttempt("bounded", false, 5, quota, now)
		release, acquired, failure := acquireAdaptiveEnforcementLease(attempt, now)
		if !acquired || failure != nil {
			t.Fatalf("max_attempts=%d changed baseline: acquired=%v failure=%#v", maxAttempts, acquired, failure)
		}
		release(false)
	}
}

func TestAdaptiveAssistConcurrentReservationsAreAtomicAndNonBlocking(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 70)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"assist-burst": quota})
	const workers = 100
	start := make(chan struct{})
	type result struct {
		release func(bool)
		ok      bool
		failure *executionFailure
	}
	results := make(chan result, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for i := 0; i < workers; i++ {
		attempt := adaptiveAssistAttempt("assist-burst", false, 5, quota, now)
		go func() {
			defer group.Done()
			<-start
			release, ok, failure := acquireAdaptiveEnforcementLease(attempt, now)
			results <- result{release: release, ok: ok, failure: failure}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("assist acquisition waited")
	}
	acquired, deferred := 0, 0
	var releases []func(bool)
	for i := 0; i < workers; i++ {
		item := <-results
		if item.ok {
			acquired++
			releases = append(releases, item.release)
		} else if item.failure != nil && item.failure.Code == "bravo_adaptive_quota_withheld" {
			deferred++
		}
	}
	if acquired != 3 || deferred != workers-3 {
		t.Fatalf("acquired=%d deferred=%d", acquired, deferred)
	}
	for _, release := range releases {
		release(false)
	}
}

func TestAdaptiveAssistModeSnapshotSurvivesHotReload(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 5)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"reload-assist": quota})
	attempt := adaptiveAssistAttempt("reload-assist", false, 5, quota, now)
	cfg := loadedConfig()
	cfg.AdaptiveAllocatorMode = "observe"
	currentConfig.Store(cfg)
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure == nil || failure.Code != "bravo_adaptive_quota_withheld" {
		t.Fatalf("assist snapshot lost after reload: acquired=%v failure=%#v", acquired, failure)
	}
}

func TestAdaptiveAssistHotReloadCannotRetroactivelyDeferObservePlan(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 5)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"reload-observe-assist": quota})
	attempt := adaptiveAssistAttempt("reload-observe-assist", false, 5, quota, now)
	attempt.AdaptiveAllocatorMode = "observe"
	planCfg := loadedConfig()
	planCfg.AdaptiveAllocatorMode = "observe"
	attempt.AdaptiveEdgeGate = newAdaptiveEdgeGateAttemptState(planCfg, attempt, normalizedQuotaState(quota), tariffByID(planCfg, "x1"), now)
	release, acquired, failure := acquireExecutionAttemptLease(attempt)
	if !acquired || failure != nil {
		t.Fatalf("observe snapshot gained assist authority: acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
}

func TestAdaptiveAssistTailCopyIsExactAndForecastFailOpen(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 5)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"tail-auth": quota})
	attempt := adaptiveAssistAttempt("tail-auth", false, 5, quota, now)
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure == nil || !adaptiveAssistDeferredEligible(attempt, *failure) {
		t.Fatalf("initial defer acquired=%v failure=%#v", acquired, failure)
	}
	tail := adaptiveAssistTailAttempt(attempt)
	if tail.Auth.AuthIndex != attempt.Auth.AuthIndex || tail.Candidate.Provider != attempt.Candidate.Provider ||
		tail.Candidate.Model != attempt.Candidate.Model || !tail.AdaptiveAssistTail {
		t.Fatalf("tail changed authorized candidate: before=%#v after=%#v", attempt, tail)
	}
	release, acquired, failure := acquireExecutionAttemptLease(tail)
	if !acquired || failure != nil {
		t.Fatalf("tail did not fail open: acquired=%v failure=%#v", acquired, failure)
	}
	release(false)
}

func TestAdaptiveAssistBreakerRecoveryRemainsGlobalSingleFlight(t *testing.T) {
	now := time.Now().UTC()
	quota := adaptiveEnforcementQuota(now, 80)
	installAdaptiveAssistTestState(t, map[string]credentialQuotaState{"assist-breaker": quota})
	seed := adaptiveAssistAttempt("assist-breaker", false, 1, quota, now)
	beginAdaptiveEdgeGateShadow(seed, now)
	observeAdaptiveEdgeGateOutcome(seed, false, executionFailure{Status: http.StatusTooManyRequests, RetryAfter: "60"}, now)
	attempt := adaptiveAssistAttempt("assist-breaker", false, 1, quota, now.Add(time.Millisecond))
	_, acquired, failure := acquireExecutionAttemptLease(attempt)
	if acquired || failure == nil || !adaptiveBreakerLastChanceEligible(attempt, *failure) {
		t.Fatalf("active assist breaker acquired=%v failure=%#v", acquired, failure)
	}
	recovery := adaptiveBreakerLastChanceAttempt(attempt)
	const workers = 100
	start := make(chan struct{})
	type result struct {
		release func(bool)
		ok      bool
	}
	results := make(chan result, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer group.Done()
			<-start
			release, ok, _ := acquireExecutionAttemptLease(recovery)
			results <- result{release: release, ok: ok}
		}()
	}
	close(start)
	group.Wait()
	var winner func(bool)
	for i := 0; i < workers; i++ {
		item := <-results
		if item.ok {
			if winner != nil {
				t.Fatal("assist allowed overlapping breaker recovery calls")
			}
			winner = item.release
		}
	}
	if winner == nil {
		t.Fatal("assist lost the one baseline recovery call")
	}
	winner(false)
}
