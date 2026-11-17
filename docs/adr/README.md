# Architecture decision records

ADRs capture decisions that outlive a single change: architecture, security, public behavior, or release contracts. Do not rewrite accepted ADRs; add a new ADR and mark the old one superseded.

## Index

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-repository-harness.md) | Accepted | Adopt a repository harness with mechanical guardrails |
| [0002](0002-antigravity-desktop-support.md) | Accepted | Support Antigravity Desktop profiles alongside CLI with default dual-target switching and --relaunch |
| [0003](0003-unified-codex-provider-support.md) | Accepted | Unify OpenAI Codex multi-account switching and quota rotation into agy-auth |
| [0004](0004-rename-to-quotaswitch.md) | Accepted | Rename project, Go module, npm package, and primary CLI to quotaswitch with agy-auth compatibility alias |
| [0005](0005-local-llm-gateway.md) | Accepted | Local LLM gateway so Antigravity can use pooled Codex GPT models and pooled Antigravity quota |
| [0006](0006-gateway-owns-antigravity-failover.md) | Accepted | Preserve Antigravity model selection while the gateway handles account failover |
| [0007](0007-codex-owned-storage.md) | Accepted | Give Codex accounts native Quotaswitch storage with legacy fallback and optional login activation |

## Related

- [Harness](../harness.md)
- [Internals](../internals.md)
