# Bravo 0.8.5 safe diagnostics test plan

Status: implementation gate for the isolated 0.8.5 canary.

Scope: Bravo 0.8.2 plus the Claude OAuth custom-tool alias hotfix. Adaptive
quota routing from 0.8.4 is deliberately out of scope.

## Invariants

1. Every request-scoped `400` or `422`, including failures before account
   selection, receives an `X-Bravo-Trace-Id` and a durable route trace.
2. A preflight trace records a stable stage, error code, required capability,
   and safe JSON parameter path when known.
3. Provider invalid-request envelopes retain only reviewed structured fields:
   `type`, `code`, `param`, scope, and taxonomy class. Raw response JSON,
   request IDs, prompts, tool descriptions, schemas, credentials, headers, and
   OAuth material are never persisted.
4. A structured invalid-tool error cannot be reclassified as a context-window
   error because unrelated text contains the words “context window”.
5. Client-facing explanations and actions are in Russian and identify whether
   the failure happened during local contract preflight or at the provider.
6. Existing successful routing, fallback order, custom-tool aliasing, and
   0.8.2 quota behaviour remain byte-for-byte or semantically unchanged.

## Automated tests

- `SD-U01`: non-streaming contract detection failure creates a trace before
  provider selection and returns its trace ID in the response headers.
- `SD-U02`: logical-model capability rejection records stage, capability, and
  Russian action without a provider call.
- `SD-U03`: streaming startup preflight failure has the same trace contract.
- `SD-U04`: count-tokens preflight failure has the same trace contract.
- `SD-U05`: OpenAI/Codex `invalid_request_error` preserves safe `code` and
  `param` while discarding raw message and request ID.
- `SD-U06`: an invalid-tool response containing unrelated “context window”
  text remains `invalid_request`, not `context_window`.
- `SD-U07`: malformed or oversized provider JSON falls back to a generic safe
  diagnostic and never leaks its body.
- `SD-U08`: trace sanitization bounds identifiers/paths and rejects secrets,
  descriptions, schema fragments, and control characters.
- `SD-U09`: existing context-window, quota, authentication, stream, and
  custom-tool alias regression suites remain green.
- `SD-U10`: focused race tests cover simultaneous early failures and trace
  persistence.

## Canary acceptance

1. Run the existing Claude Code compatibility smoke against an isolated
   0.8.5 container.
2. Reproduce a local capability mismatch and verify `422`, trace header,
   Russian explanation, stage, capability, and zero provider calls.
3. Feed a synthetic Codex invalid-tool envelope through the canary host path;
   verify the stored trace contains the safe provider code/path and no raw
   schema or request ID.
4. Repeat the large-context case; it must still be classified as context
   overflow and recommend `/compact`.
5. Verify route trace retention, file permissions, container health, restart
   count, CPU/memory, and that production remains untouched.

Promotion to production requires a separate explicit approval after the
canary report.
