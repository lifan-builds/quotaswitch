# Quotaswitch Round 3 — Prompt Cache Hit Rate, OAuth Refresh Races & Multi-Seat Reset Orchestration

- **Generated:** 2026-11-27T10:33:47-0800 | **Model:** `gpt-6.2-astra` (`reasoning=xhigh`)
- **Sticky Seat:** `gameoflifan@gmail.com` (`9dfe5724`) | **Prompt Cache Hit Rate:** `0.0%` (`0` / `124,990` input tokens)
- **Total Tokens:** `169,941` (output/reasoning: `44,951` / `36,782`) | **Wall Time:** `520.6s`

## Pass 1: Why Prompt Cache Hit Only 7%–18% — Exact Codex Bridge Root Cause Analysis
*Seat: `gameoflifan@gmail.com` · in=`25,041` · cached=`0` (`0.0%`) · out=`6,782` · `138.5s`*

**If the calls were to `POST /v1/responses` with `prompt_cache_key` in the JSON body, QuotaSwitch did not strip the key.** If they were to `POST /v1/chat/completions`, it did. On **both** routes, caller-supplied `session_id` and `conversation_id` **headers** are lost when the gateway constructs the upstream request.

Those are exact, fixable behaviors. The source does **not** establish which one caused the observed 0% versus 99.6% split. Pinning a seat selects credentials; it does not identify a backend cache worker or establish that the prefix is resident there.

### Request trace

`internal/gateway/server.go:84–89` dispatches the two POST routes to different handlers. Neither uses the general OpenAI Platform passthrough.

| Signal | `/v1/chat/completions` → Codex | `/v1/responses` → Codex |
|---|---|---|
| JSON `prompt_cache_key` | **Dropped at decode.** `handleChatCompletions` unmarshals into `ChatCompletionRequest` (`internal/gateway/server.go:199–203`), which has no such field (`internal/gateway/codex_bridge.go:28–40`). The outgoing `codexResponsesRequest` also has no such field (`:129–139`). | **Preserved.** `handleCodexResponsesPassthrough` decodes a JSON object into `map[string]any`, edits specific fields, and re-marshals the whole map (`internal/gateway/server.go:228–245`). It sends those bytes at `:270`. |
| JSON `session_id`, `conversation_id` | Dropped for the same reason. Neither struct declares them. | Preserved as JSON members by the same map round trip. They are **not** promoted to headers. Preservation does not establish that the backend recognizes them as body parameters. |
| JSON `instructions` | A top-level `instructions` field is ignored. `buildCodexResponsesPayload` instead takes `system` and `developer` messages in order, joins text parts within each message with `"\n"`, joins messages with `"\n\n"`, trims the result, and substitutes `defaultDirectInstructions` if empty (`internal/gateway/codex_bridge.go:216–228,273–276`). It emits the result as `Instructions`, with `Stream: true` and `Store: false` (`:367–377`). | Existing `instructions` is unchanged, **even if empty or `null`**. Only an absent field gets `defaultDirectInstructions`. Likewise `store` defaults to `false` only if absent; a string `model` is resolved and rewritten (`internal/gateway/server.go:230–244`). |

The instruction default is a **fixed literal** (`internal/gateway/codex_bridge.go:25`), not a per-request nonce. It can make the effective prefix differ **between routes or request shapes**; it does not by itself explain alternating results for otherwise identical calls on one route.

**The common header-loss point is `(*Server).doCodexResponsesRequest`, `internal/gateway/codex_bridge.go:545–579`.** Line 554 calls `http.NewRequestWithContext` with the body, not the incoming headers. For a ChatGPT OAuth candidate, lines 558–573 set candidate `Authorization`, `Content-Type`, `Accept: text/event-stream`, `originator: codex_cli_rs`, the fixed `User-Agent`, and optionally `ChatGPT-Account-Id`. Neither session header is set. `X-Quotaswitch-Account` and `X-Quotaswitch-Auth` have already been consumed for candidate selection (`codex_bridge.go:414–416`; `server.go:247–249`). The header-copy loop at `server.go:294–300` is **upstream response → downstream client**, not the reverse.

### Minimal Go fix, scoped to what the caller sends

**1. For a caller sending JSON `prompt_cache_key` on Chat Completions, the entire fix is three additions** in `internal/gateway/codex_bridge.go`:

