// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package docs

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMarkdown(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLinks(t *testing.T) {
	root := t.TempDir()
	writeMarkdown(t, root, "README.md", "[ok](docs/a.md)\n[web](https://example.com)\n[anchor](#x)\n[file](file:///path/to/file)\n")
	writeMarkdown(t, root, "docs/a.md", "# A\n")
	findings, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}
}

func TestCheckMissingLink(t *testing.T) {
	root := t.TempDir()
	writeMarkdown(t, root, "README.md", "[bad](docs/missing.md)\n")
	findings, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v", findings)
	}
}

func TestCheckIgnoresFencedLinks(t *testing.T) {
	root := t.TempDir()
	writeMarkdown(t, root, "README.md", "```\n[bad](missing.md)\n```\n")
	findings, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}
}
