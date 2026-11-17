# Repository Harness

`quotaswitch` uses `cmd/harness` (`internal/harness`) to enforce repository invariants mechanically across local development and CI.

## Commands

```shell
make verify        # go run ./cmd/harness verify --profile fast
make verify-full   # go run ./cmd/harness verify --profile full
make verify-ci     # go run ./cmd/harness verify --profile ci
make lint          # go run ./cmd/harness lint
make doctor        # go run ./cmd/harness doctor
make hooks         # go run ./cmd/harness install-hooks
```

Individual subcommands: `verify`, `lint`, `secrets`, `docs`, `contracts`, `doctor`, `install-hooks`, `commit-msg`, `list`, `version`.

## Profiles

| Step | fast | full | ci |
|---|:---:|:---:|:---:|
| `gofmt -l` | ✓ | ✓ | ✓ |
| Custom Go AST lints | ✓ | ✓ | ✓ |
| Secret scan | ✓ | ✓ | ✓ |
| Docs links + version sync | ✓ | ✓ | ✓ |
| Contract fixtures | ✓ | ✓ | ✓ |
| `go vet ./...` | ✓ | ✓ | ✓ |
| `go build ./...` | ✓ | ✓ | ✓ |
| `go test -short ./...` | ✓ | ✓ |  |
| `go test -race ./...` |  | ✓ |  |
| `go test -race -count=1 -shuffle=on ./...` |  |  | ✓ |
| Build current npm target |  | ✓ | ✓ |
| Node shim smoke test |  | ✓ | ✓ |
| `npm pack --dry-run` |  | ✓ |  |
| Build all npm targets |  |  | ✓ |
| `npm pack` to a temp directory |  |  | ✓ |

## Custom lints

| Rule | Scope | Catches |
|---|---|---|
| `output-boundary` | `internal/{agy,registry,oauth,codex,cli}` | `fmt.Print*`, `log.*`, `os.Stdout/Stderr` outside `internal/cli/app.go` |
| `json-boundary` | `internal/cli` | `json.NewEncoder` outside `internal/cli/render.go` |
| `keyring-boundary` | all files | `go-keyring` use outside `internal/agy/storage.go` |
| `atomic-writes` | `internal/{backup,agy,registry,desktop,codex}` | `os.WriteFile`, `os.Create`, write-mode `os.OpenFile` outside `backup.WriteFileAtomic` |
| `file-permissions` | `internal/{agy,registry,codex}` | Literal modes other than `0600`/`0700` |
| `architecture` | all packages | Disallowed internal imports; production importing `internal/harness` |
| `no-panic` | `internal/*` non-test | `panic(...)` |
| `no-os-exit` | non-`cmd` packages | `os.Exit` |
| `test-isolation` | `_test.go` | `DefaultStore`, `DefaultStorage`, real keyring use |
| `secret-literals` | non-ignored text files | OAuth client secrets/IDs, npm/GitHub/OpenAI/AWS/Slack tokens, private keys |
| `bot-coauthor` | git history | `CommandCodeBot` or bot co-author trailers |
| `docs-links` | Markdown | Broken relative links |
| `version-sync` | repository metadata | `package.json`, `internal/cli/app.go`, and `CHANGELOG.md` version drift |
| `command-help` | CLI regression test | Command or alias without help |

## Contracts

`harness/fixtures/` holds independent fixtures validated by `harness contracts`:

- `credential.consumer.json` — full agy credential document with a fake ID token.
- `credential.minimal.json` — access-token-only document.
- `credential.invalid.json` — missing token object; must be rejected.
- `registry.valid.json` — valid registry index.
- `registry.invalid.json` — path-traversal account ID; must be rejected.

## Hooks & CI

`make hooks` configures `core.hooksPath=.githooks`:
- `.githooks/pre-commit` runs `harness verify --profile fast`.
- `.githooks/commit-msg` validates Conventional Commits (`feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert`, lowercase scope) and rejects bot co-author trailers.

`.github/workflows/ci.yml` and `.github/workflows/release.yml` run `go run ./cmd/harness verify --profile ci`.

## Related docs

- [ADRs](adr/README.md)
- [Contributing](../CONTRIBUTING.md)
- [Internals](internals.md)
- [Release process](release.md)
- [AGENTS.md](../AGENTS.md)
