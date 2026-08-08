# Bravo adaptive quota control contract

Status: proposed, contract-first gate for the adaptive allocator build.

Scope: per-subscription quota admission, concurrent request reservations,
project-aware capacity lending, quota reconciliation, near-floor routing and
management observability. This contract extends `QUOTA_REFRESH_CONTRACT.md`;
that document remains authoritative for provider polling and host/plugin secret
boundaries.

## 1. Production incident that this contract closes

The confirmed incident used a secondary Claude x5 subscription with a configured
session floor of 20 percent. Provider quota was polled every 600 seconds and the
allocator reserved the tariff's static `0.1%` for every accepted request. A burst
of 109 successful requests was therefore represented locally as only 10.9
percentage points. The real provider cost was much higher. The next confirmed
snapshot reported 2 percent and then 0 percent remaining, so routing stopped only
after the protected floor had already been crossed.

This was not a missing mutex: the current lease atomically subtracts
`in_flight + pending + reservation`. The defect was that `reservation` was a
constant unrelated to model, context, effort, observed burn or burst velocity.

The build is accepted only if the exact incident is reproducible before the
change and is prevented after the change.

## 2. Goals

- Keep normal development on the fast path: no provider quota I/O or artificial
  sleeps. Admission may wait for one bounded local durability barrier that
  records `prepared` before provider I/O.
- Allow a project's primary subscriptions to be spent down to confirmed zero.
- Preserve the configured session and weekly floors of every secondary
  subscription using conservative, dynamically sized reservations.
- Account atomically for concurrent and accepted-but-not-yet-reconciled work.
- Lend genuinely unused secondary capacity to active projects without allowing
  one burst to consume capacity protected for another project.
- Move traffic gradually as a floor approaches instead of exposing a sudden
  error to the client.
- Prefer a compatible failover over waiting. Reject only when no permitted
  candidate can safely satisfy the request contract.
- Make every decision and uncertainty term inspectable without exposing prompts,
  keys or provider credentials.

## 3. Non-goals

- Bravo does not promise the exact future percentage a provider will report.
  Provider accounting is delayed and may use undisclosed weights.
- Bravo does not interrupt a request after the provider has accepted it merely
  because a later snapshot crossed a floor.
- This contract does not authorize automatic prompt compaction, request
  truncation, model-contract degradation or hidden effort reduction.
- Quota probes remain background host operations. Inference requests must not be
  used as quota probes and quota refreshes must not be performed per request.
- Adaptive quota control is not a billing ledger or a replacement for provider
  usage records.

## 4. Terminology

- **primary**: a subscription listed in the current project's
  `primary_auth_ids`. It has priority and permission to spend below tariff
  floors, but not permission to retry a provider-confirmed zero window.
- **secondary**: any allowed, non-primary subscription considered for the
  project. Both session and weekly floors apply.
- **LKG**: the latest provider-confirmed quota snapshot.
- **lease**: an atomic admission reservation acquired immediately before one
  physical provider attempt.
- **in-flight**: leases whose provider outcome is not known.
- **pending**: conservative cost of provider-accepted or ambiguously accepted
  attempts not yet reconciled into a confirmed quota snapshot.
- **request estimate**: an upper-confidence estimate of the quota percentage one
  physical attempt can consume in each provider quota window.
- **uncertainty guard**: capacity held back for snapshot age, estimator error and
  unresolved provider accounting.
- **demand guard**: capacity held for predicted near-term primary demand of other
  projects that own the subscription.
- **safe surplus**: secondary capacity above its floor after every reservation
  and guard has been subtracted.
- **near-floor mode**: an account-local cautious mode; it is not a provider-wide
  throttle and must not slow unrelated subscriptions.

## 5. Hard invariants

### Q-A01: inference remains independent of quota HTTP latency

Allocation reads one immutable local state snapshot. It MUST NOT call, await or
sleep for a provider quota endpoint, and it MUST NOT add a batching sleep. A
refresh may be requested asynchronously. The normal path performs bounded local
work plus one small local WAL sync before provider I/O. Concurrent prepares may
share an already queued group commit; the queue is hard-bounded and saturation
fails admission closed.

### Q-A02: primary and secondary semantics are explicit

