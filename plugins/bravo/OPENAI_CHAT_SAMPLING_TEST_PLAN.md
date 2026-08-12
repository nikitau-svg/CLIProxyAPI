# Bravo 0.8.9 OpenAI Chat sampling compatibility test plan

## Contract

- OpenAI Chat `top_p` and `temperature` are advisory controls.
- Claude Code translation omits both fields rather than returning a provider
  400 for `top_p < 1` or for `temperature` together with `top_p`.
- Codex behavior is unchanged: its translator already omits unsupported
  sampling controls.
- Bravo retains the original OpenAI request for each candidate, so dropping a
  field in the Claude translation cannot mutate a later Codex fallback.
- Model choice, project authorization, allowed subscription pools, retries,
  and response formatting are unchanged.

## Automated gates

1. Translate `top_p: 0.1`, `top_p: 1`, `temperature: 0.1`, and the combined
   `temperature: 0.1` + `top_p: 0.1` request to Claude.
2. Assert neither sampling field reaches Claude and the user message survives.
3. Run the complete Claude/Codex translator and Bravo test suites.
4. Run focused race tests and `git diff --check`.

## Canary

Use an isolated Mac mini container and copied configuration/state. Submit all
four OpenAI Chat variants to `bravo/sonnet`, plus the Mem0-shaped request with
`response_format: {"type":"json_object"}`. Every request must return HTTP 200;
no trace may contain `bravo_capability_undeclared` or an upstream sampling 400.
Restore the copied config and stop the canary before production deployment.
