# Bravo implementation notes

This file tracks the project requirements and the current implementation so
the decisions survive across sessions.

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
- Synchronous stream capability preflight, so unsupported streaming requests
  return a precise 422 before any upstream call.
- Linux/arm64 shared-plugin build in the canary Dockerfile.

## Reference release evidence

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

Completed source/build evidence for 0.5.0:

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
