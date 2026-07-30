package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// A 503 without Retry-After tells an SDK "retry now", so the client walks
// straight back into the pool that just turned it away. The streaming path
// synthesizes the hint in closePluginStreamFailure; these cases pin the same
// guarantee for the non-streaming envelope, which previously answered bare.
func TestFailureEnvelopeAlwaysCarriesRetryAfterOnPoolExhaustion(t *testing.T) {
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider:     "claude",
		Model:        "claude-opus-4-8",
		Capabilities: []string{capabilityText},
	}}})
	cooldown := loadedConfig().CooldownSeconds
	if cooldown <= 0 {
		t.Fatalf("test config cooldown_seconds = %d, want a positive default", cooldown)
	}

	tests := []struct {
		name    string
		failure executionFailure
		want    string
	}{
		{
			name: "synthesized from cooldown",
			failure: executionFailure{
				Code:    "bravo_no_eligible_account",
				Message: "Bravo has no healthy account for logical model opus",
				Status:  http.StatusServiceUnavailable,
			},
			want: strconv.Itoa(cooldown),
		},
		{
			name: "upstream hint preserved",
			failure: executionFailure{
				Code:       "bravo_no_eligible_account",
				Message:    "pool exhausted",
				Status:     http.StatusServiceUnavailable,
				RetryAfter: "7",
			},
			want: "7",
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			var env envelope
			if errUnmarshal := json.Unmarshal(failureEnvelope(testCase.failure), &env); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if env.Error == nil {
				t.Fatalf("envelope carries no error: %#v", env)
			}
			if env.Error.RetryAfter != testCase.want {
				t.Fatalf("retry_after = %q, want %q", env.Error.RetryAfter, testCase.want)
			}
		})
	}
}

// A contract rejection is permanent for the request as written, so retrying it
// unchanged cannot succeed. Those failures must stay free of a backoff hint.
func TestFailureEnvelopeOmitsRetryAfterForNonRetryableStatus(t *testing.T) {
	failure := executionFailure{
		Code:    "bravo_capability_undeclared",
		Message: "candidate codex does not declare required capability reasoning",
		Status:  http.StatusUnprocessableEntity,
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(failureEnvelope(failure), &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.Error == nil {
		t.Fatalf("envelope carries no error: %#v", env)
	}
	if env.Error.RetryAfter != "" {
		t.Fatalf("retry_after = %q, want empty for a contract rejection", env.Error.RetryAfter)
	}
}

func TestFinalExecutionFailureNormalizesTransientRouteExhaustion(t *testing.T) {
	installBravoTestConfig(t, logicalModel{Candidates: []candidate{{
		Provider:     "claude",
		Model:        "claude-opus-5",
		Capabilities: []string{capabilityText},
	}}})

	failure := finalExecutionFailure([]executionFailureTrace{
		{
			Provider: "claude",
			Model:    "claude-opus-5",
			Failure: executionFailure{
				Code:      "billing_error",
				Message:   "The provider reported a billing restriction.",
				Status:    http.StatusPaymentRequired,
				Retryable: true,
			},
		},
		{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
			Failure: executionFailure{
				Code:      "server_error",
				Message:   "The provider encountered an internal error.",
				Status:    http.StatusBadGateway,
				Retryable: true,
			},
		},
	}, executionFailure{
		Code:      "server_error",
		Message:   "The provider encountered an internal error.",
		Status:    http.StatusBadGateway,
		Retryable: true,
	})

	if failure.Status != http.StatusServiceUnavailable ||
		failure.Code != "bravo_route_temporarily_unavailable" ||
		!failure.Retryable ||
		failure.Provider != nil {
		t.Fatalf("final transient route failure = %#v, want safe retryable 503 aggregate", failure)
	}
	if failure.Message == "" {
		t.Fatal("final transient route failure lost its safe route summary")
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(failureEnvelope(failure), &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.Error == nil || env.Error.RetryAfter == "" {
		t.Fatalf("transient route envelope = %#v, want Retry-After", env)
	}
}