```go
// In ChatCompletionRequest (line 28):
PromptCacheKey string `json:"prompt_cache_key,omitempty"`

// In codexResponsesRequest (line 129):
PromptCacheKey string `json:"prompt_cache_key,omitempty"`

// In buildCodexResponsesPayload's return literal (line 367):
PromptCacheKey: req.PromptCacheKey,
```

This preserves a caller-supplied stable key without inventing one or changing unkeyed requests. **No body change is needed for `/v1/responses`.**

**2. If the caller supplies session identity in headers, fix the shared sender instead.** Add `incoming http.Header` as the last parameter of `doCodexResponsesRequest` (`codex_bridge.go:545`). Pass `r.Header` at **all three** shown call sites: the initial Chat request (`codex_bridge.go:446`), its OAuth 401 retry (`:454`), and Responses passthrough (`server.go:270`). In the OAuth branch, immediately after setting `originator` (`codex_bridge.go:568`), add:

```go
for _, name := range []string{"session_id", "conversation_id"} {
	if value := incoming.Get(name); value != "" {
		req.Header.Set(name, value)
	}
}
```

That preserves the two headers when present without forwarding client `Authorization`, cookies, or account-selection headers. Neither fix assumes that `session_id`, `conversation_id`, and `prompt_cache_key` are interchangeable.

### Verify the measurement before attributing the misses

`ChatCompletionUsage.CachedTokens` is tagged **`json:"-"`** (`internal/gateway/codex_bridge.go:122–127`). The translated Chat usage object therefore **omits** cached tokens; a client that converts a missing field to zero is not measuring a cache miss. The accounting at `codex_bridge.go:519–534` uses the internal count only for API-key candidates, not OAuth seats.

For a decisive gateway test, point `Server.CodexResponsesURL` at an `httptest.Server` and assert the **received** key, session headers, resolved model, instructions, and effective prefix on two consecutive requests. For the live hit-rate test, read the raw Responses `usage.input_tokens_details.cached_tokens` (in `response.completed.response.usage` for SSE), not the translated Chat usage. If the original traffic already used `/v1/responses`, had the same JSON key, and did not supply session headers, **neither patch changes those requests**; the precise cause of that hit-rate distribution cannot be recovered from this gateway code.

## Pass 2: Single-Use Refresh Token Races & Storage Atomicity Audit
*Seat: `gameoflifan@gmail.com` · in=`24,983` · cached=`0` (`0.0%`) · out=`8,506` · `172.33s`*

The highest-priority fix is to **lock before redeeming the refresh token**, not just before saving the result. The second is to **stop treating a mismatched `auth.json` as an instruction to change the active account**. Atomic file replacement alone solves neither problem.

Line numbers below count from the SPDX header in each supplied file.

### 1. A refresh can race across processes before the store sees it

`internal/gateway/pool.go:325–345` — `Pool.EnsureCodexToken` takes a mutex from `Pool.codexRefreshLocks` and reloads the account. That mutex belongs to one `Pool`. Two gateway processes, or two pools in one process, can both load the same single-use refresh token. Both can then call `codex.RefreshTokens` at line 365. `Store.PersistRefreshedAccount` checks `previousToken` only **after** redemption (`internal/codex/storage.go:445–468`). That check can reject a stale disk write; it cannot undo a second redemption.

**Minimal fix:** move the refresh transaction into `Store`. For the smallest correct change, use the existing `accounts.lock` across reload → recheck → bounded OAuth call → save. Verify that `lockFile` provides OS-level exclusion; its implementation is not in the excerpt. Split persistence into a private `persistRefreshedAccountLocked` helper rather than calling the current locking method recursively. A waiter handling a forced refresh after a 401 should compare the token that got the 401 with the reloaded token and reuse a valid replacement instead of rotating again.

Keep the active-Codex guard (`pool.go:347–352`). A QuotaSwitch lock cannot serialize an independent Codex process that does not take it. Also, `storage.go:462` replaces **all** of `AuthData` after checking only the refresh token. Apply the returned OAuth fields to the reloaded record rather than assigning a stale `AuthData` snapshot.

### 2. Two individually atomic writes can leave the wrong account active

`internal/codex/storage.go:282,331` — `Store.saveAccounts` and `Store.WriteLiveAuth` each use `backup.WriteFileAtomic`. There is no demonstrated torn-JSON bug in those call sites. The helper is not supplied; verify that it uses a same-directory temporary file with mode `0600`, checks write/sync/close errors, atomically replaces the destination, and syncs the directory where required for crash durability.

