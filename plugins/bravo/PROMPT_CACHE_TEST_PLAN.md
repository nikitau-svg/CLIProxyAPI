# Bravo 0.7.5 Prompt Cache Verification Plan

## Baseline

- Production baseline: Bravo `0.7.4`
- Release candidate: Bravo `0.7.5`
- Baseline source commit: `e388b54181058006096aaaef9754135941cd1a8b`
- Release candidate commit: recorded in the canary release manifest
- Integration branch: `codex/bravo-0.7.5-cache-contract`
- Production must remain unchanged until every local and canary gate below passes.

## Goal

Prove that Bravo preserves or supplies each provider's native prompt-cache
contract without storing prompt bodies locally:

- Anthropic requests use valid `cache_control` breakpoints.
- OpenAI/Codex requests use a stable, correctly scoped `prompt_cache_key`.
- Cache usage survives protocol translation, streaming, and pre-response
  failover.
- Provider cache reads and writes are retained in token accounting and Bravo
  analytics.

This work does not add a local response cache. Provider prompt caches remain
the source of truth.

## Project Policy

- Every Bravo project exposes one Prompt Caching setting in the Management
  Center.
- Anthropic supports only `auto`, `5m`, and `1h`; every other value must fail
  closed before a request reaches an upstream provider.
- OpenAI/Codex subscription traffic is shown as `provider_managed`. Bravo keeps
  a project-scoped `prompt_cache_key`, but must not advertise an arbitrary TTL
  that the subscription executor does not accept.
- The policy is attached by trusted Bravo authentication metadata and consumed
  by the core executors after protocol translation. Client headers cannot forge
  or override it.
- Every pre-response retry and fallback re-enters the target core executor, so
  the same project policy is applied to every attempt using that provider's
  native schema. Provider and account caches remain isolated; a fallback cache
  miss is valid and must not break the request.

## Provider Contract

### Anthropic

- Cacheable prefixes are ordered as `tools -> system -> messages`.
- The default ephemeral TTL is 5 minutes; the supported extended TTL is 1 hour.
- A request may contain at most four cache breakpoints.
- Deferred tools are excluded from the initial prompt prefix. An automatically
  inserted breakpoint must target the last non-deferred tool.
- A real cache write is reported as `cache_creation_input_tokens > 0`.
- A real cache hit is reported as `cache_read_input_tokens > 0`.

### OpenAI/Codex

- Eligible exact prefixes are cached by the provider.
- `prompt_cache_key` must be stable for requests sharing the same prefix and
  isolated across unrelated Bravo projects or client sessions.
- For native OpenAI Responses, the client key `C` is scoped to a deterministic
  Bravo project key `B`; optional Codex identity isolation is then applied to
  `B`, never directly to `C`. HTTP, SSE, and WebSocket must derive the same
  `B`. Responses and errors restore `C` only in cache-key fields.
- An absent, empty, or null client key stays absent/empty. Bravo must not invent
  one project-wide cache identity that could merge unrelated conversations.
- Claude Code reasoning replay state is scoped by Bravo project in addition to
  session and agent identity.
- GPT-5.6-family requests should retain provider-supported cache options and
  explicit breakpoints; older models must remain compatible with automatic
  caching.
- A real cache hit is reported in `cached_tokens`.
- Models that report cache writes must retain `cache_write_tokens`.

Provider TTLs are not interchangeable. Bravo must not expose arbitrary values
that an upstream provider does not support.

## Local Test Matrix

### Payload preservation and injection

1. Project policy:
   - create a project with each supported Anthropic TTL and read the same value
     back through the management API;
   - patch the TTL and verify the next request uses the new value without
     restarting the host;
   - reject unknown fields and unsupported TTLs;
   - ignore cache-policy metadata from non-Bravo access providers.
2. Anthropic client -> Anthropic subscription:
   - preserve a client-supplied `cache_control`;
   - inject breakpoints when the client supplies none;
   - keep the breakpoint count at or below four;
   - preserve valid 5-minute and 1-hour TTL ordering.
3. Deferred Anthropic tools:
   - place an injected breakpoint on the last non-deferred tool;
   - do not add a breakpoint when every tool is deferred;
   - preserve an existing valid client breakpoint.
4. Anthropic client -> Codex subscription:
   - derive a stable `prompt_cache_key` from the client session/agent identity;
   - preserve the same key across non-streaming, SSE, and WebSocket execution;
   - keep child-agent identities stable but distinct from the parent.