For a primary, configured tariff floors are ignored and the effective floor is
zero. Confirmed zero blocks that quota window. Stale or unknown primary behavior
remains as defined by `QUOTA_REFRESH_CONTRACT.md`.

For a secondary, an attempt may be admitted only when both session and weekly
safe remaining values stay strictly above their configured floors after the new
lease. A project pool, route priority or sticky assignment may never bypass this
gate.

### Q-A03: admission is atomic per credential

Eligibility recheck and lease acquisition MUST occur in one credential-scoped
critical section. Two concurrent attempts cannot both spend the same headroom.
Selection before lease acquisition is advisory; a failed lease must continue to
the next compatible candidate without a provider call.

### Q-A04: no successful or ambiguous attempt becomes free prematurely

A lease moves from `in-flight` to `pending` when the provider accepted the
attempt or acceptance is ambiguous. It may be released without pending cost only
when Bravo proves the provider did not accept the request. HTTP success with an
invalid downstream response still counts as pending.

For the 0.8.3 host ABI, every validation, capability and rewrite failure that
Bravo can prove is pre-accept MUST occur before lease acquisition and before the
host execution callback. Once `callHost` is invoked, any returned error is
acceptance-ambiguous and MUST become pending; Bravo must not infer
`accepted=false` from transport text, HTTP class or provider wording. A future
core may narrow this only with an explicit typed `accepted=false` marker.

Pending reservations are cleared only by a confirmed snapshot whose acquisition
watermark proves that the provider observation includes those attempts. A refresh
must capture a start watermark; work accepted after that watermark remains
pending even if the refresh completes later. A refresh failure clears nothing.

### Q-A05: failover spends a new lease

Every physical retry or provider failover acquires its own lease. A failed
candidate's uncertain cost remains charged to that credential while the next
candidate is evaluated. Bravo must neither double-release the first lease nor
transfer it to the fallback credential.

### Q-A06: no request is killed to repair a forecast

A changed forecast affects new admissions only. Accepted streams and non-stream
requests run to their normal completion/deadline. Existing stream-commit rules
continue to prevent unsafe replay after client-visible content.

### Q-A07: adaptive control is account-local and project-aware

Pressure on one credential must not throttle another credential with safe
surplus. Project tempo influences which safe secondary is preferred and how much
capacity may be borrowed; it never weakens a secondary floor.

### Q-A08: uncertainty cannot masquerade as available quota

Expired or unknown quota cannot provide a protected-floor guarantee. In enforce
mode a secondary with unknown quota is blocked regardless of a legacy
`unknown_secondary_policy: allow` setting, unless the operator explicitly selects
an unsafe compatibility mode that is visibly labelled as not floor-safe. Stale
LKG is usable only with its age guard and within the configured maximum stale
interval.

### Q-A09: contract compatibility wins over quota availability

A quota-safe candidate is not eligible unless it preserves protocol, tools,
vision, reasoning, context and other request contracts. Quota control must not
silently compact, truncate, reduce effort or substitute an incompatible model.

### Q-A10: no generic allocator bypass may spend a secondary reserve

`allocatorBypassPlan` (or any renamed/generalized recovery path) MUST NOT turn an
allocator rejection into an unmanaged attempt on a secondary subscription. In
particular, it cannot bypass a session/weekly floor, a dynamic reservation, an
in-flight/pending charge, an age/burst/demand guard, or the unknown-quota policy.
The fact that a credential is authorized, healthy and capable does not make its
protected quota available.

After a secondary is withheld, recovery consists only of evaluating another
eligible, project-allowed, contract-compatible credential or provider through
the normal managed lease gate. If none exists, Bravo returns a typed,
informative Russian error describing the protected window/floor and why no
fallback remained. It does not silently set `AllocatorManaged=false`.

This rule does not remove the explicit primary policy: the same credential may
still be spent toward confirmed zero when it is primary for the current project.
Any narrowly scoped `/compact` exception remains a separate, auditable contract
and cannot be used as a generic allocator fallback.

## 6. State model

The allocator maintains the following runtime state per `auth_index` and, where
the provider exposes them, independently per session, weekly and model-weekly
window:

