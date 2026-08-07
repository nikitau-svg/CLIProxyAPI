# Bravo implementation notes

This file tracks the project requirements and the current implementation so
the decisions survive across sessions.

## Mandatory release gate

Every Bravo production change follows this order without skipping or
reordering stages:

1. Write the test plan and define the expected failure before changing code.
2. Implement the smallest reviewed change.
3. Build and deploy an isolated canary without modifying production state.
4. Run focused regression tests, the relevant full test suites, and a
   protocol-level canary smoke that proves the real fallback path.
5. Only after every check is green, commit and publish the exact tested source
   to GitHub.
6. Deploy the image built from that published source with one short production
   cutover and run a read-only smoke test.
7. Remove disposable canary containers, temporary build contexts, unused
   candidate images, and task-owned Docker build cache without pruning images
   or volumes still used by production.

Any failed or incomplete stage stops the release before GitHub or production.
The tested commit, image ID, canary evidence, production smoke, and cleanup
result are recorded in this file.

Before any Go or Docker build, the host must have at least 10 GiB free. Heavy
test/build jobs run sequentially. If the guard fails, the release stops and
only exact task-owned builders, candidate images, and caches may be removed;
shared Docker prune commands are forbidden.

## Product requirements captured

- Bravo is a configurable smart policy inside CLIProxyAPI, not a separate
  Auth2Api service.
- One project smart key must behave consistently through both the OpenAI and
  Anthropic protocols.
- A logical model exhausts all equivalent subscriptions before moving to the
  next provider class.
- Model, effort, tools, tool results, streaming, token count, and error
  contracts must remain coherent across providers.
- Projects and mappings must scale to many entries. Lists are closed by
  default and secondary detail is progressively disclosed.
- Every project can use the full Bravo pool or a strict personal/work
  subscription allowlist. Primary ownership, retries, and fallbacks must all
  obey the same fail-closed boundary.
- Consumption must be attributable by project, period, physical subscription,
  logical route, and physical provider model without exposing credentials.
- Claude personal and team workspaces with the same email remain distinct by
  organization/workspace identity.
- Limit state must distinguish provider-confirmed reset, inferred cooldown,
  and unknown.
- Provider account exhaustion must be classified by both HTTP status and
  reviewed provider error signals. Anthropic can return exhausted extra usage
  as HTTP 400 `invalid_request_error`; that account-level condition must retry
  the next subscription/provider while ordinary malformed-request 400s remain
  terminal.
- A provider-confirmed spend restriction for one named physical model is not
  account-wide exhaustion. It must cool only that provider/auth/model tuple,
  keep sibling models eligible, and continue through allowed subscriptions and
  mapped providers.
- Context-window overflow is request-scoped, never an availability cooldown,
  and must retain the ordered failure that caused the fallback rather than
  hiding it behind the terminal secondary error.
- Operator surfaces may expose only reviewed, sanitized provider detail.
  Request IDs, payment/CTA fields, credentials, raw provider JSON, and other
  unreviewed fields stay out of API responses, UI, analytics, logs, and
  persistent state.
- The integrated admin UI must expose a model compatibility/update center.
  Newly discovered provider models are never silently promoted into Bravo:
  the operator sees whether the current host, plugin, contract matrix, and
  route policy support them, exactly what is missing, and whether adopting the
  model needs only a signed catalog/policy update or a rebuilt CLIProxyAPI
  image.
- The production listener stays unchanged until a canary passes.

## Implemented in this branch

- Native `plugins/bravo/go` C-ABI dynamic plugin.
- Logical model registry, exact aliases, family policies, and provider-neutral
  named effort.
- Explicit client effort from OpenAI Chat `reasoning_effort`, OpenAI
  `reasoning.effort`, and Anthropic `thinking.type=adaptive` plus
  `output_config.effort`. Claude Code can therefore use its normal
  `/effort`, `--effort`, or `CLAUDE_CODE_EFFORT_LEVEL` controls.
- Claude Code 2.1.218's live request shape
  `thinking: {type: adaptive, display: omitted}` is accepted as the verified
  presentation-only companion to named effort. Bravo strips that source
  thinking object before host execution. Other display modes and manual
  budgets remain fail-closed.
- Explicit `low`, `medium`, `high`, `xhigh`, and `max` follows the whole
  fallback route. Each physical candidate floors the request to its greatest
  verified supported level; `auto` keeps that candidate's configured default.
  Events and response metadata retain requested and effective values.
- Signed Claude thinking replay is preserved only on the native
  Anthropic-Messages-to-Claude route. Bravo skips Codex and Claude models
  without verified thinking support for that turn; if no compatible Claude
  candidate remains, the request fails closed with 422 before an upstream
  call. A fresh named-effort request can still use either provider.
- OpenAI Responses string `input` is canonicalized to a user
  `message`/`input_text` item before physical-provider translation. The core
  Responses-to-Claude translator applies the same normalization.
- Machine-readable plugin error codes, including `bravo_effort_invalid` and
  `bravo_contract_unverified`, survive the core auth/result boundary and the
  OpenAI Chat, Responses, and Anthropic error envelopes while retaining
  request-scoped semantics. Existing status-derived OpenAI codes are
  unchanged.
- Non-exclusive SHA-256 smart-key authentication and per-key model scope.
- Smart-key routing for both `bravo/*` and unprefixed exact model names.
- Deterministic candidate/account plan with pinned auth and one-attempt core
  controls.
- Structured non-stream and stream errors with `Retry-After`.
- Semantic account-quota retry classification. Anthropic's HTTP 400
  `out of extra usage` and
  `Third-party apps now draw from your extra usage` signals are converted to
  `bravo_subscription_quota_exhausted`, so the same client call can continue
  through another Claude account and then a mapped Codex candidate; unrelated
  invalid-request 400s remain terminal. The reviewed quota failure cools the
  exhausted credential across Claude models, while 429 and upstream faults
  retain their physical-model scope.
