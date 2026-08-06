# Bravo context-window routing contract

Status: proposed for the post-0.7.11 integrated reliability build.

This contract removes the hard-coded `128000` context claim from Bravo and
defines when a context-window failure may cross a model or provider boundary.
It is intentionally fail-closed: a larger advertised window is not by itself
proof that an already oversized request fits a different tokenizer.

## Goals

- Export the registry's reviewed model limits to Bravo without exposing auth
  records or credential counts.
- Advertise one internally consistent capacity tuple for each logical Bravo
  model instead of unrelated per-field maxima.
- Skip a candidate before execution when the request is proven not to fit.
- Continue after a context failure only when the next candidate is proven to
  accept the request and no user-visible stream content has been committed.
- Keep old hosts and old plugins functional during a rolling upgrade.

This contract does not authorize automatic compaction. Compaction changes the
request and conversation semantics and remains an explicit client operation or
a separately approved policy.

## Terminology

- **physical candidate**: one configured `provider` + provider-native `model`
  pair in a Bravo route.
- **logical model**: the client-facing Bravo model, for example `bravo/opus`.
- **capacity tuple**: the related values `InputTokenLimit`, `ContextLength` and
  `MaxCompletionTokens` from one physical candidate.
- **required input**: tokens consumed by the translated prompt before generated
  completion tokens.
- **completion reservation**: the effective maximum output requested for this
  attempt. If the request does not specify it, the candidate's reviewed maximum
  is used conservatively.
- **committed content**: any semantic response event already emitted to the
  client that cannot safely be retracted or replayed from another provider.
- **proven compatible**: compatibility established using reviewed limits and a
  token count valid for the target candidate. Estimates without a conservative
  upper-bound guarantee are not proof.

## Host catalog contract

`pluginapi.HostModelListEntry` receives the following additive JSON fields:

```go
// InputTokenLimit is the maximum accepted prompt/input token count. Zero means
// unknown, not unlimited.
InputTokenLimit int64 `json:"input_token_limit,omitempty"`

// ContextLength is the maximum combined input plus generated context. Zero
// means unknown, not unlimited.
ContextLength int64 `json:"context_length,omitempty"`

// MaxCompletionTokens is the maximum generated completion token count. Zero
// means unknown, not unlimited.
MaxCompletionTokens int64 `json:"max_completion_tokens,omitempty"`
```

The values come directly from `internal/registry.ModelInfo`. The host must not
invent defaults in the callback. Negative values are invalid and are exported
as zero.

The callback keeps its current privacy boundary: limits are model metadata and
do not reveal credential IDs, credential counts, quota or project membership.

### Duplicate registry entries

Entries are merged by normalized exact `provider + model ID`.

- A reviewed static-catalog value takes precedence over a live-registry value.
- A non-zero value may fill a zero value.
- If two reviewed non-zero values conflict, the host exports the lower value
  and records a compatibility diagnostic. It must not silently export the
  larger claim.
- `Catalog` and `Available` retain their existing independent meanings.
- Limits describe model capability; `Available` describes current connected
  availability. A temporarily unavailable model does not lose its limits.

Alias resolution occurs before the lookup. Bravo must compare the resolved
provider-native model ID, not a client-facing alias.

## Registration snapshot

`model.register` currently has no authenticated host-callback context, so a
plugin cannot call `host.model.list` while registering its logical models. The
host therefore supplies the same redacted catalog snapshot additively:

```go
type ModelRegistrationRequest struct {
    Plugin     Metadata
    HostModels []HostModelListEntry
}
```

The snapshot is produced before Bravo's own models are committed and excludes
models owned by the registering plugin. This prevents a recursive Bravo model
from becoming its own candidate.

At execution time Bravo obtains a fresh request-scoped `host.model.list`
snapshot using `HostCallbackID`. Registration metadata is descriptive; the
execution snapshot is authoritative for routing.

## Logical Bravo model metadata

Hard-coded limits are forbidden. In particular, text routes must no longer
unconditionally advertise `128000/32000`.

For every configured logical model Bravo joins its physical candidates to the
registration snapshot. Candidates with no exact catalog match have unknown
limits and create a compatibility warning, but do not borrow values from a
similar model name.

### Advertisement anchor

The logical model advertises one capacity tuple copied from one physical
candidate, called the **advertisement anchor**. It never computes independent
maxima because those maxima may describe an impossible combination, for
example a 1M context candidate with 8K output and a 128K context candidate with
64K output.

Eligible anchors are configured text candidates with at least one non-zero
input/context limit. Selection is deterministic:

1. highest proven effective input capacity with the candidate's maximum
   completion reservation;
