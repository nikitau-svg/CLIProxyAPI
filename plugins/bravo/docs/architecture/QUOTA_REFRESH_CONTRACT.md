# Bravo quota refresh contract

Status: proposed implementation contract
Scope: host quota acquisition, Bravo quota cache, allocator eligibility, and management presentation
Out of scope: model-request cooldown classification, billing reconciliation, and provider credential refresh

## 1. Problem statement

Quota discovery is advisory control-plane traffic. It must never behave like a
model request and it must not turn a temporary failure of a provider's quota
endpoint into a false statement that a subscription is exhausted.

The current implementation replaces a previously confirmed snapshot with
`confidence=error` when a refresh times out or fails. A bulk refresh can also
start several provider calls at once. Together these behaviours can remove a
healthy credential from the secondary pool merely because quota discovery was
rate-limited.

This contract replaces that behaviour with stale-while-revalidate (SWR):

- the last provider-confirmed usage windows are immutable until a newer valid
  usage response replaces them;
- refresh health is recorded separately from quota confidence;
- request routing never waits for quota HTTP calls;
- provider quota endpoints are protected by provider-wide scheduling,
  `Retry-After`, backoff, and deterministic staggering;
- profile metadata and usage windows have independent freshness and failure
  state.

## 2. Non-negotiable invariants

1. A failed refresh MUST NOT overwrite, zero, or relabel a last-known-good
   (LKG) confirmed quota snapshot.
2. A quota-endpoint `429`, timeout, `5xx`, DNS error, or malformed response MUST
   NOT create a model-execution cooldown and MUST NOT mark the credential as
   provider-exhausted.
3. The allocator MUST NOT perform provider quota I/O while selecting an auth.
   It reads cache state and schedules refresh work only.
4. Only a fully validated usage response may advance `confirmed_at` or replace
   confirmed windows. Partial or malformed windows are never merged into LKG.
5. A profile failure MUST NOT alter usage windows, usage freshness, tariff
   floors, or routing eligibility.
6. A forced management refresh bypasses freshness only. It MUST still join
   singleflight work, obey provider concurrency limits, and obey a live
   `Retry-After` deadline.
7. No secret, upstream body, filesystem path, access token, or raw provider
   response may enter persisted quota state or a management response.
8. Runtime model-request evidence remains authoritative. A reviewed execution
   cooldown or disabled core auth blocks routing even when LKG says quota is
   available.

## 3. Terms and state model

### 3.1 Usage snapshot

`usage_snapshot` is the last fully validated set of session, weekly, and
model-weekly windows returned by the provider through the host callback.

It has these independent properties:

| Property | Meaning |
| --- | --- |
| `confidence` | `confirmed` only when a complete LKG exists; otherwise `unknown` |
| `confirmed_at` | provider observation time of the LKG usage response |
| `freshness` | derived at read time: `fresh`, `stale`, or `expired` |
| `dirty` | local model usage occurred after `confirmed_at`; requests an early background refresh but does not invalidate LKG |

`error` is not a valid quota confidence. Refresh failure and quota confidence
are different axes. Legacy `confidence=error` is accepted only while migrating
old state.

### 3.2 Refresh state

Usage and profile each own a `refresh_state`:

```text
refresh_state:
  last_attempt_at
  last_success_at
  last_failure_at
  consecutive_failures
  next_attempt_at
  error:
    code
    safe_message
    status_code
    retryable
    retry_after
```

`in_flight` is runtime-only and MUST NOT be persisted. `next_attempt_at` is the
maximum of the provider `Retry-After` deadline and locally calculated backoff.
All timestamps are UTC.

The error code is stable and machine-readable. At minimum the implementation
must distinguish:

- `rate_limited` (`429`);
- `timeout`;
- `transport_unavailable`;
- `provider_unavailable` (`5xx`);
- `auth_stale` (`401`/reviewed equivalent);
- `forbidden` (`403`);
- `response_invalid`;
- `windows_missing`;
- `provider_not_supported`.

Safe messages are localized at the presentation boundary. Routing decisions
use codes and fields, never message substrings.

### 3.3 Profile snapshot

Workspace label, account label, and plan label form a separate profile
snapshot with `profile_confirmed_at` and its own refresh state. A successful
usage response can be committed even if profile refresh fails. Existing safe
labels remain visible until replaced by a newer valid profile; local auth-file
labels are the final fallback.

## 4. Freshness contract

The following configuration is required:

```yaml
quota_usage_refresh_seconds: 60
quota_usage_max_stale_seconds: 900
quota_profile_refresh_seconds: 21600
quota_refresh_jitter_percent: 20
quota_refresh_provider_min_interval_ms: 250
quota_refresh_provider_concurrency: 1
```

