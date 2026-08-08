# Bravo adaptive quota control test plan

Status: required release gate for `ADAPTIVE_QUOTA_CONTROL_CONTRACT.md`.

The test suite uses a fake clock, deterministic quota provider, deterministic
token counts and a controllable executor. No test calls real Anthropic/OpenAI
quota endpoints. Production prompts, credentials, emails and request IDs must
not enter fixtures.

## 1. Release sequence

The implementation follows this order without exceptions:

1. keep this contract and test plan reviewed alongside the existing quota
   contract;
2. add failing deterministic tests, including the exact production incident;
3. implement without changing unrelated routing contracts;
4. run focused tests and the full repository suite;
5. build an isolated canary on a non-production port and copied synthetic state;
6. inspect canary traces, latency and observed quota slip;
7. only if every gate passes, commit intentionally and publish a PR to `stable`;
8. deploy one approved image only after explicit user approval;
9. run production smoke checks, retain rollback, then clean obsolete build caches.

## 2. Deterministic harness

The harness must control:

- session, weekly and model-weekly quota windows and reset times;
- quota observation start/end watermarks, delays, failures and out-of-order
  responses;
- provider acceptance before success/error/transport ambiguity;
- model, context size, input/output/cache/reasoning tokens and effort;
- concurrent start barriers so lease races are repeatable;
- per-project primary and allowed subscription pools;
- stream failure before and after first committed content;
- project traffic at 1/10/60-minute rates;
- restart between accepted work and confirmed reconciliation.

Every assertion reads safe exported state or a test-only snapshot under lock; it
must not depend on sleeps or wall-clock timing.

## 3. Incident regression (mandatory first test)

### AQC-I01: exact x5 109-request slip

Fixture:

```text
role: secondary
tariff: x5
session floor: 20%
weekly floor: safely above session constraint
provider polling interval: 600s
legacy reservation: 0.1% per request
initial confirmed session remaining: incident-compatible value above floor
traffic: 109 successful requests inside one polling interval
model mix: the recorded long/high-effort Claude workload
provider-reported final session remaining: 2%, then 0% in the legacy replay
```

Assertions:

1. A legacy-control replay demonstrates that `109 * 0.1%` admits the burst and
   crosses the 20-percent floor. This proves the fixture is meaningful.
2. Adaptive observe mode emits the point at which enforcement would stop using
   the secondary and names the safer fallback.
3. Adaptive enforce mode atomically stops issuing leases before its conservative
   post-admission bound reaches 20 percent.
4. Remaining requests continue through a compatible secondary/provider fallback;
   callers see successful responses and no limiter-induced sleep.
5. No admitted secondary attempt is missing from `in-flight` or `pending` before
   the confirmed reconciliation watermark includes it.
6. In the calibrated replay, provider-observed remaining never falls more than
   one percentage point below the configured floor. If the fallback pool is
   exhausted, Bravo returns the typed protected-floor error instead of consuming
   to 2/0 percent.

## 4. Unit tests

