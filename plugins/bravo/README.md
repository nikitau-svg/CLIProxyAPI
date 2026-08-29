# Bravo Smart Router

Bravo is a native CLIProxyAPI plugin that turns a project API key into a smart,
provider-independent routing policy. The same key and logical model work
through OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages.

The plugin owns retries and fallback. It tries every eligible account for the
current physical model before moving to the next mapped model/provider, pins
each nested call to one credential, and disables the host's implicit retry
loops. A stream may fall back only before the first client-visible payload.

When a reviewed provider error reports `credits_required` for a named physical
model, Bravo treats it as model-scoped spend exhaustion. It may try another
eligible credential for that model and then the next mapped candidate, while
sibling models on the original credential remain eligible. If a later fallback
cannot fit the request in its context window, the terminal diagnostic retains
both ordered failures; context overflow itself is request-scoped and never
creates a credential, model, or provider cooldown.

## Client endpoints

OpenAI-compatible clients:

```text
Base URL: http://<gateway-host>:8317/v1
API key:  the project Bravo key
Model:    bravo/fast
```

Anthropic-compatible clients:

```text
Base URL: http://<gateway-host>:8317
API key:  the same project Bravo key
Model:    bravo/fast
```

Claude Code can keep its normal aliases and effort controls:

```bash
export ANTHROPIC_BASE_URL=http://<gateway-host>:8317
export ANTHROPIC_AUTH_TOKEN='<project Bravo key>'
export ANTHROPIC_DEFAULT_OPUS_MODEL=bravo/opus
export ANTHROPIC_DEFAULT_SONNET_MODEL=bravo/sonnet
export ANTHROPIC_DEFAULT_HAIKU_MODEL=bravo/haiku
claude --model opus --effort xhigh
```

Inside Claude Code, `/effort low|medium|high|xhigh|max` is carried through the
Bravo route. The same applies to `--effort` and
`CLAUDE_CODE_EFFORT_LEVEL`. Claude Code sends adaptive thinking and named
effort through the Anthropic contract; the user does not need a separate Bravo
command. Claude Code 2.1.218 also sends `thinking.display: "omitted"`;
Bravo accepts this live-verified presentation preference and strips the source
thinking object before executing a physical candidate.

The current Bravo release also preserves PNG/JPEG image blocks sent through Anthropic
Messages, including images nested in an older Claude Code `tool_result`.
This path is live-verified for both Claude and Codex physical candidates,
with and without streaming. Images do not change effort routing.

OpenAI Chat accepts `reasoning_effort` or `reasoning.effort`; OpenAI Responses
accepts `reasoning.effort` and the compatibility form `reasoning_effort`.
OpenAI Chat also accepts `response_format: {"type":"json_object"}`. Codex
receives its native JSON-mode equivalent; Claude executes the request without
rejecting the format hint and receives an explicit instruction to emit one bare
JSON object. If a non-streaming provider still wraps one valid object in an
outer `json` Markdown fence, Bravo removes that fence before returning the
OpenAI Chat response. Strict OpenAI Chat `json_schema` remains fail-closed until
that entry protocol is verified cross-provider. Anthropic Messages
`output_config.format` is separately live-verified for the `bravo/haiku`
Claude Haiku / Codex Luna route: Claude receives the native schema and Codex
receives the equivalent Responses `text.format` contract.
OpenAI Chat sampling controls are also treated as advisory when routed to
Claude Code: `temperature` and `top_p` are omitted from the translated Claude
request because the provider rejects `top_p < 1` and mixed sampling controls.
The original request remains available unchanged to a later Codex fallback.
Explicit effort wins over the candidate's mapped default. `auto` uses each
mapped physical candidate's default. When a physical model does not expose the
exact requested level, Bravo uses the greatest supported level below it and
reports `bravo_requested_effort` and `bravo_effective_effort` separately.
Manual token budgets, reasoning summaries, `none`, and `minimal` remain
fail-closed.

A fresh adaptive/named-effort request can route to Claude or Codex. If a later
Anthropic Messages turn replays a signed Claude `thinking` block, Bravo keeps
that turn on native Claude candidates with verified thinking support. It skips
Codex rather than dropping or rewriting the signature; if no compatible
Claude candidate remains, the request fails closed with 422 before an
upstream call.

OpenAI Responses string `input` is normalized to a user
`message`/`input_text` item before candidate translation, so the short and
canonical input forms share the same fallback behavior.

