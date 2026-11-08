---
name: agy-auth-switcher
description: Batch-ingest, switch, rotate, and check quota for Google Antigravity accounts on macOS using agy-auth without a proxy. Use when adding one or more Google accounts to agy-auth, automating Google OAuth login from an existing Chrome session or password, switching the active Antigravity account, or troubleshooting BotGuard/reCAPTCHA loops during Google OAuth.
---

# Antigravity Account Switcher (`agy-auth`)

Manage multiple Google Antigravity Pro/Free accounts on macOS using `agy-auth` (`@fikrilal/agy-auth`, source checkout at `<repo-dir>`).

## Quick Reference

```bash
# List accounts and live model quota
agy-auth list

# List accounts fast without network quota requests
agy-auth list --no-usage

# Capture the currently logged-in Antigravity Desktop account (token-only, preserves window layout)
agy-auth add --target=cli --alias <alias>

# Switch active account (updates Keychain, file mirror, credential-user-scope, and hot-reloads language_server)
agy-auth switch <alias-or-email>

# Switch CLI credential only without restarting the Desktop language_server
agy-auth switch <alias-or-email> --no-relaunch

# Switch without auto-continuing quota-stopped Desktop conversations
agy-auth switch <alias-or-email> --no-auto-continue

# Rotate to the next account when the active model group (or both priority groups) hits 0%
agy-auth rotate

# Run continuous background auto-switcher (polls every 30s, restarts Hub, and auto-continues stopped turns)
agy-auth rotate --watch --interval=30s --relaunch
```

## Auto-Switch Quota Policy (`Claude -> Gemini 4 Argon High`) & Auto-Continue

- **Tracked quota groups (`retrieveUserQuotaSummary`):**
  1. **Claude Family (`"Claude and GPT models"`, buckets `3p-5h`, `3p-weekly`):** effective remaining = `min(3p-5h, 3p-weekly)`.
  2. **Gemini 4 Argon (`"Gemini 4 Argon"`, bucket `gemini-argon-wtu-weekly`):** effective remaining = `gemini-argon-wtu-weekly`.
  3. **Ignored (`"Gemini Models"` on Pro accounts):** regular Gemini Flash/Pro quota is ignored for rotation decisions on Pro accounts.
  4. **Free / Starter accounts (`Plan: Free`, project `aicode-consumers`):** `retrieveUserQuotaSummary` returns only `Gemini Models` and `Claude and GPT models` (there is no separate `Gemini 4 Argon` group because Argon shares the `Gemini Models` 5h/weekly buckets). Their Claude quota is ignored, and Argon quota is read from `Gemini Models`.
- **Same-account model waterfall (`reconcileActiveAccountModelAndContinue`):**
  1. **Use Claude quota first (`Claude > 0%`):** While the active account has `Claude Family > 0%`, it uses your preferred Claude model (remembered in `~/Library/Application Support/agy-auth/active_quota_state.json`, defaulting to `Claude Opus 5.5 High`).
  2. **Auto-switch to `Gemini 4 Argon High` on the same account when Claude hits `0%`:** Once `Claude Family == 0%` while `Gemini 4 Argon > 0%` on the active account, `agy-auth rotate` stays on the same account, switches the Antigravity Hub model selector (`[data-testid="model-selector-trigger"]`) and `~/.gemini/antigravity/antigravity_state.pbtxt` to **`Gemini 4 Argon High`**, and automatically sends `"Continue"` to any stopped conversations.
  3. **Account rotation when both hit `0%`:** Once **both** `Claude Family == 0%` **and** `Gemini 4 Argon == 0%` on the active account, `agy-auth rotate` switches to the next healthy account (preferring accounts with `Claude Family > 0%` first, then `Gemini 4 Argon > 0%`), selects Claude (if `Claude > 0%`) or `Gemini 4 Argon High` (if `Claude == 0% && Argon > 0%`) in the reloaded Hub UI, and auto-continues stopped conversations.
- **Automatic conversation continuation (`internal/cli/autocontinue.go`):**
  - Before restarting `language_server`, `switch` and `rotate` snapshot the open Hub conversation path (`/c/<id>?project=...`), any conversations actively running in the sidebar (`[aria-label="Stop execution"]`, `[data-testid="status-loading-spinner"]`), and recent (`<= 2h`) top-level `~/.gemini/antigravity/brain/<id>/.system_generated/logs/transcript.jsonl` transcripts that stopped mid-turn (`GENERIC`, `USER_INPUT`, `SYSTEM_MESSAGE`, `PLANNER_RESPONSE` with `tool_calls` / `ERROR`, or a parent turn that invoked `invoke_subagent`, `send_message`, `schedule`, or a background `run_command` and has not yet received the completion `SYSTEM_MESSAGE`).
  - Subagent transcripts (`step 0` not `USER_EXPLICIT`), user-cancelled turns (`CANCELED`), `ask_question` prompts, non-empty message input drafts, and conversations that already received `2` consecutive unprogressed `"Continue"` prompts are skipped.
  - Once Electron reloads the Hub window on the new `language_server` port (`DevToolsActivePort` inside `~/Library/Application Support/Antigravity`), `agy-auth` attaches via localhost CDP WebSocket in the background, waits for `ListCascades` to populate `a[href^="/c/"]` in the sidebar (expanding collapsed `button[data-testid="project-header"][aria-expanded="false"]` folders if needed so client-side navigation never triggers a full page reload), waits for `AgentInputBox.isConvoLoading` (`[data-testid="send-button-loading"]`) to clear, submits `"Continue"` (`Enter` + `button[data-testid="send-button"]`), and restores the user's original open conversation view. Pass `--no-auto-continue` to disable.
