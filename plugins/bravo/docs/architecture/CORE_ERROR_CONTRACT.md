# Core provider failure contract

Status: draft for the Bravo large build. This document is normative for the
core/executor/auth/plugin error path. It does not authorize a production
deployment by itself.

## 1. Problem and scope

The same upstream failure currently has different meanings depending on the
path it takes:

- the direct Claude handler may extract the original JSON message;
- the Claude executor may replace that message with a generic safe message;
- the internal model executor may lose the provider detail and emit only
  `model_execution_failed`;
- the plugin ABI can transport `provider_error`, but only if an earlier layer
  retained it;
- the auth conductor reconstructs request scope by searching English text such
  as `invalid_request_error` in `Error.Message`.

This contract makes the machine-readable classification authoritative. Human
text is presentation only and MUST NOT control retry, fallback, cooldown, or
subscription health.

The required parity covers all of these paths:

1. Claude HTTP error returned before a non-streaming response;
2. Claude SSE `event: error` observed during a non-streaming execution;
3. Claude SSE `event: error` observed during a streaming execution;
4. direct Claude-compatible API response;
5. nested non-streaming `host.model.execute` callback;
6. nested streaming `host.model.execute_stream` / `host.model.stream.read`
   callback;
7. auth result accounting and persisted cooldown state.

Parity means identical class, scope, stable code, HTTP status, retryability,
safe numeric facts, and subscription-state effect. Wire envelopes may still
differ between OpenAI and Claude protocols.

## 2. Normative vocabulary

### 2.1 Failure scope

Scope answers which input can be changed to make the same operation valid.

| Scope | Meaning | Permitted health mutation |
| --- | --- | --- |
| `request` | The current request or conversation is invalid for the selected physical model. Trying another credential for that same model cannot fix it. | None. Do not mark an account or model unhealthy. |
| `model` | The provider model is temporarily or permanently unavailable to this credential. | Only `Auth.ModelStates[physicalModel]`. |
| `account` | The credential/workspace itself is unavailable independent of model. | Account-level state; the selector must exclude all of its models for the applicable cooldown. |
| empty/unknown | The core could not prove a scope. | No durable health mutation and no automatic route fallback. Return a terminal diagnostic and record telemetry. |

`unknown` is deliberately availability-neutral and route-terminal. Treating an
unknown failure as `request` could repeat a dangerous request; treating it as
`account` could evict a healthy subscription.

### 2.2 Failure class

The initial closed taxonomy is:

| Class | Default scope | Notes |
| --- | --- | --- |
| `invalid_request` | `request` | Schema, parameter, or protocol contract error. |
| `context_window` | `request` | Input token count exceeds the selected physical model's context window. |
| `payload_too_large` | `request` | HTTP/body byte-size limit, not token context. |
| `authentication` | `account` | Credential is expired, revoked, or rejected. |
| `permission` | `account` | Workspace/account permission failure. A specifically reviewed model-entitlement error may use `model`. |
| `billing` | `account` | Account/workspace billing restriction. |
| `quota` | `model` | Included allowance or credits for a specific model are exhausted. A reviewed provider signal may override scope to `account`. |
| `rate_limit` | `model` | Temporary model rate limit for the selected credential. |
| `not_found` | `request` | Request-scoped resource is missing. A reviewed missing-model signal uses `model`. |
| `conflict` | `request` | Request conflicts with provider state. |
| `timeout` | `model` | Provider execution timeout. |
| `overloaded` | `model` | Provider capacity failure. |
| `provider_internal` | `model` | Reviewed provider server failure. |
| `transport` | `model` | Connection/proxy/TLS failure before a usable response. |
| `canceled` | `request` | Caller or losing-route cancellation; never provider health. |

New class or scope values MUST be reviewed and added to this table before they
can affect routing. Unknown values are handled as unknown.

## 3. Canonical Go and JSON types

The existing `providererror.Detail` remains the cross-boundary safe object.
The change is additive. Aliases rather than distinct named string types are
intentional for Go source compatibility with existing external plugins.

