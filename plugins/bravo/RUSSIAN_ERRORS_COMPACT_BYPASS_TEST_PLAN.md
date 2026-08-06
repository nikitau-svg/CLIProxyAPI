# Russian errors and Claude Code compact bypass test plan

## Goal

Make every client-visible Bravo failure actionable in Russian while preserving
the stable machine-readable error code, HTTP status, retry metadata, and safe
provider diagnostics. When Claude subscriptions are healthy but withheld only
by Bravo's internal reserve floors, a real Claude Code `/compact` request may
use one Claude subscription through a rate-limited bypass instead of falling
through to a smaller-context Codex model.

## Safety contract

- The bypass applies only to the Claude protocol, the Claude CLI user agent,
  a non-empty Claude session ID, and the current Claude Code compaction prompt.
- Historical `/compact` text in an ordinary conversation must not trigger it.
- Authorization boundaries, project `allowed_auth_ids`, disabled subscriptions,
  provider/Core cooldowns, authentication failures, and provider quota failures
  are never bypassed.
- Only allocator reserve floors may be bypassed, and only for a Claude candidate
  that still has confirmed positive provider quota.
- A successful bypass starts a per-project/per-session cooldown. Concurrent
  compactions for the same key cannot spend the reserve twice.
- A bypass is visible through a warning header/metadata where the response shape
  supports them and through a redacted Russian warning in the service log.
- Streaming and non-streaming failures use the same Russian localization path.
- Project, route, tariff, subscription-pool, and prompt-cache management errors
  also pass through the Russian localization boundary.
- Raw provider JSON, request IDs, credentials, and payment details never appear
  in the client message or service warning.

## Required tests

1. **Allocator floor followed by smaller context window**
   - Fable is healthy but below an internal secondary floor.
   - Sol remains eligible and returns a context-window failure.
   - The final Russian message says that Claude was withheld by CLIProxyAPI's
     internal reserve, that the request moved to Sol, that Sol could not fit the
     conversation, and suggests changing the internal Claude limits, `/compact`,
     or a new session.

2. **Russian client error catalog**
   - Known Bravo codes for authentication, authorization, provider quota,
     model credits, model access, context size, temporary route exhaustion,
     request validation, and local bridge failure produce Russian messages.
   - Unknown codes produce a safe Russian generic message while retaining the
     original machine-readable code.
   - Non-streaming envelopes and stream-close errors use identical localization.
   - Management errors for projects, routes, tariffs, subscription pools, and
     prompt caching are understandable in Russian and retain their codes.

3. **Real compact detection**
   - A real Claude CLI compaction prompt with the expected headers is detected.
   - Missing CLI user agent/session ID, a different protocol, an ordinary user
     request, and historical `<command-name>/compact</command-name>` text are not.

4. **Compact reserve bypass**
   - A normal request below the floor routes to Codex.
   - The matching compact request first receives the healthy Claude candidate.
   - Project pool restrictions and provider/Core cooldowns still fail closed.
   - Unknown or zero provider quota never receives a bypass.

5. **Cooldown and concurrency**
   - The first compact bypass lease succeeds.
   - A concurrent or immediate second lease is rejected with a Russian cooldown
     reason and retry hint.
   - Releasing an uncommitted local failure restores eligibility.
   - A committed attempt becomes eligible again after the configured cooldown.

6. **Observability**
   - A successful non-streaming bypass carries a safe warning header and result
     metadata.
   - A successful streaming bypass records one redacted Russian warning.
   - Attempt analytics mark the compact bypass without storing prompt content.

## Verification order

1. Focused Bravo tests for localization, planning, compact detection, cooldown,
   non-streaming execution, and streaming execution.
2. Existing Bravo fallback, quota, context, retry, and contract suites.
3. Full `(cd plugins/bravo/go && go test ./...)`.
4. Required server compile check from `AGENTS.md`.

No Docker image, container restart, canary deployment, production deployment,
Git commit, or GitHub push is part of this candidate-preparation phase.
