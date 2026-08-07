# Bravo quota polling test plan

Status: implementation contract for the quota scheduler introduced after 0.8.0.

## Operator contract

- Provider quota discovery never runs in the inference hot path and never blocks a model request.
- Reading the Bravo administration pages is cache-only and does not contact a provider.
- Usage quota polling defaults to 15 minutes (`900` seconds).
- The Bravo administration page can change the usage polling interval without editing YAML.
- Accepted values are 5 minutes through 24 hours. Values below 10 minutes are allowed but must carry a high-frequency warning in the UI.
- Profile metadata remains a separate, slower resource with a six-hour default TTL.
- Last-known-good quota data survives transient provider failures. A failed poll must not erase a confirmed snapshot.
- Provider `Retry-After` and per-account backoff are authoritative. A manual refresh must not bypass an active provider cooldown.
- Reconfiguration replaces the current schedule and must not create duplicate polling workers.
- Plugin shutdown stops the polling worker before persistent state is flushed.
- The administration page shows, for usage and profile independently:
  - total provider request attempts;
  - successful and failed attempts;
  - last attempt and next permitted attempt.
- Counters are persistent, additive fields in the existing state format. Upgrading old state starts missing counters at zero.
- No counter, event, or API response may expose OAuth tokens, API keys, proxy credentials, or raw auth-file contents.

## Automated tests

1. Defaults and validation
   - a config without polling fields normalizes to 900 seconds;
   - an explicit interval inside the accepted range is preserved;
   - intervals outside 300..86400 seconds are rejected;
   - stale authorization age is never shorter than the configured polling interval.
2. Scheduling policy
   - an unobserved account is due;
   - a fresh account is not due before its interval plus deterministic jitter;
   - inference may mark a confirmed quota dirty, but dirty state cannot bypass the configured provider polling interval;
   - usage and profile schedules are independent;
   - `NextAttemptAt` suppresses a scheduled and a manual refresh;
   - repeated wakeups do not duplicate an in-flight `(auth, resource)` request.
3. Hot-path isolation
   - building an execution plan performs zero quota provider callbacks;
   - allocating candidates reads only the cached snapshot;
   - listing subscriptions performs zero quota provider callbacks.
4. Accounting and persistence
   - exactly one attempt is counted when a provider request starts;
   - success and failure counters are mutually exclusive and increment once;
   - a 429 retains last-known-good quota and records its retry time;
   - counters survive a state flush/reload;
   - old state without counters loads as zero.
5. Management API
   - GET settings returns effective interval and aggregate counters;
   - PATCH settings persists a valid interval and immediately reconfigures the scheduler;
   - invalid/unknown fields fail closed without changing runtime or YAML;
   - manual refresh returns the selected account IDs and does not wait for provider I/O;
   - accounts under an active per-account or shared-egress cooldown add no provider request count.

## Canary acceptance

1. Start an isolated canary with copied, redacted configuration and a separate state path.
2. Confirm inference succeeds while provider quota callbacks are delayed or return 429.
3. Generate at least 100 inference requests inside one polling interval and verify they add zero provider quota requests.
4. Verify one due polling cycle makes at most one usage request per eligible account and respects provider pacing.
5. Change the interval in the Bravo UI, reload the page, and confirm it persisted and the next due time moved accordingly.
6. Verify request counters, success/failure counts, and timestamps in Chrome through Playwright at desktop and narrow widths.
7. Run the plugin unit suite, repository tests, build, and race-sensitive quota tests before publishing.

Production is updated only after every canary acceptance item passes. Rollback is the previous plugin binary/config pair; quota state remains backward compatible.