- Reviewed structured provider errors are parsed and sanitized at the executor
  boundary, carried as typed safe detail through Core, host callbacks, and
  Bravo, and never persisted as their original response body.
- Anthropic `credits_required` with a named physical model becomes
  `bravo_subscription_model_credits_exhausted`. Bravo tries another eligible
  credential for that model and then the next mapped provider while keeping
  sibling Claude models on the original credential eligible.
- Exact model-credits exhaustion without a valid `Retry-After` uses a
  15-minute minimum probe barrier. Generic retryable 429 retains the configured
  cooldown, and any valid explicit provider `Retry-After` remains
  authoritative.
- Active provider/auth/base-model cooldowns are stored as an optional
  sanitized schema-v2 field in `bravo-state.json`, restored across plugin and
  container restarts, pruned by exact-instance expiry, and isolated across
  `state_path` generations. Existing schema-v1 migrations and schema-v2 files
  without the field continue to load.
- Context overflow terminates the current route without cooling any account or
  blindly retrying another unverified context size. Ordered attempt
  diagnostics retain both the primary credits failure and the secondary
  context mismatch.
- Recognized provider preludes are buffered before client-visible content for
  both Claude and Codex physical providers. Anthropic Messages, OpenAI Chat
  Completions, and OpenAI Responses may therefore all fall back across
  providers before visible content, while incomplete, unknown, or post-content
  streams fail closed without provider splicing.
- No stream fallback after the first client-visible payload.
- OpenAI Chat, OpenAI Responses, and Anthropic Messages request-contract
  detection.
- Host-delegated token counting.
- Redacted management endpoints and a compact, searchable dashboard.
- Authenticated project CRUD and key rotation Management API. Project keys are
  returned once, only SHA-256 is persisted, and the stable project record
  carries `primary_auth_ids`, model scope, and policy.
- Enforced per-project allocator: primary subscriptions are tried first and
  may drain to zero; secondary subscriptions require confirmed session and
  weekly quota above tariff-specific reserve floors.
- Enforced `allowed_auth_ids` on every allocator path. Empty retains the
  backward-compatible all-subscriptions mode; a non-empty list is a hard
  personal/work pool, every primary must be inside it, and stale identities
  fail closed.
- `x1` defaults to 50%/50% session/week reserves and `x5` to 30%/30%, with
  explicit per-subscription tariff override and per-tariff UI editing.
- Provider-aware `x20` for Codex/OpenAI Pro and Pro Lite, with 20%/20%
  session/week defaults. Claude Pro remains `x1`.
- Least-stressed secondary ordering combines normalized quota headroom,
  tariff-normalized weekly usage, concurrent/pending reservations, and stable
  rendezvous selection instead of walking a fixed credential list.
- Persistent project/auth usage ledger for requests, failures, latency, and
  input/output/reasoning/cache/total tokens. Atomic state is mode 0600 and
  stored outside auth discovery. The same snapshot retains only sanitized
  active model cooldowns; raw provider bodies and request identifiers are
  excluded.
- Analytics schema v2 migrates historical totals without fabricating old
  project/subscription joins. It retains hourly buckets for 31 days and daily
  buckets for 400 days, and exposes authenticated redacted breakdowns for
  project × subscription × logical/physical model.
- Typed host-owned live quota acquisition for Claude and Codex. The plugin
  never receives credential files, upstream URLs, tokens, paths, or raw
  provider JSON.
- Provider-confirmed `inactive` session resets and `not_applicable` windows
  distinguish unused rolling windows from missing data without weakening
  fail-closed validation.
- Native Bravo project administration in `/management.html`: project cards
  are closed by default and support create, edit, model allowlist, search,
  enable/disable, rotate, and delete. Subscription cards, quota meters, reset
  explanations, primary selection, tariff floors, and model allowlists use
  progressive disclosure. The one-time key dialog includes Claude Code and
  OpenAI quick starts; the read-only health dashboard remains separate.
- Native project analytics in `/management.html`: 24h/7d/30d/90d/custom
  periods, previous-period comparison, KPI cards, token mix, graph/table, CSV,
  coverage messaging, and per-subscription/model attribution.
- Authenticated hot route editor with validation, preview, save and reset.
  Default text routes use one preferred Claude candidate and one equivalent
  Codex candidate; obsolete same-family Claude hops are not inserted between
  them. Capabilities remain read-only and fail closed.
- Thirty-six logical models, including three Codex-only image aliases.
- Live-verified built-in web search through OpenAI Chat, OpenAI Responses, and
  Anthropic Messages for both Claude and Codex.
- Live-verified image generation and edit through `bravo/image`,
  `bravo/gpt-image-2`, and `bravo/gpt-image-1.5`.
- Fail-closed handling for unverified provider tools, web-search domain
  filters, image-generation streaming, OpenAI Chat/Responses vision,
  file/document input, manual reasoning budgets/summaries, cross-provider
  signed thinking replay, structured output, and background execution. Vision
  through Anthropic Messages, named effort, and native Claude signed-thinking
  replay are verified exceptions.
- Claude sampling fields are preserved when thinking is absent. When thinking
  is active, unsupported `temperature`, `top_p`, or `top_k` combinations are
  rejected synchronously instead of being silently removed.
- Pinned Claude Opus 5, Sonnet 5, and Fable 5 model-contract overlays protect
  the runtime when the remotely refreshed catalog is missing or incomplete.
  Default-on and always-on thinking, forced-tool disable rules, named effort,
  and sampling restrictions are validated before the upstream request.