| ID | Scenario | Required assertion |
| --- | --- | --- |
| AQC-U01 | Green secondary, fresh LKG | admitted locally; no refresh call, wait or queue |
| AQC-U02 | Primary below configured floor | admitted while both confirmed windows remain positive |
| AQC-U03 | Primary confirmed zero session | rejected for that credential and compatible fallback evaluated |
| AQC-U04 | Secondary at exact floor | rejected because post-admission value must be strictly above floor |
| AQC-U05 | Session safe, weekly unsafe | weekly floor rejects |
| AQC-U06 | Weekly safe, session unsafe | session floor rejects |
| AQC-U07 | Model-weekly limit | matching reviewed model window controls admission |
| AQC-U08 | Larger context | reservation is not smaller than same model/effort with smaller context |
| AQC-U09 | Higher effort | reservation is not smaller than same model/context with lower effort |
| AQC-U10 | Expensive model family | cold-start Opus/Fable reservation exceeds cheap-family minimum |
| AQC-U11 | Sparse exact-model history | falls back to conservative family/provider estimate |
| AQC-U12 | Rich cheap history | learned estimate never falls below tariff minimum or configured lower confidence bound |
| AQC-U13 | Non-finite/corrupt estimator | conservative cold-start value; no admission widening |
| AQC-U14 | Snapshot age grows | age guard is monotonic until refresh/reset |
| AQC-U15 | Higher project burst | burst guard is monotonic and account-local |
| AQC-U16 | Unknown secondary in enforce mode | blocked and marked not floor-safe regardless of legacy allow default |
| AQC-U17 | Stale within maximum | LKG is used only after age/uncertainty guard subtraction |
| AQC-U18 | Expired secondary | protected and fallback evaluated |
| AQC-U19 | Primary stale/unknown | preserves reviewed primary behavior without claiming confirmed quota |
| AQC-U20 | Proven provider reset | estimator is not trained with negative consumption |
| AQC-U21 | Unproven quota increase | does not clear reservations or create capacity |
| AQC-U22 | Out-of-order snapshot | ignored for reconciliation and admission widening |
| AQC-U23 | Config reload raises floor | new admissions obey it; existing leases continue |
| AQC-U24 | Config reload lowers floor | new admissions may use newly safe surplus |
| AQC-U25 | Observe mode | computes identical decision/telemetry but does not enforce |
| AQC-U26 | `allocatorBypassPlan`, secondary below floor | produces no unmanaged attempt; another normally eligible route is evaluated |
| AQC-U27 | `allocatorBypassPlan`, no alternative | typed Russian protected-floor/no-fallback error; zero provider calls on the protected secondary |
| AQC-U28 | Same credential primary for this project | normal managed primary lease may spend below tariff floor toward confirmed zero |
| AQC-U29 | Adaptive ledger union reaches 4096 auth identities | a new identity fails before provider I/O; an existing identity can finalize and reconcile |
| AQC-U30 | Oversized legacy snapshot/WAL | bounded retained entries plus a durable saturation marker block every secondary and `/compact` after restart |
| AQC-U31 | Saturation recovery | authenticated explicit reconciliation is rejected while retained/runtime/in-flight debt exists and durably clears only after all preconditions hold |
| AQC-U32 | v3 state migration and v4 rollback guard | v3 loads as v4; an older v3 binary rejects the v4 schema rather than saving over adaptive debt |

## 5. Concurrency and lease tests

### AQC-C01: atomic last-headroom race

Start N goroutines on a barrier where only one dynamic reservation fits. Exactly
one lease is acquired; the others receive a local lease rejection and evaluate
fallback. Provider execution count for the protected credential is one.

### AQC-C02: 109 simultaneous requests

Start all 109 incident requests before releasing any provider response. Assert
that the sum of atomic in-flight reservations never exceeds safe surplus and no
race detector failure occurs.

### AQC-C03: accepted success

On provider success, the reservation moves once from in-flight to pending. A
double completion callback changes nothing.

### AQC-C04: proven pre-accept failure

Invalid JSON, contract/capability validation and deterministic request rewrite
fail before lease acquisition: zero provider calls and zero pending/prepared
cost. Under the 0.8.3 ABI there is no generic proven-rejection signal after the
host callback boundary; such errors are covered by C05 until the core exposes an
explicit typed `accepted=false` marker.

### AQC-C05: ambiguous transport failure

The first credential retains pending cost. Fallback admission includes its own
lease. Neither lease is transferred or double-counted.

### AQC-C06: refresh watermark race

Begin quota acquisition, accept request A, complete quota acquisition, then
accept request B. Clear only reservations the provider observation proves were
included. Repeat with request acceptance immediately before/after the captured
watermark.

### AQC-C07: refresh failure

Timeout/429/5xx clears no in-flight or pending cost and expands age uncertainty.

### AQC-C08: restart with pending work

Persist accepted work, restart before refresh and assert the restored guard is at
least as conservative as pre-restart admission. One confirmed snapshot reconciles
only covered work.

### AQC-C09: configure publish/restore admission gate

Pause configuration after publishing a destination snapshot but before runtime
restore. A new acquire/finalize must remain blocked. Test both an empty
destination and a destination with pending debt that would reject the request.

### AQC-C10: WAL ordering and bounded async finalization

Block the finalize writer. Finalize returns without waiting for fsync, but the
next prepare cannot return until the queued absolute finalize record is ordered
and synced. Flood async finalization and assert the queue stays within its hard
bound and creates no waiter goroutine per request.

### AQC-C11: state path switching

While a source has pending, prepared, or saturated debt, A→B and repeated
A→B→A switching fail closed. Proven rejection clears the prepared lease and
then permits switching. Commit, rejection and crash/restart never add the same
scalar debt twice.

### AQC-C12: bounded WAL and maximum checkpoint delay

Run continuous prepare/finalize traffic with tiny test limits. The WAL never
exceeds its byte or record cap, repeated debounce activity cannot postpone the
checkpoint beyond the maximum delay, and restart restores exact conservative
debt. Inject checkpoint/compaction failure at the cap: a new prepare fails
before provider I/O, async accepted work remains conservatively represented,
and the on-disk WAL remains within its hard bound.

### AQC-C13: checkpoint I/O does not block provider admission