Image generation and edit use the ordinary OpenAI image endpoints with one of:

```text
bravo/image
bravo/gpt-image-2
bravo/gpt-image-1.5
```

Manage Bravo from the standard authenticated CLIProxyAPI panel:

```text
http://<gateway-host>:8317/management.html
```

Open **Bravo** in the panel navigation. Projects, subscription pools, routes,
quotas, and analytics are all managed there.

## Create a project key

Open the authenticated CLIProxyAPI panel:

```text
http://<gateway-host>:8317/management.html
```

Open Bravo in the plugin area, select **Create project**, name the project,
choose all subscriptions or a strict personal/work subscription pool, choose
all logical models or a model allowlist, and create it. A primary subscription
must be inside the allowed pool and may belong to only one active project.
Allowed secondary subscriptions may be shared by multiple projects. The
plaintext `brv_...` key is displayed once. The same page can edit,
enable/disable, rotate, or delete a project; project details are closed by
default.

Use the new key as either an OpenAI API key or an Anthropic API key. Rotating
immediately replaces the old key. Deleting a project invalidates its key.
Revoked projects cannot be re-enabled or rotated.

The one-time key dialog also shows two project-key endpoints. They require the
same `brv_...` key and never expose credential identities or other projects:

```text
GET /v1/bravo/limits?format=json
GET /v1/bravo/limits?format=text
GET /v1/bravo/routes
```

`limits` returns confirmed provider reset windows plus project-only usage for
the latest 30 days (daily series and provider/model breakdown). A project gets
one fresh limits result every five minutes; repeated calls receive the cached
snapshot with HTTP 200 and `cached: true`. It performs no provider request.
`routes` returns the effective logical routes allowed by this key, their
preferred/fallback order, physical provider/model, effort, capabilities, and
whether each route comes from the built-in default or an operator override. In
the 0.9 preview both responses also state the selected adaptive mode, whether
routing is enforced, and the invariant that the allocator generates no
additional provider requests.

## Adaptive allocator 0.9 preview

Preview.13 keeps the narrow, reversible production breaker and adds a separate
opt-in soft assist while the hard forecast remains experimental. The default
remains `observe`; the recommended production routing change is explicit
`adaptive_allocator_mode: breaker`.

The modes have deliberately small contracts:

- `off` disables adaptive telemetry and routing decisions;
- `observe` records the counterfactual decision while preserving the ordinary
  allocator's attempt order, provider-call count, fallback and responses;
- `breaker` never applies token/headroom forecasts. It skips an auth/model only
  behind a breaker created by a real reviewed quota/rate-limit failure, then
  immediately continues the configured neighboring route. If no neighbor
  answers and the configured provider-call budget remains, one original
  attempt may run as a global single-flight recovery probe. At most one such
  call is dispatched per breaker generation; scheduled and recovery proofs
  share one per-subscription turnstile across model/account scope, and
  competing requests never wait. A late proof cannot erase newer quota
  evidence. Local cancellation or bootstrap failure is inconclusive and keeps
  the breaker closed. A local `bravo_adaptive_*` decision can never become the final client error; an
  exhausted operator `max_attempts` may still return the ordinary aggregate
  route failure produced by real neighboring attempts;
- `assist` keeps the evidence breaker and may atomically defer only a
  non-primary attempt with fresh confirmed quota and complete token calibration
  for both effective quota windows. Deferral preserves the exact already
  authorized provider/model/auth attempt in the sequential request tail; it is
  not a rejection. Primary, partial/unknown/stale calibration, compact and
  allocator bypasses, protected breaker proofs, runtime saturation, and every
  finite `max_attempts` fail open to baseline behavior. Streaming assist
  disables hedging only for an immutable request snapshot with unlimited
  `max_attempts`; finite budgets keep the baseline hedge because forecast
  assist is fully inert. The global mode alone
  is insufficient: a project must also opt in with `adaptive_assist: true`;
  every other project remains breaker-only. Use this mode only for an isolated
  beta canary; setting the project flag to false is its narrow kill switch and
  returning the global mode to `breaker` disables all canaries;
- `enforce` atomically reserves the predicted session and weekly/model-weekly
  cost against a fresh, provider-confirmed quota. If the current attempt is
  confirmed unsafe, busy at the guarded edge, or behind a provider-confirmed
  breaker, it is not dispatched. This full forecast mode remains experimental
  and is not the production default.