5. OpenAI client -> Codex subscription:
   - preserve a valid client `prompt_cache_key`;
   - preserve supported cache options and breakpoints;
   - do not forward deprecated or unsupported fields to models that reject
     them.
6. OpenAI client -> Anthropic subscription:
   - translate the request first, then apply a valid Anthropic breakpoint;
   - do not leak OpenAI-only cache fields to Anthropic.

### Bravo routing and failover

1. Bravo's logical project cache identity and stable prompt prefix must not
   change during retries. The credential-scoped upstream identity may change
   when Bravo selects another account because provider caches are isolated;
   that legitimate cold miss must not be treated as a routing failure.
2. A provider or account switch may produce a legitimate cache miss; it must
   not be treated as a routing failure.
3. Cross-provider fallback must use the target provider's native cache
   contract rather than forwarding foreign cache fields.
4. Bravo must never retry after response bytes have been committed.
5. Unrelated projects must not be forced through the same OpenAI
   `prompt_cache_key`.
6. A recognized `bravo/*` or registered physical route stays inside Bravo even
   when the host's optimistic provider snapshot is empty. Project model scope
   is checked before pool exhaustion.
7. A valid Bravo project key also owns unknown unprefixed model IDs and fails
   them closed as `bravo_model_unknown` until the compatibility workflow has
   reviewed and registered an exact route. Ordinary non-Bravo keys retain
   native routing.
8. Registered physical model IDs remain callable with a Bravo project key and
   continue to obey that project's model scope, `allowed_auth_ids`, allocator,
   analytics, and retry policy.

### Usage and analytics

1. Preserve Anthropic `cache_creation_input_tokens` and
   `cache_read_input_tokens` in non-streaming and streaming usage.
2. Preserve OpenAI `cached_tokens` and `cache_write_tokens` wherever the
   provider returns them.
3. Attribute cache usage to the actual Bravo project, key, subscription,
   logical model, and physical model.
4. Keep ordinary input/output totals correct; cache-detail fields must not
   double-count total input tokens.
5. Analytics with older records or providers that omit cache details must
   remain backward compatible.

## Local Gates

The implementation may proceed only after a focused test demonstrates a real
gap. Required gates after any code change:

1. Focused cache and Bravo contract tests.
2. `gofmt` verification.
3. Full host `go test ./... -count=1`.
4. Host compile verification.
5. Bravo plugin `go test ./... -count=1`.
6. Bravo plugin `go vet ./...`.
7. Source diff check with no generated binaries, credentials, runtime caches,
   or temporary patch-delivery files.

## Canary Gates

Use a dedicated temporary project, key, and isolated canary container. Do not
reuse a production client key.

1. Send a stable prompt prefix large enough for the selected provider/model.
2. Complete the first request before sending the second request.
3. Repeat the same prefix within the provider TTL.
4. Anthropic pass criteria:
   - first request reports a cache write;
   - second request reports a cache read;
   - response content and streaming remain valid.
5. OpenAI/Codex pass criteria:
   - both requests use the expected stable cache key;
   - the second request reports cached tokens when the subscription endpoint
     exposes cache usage;
   - absence of provider cache telemetry is reported as unknown, not success.
6. Repeat the check for non-streaming, SSE, tools, deferred tools, reasoning,
   and a controlled pre-response failover.
7. Verify Bravo analytics and the Management Center in Chrome/Playwright.
   - Prompt Caching is collapsed by default in each project card;
   - changing `auto`/`5m`/`1h` persists immediately and remains after reload;
   - OpenAI/Codex is clearly marked as provider-managed rather than presenting
     a non-functional TTL control.
8. With a restricted project, verify a disallowed route returns the exact
   `403 bravo_model_forbidden` code over OpenAI HTTP, Anthropic HTTP, SSE, and
   Responses WebSocket. An allowed route with an empty pool must instead
   return `503 bravo_no_eligible_account`.
9. Remove the temporary project/key and verify production remained untouched.

## Release Gates

1. Build the canary image from a clean commit and embed the exact backend and
   UI commit identifiers.
2. Publish the reviewed branch and open a focused pull request only after all
   local and canary gates pass.
3. Replace the production container once, with a retained rollback image and
   a pre-cutover configuration/data backup.
4. Verify health, model discovery, one Anthropic request, one OpenAI request,
   project analytics, and existing client traffic.
5. Remove only temporary containers, build cache, and superseded intermediate
   images. Keep the rollback image and persistent volumes.
