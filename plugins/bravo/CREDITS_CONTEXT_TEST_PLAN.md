# Bravo 0.7.9 Model Credits and Context Failover Test Plan

## Incidents

Palantir returned Anthropic's structured `rate_limit_error` with
`details.error_code=credits_required`, `details.model=claude-fable-5`, and the
notice `You've hit your monthly spend limit`. The HTTP status may still be 429,
but this is not an anonymous transient rate spike. It is a provider-confirmed
credits/spend restriction for the named model.

Bravo retried another provider, but the accumulated Claude Code conversation
fit Fable 5's 1M context and did not fit the 372k Codex fallback. The final
client error only described the secondary context overflow, hiding the
provider-confirmed reason that caused the fallback.

## Required semantics

- Parse reviewed structured provider errors without depending on English
  substring matching.
- Normalize Anthropic `credits_required` as a model-scoped credits/spend
  exhaustion when the response names a model.
- Keep the affected credential eligible for sibling models unless the provider
  explicitly reports a credential/workspace-wide restriction.
- Continue through other eligible credentials and providers for the same
  logical Bravo request.
- Store and expose a safe operator summary: provider error code, physical model,
  display name, notice title/text, and disabled reason. Do not expose tokens,
  credentials, paths, raw provider JSON, or request IDs.
- Show the affected subscription/model as, for example,
  `Fable 5 — monthly spend limit reached`, while making clear that sibling
  models may still be available.
- Preserve every ordered attempt in analytics. If all fallbacks fail, the
  terminal response must retain both the primary credits failure and the
  secondary fallback failure instead of silently replacing one with the other.
- Treat context overflow as request-scoped. It must never cool a credential,
  model, or provider.
- Do not retry another credential for the same physical model after a context
  overflow. This release fails closed because candidate configuration does not
  yet carry a verified context-window size. A later release may try a strictly
  larger compatible candidate only after that metadata contract exists.
- Never splice providers after a client-visible stream payload.
- Base pre-content stream safety on the actual upstream provider/event
  contract, not only on the client's requested protocol. An OpenAI
  chat-completions or Responses client routed to a Claude subscription must
  still buffer Claude's non-substantive `message_start`/`ping` prelude and
  intercept a following structured provider error before any client-visible
  commit.

## Red phase

Before implementation, add focused tests proving the current defects:

1. `credits_required` remains the generic `model_execution_failed` or generic
   HTTP 429 code.
2. The structured notice and physical model are absent from attempt analytics.
3. Subscription management cannot expose the affected model and safe reason.
4. A model-scoped credits failure can poison a credential-wide presentation or
   cooldown.
5. When the fallback returns context overflow, the final result loses the
   preceding credits failure.
6. A context overflow can be retried blindly on another credential/model even
   though the route has no verified context-window metadata.
7. An OpenAI chat-completions or Responses client routed to Claude can commit
   after the translated `message_start`, before a following
   `credits_required` event is classified, preventing Bravo fallback.
8. A non-streaming Anthropic HTTP 429 can retain the raw provider body in
   Core's auth/model `LastError`, persisted cooldown state, or the host callback
   envelope before Bravo gets a chance to sanitize it.

Record the failing test names before changing implementation.

### Recorded red evidence

The isolated Go 1.26 runs failed before production code changed:

- `TestStatusErrExtractsStructuredProviderErrorCode`: `statusErr` had no
  `ErrorCode`, so `credits_required` was lost at the executor boundary.
- `TestHostAuthModelStatesCarriesSafeErrorInputs`: host model state contained
  the complete upstream JSON and request ID but no structured error code.
- `TestCreditsRequiredPayloadClassifiesAsModelCreditsExhausted`:
  host/HTTP paths remained `model_execution_failed` or
  `bravo_candidate_http_error`.
- `TestCreditsRequiredAttemptExposesOnlySafeProviderDetails`: every normalized
  field was absent and `attempt.error` leaked the raw payload/request ID.
- `TestSubscriptionViewExposesModelCreditsIssue`: `model_issues` was absent.

`TestCreditsRequiredCooldownIsModelScoped` already passed. The existing
provider + auth + physical-model cooldown primitive correctly left sibling
Claude models eligible, so this release must preserve it rather than replace
it with an account-wide cooldown.

