# Adaptive edge gate v4 test plan

Status: Bravo 0.9.0-preview.12. `observe` is shadow-only; production `breaker`
may skip only a route closed by a real reviewed quota/rate-limit failure and
immediately continue the configured neighboring route. Full forecast
`enforce` remains experimental.

## Contract

1. `off` and `observe` keep the same attempts, order, provider-call count,
   responses and quota-poller cadence.
2. Green and Guarded do not serialize requests in `breaker`; 100 concurrent
   low-headroom attempts without a breaker must all dispatch.
3. Only Tripped/Half-open state can skip in `breaker`; no condition variable,
   channel wait or retry loop is allowed.
4. Only a real quota/rate-limit outcome trips a breaker. Account-wide outcomes
   cover the subscription; explicit model scope covers only that physical
   model.
5. Before expiry a matching attempt is `would_skip_tripped`. After expiry
   exactly one concurrent attempt is `would_probe`; all peers are
   `would_skip_busy`. Applied skips continue the existing neighboring route
   without a background provider request.
6. A successful or provider-confirmed reviewed non-quota probe reopens. A
   local cancellation, supersede, callback failure or bootstrap panic is
   inconclusive: it releases only its owned lease and keeps the breaker closed.
   A quota-failed probe retrips using provider `Retry-After` when valid and the
   existing cooldown fallback otherwise.
7. The simulated lease remains owned until classified outcome, even though the
   real allocator lease may already have been released.
8. Token-count probes and attempts rejected before provider dispatch never
   mutate the gate.
9. Unknown/stale quota and forecast-runtime caps fail open and are visible. A
   protected scheduled or recovery proof never fails open when proof
   coordination is saturated; it is skipped locally. Scheduled and recovery
   proofs share one non-queued per-auth turnstile, and stale leases recover
   after the hard maximum age.
10. If all ordinary neighbors fail and the configured provider-call budget has
    room, at most one skipped attempt per breaker generation is restored as a
    global single-flight recovery probe. Concurrent requests never wait and may
    not dispatch additional recovery calls. The coordinator never reserves a
    slot by withholding a potentially healthy neighbor. No final envelope or
    stream close may contain a `bravo_adaptive_*` code, including when
    `max_attempts` is exhausted.
    In streaming execution this retained proof is appended only at the tail;
    it is never selected or launched as a latency hedge.
11. Every trusted quota fact advances a monotonic evidence revision. A late
    result from an older proof may release only its own lease; it cannot erase
    newer quota evidence, shorten its cooldown or cross a model-to-account
    rescope.
12. Audit JSON contains state and numeric outcomes but no project ID,
    subscription/auth identity, key, prompt, headers, raw provider body or
    response.

## Required checks

```text
gofmt
cd plugins/bravo/go && go test .
cd plugins/bravo/go && go test -race .
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

## Observe and breaker review

Collect at least 100 edge-gate attempts spanning six hours, preferably a full
day. Review independently:

- Green / Guarded / Tripped / Half-open counts;
- `would_skip_busy` and `would_skip_tripped` frequency;
- successful actual attempts behind a counterfactual breaker skip (fallback
  cost; proactive Guarded evidence does not authorize `breaker`);
- quota failures behind a counterfactual breaker skip (pressure it could avoid);
- quota failures on dispatch/probe and resulting trip transitions;
- exact-one-probe and reopen behavior;
- queue/disk loss, runtime saturation, mode-correct routing changes and the
  invariant zero additional provider requests;
- every applied skip is `not_dispatched` unless it is the one explicit
  last-chance baseline attempt;
- switching the config during an in-flight request does not relabel the
  immutable attempt or its audit record.

`ready_for_review` remains evidence, not an automatic mode switch. Roll out in
`observe`, then set explicit `breaker`; returning to `observe` is the immediate
rollback and 0.8.11 remains the stable binary rollback. Full `enforce` needs a
separate forecast gate and is not covered by breaker readiness.