Neither `breaker` nor `enforce` waits, creates a cross-project queue, wakes
quota polling or adds background provider requests. Unknown state and
forecast-runtime saturation fail open to the ordinary allocator. Once a real
breaker exists, coordination saturation is intentionally fail-closed for that
single protected proof: it is skipped locally rather than risking a provider
stampede. The ordinary allocator remains
authoritative for project allowlists, ownership, disabled credentials,
cooldowns and tariff floors. A primary has a zero owner floor but confirmed
zero quota is still exhausted.

The ordinary allocator's narrow scheduled-reset bypass is stampede-safe too.
When a confirmed zero belongs to an already elapsed reset but the background
snapshot has not refreshed yet, exactly one non-blocking request may probe that
auth plus reset generation. Competitors immediately continue the neighboring
route. A pre-dispatch failure releases the lease; once provider dispatch starts,
that generation stays consumed until a fresh confirmed snapshot or a newer
reset generation supersedes it. This gate is bounded to 4096 entries, never
waits, and does not wake quota polling.

The independent edge state machine deliberately uses cached headroom and
actual provider outcomes rather than treating a token forecast as proof of a
provider failure:

```text
Green     -> normal concurrency
Guarded   -> fresh confirmed quota near its guard band: one in-flight attempt
Tripped   -> only after an actual quota/rate-limit failure
Half-open -> after expiry: exactly one non-queued probe
```

In `observe`, `would_skip_busy`, `would_skip_tripped` and
`would_withhold` remain counterfactual and the attempt still runs. In
`enforce`, the corresponding attempt is recorded as `not_dispatched`, the
audit increments `routing_changes_applied`, and execution continues without a
retry loop. Only a real provider quota/rate-limit outcome may trip a breaker;
after expiry exactly one Half-open probe owns the turnstile.

Shadow commitments and learned uncertainty have a five-minute half-life by
default and become exactly inert after thirty minutes. Runtime state is bounded
to 4096 tracked identities, 256 cooled commitments per identity and 512 live
forecast reservations per identity; stale live leases recover after two hours.
Edge leases and breakers are separately bounded. Saturation is visible and
fail-open for shadow/forecast work; protected scheduled and recovery proofs
are the narrow fail-closed local-skip exception. Runtime state is intentionally
discarded on restart. The standard
Management Center, `/v1/bravo/limits`, and `/v1/bravo/routes` show the current
mode and aggregate cooling state without credential identities.

Every real inference attempt also feeds a separate privacy-safe shadow audit.
The request path performs only a non-blocking enqueue; JSON encoding, disk
writes, `fsync`, and rotation happen in one telemetry worker. The queue is
bounded to 1024 records, memory to 4096 records, and disk to two JSONL files of
4 MiB each (8 MiB total). Queue overflow or an unwritable disk drops telemetry
and raises a warning, but never blocks a model request or changes routing. The
records contain the logical/physical model, shadow decision and numeric
headroom, provider outcome, latency, and sanitized error code. They never
contain project IDs, credential IDs, keys, headers, prompts, request bodies, or
model responses.

The authenticated Management API exposes a 1–168 hour report in JSON or
Russian text:

```text
GET /v0/management/bravo/adaptive-audit?hours=24&recent=20&format=json
GET /v0/management/bravo/adaptive-audit?hours=24&recent=0&format=text
```

The same 24-hour summary is visible in the existing subscription page. It
reports actual requests/execution attempts/fallbacks, shadow `would_admit` and
`would_withhold` decisions, successful attempts shadow would have withheld,
quota failures shadow would have admitted, enforced `not_dispatched` attempts,
applied routing changes, and queue/disk loss. Additional provider requests must
remain zero in every mode. A clean observe report becomes `ready_for_review`
only after at least 100 requests spanning at least six hours; this is evidence
for a human decision, never an automatic mode switch.

