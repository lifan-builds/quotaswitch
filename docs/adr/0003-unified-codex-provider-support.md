# ADR-0003: Unified OpenAI Codex provider support

- **Status:** Accepted
- **Date:** 2026-11-08
- **Decision makers:** lifan-builds
- **Consulted:**
- **Related:** [`docs/exec-plans/completed/2026-11-08_unified-codex-provider-support.md`](../exec-plans/completed/2026-11-08_unified-codex-provider-support.md), [`docs/harness.md`](../harness.md)

## Context

Developers running both Google Antigravity (`agy`) and OpenAI Codex (`codex` CLI + `Codex.app`) previously needed two separate daemons/apps to monitor 5-hour and weekly rate limits, switch accounts, and recover interrupted sessions: `agy-auth` (Go CLI + LaunchAgent) for Antigravity, and `codex-switcher` (Tauri/Rust desktop app) for Codex.

OpenAI Codex stores its live session in `~/.codex/auth.json` using single-use rotating OAuth `refresh_token`s, while `Codex Switcher` stores saved multi-workspace accounts in `~/.codex-switcher/accounts.json` (with `serde` snake-case `auth_mode: "chat_g_p_t"` | `"api_key"`).

## Decision drivers

- Run a single lightweight Go CLI and background LaunchAgent (`agy-auth rotate --provider=all --watch`) for both Antigravity and Codex.
- Preserve 100% read/write compatibility with existing `~/.codex-switcher/accounts.json` and `~/.codex/auth.json` files without forcing re-login or data migration.
- Respect OpenAI's single-use `refresh_token` semantics so background quota checks never invalidate a running Codex session.
- Maintain all repository harness invariants (`0700`/`0600` permissions, `writeFileAtomic`, no direct stdout/stderr in library packages, test isolation).

## Considered options

- **Option A: Keep two separate repositories/binaries** — Maintain `codex-switcher` (Tauri) and `agy-auth` separately. Rejected due to duplicated daemon management, heavier desktop memory footprint, and fragmented CLI UX.
- **Option B: Import Codex accounts into `agy-auth`'s encrypted vault with a custom format** — Breaks compatibility with `Codex Switcher.app` and diverges from `~/.codex-switcher/accounts.json`.
- **Option C: Add `internal/codex` and `agy-auth codex` + `--provider=agy|codex|all` with native schema compatibility** — Directly read/write `~/.codex-switcher/accounts.json` and `~/.codex/auth.json` atomically, reconcile rotated live tokens before switching, and expose unified CLI commands.

## Decision

Adopt **Option C**:
1. Add `internal/codex` implementing schema-compatible `Store` (`~/.codex-switcher/accounts.json` and `~/.codex/auth.json`), PKCE S256 login (`app_EMoamEEZ73f0CkXaXp7hrann`), `wham/usage` quota inspection, process management (`Codex.app`, CLI, IDE `app-server`), and interrupted rollout session continuation (`codex queue` + `codex://threads/<id>`).
2. Add `agy-auth codex <list|usage|switch|rotate|add|login|warmup|status>` and extend `agy-auth list` and `agy-auth rotate` with `--provider=agy|codex|all` (defaulting to `agy` for backwards compatibility, and `all` in `scripts/install-autoswitch-service.sh`).
3. Enforce `internal/codex` in `internal/harness/lint` (`output-boundary`, `atomic-writes`, `file-permissions`, `architecture`, and `test-isolation`).

## Consequences

- **Good:** Zero-migration compatibility with existing `~/.codex-switcher/accounts.json` accounts; one 30s LaunchAgent monitors and auto-rotates both Antigravity and Codex; multi-workspace accounts sharing an email are cleanly disambiguated.
- **Neutral:** `agy-auth rotate` keeps `--provider=agy` as the CLI default so existing scripts stay unchanged while `--provider=all` opts into unified rotation.
- **Bad:** None identified.

## Confirmation

- Unit and integration tests in `internal/codex/codex_test.go` and `internal/cli/codex_cli_test.go`.
- Harness enforcement via `make verify-full`.
