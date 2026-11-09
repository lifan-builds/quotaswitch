# ADR-0005: Local LLM gateway for pooled Codex and Antigravity models

- **Status:** Accepted
- **Date:** 2026-11-08
- **Decision makers:** lifan-builds
- **Consulted:**
- **Related:** [`docs/adr/0003-unified-codex-provider-support.md`](0003-unified-codex-provider-support.md)

## Context

Codex (ChatGPT plan) quota and Antigravity quota were only usable inside their own apps. Antigravity's `language_server` supports an admin-configured OpenAI-compatible gateway (`~/.antigravity/settings.json` → `gateway` + `models`), which lets it list and call arbitrary model IDs.

## Decision

1. Add `internal/gateway`, a local HTTP server (`127.0.0.1:18455`) that exposes `/v1/chat/completions`, `/v1/responses`, `/v1/models`, and `/healthz`.
2. Route by model ID: GPT/Codex slugs go to `chatgpt.com/backend-api/codex/responses` using the stored Codex accounts; `gemini-*`, `claude-*`, and `gpt-oss-*` go to CloudCode `v1internal:streamGenerateContent` using the stored Antigravity accounts.
3. On 401 refresh the token once; on 401/403/429/5xx or quota errors, cool the account down and retry the next account in the pool.
4. Every streamed chunk carries exactly one choice (the `language_server` OpenAI converter rejects chunks without choices); usage rides on the final chunk.
5. `quotaswitch gateway enable` writes the `gateway` and `models` keys (preserving other keys, backing up the original once) and installs the `com.lfan.quotaswitch-gateway` LaunchAgent. The model list combines visible models from `~/.codex/models_cache.json` with the live CloudCode `fetchAvailableModels` agent list (built-in fallback).
6. `quotaswitch gateway disable` removes those keys and the LaunchAgent.

## Consequences

- **Good:** GPT models run inside Antigravity's chat, tools, and subagents; quota from all accounts in both pools is used before a request fails.
- **Neutral:** The Codex backend and CloudCode `v1internal` are private endpoints; changes upstream may require updates.
- **Bad:** When the gateway is enabled, Antigravity depends on the local service being up. `gateway disable` reverts.

## Confirmation

- `internal/gateway` unit tests (failover, streaming, tool calls, settings round-trip) and `make verify-full`.
