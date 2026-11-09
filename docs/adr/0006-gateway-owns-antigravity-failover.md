# ADR-0006: Gateway owns automatic Antigravity failover

- **Status:** Accepted
- **Date:** 2026-11-08
- **Related:** [Local gateway](0005-local-llm-gateway.md)

## Context

The native quota watcher continued switching the Hub to Gemini and restarting
the language server after the gateway was enabled. Native account quota does
not determine which pooled GPT model a gateway conversation can use.

## Decision

When the Desktop gateway settings contain an enabled gateway with a URL,
ordinary Antigravity rotation and live-picker automatic switching defer to the
gateway. They preserve the selected model, live credential, and Desktop process.
The watcher rereads this configuration each cycle. Codex desktop rotation still
runs under `--provider=all`. Explicit `rotate --force` remains available but
does not select a Claude/Gemini model while gateway mode is enabled.

## Consequences

GPT selections are no longer overwritten by the native quota waterfall. A
gateway outage must be repaired or disabled; silently switching native accounts
would not repair it. Disabling gateway mode restores native rotation.

## Confirmation

`internal/cli/gateway_rotation_test.go` verifies the early return, unchanged
credentials, configuration reload, and explicit-force behavior.
