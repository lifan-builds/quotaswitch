// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import "fmt"

func (a *App) cmdPath(args []string) int {
	fs := a.newFlagSet("path")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("path", "path takes no arguments")
	}
	payload := map[string]any{
		"registry":          a.Store.Dir,
		"registry_file":     a.Store.Registry,
		"accounts_dir":      a.Store.AccountsDir,
		"encryption_key":    a.Store.Vault.KeyPath,
		"agy_token_file":    a.Storage.FilePath,
		"keyring_service":   a.Storage.Service,
		"keyring_user":      a.Storage.User,
		"keyring_available": a.Storage.KeyringAvailable(),
	}
	if a.JSON {
		return a.okJSON(payload)
	}
	fmt.Fprintf(a.Out, "registry:      %s\n", a.Store.Dir)
	fmt.Fprintf(a.Out, "accounts:      %s\n", a.Store.AccountsDir)
	fmt.Fprintf(a.Out, "encryption:    %s\n", a.Store.Vault.KeyPath)
	fmt.Fprintf(a.Out, "agy token:     %s\n", a.Storage.FilePath)
	fmt.Fprintf(a.Out, "keyring:       %s / %s\n", a.Storage.Service, a.Storage.User)
	return 0
}
