# Bravo integrated reliability build: release test plan

Status: contract-first gate. Production deployment is forbidden until every
required canary case passes.

## Release contents

The integrated candidate combines:

- the pending 0.7.11 Russian error and controlled `/compact` work;
- typed core/provider errors and correct auth-state scope;
- context-window metadata and context-aware fallback;
- quota stale-while-revalidate and provider-friendly polling;
- persistent safe route traces and explicit latency metrics.

## Gate 1: contract review

The following documents must agree on field names, scopes and compatibility:

- `CORE_ERROR_CONTRACT.md`;
- `CONTEXT_ROUTING_CONTRACT.md`;
- `QUOTA_REFRESH_CONTRACT.md`;
- `OBSERVABILITY_CONTRACT.md`.

No implementation starts until contradictions are resolved in this directory.

## Gate 2: unit and package tests

Required packages:

```text
./sdk/cliproxy/providererror
./internal/runtime/executor
./sdk/cliproxy/auth
./sdk/api/handlers
./internal/pluginhost
./internal/logging
./plugins/bravo/go
```

Every defect found in production receives a fixture with the exact safe shape
of the provider response. Raw production credentials, prompts and request IDs
must not enter fixtures.

## Gate 3: repository verification

- `gofmt` on changed Go files;
- targeted tests for every changed package;
- `go test ./...`;
- server compile;
- Bravo plugin build;
- dirty-worktree audit so unrelated user changes are not committed.

## Gate 4: isolated canary

The canary uses copied configuration and synthetic project keys. It must not
bind the production port, write the production state directory or refresh all
production credentials in a burst.

Required scenarios:

1. Normal Claude streaming success.
2. Normal Codex streaming success.
3. Claude provider quota error -> another Claude credential -> Codex fallback.
4. Exact Anthropic `prompt is too long: N tokens > M maximum` error.
5. Context overflow where a verified larger candidate exists.
6. Context overflow where no compatible candidate exists.
7. Quota refresh 429 while a last-known-good snapshot exists.
8. Quota refresh timeout and later recovery.
9. Failure before first content and failure after committed content.
10. `/compact` reserve bypass and cooldown warning.
11. Restart and persisted trace recovery.
12. Sentinel privacy scan over every generated log and state file.

## Gate 5: GitHub

Only after the canary passes:

- create intentional commits grouped by contract boundary;
- push the candidate branch;
- open or update a PR targeting `stable`;
- wait for CI and resolve review findings;
- update AWS clean-install documentation and release notes.

## Gate 6: production

Production rollout requires an explicit user command after the canary and CI
results are reported.

- build/pull the approved commit;
- replace the production container once;
- run smoke tests through Claude and OpenAI protocols;
- verify existing projects, keys, routes, quotas and analytics;
- verify route trace privacy;
- clean only obsolete Bravo build images and caches after health is confirmed.

Rollback keeps the previous image and current state snapshot until the new
container has passed smoke tests.
