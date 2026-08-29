# Bravo Adaptive Quota 0.9 Preview Contract

Status: Bravo 0.9.0-preview.12, phase 3 with explicit `observe`, evidence-only
`breaker`, and experimental full `enforce` modes, token calibration v2,
forecast backtest v3 and edge-gate state machine v5. Base and stable rollback:
published Bravo 0.8.11.

## Purpose

The preview measures subscription use and provides a reversible local routing
gate without changing the logical-route contract. Token math remains useful for
analytics and capacity planning and, only in experimental full `enforce`, may
reserve a fresh provider-confirmed quota before dispatch. Production `breaker`
does not predict request cost or act on cached low headroom: only a real reviewed
quota/rate-limit outcome may close a route.

## Non-negotiable invariants

1. `adaptive_allocator_mode` accepts `off`, `observe`, `breaker`, and `enforce`;
   an empty value normalizes to `observe`. `assist` and unknown values fail
   configuration with an explicit error.
2. `observe` preserves the exact 0.8.11 execution order, eligibility decisions,
   fallback behavior and provider-call count. Shadow data cannot withhold an
   account or change a tariff floor.
3. `breaker` may skip only an auth/model protected by a real reviewed quota or
   rate-limit failure. It returns immediately to the existing executor so an
   already configured neighboring route can continue. If no ordinary neighbor
   answers and the configured provider-call budget remains, exactly one
   previously skipped attempt may run as a global single-flight recovery probe.
   At most one recovery call is dispatched per breaker generation; competing
   requests never wait. The coordinator never reserves that slot by withholding
   a potentially healthy neighbor. A local `bravo_adaptive_*` decision can never
   be the final client error, including when `max_attempts` is exhausted. The
   mechanism never waits, queues, or creates background work.
4. The ordinary allocator remains authoritative for project allowlists,
   primary ownership, disabled subscriptions, cooldowns and tariff floors. A
   project primary precedes shared capacity and has a zero owner floor, but
   confirmed zero quota is still exhausted. A secondary may consume only
   capacity above the owner's independent session and weekly/model-weekly
   floors. There is no fixed per-project concurrency cap.
5. The adaptive module performs no provider I/O, does not wake quota polling and
   does not shorten its cadence. It may only consume quota snapshots already
   produced by the existing background poller.
6. `breaker` ignores forecast/headroom admission entirely. Full `enforce`
   requires a fresh, provider-confirmed quota and valid local forecast; unknown
   or stale quota, missing calibration and bounded-runtime saturation fail open
   to the ordinary allocator. A protected scheduled or recovery proof is the
   narrow exception: coordination saturation skips it locally instead of
   risking a retry stampede. Full `enforce` live forecast reservation is atomic per
   credential; at most 512 are retained per identity and every stale lease has
   a hard two-hour recovery bound.
7. Legacy shadow state is bounded to 4096 credential identities and 256 recent
   commitments per identity. Token calibration is separately bounded to 4096
   identities, 2048 unreconciled usage events per identity, 32768 runtime events
   globally, 4096 usage profiles, and 4096 quota-window profiles. Saturation is
   visible and fail-open; it cannot reject inference merely because telemetry
   storage is full.
8. Every commitment and learned uplift cools exponentially. Default half-life is
   five minutes and the hard maximum age is thirty minutes. At maximum age its
   effect is exactly zero; no adaptive decision can remain sticky forever.
9. Pending commitments, in-flight reservations, transports and edge leases are
   runtime-only. Reconciled token-rate profiles and aggregate consumption
   analytics persist so that observation can continue across restarts. No
   database, credential, YAML or state-file migration is required for
   preview.12.
10. Public project views expose only aggregate values after the project's allowed
   account pool is applied. Credential identities are not returned.
11. The durable audit writer is telemetry-only. Enqueue is non-blocking; queue,
   memory, records, and disk are hard-bounded. A telemetry failure may lose an
   audit record but cannot delay or fail inference. Only the synchronous,
   explicit `enforce` decision may alter a route.
12. Audit records never contain project/credential identity, API or OAuth
   secrets, headers, prompt/request/response bodies, or raw provider messages.
13. Forecast backtesting is computed from the pre-request reservation already
    attached to a real attempt and the next confirmed quota snapshot. It never
    trains on an interval before scoring that interval and never replays a
    request against a provider.
