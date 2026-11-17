# Contributing

`quotaswitch` is a Go CLI with an npm distribution layer.

## Setup

- Go 1.24+
- Node.js 18+ for the npm shim/package
- Antigravity CLI and/or Codex CLI installed for manual testing

```shell
git clone https://github.com/lifan-builds/quotaswitch.git
cd quotaswitch
make verify
make build
```

## Verification

`make verify` runs formatting, custom Go AST lints, secret scanning, docs/version checks, contract fixtures, `go vet`, build, and short tests. `make verify-full` adds `-race` tests and npm packaging; CI runs `make verify-ci`. See [docs/harness.md](docs/harness.md).

`make hooks` installs the pre-commit and commit-msg hooks. Commits use Conventional Commits (`feat(scope): summary`, lowercase scope) with no co-authored-by or bot trailers.

Add an ADR under [docs/adr/](docs/adr/) when a decision changes architecture, credential handling, public CLI behavior, CI, or release.

## Project layout

| Path | Responsibility |
|------|----------------|
| `cmd/quotaswitch` | main CLI entrypoint |
| `cmd/harness` | repository verification CLI |
| `internal/agy` | Antigravity credential JSON, keyring/file backend |
| `internal/registry` | Antigravity encrypted snapshots, metadata, queries |
| `internal/desktop` | Antigravity Desktop profile snapshots and process control |
| `internal/oauth` | Google PKCE login and client fingerprint discovery |
| `internal/codex` | OpenAI Codex account/live-auth storage, PKCE login, usage/credits, recovery |
| `internal/gateway` | local OpenAI-compatible pooling gateway (`127.0.0.1:18455/v1`) |
| `internal/backup` | atomic file writes and timestamped backup retention |
| `internal/cli` | commands, rendering, interactive picker, live view |
| `internal/harness` | AST lints, secret scan, docs/version checks, contract runner |
| `harness/fixtures` | credential and registry contract fixtures |
| `npm/bin` | Node shim for prebuilt binaries |
| `scripts` | cross-compilation and helper scripts |
| `docs` | user guide, internals, harness, ADRs, release process |

## Testing with isolated credentials

Never point tests at the real keyring. Use a temporary `QUOTASWITCH_HOME` and the file backend:

```shell
TMP=$(mktemp -d)
QUOTASWITCH_HOME="$TMP/config" \
QUOTASWITCH_STORAGE=file \
AGY_AUTH_AGY_TOKEN_FILE="$TMP/token" \
./bin/quotaswitch list
```

The opt-in real-keyring test uses a throwaway service name:

```shell
AGY_AUTH_KEYRING_TEST=1 go test ./internal/agy -run TestKeyringRoundTrip -v
```

## Security

See [SECURITY.md](SECURITY.md). Do not open public issues for credential leaks, token handling flaws, or auth bypasses.