Preview.5 additionally calibrates the shadow estimate against actual usage
tokens already emitted by normal inference. Session, weekly and model-weekly
quota windows learn independent percentage-point-per-token rates; they are not
summed or forced through one scalar. Until a profile has enough confirmed
intervals the existing conservative shape estimate remains in use. The
Management/project status exposes only aggregate per-window calibration rows,
and the audit separates fully `token_calibrated_*` attempts from partial and
cold/legacy ones, with an independent verdict for the new formula.
Profiles are bounded, decay with a 24-hour half-life, survive restart, contain
no prompts or responses, and gain routing authority only under the explicit
`enforce` mode with fresh confirmed quota.
The invariants and release gates are in
[`docs/architecture/ADAPTIVE_QUOTA_0_9_CONTRACT.md`](docs/architecture/ADAPTIVE_QUOTA_0_9_CONTRACT.md),
[`ADAPTIVE_TOKEN_CALIBRATION_TEST_PLAN.md`](ADAPTIVE_TOKEN_CALIBRATION_TEST_PLAN.md),
and [`ADAPTIVE_EDGE_GATE_TEST_PLAN.md`](ADAPTIVE_EDGE_GATE_TEST_PLAN.md).

No database, credential, YAML or state-file migration is required. Existing
analytics continue to load, while runtime leases are rebuilt empty. A safe
rollout starts in `observe`, checks the adaptive audit, then hot-reloads the
explicit `enforce` value. Returning to `observe` removes adaptive routing
authority without replacing the image; Bravo 0.8.11 remains the stable binary
rollback point.

## Shared transport pool and safe error diagnostics

Provider HTTP/2 transports are shared within a strict
subscription/provider/egress boundary. Auth material and HPACK state therefore
never cross identities. The process retains at most 256 idle transport
identities for ten minutes and retires an idle HTTP/2 connection after 90
seconds. Active entries may temporarily exceed the idle bound and are never
evicted or forced through a hidden queue; they are trimmed after becoming
idle. Request-scoped custom transports are never cached, and a request without
a stable auth identity gets an isolated transport.

When full `request-log` is disabled, the default error-only policy is safe
metadata capture:

```yaml
error-log-capture:
  mode: metadata
```

`metadata` records method/path, masked headers, bounded declared/consumed byte
counts, completion, HTTP status and sanitized machine-readable error fields.
It does not retain or write request, response or upstream bodies, unmasked
authentication headers, prompts, model output or raw provider messages. `off`
disables forced request-error files. `body` is reserved and rejected by
configuration rather than silently enabling payload capture.
Forced files use mode `0600` in a `0700` directory and are never forwarded to
Home logging. `error-logs-max-files` still bounds retained files.

## Per-project prompt caching

Each project has a collapsed **Prompt caching** section in the standard
Management Center. Claude exposes only the provider-supported choices
`auto`, `5m`, and `1h`. OpenAI/Codex is intentionally read-only and labelled
`provider_managed`: when the client supplies a supported cache identity, Bravo
isolates it by project, while the subscription endpoint owns retention.

The setting is authenticated project metadata, not a client-controlled header.
CLIProxyAPI's core executor applies it after request translation to the native
target schema for ordinary, streaming, token-counting, retry, and pre-response
fallback attempts. Bravo never stores prompt bodies or model responses.

A retry on the same provider/account can reuse an eligible exact prefix.
Switching provider or account can legitimately start with a cache miss because
provider caches are isolated. A miss continues as a normal request and does
not alter the generated answer. Prompt caching improves repeated eligible
prefixes; it does not make a cold request faster and provider write pricing
still applies.

The implementation follows the current provider contracts:

- [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)

## Smart-key configuration

Only a SHA-256 digest is stored in YAML:

```yaml
plugins:
  enabled: true
  dir: plugin-dist
  configs:
    bravo:
      enabled: true
      prefix: bravo/
      require_smart_key: true
      max_attempts: 0
      cooldown_seconds: 30
      compact_bypass_cooldown_seconds: 900
      adaptive_allocator_mode: observe # off, observe, breaker, beta assist, or experimental enforce
      smart_keys:
        - id: prj_example
          name: default-project
          sha256: "<sha256-of-the-project-key>"
          enabled: true
          status: active
          models: ["*"]
          allowed_auth_ids: []
          primary_auth_ids: []
          adaptive_assist: false # explicit per-project beta opt-in
          policy:
            prompt_cache:
              anthropic_ttl: 5m
```

`max_attempts: 0` means all eligible accounts may be tried. Ordinary API keys
keep native CLIProxyAPI routing and cannot enter the `bravo/*` namespace.
The configured `cooldown_seconds` still controls a generic retryable 429 with
no provider hint. Exact model-credits exhaustion without a valid
`Retry-After` uses a conservative 15-minute probe barrier; any valid explicit
`Retry-After`, including a shorter value, remains authoritative.