- Authenticated compatibility endpoint plus a compact, collapsed Management
  UI advisor. It compares the static/live host catalog, reviewed Bravo
  profiles, effective YAML model map, and logical routes; reports
  code/YAML/route fixes with exact targets and snippets; and never applies a
  suggestion automatically.
- Synchronous stream capability preflight, so unsupported streaming requests
  return a precise 422 before any upstream call.
- Linux/arm64 shared-plugin build in the canary Dockerfile.

## Bravo 0.7.7 source, canary, and production evidence

- The test plan is recorded in
  `plugins/bravo/QUOTA_FAILOVER_TEST_PLAN.md`. Before the classifier changed,
  the exact Maria-OpenClaw message produced four expected red failures:
  host classification, pre-commit streaming, full Claude-to-Codex fallback,
  and cross-model credential cooldown.
- Three additional red tests reproduced the pre-built-plan defect with
  `max_attempts: 2` in regular execution, token counting, and streaming:
  a newly cooled same-account Claude sibling consumed the final plan slot and
  hid the eligible Codex fallback.
- The classifier uses the precise reviewed signal
  `third-party apps now draw from your extra usage`. Generic malformed,
  schema, and tool-choice HTTP 400 errors remain terminal. Quota evidence is
  aggregated across the host error code, message, response body, and stream
  detail, so an earlier generic usage-limit field cannot hide a later exact
  account-wide Anthropic signal.
- Confirmed account-quota exhaustion now cools the credential across Claude
  models. HTTP 429, upstream faults, and model-entitlement errors retain their
  previous physical-model scope. `max_attempts` caps actual upstream calls;
  cooldown, contract, and unavailable-lease skips do not spend the budget.
- Focused quota/fallback/fail-closed tests passed, followed by the full Bravo
  test suite, explicit hard-cap tests for execute/count/stream, the Bravo race
  detector, vet, and shared-plugin build. The full CLIProxyAPI Go test suite
  and repository-wide build also passed sequentially.
