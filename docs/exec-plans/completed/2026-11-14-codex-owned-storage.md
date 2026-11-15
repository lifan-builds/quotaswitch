# Quotaswitch-owned Codex account storage

| Field | Value |
|---|---|
| Status | completed |
| Owner | lifan-builds |
| Date | 2026-11-14 |
| Related | [ADR-0007](../../adr/0007-codex-owned-storage.md) |

## Objective

Let Quotaswitch manage Codex accounts independently of Codex Switcher while keeping existing saved accounts usable and allowing sign-ins that leave the live Codex session alone.

## Context

Codex accounts currently default to `~/.codex-switcher/accounts.json`, and every `codex login` activates the authenticated account. The user is retiring the desktop switcher and renewing saved accounts.

## Scope

### In scope

- A native `<quotaswitch config home>/codex/accounts.json` default, explicit legacy override, and fallback to an existing legacy store without copying credentials.
- `codex login --no-activate` and isolated regressions for storage selection and activation behavior.
- User documentation and an ADR for the credential-path change.

### Out of scope

- Installing binaries, changing live credentials/services, or performing browser sign-ins.
- Changing the Codex account schema or session recovery implementation.

## Expected paths

- `internal/codex/storage.go` and storage tests.
- `internal/cli/cmd_codex.go`, CLI specs, and Codex CLI tests.
- `README.md`, `AGENTS.md`, and `docs/`.

## Risks

- Selecting an empty native store could hide existing legacy accounts; retain an in-place fallback when no config-home override is given.
- A re-login of the active identity can later be overwritten by a running Codex session's live credentials; document staging and require a normal switch for activation.

## Acceptance criteria

- [x] New stores belong to Quotaswitch; existing stores and explicit `CODEX_SWITCHER_HOME` remain usable.
- [x] Explicit config homes isolate tests and staged sign-ins from the real legacy store.
- [x] `--no-activate` preserves live `auth.json`, active selection, saved identity IDs, and account metadata.
- [x] The required full harness profile passes.

## Verification plan

```shell
go test ./internal/codex ./internal/cli
make verify-full
```

## Rollback

Revert these code changes or set `CODEX_SWITCHER_HOME` to the intended account directory. This change does not move or delete any credentials automatically.

## Decision log

| Date | Decision | Rationale |
|---|---|---|
| 2026-11-14 | Use the platform Quotaswitch config home with a `codex/` child | One owner for native account storage; existing schema remains compatible |
| 2026-11-14 | Preserve legacy storage in place as a fallback | Existing installations remain usable without copying secrets |

## Completion

- Commit/tag: none requested.
- Verification result: `env -u NO_COLOR make verify-full` passed, including race tests and current npm packaging; `make build` passed. The host exports `NO_COLOR=1`, which conflicts with existing color-rendering assertions, so the verification environment removed that variable.
- Residual risk: active-account re-login still requires coordination with the running app. Storage selection never migrates or deletes live credentials automatically.