Client-visible Bravo execution errors are localized in Russian while retaining
their stable machine-readable `code`, HTTP status, and `Retry-After`. A final
failure describes the complete safe route, including candidates withheld by
CLIProxyAPI's internal reserve floors and the physical fallback that ultimately
failed.

For Claude Code, a genuine `/compact` request may cross only the internal
allocator reserve floor when a project-authorized Claude credential is healthy
and still has confirmed positive provider quota. The detector requires the
Claude protocol, Claude CLI user agent, session ID, and the current compaction
prompt; historical `/compact` text does not qualify. Authorization boundaries,
provider/Core cooldowns, disabled subscriptions, and exhausted or unknown quota
remain fail-closed. A committed bypass starts the configured per-project/session
cooldown and emits a redacted Russian warning; `0` disables this behavior.

The authenticated CLIProxyAPI Management API owns project-key persistence:

```text
GET    /v0/management/bravo/projects
POST   /v0/management/bravo/projects
PATCH  /v0/management/bravo/projects
DELETE /v0/management/bravo/projects
POST   /v0/management/bravo/projects/rotate
```

`POST` and `rotate` return `plaintext_key` exactly once. Only its SHA-256 digest
is sent to the host configuration persistence callback and stored in YAML.
`PATCH`, `DELETE`, and `rotate` take the stable project `id` in their JSON body;
`DELETE` also accepts `?id=...`. Project configuration already reserves
`allowed_auth_ids`, `primary_auth_ids`, and `policy` for the subscription
allocator. An empty allowed list is the backward-compatible all-subscriptions
mode. A non-empty list is a hard authorization boundary for every primary,
retry, and fallback; stale entries fail closed.

## Per-project allocation and live quota

The current release implements the **subscription-communism** contract: all
authorized projects may borrow genuinely free shared capacity, but ownership
and its protected reserve remain enforceable. There is no fixed per-project or
per-agent concurrency cap; concurrent reservations are instead accounted
atomically against the relevant subscription windows.

Project ownership, strict pools, and reserve policy work as follows:

- the persistent usage ledger is keyed by stable project ID and exact
  `auth_index`;
- a project can own one or more primary subscriptions; they are attempted before
  shared capacity regardless of host list order or current shared-account
  stress;
- a confirmed exhausted or unavailable primary immediately falls through to
  eligible shared capacity instead of failing the whole logical route;
- an owner may consume its own primary reserve down to confirmed zero, while a
  secondary may borrow only capacity that remains above the owner's configured
  session and weekly/model-weekly floors after all in-flight reservations;
- `allowed_auth_ids` limits every allocator path to the project's selected
  personal/work pool;
- secondary `x1` subscriptions default to 50% session/week floors and `x5`
  subscriptions to 30%/30%; provider-aware Codex/OpenAI Pro uses `x20` with
  20%/20%, while Claude Pro remains `x1`;
- both windows must remain above their independent floor after in-flight and
  pending reservation;
- confirmed secondary candidates are ordered by normalized quota headroom,
  tariff-normalized weekly tokens, reservations, and a deterministic
  rendezvous tie-break;
- unknown/stale quota is blocked for secondary use by default rather than
  treated as full;
- ordinary allocator bypass/fallback cannot cross an owner floor, known
  exhaustion, a disabled subscription or a mixed-pool rejection that contains
  any of those protected conditions;
- Claude accounts with an unused session (`utilization=0`, `resets_at=null`)
  remain confirmed at 100% with an `inactive` reset mode; a missing Codex
  window can be explicitly `not_applicable`.

Quota discovery is a background control-plane task, not part of inference or a
page read. Usage polling defaults to 15 minutes and is editable from the Bravo
subscription-pool section (5 minutes through 24 hours, with a warning below 10
minutes). The same panel shows persistent usage/profile provider-request
counters globally and per account. Profile metadata keeps its independent
six-hour interval. A 429 blocks only credentials sharing the same safe egress
fingerprint: direct accounts share one group, the same proxy shares one group,
and different proxies remain independent. Proxy URLs never enter plugin state
or management responses.

The authenticated allocator endpoints are:

```text
GET   /v0/management/bravo/subscriptions
GET   /v0/management/bravo/adaptive-audit
PATCH /v0/management/bravo/subscriptions
PATCH /v0/management/bravo/tariffs
POST  /v0/management/bravo/quotas/refresh
```