```go
package providererror

type FailureClass = string
type FailureScope = string

const FailureTaxonomyV1 uint8 = 1

const (
    ClassInvalidRequest   FailureClass = "invalid_request"
    ClassContextWindow    FailureClass = "context_window"
    ClassPayloadTooLarge  FailureClass = "payload_too_large"
    ClassAuthentication  FailureClass = "authentication"
    ClassPermission      FailureClass = "permission"
    ClassBilling         FailureClass = "billing"
    ClassQuota           FailureClass = "quota"
    ClassRateLimit       FailureClass = "rate_limit"
    ClassNotFound        FailureClass = "not_found"
    ClassConflict        FailureClass = "conflict"
    ClassTimeout         FailureClass = "timeout"
    ClassOverloaded      FailureClass = "overloaded"
    ClassProviderInternal FailureClass = "provider_internal"
    ClassTransport       FailureClass = "transport"
    ClassCanceled        FailureClass = "canceled"
)

const (
    ScopeRequest FailureScope = "request"
    ScopeModel   FailureScope = "model"
    ScopeAccount FailureScope = "account"
)

type Detail struct {
    // Existing reviewed provider fields. Their JSON names remain unchanged.
    Type             string `json:"type,omitempty"`
    Code             string `json:"code,omitempty"`
    Message          string `json:"message,omitempty"`
    Model            string `json:"model,omitempty"`
    ModelDisplayName string `json:"model_display_name,omitempty"`
    NoticeTitle      string `json:"notice_title,omitempty"`
    NoticeText       string `json:"notice_text,omitempty"`
    DisabledReason   string `json:"disabled_reason,omitempty"`
    Scope            FailureScope `json:"scope,omitempty"`
    Reason           string `json:"reason,omitempty"`

    // V1 taxonomy additions.
    TaxonomyVersion uint8        `json:"taxonomy_version,omitempty"`
    Class           FailureClass `json:"class,omitempty"`
    RequiredTokens  int64        `json:"required_tokens,omitempty"`
    LimitTokens     int64        `json:"limit_tokens,omitempty"`
}
```

`Type` is the reviewed provider envelope type, for example
`invalid_request_error`. `Code` is the stable normalized reason consumed by the
host/plugin, for example `context_window_exceeded`. `Class` and `Scope` drive
policy. `Message` is a bounded, provider-neutral summary derived by the core.
The two token fields are facts, not estimates.

The public auth error is a legacy four-field source-compatibility boundary.
It MUST remain unchanged because external clients may still use positional Go
literals:

```go
package auth

type Error struct {
    Code       string `json:"code,omitempty"`
    Message    string `json:"message"`
    Retryable  bool   `json:"retryable"`
    HTTPStatus int    `json:"http_status,omitempty"`
}
```

The core MUST flatten only reviewed `Code`, `Message`, `Retryable`, and
`HTTPStatus` values into auth/model state and cooldown records. The complete
safe `providererror.Detail` remains authoritative in executor/plugin envelopes
and Bravo route traces. No auth policy may recover scope by inspecting the
human message. Adding a fifth field to `auth.Error` is a breaking change and is
forbidden without a new major API version.

During the migration, an untyped legacy error with an empty `Code` may pass
through the pre-existing, narrowly enumerated compatibility recognizers for
`invalid_request_error`, model-support, invalid-grant, Cloudflare challenge,
and the OpenAI `store=false` not-found response. This exception MUST NOT run
when any machine-readable code is present, MUST NOT be extended for new
providers, and may be removed only in a major release after all built-in
executors emit `providererror.Detail`.

No new ABI envelope is required. The existing fields remain authoritative:

```go
type HostModelExecutionError struct {
    Code          string                `json:"code,omitempty"`
    Message       string                `json:"message"`
    HTTPStatus    int                   `json:"http_status,omitempty"`
    Retryable     bool                  `json:"retryable,omitempty"`
    Headers       http.Header           `json:"headers,omitempty"`
    RetryAfter    string                `json:"retry_after,omitempty"`
    ProviderError *providererror.Detail `json:"provider_error,omitempty"`
}
```

The same extended `Detail` flows through `pluginabi.Error.ProviderError`,
`ModelExecutionStreamError.ProviderError`, and
`HostModelExecutionError.ProviderError`. The host MUST NOT reconstruct a detail
from the human message when a typed detail is absent.

