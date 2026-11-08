# ADR-0004: Rename project and CLI to quotaswitch

- **Status:** Accepted
- **Date:** 2026-11-08
- **Decision makers:** lifan-builds
- **Consulted:**
- **Related:** [`docs/adr/0003-unified-codex-provider-support.md`](0003-unified-codex-provider-support.md)

## Context

After unifying Google Antigravity (`agy`) and OpenAI Codex (`codex`) multi-account quota rotation and session recovery into a single binary, the original Antigravity-only name (`agy-auth` / `agy-auth-switcher`) no longer described the scope of the tool.

## Decision

1. Rename the GitHub repository (`lifan-builds/quotaswitch`), Go module (`github.com/lifan-builds/quotaswitch`), unscoped npm package (`quotaswitch`), and primary CLI command (`quotaswitch`).
2. Ship `agy-auth` as a secondary `bin` alias in `package.json` and `Makefile` (`install`) so existing scripts and muscle memory continue to work unchanged.
3. Preserve backwards-compatible storage and environment fallbacks:
   - Keep `snapshotPrefix = "agy-auth-encrypted:v1:"` in `internal/agy/snapshot.go` so existing AES-256-GCM vaults decrypt without re-encryption.
   - Support `QUOTASWITCH_*` environment variables first while falling back to `AGY_AUTH_*`, and fall back from `<UserConfigDir>/quotaswitch` to `<UserConfigDir>/agy-auth` if an unmigrated legacy registry exists.

## Consequences

- **Good:** Clear, provider-agnostic name aligned with quota-driven subscription switching across AI coding tools; unscoped `npx quotaswitch` works cleanly; zero breaking impact on existing encrypted vaults or `agy-auth` callers.
- **Neutral:** Historical ADRs (`0001`–`0003`), completed execution plans, and prior `CHANGELOG.md` entries retain their original `agy-auth` wording as historical records.
- **Bad:** None.

## Confirmation

- `make verify-full` and GitHub Actions `ci` (`harness verify --profile ci`).
