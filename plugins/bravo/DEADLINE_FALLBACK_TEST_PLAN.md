# Bravo 0.7.8 Pre-Commit Hedge and Cancellation Test Plan

## Incident

Claude Code auto mode can cancel its safety-classifier request after roughly
60 seconds without sending that deadline to the server. Production traces show
the first Claude attempt consuming almost the whole window and the Codex
fallback starting with less than two seconds left. The shared request context
is then canceled, but Core currently reports that cancellation as a retryable
500 and Bravo may apply a provider cooldown.

The inbound `X-Stainless-Timeout` value is 600 seconds in the affected traces,
so it cannot be used as the classifier deadline.

## Safety constraints

- Do not add a timeout to an established upstream connection.
- Do not splice streams or switch providers after any client-visible payload.
- Do not treat client cancellation or a superseded hedge as provider failure.
- Do not let a losing attempt increment Core success/failure counters, clear
  model quota, mark auth/model unavailable, or create a cooldown.
- If a superseded request may already have reached the provider, remove its
  in-flight lease but retain one conservative pending-spend reservation until
  the next provider-confirmed quota refresh. This protects tariff floors
  without presenting the loser as an outage.
- Preserve the configured provider-call budget and project subscription pool.
- Hedge only between already contract-compatible candidates and prefer a
  different provider, so another slow credential on the same provider cannot
  consume the fallback window.
- Keep production unchanged until the exact candidate passes every local,
  isolated-canary, and real Claude Code gate below.

## Design under test

The affected Claude Code traces are all streaming requests. Bravo starts with
its normal highest-priority streaming candidate. If that attempt has not
returned its streaming bootstrap after the configured hedge delay, Bravo may
start one eligible cross-provider candidate concurrently. In the current Core
callback, bootstrap return means the first upstream payload has already been
buffered; it does not merely mean that response headers arrived.

The first successful pre-commit attempt wins. The host cancels the losing
nested attempt through an opaque child callback scope owned by the same plugin.
Each child inherits request values/deadline/plugin ownership while isolating
cancellation and Core result accounting. `host.callback.commit` applies only
the selected child result; `host.callback.close` cancels and discards an
uncommitted loser. This is winner-driven cancellation, not a network timeout.
When no safe cross-provider fallback exists, `max_attempts` disallows another
real call, or hedging is disabled, execution remains sequential.

Core normalizes cancellation of the root client request as
`request_canceled`, HTTP 499, non-retryable and request-scoped. Bravo treats
that result as terminal and never creates a cooldown. Winner-driven
cancellation is recorded as `bravo_attempt_superseded`, HTTP 499,
non-retryable and without cooldown.

The race is limited to streaming bootstrap and the short pre-commit interval
before the first payload is emitted downstream. The alternate remains
cancelable until that first successful emit; after it, the loser is canceled,
the winner's deferred Core result is committed, and the existing
committed-stream gate remains authoritative. A successful zero-payload EOF and
a real provider failure are also committed; request cancellation, local bridge
failure, and a superseded result are discarded. Non-streaming execution and
token counting retain their existing sequential fallback semantics in 0.7.8:
racing complete generations can double cost, and racing provider tokenizers
can change count semantics.

## Red phase

Before implementation, add focused tests that demonstrate:

1. request cancellation is exposed as a retryable 500 instead of a stable 499;
2. a slow first Claude streaming bootstrap prevents a fast Codex fallback;
3. the host has no callback-owned way to cancel one nested attempt;
4. a canceled nested result can affect provider availability/cooldown;
5. initial stream cancellation can be mistaken for an empty successful stream.

Record the failing test names and expected failure before changing production
code.

The final independent audit added a second red phase before canary:

- `TestHostStreamEmitWithCanceledCallbackReturnsRequestCanceled` returned the
  plain local error `stream 1 is not open` instead of typed
  `request_canceled`/499;
- the first-emit Bravo regressions did not compile because downstream emit had
  no callback identity, so cancellation between bootstrap and first emit
  could be classified as a retryable 502 and cool a healthy provider;
- recent status had no neutral superseded bucket, so every successful hedge
  inflated the red failure count.

## Core and SDK regression coverage

- `host.callback.fork` creates a child scope that inherits values, deadline,
  and plugin ownership while deferring Core result accounting.
- `host.callback.commit` atomically selects the child's pending Core result;
  a fast one-payload loser cannot record success before Bravo chooses it.
- `host.callback.close` cancels execute, count, and stream-bootstrap work and
  discards any uncommitted Core result owned by that child scope.
- Closing a parent callback cascades to every child attempt and stream.
- Parent close and child commit are linearized, so a late child cannot commit
  during the cascade.
- A plugin cannot fork, commit, or close a callback owned by another plugin.
- A closed or unknown non-empty callback ID fails closed instead of silently
  falling back to `context.Background()`.