The following invariants are mandatory at every boundary:

- `HostModelExecutionError.Code == ProviderError.Code` when provider code is
  present;
- `HTTPStatus` and `Retryable` equal the source `Classification` values;
- `Message == ProviderError.Summary()` for reviewed provider failures;
- a taxonomy-v1 detail has a known non-empty `Class` and `Scope`;
- `context_window` always has `Code=context_window_exceeded`, status 400,
  `Retryable=false`, and `Scope=request`;
- arbitrary upstream JSON, request IDs, credentials, payment metadata, prompts,
  and arbitrary provider-authored messages never cross the typed boundary.

`Retryable` means that retrying an eligible physical candidate can be
reasonable. It does not override scope and does not by itself authorize Bravo
route fallback.

## 4. Anthropic context overflow parser

The parser operates on the already JSON-decoded `error.message`; it MUST NOT
search the whole raw response. It recognizes only an Anthropic
`invalid_request_error` and this anchored shape:

```text
(?i)^\s*prompt is too long:\s*([0-9]{1,12})\s+tokens?\s*>\s*([0-9]{1,12})\s+maximum\s*$
```

Both numbers are parsed with `strconv.ParseInt(..., 10, 64)`. Recognition fails
closed unless:

- both values are greater than zero;
- `required_tokens > limit_tokens`;
- neither value exceeds `1_000_000_000_000`;
- the full provider payload is within the existing 256 KiB parser limit;
- the envelope is exactly a top-level Anthropic `type=error` envelope.

On success the parser emits this classification, without copying the upstream
message:

```go
Classification{
    Detail: Detail{
        Type:             "invalid_request_error",
        Code:             "context_window_exceeded",
        Message:          fmt.Sprintf("Input requires %d tokens and exceeds the model context limit of %d tokens.", required, limit),
        Scope:            ScopeRequest,
        Reason:           "prompt_too_long",
        TaxonomyVersion:  FailureTaxonomyV1,
        Class:            ClassContextWindow,
        RequiredTokens:   required,
        LimitTokens:      limit,
    },
    Status:    http.StatusBadRequest,
    Retryable: false,
}
```

The parser runs before the generic Anthropic standard-error mapping in both
`claudeProviderStreamError` and `claudeHTTPStatusError`. The existing phrases
such as `input exceeds the context window` must be normalized through the same
classifier; they produce the same class/scope/code but leave token facts zero
when the provider supplied no reviewed counts.

An unmatched or malformed message falls through to the generic reviewed
`invalid_request_error` classification. It MUST NOT retain the provider message
and MUST NOT be classified later by substring matching.

## 5. Core behavior by scope

### Request

- The auth manager records the request failure counter/telemetry only.
- It does not set `Auth.Unavailable`, `Auth.StatusError`, `Auth.LastError`, a
  model state, model quota, registry suspension, cooldown, or retry deadline.
- The core does not try another credential for the same physical model.
- `context_window` may be offered to Bravo as a terminal candidate result.
  Bravo may choose another physical model only when separate catalog metadata
  proves that model can accept the request. This is not a core auth retry.

### Model

- Only `Auth.ModelStates[physicalModel]` is changed.
- No account-global unavailable flag is set.
- `Retry-After`, when valid, controls the model cooldown.
- Another credential or physical model may be considered by higher-level
  routing policy.

### Account

- Account-global state is changed and applies to all models on that credential.
- A model argument on `auth.Result` does not narrow an account-scoped failure.
- Existing per-model states are not rewritten merely to duplicate the account
  state.

### Unknown

- No durable auth/model health mutation.
- No automatic same-request fallback.
- Emit a stable `unclassified_provider_failure` terminal error and structured
  telemetry for later review.

The conductor's scope decision MUST use, in order:

1. a valid taxonomy-v1 `ProviderError.Scope`;
2. explicit core-owned interfaces/codes (`IsRequestScoped`,
   `request_scoped`, `request_canceled`) for internal errors;
3. status/code mappings maintained in a closed core table for legacy typed
   errors;
4. unknown.