The **demonstrable** problem is between the files. `Store.upsertTokens` saves `accounts.json` before writing `auth.json` (`storage.go:633–640`); `Store.PersistRefreshedAccount` uses the same order (`464–469`). If activation of B commits to the store but the live write fails, the store selects B while the live file still names A. On the next request, `Store.SyncActiveFromLiveAuth` can select A again (`366–369,407–417`). The selection change is **outside** the token-freshness check at line 376: even a live token rejected as stale can change the active account.

**Minimal fix:** make ordinary sync update credentials only for the already-selected identity. On a different live identity, return a distinct conflict without changing `ActiveAccountID` or `LastUsedAt`. Keep deliberate adoption of an external Codex switch as a separate operation. Propagate the sync error: `Pool.CodexCandidatesWithMode` currently discards it at `internal/gateway/pool.go:201–205`.

For a failed second write, report **“store committed; live auth not updated”**. Do not retry OAuth or roll back a rotated refresh token. If automatic crash recovery is required, persist a pending live-write intent *in the same store commit*, with the expected old live digest and intended new digest. Recovery can recognize an already-applied write, repair an unchanged old file, or report a conflict. A digest check followed by rename is **not** compare-and-swap against an uncooperative Codex writer; defer repair while it may be writing. `WriteLiveAuth` itself takes no lock (`storage.go:304–332`), so QuotaSwitch callers should use a locked operation that rechecks the selection, not write an old `StoredAccount` directly.

### 3. Importing live auth can roll credentials backward

`internal/codex/storage.go:423–442` — `olderLiveTokens` uses token **expiry** to order different credentials. Expiry is not a rotation sequence. If expiries do not distinguish the records and the live timestamp is missing, line 437 accepts the live tuple. There is also a path around this check entirely: `Store.AddFromLiveAuth` calls `upsertTokens(..., true, now)` (`646–657`), which replaces the matching credentials (`546–558`), stamps them with `now`, and writes them back to live auth (`633–640`). An old import can therefore look newly refreshed.

**Minimal fix:** separate **import** from **activation**. Importing the live file should not rewrite its source or assign import time as proof of credential freshness. For an existing identity, leave identical credentials alone; accept a differing tuple only on reliable freshness evidence, otherwise return a reconciliation conflict. For gateway bootstrap, recheck that the store is still empty **under the import lock**: the emptiness test in `pool.go:203` occurs before `AddFromLiveAuth` acquires that lock.

### 4. Workspace ambiguity can overwrite the wrong credential record

`internal/codex/storage.go:475–513` — `matchStoredAccountIndex` prefers the active record, then the first match. `sameIdentity` rejects a workspace mismatch only when **both** full workspace IDs are present. With one ID missing, the same subject or email can match a record in another workspace. At `546–558`, `upsertTokens` then replaces that record’s credentials and can erase its known `AccountID`.

There is a separate lookup bug: `Resolve` returns the **first** full `AuthData.AccountID` match at `745–754`. Two principals can belong to the same workspace. By contrast, the six-character `WorkspaceLabel` (`91–101`) is already checked for ambiguity in `Resolve` (`756–768`); lengthening the label would not fix either first-match bug.

**Minimal fix:** use the full effective workspace ID plus principal identity for credential updates. Do not use a missing workspace as a wildcard for a known one. Have the matcher return `(index, error)`: multiple matches return `ErrAmbiguous`, and insufficient identity for a potentially destructive update returns a conflict, not “create or overwrite.” For `Resolve`, give the local `StoredAccount.ID` its own exact-match pass, then collect full workspace matches and require exactly one. Never use a display suffix to merge credentials.

### 5. `Save` has a lost-update bypass

`internal/codex/storage.go:218–236,244–270` — `Store.Load` gives even a missing file a revision (`sha256.Sum256(nil)`), but `Store.Save` skips its revision check for a zero-valued `AccountsStore.revision`. A caller constructing `&AccountsStore{...}` can overwrite an existing store, including a newer refresh token or active selection, while holding the lock correctly.

Make a zero revision mean **create only**, not unconditional replacement. Inside the existing `Save` lock:

```go
want := data.revision
if want == ([sha256.Size]byte{}) {
	want = sha256.Sum256(nil) // revision Load returns for an absent file
}
current, err := s.Load()
if err != nil {
	return err
}
if current.revision != want {
	return errors.New("Codex accounts changed since loading")
}
```