- **Confirmed quota only:** `rotate`, `rotate --force`, and `switch --live` switch only into activated accounts whose quota was read successfully and whose Claude or Argon quota is **>= 2%** (fractions below 1% show as `0%`). Unactivated accounts (`forbidden`, HTTP 403 `VALIDATION_REQUIRED`) are never chosen automatically, not even with `--force`, because `language_server` starts signed out with them. Activate them ahead of time with `scripts/onboard-accounts.mjs`. When nothing qualifies, `rotate` fails and reports the earliest reset (`earliest reset: <alias> at <time>`). Before writing Keychain, `switch` refreshes access tokens that are expired or expire within 2 minutes.
- **Token refresh without `agy` on `PATH`:** `oauth.EnsureClient` automatically falls back to `/Applications/Antigravity.app/Contents/Resources/bin/language_server` (and Windows/Linux equivalents) whenever `agy` is missing, a dangling symlink, or a wrapper script without embedded OAuth client hashes.
- **Background service:** Installed at `~/Library/LaunchAgents/com.agy-auth.autoswitch.plist` (logs at `~/Library/Application Support/agy-auth/autoswitch.log`). After rebuilding, restart it with `launchctl kickstart -k gui/$(id -u)/com.agy-auth.autoswitch`.
- **Polling cost:** On a non-forced check, `rotate` queries only the active account first and fans out to the other accounts only when the active one is exhausted, unactivated, or has an auth failure. `usage.Client` caches refreshed OAuth access tokens per refresh token, runs `retrieveUserQuotaSummary` and `retrieveUserQuota` in parallel, and `FullCache` caches `forbidden` results for the TTL. The watcher runs at `--interval=30s`, and each log line is timestamped (`2006-01-02 15:04:05`).
- **Debugging a failed auto-switch:** Look at `autoswitch.log` (rotation lines), `autoswitch.err.log`, and `jq '.accounts[] | {alias, last_used_at}' registry.json` plus `backups/registry-*.json` (the `active` field at each switch) to rebuild the timeline. Then run `agy-auth usage --all` to see each account's 5h and weekly buckets. Pitfalls: the Keychain value starts with `go-keyring-base64:` (decode it before parsing); `~/.gemini/antigravity-cli/antigravity-oauth-token` can be stale when Desktop only updates Keychain; `backups/active-*.json` files are vault-encrypted; and `ANTIGRAVITY_PROJECT_ID` inside Antigravity terminals is a workspace UUID rather than a GCP project ID.
- **Source of the patched build:** `<repo-dir>` (upstream v0.3.0 + local patches). Build with `go build -o /opt/homebrew/lib/node_modules/@fikrilal/agy-auth/npm/vendor/agy-auth-darwin-arm64 ./cmd/agy-auth`. `npm update -g @fikrilal/agy-auth` overwrites the patched binary.

## Re-login or Manual Sign-in Checklist

1. After signing in to an account in Antigravity Desktop, register it right away (token-only):
   `agy-auth add --target=cli --alias <alias>`
   (`rotate` also auto-ingests an unregistered live CLI credential automatically.)
2. Confirm the watcher sees it: `tail -n 3 ~/Library/Application\ Support/agy-auth/autoswitch.log`
   should say `Active account <alias> ... is healthy`.
3. Check every account at once: `agy-auth usage --all` (`5H` and `WEEKLY` are always shown; `* ` marks the live account).
   An account is usable only if both its 5H and WEEKLY are >= 2% for Claude (or Argon).
4. For `forbidden` accounts, run `scripts/onboard-accounts.mjs` and finish phone QR verification. Do not switch into them first.

## Batch-Ingesting Google Accounts

Use `scripts/batch-login.mjs` to log in multiple Google accounts end-to-end (~4s per Chrome-signed-in account, ~8s per password account):

```bash
# Pass --password-env (or AGY_LOGIN_PASS) if some accounts are not yet signed in inside Chrome Default profile.
# Pass --force to re-login accounts that are already registered.
AGY_LOGIN_PASS='<password>' node <repo-dir>/scripts/batch-login.mjs \
  user1@gmail.com user2@gmail.com

# For accounts returning 403 VALIDATION_REQUIRED (`forbidden`), auto-login to the phone QR verification screen:
AGY_LOGIN_PASS='<password>' node <repo-dir>/scripts/onboard-accounts.mjs
```