```text
confirmed_remaining
confirmed_at
snapshot_generation
in_flight[attempt_id] = reservation + admitted_at + project + request_features
pending[attempt_id]   = reservation + accepted_at + snapshot_watermark
burn_estimator
uncertainty_error
near_floor_state
```

Project demand state is keyed by stable project ID and contains only aggregate,
non-secret measurements:

```text
admitted reservation rate over 1m / 10m / 60m
completed request rate over 1m / 10m / 60m
active leases by model family and credential
recent input, output, cached and reasoning token totals
last activity time
```

Attempt IDs and reservation state are runtime-only. Aggregate estimator state
and enough pending state for conservative restart recovery may be persisted.
Persistence must use bounded collections, schema versioning and the existing
atomic state-write path. A restart may over-reserve until reconciliation; it may
not forget accepted work and over-admit secondaries.

The adaptive ledger is schema v4. A v3 snapshot is migrated additively to v4;
an older binary must reject v4 instead of decoding it as v3 and erasing debt.
`prepared` is synchronously durable before provider I/O. Finalization changes
the in-memory absolute ledger first and is queued without waiting for another
fsync; a crash before that queued record is durable restores `prepared`, which
only over-reserves. The next synchronous prepare is ordered after queued
finalization records.

The sidecar WAL is hard-bounded to 8 MiB and 16,384 records per state path.
Every ledger transition schedules a debounced checkpoint, but continuous
traffic cannot postpone that checkpoint beyond 30 seconds from the first dirty
transition. Reaching either hard cap makes a synchronous prepare checkpoint and
compact before provider I/O; if that durability step fails, admission fails
closed. Async finalization never drops debt: it retains the durable prepare and
requests the same conservative checkpoint fallback.

Periodic checkpoint filesystem work never holds the live usage-state mutex.
Bravo captures a deep, consistent snapshot under the short state lock, then
marshals, fsyncs and renames after releasing it. WAL compaction removes only
per-auth revisions represented by that snapshot; later prepare/finalize records
survive the rewrite. A stalled checkpoint therefore cannot delay provider
admission, and a crash still replays every post-capture mutation.

### Rollback and canary procedure

- Back up the last production v3 state before the first v4 deployment.
- A canary uses a copied, separate state path and WAL; it never points at the
  production state files.
- Never start a v3/0.8.2 binary against a v4 state file.
- Rollback requires disabling secondary routing, restoring the v3 backup, and
  reconciling provider quotas before secondaries are enabled again.
- A saturated overflow marker is cleared only by the authenticated management
  reconciliation action after retained pending/prepared and live in-flight work
  are all zero. Until then all secondary and `/compact` routes fail closed;
  existing tracked primaries may continue, but a new auth identity may not.

## 7. Dynamic request estimate

`tariff.reservation_percent` becomes the minimum reservation, not the complete
estimate. The estimate is calculated before each physical attempt from safe
request metadata already available to Bravo:

- provider and exact physical model, with model-family fallback;
- tariff multiplier;
- translated input/context token count or a conservative byte/token bound;
- requested maximum output;
- requested and effective effort;
- streaming/background mode, tools and other cost-relevant capabilities;
- recent observed cost distribution for comparable attempts;
- current concurrency and project burst velocity;
- estimator sample count and error.

No prompt content is stored. A missing feature increases uncertainty; it must not
reduce the estimate.

For each quota window `w`:

```text
request_reservation(w) = max(
    tariff_minimum,
    cold_start_upper_bound,
    learned_upper_quantile(features, w) + model_error_margin(w)
)
```

The learned value must be an upper confidence estimate, not a mean. An EWMA may
track changing workloads, but admission uses a bounded high quantile or the EWMA
plus an error margin. Samples from short Haiku requests must not train down the
estimate for long Opus/Fable requests. Sparse exact-model buckets fall back to a
more conservative provider/model-family bucket.

Reservations have configured lower and upper bounds. The upper bound protects
against a single pathological request; if the remaining secondary surplus cannot
cover that bound, Bravo fails over rather than gambling the protected floor.

## 8. Reconciliation and learning

When a new confirmed snapshot arrives, Bravo calculates the provider-observed
delta from the previous snapshot after accounting for a real reset. The delta is
attributed only across attempts inside the snapshot's proven watermark range.
Attribution uses request weights (model, context, effort and tokens), never equal
`request_count` when richer evidence exists.

