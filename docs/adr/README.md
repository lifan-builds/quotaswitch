# Architecture decision records

ADRs capture decisions that outlive a single change: architecture, security, public behavior, or a durable process. They explain **why**, not just what.

## When to add one

- The decision changes a baseline architecture or dependency direction.
- The decision changes credential handling, storage, public CLI behavior, CI, or release.
- A future maintainer would otherwise ask "why is it done this way?"
- An execution plan reveals a decision worth preserving.

Do not rewrite accepted ADRs. Add a new ADR and mark the old one superseded.

## Naming

```text
NNNN-short-title.md
```

Use the next number. Statuses: `Proposed`, `Accepted`, `Superseded`, `Deprecated`.

## Index

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-repository-harness.md) | Accepted | Adopt a repository harness with mechanical guardrails, without loop engineering |
| [0002](0002-antigravity-desktop-support.md) | Accepted | Support Antigravity Desktop profiles alongside CLI with default dual-target switching and --relaunch |
| [0003](0003-unified-codex-provider-support.md) | Accepted | Unify OpenAI Codex multi-account switching and quota rotation into agy-auth |
| [0004](0004-rename-to-quotaswitch.md) | Accepted | Rename project, Go module, npm package, and primary CLI to quotaswitch with agy-auth compatibility alias |
| [0005](0005-local-llm-gateway.md) | Accepted | Local LLM gateway so Antigravity can use pooled Codex GPT models and pooled Antigravity quota |
| [0006](0006-gateway-owns-antigravity-failover.md) | Accepted | Preserve Antigravity model selection while the gateway handles account failover |

## Template

Use [_template.md](_template.md).

## Related

- [Engineering guides](../engineering/README.md)
- [Execution plans](../exec-plans/README.md)
- [Harness](../harness.md)