**Regression tests worth landing with the fixes:** two independent pools sharing one store and a counting fake issuer must produce one redemption; failure between the two file writes must not reselect A or redeem again; stale or undated live auth must not replace a known rotated token; two principals sharing a full workspace ID must resolve as ambiguous; and a zero-revision save over an existing store must leave its bytes unchanged.

## Pass 3: Expiring Reset-Credit & 5h-Window Partition-Sticky Scheduler Design
*Seat: `gameoflifan@gmail.com` · in=`25,008` · cached=`0` (`0.0%`) · out=`7,743` · `157.01s`*

**Use quota priority to choose an account for a new partition; use affinity to keep a warm partition on that account.** An earlier reset on account B should attract new partitions, not move a healthy partition off account A. That distinction is what preserves cache locality.

### Routing rule in `internal/gateway/pool.go`

Add a partition-aware variant of `(*Pool).CodexCandidatesWithMode`, sharing its existing account and auth filtering (`internal/gateway/pool.go:182–237`). Keep the existing method for unkeyed requests and Platform passthrough. An explicit `X-Quotaswitch-Account` also takes the existing path: a pinned request should neither consult nor change an automatic binding.

For a keyed request, order candidates as follows:

| Priority | Candidate |
|---|---|
| 1 | The partition’s bound account, if still eligible, ready, and not known to be out of quota. |
| 2 | Ready Team OAuth accounts with confirmed usable 5-hour allowance, earliest upcoming reset first. A known exhausted weekly or other applicable limit disqualifies the account. |
| 3 | Other ready accounts with usable, expiring credit, earliest loss of usable credit first. |
| 4 | Remaining ready accounts, including those with unknown deadlines. Prefer OAuth over undated API keys to retain the current default. |

Break equal ranks with a stable hash of `(partition, account.ID)`, not the global round-robin counter. Never promote a cooling account over a ready one or retry a known-empty window before its reset. Today `Pool.MarkHealthy` changes one global `preferredCodex` (`pool.go:104–117`), and `CodexCandidatesWithMode` rotates API keys whose expiries fall within 24 hours of the earliest (`pool.go:279–299`). Neither mechanism should decide ownership for a keyed request.

**State:** add one bounded, idle-expiring affinity table to `Pool` (`pool.go:42–60`), protected by its existing `mu`. Key it by a digest of `(canonical upstream model, normalized auth mode, partition)` and store the **account ID**, not a `CodexCandidate` containing credentials. Refresh the idle deadline on successful reuse. Size the table and idle timeout for the observed working set, not for the 5-hour quota period.

**Rebinding:** look up the binding when ordering candidates, but install or replace it only after a successful completion. Compare against the binding generation observed at lookup so a late completion cannot undo a newer failover. Use the existing success points: `handleCodexChatCompletions` in `internal/gateway/codex_bridge.go:491–519` and `handleCodexResponsesPassthrough` in `internal/gateway/server.go:302–318`. Keep the chat handler’s no-replay-after-commit rule intact.

### Two inputs the pool does not have today

- **Partition key.** Use nonblank `X-Quotaswitch-Partition` first, otherwise the body’s `prompt_cache_key`. Treat the header as a local routing override; do not overwrite a caller’s upstream cache key. Chat currently drops that field: add `PromptCacheKey string` with `json:"prompt_cache_key,omitempty"` to both `ChatCompletionRequest` and `codexResponsesRequest`, and copy it in `buildCodexResponsesPayload` (`internal/gateway/codex_bridge.go:28–40,129–139,367–377`). Pass the routing key at the chat candidate lookup (`codex_bridge.go:412–422`). Responses passthrough already has the body map and resolved model before its lookup (`internal/gateway/server.go:228–249`).
- **Window deadline and headroom.** `codex.StoredAccount` exposes `PlanType` and `SubscriptionExpiresAt`, not 5-hour remaining allowance or reset time (`internal/codex/storage.go:48–59`). The existing API-key sort uses subscription expiry (`internal/gateway/pool.go:165–180`); that is **not** a rolling reset. Feed the pool a small, expiring per-account quota snapshot from authoritative usage data or documented upstream rate-limit headers. `doCodexResponsesRequest` can observe headers before it discards non-2xx responses (`internal/gateway/codex_bridge.go:579–595`). Missing or stale data means *unknown*, not “full”; never manufacture a deadline as `now + 5h`.