2. highest `ContextLength`;
3. highest `MaxCompletionTokens`;
4. highest Bravo route priority;
5. normalized provider and model ID as the final stable tie-breaker.

The chosen candidate's full tuple is copied to the logical `ModelInfo`:

- `InputTokenLimit` is copied as-is;
- `ContextLength` is copied as-is;
- `MaxCompletionTokens` is copied as-is;
- `OutputTokenLimit` mirrors `MaxCompletionTokens` for clients that still read
  the older field.

Zero fields remain zero. If no candidate has reviewed capacity metadata, all
four logical limit fields are zero. Bravo must never restore the current
hard-coded values as a fallback.

The model description states that these are the maximum reviewed limits of one
route candidate and that current availability can reduce runtime capacity.
They are not a promise that every physical candidate has the same window.

Image-only logical models retain their existing image metadata and are outside
this text-context contract.

## Candidate capacity calculation

For candidate `c`, required input `i` and completion reservation `o`, the
candidate fits only if every known applicable constraint is satisfied:

```text
InputTokenLimit(c) == 0       or i <= InputTokenLimit(c)
MaxCompletionTokens(c) == 0  or o <= MaxCompletionTokens(c)
ContextLength(c) == 0         or i + o <= ContextLength(c)
```

At least one reviewed input bound must be known:

- a non-zero `InputTokenLimit`; or
- a non-zero `ContextLength` together with a known completion reservation.

Otherwise the result is `unknown`, not `fits`.

The completion reservation is the post-normalization `max_tokens` /
`max_output_tokens` actually sent for that candidate. If it is absent, Bravo
uses `MaxCompletionTokens`. If both are unknown, `ContextLength` alone cannot
prove compatibility.

Arithmetic is checked for overflow. Invalid or negative counts make capacity
unknown and emit an internal compatibility diagnostic; they never make a
candidate eligible.

## Token-count evidence

The routing decision carries a structured context requirement:

```text
required_input_tokens   optional positive integer
requested_output_tokens optional positive integer
count_kind              exact | upper_bound | lower_bound | unknown
count_scope             target_model | tokenizer_family | provider | unknown
count_provider          optional normalized provider
count_model             optional provider-native model
```

`providererror.Detail.RequiredTokens` and `LimitTokens` from
`CORE_ERROR_CONTRACT.md` seed this structure as an exact count scoped to the
physical model that produced the error. They do not need duplicate public JSON
fields. `LimitTokens` is retained as observed source-model evidence; target
eligibility is evaluated against the target's `HostModelListEntry` limits.

Only `exact` and conservative `upper_bound` counts can prove that a target
candidate fits. A provider's context rejection establishes only a lower bound
unless it reports the actual required count.

Provider-reported token counts are not automatically portable across
providers. They may be reused only when either:

- the count was produced for the exact target model; or
- the core has a reviewed tokenizer-family rule declaring the source and target
  counts comparable.

The default tokenizer-family rule is deny. Same provider alone is not a proof.
Cross-provider Claude-to-Codex fallback therefore requires a Codex-specific
exact count or conservative upper bound; the Claude count from
`prompt is too long: N tokens > M maximum` is not sufficient by itself.

Bravo obtains that target-specific proof through the existing
`host.model.count_tokens` callback after a context failure and before launching
the fallback generation attempt. The proof step is bounded:

- at most one count call per unique normalized target `provider + model` in a
  route;
- it uses the already translated target candidate body, pinned project-allowed
  auth and `SingleAttempt=true`;
- it is allowed only before the streaming commit barrier;
- it does not consume the generation `max_attempts` budget and is recorded as a
  preflight proof, not a provider generation attempt;
- only a successful response with a reviewed exact `input_tokens` value is
  accepted;
- unavailable counters, HTTP errors, malformed/zero/negative values and
approximate local estimates leave the target unproven and therefore skipped.

The first implementation uses this target count for every different physical
model, including two Claude models. Tokenizer-family reuse remains a future
optimization and cannot be enabled without its own reviewed compatibility
table and fixtures.

Approximate character ratios, byte division and historical average token
ratios may be shown as operator diagnostics but must not authorize fallback.

## Routing behavior

### Before the first provider attempt

If the core supplies a target-valid exact or upper-bound count, Bravo evaluates
each candidate before reserving a subscription:

- `fits`: candidate remains eligible;
- `does_not_fit`: candidate is skipped with
  `context_preflight_rejected`; no auth cooldown or failure is recorded;
- `unknown`: candidate remains eligible for its initial attempt, but is not a
  proven context fallback target.

Unknown metadata must not block ordinary requests that have not yet produced a
context error. This preserves compatibility with live-only and older-provider
models.

