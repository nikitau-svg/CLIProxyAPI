# Bravo 0.8.7 OpenAI Chat vision test plan

Status: implementation and isolated-canary gate.

Scope: Bravo 0.8.6 plus the narrow OpenAI Chat vision contract for Claude and
Codex. OpenAI Responses vision, files/documents, image generation, and adaptive
quota routing are out of scope.

## Contract tests

1. Detect `messages[].content[].type=image_url` as `vision`.
2. Admit OpenAI Chat vision for both Claude and Codex candidates that declare
   `vision`.
3. Continue to reject OpenAI Responses `input_image` as unverified.
4. Continue to reject file/document inputs independently of vision.

## Execution tests

1. Preserve the complete `image_url` data URL and detail field in the nested
   host request sent to Claude.
2. On an opaque candidate-local Claude 400, preserve the same image input in
   the Codex fallback request and return the Codex response.
3. Keep precise request-contract failures terminal.
4. Make at most one physical provider call per route attempt and never escape
   the project's allowed credential pool.

## Canary acceptance

1. Use an isolated 0.8.7 container and copied configuration/state.
2. Send a tiny PNG through `/v1/chat/completions` as `bravo/sonnet` using the
   Maria-OpenClaw project contract.
3. Verify the route reaches a physical provider instead of returning local
   `bravo_contract_unverified`.
4. Verify a forced opaque Claude 400 falls back to Codex with the image intact.
5. Verify OpenAI Responses vision remains rejected before provider I/O.
6. Confirm the production 0.8.6 container is untouched during canary tests.