An independent pre-canary review added a second RED phase. The new regressions
failed before the follow-up fixes:

- `TestHostRollupDoesNotDisableModelsWithHealthyOwnState`: a Core aggregate
  deadline from Fable blocked a previously unseen sibling model, unlike Core's
  native selector.
- `TestSubscriptionViewKeepsSiblingModelsAndAccountReady`: the account card
  showed credential-wide cooldown for one model issue.
- `TestCreditsRequiredScopeOverridesForbiddenHTTPEnvelope`: an HTTP 403 wrapper
  overrode the provider's explicit `scope=model`.
- `TestUnknownStructuredProviderFailureIsRedactedEverywhere`: an unreviewed
  JSON body leaked `request_id`, payment data, and provider text to events and
  the client.
- `TestHostAuthModelStatesCarriesSafeErrorInputs`: the full auth entry still
  exposed the raw top-level status message after its model states were
  redacted.
- `TestParseRedactsSensitiveOrOversizedAllowedFields`: reviewed fields accepted
  embedded credential/request markers and unbounded text.
- `TestSubscriptionViewDropsExpiredModelCreditsIssue`: an expired issue stayed
  visible as active.

A final cross-protocol review added a third RED phase before publication:

- `TestClaudeExecutorStreamProviderErrorsAreTerminalAcrossProtocols`: all nine
  Claude/OpenAI/Responses combinations failed. Native Claude leaked the raw
  error SSE, Chat Completions emitted a lossy HTTP-200 error chunk, and
  Responses silently dropped the error; none produced a typed terminal error.
- `TestBravoStreamPreContentCreditsFallbackAcrossClientProtocols`: OpenAI Chat
  and Responses committed their translated prelude and never reached Codex.
- `TestBravoStreamPreContentUnknownStructuredErrorFailsClosedAcrossClientProtocols`:
  native Claude retried an unreviewed error, while the OpenAI protocols exposed
  translated prelude before failing.
- `TestBravoStreamPreContentContextOverflowIsRequestScopedAcrossClientProtocols`:
  OpenAI Chat and Responses emitted prelude before the request-scoped 400.
- `TestClaudeExecutorIncompleteStreamIsTerminalAcrossProtocols`: all six
  before/after-content cases treated clean EOF without `message_stop` as a
  successful stream.

A final non-streaming persistence audit adds a fourth RED phase before the new
canary:

- the exact Palantir HTTP 429 must become a typed safe executor error whose
  `Error()` contains no raw JSON, request ID, CTA, or payment fields;
- Core auth/model state and `.cds` persistence must retain only the safe code,
  normalized summary, and the physical model already carried by the model-state
  key/cooldown record. Rich safe provider detail travels through the executor,
  host callback ABI, and Bravo; do not add a source-breaking field to the
  public `auth.Error` struct;
- the immediate host callback envelope must carry the same safe detail without
  ever serializing the original provider body.

A restart-scope audit adds a fifth RED phase:

- simulate a restarted Bravo process whose in-memory cooldown map is empty
  while Core restores a persisted model state under the real
  effort-qualified execution key `claude-fable-5(xhigh)`;
- a request for the base physical model `claude-fable-5` must still skip only
  that auth/model pair;
- Sonnet on the same Claude auth, Fable on another Claude auth, and the
  configured Codex fallback must remain eligible;
- canonicalization must compare both the requested candidate and stored
  `ModelStates` keys, because Core deliberately preserves the actual
  effort-qualified execution model in its cooldown record.

A config-credential restart audit adds a sixth RED phase:

- `setCooldownWithProviderError` must atomically persist the active Bravo
  barrier in the configured `state_path`, alongside usage and quota state;
- a fresh Bravo process must restore the exact provider + auth ID + base
  physical-model scope even when the credential comes from config/API-key
  auth and therefore has no Core `.cds` model state;
- an effort-qualified execution model such as `claude-fable-5(xhigh)` must be
  stored and restored as the base physical model `claude-fable-5`;
- expired barriers must be discarded during load and removed from the next
  snapshot;
