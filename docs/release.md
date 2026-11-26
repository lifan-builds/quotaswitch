# Release process

The npm package is `quotaswitch`; the installed command is `quotaswitch` (with `agy-auth` as compatibility alias).

## One-time setup

1. Log in to npm and confirm publish access to `quotaswitch`:

   ```shell
   npm login
   npm whoami
   ```

2. Create a granular access token with read/write access to `quotaswitch` (or configure npm trusted publishing / OIDC) and add it to GitHub as `NPM_TOKEN`:

   ```shell
   gh secret set NPM_TOKEN --repo lifan-builds/quotaswitch
   ```

   The release workflow skips npm publishing when `NPM_TOKEN` is absent, so GitHub releases still work without it.

## Versioning

`package.json`, `internal/cli/app.go` (`Version`), and `CHANGELOG.md` are kept in sync (`harness docs` enforces this). The npm build also injects `package.json`'s version into the Go binary via `-X github.com/lifan-builds/quotaswitch/internal/cli.Version`.

1. Update `"version"` in `package.json` and `var Version` in `internal/cli/app.go`.
2. Add an entry to `CHANGELOG.md`.
3. Run `make verify-ci`, then commit and tag:

   ```shell
   git add package.json internal/cli/app.go CHANGELOG.md
   git commit -m "chore(release): vX.Y.Z"
   git tag vX.Y.Z
   git push origin main && git push origin vX.Y.Z
   ```

4. The `release` workflow verifies tag/version parity, runs `go run ./cmd/harness verify --profile ci`, builds all six platform binaries into `npm/vendor/`, publishes `quotaswitch` to npm when `NPM_TOKEN` is set, and creates a GitHub release with the binaries and `checksums.txt`.

## Post-release check

```shell
npm view quotaswitch version
gh release view "v$(node -p 'require("./package.json").version')" \
  --json assets --jq '.assets[].name'
quotaswitch version --check
```