**Verify the 90% target with two separate metrics.** Measure same-account reuse for repeat partitions, and measure actual cache hits as `eligible repeat requests with CachedTokens > 0 / eligible repeat requests with reported usage`. `ChatCompletionUsage` already has `CachedTokens` (`internal/gateway/codex_bridge.go:122–127`). Test that unrelated successes and newly earlier deadlines cause **zero** healthy-binding moves, while exhaustion moves only the affected partition. Affinity can preserve locality; it cannot guarantee an upstream cache hit for a changed prefix, an evicted entry, or requests landing on different gateway replicas. For multiple replicas, route the same partition to the same replica or share the binding table.

## Pass 4: Streaming SSE Error Recovery, Accounting Accuracy & Client Disconnect Safety
*Seat: `gameoflifan@gmail.com` · in=`24,977` · cached=`0` (`0.0%`) · out=`10,800` · `218.24s`*

**The clearest accounting bug is in `Server.handleCodexChatCompletions`: the gateway can receive final token usage from the upstream, fail to deliver only `[DONE]` to the client, and discard the entire billing record.** The raw Responses path fails differently: it usually records the request, but can record unknown usage as zero.

Line numbers below count from the SPDX line of each supplied file. The excerpts end partway through `collectCodexToChatCompletion` and `buildPlatformCreditDelta`; conclusions about those functions are limited to the visible code.

### 1. High — Accounting depends on downstream delivery

- **Chat bridge:** `streamCodexToChatCompletions` parses `ChatCompletionUsage` from `response.completed` and returns it **even if** the final chunk or `[DONE]` write fails (`gateway/codex_bridge.go:866–879`). `Server.handleCodexChatCompletions` then returns on cancellation or `errDownstreamWrite` **before** `RecordAPIKeyUsage` (`gateway/codex_bridge.go:486–495, 519–534`). This loses known usage and the request count. On an earlier disconnect, usage may genuinely be unknown, but the upstream attempt disappears just the same.
- **Raw Responses:** `Server.forwardResponsesStream` preserves usage it has already parsed, but returns before seeing terminal usage on an earlier disconnect (`gateway/server.go:347–350, 379–395`). `Server.handleCodexResponsesPassthrough` then records `Requests: 1` with zero token counts when `usage == nil` (`gateway/server.go:319–333`). In the JSON branch, `io.MultiWriter(w, &capture)` writes to the client **first**: a failed client write can prevent the accounting capture from receiving even bytes already read from upstream (`gateway/server.go:304–315`).
- **Platform:** `Server.handleOpenAIPlatformPassthrough` marks the candidate healthy before copying the body, discards the `io.Copy` error, and passes a potentially partial embeddings response to `buildPlatformCreditDelta`, which ignores the JSON decode error (`gateway/server.go:574–596, 623–640`).

**Fix:** Finalize accounting once per upstream attempt, independently of client delivery. Persist known usage even on a final-write error; represent missing usage as *unknown*, not measured zero. Keep upstream completion, downstream delivery, and billing as separate outcomes. Do not replay a committed request or keep generating indefinitely just to obtain usage.

### 2. High — Both SSE parsers treat a line as an event

`nextCodexEvent` unmarshals each `data:` line immediately and returns without waiting for the blank-line event boundary (`gateway/codex_bridge.go:616–645`). `Server.forwardResponsesStream` makes the same assumption for accounting, although it forwards the original lines (`gateway/server.go:347–389`). TCP fragmentation alone is **not** the bug: both readers assemble lines across reads. The failures are at the SSE framing layer:

- A valid event with multiple `data:` fields can fail JSON parsing because the fields are decoded separately instead of joined with `\n`.
- A complete-looking `response.completed` data line followed by EOF **without the event delimiter** can be accepted as completion.
- The bridge has a 4 MiB scanner-token limit; crossing it yields a generic error that defaults to retryable, potentially cooling and retrying another account for a local parser limit (`gateway/codex_bridge.go:606–612, 642–645, 743–751, 893–899, 496–515`). The passthrough has the opposite problem: `ReadBytes('\n')` can accumulate an unbounded line and delays forwarding its first byte until newline or read termination (`gateway/server.go:341–348, 385–390`).

**Fix:** Share a bounded SSE event decoder. Dispatch only at an event boundary, join `data:` fields, and distinguish truncated input and local size limits from account failures. On passthrough, keep byte forwarding independent of metadata parsing. Apply upstream failure state when the failure event is decoded: currently a downstream write or flush error can skip an already-observed exhaustion or cooldown signal (`gateway/server.go:375–401`).