- persistence may contain only the reviewed, sanitized provider detail. Raw
  provider JSON, request IDs, payment/CTA fields, tokens, and credential
  material must never reach `bravo-state.json`;
- the new state field must remain optional so existing schema-v2 snapshots and
  migrated schema-v1 snapshots continue to load unchanged.
- a same-path plugin reconfigure must merge the persisted barriers into the
  live runtime map. It must not erase a cooldown that a concurrent request
  installed before waiting for the state-store mutex;
- switching to a different `state_path` must still replace the old runtime
  barriers rather than merge state belonging to another deployment.

Recorded RED evidence:

- `TestPersistedEffortQualifiedModelStateBlocksOnlyItsPhysicalModelAfterRestart`
  returned `ready` for base `claude-fable-5` with an active restored
  `claude-fable-5(xhigh)` state and an empty Bravo runtime cooldown map.
- The regression is GREEN after deterministic aggregation of all stored effort
  keys sharing the same base physical model. Clean and expired variants cannot
  mask an active one; the same-auth Sonnet route, another Claude auth, and Codex
  remain eligible.
- `TestCooldownStatePersistsAndReloadsConfigAuthModelScope` failed because
  `bravo-state.json` contained usage/quota maps but no `cooldowns` field.
- `TestCooldownStateLoadPrunesExpiredAndResanitizesPersistedDetail` failed
  because a fresh plugin process restored no active model barrier from the
  configured state path.
- `TestCooldownSamePathReconfigureMergesConcurrentRuntimeBarrier` failed
  because same-path restore replaced the runtime map and erased the barrier
  installed by the deterministic concurrent-set interleaving.

The sixth phase is GREEN after synchronous atomic cooldown persistence,
sanitized/base-model reload, expiry pruning, same-path merge, different-path
replacement, and post-store-mutex runtime reassertion. The focused cooldown
suite passed under the race detector. The combined final nested Bravo gates
also passed: full test, full race, vet, and build.

A persistence concurrency review adds a seventh RED phase:

- stale expiry cleanup must compare the exact cooldown instance it observed
  before deleting either runtime or persisted state. A refreshed same-key
  barrier must survive cleanup started for its expired predecessor;
- a setter binds to the `state_path` generation active when it begins. If a
  different path replaces that generation while the setter waits for the
  state-store mutex, the old setter must neither write into the new snapshot
  nor reassert its runtime barrier;
- same-path reconfigure remains merge-safe, while an ordinary sequential path
  switch still replaces old barriers and accepts new-generation setters.

Recorded RED evidence:

- `TestStaleExpiryCleanupKeepsRefreshedSameKeyBarrier` could not compile
  because expiry cleanup had no compare-and-delete primitive for the exact
  observed cooldown instance;
- `TestOldStatePathSetterCannotCrossReconfigureGeneration` could not compile
  because the state store carried no path generation, so a setter had nothing
  to bind to before waiting for its mutex.

The seventh phase is GREEN after instance-conditional runtime/persisted
deletion and generation-bound persistence. Both deterministic interleavings
pass under the race detector. The complete nested Bravo test, race, vet, and
build gates remain green.

A model-credits probe review adds an eighth RED phase:

- exact `bravo_subscription_model_credits_exhausted` without a valid upstream
  `Retry-After` must stay unavailable for at least 15 minutes before Bravo
  probes that subscription/model again;
- generic 429 and other retryable failures without a hint continue to use the
  configured `cooldown_seconds`;
- any valid explicit `Retry-After` remains authoritative for model credits,
  including a value shorter than 15 minutes.

Recorded RED evidence:

- `TestCreditsRequiredCooldownProbeIntervalPolicy` could not compile because
  Bravo had neither a documented model-credits minimum nor a failure-aware
  cooldown-deadline policy; `applyFailureCooldown` fell directly back to the
  generic configured interval.

The eighth phase is GREEN with a 15-minute minimum only for the exact reviewed
model-credits code when no valid provider hint exists. Generic failures retain
the configured cooldown, and explicit shorter `Retry-After` values remain
authoritative. Focused normal/race and the complete nested normal test suite
pass.

A reverse-provider streaming audit adds a ninth RED phase:

