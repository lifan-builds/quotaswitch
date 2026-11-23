// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lifan-builds/quotaswitch/internal/backup"
)

func TestResolveAccountsDir(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	native := filepath.Join(base, "quotaswitch", "codex")
	legacy := filepath.Join(home, ".codex-switcher")
	if got := resolveAccountsDir(home, base, "", ""); got != native {
		t.Fatalf("new installation: got %q, want %q", got, native)
	}
	if err := backup.WriteFileAtomic(filepath.Join(legacy, "accounts.json"), []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveAccountsDir(home, base, "", ""); got != legacy {
		t.Fatalf("legacy installation: got %q, want %q", got, legacy)
	}
	isolated := t.TempDir()
	if got := resolveAccountsDir(home, base, "", isolated); got != filepath.Join(isolated, "codex") {
		t.Fatalf("explicit config home must not use real legacy accounts: %q", got)
	}
	explicit := t.TempDir()
	if got := resolveAccountsDir(home, base, explicit, isolated); got != explicit {
		t.Fatalf("explicit legacy override must win: %q", got)
	}
	if err := backup.WriteFileAtomic(filepath.Join(native, "accounts.json"), []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveAccountsDir(home, base, "", ""); got != native {
		t.Fatalf("native store must win after migration: %q", got)
	}
	if info, err := os.Stat(filepath.Join(native, "accounts.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("native store must remain private: info=%v err=%v", info, err)
	}
}