### After a context-window failure

The failure contract supplies safe structured numeric evidence where available.
Bravo merges stronger evidence into the request's context requirement and does
not mark the subscription unhealthy: context overflow is request-scoped.

Bravo may continue only when all of the following are true:

1. no user-visible content is committed;
2. the failure is classified as context-window overflow;
3. the next physical model is proven compatible under this contract;
4. the candidate satisfies the project's provider/model/subscription policy;
5. the candidate satisfies the request's tools, modalities, schema and effort
   contracts;
6. normal route attempt limits are not exhausted.

A candidate with a merely larger context window is not enough. A candidate
whose limits are unknown is not enough. Another credential for the same
physical model is skipped because a request-scoped context failure cannot be
fixed by changing credentials.

When no candidate is proven compatible, routing stops with
`bravo_context_window_exceeded`. The safe Russian client message includes
known required and supported counts and says whether the next action is
`/compact` or a new session. It must not claim that provider quota is exhausted.

### Missing exact token count

When the provider reports only a generic context overflow:

- the failed physical model and all equivalent credentials are skipped;
- no different physical model is attempted solely because its advertised
  context is larger;
- an already available target-specific conservative upper bound may still
  prove compatibility;
- Bravo may make the single bounded `host.model.count_tokens` proof call for a
  target candidate;
- otherwise Bravo stops fail-closed with an actionable context error.

This rule prevents repeated paid attempts and prevents a cross-provider route
from turning one clear context error into an opaque chain of failures.

## Streaming commit barrier

Before commitment Bravo may buffer and discard transport prelude while it
chooses another proven-compatible candidate. Prelude includes:

- SSE comments and keep-alives;
- `message_start` metadata;
- provider ping events;
- empty content-block starts that have no client-visible semantic payload;
- usage metadata that has not been emitted to the client.

The stream becomes committed immediately before Bravo emits the first semantic
event that can alter client state, including:

- non-empty text or reasoning content;
- any tool-call/tool-use event or argument delta;
- image, audio or other binary content;
- a refusal, citation or structured-output delta;
- provider metadata already emitted under a protocol contract that cannot be
  reproduced by the fallback candidate.

After commitment Bravo must not fallback, replay the request or splice another
provider into the stream. It closes the stream with the safe terminal error and
records `content_committed=true` plus `fallback_blocked=content_committed`.

A context error event received before commitment is consumed internally and is
not emitted before the fallback decision. A client cancellation never triggers
fallback.

## ABI and rolling compatibility

The native C ABI remains `ABIVersion = 1`, and the RPC JSON schema remains
`SchemaVersion = 1`. All changes in this contract are optional additive JSON
fields; no existing method or field changes meaning.

Compatibility matrix:

| Host | Bravo plugin | Behavior |
|---|---|---|
| old | old | Existing behavior; no new guarantees. |
| new | old | Old plugin ignores additive fields; existing routes continue. |
| old | new | Missing fields decode as zero. Logical limits are unadvertised, and context fallback remains fail-closed. Ordinary routing continues. |
| new | new | Full registration metadata, preflight and proven-compatible fallback. |

New Bravo must treat zero as unknown and must not require the new fields during
plugin initialization. New host must continue accepting registration responses
from plugins that know nothing about `HostModels`.

Because Go plugins communicate through the existing JSON RPC envelope across
the native ABI, adding struct fields does not change the native function table.
A future change that makes fields mandatory, changes zero semantics or changes
method shape requires a schema-version review.

## Migration strategy

1. **Core metadata, dark:** add fields to `HostModelListEntry`, populate them in
   `host.model.list`, add the registration snapshot and tests. Do not change
   Bravo routing yet.
2. **Bravo metadata:** consume the registration snapshot, replace hard-coded
   logical limits with the advertisement-anchor tuple and expose compatibility
   warnings for missing/conflicting limits.
3. **Decision shadow mode:** calculate context preflight/fallback decisions and
   persist them in safe route traces, but retain the current fail-closed runtime
   behavior. Compare decisions in canary.
4. **Canary enforcement:** enable preflight skips and context fallback only for
   synthetic canary projects. Verify the stream commit barrier and token-count
   provenance.
5. **Production enforcement:** enable for all Bravo projects only after the
   integrated canary and repository gates pass. Keep the previous image and
   state snapshot for rollback.

No state-file migration is required. Existing YAML routes remain valid. Missing
metadata degrades to unknown and fail-closed behavior rather than startup
failure.

## Test matrix

### Host catalog and ABI