### 3. High — The normal client has no post-header timeout

`NewServer` sets a ten-minute `Transport.ResponseHeaderTimeout`, not a response-body timeout (`gateway/server.go:47–55`). The `http.Client{Timeout: 10 * time.Minute}` alternatives run only when `s.HTTPClient == nil`, which is **not** the constructor path (`gateway/codex_bridge.go:575–579`; `gateway/server.go:495–498`). An upstream that sends headers and then stalls can therefore retain a handler and connection while the downstream stays connected. `forwardResponsesStream` even waits for EOF after observing completion (`gateway/server.go:379–382, 403–405`). A slow downstream can likewise block a write or flush; no write deadline is set in the shown helpers (`gateway/server.go:428–446`).

This is a **missing deadline**, not evidence of a detached request context: both upstream request constructors use the inbound context (`gateway/codex_bridge.go:554`; `gateway/server.go:511`). Add a configurable body-idle deadline that cancels the upstream read, a bounded downstream write deadline, and an overall request budget spanning retries. Finalize usage at the terminal event rather than waiting for EOF. Keep upstream timeouts distinct from client cancellation.

### 4. High at a trust boundary — Passthrough header filtering is incomplete

`Server.handleOpenAIPlatformPassthrough` strips `Authorization`, `X-API-Key`, and several gateway fields, but copies every other inbound header before installing the pooled bearer key (`gateway/server.go:516–525`). That can forward gateway-origin cookies, proxy credentials, forwarding metadata, and unrelated custom authentication headers. Both passthrough handlers also copy upstream response headers wholesale, including `Set-Cookie` and connection-scoped fields (`gateway/server.go:294–300, 574–582`). The Codex request builder itself uses fresh headers; the broad **request-side** copy is specific to Platform (`gateway/codex_bridge.go:554–573`).

**Fix:** Allowlist request headers needed by the supported routes. In both directions, remove hop-by-hop headers **and headers named by `Connection`**. Do not expose upstream cookies on the gateway origin. Restrict redirects to trusted upstream origins: the constructed client has no explicit redirect policy, and custom headers such as `ChatGPT-Account-Id` should not rely on Go’s redirect protection for `Authorization` (`gateway/server.go:47–55`; `gateway/codex_bridge.go:570–579`).

### 5. Medium — Account identity headers can leak or survive failover

`setQuotaswitchHeaders` exposes the display name and only sets `X-Quotaswitch-Account-ID` when its argument is nonempty; it never deletes a previous value (`gateway/server.go:121–127`). Callers pass `cand.Account.Name` as the “ID,” and `StoredAccount.DisplayName()` can include an email address (`gateway/codex_bridge.go:481`; `gateway/server.go:299, 581`; `codex/storage.go:61–71`). The chat bridge stages those headers **before** it knows whether the attempt will commit, then can retry or emit an unrelated gateway error using the same `ResponseWriter` (`gateway/codex_bridge.go:481–492, 506–517`). A failed candidate’s identity can consequently appear on an error, or its optional ID can survive into the next attempt.

**Fix:** Install identity headers only when committing the selected response. Clear the gateway-owned header namespace first, including any upstream-supplied values. If clients need account affinity, expose a deliberate opaque identifier rather than an alias or email.

### 6. Medium — Partial deltas suppress valid final content

`streamCodexToChatCompletions` uses one response-wide `sentText` flag. Any text delta suppresses the `output_item.done` text fallback for **all** message items (`gateway/codex_bridge.go:705–712, 765–769, 851–864`). Tool arguments have a similar issue: one delta suppresses the entire final argument snapshot, and an unmapped `item_id` silently routes arguments to tool index zero (`gateway/codex_bridge.go:802–824, 830–850`). If the final snapshot contains a suffix absent from the deltas, the client can receive truncated text or invalid tool arguments followed by a successful finish.

**Fix:** Track emitted content per item and content part, and arguments per tool item. Emit only a verified missing suffix from a final snapshot; treat inconsistent snapshots or unknown tool IDs as protocol errors rather than assigning them to tool zero.

**First regression test:** make only the final `[DONE]` write fail after a valid `response.completed` containing usage. Assert **one ledger entry with the exact upstream counts, no retry, and a downstream-delivery failure**. That isolates the highest-confidence defect without depending on socket timing.

