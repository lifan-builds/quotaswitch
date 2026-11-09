# Restore GPT use through the Antigravity gateway

| Field | Value |
|---|---|
| Status | completed |
| Owner | lifan-builds |
| Date | 2026-11-08 |
| Related | [Gateway ADR](../../adr/0005-local-llm-gateway.md) |

## Objective

Make the existing local gateway usable from Antigravity and report upstream failures accurately.

## Context

The gateway is healthy and a direct GPT streaming request succeeds. The installed CLI rejects the GPT model because it does not load the Desktop admin model configuration. Desktop logs show inline models loaded and earlier CloudCode 404 errors. UI actions currently fail with `noWindowsAvailable`, so Desktop selection is not yet verified. Code review also found that failed or truncated Codex SSE streams are converted to successful empty replies.

## Scope and expected paths

- Investigate installed CLI/Desktop model configuration and repair the supported settings integration in `internal/gateway` and `internal/cli` where necessary.
- Handle Codex stream failures and add regression coverage in `internal/gateway`.
- Update gateway instructions in `docs/user-guide.md` with verified behavior.
- Preserve existing uncommitted gateway work and account credentials; do not commit or publish.

## Risks

Private upstream protocols may differ between the installed CLI and Desktop versions. Retrying after streamed output could duplicate tool calls, so failover is permitted only before output is committed.

## Acceptance criteria

- [x] Reproduce and identify the model-selection failure.
- [x] Verify GPT text and a tool round trip through the supported Antigravity path.
- [x] Failed and interrupted upstream streams cannot appear as normal success.
- [x] Run `make verify-full` and install the verified local build.

## Verification plan

Focused gateway regression tests, `make verify-full`, and harmless live diagnostic prompts without file edits.

## Rollback

Restore the backed-up binary/configuration if the local installation regresses. Keep account stores unchanged.

## Decision log

| Date | Decision | Rationale |
|---|---|---|
| 2026-11-08 | Test gateway and client independently | Direct GPT success does not establish that Antigravity selects or renders it correctly. |

## Completion

- Installed the verified build to `/opt/homebrew/bin/quotaswitch`; restarted the gateway and autoswitch LaunchAgents. Rollback binary: `.local/gateway-gpt-fix/quotaswitch.before`.
- `env -u NO_COLOR make verify-full` passed (static checks, unit/race tests, current npm build, shim smoke test, package dry run). An initial run exposed an existing color test's dependency on the inherited `NO_COLOR` environment; no tests were skipped.
- Live gateway GPT text and tool round trip passed. Live agy ACP GPT text and completed MCP echo round trip passed.
- Normal `agy --model gpt-6.1-sol --print ...` with gateway environment overrides and global customizations returned `SUCCESS`, `AGY_GPT_OK`, one turn in 2.57 seconds.
- The normal CLI test initially stalled before inference: `/Users/lfan/Project/agent/scripts/fmu/cli.py` ignored Antigravity's pre-initialize `server/discover`. Added the JSON-RPC unknown-method response and a handshake regression test in that owning project; its unittest passed. No global MCP config was changed.
- Desktop backend reloaded successfully and loaded inline admin models. Multiple watcher cycles report `gateway_managed` and preserve selection; Codex desktop quota rotation still works.
- No commit or publication. Desktop click-through verification was unavailable because native UI actions returned `noWindowsAvailable`; inference was verified through normal CLI and ACP. The user should reselect GPT once if the previous watcher left Gemini selected.