It MUST NOT inspect `Error.Message`, raw JSON text, localized text, or provider
phrases. In particular, `isRequestInvalidResultError` and
`isRequestInvalidError` must not search for `invalid_request_error`,
`bad_request_error`, `INVALID_ARGUMENT`, or `FAILED_PRECONDITION` in message
text.

## 6. Streaming and direct-response rules

- The HTTP and SSE parsers call the same classifier function and construct the
  same typed error implementation.
- A streaming provider error detected before user-visible output is delivered
  as a terminal typed stream error and may participate in safe routing.
- An error after user-visible output retains the same classification, but no
  cross-provider replay is authorized by this contract.
- `ExecuteModel` and `ExecuteModelStream` expose identical failure metadata.
- `host.model.execute` returns the detail through `pluginabi.Error`;
  `host.model.stream.read` returns it through `ErrorDetail`. No field may be
  dropped between them.
- The direct Claude handler obtains type/code/message from the typed detail. It
  may serialize the provider-neutral message into the Claude error envelope,
  but it must not reparse arbitrary `err.Error()` JSON.
- API localization happens after classification. A Russian string and an
  English string for the same failure must carry the same stable code and must
  produce the same state transition.

## 7. Compatibility and rollout

This is an additive JSON change:

- old plugins ignore `taxonomy_version`, `class`, `required_tokens`, and
  `limit_tokens`;
- new plugins accept old details but treat details without a proven scope/class
  as legacy/unknown unless an existing reviewed code is in the closed mapping;
- keeping `FailureClass` and `FailureScope` as aliases preserves Go composite
  literals that currently assign string variables;
- existing JSON names and existing `ProviderError` locations do not change;
- persisted cooldown records without `provider_error` remain readable.

Rollout order:

1. add taxonomy types, sanitizer support, exact parser, and typed executor
   errors;
2. propagate the detail through non-streaming, streaming, RPC, and plugin ABI
   tests;
3. persist the detail on `auth.Error` and make conductor mutations scope-driven;
4. update Bravo to consume taxonomy-v1 fields;
5. only after parity tests pass, remove message-based conductor fallbacks;
6. canary with isolated credentials and synthetic provider fixtures before any
   production container replacement.

During mixed-version operation, absence of the new fields is not evidence of
request scope. Unknown remains terminal and availability-neutral.

## 8. Fail-closed rules

1. Only reviewed parsers may create taxonomy-v1 details.
2. Unknown provider type, code, phrase, scope, or class does not authorize retry
   or fallback.
3. A message containing a known keyword is not a typed failure.
4. Parsed numeric facts must satisfy the constraints in section 4.
5. Context overflow never cools down or marks a subscription unhealthy.
6. Account failures are never narrowed to model scope merely because the
   request had a model name.
7. Model failures are never widened to the whole account without a reviewed
   account-scoped signal.
8. `Retryable=true` cannot override `Scope=request`.
9. Raw provider bodies and request IDs never enter `Detail`, auth persistence,
   plugin ABI, analytics, or user-facing diagnostics.
10. If safe classification and raw provider semantics disagree, return the
    safe unknown terminal error and preserve only redacted telemetry.

## 9. Test matrix

Every row is required before the build may enter canary.