- for Claude Messages, OpenAI Chat Completions, and OpenAI Responses clients,
  a Codex primary that emits only a translated non-substantive prelude and then
  returns a retryable `model_execution_failed`/5xx must remain uncommitted;
- Bravo must discard that failed prelude, continue to an eligible Claude
  candidate, and expose one coherent fallback stream under the requested
  logical model identity;
- once Codex has emitted substantive content, Bravo must never splice a Claude
  fallback into the same client stream;
- a retryable post-content provider failure still describes physical-model
  health. It must retain its model-scoped cooldown even though the committed
  stream itself cannot continue;
- context overflow remains request-scoped and terminal. Prelude buffering must
  not turn it into a blind cross-model retry or a cooldown.

Record the focused failures before changing the stream coordinator. The
expected pre-fix defect is that Codex prelude is committed immediately because
buffering is gated on `provider == claude`; the reverse Codex-to-Claude
fallback therefore never starts. The expected post-content defect is that the
coordinator overwrites `Retryable=false` merely to prohibit stream splicing,
which also suppresses the physical-model cooldown needed by later requests.

Recorded RED evidence:

- all three
  `TestBravoStreamCodexPreludeThenServerErrorFallsBackToClaude` protocol
  variants made only the Codex call instead of continuing to Claude;
- all three
  `TestBravoStreamCodexServerErrorAfterContentNeverSplicesButCoolsModel`
  variants preserved the no-splice boundary but failed to create the expected
  Codex Sol model-scoped cooldown;
- all three `TestBravoStreamCodexPreludeThenContextOverflowFailsClosed`
  variants leaked the rewritten Codex prelude before returning the terminal
  request-scoped context error.

A post-merge smart-key canary adds a tenth RED phase:

- creating, replacing, rotating, or deleting a project may trigger both the
  management-request reload and the filesystem-watcher reload;
- those reloads must never execute concurrently or let an older callback
  complete after a newer callback;
- every successful project-create or rotate response is a runtime contract:
  its returned plaintext key must authenticate immediately on every protocol;
- a deleted or superseded key must remain unavailable, and an older config
  callback must never resurrect it;
- the regression test must deterministically block the first config callback,
  start a second reload after writing a newer config, and prove that callbacks
  complete in persisted order without overlap. The final runtime callback and
  watcher snapshot must both contain the newer config.

Recorded canary evidence before the fix:

- the final merged 0.7.9 image passed five synthetic scenarios, then
  intermittently returned `401 Missing API key` for newly created project keys
  on Anthropic Messages, OpenAI Responses, and reverse Codex-to-Claude paths;
- the same canary and production container stayed healthy, and no production
  state was modified;
- successful creation and persistence followed by protocol-dependent 401s
  exclude key generation and request-header parsing. The remaining
  interleaving is concurrent config reload callbacks applying plugin runtime
  snapshots out of order.

Recorded RED tests:

- `TestReloadConfigIfChangedSerializesCallbacksInPersistedOrder` proved that a
  newer callback could enter while an older callback was still active;
- `TestReloadConfigIfChangedDoesNotAcknowledgeUnappliedWrite` applied port 8080,
  wrote port 9090 during the callback, then skipped 9090 because the old reload
  acknowledged the newer file hash;
- `TestLoadConfigDataDoesNotRewriteNewerLiveFile` proved that parsing a captured
  snapshot with an older plaintext management secret could rewrite the newer
  live YAML;
- transaction review found a second mutation could enter after plugin-local
  installation but before the first post-call reload. The deterministic
  `TestPluginConfigListMutationSerializesThroughPostCallReload` now covers
  overlapping append, append, and delete operations on actual `smart_keys`.

The focused normal tests and ten repeated race-detector runs are GREEN after:

- parsing and hashing one immutable captured YAML snapshot without live-file
  writes;
- serializing all manual and fsnotify config reloads;
- deferring the plugin config reload until the native management call exits;
- holding a dedicated plugin-mutation transaction lock through that reload;
- refusing an already-closed callback lifecycle instead of running a
  re-entrant cleanup inline.

Pre-PR canary evidence:

