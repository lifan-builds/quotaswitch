# Repair GPT failures in existing Desktop conversations

| Field | Value |
|---|---|
| Status | completed |
| Owner | lifan-builds |
| Date | 2026-11-08 |
| Related | [Earlier gateway fixes](../completed/2026-11-08-gateway-gpt-compatibility.md) |

## Objective

Verify and repair the user's existing Antigravity Desktop chat, which still fails despite fresh CLI/ACP GPT tests passing.

## Evidence and scope

Desktop sends GPT-6 Astra a roughly 334 kB request with 123 messages and 17 tools. Five accounts return HTTP 200 then fail before output; the final account returns 429, masking the earlier failures. Tiny Astra and Sol prompts work. Add request metadata diagnostics, identify and correct the incompatible history translation, and preserve the actual failure when failover cannot recover. Do not change the user's model or discard conversation history.

## Expected paths

`internal/gateway`, `internal/cli/cmd_gateway.go`, gateway documentation and regression tests.

## Risks and rollback

Avoid logging tokens or request contents. Do not replay emitted tool calls. Restore the previously backed-up binary if needed; no account-store migration.

## Acceptance and verification

- Reproduce the user's Desktop failure and identify its upstream cause.
- Add a regression for the cause and for misleading final errors.
- Run `env -u NO_COLOR make verify-full`.
- Install locally and verify an answer in the same Desktop conversation.

## Completion

The upstream error was `No tool output found for function call`: interrupted tool calls in the existing Antigravity history had no matching results. The Responses translation now inserts an explicit unknown-outcome result for each missing result, preserving real results and leaving saved conversation history unchanged. Tool-history validation errors now return directly instead of rotating through accounts and masking the cause with a final quota error. Gateway diagnostics record request metadata and attempt failures without tokens or prompt bodies.

Regression tests cover missing and existing results, empty results, and history errors followed by an account with exhausted quota. `env -u NO_COLOR make verify-full` passed. Installed the updated binary in `/opt/homebrew/bin/quotaswitch` and restarted the gateway LaunchAgent.

At 20:54 Pacific on November 8, the same Desktop conversation retried successfully with GPT-6 Astra. Its transcript recorded `DESKTOP_GPT_OK`; refreshing the view then visibly showed that answer, removed the current error banner, and retained GPT-6 Astra in the model picker. Changes remain uncommitted.