- GitHub PR
  [`#8`](https://github.com/nikitau-svg/CLIProxyAPI/pull/8) merged the tested
  feature commit
  `183fa79e4a2382ed0d35a26f26234dee2cdd55b9` as runtime source commit
  `ef08c3a9736f8ee63c2bd168f35c001f770aa72e`. Both `bravo/stable` and
  `clean/bravo-0.7-production` pointed to that merge before the final image
  was built; `main` remained unchanged.
- The pre-publish Linux/arm64 canary image was
  `cliproxyapi-local:v7.2.94-bravo-native0.7.7-canary-review3-ui9578c1a`
  (`sha256:250a33332fbf5dc543693939d0b6a99d9c9f262852eaf69fbe8e0a1e8fff00a7`).
  Its binary reports
  `v7.2.94-bravo-native0.7.7-canary-review3`/`prepublish-review3`.
  It embedded the current production Management UI
  `9578c1aaa24884847aca61dfe3ce9340c538c08b`
  (`sha256:a971f98da6f816d67604d461c76d592b102a4c3c1c428f52e065581ad22a55be`).
- The isolated canary used no real credentials or subscription tokens. A fake
  Claude endpoint returned the verbatim production HTTP 400 and a fake Codex
  endpoint returned a valid Responses stream. `bravo/sonnet` completed through
  Codex Terra in the same downstream stream, with the Claude event normalized
  to retryable `bravo_subscription_quota_exhausted`.
- An immediate `bravo/haiku` request skipped the same cooled-down Claude
  credential and completed through Codex Luna. The mock observed exactly one
  Claude Sonnet call followed by Codex Terra and Codex Luna. The temporary
  project was deleted; the canary was healthy with zero restarts, was removed,
  and ports 18319/18991 were released.
- Chrome/Playwright verified the integrated production Management page after
  service recovery. Refreshing quotas completed in roughly two seconds; all
  five accounts showed provider-confirmed `только что`, the refresh button
  re-enabled, no UI alert appeared, and the 1470 px viewport had no horizontal
  overflow. No subscription or project setting was changed. That interactive
  check ran against the preceding 0.7.6 runtime; the final image embeds the
  identical UI bytes, and the post-cutover HTTP checksum matched.
- The final Linux/arm64 image
  `cliproxyapi-local:v7.2.94-bravo-native0.7.7`
  (`sha256:394234330795a0f70bd4d06db7ce97b132983db7d7a5d865ff8698231bf2d017`)
  was built from the published merge, not retagged from the pre-publish
  candidate. Its binary reports
  `v7.2.94-bravo-native0.7.7`,
  `ef08c3a9736f8ee63c2bd168f35c001f770aa72e`, and
  `2026-07-26T22:17:46Z`; the embedded Management UI checksum is
  `a971f98da6f816d67604d461c76d592b102a4c3c1c428f52e065581ad22a55be`.
- The strict zero-credential canary was repeated against that exact final
  image. It again observed exactly Claude Sonnet HTTP 400, Codex Terra
  success, then Codex Luna success while the cooled Claude account was
  skipped. Bravo reported 0.7.7, the container was healthy with zero
  restarts, the disposable project was removed, and ports 18319/18991 were
  released before production changed.
- Production moved once from
  `cliproxyapi-local:v7.2.94-bravo-native0.7.6-uihotfix1-9578c1a`
  (`sha256:52a160b0e81b001bb3dced881eb06dac998be1228dc56294f77e6c8915ea426e`)
  to the final 0.7.7 image. A mode-0700 forensic backup is retained at
  `backups/pre-bravo-0.7.7-20260726T222935Z`; rollback would restore only the
  old compose/image unless state corruption were proven.
- The post-cutover read-only smoke reported 8 projects, 5 subscriptions,
  39 routes, 64 ordinary-key models, and compatibility 23/23 with no action
  required, identical to the baseline. The config checksum remained
  `b39df08c7fd83f8003851a6a152065be657ee14f238c87ef6f6d9a913ffbd6fc`,
  the served Management UI checksum matched the pinned bytes, and production
  remained healthy with zero restarts and a zero healthcheck failing streak.
- Cleanup removed the disposable canary/mock, candidate compose, pre-publish
  image, task-owned source/artifact/runtime directories, and the private
  BuildKit cache records created by the final build. No broad Docker prune was
  used. The previous production image and backup remain available for
  rollback; the Mac mini remained above the 10 GiB build-safety floor after
  cleanup.

### Release safety incident and guard

- Multiple heavyweight builds were mistakenly started concurrently and reduced
  Mac mini free space to roughly 156 MiB. Docker Desktop's network/API proxy
  then stalled, so every container lost outbound provider access even though
  the production container still reported healthy.
- The production image, compose configuration, keys, plugin data, and project
  state were not changed by the incident. Docker Desktop was restarted and the
  exact previous production image recovered with outbound networking.
- The release gate now requires at least 10 GiB free, forbids concurrent heavy
  builds, and allows only exact task-owned cache cleanup.

### Tracked follow-up: Claude Code auto-mode safety classifier

- Claude Code 2.1.220 can spend nearly its whole 60-second caller deadline on
  the first Claude safety-classifier attempt. Codex may then be selected with
  less than two seconds remaining and receive `context canceled`, after which
  Claude Code reports that it cannot determine whether `Edit` is safe.
- Bravo 0.7.7 fixes the exact quota classification and avoids poisoning Codex
  with a same-account Claude retry, but it does not reserve fallback time from
  the caller deadline.
- A separate release must first normalize caller cancellation as a stable
  non-retryable 499 without cooldown, then design a Core + SDK + Bravo
  deadline-aware attempt budget that preserves 10–15 seconds for fallback.
  This broader contract change is intentionally not mixed into the quota patch.
- Bravo 0.7.8 uses a streaming-bootstrap cross-provider hedge rather than
  terminating an established provider connection on a timer. All three
  incident traces are `stream=true`; the client does not expose its real
  60-second classifier deadline to the server (`X-Stainless-Timeout` is 600
  seconds in those traces), and repository policy forbids adding a
  post-connect network timeout. After a configurable delay, one compatible
  cross-provider stream may start before the first payload; the first
  successful bootstrap is forwarded and the host cancels the losing
  callback-owned attempt at the first downstream emit. Forked callback scopes
  defer Core success/failure accounting: only the selected winner or a real
  provider failure is committed, while a superseded/client-canceled attempt is
  discarded without changing Core quota or availability. A possible upstream
  spend still retains one conservative pending allocator reservation until a
  confirmed quota refresh. Non-stream generation and token counting stay
  sequential to avoid duplicate full-response cost and tokenizer races. Root
  cancellation and a superseded hedge are request-scoped 499 outcomes and
  never provider cooldowns. The complete red/local/canary/real-client plan is
  recorded in `plugins/bravo/DEADLINE_FALLBACK_TEST_PLAN.md`.

## Bravo 0.7.8 pre-publish evidence

- The release candidate is based on `origin/bravo/stable` commit
  `3745763f2da20d95fd12bff23287b578728e3575`; the divergent `main` branch is
  not a release target.
- Red tests first reproduced root cancellation as a retryable 500, a stalled
  Claude bootstrap consuming the fallback window, missing child-callback
  cancellation/accounting ownership, first-emit cancellation becoming a local
  bridge error, and a superseded hedge inflating the failure counter.
- Focused Core/SDK tests, the full root test suite and build, the full Bravo
  suite, Bravo race detector, vet, shared-plugin build, Ruby syntax checks,
  and `git diff --check` all passed sequentially. Execute and count remain
  sequential and are covered by unit gates; the incident and deterministic
  canary are streaming-only.
- The pre-publish Linux/arm64 image is
  `cliproxyapi-local:v7.2.94-bravo-native0.7.8-canary-6fa0567b`
  (manifest list
  `sha256:86334429430e0ddcdc5bf2570c10788d59119dd1882aba04b9475df97261440b`,
  config
  `sha256:298d1d61e61eae44b0dd9cee18fd373ed643e117773b3cafb385690ae0711793`).
  Its binary reports `v7.2.94-bravo-native0.7.8` and
  `prepublish-6fa0567b`. It reuses the production Management UI byte-for-byte
  (`sha256:a971f98da6f816d67604d461c76d592b102a4c3c1c428f52e065581ad22a55be`).
  The separate inline Bravo dashboard is generated by the plugin and does
  change: its attempt summary now separates neutral `переключено` from real
  errors.
- The isolated deterministic canary used one fake Claude API-key provider
  that withheld its first payload and one fake Codex provider that returned a
  valid stream. Repeated runs proved one invisible Claude-to-Codex hedge,
  exactly one canceled Claude loser, one coherent downstream response, a
  neutral `bravo_attempt_superseded` 499, Codex-only Core success accounting,
  zero loser Core failures/cooldowns, and winner-only project analytics. The
  smoke is repeatable: it compares counter deltas and removes its disposable
  project on every exit.
- A separate real-client canary copied one still-valid Codex access token into
  its isolated auth directory after removing `refresh_token`; the production
  credential file was never changed and the temporary copy was deleted
  immediately after the gate. Claude Code 2.1.220 ran with
  `--permission-mode auto --tools Read,Edit` and without `--allowedTools`,
  `acceptEdits`, or bypass mode. Both direct `opus` and direct `sonnet` runs
  performed a real `Edit` and exact read-back verification through Bravo.
  Across the two runs, Bravo recorded six Codex 200 winners and six neutral
  Claude 499 supersedes, Core recorded six Codex successes and zero Claude
  successes/failures, cooldowns stayed zero, and project analytics attributed
  every request to Codex. The Opus run did not emit a separate Sonnet network
  classifier call in this Claude Code build, so Sonnet was also exercised
  directly rather than claiming an unobserved internal call.
- Chrome/Playwright verified the inline Bravo dashboard against the isolated
  canary after a real deterministic hedge. It rendered
  `1 успешно · 1 переключено · 0 ошибок`, kept the superseded count visually
  neutral, reported no console errors, and had no horizontal overflow at
  either 1470 px or 390 px.
- The shipped hedge default of 40 seconds, explicit zero-disable behavior, and
  maximum-attempt enforcement are covered by config/unit gates. The
  deterministic and real-client canaries intentionally used a one-second
  hedge to exercise the lifecycle repeatedly; they do not claim to validate
  production latency at the 40-second default.
- The one-time real-canary project, smart key, project-id file, and copied
  access token were deleted. Both canary containers had restart count zero.
  Production remained on
  `cliproxyapi-local:v7.2.94-bravo-native0.7.7`, container
  `550d44dfd00a3f4c00beedffba73d5601ed9b8aa2449b45e4c8803b2810c80ec`,
  healthy/running with restart count zero throughout the canary stage.
- A final audit questioned cleanup registration during callback closure.
  `callbackContextRegistry.addCleanup` synchronously runs a rejected cleanup;
  a deterministic forked-child regression closes the callback inside
  `ExecuteModelStream` and proves the bridge count returns to zero. No product
  change was needed for that finding. No Go/plugin/runtime build input changed
  after the binary canary; only tests, reusable canary harnesses, and evidence
  documentation were hardened. The published merge commit will be the release
  identity, will be rebuilt into the final image, and that exact image will
  receive a fresh isolated smoke before production.

## Bravo 0.7.8 published release and production evidence

- GitHub PR
  [`#10`](https://github.com/nikitau-svg/CLIProxyAPI/pull/10) merged tested
  feature commit
  `433736a43009e76b941daca7d45cbe50b5b4e124` into `bravo/stable` as
  `9c8eeed09b3ce0911ef5f92a57b2a53c4622d819`. The feature and merge commits
  have the same source tree,
  `bbf0c910198f9bd5b4e268bfea1cd19e3a6d6156`. GitHub Actions
  `translator-path-guard` and `pr-test-build` both passed before merge.
- The final Linux/arm64 image was rebuilt from a `git archive` of that exact
  merge commit; it was not retagged from the pre-publish candidate. The image
  is `cliproxyapi-local:v7.2.94-bravo-native0.7.8`
  (`sha256:10add75aaceb25512b46e8a9d5a3b68cb86a060fb38866a768c2f40f06c1d871`;
  platform manifest
  `sha256:5f7336efca73079b644be8459958dc044009cc9a69f81948aedc362b067f8575`;
  config
  `sha256:552521b404e6ed6cf642296cfea633fbb4037c1c41f97bb4e66786c25b4dcf1b`).
  Its binary reports `v7.2.94-bravo-native0.7.8`,
  commit `9c8eeed09b3ce0911ef5f92a57b2a53c4622d819`, and build date
  `2026-07-27`. The embedded Management UI checksum remains
  `a971f98da6f816d67604d461c76d592b102a4c3c1c428f52e065581ad22a55be`.
- A clean isolated canary was created from the final image on
  `127.0.0.1:18319`, with fresh state and fake providers only. The exact
  final-image smoke passed 8/8: Bravo 0.7.8/config identity, subscription
  readiness, invisible slow-Claude-to-fast-Codex hedge, one canceled loser,
  neutral superseded accounting, Codex-only Core success, zero cooldown, and
  winner-only analytics. Its disposable project was removed and the container
  exited with restart count zero.
- Immediately before cutover, production 0.7.7 was healthy with restart count
  zero. The read-only baseline was 8 projects, 5 subscriptions, 39 routes,
  64 ordinary-key models, and 23/23 compatible profiles with zero required
  actions. The Compose diff changed only the recorded image ID and image tag.
  A pre-cutover backup of Compose, config, and Bravo state was saved at
  `/Users/juloaipc/projects/cliproxyapi-prod/backups/pre-bravo-0.7.8-20260727T023039Z`;
  auth files were intentionally excluded. The retained rollback image is
  `cliproxyapi-local:v7.2.94-bravo-native0.7.7`
  (`sha256:394234330795a0f70bd4d06db7ce97b132983db7d7a5d865ff8698231bf2d017`).
- Compose performed one health-gated service replacement. Production now runs
  container `081e9e687f192a7b869ea20b21f06baca15e4de6d084d365cda4eb2ff7619135`
  from the exact final image. It is healthy, has restart count zero,
  `OOMKilled=false`, and retains the same port, network, mounts, 1 GiB memory,
  2 CPU, and 256-PID limits. The post-cutover read-only baseline matched all
  pre-cutover counts exactly.
- A production Anthropic Messages streaming smoke created a one-time
  `sonnet`-only project, sent `bravo/sonnet` with adaptive thinking and
  explicit `effort=low`, observed assistant text plus a complete
  `message_stop`, and removed the project in `ensure`. An earlier strict probe
  with only 64 output tokens was deliberately not accepted: the successful
  Claude response spent that budget on adaptive thinking/signature and had no
  visible text. Final project count remained 8.
- Cleanup removed all three task-owned 0.7.8 canary containers, both
  superseded candidate image tags, the fake-provider process and listener,
  exported build contexts/archives, isolated canary runtimes, and local/remote
  smoke files. Only the final 0.7.8 and rollback 0.7.7 images remain. Shared
  BuildKit cache was not broadly pruned because it also serves 28 running
  containers and individual layer ownership cannot be proven; host free space
  after cleanup was 12 GiB.

## Bravo 0.7.9 pre-publish credits/context evidence

- The release follows
  `plugins/bravo/CREDITS_CONTEXT_TEST_PLAN.md`. RED phases reproduced loss of
  Anthropic's structured `credits_required`, unsafe raw-body persistence,
  credential-wide presentation of a model-only restriction, hidden ordered
  diagnostics, cross-protocol stream-prelude commits, restart loss for config
  credentials, stale-expiry deletion, and a setter crossing a `state_path`
  generation.
- The exact Management Center source archive is
  `sha256:990a3160303fcddd5b03a2006f4bbc47e8f5acc626079bb2f87e6505da6bb972`.
  Its release build produced `dist/index.html`
  `sha256:74da7ec03778a29b284a9fe7729c611707f5d098cbe6cd828763b018dd40171e`.
  All 102 frontend tests passed, together with lint, production build, and
  changed-file Prettier checks.
- The tested backend code snapshot, before these evidence-only documentation
  edits, is
  `sha256:82f7560d24167373c6a80aedd61f9f53f0b60f6395e963a951c8465dee947804`.
  Full root tests/build and the nested Bravo test, race, vet, and build gates
  passed. The exact Linux/arm64 artifacts are:
  `CLIProxyAPI`
  `sha256:244b235e1873766c03bbcedff2f9f905680d9cbf3379b811b1145a0463effe51`,
  `cliproxy-healthcheck`
  `sha256:45a95656a1cbd8949aa50fa35696ef0fa57e10ce8156009b6cd034310e7b6fea`,
  and `bravo.so`
  `sha256:09ace309dd75c386c5f5a06d17497ae4b49c8533290e6d9439dfda5176e2bc0a`.
- The first isolated synthetic-provider canary passed 8/8. It proved the composite
  Fable 5 monthly-spend plus Codex context failure, persisted model barrier
  across an actual canary-container restart, immediate fallback without
  re-probing the exhausted tuple, same-auth Claude sibling availability,
  redacted management presentation, coherent Anthropic/Chat/Responses
  pre-content fallback, and event-aware Responses output/order.
- A targeted scan of canary state and logs found zero forbidden raw provider
  fields. The snapshot retained only the reviewed code, safe summary,
  provider/auth/base-model scope, and expiry; no request ID, payment/CTA data,
  credentials, or original provider JSON was present.
- Production was not restarted or modified during these gates. It remained
  healthy on the existing Bravo 0.7.8 image.
- A follow-up candidate added provider-neutral prelude buffering and passed the
  focused nine-case protocol matrix plus the complete nested/root gates. Its
  isolated synthetic-provider canary passed 9/9, including the
  production-shaped Codex `response.created` followed by
  `model_execution_failed/server_error` and a coherent Claude fallback.
- Google Chrome controlled through Playwright verified the exact embedded
  Management UI at desktop width. The project form opens with model access
  collapsed; after expansion, model rows, search, independent scrolling,
  prompt-cache disclosure, and subscription cards render without overlap.
  The UI interaction created no project and changed no canary configuration.
- GitHub publication, the merged-commit rebuild and final-image canary, the
  one-time production cutover, production smoke, and task-owned cleanup remain
  pending. This pre-publish evidence does not authorize skipping any of those
  release gates.

## Bravo 0.7.6 source and canary evidence

- Subscription identity is now operator-first everywhere: the auth-file note
  is the bold primary label; without a note, the safe fallback is
  `workspace · email`, then provider plus a redacted analytics ID.
- Technical auth names, filenames, indices, and credential identifiers are
  never promoted into Management API display names.
- Project analytics now includes a redacted subscription-usage timeline. The
  newest eight time buckets are visible and older buckets remain collapsed by
  default, while the existing totals and per-subscription/model attribution
  remain backward-compatible.
- The latency calculation was verified against production data:
  `598067 / 55 = 10873.945 ms`. It is the complete provider-attempt duration,
  including a streamed response through completion, not ping or time to first
  token. The UI therefore labels it `Среднее время ответа`, formats it as
  `10,9 с`, and exposes a keyboard-accessible explanation.
- Full repository Go tests/build, focused Bravo race/vet tests, and the exact
  Bun 1.3.14 Management Center verification passed. The only full-repository
  vet messages are unchanged pre-existing core warnings outside this patch.
- Exact canary image
  `cliproxyapi-local:v7.2.94-bravo-native0.7.6-eb7a418b` built for
  `linux/arm64`; build manifest digest:
  `sha256:a136430cd39aadb958555014f19703e644ca1d9bd17dd475269b7e086b21e210`.
- Canary stayed healthy with zero restarts. Read-only schema/identity/timeline
  smoke passed, followed by the existing 21/21 Management API and protocol
  smoke including temporary project lifecycle and fail-closed contracts.
- Chrome/Playwright QA used the exact embedded Management Center artifact.
  Desktop and 390 px layouts have no horizontal overflow; account/project
  disclosures are closed initially; note and workspace/email identities are
  bold; the latency help is focusable; the timeline and collapsed older
  periods render correctly; browser console errors are empty.

## Bravo 0.7.5 source, canary, and production evidence

- Per-project Claude prompt-cache TTL is managed in the standard Management
  Center as `auto`, `5m`, or `1h`; OpenAI/Codex is explicitly
  `provider_managed`.
- CLIProxyAPI core applies trusted Bravo cache metadata after translation in
  execute, stream, token-count, retry, and pre-response fallback paths.
- Full Go tests/builds, Bravo race/vet/build, Management Center tests/lint/
  typecheck/build, management smoke, and Chrome/Playwright QA passed.
- The exact production image is
  `cliproxyapi-local:v7.2.94-bravo-native0.7.5-d1a76342`
  (`sha256:b05b66f484502593fdc7da04e5dc161dd867d0ae9f44049dd324bfef40fbb6de`).
- A live Claude subscription probe produced a 72,399-token cache write and an
  identical 72,399-token cache read. A live Codex-primary Responses probe
  reported 15,104 cached tokens on its second identical request.
- Both probes used temporary projects and deleted them. Production retained
  seven original projects, five subscriptions, healthy container status, and
  zero restart loops after the single cutover.
- Analytics schema v2 records cache creation, cache read, and provider cached
  tokens in project/subscription/model breakdowns.

## Bravo 0.6 source and canary evidence

- Full CLIProxyAPI `go test ./... -count=1` passed after the Opus 5,
  compatibility-advisor, and semantic failover changes.
- Full Bravo `go test -race ./... -count=1` and `go vet ./...` passed.
- Exact Bun 1.3.14 release container passed 88 WebUI tests, ESLint, TypeScript,
  and the single-file production build.
- Isolated zero-credential canary passed plugin status, compatibility,
  Opus-5-to-Sol routes, project create/redaction/patch/rotate/delete, and
  analytics schema-v2 management smoke checks.
- Unit/integration coverage proves Claude HTTP 400 quota exhaustion and
  reviewed 400/422 model-entitlement failures continue to Codex, while
  malformed request/schema/contract failures remain terminal.
- Chrome/Playwright desktop and 400 px mobile QA passed with all disclosures
  closed by default, working compatibility search/filter/details, and no
  horizontal overflow.
- The AWS clean installer follows `bravo/stable` plus a tracked
  `deploy/aws/release.env`; a release tag is optional and never required for a
  fresh install.

## Historical 0.5 reference release evidence

- CLI image: `cliproxyapi-local:v7.2.94-bravo-native0.5.0`
- Image digest:
  `sha256:605c9888b2f58c2d3db37575efecd2663b90e298f60b005315b073672b420b18`
- Plugin: `plugin-dist/bravo-v0.5.0.so`
- The health-gated reference deployment remained healthy with zero restart or
  OOM events after cutover.
- The mixed Claude/Codex test pool keeps same-email Claude personal/team
  workspaces distinct by workspace identity.
- Live quota is confirmed for the test pool. Claude team plans
  auto-map to `x5`, Claude personal/pro to `x1`, and Codex Pro to `x20`.
- Production state is schema v2. Historical project totals are retained and
  exact project/subscription/model coverage starts with the 0.5.0 deployment.
- Full final-image production protocol smoke: 12 passed, 0 failed.
- Final-image canary Bravo 0.5 management smoke: 9 passed, 0 failed.
- Controlled pre-payload 429 failover: 2 passed, 0 failed, including a strict
  pool that could not escape to a disallowed Codex account.
- Real Claude Code 2.1.218 with `--model opus --effort xhigh` passes on
  both the final canary and production through `bravo/opus`.
- Anthropic Messages vision passes on production through Claude-first
  `bravo/opus` and Codex-first `bravo/sol`, including PNG/JPEG nested inside
  historical tool results, adaptive `xhigh`, non-stream, and stream.
- Ordinary non-Bravo API keys remain valid.
- Chrome/Playwright production QA passed with compact disclosures, Russian UI,
  project analytics, strict-pool editing, decoded provider labels, and zero
  horizontal overflow. A temporary strict-pool project was created, issued,
  inspected, and deleted on canary.
- Persistent usage/quota state is mode 0600 in a separate `bravo-data` volume,
  not in the credential discovery directory.
- The cutover retains an on-disk compose/config/schema-v1 state backup; the
  rollback procedure stops schema v2 before restoring all three files.

## Verification gates

- Unit and contract tests in `plugins/bravo/go`.
- Full repository compile and `go test ./...`.
- Canary plugin load and model registration.
- Live text matrix:
  - Claude through OpenAI Chat, Responses, and Anthropic.
  - Codex through OpenAI Chat, Responses, and Anthropic.
- Live tools and tool-result turns in all supported cells.
- Live Anthropic Messages vision through Claude and Codex, including nested
  tool-result images with adaptive `xhigh`.
- Token count through an Anthropic client.
- Streaming response identity and late-stream failure behavior.
- Forced fallback after exhausting every account of the primary candidate.
- Smart-key scope and ordinary-key non-interference.
- Chrome/Playwright desktop and narrow-viewport dashboard review.

Historical completed source/build evidence for 0.5.0:

- Full repository `go test -count=1 -timeout=10m ./...`.
- Focused host/plugin race tests, full Bravo race suite, and `go vet`.
- WebUI TypeScript, ESLint, Vite production build, and 77 Bun tests.
- Final Linux/arm64 image with server, healthcheck, Bravo 0.5.0 plugin, and
  native Management UI:
  `sha256:605c9888b2f58c2d3db37575efecd2663b90e298f60b005315b073672b420b18`.
- Final-image canary protocol smoke 12/12; Bravo 0.5 management/pool/routes/
  analytics smoke 9/9; controlled hot failover 2/2.
- Real Claude Code `--model opus --effort xhigh` passed against final canary
  and production.
- Production cutover performed one successful container recreation, migrated
  state v1 to v2, passed the 12-cell protocol gate before commit, and retained
  an automatic rollback bundle. Canary was stopped afterward.

Completed source/build evidence for 0.4.1:

- Full repository `go test -count=1 -timeout=10m ./...`.
- `go test -race -count=1 ./...` in the final Bravo module.
- Native server, healthcheck, and `bravo-v0.4.1.so` builds.
- Canary protocol smoke: 14/14; management lifecycle: 20/20; quota refresh:
  4/4.
- Direct and smart-key vision conformance for Claude Opus and Codex Sol:
  user image and nested tool-result image, non-stream and stream, with
  adaptive `xhigh`.
- Real Claude Code `--model opus --effort xhigh` passed on canary and
  production.
- Production upgrade used an automatic health-gated rollback and retained the
  complete `0.4.0` compose/config/state backup.

Completed source/build evidence for 0.4.0:

- `go test -race ./...` in the final Bravo module.
- Full repository `go test -count=1 -timeout=10m ./...`, including the
  Responses string-input translator, auth error-code regressions, and SDK
  error-envelope/SSE and live quota callback tests.
- Native server and embedded container healthcheck builds.
- Linux/arm64 image
  `cliproxyapi-local:v7.2.94-bravo-native0.4.0` built with
  `bravo-v0.4.0.so`, a static liveness probe, and the current Management UI.
- WebUI TypeScript, full ESLint, Vite production build, and 68 Bun tests
  passed for the embedded Management UI.
- Reusable quota/allocator smoke: 11 tests and 67 assertions, followed by a
  live production pass with four confirmed subscriptions.
- All 0.4.0 canary and production release gates listed above are complete.
  Cross-provider
  replay of already-signed Claude thinking and typed errors after a stream has
  emitted client-visible payload remain explicitly deferred contracts, not
  failed release gates.

Historical 0.2.1 conformance baseline:

- `go test -race -count=1 ./...` in the Bravo module.
- Focused core, translator, image-executor, image-handler, auth, ABI, and API
  package tests.
- Full repository `go test -count=1 -timeout=2m ./...`.
- Smart-key text/tools/tool-result matrix through all three protocols for both
  Codex-first and Claude-first logical routes.
- Direct web-search matrix: 6/6 provider/protocol cells.
- Smart-key web-search matrix: 3/3 protocols.
- Smart-key image generation and edit: HTTP 200/200, with image bytes kept in
  memory and never logged.
- Unsupported smart image streaming: synchronous HTTP 422 in about 20 ms.
- Dashboard: v0.2.1, 36 models, all disclosures closed initially, no
  horizontal overflow at desktop or 400 px, 44 px minimum interactive target,
  labeled search, polite live status, keyboard disclosure, and no email,
  smart-key, or SHA-256 leakage.

## Deferred until evidence exists

- A provider response with no validated percentage remains `unknown`; Bravo
  never fabricates reserve headroom. Provider-confirmed zero-utilization
  windows with no active reset timer are represented separately as
  `inactive`, and provider plans without that class of window as
  `not_applicable`.
- Image-generation streaming, OpenAI Chat/Responses vision, file/document
  input, arbitrary structured-output schema/background contracts, web-search
  domain filters, manual reasoning budgets/summaries, and unknown
  provider-specific built-in tools remain fail-closed until promoted by a live
  conformance run.
- Special direct-only Codex models are not presented as general Bravo models.

## Next allocator/product increments

- Quota-throttle audit evidence (2026-08-07): the historical implementation
  used a 60-second quota TTL in the request planning path. In the 100 model
  requests immediately preceding the earliest retained user-visible quota-429
  evidence (2026-08-06 18:17:12--19:56:41 UTC), traffic remained active in 28
  distinct minutes. Replaying the old TTL against those request timestamps
  produces 24 refresh cycles: approximately 72 Claude `/api/oauth/usage`
  calls plus 72 Claude profile calls for three credentials. No Bravo dashboard
  request occurred in that sample, so the hot request path alone was sufficient
  to create the polling load. Exact historical quota calls cannot be listed
  because the old callback did not emit per-attempt audit events and persisted
  only its latest snapshot.
- The replacement design must take quota discovery out of the inference hot
  path. Use an adaptive background schedule, retain last-known-good windows,
  coalesce UI and scheduler work, respect account-scoped `Retry-After`, and
  maintain a separate low-rate egress budget. Provider profile metadata must
  keep its long TTL and must never be paired with every usage refresh.
- Add the audit events that the historical implementation was missing: record
  every quota attempt by pseudonymous account and effective egress, including
  its trigger (scheduler, dashboard refresh, or operator action), timestamp,
  HTTP status, and `Retry-After`. Verify the new cadence and single-flight
  behavior with a controlled canary reproduction before changing production.
- Redesign quota refresh only after that audit: account-scoped cooldown,
  separate safe egress pacing keyed by a credential-free proxy/direct
  fingerprint, stale-last-good quota retention, coalesced manual refresh, and
  no inference denial caused solely by failure of the quota metadata endpoint.
- Signed release-feed/GHCR delivery on top of the implemented compatibility
  advisor, so the UI can link a required code fix to a ready canary image and
  release notes. Binary updates remain operator-approved and pass through the
  canary/rollback gate rather than modifying production in place.
- Per-project route overrides on top of the current global hot route editor.
- operator-defined per-project budgets, rate caps, and alerts;
- explicit fairness debt/credit so intermittent users stay responsive under
  sustained heavy-key load;
- session/week quota history and burn-velocity forecasting per subscription;
- adaptive recommendations for reserve floors while retaining hard operator
  limits;
- policy templates/import-export for 25-30 projects;
- richer per-project cost attribution when providers expose trustworthy
  monetary usage;
- live promotion of the remaining vision/schema/background/tool contracts.