14. A valid taxonomy-v1 classification is authoritative even when legacy text
    or HTTP status contradicts it. Only taxonomy-v1 `quota`/`rate_limit` with a
    reviewed model/account scope, or bare HTTP 429 when valid taxonomy is
    absent (model scope), may trip the breaker. Raw provider strings and cached
    quota cannot fabricate a trip. After expiry, exactly one attempt becomes
    the Half-open probe. `observe` records counterfactual behavior; `breaker`
    applies only evidence-backed Tripped/Half-open decisions.
15. Edge-gate runtime is bounded to 4096 in-flight account leases and 4096
    breakers. Saturation is visible. Shadow/forecast work fails open, while a
    scheduled or recovery proof for an existing breaker skips locally when its
    coordination lease cannot be acquired. A stale simulated lease has a hard
    two-hour recovery bound.
16. The attempt snapshots whether it was built under `observe`, `breaker`, or
    `enforce`. A concurrent hot reload cannot retroactively apply or erase
    routing authority in its asynchronous audit record.
17. A stale confirmed zero whose scheduled reset already elapsed may bypass the
    ordinary allocator only through one non-queued probe per credential and
    reset generation. Competitors continue a neighboring route without a
    provider call. The generation is consumed at provider dispatch, obsolete
    consumed generations are reconciled by fresh confirmed quota, and the
    runtime is bounded to 4096 entries with a two-hour abandoned-lease bound.

## Edge gate v5

The state machine is scoped to one subscription and, for reviewed model-scoped
provider failures, one physical model:

```text
Green     confirmed cached headroom outside the guard band
Guarded   session headroom <= 8pp, weekly headroom <= 2pp,
          or the cached quota is stale/unknown
Tripped   an actual quota/rate-limit result supplied a cooldown/reset window
Half-open the window expired and one non-queued probe owns the turnstile
```

For secondary subscriptions, observed headroom is still measured after the
configured tariff floor; primary subscriptions use a zero floor. This remains
useful shadow telemetry, but `breaker` never serializes or skips a route merely
because it is Green/Guarded, stale, unknown, or forecast-unsafe. Without an
active breaker all attempts remain fully concurrent. An unexpired trusted
breaker records `would_skip_tripped`; after expiry one Half-open user request
owns the nonblocking probe and peers record `would_skip_busy`. In `observe`
these decisions remain counterfactual. In `breaker`, the skipped attempt is
`not_dispatched` while neighbors are tried, with one synchronous baseline last
chance if none answers. Full proactive Guarded/forecast behavior exists only in
experimental `enforce`.

The simulated lease is acquired only after the real allocator grants an
attempt and is settled only after the provider outcome has been classified.
Scheduled and last-chance recovery proofs also share one per-auth nonblocking
turnstile across every model/account breaker for that subscription. A new
trusted quota fact advances a monotonic evidence revision. A late result from
an older proof may release only its own turnstile lease; it cannot reopen or
weaken the newer breaker, including after model-to-account rescope. Local
cancellation, supersede, callback failure and bootstrap panic remain
inconclusive and keep the breaker closed.
This closes the race between releasing an in-flight slot and observing a 429.
Token-count probes never acquire the lease. Account-wide credential failures
trip the account key; explicit provider model scope trips only that physical
model. A successful or non-quota Half-open result reopens the breaker.
The explicit, rate-limited Claude Code `/compact` escape hatch is not subjected
to a second adaptive gate; it still requires confirmed positive quota and keeps
its existing authorization, cooldown and zero-exhaustion boundaries.

## Estimate

Before token calibration is ready, the local cold-start estimate combines:

- the configured tariff reservation as a non-decreasing baseline;
- physical model family and effective effort;
- request bytes and a trusted top-level output-token declaration;
- a conservative maximum output allowance when that declaration is absent or
  ambiguous;
- a provider-observed calibration that may raise the estimate and then cools
  back to `1.0`.

The JSON scanner recognizes only exact top-level `max_tokens`,
`max_output_tokens`, or `max_completion_tokens`. Matching text inside prompts or
tool schemas is ignored. The calculation is local and read-only.

Token calibration v2 consumes the actual input, output, reasoning, cache-read,
and cache-creation counters already delivered to Bravo's normal usage hook. It
does not tokenize prompts, retain request content, or issue a probe. A profile is
keyed by credential, provider, exact physical model, effective effort, and
tariff. Its output estimate uses a decayed p90 completion histogram; its input
estimate remains the bounded local request-size estimate available before the
provider call.

