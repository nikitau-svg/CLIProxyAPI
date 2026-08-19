# Bravo Adaptive Quota 0.9 Preview Contract

Status: preview, phase 1 (`shadow_only`), token calibration v2. Base: published Bravo 0.8.11.

## Purpose

The preview measures how much subscription headroom a real request may consume
without allowing that estimate to control production routing. It exists to
collect evidence for a later allocator, not to activate one implicitly.

## Non-negotiable phase-1 invariants

1. `adaptive_allocator_mode` accepts only `off` and `observe`. `assist` and
   `enforce` fail configuration with an explicit error.
2. `observe` preserves the exact 0.8.11 execution order, eligibility decisions,
   fallback behavior and provider-call count. Shadow data cannot withhold an
   account or change a tariff floor.
3. The adaptive module performs no provider I/O, does not wake quota polling and
   does not shorten its cadence. It may only consume quota snapshots already
   produced by the existing background poller.
4. Legacy shadow state is bounded to 4096 credential identities and 256 recent
   commitments per identity. Token calibration is separately bounded to 4096
   identities, 2048 unreconciled usage events per identity, 32768 runtime events
   globally, 4096 usage profiles, and 4096 quota-window profiles. Saturation is
   visible and falls back to the legacy shadow estimate; it never changes
   routing.
5. Every commitment and learned uplift cools exponentially. Default half-life is
   five minutes and the hard maximum age is thirty minutes. At maximum age its
   effect is exactly zero; no adaptive decision can remain sticky forever.
6. Phase-1 pending commitments are runtime-only. Reconciled token-rate profiles
   and aggregate consumption analytics persist so that observation can continue
   across restarts. They remain telemetry-only and therefore cannot reopen or
   close a production route.
7. Public project views expose only aggregate values after the project's allowed
   account pool is applied. Credential identities are not returned.
8. The durable shadow audit is telemetry-only. Enqueue is non-blocking; queue,
   memory, records, and disk are hard-bounded. A telemetry failure may lose an
   audit record but cannot delay or fail inference and cannot alter a route.
9. Audit records never contain project/credential identity, API or OAuth
   secrets, headers, prompt/request/response bodies, or raw provider messages.

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

Session, weekly and model-weekly percentage points are separate constraints and
must never be summed into a single consumption percentage. Capacity estimates
may express attributed use as subscription-window equivalents, average/peak
percentage points per hour and x1-equivalent capacity. They remain advisory and
carry a confidence state until enough confirmed observations exist.

## Observability

`/v1/bravo/limits`, `/v1/bravo/routes`, the subscription Management API and the
Bravo status response disclose:

- mode and `shadow_only` effect;
- `routing_enforced=false`;
- `additional_provider_requests=false`;
- cooling half-life and hard maximum age;
- bounded aggregate commitment, effective pending and learned-scale values;
- saturation/drop counters without credential identities.

The aggregate view also exposes `token_calibration.status`, bounded profile and
event counts, and a separate row for every provider/window/model-window. A row
reports effective profile intervals, profile coverage, token units, confirmed
drop, and percentage points per million tokens. It never adds session, weekly,
and model-weekly drops into a false grand total.

The management analytics response and the project limits response additionally
contain 30-day quota-consumption analytics. Management may show shared-pool
observed and unattributed aggregates plus a per-window project ranking. A
project-key response exposes only that project's rows and never credential or
neighbouring-project identities. Model/effort shares and subscription-capacity
recommendations are advisory; low-confidence data explicitly recommends
continued collection rather than reallocation.

`GET /v0/management/bravo/adaptive-audit` adds a bounded 1–168 hour audit report
and an optional limited recent-record sample. It is built exclusively from real
inference attempts; token-count probes are excluded. Its
`routing_changes_applied` and `additional_provider_requests` values must remain
zero in phase 1. The audit worker uses a 1024-record queue, 4096-record memory
ring, at most 16 attempts per request, and two 4 MiB JSONL files. Disk errors
degrade telemetry only.

The audit distinguishes fully `token_calibrated_*` attempts from partial and
cold/legacy shape attempts and reports disagreements for both. An attempt joins
the fully calibrated cohort only when the effective session and weekly (global
or model-specific) dimensions both use learned token rates. A separate
`token_calibration_verdict` therefore cannot be contaminated by a legacy
fallback in the limiting window. `ready_for_review` requires at
least 100 requests over at least six hours with
no queue/disk loss, no unknown shadow decision, no successful `would_withhold`
attempt, and no quota failure on an attempt marked `would_admit`. This verdict
does not enable routing authority.

## Promotion gate

No phase-2 routing authority may be added to this preview branch. Promotion
requires a separate reviewed change with production traces proving:

- identical route/provider-call behavior between `off` and `observe`;
- zero additional quota/provider requests under request bursts;
- bounded memory under identity and commitment saturation;
- independent session/weekly/model-weekly token rates surviving restart;
- refresh-watermark tests proving that post-snapshot usage remains queued;
- saturation degrading to cold shadow estimates without any routing effect;
- deterministic half-life and hard-expiry behavior;
- acceptable latency on maximum supported request bodies;
- a clean bounded audit with at least 100 requests over six hours and a manual
  review of fallback and disagreement samples;
- an explicit fail-open availability path if future adaptive state is unknown.
