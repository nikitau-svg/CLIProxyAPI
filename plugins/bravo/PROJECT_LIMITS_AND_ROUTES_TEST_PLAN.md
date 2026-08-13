# Bravo 0.8.11 project limits and routes test plan

## Contract

- A valid Bravo project key can read `GET /v1/bravo/limits` as JSON or Russian
  terminal text and `GET /v1/bravo/routes` as JSON.
- The limits response contains only locally persisted, provider-confirmed quota
  windows and project-local 30-day analytics. Reading it performs no provider
  I/O.
- A project receives a fresh local limits response at most once every five
  minutes. Repeated calls return the cached response with HTTP 200 and perform
  no provider I/O.
- The limits response expresses project consumption in subscription-quota
  percentage points, separately for session, weekly and model-weekly windows.
  It includes average/peak pace, physical/logical model, effort and tariff
  dimensions, plus confidence-gated subscription-capacity advice.
- Management analytics may rank projects by attributed share of each independent
  pool window. Provider drop not safely attributable to a local project remains
  `external_or_estimator_gap`.
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
4. Confirm five-minute cached HTTP 200 behavior and zero provider calls on both
   cache miss and hit.
5. Confirm model allowlist and effective override order in project routes.
6. Confirm both public routes use ordinary API authentication and fail closed
   when Bravo is unavailable.
7. Confirm create and rotate return one-time endpoint documentation without
   persisting plaintext project keys.
8. Run the Bravo full suite, race suite, core API tests, UI tests/typecheck/
   production build, and `git diff --check`.
9. Reconcile deterministic provider drops against two projects, an anonymous
   local commitment and external residual; confirm windows remain independent.
10. Confirm reset/increase intervals are skipped, project rankings and model /
    effort shares are stable, raw credential identities never leave management
    state, and persisted aggregates survive restart.

## Isolated canary

Use a disposable project key and copied state. Verify JSON/text output, repeat
rate limiting, and an intentionally reproduced Claude-limit to Codex-context
failure. Provider request counters must not change while reading either project
endpoint. Production is promoted only after the canary remains private,
project-scoped, and provider-I/O-free.
