// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gitcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lifan-builds/quotaswitch/internal/harness/repo"
)

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = root
	command.Env = repo.CleanGitEnv()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = repo.CleanGitEnv()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestCheckBotTrailers(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "file.txt")
	runGit(t, root, "commit", "-m", "feat(harness): safe commit")

	findings, err := CheckBotTrailers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}

	runGit(t, root, "commit", "--allow-empty", "-m", "feat(harness): bad commit\n\nCo-authored-by: CommandCodeBot <noreply@commandcode.ai>")
	findings, err = CheckBotTrailers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v", findings)
	}
}

func TestGitHelpersIgnoreWorktreeEnvOverrides(t *testing.T) {
	parentRepo := t.TempDir()
	runGit(t, parentRepo, "init", "-q")
	runGit(t, parentRepo, "config", "user.email", "parent@example.com")
	runGit(t, parentRepo, "config", "user.name", "Parent User")
	if err := os.WriteFile(filepath.Join(parentRepo, "README.md"), []byte("parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, parentRepo, "add", "README.md")
	runGit(t, parentRepo, "commit", "-m", "chore: initial parent commit")
	parentHead := gitOutput(t, parentRepo, "rev-parse", "HEAD")

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	runGit(t, parentRepo, "worktree", "add", "-q", "-b", "wt-branch", worktreeDir)
	worktreeGitDir := gitOutput(t, worktreeDir, "rev-parse", "--absolute-git-dir")
	parentCommonDir := gitOutput(t, parentRepo, "rev-parse", "--absolute-git-dir")

	// Simulate the git environment passed to a pre-commit hook inside a worktree.
	t.Setenv("GIT_DIR", worktreeGitDir)
	t.Setenv("GIT_WORK_TREE", worktreeDir)
	t.Setenv("GIT_IMPLICIT_WORK_TREE", "1")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(worktreeGitDir, "index"))
	t.Setenv("GIT_COMMON_DIR", parentCommonDir)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(parentCommonDir, "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(parentCommonDir, "objects"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'user.name=Leaked Config'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_PREFIX", "")

	TestCheckBotTrailers(t)

	if got := gitOutput(t, parentRepo, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("parent HEAD changed from %s to %s", parentHead, got)
	}
	if got := gitOutput(t, worktreeDir, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("worktree HEAD changed from %s to %s", parentHead, got)
	}
	if got := gitOutput(t, parentRepo, "config", "--get", "user.name"); got != "Parent User" {
		t.Fatalf("parent user.name mutated to %q", got)
	}
	if got := gitOutput(t, parentRepo, "config", "--get", "core.bare"); got != "false" {
		t.Fatalf("parent core.bare mutated to %q", got)
	}
	if got := gitOutput(t, worktreeDir, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree status dirtied: %q", got)
	}
}