If exact per-attempt attribution is impossible:

1. distribute the delta proportionally to conservative request weights;
2. retain an unattributed-error term for the credential;
3. increase, never decrease, the near-term uncertainty guard;
4. decay the guard only after multiple accurate confirmed intervals.

A quota increase is treated as a reset only when the provider window metadata
proves a reset. It is not negative consumption and must not train the estimator.
Out-of-order snapshots do not reconcile or clear reservations.

## 9. Safe remaining and admission rule

For each applicable secondary window:

```text
age_guard = upper_burn_rate * snapshot_age + estimator_uncertainty
burst_guard = forecast_unobserved_burn_until_next_refresh
demand_guard = protected_primary_demand_of_other_projects

safe_remaining = confirmed_remaining
               - sum(in_flight reservations)
               - sum(pending reservations)
               - age_guard
               - burst_guard
               - demand_guard

admit iff safe_remaining - new_request_reservation > configured_floor
```

The same lease reservation can be conservatively applied to both session and
weekly windows when the provider does not expose separate cost weights. A
model-weekly window replaces the generic weekly value only according to the
existing reviewed matching rules.

The operator's floor is the protected value. Safety is created by stopping at
`floor + guards`, not by silently changing the displayed floor. The management
view must show both values.

Because provider accounting is not synchronous, the floor is an admission
guarantee based on an explicit conservative bound, not a claim that an unknown
provider formula can never report a lower value. Canary acceptance therefore
also measures observed slip. The default SLO is no more than one percentage
point below a secondary floor in the calibrated incident workload; any larger
slip automatically increases the guard and blocks release.

## 10. Project tempo and capacity lending

Bravo estimates each active project's near-term demand from the 1/10/60-minute
reservation rates. The short window reacts to agent bursts, while the longer
windows prevent one pause from making an active owner appear idle.

For a subscription, projects listing it as primary are its owners. The demand
guard is the conservative aggregate owner demand until the next reliable quota
observation, capped so that stale forecasts cannot reserve the whole account
forever. Non-owner projects may borrow only the remaining safe surplus.

Among eligible secondaries, ordering considers:

1. greatest safe surplus after floors and guards;
2. lowest predicted contention with owner projects;
3. lowest normalized recent provider consumption for the tariff multiplier;
4. existing stable rendezvous tie-breakers.

There is no fixed per-project throughput cap in the green zone. An active project
may consume idle surplus rapidly while it remains provably safe. When owners
become active or a quota becomes stale, new borrowed work is smoothly directed
elsewhere; already accepted work continues.

## 11. Operating modes

Mode is computed per credential from the smallest session/weekly safe headroom.
Thresholds include estimator uncertainty and may move outward during bursts.

### Green

- Safe headroom is comfortably above floor and guard.
- Allocation is the existing local fast path.
- No artificial delay or per-project concurrency limit is introduced.
- Background polling keeps its normal configured cadence.

### Amber (near floor)

- The credential is still safe, but forecast headroom is narrowing.
- Reservations use the conservative upper estimate.
- A background refresh is requested subject to the provider polling gate; the
  inference path does not wait for it.
- New traffic is preferentially routed to a safer compatible credential.
- Credential-local admission concurrency may be reduced to the number whose
  reservations fit; there is no global provider pause.
- If another candidate exists, failover is immediate and invisible to the
  client.

### Red (protected)

- The post-admission bound cannot remain above a secondary floor, quota is
  expired, or estimator uncertainty exceeds safe surplus.
- No new secondary lease is issued.
- Bravo proceeds to the next permitted compatible credential/provider.
- If no route remains, the response explains in Russian which floor/window
  protected the account and which fallback constraints prevented continuation.

Primary credentials use the same estimation and telemetry but their floor is
zero. They may enter amber near zero and fail over proactively; only confirmed
zero or insufficient positive bound forces red.

Mode transitions use hysteresis so snapshots around a threshold do not flap.

## 12. Latency and availability requirements

- Green allocator-compute p95 overhead introduced by adaptive control: at most
  2 ms in the canary benchmark at realistic project, credential and candidate
  cardinality, with no network-dependent tail.