### Why This Script Takes ~4–8s per Account (and Avoids BotGuard)

1. **Single persistent off-screen Chrome instance across the whole batch:**
   - Spawns one off-screen Chrome (`--window-position=3200,3200`) with `--disable-backgrounding-occluded-windows --disable-renderer-backgrounding --disable-background-timer-throttling` so macOS WindowServer occlusion never throttles `requestAnimationFrame` during Google's Material card slide transitions.
   - Opens and closes a fresh tab per account via CDP HTTP (`PUT /json/new?<url>` and `/json/close/<id>`), binding ephemeral ports (`0`) for both the OAuth callback server and DevTools so concurrent or back-to-back runs never collide.
2. **Seeds Chrome cookies and processes already-signed-in accounts first:**
   - Copies `Local State`, `Default/Cookies`, `Default/Preferences`, and `Default/Secure Preferences` from `~/Library/Application Support/Google/Chrome` into a temporary `--user-data-dir`. Because both Chrome instances share the macOS `"Chrome Safe Storage"` Keychain entry, all Google session cookies (`SID`, `LSID`, `ACCOUNT_CHOOSER`, `__Host-GAPS`, `NID`) decrypt cleanly.
   - Reads `account_info` from `Default/Preferences` and sorts already-signed-in accounts **first** so they finish in ~4s each with a single chooser/consent click before touching any password-entry flow.
3. **Never re-types the email and filters multi-card SPA nodes via `DOM.querySelectorAll`:**
   - `agy-auth login --login-hint <email>` causes Google's `/v3/signin/identifier` page to pre-fill `input#identifierId` server-side; the script only clicks `#identifierNext button`.
   - Google's `/v3/signin` SPA keeps hidden `input[type="password"]` nodes from `/signin/identifier` in the DOM when transitioning to `/challenge/pwd`. `getStableCenter` queries `DOM.querySelectorAll`, selects the in-viewport box whose coordinates have stopped sliding across 70ms, and verifies `el.value.length === PASSWORD.length` before clicking `#passwordNext button`.
4. **Waits 1.6–2.6s before attaching CDP and never passes `--disable-blink-features=AutomationControlled`:**
   - Attaching CDP while BotGuard's initial VM is executing on `/v3/signin/identifier`—or passing `--disable-blink-features=AutomationControlled`—flags the session and forces a reCAPTCHA challenge. Waiting ~1.6–2.6s before opening the tab WebSocket avoids `Runtime.enable` hooks and advances straight to `/v3/signin/challenge/pwd`.
5. **Ignores Chrome's `chrome://signin-dice-web-intercept.top-chrome/` target and handles reCAPTCHA fallback:**
   - Filters targets with `t.type === "page" && t.url.startsWith("http")` so Chrome's Dice bubble never shadows the OAuth tab, and waits 6.2s for `#recaptcha-anchor` warmup if Google ever presents a checkbox.

## How Desktop Hub Switching Works on macOS (`v2.28+`)

- Always register or log in accounts using `--target=cli` (`agy-auth add --target=cli --alias <alias>`) so legacy Electron `Cookies` / `Local Storage` directories are not snapshotted.
- In Antigravity `v2.28+` (Hub architecture), the UI is served by `/Applications/Antigravity.app/Contents/Resources/bin/language_server --standalone --subclient_type hub`.
- `language_server` reads the OAuth token from macOS Keychain (`svce="gemini", acct="antigravity"`) and `~/.gemini/antigravity-cli/antigravity-oauth-token` **only at process startup** (`authclient.NewAuthClient -> tryRestoreStoredToken`), and writes its in-memory `StoredToken` (`{"token": ..., "auth_method": "consumer", "id_token_email": "..."}`) back to Keychain on token refresh.
- Running `agy-auth switch <alias>` or `agy-auth rotate`:
  1. Updates `~/Library/Application Support/Antigravity/credential-user-scope` (`"u" + sha256(sub)[:16]`).
  2. Writes the target account's token to Keychain and `~/.gemini/antigravity-cli/antigravity-oauth-token`.
  3. Terminates `language_server` (`pkill -x language_server`), waits 400ms, and re-writes Keychain so any shutdown flush is overwritten before Electron's `monitorLsCrashInternal` respawns `language_server` 2s later and reloads the Hub window.
  4. Enforces a 2-restarts-per-65s cap (`~/Library/Application Support/agy-auth/ls_restarts.json`) so Electron never hits its `MAX_RESTARTS = 3` in 60s limit.

## Cross-Machine Account Sharing (macOS <-> Windows)

- Google OAuth `refresh_token`s in `agy-auth` are **not** hardware-bound; only the storage wrapper (`~/Library/Application Support/agy-auth/encryption.key`) is machine-local.
- To copy all accounts to Windows (or another Mac):
  ```bash
  # On source machine: export decrypted portable credential directory
  mkdir -p ~/Desktop/agy-accounts && agy-auth export --dir ~/Desktop/agy-accounts

  # On target machine (Windows PowerShell):
  .\agy-auth-windows-amd64.exe import .\agy-accounts
  ```