Compatibility rule: when `quota_usage_refresh_seconds` is absent, the existing
`quota_refresh_seconds` value supplies it. The legacy field remains readable
for at least one release and is not rewritten unless configuration is saved.

Freshness is derived as follows:

- `fresh`: `now - confirmed_at < usage TTL`, LKG may be used;
- `stale`: usage TTL elapsed but max-stale has not elapsed, LKG may be used and
  background refresh is due;
- `expired`: no LKG, max-stale elapsed, or a scheduled quota window reset has
  passed without a newer confirmation; percentages remain display-only and
  may not authorize a secondary under the default policy.

`dirty=true` makes refresh due immediately, subject to `next_attempt_at` and
provider scheduling. It does not change `confirmed_at`, `confidence`, or
freshness by itself. Pending/in-flight reservations continue to be subtracted
from LKG during allocation.

An administrator may configure max-stale, but it MUST be at least one usage TTL
and SHOULD default to 15 minutes. Unlimited stale routing is forbidden.

## 5. Read and refresh algorithm

### 5.1 Request path

For every candidate auth, allocation performs these steps without blocking:

1. Read one immutable quota-cache snapshot.
2. Derive freshness from the injected clock.
3. If refresh is due and `now >= next_attempt_at`, enqueue a refresh job.
4. Continue allocation using the snapshot and the rules in section 9.

The request path MUST NOT wait for the queue, a singleflight result, provider
limiter, host callback, or state-file write. Initial quota discovery therefore
does not add quota-endpoint latency to inference.

### 5.2 Management path

`POST /v0/management/bravo/quotas/refresh` schedules work and returns a job
identifier plus per-auth state (`queued`, `joined`, `deferred`, or `skipped`).
It SHOULD return `202 Accepted`; the UI polls quota state. It must not hold one
HTTP request open while every subscription refreshes.

`force=true` means "refresh even when fresh". It does not mean "ignore
Retry-After" or "start duplicate work". An explicit future emergency override,
if implemented, must be separately named, admin-only, auditable, and absent
from normal UI.

### 5.3 Successful usage refresh

Commit is atomic under the state lock:

1. validate every required window and observation timestamp;
2. replace all usage windows as one snapshot;
3. set `confidence=confirmed`, `confirmed_at=ObservedAt`, and `dirty=false`;
4. clear usage refresh error, failure count, and `next_attempt_at`;
5. clear only the pending reservation amount captured before this refresh
   began; reservations created after the request started remain pending;
6. schedule an atomic state-file save.

A response observed before the current `confirmed_at` is ignored as stale. An
older singleflight completion must never overwrite a newer snapshot.

### 5.4 Failed usage refresh

On failure:

1. keep confidence, confirmed windows, `confirmed_at`, `dirty`, plan, and safe
   labels unchanged;
2. update only usage `refresh_state`;
3. calculate `next_attempt_at` as specified in section 8;
4. persist the refresh state;
5. keep LKG eligible only according to its derived freshness.

If no LKG exists, quota remains `unknown`. The failure is shown separately; it
does not invent zero percent remaining.

### 5.5 Profile refresh

Profile is refreshed only when its independent TTL expires. Usage and profile
calls need not run concurrently. A profile failure keeps prior labels and
updates only profile refresh state. A profile success never changes usage
`confirmed_at`.

The host/plugin ABI must support independent acquisition scopes (`usage`,
`profile`) or implement an equivalent host-side profile cache. An empty legacy
scope means `usage+profile` for backward compatibility. New Bravo code should
request `usage` on the short schedule and `profile` on the long schedule.

## 6. Singleflight and locking

Refresh work is keyed by `(auth_index, resource)`, where resource is `usage` or
`profile`.

- At most one job for a key may perform host I/O.
- Automatic and forced callers join the same job.
- Joining a job does not reset backoff or create a second provider request.
- Cancelling a management waiter does not cancel shared refresh work; the job
  has its own bounded context.
- No state-store mutex or allocator mutex may be held during queue wait, rate
  limiter wait, host callback, JSON decoding, or provider HTTP I/O.
- Commit acquires only the state-store mutex and rechecks generation/
  `confirmed_at` before replacing state.
- The in-memory singleflight registry deletes completed entries even after
  panic, cancellation, or timeout.

The race detector is part of the acceptance gate.

## 7. Provider scheduling, staggering, and load limits

Quota discovery uses a dedicated limiter and MUST NOT share capacity with model
execution.

