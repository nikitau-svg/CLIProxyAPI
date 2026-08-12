# Bravo 0.8.8 OpenAI Chat JSON object test plan

## Contract

- `response_format: {"type":"json_object"}` is accepted on OpenAI Chat
  requests for both Claude and Codex candidates.
- Codex receives `text.format.type: "json_object"`.
- Claude receives a valid translated request without the unsupported OpenAI
  field; JSON object mode is advisory on that fallback.
- `json_schema` remains a strict `structured_output` capability and must still
  fail closed when it is not explicitly verified.
- Routing order, allowed subscription pools, retries, and fallback behavior do
  not change.

## Automated gates

1. Detect `json_object` as an ordinary text contract.
2. Preflight both Claude and Codex with their normal text capability sets.
3. Keep `json_schema` classified as `structured_output`.
4. Verify the Codex translator emits native JSON object mode.
5. Verify the Claude translator safely ignores the OpenAI-only hint.
6. Execute a full Claude-to-Codex fallback while preserving the original
   `response_format` at both host attempts.
7. Run the complete Bravo and translator test suites plus focused race tests.

## Canary

Use an isolated container and copied configuration/state. Send the same
Hermes-shaped OpenAI Chat request with `json_object` to a Claude-first logical
route and a Codex-first logical route. Both must return HTTP 200; no attempt may
end as `bravo_capability_undeclared`. Production remains unchanged until the
canary is green and deployment is explicitly approved.
