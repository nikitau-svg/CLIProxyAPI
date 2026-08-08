package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProviderAcceptanceCancellationControlsDurableLeaseAccounting(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		stream     bool
		started    bool
		ambiguous  bool
		retainDebt bool
	}{
		{name: "nonstream proven pre-provider"},
		{name: "nonstream after provider start", started: true, ambiguous: true, retainDebt: true},
		{name: "stream proven pre-provider", stream: true},
		{name: "stream after provider start", stream: true, started: true, ambiguous: true, retainDebt: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restoreUsage := isolateBravoUsageState(t)
			defer restoreUsage()
			isolateBravoFallbackTestState(t)
			resetAdaptiveReserveForTest()
			defer resetAdaptiveReserveForTest()
			path := filepath.Join(t.TempDir(), "cancellation-state.json")
			if errConfigure := configureUsageState(path); errConfigure != nil {
				t.Fatal(errConfigure)
			}

			authIndex := "cancellation-accounting-auth"
			cfg := defaultPluginConfig()
			cfg.Enabled = true
			cfg.AllocatorMode = "enforce"
			cfg.FallbackHedgeDelaySeconds = 0
			cfg.Tariffs = []tariffConfig{{ID: "x1", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 1, ReservationPercent: 1}}
			cfg.Subscriptions = []subscriptionConfig{{AuthIndex: authIndex, Tariff: "x1"}}
			cfg.SmartKeys = []smartKeyConfig{{
				ID: "cancellation-project", Name: "Cancellation project", SHA256: strings.Repeat("a", 64),
				Enabled: boolPointer(true), Status: projectStatusActive, Models: []string{"*"},
			}}
			cfg.Models = map[string]logicalModel{"cancellation-route": {Candidates: []candidate{{
				Provider: "claude", Model: "claude-fable-5", Priority: 100,
				Capabilities: []string{capabilityText, capabilityStream},
			}}}}
			if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
				t.Fatal(errNormalize)
			}
			previousConfig := loadedConfig()
			currentConfig.Store(cfg)
			defer currentConfig.Store(previousConfig)
			setAdaptivePersistenceQuota(t, authIndex, 90)

			started := testCase.started
			rawCancellationEnvelope := mustBravoJSON(t, envelope{OK: false, Error: &envelopeError{
				Code: "request_canceled", Message: "client request was canceled", HTTPStatus: 499,
				ProviderStarted: &started, ProviderExecutionAmbiguous: testCase.ambiguous,
			}})
			var streamClose rpcStreamCloseRequest
			installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
						ID: "cancellation-auth-id", AuthIndex: authIndex, Provider: "claude",
					}}}), nil
				case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
					// Exercise the same JSON ABI decoder as the dynamic host bridge;
					// explicit false must survive the envelope round trip.
					return decodeHostABIResponse(method, 0, rawCancellationEnvelope)
				case pluginabi.MethodHostStreamClose:
					decodeBravoPayload(t, payload, &streamClose)
					return json.RawMessage(`{}`), nil
				case pluginabi.MethodHostLog:
					return json.RawMessage(`{}`), nil
				default:
					return json.RawMessage(`{}`), nil
				}
			})

			body := []byte(`{"model":"bravo/cancellation-route","messages":[{"role":"user","content":"continue"}]}`)
			if testCase.stream {
				body = []byte(`{"model":"bravo/cancellation-route","messages":[{"role":"user","content":"continue"}],"stream":true}`)
			}
			request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "bravo/cancellation-route", Format: protocolClaude, SourceFormat: protocolClaude,
				OriginalRequest: body, Metadata: compactProjectMetadata("cancellation-project"),
			}, HostCallbackID: "cancellation-callback"}
			if testCase.stream {
				runBravoStream(request, "cancellation-stream")
				if streamClose.ErrorCode != "request_canceled" {
					t.Fatalf("stream close = %#v", streamClose)
				}
			} else {
				raw, errExecute := execute(mustJSONValue(t, request))
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				var env envelope
				if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.OK || env.Error == nil ||
					env.Error.Code != "request_canceled" || env.Error.ProviderStarted == nil ||
					*env.Error.ProviderStarted != testCase.started || env.Error.ProviderExecutionAmbiguous != testCase.ambiguous {
					t.Fatalf("cancellation response = %s error=%v", raw, errUnmarshal)
				}
			}

			pendingBeforeRestart := pendingReservationPercent(authIndex)
			if (!testCase.retainDebt && pendingBeforeRestart != 0) || (testCase.retainDebt && pendingBeforeRestart <= 0) {
				t.Fatalf("pending after cancellation = %.3f, retain=%t", pendingBeforeRestart, testCase.retainDebt)
			}
			traces, _, errList := listCurrentRouteTraces(routeTraceQuery{ProjectID: "cancellation-project", ErrorsOnly: true, Limit: 2}, time.Now().UTC())
			if errList != nil || len(traces) != 1 || len(traces[0].Attempts) != 1 {
				t.Fatalf("cancellation traces = %#v error=%v", traces, errList)
			}
			attempt := traces[0].Attempts[0]
			if attempt.ProviderStarted == nil || *attempt.ProviderStarted != testCase.started ||
				attempt.ProviderExecutionAmbiguous != testCase.ambiguous {
				t.Fatalf("cancellation trace attempt = %#v", attempt)
			}
			if attempt.Committed != testCase.retainDebt {
				t.Fatalf("cancellation trace committed=%t, pending retained=%t", attempt.Committed, testCase.retainDebt)
			}

			resetAdaptiveReserveForTest()
			simulateFreshBravoProcess(t, path)
			if got := pendingReservationPercent(authIndex); got != pendingBeforeRestart {
				t.Fatalf("pending after restart = %.3f, want %.3f", got, pendingBeforeRestart)
			}
		})
	}
}

func TestLegacyCancellationWithoutAcceptanceProofRetainsDebt(t *testing.T) {
	started := false
	failure := classifyExecutionError(&hostCallError{
		Code: "request_canceled", Message: "legacy callback", HTTPStatus: 499,
	})
	if failure.ProviderStarted != nil || provenPreProviderCancellation(failure) {
		t.Fatalf("legacy cancellation was treated as proven: %#v started=%t", failure, started)
	}
	if failure.Status != 499 || failure.Retryable || failure.Headers.Get("Retry-After") != "" {
		t.Fatalf("legacy cancellation classification = %#v", failure)
	}
}
