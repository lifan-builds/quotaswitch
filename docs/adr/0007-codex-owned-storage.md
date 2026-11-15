# ADR-0007: Quotaswitch-owned Codex account storage

- **Status:** Accepted
- **Date:** 2026-11-14
- **Decision makers:** lifan-builds
- **Related:** [execution plan](../exec-plans/completed/2026-11-14-codex-owned-storage.md), [ADR-0003](0003-unified-codex-provider-support.md)

## Context

Codex support reused Codex Switcher's account schema and directory. Quotaswitch now covers account login, quotas, switching, and recovery independently, so new installations should not require a directory owned by the retired desktop app. Existing accounts and single-use refresh tokens must remain usable.

## Decision

Use `<os.UserConfigDir()/quotaswitch>/codex/accounts.json` for new installations. Respect `QUOTASWITCH_HOME`, then `AGY_AUTH_HOME`, while keeping explicit `CODEX_SWITCHER_HOME` as the highest-precedence Codex directory override. Without overrides, a native file wins; an existing legacy store remains an in-place fallback when the native file is absent. Never copy or delete credentials as a side effect of path resolution.

Preserve the existing account schema and atomic private writes. Add `codex login --no-activate` for saving a renewed identity without writing live auth or changing the selected active account. Active-account renewal still requires coordination with the running app because live-auth synchronization intentionally remains authoritative.

## Consequences

- New Codex stores belong to Quotaswitch, while existing installations continue working without an automatic credential migration.
- Explicit config homes isolate staged sign-ins and tests from real legacy accounts.
- Retiring Switcher requires one deliberate private copy, followed by inventory verification.
- The Codex store remains plaintext credential JSON with restrictive permissions, separate from encrypted Antigravity snapshots.

## Confirmation

Temporary-directory storage selection tests cover native, legacy, explicit, and isolated paths. Mock OAuth CLI tests cover activation defaults, save-only re-login, row identity, metadata preservation, and unchanged live auth. Run `make verify-full` before installing.
