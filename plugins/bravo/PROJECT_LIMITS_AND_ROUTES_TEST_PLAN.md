# Bravo 0.8.11 project limits and routes test plan

## Contract

- A valid Bravo project key can read `GET /v1/bravo/limits` as JSON or Russian
  terminal text and `GET /v1/bravo/routes` as JSON.
- The limits response contains only locally persisted, provider-confirmed quota
  windows and project-local 30-day analytics. Reading it performs no provider
  I/O.
- A project receives at most one fresh limits response per hour. Repeated calls
  return typed `429`, `Retry-After`, and `next_allowed_at`.
- Route output is limited by the project model allowlist and contains the
  effective preferred/fallback candidate order after overrides. It never
  contains credential identities.
- When a confirmed Claude limit forces fallback and the compatible Codex model
  cannot fit the context, the client-visible Russian error includes the known
  model/general reset times, Codex availability, context requirement/window,
  and `/compact` action. Unknown or stale reset data is never invented.

## Automated gates

1. Confirm Claude session/model/week reset aggregation and available Codex.
2. Confirm 30-day usage summary, daily series, provider and model breakdown.
3. Confirm account names, emails, auth indices, outside-pool credentials, raw
   provider errors, and key material are absent.
4. Confirm one-hour rate limiting and exact `Retry-After`.
5. Confirm model allowlist and effective override order in project routes.
6. Confirm both public routes use ordinary API authentication and fail closed
   when Bravo is unavailable.
7. Confirm create and rotate return one-time endpoint documentation without
   persisting plaintext project keys.
8. Run the Bravo full suite, race suite, core API tests, UI tests/typecheck/
   production build, and `git diff --check`.

## Isolated canary

Use a disposable project key and copied state. Verify JSON/text output, repeat
rate limiting, and an intentionally reproduced Claude-limit to Codex-context
failure. Provider request counters must not change while reading either project
endpoint. Production is promoted only after the canary remains private,
project-scoped, and provider-I/O-free.