## Pass 5: Go Unit & Race-Detector (`go test -race`) Regression Test Spec
*Seat: `gameoflifan@gmail.com` · in=`24,981` · cached=`0` (`0.0%`) · out=`11,120` · `225.09s`*

**Two behaviors need new tests that initially fail:** Chat Completions drops `prompt_cache_key`, and Codex routing has no partition key. `ChatCompletionRequest` and `codexResponsesRequest` both omit the field (`gateway/codex_bridge.go:28–40,123–133`), whereas native Responses preserves unknown JSON members (`gateway/server.go:228–245`). `Pool.CodexCandidatesWithMode` takes only an account filter and an auth preference; it uses one preferred OAuth account and rotates ready API keys (`gateway/pool.go:182–193,239–254,279–299`).

For the sticky-routing tests below, the proposed contract is: a nonempty `prompt_cache_key` selects a stable account within the eligible routing scope; explicit filters take precedence; a successful failover rebinds that partition. An absent key retains existing routing. The tests do **not** require a particular hash or assume different keys must land on different accounts.

**Fixture:** Use a fresh `Pool` and `codex.Store` under `t.TempDir()` per test, a fixed `Pool.Now`, synthetic credentials, and a scripted `Server.HTTPClient` transport that records the outbound credential label and decoded JSON. Use a local fake OAuth issuer for refreshes. Synchronize with channels, not sleeps; use deadlines only to fail a stuck test. Give each concurrent request its own recorder and each refresh caller its own `CodexCandidate` value.

### `prompt_cache_key` forwarding

1. **`TestChatPromptCacheKeyOnWire`** — POST raw JSON to `/v1/chat/completions`; do not construct the request with the Go struct under test. Table-test a present key such as `"team/α:07"` and an absent key. For the present case, script a pre-commit 429 followed by success on a second account. Assert the **top-level outbound JSON** contains the exact decoded string on both attempts. For the absent case, assert the member is absent, not `null` or `""`. This catches a fix to only one of the two structs. Sources: `Server.handleChatCompletions`, `gateway/server.go:190–215`; `buildCodexResponsesPayload` and the marshal/retry path, `gateway/codex_bridge.go:361–372,406–440`.

2. **`TestResponsesPromptCacheKeySurvivesRewrite`** — POST the same present/absent cases to `/v1/responses`, omitting `store` and `instructions`. Inspect the actual upstream body: assert key value or absence, `store:false`, and `instructions == defaultDirectInstructions`. Checking the defaults proves the test exercised JSON rewriting rather than an unchanged byte copy. Source: `Server.handleCodexResponsesPassthrough`, `gateway/server.go:218–249,257–270`.

### Concurrent OAuth refresh

3. **`TestEnsureCodexTokenConcurrentExpiredCopiesRotateOnce`** — Take 16 copies of one expired stored candidate before starting any workers. Release them together into `Pool.EnsureCodexToken(ctx, &copy, false)`. Have the issuer accept the old refresh token once and return a new refresh token and a fresh access JWT. Assert exactly one exchange, 16 nil errors, the new access token in every copy, and the new token pair after reloading the store. This tests both locking and the **reload under the lock**. Sources: `gateway/pool.go:310–346,365–381`; `Store.PersistRefreshedAccount`, `codex/storage.go:445–470`.

4. **`TestConcurrentChat401sShareForcedRefresh`** — Start with an unexpired access token and one OAuth account. Hold all eight old-bearer upstream requests at a barrier before returning 401 to any of them. Accept the replacement bearer and return a completed stream. Assert eight old-bearer attempts, **one** token exchange, eight replacement-bearer attempts, and eight successful client responses. The barrier matters: without it, later callers might never exercise `force=true`. Sources: `Server.handleCodexChatCompletions`, `gateway/codex_bridge.go:440–450`; `Pool.EnsureCodexToken`, `gateway/pool.go:321–346`.

5. **`TestEnsureCodexTokenDifferentAccountsRefreshInParallel`** — Start one expired candidate each for A and B. Each issuer call announces its refresh-token label and blocks. Require **both labels to arrive before releasing either response**; then assert each stored account contains only its own replacement pair. A single global network-refresh lock fails this test even if all final tokens would otherwise be correct. Make the barriers cancellable for failure cleanup. Sources: `Pool.codexRefreshLocks` and its account-ID lookup, `gateway/pool.go:53–60,325–329`; persistence, `codex/storage.go:455–469`.

