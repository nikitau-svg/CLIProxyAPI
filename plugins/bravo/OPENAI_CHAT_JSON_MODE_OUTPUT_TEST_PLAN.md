# Bravo 0.8.10 OpenAI Chat JSON-mode output test plan

## Contract

- OpenAI Chat `response_format: {"type":"json_object"}` enables actual JSON
  output behavior, not only routing compatibility.
- Codex keeps its native JSON object mode.
- Claude receives a final system instruction to return exactly one bare JSON
  object without Markdown or prose.
- For non-streaming OpenAI Chat responses, Bravo removes an outer `json` or
  unlabelled Markdown fence only when its complete contents parse as one JSON
  object.
- Bravo never extracts JSON from prose, arrays, malformed JSON, partial fences,
  nested content, tool calls, or requests that did not ask for JSON object mode.
- Streaming remains streaming: Bravo does not buffer the complete SSE response;
  the Claude instruction is the JSON-mode enforcement for that path.

## Automated gates

1. Verify Claude receives the instruction in streaming and non-streaming mode.
2. Normalize fenced valid objects for Claude and Codex responses.
3. Preserve bare objects byte-for-byte at the content-string level.
4. Preserve arrays, malformed JSON, prose, non-JSON fences, and ordinary text.
5. Execute a full Bravo request whose host returns fenced JSON and assert the
   client receives a bare object.
6. Run full Bravo and translator suites, focused race, and `git diff --check`.

## Canary and production

Use an isolated Mac mini canary with copied state. Send the Mem0-shaped request
with `top_p: 0.1` and `response_format=json_object` through both Claude-first
and Codex-first logical routes. Parse `choices[0].message.content` as a JSON
object and reject Markdown fences. After a green canary, publish the branch,
deploy only the tested image, verify health/version/restart count, retain the
previous image and compose file for rollback, then remove the stopped canary.