| Case | Expected result |
|---|---|
| Static Claude model has all three limits | Exact registry values appear in `HostModelListEntry`. |
| Live entry has zero, catalog has values | Catalog values survive merge. |
| Catalog has zero, live entry has values | Non-zero live values fill the unknown fields. |
| Two reviewed entries conflict | Lower value exported and compatibility diagnostic emitted. |
| Negative registry value | Exported as zero/unknown. |
| Alias points to provider-native model | Limits come from the resolved exact model. |
| Similar model name only | No fuzzy borrowing of limits. |
| New host with old plugin fixture | Unknown JSON fields are ignored. |
| Old host response with new Bravo | Zero-value limits; startup and ordinary routing succeed. |
| Registration snapshot | Excludes the registering plugin's own models. |

### Logical model registration

| Candidates | Expected logical metadata |
|---|---|
| Claude 1M/128K plus Codex 256K/32K | One complete tuple from the Claude anchor, never mixed maxima. |
| Candidate A 1M/8K, candidate B 128K/64K | Tuple comes wholly from A under the deterministic anchor rule. |
| All candidates unknown | All logical limit fields are zero, not 128K/32K. |
| Image-only route | Existing image metadata unchanged. |
| Same config and snapshot in random order | Byte-equivalent deterministic registration response. |

### Capacity calculation

| Evidence and limits | Expected result |
|---|---|
| Exact input below both input and combined limits | `fits`. |
| Exact input equals limit | `fits`. |
| Exact input exceeds input limit by one | `does_not_fit`. |
| Input fits but input + reserved output exceeds context | `does_not_fit`. |
| Requested output exceeds completion maximum | `does_not_fit`. |
| Only context known and completion reservation known | Deterministic fit/reject from combined limit. |
| Only context known and completion reservation unknown | `unknown`. |
| All limits zero | `unknown`. |
| Integer addition overflow | `unknown`, never `fits`. |

### Observed overflow and fallback

| Scenario | Expected route behavior |
|---|---|
| Claude reports exact required input; larger Claude target count proves fit | Failed model credentials skipped; compatible model attempted. |
| Claude reports exact count; Codex target has a smaller window | Codex skipped; actionable compact/new-session error. |
| Claude exact count, Codex larger, tokenizer comparability unknown | No cross-provider fallback. |
| Target-specific exact Codex count from bounded host callback fits | Codex fallback allowed before commitment. |
| Target counter unavailable, fails or returns malformed/approximate count | Codex remains unproven; fail closed. |
| Two credentials for the same target physical model | At most one target count call. |
| Generic overflow with no count | No model fallback based only on a larger advertised window. |
| Overflow on one credential, same physical model on another credential | Second credential skipped without cooldown. |
| Context failure | No account/provider cooldown and no quota decrement classification. |
| Unknown metadata before any failure | Initial ordinary attempt remains allowed. |

### Streaming

| Stream state | Expected behavior |
|---|---|
| Error before any payload | Proven-compatible fallback allowed. |
| Error after buffered `message_start` and ping | Prelude discarded; fallback allowed. |
| Error after empty content block only | Fallback allowed if no semantic event was emitted. |
| Error after first non-empty text delta | No fallback. |
| Error after first tool-use event | No fallback. |
| Error after reasoning, image or structured-output delta | No fallback. |
| Client cancellation before content | No fallback; request canceled. |
| Fallback succeeds | Client receives only the winning provider's coherent stream. |

### Integration and canary

- Register `bravo/opus` from a host snapshot containing Claude 1M and Codex
  models; verify that `/models` no longer reports the hard-coded 128K tuple.
- Replay a sanitized `prompt is too long: 1003466 tokens > 1000000 maximum`
  fixture; verify no credential cooldown and no Codex attempt without
  target-valid evidence.
- Supply a synthetic target-valid upper bound and a verified 2M candidate;
  verify one bounded target count, one pre-content fallback and a successful
  coherent response.
- Repeat the failure after the first visible text and verify exactly one
  physical attempt.
- Run the same canary through Claude and OpenAI entry protocols; routing and
  context decisions must be identical after normalization.
- Verify safe trace fields include required/supported counts and count scope,
  but no prompt, provider prose, auth ID or provider request ID.
- Restart the canary with the previous config; no state migration and no route
  loss are allowed.

## Release acceptance criteria

- No Bravo text model advertises a fabricated fixed context limit.
- Every context fallback target is accompanied by machine-verifiable evidence
  that the request fits its reviewed limits and tokenizer scope.
- Context overflow never cools or disables a credential.
- No fallback occurs after committed semantic content.
- Old host/new plugin and new host/old plugin tests pass.
- The integrated test plan, canary, GitHub and production gates in
  `BIG_BUILD_TEST_PLAN.md` all pass before deployment.