- Closing a child twice is safe and leaves no registry or bridge entries.
- Root request cancellation maps to
  `request_canceled`/499/non-retryable in execute, count, and initial stream
  receive paths.
- Downstream stream emit carries callback ownership and maps cancellation
  between upstream bootstrap and the first emit to the same typed 499.
- A request-scoped 499 never reaches auth availability/cooldown accounting.
- A committed child keeps successful accounting, but a later cancellation
  result cannot overwrite it.
- An upstream/provider 499 that is not caused by the root request is not
  silently reclassified as a client cancellation.

## Bravo regression coverage

- Stream: slow Claude bootstrap is hedged by Codex before any payload; Codex
  wins; Claude is canceled; only Codex bytes reach the client.
- Execute and count remain sequential and do not start a latency hedge.
- Once the first payload is committed, no hedge or fallback starts.
- A second Claude credential is not selected as the hedge target when an
  eligible Codex candidate exists.
- `max_attempts: 1` prevents the hedge.
- `fallback_hedge_delay_seconds: 0` disables the hedge.
- A primary success before the delay never starts Codex.
- A terminal contract/request error never starts a hedge.
- Root 499 and superseded 499 do not create Bravo cooldown entries.
- A local stream-bridge failure is terminal, never retries another provider,
  never commits deferred Core accounting, and never creates a cooldown.
- Every real provider call consumes exactly one provider-call budget slot.
- Attempt analytics distinguishes success, provider failure, client
  cancellation, and a superseded hedge without counting the loser as a
  provider outage.
- Supersede and coordinator-panic paths release in-flight leases exactly once;
  possible upstream spend remains pending until confirmed quota refresh.

## Required local gates

Run sequentially and require at least 10 GiB free before heavyweight work:

1. focused Core cancellation and host callback tests;
2. focused Bravo hedge, fallback, quota, and committed-stream tests;
3. root `go test ./... -count=1`;
4. root compile verification;
5. Bravo `go test ./... -count=1`;
6. Bravo `go test -race ./... -count=1`;
7. Bravo `go vet ./...`;
8. Bravo `go build ./...`;
9. clean diff check with no credentials, runtime state, logs, or generated
   binaries.

## Isolated canary

Build the exact candidate source into a disposable Linux/arm64 image on a free
loopback port. Use only fake Claude and Codex upstreams:

- fake Claude accepts the request and withholds its first response;
- fake Codex returns a valid response/stream immediately;
- the test hedge delay is short and explicit.

Verify the streaming bootstrap race end to end. Execute/count cancellation
normalization remains covered by the focused and full unit gates because this
incident and the hedge are streaming-only. Assert the exact streaming upstream
sequence, one canceled loser, no Core or Bravo cooldown/counter mutation for
that loser, no duplicate client payload, winner-only project analytics, and no
canary restart.

## Real Claude Code gate

Use a disposable canary project and key with the same Claude-first route shape
as the incident. Run a real Claude Code auto-mode `Edit` decision under the
same permission policy. The reusable gate is
`scripts/bravo-claude-auto-edit-smoke.rb`; it intentionally uses
`--permission-mode auto --tools Read,Edit` and does not pre-authorize Edit.
Run it once with `--model opus` for the normal user route and once with
`--model sonnet` to exercise the exact logical route named by the original
classifier error. If a Claude Code build emits a separate safety-classifier
request, require it in the server trace. If it does not, record that fact and
use the direct Sonnet auto/Edit run as the observable route proof instead of
claiming an unobserved internal call.
Pass criteria:

- the action no longer fails with “auto mode cannot determine the safety of
  Edit” when the Claude attempt stalls;
- every observed Opus/Sonnet request reaches Codex before client cancellation;
- the client observes one coherent result and no provider-switch artifact;
- the canceled Claude attempt creates neither Core nor Bravo cooldown;
- no production key, project, state, port, or container is modified.

The pre-publish real-client gate passed for both models with Claude Code
2.1.220. The two tool loops produced six Codex winners and six neutral Claude
supersedes, zero Claude Core success/failure accounting, zero cooldowns, and
Codex-only project analytics. This Claude Code build emitted only Opus calls
during the Opus run, so the Sonnet route was verified by the separate direct
Sonnet auto/Edit run rather than by claiming an unobserved hidden request.

## Publish, production, and cleanup gates

Only after every gate is green:

1. commit and push the exact tested source;
2. open and merge a focused PR into `bravo/stable`;
3. build the production image from the merged commit;
4. retain the current image and configuration/state backup;
5. perform one health-gated production container replacement;
6. verify health, plugin version, project count, read-only model discovery,
   and one protocol smoke;
7. remove only the disposable canary, mocks, task-owned build context, and
   task-owned cache; never run a broad Docker prune.