- The single local durability barrier that records `prepared` before provider
  I/O has a separate canary p95 budget of at most 15 ms. It performs no provider
  or quota network I/O and adds no fixed batching sleep. Finalization is ordered
  and asynchronous; a crash before it is durable restores conservative
  `prepared` debt.
- No quota refresh is triggered once per inference request.
- Amber must not queue a request when an eligible fallback exists.
- A credential-local concurrency guard must not block unrelated credentials,
  providers or projects.
- Estimator updates, finalization and snapshot compaction run outside the
  response-critical section. Only the bounded pre-provider `prepared` WAL sync
  is response-critical.
- Every mutex critical section is bounded; no provider execution occurs while an
  allocator lock is held.

## 13. Failure and recovery behavior

- Provider quota/rate-limit/model errors retain their existing typed scopes and
  cooldown semantics.
- A reviewed account-quota exhaustion sets safe remaining to zero for the
  affected credential/window and causes immediate compatible failover.
- A quota refresh failure keeps LKG and pending reservations. Its increasing age
  expands the guard until the secondary becomes red.
- If the adaptive estimator is corrupt, non-finite or missing, a conservative
  cold-start bound is used. Invalid numbers never make capacity available.
- On restart, uncertain accepted work is restored conservatively or represented
  by a restart guard until one confirmed snapshot reconciles it.
- Configuration reload is atomic. Lowering a floor affects future admissions;
  raising a floor cannot cancel existing work.
- `allocator_mode: observe` computes and records decisions but preserves legacy
  routing. `enforce` applies this contract. Observe mode must be visibly labelled
  as unprotected.

Observe shadow debt is intentionally isolated from the enforce estimator and
project-demand state in 0.8.3. It validates would-admit/withhold/fallback
decisions for static/current inputs but does not promote provider-delta learning
or legacy owner/borrower tempo into enforcement. Promotion without separate
provenance could poison enforce with traffic that ignored shadow gates.
Enforcement therefore starts from the conservative cold prior and must pass the
synthetic enforce canary, including owner-demand scenarios, before production
activation. Isolated observe estimator/demand promotion is a follow-up phase.

## 14. Management and route-trace contract

For each subscription the management API and UI expose, in safe rounded form:

- provider-confirmed remaining and observation age;
- configured session/weekly floor;
- effective admission cutoff (`floor + guards`);
- green/amber/red mode and a Russian explanation;
- in-flight and pending request counts plus reserved percentage;
- current dynamic request estimate range;
- age, burst, uncertainty and owner-demand guard totals;
- next scheduled/allowed quota refresh and refresh error independently;
- 1/10/60-minute aggregate request tempo;
- last decision reason and safe failover target, when one was used.

Selectable-period history for observed floor slip and estimator error is a
follow-up analytics phase, not a 0.8.3 admission-safety gate. Version 0.8.3
exposes the current decision, guards, estimate, counters and route trace needed
to diagnose a slip; it does not claim historical percentile storage.

Route traces record the reservation estimate, safe remaining before/after,
project role (primary/secondary), mode and rejection reason. They do not record
prompt bodies, plaintext keys, OAuth data, full auth paths or provider secrets.

Required stable decision reasons include:

```text
adaptive_green_admitted
adaptive_amber_admitted
adaptive_secondary_floor_protected
adaptive_quota_stale_protected
adaptive_concurrency_recheck_failed
adaptive_primary_zero
adaptive_failover_selected
adaptive_no_compatible_fallback
```

## 15. Compatibility and rollout

- Existing tariff `reservation_percent` remains valid as the minimum. No clean
  installation or upgrade may silently reset configured floors.
- Old persisted state without estimator fields loads with conservative cold-start
  estimates.
- A 0.8.2/v3 binary must not open the v4 state. Downgrade follows the documented
  backup restore, secondary-disable and quota-reconciliation procedure in
  section 6; silently ignoring adaptive debt is forbidden.
- The feature ships observe-first in canary. Enforcement is enabled only after
  the incident replay, concurrency, latency and failover gates in
  `ADAPTIVE_QUOTA_TEST_PLAN.md` pass.
- Production replacement, GitHub publication and cache cleanup follow the
  established sequence and require the user's explicit production approval.
