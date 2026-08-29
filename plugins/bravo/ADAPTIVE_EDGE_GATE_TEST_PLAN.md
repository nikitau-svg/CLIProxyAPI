# Adaptive edge gate v4 test plan

Status: Bravo 0.9.0-preview.11. `observe` is shadow-only; explicit `enforce`
may skip the current confirmed-unsafe attempt and immediately continue the
configured neighboring route.

## Contract

1. `off` and `observe` keep the same attempts, order, provider-call count,
   responses and quota-poller cadence.
2. Green does not serialize requests.
3. Guarded allows one in-flight attempt per subscription. A second attempt
   records `would_skip_busy` immediately. It still dispatches in `observe` and
   is `not_dispatched` in `enforce`; no condition variable, channel wait or
   retry loop is allowed.
4. Only a real quota/rate-limit outcome trips a breaker. Account-wide outcomes
   cover the subscription; explicit model scope covers only that physical
   model.
5. Before expiry a matching attempt is `would_skip_tripped`. After expiry
   exactly one concurrent attempt is `would_probe`; all peers are
   `would_skip_busy`. Enforced skips continue the existing neighboring route
   without an extra provider request.
6. A successful or non-quota probe reopens. A quota-failed probe retrips using
   provider `Retry-After` when valid and the existing cooldown fallback
   otherwise.
7. The simulated lease remains owned until classified outcome, even though the
   real allocator lease may already have been released.
8. Token-count probes and attempts rejected before provider dispatch never
   mutate the gate.
9. Unknown/stale quota and runtime caps fail open and are visible. Stale leases
   recover after the hard maximum age.
10. Audit JSON contains state and numeric outcomes but no project ID,
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

## Observe and enforce review

Collect at least 100 edge-gate attempts spanning six hours, preferably a full
day. Review independently:

- Green / Guarded / Tripped / Half-open counts;
- `would_skip_busy` and `would_skip_tripped` frequency;
- successful actual attempts behind a counterfactual skip (fallback cost);
- quota failures behind a counterfactual skip (pressure the gate could avoid);
- quota failures on dispatch/probe and resulting trip transitions;
- exact-one-probe and reopen behavior;
- queue/disk loss, runtime saturation, mode-correct routing changes and the
  invariant zero additional provider requests;
- every enforced skip is `not_dispatched` and never appears as a provider
  attempt;
- switching the config during an in-flight request does not relabel the
  immutable attempt or its audit record.

`ready_for_review` remains evidence, not an automatic mode switch. Roll out in
`observe`, then set explicit `enforce`; returning to `observe` is the immediate
rollback and 0.8.11 remains the stable binary rollback.