Each quota constraint has its own learned rate:

```text
raw_rate(window) = attributed_confirmed_drop_pp / actual_token_units
reservation(window) = min(10pp,
    predicted_tokens * (raw_rate + quantization_margin) * 1.25)
```

Session, weekly, and model-weekly reservations are never replaced by one shared
scalar. A profile becomes usable only after at least eight usage samples, four
strictly newer confirmed intervals, thirty minutes of effective coverage, and a
minimum decayed evidence weight. Missing or stale evidence selects the legacy
cold-start estimate for that window. The rate and completion histogram have a
24-hour half-life and profiles expire after 31 inactive days.

## Reconciliation

A provider-confirmed quota observation may reconcile only shadow commitments at
or before its strictly newer observation timestamp. Commits after that timestamp
remain. Failed, equal, older or unknown observations do not clear shadow state.

For each independently confirmed session, weekly and model-weekly window, Bravo
also records the observed percentage-point drop. Completed local attempts in the
same observation interval are attributed to projects, physical/logical models,
effort and tariff in proportion to actual usage-token counters when available,
with the pre-call token estimate as a compatibility fallback. If estimates exceed
the observed drop, attribution is scaled down. If the observed drop exceeds all
local estimates, the residual is reported as `external_or_estimator_gap`; it is
never silently assigned to a project or claimed to be known external traffic.
Intervals containing a reset or an unexplained increase are excluded. The token
ledger uses the same strict observation watermark: usage completed after the
snapshot timestamp remains queued for the next interval. Quota drop is divided
between local model/effort/tariff profiles by their actual token volume. Because
unobserved clients may share a subscription, this is treated as a conservative
upper-bound rate rather than proof that all observed drop came from Bravo.
An unreconciled usage event delivered after its nominal interval is excluded
and increments a visible dropped-telemetry counter. It is never moved into a
newer interval where it could lower the learned rate without provider proof.

## Forecast backtest

Every valid confirmed interval keeps the session, global-weekly, and
model-weekly reservation vectors independent. The score for one window is:

```text
signed_error_pp = confirmed_drop_pp - pre_request_reservations_pp
underprediction_pp = max(signed_error_pp, 0)
overprediction_pp = max(-signed_error_pp, 0)
```

Only intervals whose applicable local commitments all used a token-calibrated
reservation for that exact window enter the paired cohort. Calibration is
checked independently: a calibrated session estimate cannot make an
uncalibrated global-weekly estimate look ready. Intervals containing a reset or
unexplained quota increase are excluded by the existing watermark. Intervals
with no local request and intervals containing cold/partial estimates are
counted separately and cannot improve the score.

The 7-day public backtest exposes paired/skipped interval counts, coverage,
predicted and observed drop, mean bias, mean absolute error, total
underprediction/overprediction, conservative-coverage rate, p95
underprediction, and maximum underprediction. The p95 distribution uses a
fixed percentage-point histogram stored inside the already bounded quota
observation buckets; it cannot grow with request volume. Old state is accepted
without migration and begins the new paired cohort at zero.

A positive residual is an upper bound on estimator slippage, not proof of a
local estimator miss: another client may consume the same subscription between
the two provider snapshots. That caveat must remain visible in public output.

Session, weekly and model-weekly percentage points are separate constraints and
must never be summed into a single consumption percentage. Capacity estimates
may express attributed use as subscription-window equivalents, average/peak
percentage points per hour and x1-equivalent capacity. They remain advisory and
carry a confidence state until enough confirmed observations exist.

## Observability

`/v1/bravo/limits`, `/v1/bravo/routes`, the subscription Management API and the
Bravo status response disclose:

- mode and effective `disabled`, `shadow_only`, `breaker_routing_enforced`, or
  `routing_enforced` effect;
- the mode-derived `routing_enforced` and `forecast_routing_enforced` booleans;
- `additional_provider_requests=false`;
- cooling half-life and hard maximum age;
- bounded aggregate commitment, effective pending and learned-scale values;
- saturation/drop counters without credential identities.

The aggregate view also exposes `token_calibration.status`, bounded profile and
event counts, and a separate row for every provider/window/model-window. A row
reports effective profile intervals, profile coverage, token units, confirmed
drop, and percentage points per million tokens. It never adds session, weekly,
and model-weekly drops into a false grand total.

