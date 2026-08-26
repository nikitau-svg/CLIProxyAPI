# Adaptive edge gate v4 test plan

Status: Bravo 0.9.0-preview.8 shadow-only. The gate has no routing authority.

## Contract

1. `off` and `observe` keep the same attempts, order, provider-call count,
   responses and quota-poller cadence.
2. Green does not serialize requests.
3. Guarded allows one simulated in-flight attempt per subscription. A second
   attempt returns `would_skip_busy` immediately; no condition variable,
   channel wait or retry loop is allowed.
4. Only a real quota/rate-limit outcome trips a breaker. Account-wide outcomes
   cover the subscription; explicit model scope covers only that physical
   model.
5. Before expiry a matching attempt is `would_skip_tripped`. After expiry
   exactly one concurrent attempt is `would_probe`; all peers are
   `would_skip_busy`.
6. A successful or non-quota probe reopens. A quota-failed probe retrips using
   provider `Retry-After` when valid and the existing cooldown fallback
   otherwise.
7. The simulated lease remains owned until classified outcome, even though the
   real allocator lease may already have been released.
8. Token-count probes and attempts rejected before provider dispatch never
   mutate the gate.
9. Runtime caps fail open and are visible. Stale leases recover after the hard
   maximum age.
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

## Shadow review

Collect at least 100 edge-gate attempts spanning six hours, preferably a full
day. Review independently:

- Green / Guarded / Tripped / Half-open counts;
- `would_skip_busy` and `would_skip_tripped` frequency;
- successful actual attempts behind a counterfactual skip (fallback cost);
- quota failures behind a counterfactual skip (pressure the gate could avoid);
- quota failures on dispatch/probe and resulting trip transitions;
- exact-one-probe and reopen behavior;
- queue/disk loss, runtime saturation and the invariant zero routing changes /
  zero additional provider requests.

Promotion is a separate reviewed release. `ready_for_review` is evidence, not
an automatic switch.