- candidate image
  `sha256:86a3406ab64600efb8da7c3691d65c424e3e9ef1462b9ae474d0e2a02b84978f`
  ran with the previously approved management UI bytes
  `74da7ec03778a29b284a9fe7729c611707f5d098cbe6cd828763b018dd40171e`;
- seven consecutive isolated smoke runs each passed all nine
  credits/context/stream/fallback scenarios (63/63 total);
- those runs performed 28 immediate post-create authentication probes and 28
  immediate post-delete rejection probes without reproducing the stale
  smart-key runtime;
- the final three runs also rotated a live project key, immediately rejected
  the superseded key, and authenticated the replacement key before continuing;
- the canary finished healthy with no remaining disposable smart keys or
  critical log markers. Production remained healthy on the unchanged 0.7.8
  image throughout the test.

## Core and host regression coverage

- The executor extracts nested `details.error_code=credits_required` from both
  streaming and non-streaming Anthropic errors.
- The non-streaming executor never places the raw Anthropic response body in
  `Error()`, Core `LastError`, model status, persisted cooldown state, or the
  host callback ABI.
- The original HTTP status remains available independently of the semantic
  provider code.
- Core records the failure under the physical model state, not as an
  account-wide disable.
- A persisted effort-qualified model state is matched to its base physical
  model after Bravo restarts; the automatic effort suffix must not create a
  route around an active Core cooldown.
- Host auth metadata carries only the model state's safe error inputs required
  by Bravo; secrets and raw auth metadata remain absent.
- Existing generic 429 handling and account-wide Anthropic extra-usage handling
  remain unchanged.
- Request-scoped context overflow never mutates auth/model availability,
  cooldown, success, or quota state.

## Bravo regression coverage

- `credits_required` is normalized to a stable model-credits error code in:
  - host execution failures;
  - HTTP response bodies;
  - stream errors before the first client-visible frame.
- The normalized attempt includes safe structured fields for Fable 5, the
  monthly spend notice, and `org_level_disabled_until`.
- Same-model retries move to another credential; a sibling Claude model on the
  original credential remains eligible.
- If Claude candidates cannot serve the request, Bravo reaches the configured
  Codex fallback.
- The affected physical model receives a model-scoped cooldown; the whole
  credential does not.
- Generic transient HTTP 429 remains retryable and model-scoped.
- Existing reviewed account-wide extra-usage errors still cool every Claude
  model on that credential.
- A context overflow does not create cooldown and terminates the current route
  without trying another unverified physical model.
- The client receives a non-retryable 400 with the physical model and
  context-window mismatch.
- When credits exhaustion is followed by context overflow, ordered diagnostics
  retain both failures and the terminal message explains both.
- Streaming fallback is allowed only before the first client-visible payload.
- For every supported client protocol, a Claude `message_start`/`ping`
  followed by `credits_required` remains pre-content: emit no provider bytes,
  preserve the safe model-scoped reason, and continue to the next eligible
  Bravo candidate.
- Exercise the preceding assertion through Claude Messages, OpenAI
  chat-completions, and OpenAI Responses entry points. The OpenAI-facing
  payload shape may differ after translation, but provider ordering,
  model-scoped cooldown, redaction, and fallback semantics must be identical.
- Validate OpenAI Responses streams by event semantics, never by counting a
  marker across the raw SSE body. Reconstruct client-visible text by
  concatenating the `delta` field of only `response.output_text.delta` events,
  require exactly one `response.created` and one `response.completed`, and
  require the completed Response aggregate's `output[].content[].text` to
  equal the reconstructed delta text. The same text appearing once
  incrementally and once in the final aggregate is the required Responses
  contract, not duplicated client output.
- Assert every synthetic Responses frame is emitted exactly once and that no
  failed-provider prelude, physical model, or provider diagnostic survives in
  the successful fallback stream.
- A Claude stream that emits actual content before an error must commit once
  for all three client protocols and must never splice a second provider.
- An unknown structured Claude error before content must fail closed and must
  not leak provider JSON through any client protocol.

## Management API and UI regression coverage

- `/v0/management/bravo/subscriptions` exposes a redacted list of active
  model-specific issues per subscription.
- `/v0/management/bravo/events` exposes the normalized provider code/model and
  safe notice fields for each attempt.