| Area | Fixture / operation | Required assertion |
| --- | --- | --- |
| Provider parser | Exact `prompt is too long: 1003466 tokens > 1000000 maximum` envelope | Taxonomy v1; `context_window`; request scope; code `context_window_exceeded`; 400; non-retryable; exact numeric fields. |
| Provider parser | Leading/trailing whitespace and singular `token` | Accepted and normalized, no provider-authored message retained. |
| Provider parser | Required equals/below limit, zero, negative, 13-digit number, integer overflow | Rejected; no partial detail. |
| Provider parser | Valid prefix plus `request_id=...`, newline, or arbitrary suffix | Rejected by the anchored parser; suffix never appears in output. |
| Provider parser | Private fields in envelope (`request_id`, payment/token metadata) | Serialized classification contains none of them. |
| Provider parser | Existing context phrases without counts | Same class/scope/code; token fields omitted. |
| Provider parser | Generic `invalid_request_error` | Class `invalid_request`, request scope, generic safe message. |
| Provider parser | All documented Anthropic standard types | Exact class/scope/status/retryable mapping from section 2. |
| Claude HTTP executor | HTTP 400 with exact prompt-too-long body | Typed error exposes the canonical detail and normalized message. |
| Claude stream executor | SSE terminal error before content | Same detail as HTTP execution; no raw event in `Error()`. |
| Claude non-stream SSE | Provider returns SSE error to `Execute` | Same detail as streaming and HTTP paths. |
| Stream timing | Same error after one visible content chunk | Same classification, but no automatic cross-provider replay. |
| Internal model execution | `ExecuteModel` receives typed context failure | `pluginabi.Error.ProviderError` round-trips every taxonomy field. |
| Internal stream bridge | `ExecuteModelStream` then `stream.read` | `HostModelExecutionError.ProviderError` is field-for-field equal to non-streaming. |
| RPC round trip | Marshal/unmarshal `pluginabi.Error` and `HostModelExecutionError` | No loss of class, scope, code, token counts, status, retryability, or retry-after. |
| Direct Claude handler | Typed context failure | Claude envelope is 400 `invalid_request_error`, code `context_window_exceeded`, safe message; no arbitrary `err.Error()` JSON parsing. |
| Cross-protocol | Same typed error returned through Claude and OpenAI endpoints | Protocol envelope differs, internal taxonomy and state effects are identical. |
| Auth conductor request scope | Context overflow with a non-empty physical model | Failure count/telemetry may change; auth/model availability, cooldown, quota, registry suspension, and last health error do not. |
| Auth conductor model scope | Rate limit for one physical model | Only that account/model state cools down; other models on the credential remain eligible. |
| Auth conductor account scope | Authentication/billing failure with a physical model present | Whole credential cools down; scope is not narrowed to the model. |
| Auth conductor unknown scope | 400 `model_execution_failed` without typed detail | Terminal and availability-neutral; no retry, fallback, or cooldown. |
| No text policy | Message contains `invalid_request_error` but has no typed/closed code | Message text does not determine request scope. |
| Localization | Russian and English messages with the same detail | Identical scope decision and state mutation. |
| Persistence | Restart after model/account failure | Sanitized provider detail and scope survive; no raw payload survives. |
| Persistence | Restart after context overflow | No cooldown or unhealthy state appears after reload. |
| Compatibility | Decode pre-taxonomy plugin/persisted payload | Accepted as legacy; no panic; unknown is terminal and neutral unless closed legacy code mapping applies. |
| Security fuzz | Fuzz JSON envelopes/messages, Unicode, very large numbers, embedded secrets | No panic, overflow, secret propagation, or accidental reviewed classification. |
| Race | Concurrent failures of request/model/account scope | Race-clean state; request errors cannot overwrite account/model health. |

Suggested test locations:

- `sdk/cliproxy/providererror/provider_error_test.go`;
- `internal/runtime/executor/claude_stream_provider_error_test.go`;
- a new Claude HTTP provider-error parity test beside
  `claude_stream_provider_error_test.go`;
- `sdk/api/handlers/model_execution_test.go`;
- `internal/pluginhost/host_callback_provider_error_safety_test.go`;
- `internal/pluginhost/host_callbacks_test.go`;
- `sdk/pluginapi/types_test.go`;
- `sdk/api/handlers/claude/code_handlers_error_test.go`;
- `sdk/cliproxy/auth/provider_error_persistence_test.go`;
- `sdk/cliproxy/auth/conductor_overrides_test.go`;
- Bravo cross-protocol and context-fallback tests after the core contract is
  implemented.

## 10. Acceptance gate

The core error work is complete only when:

- all matrix rows pass under `go test` and the relevant race tests pass;
- one captured provider fixture produces byte-equivalent safe taxonomy across
  HTTP, SSE, direct API, nested non-streaming, and nested streaming paths;
- context overflow does not alter subscription health in a canary state
  snapshot;
- no route or auth decision reads human error text;
- canary logs show the stable code, class, scope, required tokens, and limit,
  without the prompt or raw provider body;
- the production rollout still follows the project sequence: test plan, code,
  canary, test review, GitHub, one Docker deployment, then Docker build-cache
  cleanup.