Project and credential usage summaries include requests, failures, latency,
input/output/reasoning/cache tokens, and total tokens. State is atomically
persisted outside the credential discovery directory in
`bravo-data/bravo-state.json`. The same schema-v3 snapshot optionally stores
active provider/auth/physical-model cooldowns with reviewed, sanitized
provider detail. Existing snapshots without that field continue to load.
Preview.11 adds no schema migration: transport entries, adaptive in-flight
reservations and edge leases are runtime-only and start empty after restart.

Schema v3 also retains hourly analytics for 31 days and daily analytics for
400 days. The authenticated analytics endpoint supports project,
subscription, provider, model, time-range, and interval filters while exposing
only stable redacted subscription IDs. Its compact `subscription_timeline`
shows which subscription served each populated hour or day without storing new
identity data. `latency_ms` is the complete provider-attempt duration, including
stream consumption; `average_latency_ms` is that total divided by provider
attempts, not time to first token:

```text
GET /v0/management/bravo/analytics
```

In the adaptive 0.9 preview the analytics payload also includes
`quota_consumption`. It attributes provider-confirmed percentage-point drops to
projects, physical/logical models, effort, and configured tariff. Session,
weekly, and model-weekly windows remain separate. The management view exposes a
per-window project ranking and shared-pool residual; project-key limits expose
only the caller's project. Residual observed burn is named
`external_or_estimator_gap` because the proxy cannot prove whether it came from
outside Bravo or from estimator error.

Subscription responses expose the operator-authored `note` separately and a
deterministic `display_name`. The display name prefers the note and otherwise
combines workspace and email; the legacy `label` remains the same display name
for older Management Center builds.

Each subscription may also expose a redacted `model_issues` list. A model card
can therefore explain that Fable 5 reached its monthly spend limit without
marking the whole Claude workspace unavailable. Raw provider JSON, request IDs,
payment state, CTA fields, tokens, and credentials are never part of this
management contract. Attempt analytics keeps the ordered subscriptions,
physical models, and safe reasons that led to a fallback.

The native Management UI provides 24h/7d/30d/90d/custom periods,
previous-period comparison, charts, tables, CSV export, and
project/subscription/logical-model/physical-model drill-down.

Global logical routes are editable at runtime with validation, non-persistent
preview, and reset to built-in defaults:

```text
GET  /v0/management/bravo/routes
PUT  /v0/management/bravo/routes
POST /v0/management/bravo/routes/reset
```

Capabilities are intentionally read-only. An unverified contract is rejected
instead of being promoted by configuration.

The authenticated compatibility advisor compares the host's static and live
model catalogs with reviewed Bravo contracts, the effective model map, and
logical routes:

```text
GET /v0/management/bravo/compatibility
```

The standard Management UI runs this check automatically and keeps the details
collapsed until they are needed. A newly discovered model remains fail-closed
and is classified by the first required action: host/catalog work is a code
fix, Bravo model-map work is a YAML fix, and logical assignment is a route
fix. Every suggestion names its target and includes a reviewable snippet;
suggestions are never applied automatically.

## Verified contract

Text logical models currently support:

- text and streaming;
- client-defined function tools;
- synthetic tool results;
- built-in web search;
- PNG/JPEG vision through OpenAI Chat on both Claude and Codex candidates;
- PNG/JPEG vision through Anthropic Messages, including nested `tool_result`
  history, on both Claude and Codex candidates;
- strict Anthropic `output_config.format` JSON Schema on the live-verified
  Claude Haiku / Codex Luna route;
- Anthropic token counting;
- OpenAI Chat, OpenAI Responses, and Anthropic Messages entry/exit protocols.

Image logical models currently support non-streaming generation and edit.

The plugin rejects unverified semantics instead of silently dropping them.
This currently includes image streaming, web-search domain filters, arbitrary
provider-built-in tools, manual reasoning budgets/summaries, cross-provider
signed-thinking replay, OpenAI Responses vision, file/document inputs,
structured output outside the reviewed Anthropic Haiku/Luna contract, and
background execution. Named effort is supported and
resolved against each physical model before execution; signed Claude thinking
replay is supported only on the native Anthropic-Messages-to-Claude route.