- Subscription cards show the operator-authored note in bold and place
  model-specific availability under progressive disclosure.
- Attempt analytics shows when each subscription was tried and why the attempt
  moved to the next route.
- Unknown provider payloads use a generic safe message instead of rendering raw
  JSON.
- The model picker and project cards retain their existing closed-by-default,
  non-overlapping layout.

## Required local gates

Run heavyweight gates sequentially and require at least 10 GiB free:

1. focused executor/provider-error tests;
2. focused Core auth/model-state and host callback tests;
3. focused Bravo quota, fallback, context, management, and streaming tests;
4. root `go test ./... -count=1`;
5. root compile verification;
6. nested Bravo `go test ./... -count=1`;
7. nested Bravo `go test -race ./... -count=1`;
8. nested Bravo `go vet ./...`;
9. nested Bravo `go build ./...`;
10. Management Center tests, lint, type-check, and production build;
11. clean diff and secret scan.

## Isolated canary

Build the exact candidate source into an isolated container on a free loopback
port with disposable state and fake upstreams:

- fake Claude returns the verbatim `credits_required` Fable 5 payload;
- one fake Claude sibling model succeeds, proving the credential is not
  globally disabled;
- fake Codex returns a context overflow for a 372k candidate;
- fake Codex can emit only `response.created` and then the observed generic
  server-error event; a Codex-first route must discard that translated prelude
  and complete once through its healthy Claude fallback;
- another configured candidate remains untouched, proving the router does not
  guess about context capacity.

Assert the ordered provider calls, model-scoped cooldown, unchanged sibling
availability, redacted subscription issue, attempt timeline, composite
credits-plus-context failure, reverse Codex-to-Claude pre-content fallback,
coherent streaming termination, and zero production state access.

Restart the isolated Bravo candidate (or otherwise clear only its in-memory
runtime state) without deleting Core's persisted cooldown files, then repeat
the affected route. Assert that `claude-fable-5(xhigh)` still blocks
`claude-fable-5` only on the original auth, while sibling Claude models, another
Claude auth, and Codex fallback remain available. The canary smoke performs
this with an explicit `--restart-container CLIProxyAPI-Bravo-*` target and
refuses the production container name.

## Verified pre-publish candidate evidence

The exact 2026-07-28 `0.7.9` pre-publish inputs are:

- Management Center source archive
  `sha256:990a3160303fcddd5b03a2006f4bbc47e8f5acc626079bb2f87e6505da6bb972`;
- Management Center `dist/index.html`
  `sha256:74da7ec03778a29b284a9fe7729c611707f5d098cbe6cd828763b018dd40171e`;
- tested backend code snapshot (before these evidence-only documentation edits)
  `sha256:82f7560d24167373c6a80aedd61f9f53f0b60f6395e963a951c8465dee947804`;
- Linux/arm64 `CLIProxyAPI`
  `sha256:244b235e1873766c03bbcedff2f9f905680d9cbf3379b811b1145a0463effe51`;
- Linux/arm64 `cliproxy-healthcheck`
  `sha256:45a95656a1cbd8949aa50fa35696ef0fa57e10ce8156009b6cd034310e7b6fea`;
- Linux/arm64 `bravo.so`
  `sha256:09ace309dd75c386c5f5a06d17497ae4b49c8533290e6d9439dfda5176e2bc0a`.

The candidate passed:

- focused provider-error, host callback, executor, runtime issue, expiry,
  generation, persistence, streaming, and redaction tests;
- the complete root Go test and build;
- the complete nested Bravo test, race, vet, and build gates;
- all 102 Management Center tests, lint, production build, and changed-file
  Prettier checks;
- an isolated synthetic-provider canary: 8 scenarios passed, 0 failed.

The 8/8 canary covered the composite Fable 5 monthly-spend plus Codex context
failure, same-auth Claude sibling availability, redacted subscription
presentation, all three downstream protocols, event-aware Responses
reconstruction/order, and an actual canary-container restart. Immediately
after that restart, the persisted provider/auth/base-model barrier prevented a
new probe of the exhausted tuple while the allowed fallback remained usable.

