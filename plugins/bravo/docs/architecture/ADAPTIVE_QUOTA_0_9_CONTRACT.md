# Bravo Adaptive Quota 0.9 Preview Contract

Status: preview, phase 1 (`shadow_only`). Base: published Bravo 0.8.11.

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
4. State is bounded to 4096 credential identities and 256 recent commitments per
   identity. Excess recent commitments are conservatively coalesced; an excess
   identity is counted as dropped telemetry and never changes routing.
5. Every commitment and learned uplift cools exponentially. Default half-life is
   five minutes and the hard maximum age is thirty minutes. At maximum age its
   effect is exactly zero; no adaptive decision can remain sticky forever.
6. Phase-1 state is runtime-only. A restart intentionally cold-starts the shadow
   estimator and cannot reopen or close a production route because the estimator
   has no routing authority.
7. Public project views expose only aggregate values after the project's allowed
   account pool is applied. Credential identities are not returned.

## Estimate

The local estimate combines:

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

## Reconciliation

A provider-confirmed quota observation may reconcile only shadow commitments at
or before its strictly newer observation timestamp. Commits after that timestamp
remain. Failed, equal, older or unknown observations do not clear shadow state.

This preview does not claim exact attribution for provider activity made outside
Bravo. Such activity can make learned calibration conservative; cooling bounds
that effect.

## Observability

`/v1/bravo/limits`, `/v1/bravo/routes`, the subscription Management API and the
Bravo status response disclose:

- mode and `shadow_only` effect;
- `routing_enforced=false`;
- `additional_provider_requests=false`;
- cooling half-life and hard maximum age;
- bounded aggregate commitment, effective pending and learned-scale values;
- saturation/drop counters without credential identities.

## Promotion gate

No phase-2 routing authority may be added to this preview branch. Promotion
requires a separate reviewed change with production traces proving:

- identical route/provider-call behavior between `off` and `observe`;
- zero additional quota/provider requests under request bursts;
- bounded memory under identity and commitment saturation;
- deterministic half-life and hard-expiry behavior;
- acceptable latency on maximum supported request bodies;
- an explicit fail-open availability path if future adaptive state is unknown.