Pause periodic snapshot I/O after capture but before filesystem writes. Four
concurrent requests must still acquire their durable leases and reach the
provider-call boundary. Release them while I/O remains paused, complete the
checkpoint, restart, and prove revision-aware WAL compaction preserved every
post-capture transition without phantom debt.

Run all concurrency cases with `go test -race`.

## 6. Estimator and reconciliation tests

| ID | Scenario | Required assertion |
| --- | --- | --- |
| AQC-E01 | One confirmed interval, homogeneous requests | learned estimate moves toward observed cost but remains upper-confidence |
| AQC-E02 | Mixed cheap/expensive requests | delta is weighted; it is not divided equally by request count |
| AQC-E03 | Sudden expensive workload after cheap history | cold/model guard prevents immediate under-reservation |
| AQC-E04 | Underprediction | error/uncertainty guard increases on the next decision |
| AQC-E05 | Repeated accurate intervals | guard decays gradually, never in one sample |
| AQC-E06 | Unattributed external/provider usage | unattributed error increases credential guard rather than being ignored |
| AQC-E07 | Browser/CLI use outside Bravo | observed unexplained burn cannot train a project as if it caused exact cost |
| AQC-E08 | Cached versus uncached prompt | feature buckets remain distinct when provider evidence supports a difference |
| AQC-E09 | Session and weekly deltas differ | independent estimators/guards are retained |
| AQC-E10 | Reset during in-flight work | reset generation prevents pre-reset work corrupting post-reset learning |

Property tests generate arbitrary valid request sequences and assert:

```text
dynamic reservation is finite and positive
safe_remaining never increases from a new admission
adding in-flight/pending work never changes reject -> admit
increasing snapshot age never changes reject -> admit without new evidence
raising a floor never changes reject -> admit
primary permission never changes a secondary decision for another project
```

## 7. Project-tempo and lending tests

### AQC-P01: idle owner, active borrower

An active project borrows safe surplus at full green-path throughput. There is no
fixed artificial per-project rate cap and the secondary floor remains protected.

### AQC-P02: owner becomes active

After owner traffic starts, its 1-minute forecast increases the demand guard.
New borrower attempts move smoothly to fallback; accepted borrower work is not
cancelled.

### AQC-P03: short pause does not erase ownership demand

The 10/60-minute windows prevent a brief owner pause from releasing all guarded
capacity.

### AQC-P04: inactive project decay

Demand guard decays after configured inactivity so idle capacity eventually
becomes borrowable; it cannot remain pinned forever.

### AQC-P05: multiple owners

Owner guards aggregate conservatively without double-counting shared attempts.

### AQC-P06: unrelated account isolation

An amber/red Claude credential does not reduce concurrency or add delay to a
green Claude credential on the same direct egress, except for independently
reviewed provider cooldown behavior.

### AQC-P07: project pool isolation

Fallback uses only the project's `allowed_auth_ids`. A safe but disallowed
credential is never selected.

### AQC-P08: primary differs by project

The same credential may be primary for project A and secondary for project B.
Project A may spend below the tariff floor; project B is rejected there.

## 8. Near-floor and failover tests

| ID | Scenario | Required assertion |
| --- | --- | --- |
| AQC-F01 | Green -> amber | hysteretic transition, async refresh wake, no request-path wait |
| AQC-F02 | Amber with safe fallback | fallback selected immediately; user request succeeds |
| AQC-F03 | Amber without fallback but reservation fits | request admitted; credential-local bound remains safe |
| AQC-F04 | Red with cross-provider fallback | compatible provider succeeds and trace explains the protected floor |
| AQC-F05 | Red, fallback lacks tools/vision/reasoning | incompatible candidate skipped; no contract degradation |
| AQC-F06 | Red, fallback context too small | typed no-compatible-fallback error; no truncation or compact |
| AQC-F07 | Failure before stream content | independent fallback lease may be attempted |
| AQC-F08 | Failure after stream content | existing stream commit rule prevents replay; reservation remains conservative |
| AQC-F09 | Retryable first provider error | uncertain first cost remains while fallback lease is acquired |
| AQC-F10 | All secondaries protected, primary positive | primary remains usable to zero according to its project role |
| AQC-F11 | All permitted routes unavailable | Russian error distinguishes floor protection, quota age and compatibility reasons |
| AQC-F12 | Threshold jitter | hysteresis prevents green/amber/red flapping across alternating snapshots |
| AQC-F13 | Only healthy/capable auth is a protected secondary | health does not bypass budget policy; informative error, no provider call |

## 9. Polling and load-safety tests

