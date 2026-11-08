# quotaswitch

[![npm](https://img.shields.io/npm/v/quotaswitch?label=npm)](https://www.npmjs.com/package/quotaswitch)
[![CI](https://github.com/lifan-builds/quotaswitch/actions/workflows/ci.yml/badge.svg)](https://github.com/lifan-builds/quotaswitch/actions/workflows/ci.yml)
[![license](https://img.shields.io/github/license/lifan-builds/quotaswitch)](LICENSE)

`quotaswitch` is a unified multi-account switcher and background quota auto-rotator for **[Google Antigravity](https://antigravity.google/docs/cli/overview) (`agy`)** and **OpenAI Codex (`codex`)**. Keep personal, work, and multi-workspace accounts side by side, monitor 5-hour and weekly rate limits across both ecosystems, rotate automatically before/upon quota exhaustion, and auto-continue interrupted agent sessions.

![Interactive Account Switcher](assets/demo-switch.png)

## Highlights

- **Unified Antigravity + OpenAI Codex Support (`--provider=agy|codex|all`):** Manage both Google Antigravity (`Keychain` + `~/.gemini/antigravity-cli/antigravity-oauth-token`) and OpenAI Codex (`~/.codex-switcher/accounts.json` + `~/.codex/auth.json`, 100% compatible with `Codex Switcher.app`) from a single binary and a single background LaunchAgent.
- **OpenAI Codex Account Management (`quotaswitch codex ...`):** PKCE S256 login (`quotaswitch codex login`), live credential ingest (`quotaswitch codex add`), safe single-use refresh-token sync (`SyncActiveFromLiveAuth`), ChatGPT `wham/usage` 5-hour/weekly quota inspection, graceful `Codex.app` + VS Code/Antigravity `app-server` reload, and interrupted rollout auto-continuation (`codex queue --thread <id>` + `codex://threads/<id>`).
- **Batch Google OAuth Login (`scripts/batch-login.mjs`):** Onboard multiple Google accounts in ~4–8s per account using a single off-screen Chrome instance seeded with your existing Chrome session cookies (no proxy needed, BotGuard-safe).
- **Phone QR Onboarding Helper (`scripts/onboard-accounts.mjs`):** Automatically signs in to accounts returning `403 VALIDATION_REQUIRED` (`forbidden`) and navigates straight to the one-time phone QR verification screen.
- **Same-Account Model Waterfall (`Claude -> Gemini 4 Argon High`):** Uses your preferred Claude model while Claude quota is available (`> 0%`), then automatically switches the Antigravity Hub model selector to `Gemini 4 Argon High` on the same account when Claude hits `0%` (and treats Free/Starter `aicode-consumers` accounts as Argon-first).
- **Automatic Account Rotation (`quotaswitch rotate --provider=all --watch --interval=30s --relaunch`):** Monitors both Antigravity and Codex every 30s, rotates to the healthiest available account when priority quota hits `0%`, refreshes OAuth tokens safely before swapping, and restarts/hot-reloads Desktop surfaces.
- **Automatic Conversation Continuation (`internal/cli/autocontinue.go` & `internal/codex/process.go`):** Detects top-level Antigravity Hub conversations and Codex rollouts stopped mid-turn and automatically resumes them after switching.
- **Full Quota & Status View (`quotaswitch list --provider=all` / `quotaswitch usage --all`):** Shows `5H` and `WEEKLY` quota percentages and reset times across all stored accounts in parallel.
- **Encrypted Snapshots & Cross-Machine Portability:** AES-256-GCM encrypted local Antigravity storage with `quotaswitch export` / `quotaswitch import` for moving accounts across macOS, Windows, and Linux.

## Install

npm is the recommended install:

```shell
npm install -g quotaswitch
quotaswitch version
```

Run it without a global install:

```shell
npx quotaswitch list
```

> Installing `quotaswitch` also provides an `agy-auth` CLI alias for backwards compatibility.

## Quick start

```shell
# Add an account directly through Google OAuth.
quotaswitch login --alias personal

# Already signed in with agy? Save that account instead:
# quotaswitch add --alias personal

# Add a second account.
quotaswitch login --alias work

# Inspect and switch.
quotaswitch list
quotaswitch switch work

# Toggle back to the previous account.
quotaswitch switch -
```

After switching, restart `agy`; a running session keeps the old credential in memory. By default, `quotaswitch switch` updates both the CLI credential and Antigravity Desktop profile; use `--relaunch` to restart Desktop automatically.

## Commands

| Command | Description |
|---------|-------------|
| `quotaswitch list [--provider agy\|codex\|all] [--active] [--no-sync] [--no-usage] [--live]` | List accounts, plan, and quota across Antigravity and/or Codex |
| `quotaswitch add [--alias <alias>] [--target all\|cli\|desktop]` | Save the account signed in to agy or Desktop |
| `quotaswitch login [--alias <alias>] [--no-browser] [--via-agy]` | Add an account through Google OAuth |
| `quotaswitch switch [<query>\|-] [--live] [--target all\|cli\|desktop] [--relaunch] [--no-relaunch] [--no-auto-continue]` | Switch interactively, by query, or to the previous account |
| `quotaswitch rotate [--provider agy\|codex\|all] [--target all\|cli\|desktop] [--relaunch] [--no-relaunch] [--no-auto-continue] [--force] [--watch] [--interval <dur>]` | Automatically switch model/account on quota exhaustion for Antigravity, Codex, or both |
| `quotaswitch codex <list\|usage\|switch\|rotate\|add\|login\|warmup\|status> [options]` | Manage OpenAI Codex accounts, quota, switching, PKCE login, and session continuation |
| `quotaswitch remove [<query>...] [--all] [--logout] [--yes] [--live]` | Remove stored accounts |
| `quotaswitch alias set <query> <alias>` | Set an alias |
| `quotaswitch alias clear <query>` | Clear an alias |
| `quotaswitch alias list` | List aliases |
| `quotaswitch current` | Print the live agy account |
| `quotaswitch status` | Show storage paths and credential health |
| `quotaswitch usage [<query>] [--all]` | Show or compare quota groups |
| `quotaswitch config live [--interval <seconds>]` | Show or set the live refresh interval |
| `quotaswitch import <file-or-dir> [--alias <alias>] [--activate]` | Import credential JSON |
| `quotaswitch import --purge` | Rebuild the registry from stored snapshots |
| `quotaswitch export [--dir <directory>] [<query>...]` | Export credentials |
| `quotaswitch path` | Print storage locations |
| `quotaswitch clean [--dry-run]` | Prune old backups and orphan snapshots |
| `quotaswitch version [--check]` | Print the version; `--check` queries the latest release |

Queries match row number, account ID, alias, email, or account ID prefix.

Global options: `--json` for machine-readable output and `--no-color` to disable colors. See the [user guide](docs/user-guide.md#json-output) for JSON details.

## Quota and live views

`quotaswitch list` shows the current plan and the 5-hour/weekly quota groups, including reset times. `quotaswitch usage --all` shows every group for every stored account:

![Quota Groups Comparison](assets/demo-usage.png)

```shell
quotaswitch usage --all
```

For an interactive view:

```shell
quotaswitch list --live
quotaswitch switch --live
quotaswitch remove --live
```

In `switch --live`, pressing Enter switches and keeps the view open. The view can also switch automatically when a quota group is exhausted. See the [user guide](docs/user-guide.md#quota-and-live-views) for keys and details.

## Batch Onboarding & Continuous Auto-Switch (macOS)

```shell
# 1. Batch-login multiple Google accounts (~4s for Chrome-signed-in accounts, ~8s with password)
AGY_LOGIN_PASS='<password>' node scripts/batch-login.mjs user1@gmail.com user2@gmail.com

# 2. Complete one-time phone QR verification for any unactivated (403 VALIDATION_REQUIRED) accounts
AGY_LOGIN_PASS='<password>' node scripts/onboard-accounts.mjs

# 3. Check 5-hour and weekly quota across both Antigravity and OpenAI Codex
quotaswitch list --provider=all

# 4. Install and start the unified background 30s auto-switcher + auto-continue LaunchAgent (--provider=all)
./scripts/install-autoswitch-service.sh
```

## Updates

`quotaswitch` checks the latest npm release at most once every 24 hours. On an interactive terminal it offers to update in place; otherwise it prints the update command. Run:

```shell
quotaswitch version --check
```

to check immediately. Disable automatic checks with `QUOTASWITCH_NO_UPDATE_CHECK=1`. npm installs update through npm; standalone binaries verify the release checksum before replacing themselves.

## Storage and safety

`quotaswitch` uses the same credential storage as `agy` (OS keyring with a file fallback). Account snapshots and active-credential backups are encrypted with AES-256-GCM. `quotaswitch path` prints the storage locations.

> Back up `~/.config/quotaswitch/encryption.key` separately. Without it, encrypted snapshots cannot be recovered. Exported credentials are plaintext; delete them after use.

See [Security](SECURITY.md) and the [user guide](docs/user-guide.md#storage-and-switching) for the trust model and recovery details.

## Requirements

- Antigravity CLI installed. `quotaswitch login` adds accounts directly; it uses the installed CLI to discover the OAuth client. Signing in to `agy` first is only needed for `quotaswitch add` and for running `agy` after a switch.
- Linux, macOS, or Windows. Linux uses a Secret Service provider when available and otherwise falls back to file storage.
- Node.js 18+ for the npm package. The package bundles prebuilt binaries, so no Go toolchain is required.

## Learn more

- [User guide](docs/user-guide.md) — storage, quota, OAuth, JSON, environment variables
- [Security](SECURITY.md) — credential handling and reporting
- [Internals](docs/internals.md) — implementation details
- [Contributing](CONTRIBUTING.md) and [Development](docs/development.md) — building and testing
- [Release process](docs/release.md) and [Changelog](CHANGELOG.md)
- [Documentation index](docs/README.md)

## Disclaimer

This project is not affiliated with Google. Use it at your own risk. The default OAuth login uses the same public client as Antigravity CLI; review Google's terms and your account policies before using it with a work or enterprise account.