The default is one in-flight quota HTTP request per provider and at least 250 ms
between starts for that provider. The limiter is process-wide, not per project
or per credential. `usage` and `profile` consume the same provider budget.

Scheduled due time is deterministically staggered:

```text
due = confirmed_at + ttl + hash(auth_index, resource, ttl_bucket) % jitter_window
jitter_window = ttl * quota_refresh_jitter_percent / 100
```

The hash must not expose `auth_index` and should remain stable within a TTL
bucket. Stagger applies after startup and after a bulk refresh. Cold startup
with no LKG distributes initial work across a bounded bootstrap window instead
of launching every account immediately.

The queue is bounded and deduplicated. When full, refresh remains due and is
retried by the scheduler; routing is unaffected. There is no unbounded goroutine
per auth.

## 8. Retry-After and retry policy

The host quota error contract must preserve safe retry metadata:

```text
status_code
retryable
retry_after       # sanitized HTTP value, optional
retry_at          # parsed UTC deadline, optional
```

Both delta-seconds and HTTP-date forms of `Retry-After` are supported. Invalid
or negative values are ignored. Valid deadlines are clamped to a documented
upper safety bound (recommended: 24 hours), and the original sanitized value
may be displayed.

For retryable failures without a usable `Retry-After`, use full-jitter
exponential backoff per `(auth_index, resource)`:

```text
cap = min(5 seconds * 2^(consecutive_failures-1), 5 minutes)
delay = random(0, cap)
next_attempt_at = now + delay
```

For `429`, `next_attempt_at` is at least the provider deadline. Provider-wide
`Retry-After` applies to every quota refresh for that provider, preventing a
bulk operation from rate-limiting each credential in turn. It does not block
model execution.

Non-retryable acquisition failures use a longer recheck interval (recommended:
15 minutes) and remain visible. `auth_stale` from a quota endpoint alone does
not disable model execution; only the core auth-health contract or an actual
model request may do that.

## 9. Allocation rules

These rules apply after allowed-pool, subscription-enabled, core auth-health,
and execution cooldown gates.

| Credential role | Fresh LKG | Stale LKG | Expired/no LKG |
| --- | --- | --- | --- |
| Primary | enforce zero-percent exhaustion; may spend below tariff floors | same, using LKG plus pending reservations | routable by default; quota is unknown, with an observable warning |
| Secondary, policy `block` | enforce tariff floors and reservations | enforce the same floors against conservative LKG | blocked |
| Secondary, policy `allow` | enforce tariff floors and reservations | enforce the same floors against conservative LKG | routable with unknown-quota warning |

Primary means priority and permission to spend below configured reserve floors;
it is not permission to retry a provider-confirmed zero-percent window. If LKG
is expired, the allocator does not claim the primary has quota; it permits one
normal model attempt because quota discovery is not authoritative model health.
Any reviewed execution cooldown then blocks it normally.

For stale LKG, eligibility uses:

```text
effective_remaining = LKG remaining - in_flight - pending - reservation
```

It never assumes a reset increased quota. When a reset timestamp passes without
a confirmed refresh, the snapshot is expired, not reset to 100 percent.

A quota-refresh `429` must never appear as "provider rate limit reached" on the
subscription's model-health card. UI must present two independent facts, for
example:

```text
Quota: 76% remaining (last confirmed 3 minutes ago)
Refresh: rate-limited; next attempt at 12:41
```

## 10. Host/plugin ABI contract

The normalized host contract remains the only component allowed to call
provider quota endpoints with credentials. Bravo must not implement provider
HTTP or token handling.

The ABI extension must be additive:

- request acquisition scope: `usage`, `profile`, or legacy `all`;
- separate `usage_observed_at` and `profile_observed_at`;
- separate safe usage and profile errors;
- `status_code`, `retryable`, `retry_after`, and parsed `retry_at` on errors;
- partial success: confirmed usage windows may coexist with a profile error;
- unknown usage always returns no new windows; Bravo retains LKG itself.

Older plugins ignore new response fields. The host treats a missing scope as
legacy `all`. A newer Bravo must detect a host without scoped acquisition and
may call legacy `all`, but still commits usage and profile independently.

## 11. Persisted-state compatibility

Implementation should bump Bravo usage-state schema from v2 to v3 and retain
analytics and cooldown data unchanged.

Migration rules:

1. `confidence=confirmed` with valid windows becomes LKG;
   `confirmed_at=refreshed_at` and refresh state starts clean.
2. Legacy `confidence=error` with valid windows is evidence of the current
   overwrite-on-failure bug. Preserve the windows for display, schedule an
   immediate refresh, but mark freshness expired until a new confirmation;
   this avoids falsely dating old data with the failure timestamp.