### Sticky-partition routing

6. **`TestColdPartitionConcurrentRequestsChooseOneAccount`** — Seed three ready, equal-expiry API keys. Send 12 concurrent requests with the same previously unseen partition key. Record each first upstream attempt and withhold upstream responses until all 12 arrive. Assert one distinct credential label, then release the responses and assert success. This detects implementations that establish affinity only *after* the first response and round-robin the other in-flight requests. Current competing selection logic: `Pool.CodexCandidatesWithMode`, `gateway/pool.go:279–297`.

7. **`TestPartitionsStayIndependentAndRebindAfterFailover`** — With just A and B, mark B cooling and prime partition P on A. Clear B’s cooldown, mark A cooling, and prime Q on B. Advance the fake clock past A’s cooldown. Interleave P, Q, P, Q and assert **A, B, A, B**. Now return a pre-commit 429 from A for P and success from B. Assert attempt order **A, B**; after cooldown expiry P still uses B. Priming through eligibility avoids assuming a hash distribution. Sources: `Pool.MarkCooldown`, `MarkHealthy`, and `isCoolingDown`, `gateway/pool.go:89–131`; ready-before-cooling ordering, `gateway/pool.go:256–299`.

8. **`TestPartitionHonorsFiltersAndPreservesUnkeyedRouting`** — Prime an unfiltered key on API account A. With that same key, explicitly select B using `X-Quotaswitch-Account`; only B may be attempted. In an OAuth-only request using `X-Quotaswitch-Auth`, only an OAuth credential may be attempted. Repeat the original unfiltered request and assert it still uses A. As an unkeyed control, on a fresh pool with two equal-expiry API keys in store order A, B, assert three sequential API-only requests use **A, B, A**. Sources: header extraction, `gateway/codex_bridge.go:406–410`; mode, account filtering, and rotation, `gateway/pool.go:182–193,216–237,281–297`.

### SSE errors and the replay boundary

9. **`TestChatSSEFailureBeforeCommitRetriesCleanly`** — A returns HTTP 200 SSE with `response.created` followed by `response.failed` carrying `response.error.code:"rate_limit_exceeded"`. B returns a text delta and `response.completed`. Assert attempts **A, B**, A cooling, only B’s ID/text in downstream chunks, one finish chunk, and exactly one `[DONE]`. A created event alone must not commit the response. Sources: `codexEventFailure`, `gateway/codex_bridge.go:642–685`; retry versus committed-error handling, `gateway/codex_bridge.go:475–513`.

10. **`TestChatSSEFailureAfterCommitNeverReplays`** — Repeat test 9, but have A emit a nonempty text delta before failing. Run a second row where the first emitted delta is a function-call item. In both rows assert only A was attempted, the client received the delta exactly once followed by one SSE `error` object, and there is **no** success finish or `[DONE]`. The tool-call row guards against replaying work, not just duplicating text. Sources: `Server.handleCodexChatCompletions` and `writeChatStreamError`, `gateway/codex_bridge.go:486–510,687–694`.

11. **`TestNextCodexEventRejectsFalseCompletion`** — Table-test `nextCodexEvent` with empty EOF, `[DONE]` alone, malformed `data:` JSON, and `response.incomplete` with `incomplete_details.reason:"max_output_tokens"`. Assert an error in every row. Via `codexStreamFailureStatus`, assert `(502, true)` for truncation/malformed input and `(502, false)` for this explicit incomplete event. Use `response.completed` as the positive control. Sources: `gateway/codex_bridge.go:600–640,642–685`.

12. **`TestResponsesPassthroughForwardsSSEFailureWithoutMarkingHealthy`** — Return native SSE with `event: response.failed` and a `data:` object containing a nested `rate_limit_exceeded` error **but no `type`**, followed by a blank line and `[DONE]`. Assert byte-for-byte forwarding, one upstream attempt, and the account **still cooling** after the handler returns. This checks event-name fallback, distinguishes native forwarding from Chat translation, and catches an erroneous `MarkHealthy` that would clear the cooldown. Sources: `Server.handleCodexResponsesPassthrough`, `gateway/server.go:290–318`; `Server.forwardResponsesStream`, `gateway/server.go:341–382,385–405`.

Run with `go test -race -count=20 ./...`. The barriers and exact attempt counts test the concurrency contract; `-race` additionally checks for unsynchronized memory access.
