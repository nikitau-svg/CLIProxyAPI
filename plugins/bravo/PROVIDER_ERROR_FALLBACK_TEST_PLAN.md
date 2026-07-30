# Bravo Provider Error Fallback Test Plan

## Incident

A Claude Code request to `bravo/opus` received a structured provider error
inside an HTTP 200 SSE stream before any model content. Core reduced the event
to a terminal `provider_stream_error`/502. Bravo therefore stopped after one
physical call, while Claude Code treated the outward 502 as transient and sent
the same request ten more times. A later Codex `response.failed` diagnostic was
also reduced to a generic `model_execution_failed`.

The fix must keep raw provider JSON, request IDs, credentials, payment fields,
and arbitrary provider messages private while preserving enough reviewed
machine-readable information to route and diagnose the failure.

## Required deterministic tests

1. Safe provider parser
   - Recognize the documented standard provider types used by Claude and Codex:
     request validation, authentication, billing, permission, not found,
     conflict, request too large, rate limit, usage limit, API/server error,
     timeout, and overload.
   - Return a bounded generic message, stable status, retryability, and scope.
   - Preserve the existing special `credits_required` parser unchanged.
   - Reject malformed envelopes, arbitrary types/codes, internal Bravo code
     injection, oversized payloads, request IDs, credentials, cookies, and
     payment details.

2. Claude executor
   - For every supported client protocol, an SSE `overloaded_error`,
     `api_error`, `rate_limit_error`, or `billing_error` before content exposes
     a safe typed error and remains eligible for candidate fallback.
   - Context overflow and request-too-large remain terminal and request-scoped.
   - A future unrecognized structured SSE error remains redacted and
     fail-closed until its routing scope has been reviewed.
   - No raw provider JSON or private message crosses the executor boundary.

3. Codex executor and host bridge
   - `type:error` and `response.failed` preserve safe `server_error`,
     `rate_limit_error`, and context classifications.
   - The SDK and plugin-host bridges carry status, retryability, safe provider
     detail, and Retry-After without carrying the raw response.
   - HTTP-SSE and WebSocket paths share the same behavior.

4. Bravo route execution
   - Claude prelude + standard transient error performs Claude-to-Codex fallback
     inside one client request for Claude Messages, OpenAI Chat, and OpenAI
     Responses protocols.
   - A redacted unknown structured error remains terminal and creates no
     cooldown.
   - The failed provider prelude is never emitted.
   - An error after visible content never splices a second provider stream, but
     still updates failure accounting and cooldown.
   - Context and invalid-request failures stay fail-closed.
   - Attempt analytics retain only safe provider type/code and the ordered
     physical path.

5. Exhaustion semantics
   - If every candidate fails transiently, the final response contains the
     ordered safe route summary and a bounded Retry-After.
   - A partial plan records why configured candidates were excluded, without
     auth IDs or credential data.

## Local gates

- New focused tests fail on the unpatched `bravo/stable` baseline.
- Focused executor, provider-error, plugin-host, and Bravo tests pass.
- `go test ./sdk/cliproxy/providererror ./internal/runtime/executor
  ./internal/pluginhost ./plugins/bravo/go`
- Race-enabled tests for the touched packages pass.
- Required repository compile check succeeds.
- The source tree contains no fixture request IDs, real auth labels, tokens, or
  production paths.

## Canary gates

- Build from the exact candidate commit.
- Use isolated ports, config, plugin directory, auth fixtures, logs, and
  Bravo state; never mount production auth, config, logs, or Bravo data.
- A deterministic fake provider reproduces HTTP 200 + SSE error before content.
- One downstream request produces an internal ordered fallback and successful
  client response; no outward generic 502 and no client retry loop.
- Context, post-content, privacy, health, identity, mount, restart, and log
  gates remain green.
- Optional live probes use a dedicated canary key and do not mutate production.

## Release gates

- Publish only the reviewed candidate commit to GitHub.
- Merge into `bravo/stable` only after the canary evidence is recorded.
- Perform one production container replacement with the existing config, auth,
  logs, plugin, and Bravo-data mounts.
- Verify health, version/commit identity, model listing, a normal request, a
  controlled fallback request, restart/OOM counters, and fresh logs.
- Remove only the candidate-specific temporary image, container, build cache,
  fixture auth, config, logs, and state after the production verification.