3. `unknown` or invalid/missing windows remains unknown.
4. Existing `plan`, account/workspace labels, `dirty`, model windows, pending
   allocator state semantics, analytics, and persisted cooldowns are retained.
5. Legacy `status` and `refreshed_at` remain readable during migration. New
   writes may retain them as compatibility mirrors but routing uses the new
   fields exclusively.
6. Migration is idempotent. State is written atomically only after successful
   decode and migration; an unreadable/future schema is not overwritten.
7. Before first v3 write, keep the existing crash-safe temporary-file/rename
   behaviour and preserve a recoverable v2 copy according to the existing
   deployment backup policy.

Rollback note: a v2 binary cannot understand schema v3. Canary and production
deployment must therefore either provide an explicit v3-to-v2 rollback tool or
retain the pre-migration v2 state file and restore it during binary rollback.
Never solve rollback by deleting usage state.

## 12. Observability contract

Every refresh attempt emits one structured event without credentials:

- trace/job ID;
- auth index hash and safe account note/label;
- provider and resource;
- trigger (`scheduled`, `dirty`, `management_force`, `cold_start`);
- queued, started, and completed timestamps;
- outcome (`confirmed`, `joined`, `deferred`, `failed`);
- safe error code/status and `next_attempt_at`;
- whether LKG was retained and its age;
- provider limiter wait duration.

Metrics:

- refresh attempts/successes/failures by provider, resource, and code;
- LKG fresh/stale/expired credential counts;
- provider limiter queue depth and wait time;
- singleflight joins;
- refresh deferrals due to Retry-After/backoff;
- routing decisions using stale or unknown quota.

No metric or log label may contain email, workspace name, full auth index,
access token, provider body, or project API key.

## 13. Test plan

All time, jitter, randomness, host callbacks, and scheduling must be injectable.
Tests must not use wall-clock sleeps.

### 13.1 Unit tests

| ID | Scenario | Required assertion |
| --- | --- | --- |
| Q-U01 | Fresh confirmed snapshot | no refresh enqueued; LKG returned unchanged |
| Q-U02 | Stale confirmed snapshot | return immediately; exactly one background job enqueued |
| Q-U03 | Expired snapshot | secondary follows unknown policy; primary remains attemptable |
| Q-U04 | No snapshot | unknown returned immediately; cold-start job enqueued |
| Q-U05 | Refresh returns 429 | windows, confidence, confirmed time, labels, dirty flag unchanged; refresh error and deadline stored |
| Q-U06 | Timeout/transport/5xx | same LKG retention; exponential full-jitter backoff calculated |
| Q-U07 | Malformed or partial windows | response rejected atomically; no partial merge |
| Q-U08 | Successful newer refresh | all windows replaced atomically; error/backoff cleared; captured pending reservation cleared |
| Q-U09 | Older completion races newer one | older observation ignored |
| Q-U10 | Profile failure with usage success | usage commits; old labels remain; only profile error changes |
| Q-U11 | Profile success with usage failure | labels commit; LKG usage remains unchanged |
| Q-U12 | Dirty LKG | early refresh due, but confidence and eligibility preserved |
| Q-U13 | Window reset passes while stale | freshness becomes expired; never assumes 100% |
| Q-U14 | Retry-After delta seconds | exact minimum deadline respected |
| Q-U15 | Retry-After HTTP date | UTC deadline parsed and respected |
| Q-U16 | Invalid/negative Retry-After | ignored; local backoff used |
| Q-U17 | Provider-wide Retry-After | all same-provider quota jobs deferred; other provider continues |
| Q-U18 | Automatic plus forced refresh | one host call; force joins existing singleflight |
| Q-U19 | Cancelled waiter | shared job completes and state commits |
| Q-U20 | Queue full | no goroutine leak; routing continues; refresh remains due |
| Q-U21 | Secondary `block` matrix | fresh/stale eligible by floors, expired/unknown blocked |
| Q-U22 | Secondary `allow` matrix | expired/unknown allowed with warning; confirmed floors still enforced |
| Q-U23 | Primary matrix | floors ignored, confirmed zero blocked, expired/unknown permits normal attempt |
| Q-U24 | Execution cooldown plus fresh quota | cooldown wins |
| Q-U25 | Quota-refresh 429 | no execution cooldown or auth-health mutation |
| Q-U26 | Deterministic jitter | same bucket stable; accounts distributed within configured window |
| Q-U27 | State migration v2 confirmed | valid LKG preserved and immediately usable |
| Q-U28 | State migration v2 error-with-windows | windows display-only, immediate refresh due, no secondary authorization |
| Q-U29 | Migration idempotence | repeated migration produces byte-equivalent semantic state |
| Q-U30 | Secret safety | errors/events/state contain no token, raw body, path, email, or API key |