Machine-readable contract failures survive the core execution path and the
OpenAI Chat, Responses, and Anthropic error envelopes. Clients can distinguish
codes such as `bravo_effort_invalid`, `bravo_contract_unverified`, and
`bravo_subscription_model_credits_exhausted`. Contract and context-window
errors remain request-scoped and do not mark the selected credential
unhealthy. A model-credits error affects only the reported
provider/auth/physical-model tuple. Existing status-derived OpenAI codes remain
unchanged. Typed detail after a stream has already emitted client-visible
payload still requires the planned stream-close ABI extension; the current ABI
carries a legacy string at that point.

## Verification

```bash
cd plugins/bravo/go
go test -race -count=1 -timeout=5m ./...
```

`Dockerfile.canary` deliberately does not commit generated WebUI bytes. Build
the matching Management Center checkout first and copy its single-file output
into the ignored release-input directory:

```bash
# Expected layout:
#   ./CLIProxyAPI
#   ./Cli-Proxy-API-Management-Center
cd Cli-Proxy-API-Management-Center
bun install --frozen-lockfile
bun run verify

install -d ../CLIProxyAPI/.canary-dist
install -m 0644 dist/index.html \
  ../CLIProxyAPI/.canary-dist/management.html
```

Then, from the CLIProxyAPI repository root, load the pinned release manifest
and build the matching image:

```bash
. ./deploy/aws/release.env

docker build --platform "$RELEASE_PLATFORM" \
  --build-arg VERSION="$CLIPROXYAPI_VERSION" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(date -u +%F)" \
  -f Dockerfile.canary \
  -t "$CLIPROXYAPI_IMAGE" .
```

Reusable live harnesses:

```text
scripts/bravo-smoke.rb
scripts/bravo-image-smoke.rb
scripts/bravo-management-smoke.rb
scripts/bravo-claude-cli-smoke.rb
scripts/bravo-quota-allocator-smoke.rb
scripts/bravo-string-diagnostic.rb
scripts/bravo-vision-smoke.rb
scripts/bravo-credits-context-provider.rb
scripts/bravo-credits-context-canary-setup.rb
scripts/bravo-credits-context-smoke.rb
```

The live harnesses read credentials from files and avoid printing secrets or
image payloads.

`bravo/main` and `deploy/aws/release.env` are the installation channel.
Release tags are retained only as immutable rollback points. Moving the
installation channel requires the full Go suite, plugin race/vet,
risk-relevant protocol and management smoke checks, and controlled pre-payload
failover. Real Claude Code is additionally required when client
request/response translation,
effort/tool handling, stream presentation, or deadline behavior changes; a
provider-error-classification-only change may instead use the verbatim
production error in a protocol-level canary. A release that changes Management
UI bytes additionally requires WebUI tests/lint/typecheck/build and
Chrome/Playwright desktop/mobile QA. A backend-only release may reuse the exact
previously verified UI artifact when its pinned commit and SHA-256 are recorded
and the served bytes are verified after cutover.

Preview.13 has no one-shot migration. Preserve the mounted auth directory,
configuration and `bravo-data` volume; retain the previous image plus a
pre-cutover state copy. For a conservative canary, start with
`adaptive_allocator_mode: observe`, run the protocol/management smokes and
inspect `/v0/management/bravo/adaptive-audit`. Then hot-reload explicit
`breaker`. The optional `assist` needs `max_attempts: 0`, global mode `assist`,
and `adaptive_assist: true` on one isolated noncritical project; all unmarked
projects remain breaker-only. Clear the project flag for the narrow kill
switch, or return the global mode to `breaker` before considering an image
rollback. The bounded `*.hours.json` audit sidecar is created automatically,
contains aggregate counters only, and uses a writer sequence/checkpoint so a
late concurrent completion cannot disappear after a crash. Its assist counters
separately expose defer, tail dispatch/success, neighbor success and every
lifecycle invariant. Returning to `observe` is the broader routing rollback;
restoring the retained 0.8.11 image and its state copy is the stable rollback.
Never delete auth or usage state to perform a rollback.

The current clean-install guide is
[`AWS_INSTALL_RU.md`](../../AWS_INSTALL_RU.md). The operator guide for project
keys, model allowlists, existing routes, and provider-specific pools is
[`BRAVO_MODELS_AND_KEYS_RU.md`](../../BRAVO_MODELS_AND_KEYS_RU.md). The separate
`BRAVO_PRODUCTION_RUNBOOK_RU.md` is retained only as historical 0.5 migration
evidence.
