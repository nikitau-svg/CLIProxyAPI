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
  stored outside auth discovery.
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

## Bravo 0.7.7 source and canary evidence

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
  overflow. No subscription or project setting was changed.

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
