# Bravo observability and safe logging contract

Status: implemented contract for Bravo 0.8.0.

## Goals

For every Bravo request an operator must be able to answer, without reading the
prompt or provider credentials:

1. Which project and logical model received the request?
2. Which physical provider, model and subscription were considered?
3. Which attempts were skipped, started, superseded, succeeded or failed?
4. How was each failure classified and why did routing continue or stop?
5. Was any response content committed before the failure?
6. What should the operator or client do next?

The default diagnostic path must never persist prompt bodies, tool arguments,
raw provider responses, API keys, OAuth tokens, cookies or authorization header
fragments.

## Stable identifiers

One `route_trace_id` is created when a Bravo request is authenticated and stays
stable across every candidate and subscription attempt. When the host already
has a request ID or CPA trace ID, the record stores its one-way SHA-256 digest,
not the original identifier.

Every recorded attempt receives a monotonically increasing `ordinal` scoped to
the route trace. Parallel hedge completion and supersession are serialized by
the route coordinator before persistence.

## Persisted route trace schema

```json
{
  "schema_version": 1,
  "trace_id": "random-opaque-id",
  "started_at": "RFC3339Nano UTC",
  "completed_at": "RFC3339Nano UTC",
  "project_id": "stable Bravo project id",
  "logical_model": "bravo/opus",
  "source_protocol": "claude",
  "stream": true,
  "outcome": "success|failed|canceled",
  "final_code": "bravo_context_window_exceeded",
  "status": 400,
  "final_message": "safe localized summary",
  "client_action": "compact|new_session|retry|raise_quota|none",
  "attempts": []
}
```

Attempt records contain only:

- ordinal and timestamps;
- provider and physical model;
- pseudonymous subscription identity; the authenticated management response
  joins the current operator-authored note (or Workspace + Email) in memory,
  but the note and email are never persisted in the trace file;
- requested/effective effort;
- allocator decision and safe skip reason;
- status, stable error code, failure class and scope;
- retry/fallback decision and safe reason;
- `retry_after` or cooldown deadline;
- observed and supported context token counts when known;
- complete attempt duration, plus time to first byte/content when that clock is
  available from the host; missing clocks are `null`/omitted and the UI shows
  `—`, never a fabricated `0 ms`;
- whether user-visible content was committed.

Provider-authored prose is not persisted. Reviewed numeric fields and stable
machine codes may be persisted after sanitization.

## Retention and storage

- Route traces use an atomically replaced JSON snapshot beside Bravo's existing
  state file, never under the auth directory.
- Default retention is 30 days and 2,000 routes. This deliberately bounded v1
  store avoids an unbounded local log while keeping recent operational history.
- Persistence is debounced off the request path; the file is mode `0600`.
- Existing state files remain readable; this feature requires no destructive
  migration.

## Request and provider body logging

Default production mode writes the structured route trace and a bounded safe
HTTP summary. It does not write request or response bodies.

Full transport capture is a separate, time-limited diagnostic mode with all of
the following requirements:

- explicit operator opt-in and automatic expiry;
- body size limit per section and per request;
- complete replacement of Authorization, cookies, API keys and token-bearing
  headers with `[REDACTED]` (no first/last characters);
- recursive JSON redaction for known credential fields;
- a warning in the management UI that prompts and tool arguments may be stored;
- never forwarded to Home or any remote sink unless a second explicit opt-in
  is present.

The Home logging envelope must receive already-redacted headers. Raw request
headers must not be cloned into the outbound payload.

## Latency contract

The word `latency` alone is prohibited in the UI and public API. Metrics are:

- `queue_ms`: time before the physical attempt starts;
- `ttfb_ms`: start to first upstream byte;
- `ttfc_ms`: start to first user-visible content;
- `attempt_duration_ms`: complete physical attempt, including stream consume;
- `route_duration_ms`: complete Bravo route across retries and fallback;
- `fallback_overhead_ms`: route duration minus winning attempt duration.

Existing `latency_ms` remains as a compatibility alias for
`attempt_duration_ms`. The UI labels it `Полная длительность`, not
`Средняя задержка`.

Canceled hedges and superseded attempts are reported separately and never
counted as provider failures.

## Management API

`GET /v0/management/bravo/traces` returns up to 500 newest safe attempt chains.
The implemented v1 filters are `project_id`, `trace_id`, `errors_only`, and
`limit`. A single trace is retrieved with its `trace_id`; there is no raw-detail
endpoint because raw request/provider bodies are not retained.

Raw body fields do not exist in these responses. CSV export uses the same
redacted projection.

## Client-facing error contract

The final client error includes a safe localized message and stable code. It
may include the opaque `route_trace_id` so the operator can locate the route,
but never includes auth identifiers, provider request IDs or raw JSON.

After user-visible streaming content has been committed, fallback is forbidden.
The terminal trace must explicitly state that the route stopped because output
had already been committed.

## Test matrix

### Unit

- Every sensitive header family is completely redacted.
- Nested credential-looking JSON fields are redacted in diagnostic mode.
- Route and attempt ordinals remain stable for sequential and hedged execution.
- Context numbers survive while arbitrary provider prose is discarded.
- Latency fields use the documented clocks and exclude superseded attempts from
  success/failure aggregates.
- Retention enforces age and route-count bounds independently.

### Integration

- Claude quota failure followed by Codex success produces one route with two
  attempts and no prompt fragments.
- Context overflow with no compatible fallback records `compact` as the action.
- A failure after committed content records no second provider attempt.
- Restart preserves trace history and does not duplicate records.
- Home logging receives no raw Authorization value.

### Canary

- Send a synthetic prompt containing unique sentinel text and confirm the
  sentinel does not occur anywhere in the default logs or management response.
- Send a synthetic API key and confirm neither the key nor any prefix/suffix is
  persisted.
- Exercise a real fallback and verify project, subscription, provider, model,
  decision and timing are visible on one trace page.
- Compare route duration against an external monotonic timer; verify optional
  TTFB/TTFC render as `—` when the host has not supplied those clocks.
