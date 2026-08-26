# Adaptive token calibration v2 and forecast backtest v3 test plan

Status: Bravo 0.9 shadow preview. The module has no routing authority.

## Acceptance invariants

1. `off` and `observe` produce the same candidates, execution order, fallback,
   provider-call count, client response, and production quota polling cadence.
2. Calibration consumes only normal `UsageRecord` token counters and existing
   confirmed quota snapshots. It performs no token-count or quota request.
3. Session, weekly, and model-weekly rates and pending reservations remain
   independent. They are never summed into one quota percentage.
4. A snapshot reconciles only usage completed after the previous confirmed
   observation and at or before the new observation watermark. Older late
   telemetry is dropped visibly rather than moved into a newer interval.
5. Resets, unexplained increases, equal/older snapshots, unknown quota, missing
   usage, and insufficient evidence cannot produce a calibrated estimate.
6. Missing calibration falls back to the legacy shadow calculation for that
   window. Saturation, persistence failure, or telemetry loss never changes a
   real route.
7. Runtime accounts/events and persisted usage/window profiles have hard bounds.
   Public views contain aggregate numeric rows only and no credential/project
   identity, prompts, responses, keys, headers, or raw provider errors.
8. Reconciled profiles survive restart, decay with a 24-hour half-life, and are
   ignored after insufficient decayed evidence or 31 inactive days.
9. Backtest scoring uses the reservation captured before the request. It keeps
   session, weekly and model-weekly predictions independent, excludes reset and
   mixed cold/partial intervals, and never changes routing.
10. Forecast histograms are fixed-size, persist inside the bounded quota
    observation state, and expose no credential or project identity.

## Deterministic tests

- Provider-aware actual token units: Claude cache read/write and Codex reasoning
  are counted without double-counting provider totals.
- Eight usage samples across four confirmed intervals create three independent
  rates whose direction follows the fixture's session/model/weekly drops.
- Output prediction uses the decayed actual completion p90 and never exceeds a
  trusted request declaration.
- A post-snapshot event stays queued; an event older than the previous snapshot
  increments dropped telemetry and does not lower a newer interval's rate.
- Two projects with a 90/10 actual token split receive a 90/10 share of the
  locally attributed confirmed drop even when their cold estimates were equal.
- Window-vector pending proves a large weekly commitment does not consume the
  session window and vice versa, including after the 256-commit history is
  coalesced under burst load.
- JSON restart restores exact usage and window profiles; public JSON contains no
  auth identity.
- Event/profile cap+1 fixtures remain bounded and select cold shadow fallback.
- Extreme finite input clamps to the 10 percentage-point shadow ceiling.
- The production-shaped over-reservation replay replaces a cold multi-point
  reservation with a positive sub-point token estimate without changing route.
- Audit reports token-calibrated decisions separately from cold/legacy ones.
  A decision enters that cohort only when both effective quota dimensions are
  calibrated; partial profiles remain explicitly outside it.
- Window-specific fixtures compare the pre-request prediction with the next
  confirmed drop and verify signed bias, absolute error, under/overprediction,
  conservative coverage, p95, and maximum underprediction.
- Calibration eligibility is checked per exact window: a ready session profile
  cannot admit an uncalibrated weekly reservation into the paired cohort.
- Reset, external-only, and mixed legacy intervals increment explicit skipped
  counters and never enter the paired backtest cohort.
- Twelve paired intervals over six subscription-hours survive restart and make
  the corresponding window available for manual review without granting route
  authority.

## Required gates

```text
gofmt
cd plugins/bravo/go && go test .
cd plugins/bravo/go && go test -race .
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

## Shadow canary review

Run at least 24 hours and compare only complete attempts whose confidence begins
with `token_calibrated_`. The audit exposes a separate token-calibration verdict
so historical cold/legacy disagreements cannot contaminate this cohort:

- successful `would_withhold` rate;
- quota failures on `would_admit`;
- reservation/confirmed-drop ratio per independent window;
- mean bias, mean absolute error, p95 and maximum underprediction from the
  paired pre-request forecast cohort;
- dropped/saturated telemetry, persistence errors, queue depth, and disk bound;
- provider request count and quota polling cadence against the prior preview.

Promotion is a separate reviewed change. A green shadow report never enables
`assist` or `enforce` automatically.
