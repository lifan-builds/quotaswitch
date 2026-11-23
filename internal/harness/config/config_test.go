// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersionSync(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"version":"1.2.3"}`)
	writeFile(t, root, "internal/cli/app.go", `package cli
var Version = "1.2.3"
`)
	writeFile(t, root, "CHANGELOG.md", "## [1.2.3] - 2026-01-01\n")
	if err := Check(root); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersionDrift(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"version":"1.2.4"}`)
	writeFile(t, root, "internal/cli/app.go", `package cli
var Version = "1.2.3"
`)
	writeFile(t, root, "CHANGELOG.md", "## [1.2.3] - 2026-01-01\n")
	if err := Check(root); err == nil {
		t.Fatal("expected version drift")
	}
}
