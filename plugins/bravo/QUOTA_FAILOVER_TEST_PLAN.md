# Bravo 0.7.7 Claude Extra-Usage Failover Test Plan

## Scope

Fix Anthropic's HTTP 400 `invalid_request_error`:

`Third-party apps now draw from your extra usage, not your plan limits.`

Bravo must classify this reviewed message as account-wide subscription quota
exhaustion and continue the same client request through the next eligible
subscription/provider. Unrelated HTTP 400 errors must remain terminal.

This release does not change route priorities, project pools, allocator reserve
floors, capability contracts, or primary-subscription ordering.

## Red phase

Before changing the classifier, run the focused regression set and confirm:

- host execution classification is terminal instead of retryable;
- the pre-commit streaming path is terminal instead of retryable;
- the full Claude-to-Codex execution stops after Claude;
- the same Claude credential remains eligible for another physical model.

The four failures reproduce the Maria-OpenClaw production incident.

Before publishing the reviewed cooldown change, use a three-candidate route
(`Claude Sonnet -> Claude Haiku on the same credential -> Codex`) with
`max_attempts: 2` and confirm three additional red failures:

- regular execution stops after the cooled Claude candidate consumes the
  second pre-built plan slot;
- token counting stops the same way;
- streaming closes with the Claude quota error instead of reaching Codex.

These failures prove that `max_attempts` must cap actual provider calls, not
the number of candidates placed into a plan. A candidate skipped because it
entered cooldown after planning is not a provider call and cannot spend the
budget.

## Automated regression coverage

- Legacy Anthropic `out of extra usage` wording remains retryable.
- The verbatim new third-party extra-usage wording is retryable from:
  - host execution errors;
  - HTTP response bodies;
  - stream errors received before the first client-visible frame.
- A generic usage-limit code followed by the exact account-wide Anthropic
  message is aggregated as account-wide rather than returning early with a
  model-scoped cooldown.
- The normalized error code is
  `bravo_subscription_quota_exhausted`.
- A Claude failure is followed by a successful Codex call in the same Bravo
  execution, with two ordered attempt records.
- Regular execution, token counting, and pre-commit streaming all skip another
  model on the newly cooled Claude credential and still reach Codex.
- `max_attempts` remains a hard provider-call budget in all three paths;
  cooldown skips, contract skips, and unavailable leases do not consume it.
- A three-provider retryable failure probe confirms that the third real
  upstream call is blocked when `max_attempts: 2` in execute, count, and stream.
- The quota cooldown suppresses other Claude models on the same credential but
  does not suppress another Claude credential.
- Authentication failures remain account-wide.
- HTTP 429, upstream faults, and model-entitlement failures keep their existing
  model scope.
- Malformed requests, invalid schemas, and unknown tool choices remain
  terminal and never reach a fallback provider.

## Required suites

Run from the nested Bravo Go module:

- focused quota/fallback tests;
- `go test ./... -count=1`;
- `go test -race ./... -count=1`;
- `go vet ./...`;
- `go build ./...`.

Run the repository-wide Go tests and build from the root module before
publishing.

Run heavyweight suites sequentially and require at least 10 GiB free before
each build step. Stop instead of building when the guard fails; remove only
exact task-owned caches and never run a shared Docker prune.

## Zero-credential canary

Build the exact candidate source into an isolated Linux/arm64 image. Start it
on a free loopback port with disposable state and two fake config credentials:

- fake Claude upstream returns the verbatim Anthropic HTTP 400 body;
- fake Codex upstream returns a valid Responses stream.

Create a disposable Bravo project containing only those fake credentials and
install an explicit three-candidate Sonnet route
(`Claude Sonnet -> Claude Haiku -> Codex`). Send a streaming Anthropic Messages
request to `bravo/sonnet`. Assert:

- downstream status is 200;
- the response completes with the Codex mock payload;
- events contain one retryable Claude quota failure followed by one successful
  Codex attempt;
- the already-built Claude Haiku attempt on the same credential is skipped
  without another provider call;
- a second request to a different Claude-first logical model skips the same
  cooled-down Claude credential;
- the exact upstream sequence is Claude Sonnet, Codex Terra, Codex Luna, with
  no Claude Haiku call;
- no real subscription, production key, state file, port, or container is
  used.

## Production and cleanup gates

Only after every automated and canary check is green:

1. Commit and publish the exact tested source to GitHub.
2. Build or identify the image from that published commit.
3. Perform one short production cutover.
4. Verify health, plugin version, project count, and a read-only protocol smoke.
5. Remove the disposable canary, mock, temporary build context, superseded
   candidate images, and task-owned Docker build cache without pruning any
   production image or volume.
