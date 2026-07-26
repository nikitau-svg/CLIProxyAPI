# Bravo 0.7.6 Subscription Identity and Timeline Test Plan

## Goal

Make every physical subscription easy to identify without changing routing,
quota, retry, or project-isolation behavior:

- show the operator-authored auth-file note as the primary bold name;
- if no note exists, show workspace and email as the primary name;
- use the same identity in the pool, project editor, project cards, analytics,
  and the core auth-file list;
- show which subscriptions served a project in each retained hour or day;
- describe average response time accurately as the complete provider-attempt
  duration, including streamed response consumption.

## Read-only production diagnosis

Before implementation, query the existing production Management API and auth
files without printing credentials or mutating state.

- Confirm Codex auth files expose their `note` metadata.
- Confirm Claude files without a note still expose workspace and email.
- Recompute `average_latency_ms` as `latency_ms / requests`.
- Confirm the current metric records the full provider-attempt duration and is
  not time to first token.
- Confirm the temporary audit script is removed from both machines.

## Source and contract gates

Backend:

- `note` and `display_name` are additive response fields.
- The legacy `label` remains a compatibility alias.
- Raw auth indexes are never returned from analytics.
- Presentation metadata is joined at response time and never persisted in the
  usage ledger.
- Timeline points use existing schema-v2 hourly/daily buckets, honor every
  analytics filter, and merge logical/physical model rows per subscription.
- Missing host metadata degrades to the existing redacted subscription label;
  analytics itself remains available.
- Existing project pools, retries, quota floors, and routing remain unchanged.

Management UI:

- Identity order is note, explicit display name, safe legacy note, workspace
  plus email, then provider plus shortened redacted ID.
- Provider email labels are not mistaken for operator notes.
- HTML entities from legacy provider metadata are decoded as plain text.
- Notes/primary identity are bold in every subscription surface.
- A 10,874 ms value renders as a localized `10.9 s`, with help text explaining
  full response duration for streaming.
- Timeline is newest-first, compact by default, and discloses older buckets on
  demand.
- Empty and pre-schema-v2 analytics remain usable.
- Desktop and 400 px layouts have no horizontal overflow.

## Automated verification

- Focused Bravo allocation and analytics tests.
- Full Bravo module test suite and race suite.
- Full repository Go tests and required server compile.
- Focused Bravo `go vet`; compare any full-repository vet findings with the
  unchanged base branch instead of attributing pre-existing core warnings to
  this release.
- Management UI unit tests for response normalization, identity fallback,
  response-time formatting, CSV, and presentation helpers.
- Management UI lint, TypeScript, production build, and `git diff --check`.

## Canary verification

Build one Linux/arm64 candidate from the exact backend and UI commits. Deploy
it only to the isolated canary port.

- Health check passes with zero restarts.
- Existing schema-v2 state loads without migration.
- Subscription endpoint returns note/display-name fallbacks.
- Analytics endpoint returns redacted, filtered timeline points.
- Existing management, allocator, routing, and prompt-cache smoke tests pass.
- Chrome/Playwright checks the production-like Russian UI at desktop and
  400 px: bold identities, readable fallback, response-time help, timeline
  disclosure, keyboard operation, and no overflow.

## Release and cleanup gates

- Open backend and UI pull requests only after the canary passes.
- Require green GitHub checks and review the final diff before merge.
- Rebuild from merged commits and recreate production once.
- Verify health, restart count, project/subscription counts, ordinary API keys,
  Bravo keys, analytics, and prompt caching.
- Retain the explicit rollback image and pre-cutover backup.
- Remove only the exact canary container, candidate archives, temporary audit
  scripts, and candidate-only Docker build cache. Do not perform a broad Docker
  or system prune.