- 10,000 green admissions produce no quota callback and no scheduler wake per
  request.
- A dirty usage record does not bypass the configured provider polling interval.
- Many amber decisions coalesce into one background wake/single-flight refresh.
- Refresh concurrency, egress cooldown and minimum interval continue to satisfy
  `QUOTA_REFRESH_CONTRACT.md`.
- A slow or hung quota endpoint has zero dependency path to inference selection
  latency.
- One provider's refresh 429 does not become a model execution rate-limit error.
- Near-floor account-local control does not create provider-wide inference
  cooldowns.

## 10. Performance gates

Run a benchmark using the same candidate/auth cardinality before and after the
change:

- green-path allocator-compute p95 <= 2 ms at realistic project, credential and
  candidate cardinality;
- local durable `prepared` barrier p95 <= 15 ms, with no provider/quota network
  dependency and no fixed batching sleep;
- green-path throughput regression <= 5 percent;
- no allocator mutex held across provider I/O, WAL sync, snapshot persistence or
  estimator batch processing;
- 109-way incident burst completes routing without deadlock or unbounded queue;
- estimator state and pending ledger remain bounded under a 24-hour synthetic
  agent workload;
- race detector, goroutine and heap profiles show no leak after repeated
  configure/shutdown cycles.

## 11. Management and privacy tests

Management fixtures verify that each subscription exposes:

- confirmed remaining, age and reset;
- configured floor and effective cutoff;
- green/amber/red state with Russian explanation;
- in-flight/pending counts and rounded reservation totals;
- dynamic estimate and age/burst/uncertainty/demand guards;
- 1/10/60-minute project tempo;
- next quota refresh independently from execution cooldown.

Selectable-period floor-slip and estimator-error history belongs to the
follow-up analytics phase. The 0.8.3 release gate verifies current counters,
guards, decision traces and CSV/API privacy, without claiming a historical
percentile store that this build does not implement.

Route traces must contain stable adaptive decision codes and before/after safe
headroom. A sentinel scan fails if output contains a plaintext API/smart key,
OAuth token, Authorization/Cookie header, raw prompt, full auth-file contents or
an unredacted provider response.

## 12. Isolated canary scenarios

The canary uses copied configuration, synthetic keys, synthetic quota callbacks,
a separate state directory and a non-production port. It must not poll or mutate
production credentials.

Required scenarios:

1. Baseline green Claude and Codex traffic, including tools and streaming.
2. Exact 109-request x5 incident replay with a 20-percent floor and 600-second
   virtual polling interval.
3. Long/high-effort burst with concurrent subagent-shaped requests.
4. Smooth secondary Claude -> secondary Claude -> Codex failover near floor.
5. Primary spend below floor to positive near-zero, then confirmed-zero fallback.
6. Stale snapshot plus quota-refresh timeout while inference remains responsive.
7. Owner project becomes active while another project borrows surplus.
8. Context/tool/reasoning-incompatible fallback remains fail-closed.
9. Restart with pending accepted requests and later confirmed reconciliation.
10. Management UI verification in Google Chrome/Playwright at desktop and narrow
    widths: modes, guards and Russian reasons remain understandable.

Observe mode is a routing-decision shadow, not an estimator- or project-demand-
training source in 0.8.3. Before enforce is enabled, repeat the incident,
restart, concurrency and owner-demand scenarios with the isolated canary
actually in `enforce`; do not promote observe calibration into enforce state.

## 13. Canary acceptance report

The report must include, without secrets:

- admitted/rejected/failover counts per project and credential role;
- minimum provider-confirmed secondary remaining versus configured floor;
- maximum observed slip (target <= 1 percentage point in the incident replay);
- estimator predicted versus observed error percentiles;
- green-path and near-floor routing latency percentiles;
- quota callback count and minimum observed cadence;
- peak in-flight/pending reservation totals;
- all non-success trace reason codes;
- before/after CPU, heap and goroutine measurements.

Any secondary reaching 2 or 0 percent in the incident replay, any hidden contract
degradation, any per-request quota polling, or any unexplained request stall is a
release blocker.

## 14. Repository and release gates

Before GitHub publication:

```text
focused adaptive allocator tests
existing Bravo quota/allocator/failover tests
go test -race for changed packages
go test ./...
server compile
Bravo plugin build
dirty-worktree and generated-artifact audit
```

The PR targets `stable` and links this contract, incident regression and canary
report. Production is not restarted during development. After explicit approval,
replace the production container once, run both Claude- and OpenAI-protocol smoke
tests, verify existing projects/routes/analytics, retain the previous image for
rollback, and remove only obsolete Bravo build caches after health is confirmed.
