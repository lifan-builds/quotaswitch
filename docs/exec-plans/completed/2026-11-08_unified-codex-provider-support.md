# Unified OpenAI Codex provider support

| Field | Value |
|---|---|
| Status | completed |
| Owner | lifan-builds |
| Date | 2026-11-08 |
| Related | [`docs/adr/0003-unified-codex-provider-support.md`](../../adr/0003-unified-codex-provider-support.md) |

## Objective

Extend `agy-auth` into a unified multi-account switcher and background quota rotator supporting both Google Antigravity (`agy`) and OpenAI Codex (`codex`), with full compatibility for `~/.codex-switcher/accounts.json` and `~/.codex/auth.json`.

## Context

Managing Antigravity and Codex accounts separately required running both `agy-auth` and the Tauri-based `codex-switcher` desktop app. Unifying Codex support directly into `agy-auth` allows a single CLI and LaunchAgent to inspect quota, switch credentials, reload desktop/IDE surfaces, and auto-continue interrupted conversations across both tools.

## Scope

### In scope

- `internal/codex` storage, OpenAI PKCE S256 OAuth, ChatGPT `wham/usage` client, process lifecycle, and rollout session recovery.
- `agy-auth codex <list|usage|switch|rotate|add|login|warmup|status>` subcommands.
- `--provider=agy|codex|all` flag on `agy-auth list` and `agy-auth rotate`.
- Repository harness AST lint coverage for `internal/codex`.

### Out of scope

- Tauri GUI components from `codex-switcher`.

## Expected paths

Informational only; this is not enforced by a task controller.

- `internal/codex/*`
- `internal/cli/cmd_codex.go`, `internal/cli/cmd_list.go`, `internal/cli/cmd_rotate.go`, `internal/cli/app.go`, `internal/cli/spec.go`
- `internal/harness/lint/lint.go`
- `scripts/install-autoswitch-service.sh`
- `docs/*`, `README.md`, `AGENTS.md`

## Risks

- Single-use OpenAI `refresh_token` invalidation if background quota polling refreshes the active account while Codex is running — mitigated by `SyncActiveFromLiveAuth` and skipping active refresh when Codex processes are running.
- Multi-workspace accounts sharing one email — mitigated by keying accounts on `(email, account_id)` and rendering workspace suffixes (`ws-<last6>`).

## Acceptance criteria

- [x] Reads and writes `~/.codex-switcher/accounts.json` (`auth_mode: "chat_g_p_t"` | `"api_key"`) and `~/.codex/auth.json` atomically (`0600`).
- [x] Supports `agy-auth codex list|usage|switch|rotate|add|login|warmup|status` with `--json` output.
- [x] Supports `agy-auth rotate --provider=all` and `agy-auth list --provider=all`.
- [x] Passes `make verify-full` and live smoke verification.

## Verification plan

```shell
make verify
make verify-full
```

Additional checks:

- Live read-only smoke test against `~/.codex-switcher/accounts.json` (`./bin/agy-auth codex status` and `./bin/agy-auth codex list --no-sync`).

## Rollback

Revert the feature commit and restart the LaunchAgent without `--provider=all`; `~/.codex-switcher/accounts.json` schema is unchanged.

## Decision log

| Date | Decision | Rationale |
|---|---|---|
| 2026-11-08 | Keep `"chat_g_p_t"` wire tag for `AuthModeChatGPT` | Matches Rust `#[serde(rename_all = "snake_case")]` on `AuthMode::ChatGPT` in `codex-switcher`. |
| 2026-11-08 | Default `--provider=agy` on CLI, `--provider=all` in LaunchAgent installer | Preserves backwards compatibility for existing CLI callers while enabling unified daemon monitoring out of the box. |

## Completion

- Commit/tag: `main` (2026-11-08)
- Verification result: `make verify-full` passed; live smoke check against 6 stored Codex accounts succeeded.
- Residual risk: Live `wham/usage` endpoint remains an undocumented ChatGPT web/Codex backend contract and may evolve upstream.