Run unit packages with the race detector for the refresh store, queue,
singleflight, and allocator tests.

### 13.2 Host/plugin integration tests

| ID | Scenario | Required assertion |
| --- | --- | --- |
| Q-I01 | Scoped usage call | only usage endpoint called; validated windows returned |
| Q-I02 | Scoped profile call | only profile endpoint called; no usage confidence mutation |
| Q-I03 | Legacy empty scope | backward-compatible `all` behaviour |
| Q-I04 | Usage 200, profile 429 | partial success preserves usage and carries typed profile retry metadata |
| Q-I05 | Usage 429 with Retry-After | no windows returned; typed status/retry deadline crosses ABI |
| Q-I06 | Usage timeout | bounded host context; typed timeout crosses ABI |
| Q-I07 | Twenty auths become due together | per-provider concurrency and minimum start interval never exceeded |
| Q-I08 | Claude provider deferred | Codex quota refresh proceeds independently |
| Q-I09 | Request allocation during slow refresh | inference selection latency does not include host quota latency |
| Q-I10 | Concurrent management and traffic refresh | one upstream call per auth/resource |
| Q-I11 | Restart with stale LKG and backoff | state restored; no call before `next_attempt_at` |
| Q-I12 | v2 production fixture migration | analytics/cooldowns/labels/windows retained; schema becomes v3 atomically |
| Q-I13 | ABI downgrade fixture | new Bravo works with legacy host without corrupting LKG |
| Q-I14 | Auth removed during queued refresh | result discarded safely; no orphan state or panic |

### 13.3 Canary tests

Canary runs against an isolated container, copied non-secret configuration, an
isolated state path, and a separate port. Production container is not restarted
or reconfigured during this phase.

1. Seed a confirmed quota fixture, then make usage refresh return `429` with
   `Retry-After`. Verify UI keeps percentages and reset times, displays a
   separate Russian refresh warning, and secondary routing still follows the
   stale-LKG floor policy.
2. Hold a usage response for longer than normal inference selection. Send a
   model request and verify allocator latency is unaffected and the request is
   served.
3. Make twenty credentials due at once. Verify provider request timestamps
   satisfy configured concurrency/minimum interval and that no burst of usage
   plus profile calls occurs.
4. Return usage success and profile failure. Verify quota becomes confirmed,
   labels remain stable, and no subscription is removed from the pool.
5. Restart canary during Retry-After. Verify LKG, refresh error, and deadline
   survive restart and no early provider call occurs.
6. Exercise primary at below-floor but non-zero confirmed quota: it is selected.
   Exercise the same auth as secondary: it is protected by the floor.
7. Exercise confirmed zero primary: it is skipped. Then use expired/unknown
   primary: one normal model attempt is allowed and any real provider rejection
   creates only the reviewed execution cooldown.
8. Open quota management while model traffic is active. Verify no quota refresh
   failure changes the core auth status or the model-health error card.
9. Inspect persisted state, structured events, HTTP management responses, and
   container logs with automated secret scanners.
10. Roll canary back using the retained v2 snapshot and prove rollback does not
    require deleting analytics or cooldown history.

### 13.4 Acceptance gates

The quota refresh build cannot advance from canary unless all are true:

- all unit and integration tests pass, including `-race` suites;
- a refresh `429`/timeout demonstrably retains LKG and never creates an
  execution cooldown;
- inference p95 selection latency has no dependency on quota endpoint latency;
- measured quota HTTP concurrency and cadence stay within configured limits;
- restart and rollback tests preserve state;
- UI distinguishes quota data age from refresh failure in Russian;
- secret scanners find no credential or raw provider payload in state, logs,
  events, or management responses;
- no production container restart is performed before explicit deployment
  approval.

## 14. Required implementation order

1. Extend the typed host/plugin ABI and its backward-compatibility tests.
2. Introduce v3 quota and refresh state plus migration fixtures.
3. Implement scoped host acquisition and typed retry metadata.
4. Implement the bounded scheduler, provider limiter, deterministic stagger,
   and singleflight.
5. Change allocator reads to non-blocking SWR and apply the eligibility matrix.
6. Split usage/profile presentation and localize safe refresh errors.
7. Add structured metrics/events.
8. Run unit/integration tests, build an isolated canary, execute the canary
   matrix, review evidence, then merge with the other architecture work.