The same aggregate view exposes `forecast_backtest`; management quota analytics
also include the matching backtest row inside each shared-pool window. Project
views may receive only aggregates scoped to their allowed subscription pool.
Neither response contains credential identity.

The management analytics response and the project limits response additionally
contain 30-day quota-consumption analytics. Management may show shared-pool
observed and unattributed aggregates plus a per-window project ranking. A
project-key response exposes only that project's rows and never credential or
neighbouring-project identities. Model/effort shares and subscription-capacity
recommendations are advisory; low-confidence data explicitly recommends
continued collection rather than reallocation.

`GET /v0/management/bravo/adaptive-audit` adds a bounded 1–168 hour audit report
and an optional limited recent-record sample. It is built exclusively from real
inference attempts; token-count probes are excluded.
`routing_changes_applied` must remain zero in `off`/`observe` and must equal the
actual `not_dispatched` adaptive attempts in `breaker`/`enforce`.
`additional_provider_requests` remains zero in every mode. The audit worker uses
a 1024-record queue, 4096-record memory ring, at most 16 attempts per request,
and two 4 MiB JSONL files. Disk errors degrade telemetry only.

The audit distinguishes fully `token_calibrated_*` attempts from partial and
cold/legacy shape attempts and reports disagreements for both. An attempt joins
the fully calibrated cohort only when the effective session and weekly (global
or model-specific) dimensions both use learned token rates. A separate
`token_calibration_verdict` therefore cannot be contaminated by a legacy
fallback in the limiting window. `ready_for_review` requires at
least 100 requests over at least six hours with
no queue/disk loss, no unknown shadow decision, no successful `would_withhold`
attempt, and no quota failure on an attempt marked `would_admit`. This verdict
does not change the configured mode.

The independent `edge_gate_verdict` and per-attempt safe fields include:
state, counterfactual decision, reason, session/weekly headroom, remaining trip
time and outcome transition. Aggregate counters distinguish successful
attempts an enforcing gate would have skipped, quota failures on those skipped
attempts, quota failures while dispatching/probing, and trip/reopen events.
Old audit records remain readable but do not count toward edge-gate coverage.
The edge verdict needs at least 100 new attempts over six hours and still grants
no automatic routing authority.

## Release and rollback gate

Preview.12 is the first release with a production-safe evidence-only routing
authority. A
release candidate must prove:

- identical route/provider-call behavior between `off` and `observe`;
- zero additional quota/provider requests in every mode under request bursts;
- bounded, race-free atomic reservations and fail-open identity/commitment
  saturation;
- primary-before-shared ordering, shared fallback after primary exhaustion and
  independent owner floors under concurrency;
- 100 concurrent low-headroom attempts without a breaker all dispatch;
- an active breaker immediately selects a later route without a queue;
- a single-route or exhausted-neighbor request receives one baseline last
  chance and never a final `bravo_adaptive_*` error;
- exactly one probe across concurrent Half-open attempts;
- one per-auth proof turnstile across scheduled/recovery and model/account
  scope, including evidence-revision and rescope races;
- only a provider-confirmed quota/rate-limit outcome trips a breaker;
- inconclusive local termination never reopens a protected route;
- unknown/stale quota and missing calibration fail open;
- post-snapshot usage remains queued by the refresh watermark;
- independent session/weekly/model-weekly token rates survive restart;
- deterministic half-life and hard-expiry behavior;
- hot reload cannot relabel an already planned attempt between `observe`,
  `breaker`, and `enforce`;
- the bounded audit accurately records `not_dispatched`, routing changes and
  zero additional provider requests without identity or payload data;
- acceptable latency and allocation behavior on maximum supported request
  bodies and high-concurrency streams.

There is no one-shot migration. Existing schema-v3 analytics continue to load;
runtime commitments, transports and gates start empty. A production rollout
must retain the previous image plus pre-cutover configuration and state, run
first in `observe`, verify protocol/management smoke tests and the adaptive
audit, then set explicit `breaker`. Full `enforce` remains experimental until a
separate clean forecast gate is met. Returning to `observe` is the immediate
configuration rollback. Bravo 0.8.11 and the retained pre-cutover state remain
the stable binary rollback; auth files and usage state must never be deleted as
a rollback mechanism.