A targeted scan of the resulting canary state and logs found zero forbidden
fields. `bravo-state.json` retained only reviewed sanitized detail and the
model-scoped expiry; it contained no raw provider JSON, request ID, payment/CTA
data, credential material, or original provider response body.

The canary also reproduced a delayed observability defect before release.
Core briefly recorded the provider model state, then auth-file persistence and
model re-registration reset that transient snapshot while Bravo's own model
cooldown remained active. Subscription management now merges safe Core
diagnostics with the active Bravo cooldown. The same runtime barrier therefore
drives both routing and the operator-visible model restriction for its complete
TTL.

Production was not restarted or modified by any of these tests. It remained
healthy on the existing Bravo 0.7.8 image.

### 2026-07-29 reverse-provider follow-up candidate

The Codex-prelude streaming fix was rebuilt as the isolated image
`cliproxyapi-local:v7.2.94-bravo-native0.7.9-stream-fallback-canary1`:

- image ID
  `sha256:6532006659d751e265d5e08f1053462bb14f0e2864b19686700fde1857385eae`;
- Linux/arm64 `CLIProxyAPI`
  `sha256:37328785667213cc26e42a4a963fe6ca55ee0da1e35b23c97e11343c62092ff0`;
- Linux/arm64 `cliproxy-healthcheck`
  `sha256:45a95656a1cbd8949aa50fa35696ef0fa57e10ce8156009b6cd034310e7b6fea`;
- Linux/arm64 `bravo.so`
  `sha256:ab6a5a471418a0d0d82df01319633303dfad27fc24c91d19467f1c83ddc1d2ef`;
- unchanged reviewed Management UI
  `sha256:74da7ec03778a29b284a9fe7729c611707f5d098cbe6cd828763b018dd40171e`.

The focused nine-case protocol matrix, complete nested Bravo tests, complete
nested race suite, nested vet/build, complete root tests, and root compile
verification passed. The updated synthetic-provider canary then passed 9/9,
including the production-shaped sequence `response.created` followed by
`model_execution_failed/server_error` on a Codex-first route and a coherent
Claude fallback. A post-run scan of three canary state/log files found zero
forbidden fields.

Both the canary and production containers were healthy after the test.
Production remained on Bravo 0.7.8 with restart count zero and was not
modified.

### Pending before publication

- Google Chrome controlled through Playwright verified the exact UI bytes at
  desktop width. The model selector is initially collapsed; expanded model
  rows, search, independent scrolling, prompt-cache disclosure, and
  subscription cards render without overlap. No project or canary setting was
  created or changed during the visual pass.
- The Management Center and backend changes have not been committed, pushed, or
  merged. `deploy/aws/release.env` must receive the exact merged frontend
  commit, not a temporary feature-branch SHA.
- After both merges, all release artifacts must be rebuilt from the exact
  merged commits and the final image must repeat the isolated canary.
- The health-gated production replacement, post-cutover smoke, rollback record,
  and task-owned cleanup remain pending.

Nothing in this pre-publish section authorizes GitHub publication or production
cutover before those gates are complete.

## Deferred Core follow-up

Core currently persists an auth result through the provider token storage,
which rewrites the auth file and triggers its watcher. The subsequent model
registry reconciliation can reset otherwise live `ModelStates`. Bravo is
protected because its own provider/auth/model cooldown is authoritative for
Bravo routing and management presentation.

A separate Core change should make the watcher/reconciliation mode explicit
and preserve runtime model state during ordinary auth-file rewrites. That work
must not broaden this release's reviewed provider-error contract or make
plugin-local cooldowns global Core truth.

## Publish, production, and cleanup gates

Only after all local and canary checks are green:

1. Commit and push the exact tested backend and Management Center source.
2. Merge focused pull requests into their stable production branches.
3. Build production artifacts from the merged commits.
4. Retain the current image and configuration/state backup.
5. Perform one short health-gated production container replacement.
6. Verify health, plugin version, project count, subscription model issue,
   attempt analytics, and one protocol smoke.
7. Remove only disposable canary containers, mocks, task-owned build contexts,
   superseded candidate images, and task-owned build cache. Never run a broad
   Docker prune.
