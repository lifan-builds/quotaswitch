# Release process

The npm package is `quotaswitch`; the installed command is `quotaswitch` (with `agy-auth` as compatibility alias).

## One-time setup

Configure npm Trusted Publishing (GitHub Actions OIDC) on the `quotaswitch` package — no `NPM_TOKEN` secret is needed:

1. Open [npmjs.com/package/quotaswitch/access](https://www.npmjs.com/package/quotaswitch/access) (Package Settings → **Trusted Publisher** → **GitHub Actions**).
2. Add a trusted publisher with:
   - **Organization or user:** `lifan-builds`
   - **Repository:** `quotaswitch`
   - **Workflow filename:** `release.yml`
   - **Environment name:** *(leave blank)*

The release workflow uses `permissions: id-token: write` and npm >= 11.5.1 (`npm i -g npm@latest`) so `npm publish --access public` mints a short-lived OIDC token and attaches Sigstore provenance automatically.

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

4. The `release` workflow verifies tag/version parity, runs `go run ./cmd/harness verify --profile ci`, builds all six platform binaries into `npm/vendor/`, publishes `quotaswitch` to npm via OIDC Trusted Publishing, and creates a GitHub release with the binaries and `checksums.txt` (skipping creation if the GitHub release already exists).

To publish an existing tag without re-tagging (for example, after configuring Trusted Publishing):

```shell
gh workflow run release.yml -R lifan-builds/quotaswitch --ref main -f tag=v0.5.1
```

## Post-release check

```shell
npm view quotaswitch version
gh release view "v$(node -p 'require("./package.json").version')" \
  --json assets --jq '.assets[].name'
quotaswitch version --check
```
