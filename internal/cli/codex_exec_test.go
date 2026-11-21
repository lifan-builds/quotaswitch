package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveCodexExecModel(t *testing.T) {
	if got := resolveCodexExecModel("", false, false); got != "gpt-6.2-sol" {
		t.Fatalf("read-only OAuth default = %q, want gpt-6.2-sol", got)
	}
	if got := resolveCodexExecModel("", true, false); got != "gpt-6.2-astra" {
		t.Fatalf("--allow OAuth default = %q, want gpt-6.2-astra", got)
	}
	if got := resolveCodexExecModel("gpt-6.2-astra", true, true); got != "gpt-5.4" {
		t.Fatalf("API-key fallback for gpt-6.2-astra = %q, want gpt-5.4", got)
	}
	if got := resolveCodexExecModel("gpt-5.4-mini", false, true); got != "gpt-5.4-mini" {
		t.Fatalf("explicit API-key model = %q, want gpt-5.4-mini", got)
	}
}

func TestCodexExecEnforcesScopeAndRevertsStagedOrDeletedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake codex test uses sh")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(sub, "allowed.go")
	forbiddenFile := filepath.Join(repo, "forbidden.go")
	preDirty := filepath.Join(repo, "dirty.go")
	preUntracked := filepath.Join(repo, "notes.txt")

	for p, body := range map[string]string{
		allowedFile:   "package pkg\nvar x = 1\n",
		forbiddenFile: "package main\nvar keep = true\n",
		preDirty:      "package main\nvar v = 0\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", ".")
	runGit("commit", "-qm", "init")

	if err := os.WriteFile(preDirty, []byte("package main\nvar v = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preUntracked, []byte("keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeCodex := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/bin/sh
out=""
model=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -m) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'package pkg\nvar x = 2\n' > "` + allowedFile + `"
printf 'package main\nvar slop = true\n' > "` + forbiddenFile + `"
git -C "` + repo + `" add forbidden.go
printf 'extra\n' > "` + filepath.Join(repo, "staged_new.go") + `"
git -C "` + repo + `" add staged_new.go
rm -f "` + preUntracked + `"
printf '{"status":"completed","summary":"ran %s","commands_run":["sh"],"data":"ok"}\n' "$model" > "$out"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t, false)
	app, outBuf, errBuf := env.app("")
	code := app.Run([]string{
		"codex", "exec",
		"-C", sub,
		"--allow", "allowed.go",
		"--json",
		"--codex-bin", fakeCodex,
		"Update x to 2",
	})
	if code != 0 {
		t.Fatalf("codex exec failed (code=%d): stderr=%s stdout=%s", code, errBuf.String(), outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "ran gpt-6.2-astra") {
		t.Fatalf("expected gpt-6.2-astra model with --allow, got %q", outBuf.String())
	}
	if got, _ := os.ReadFile(allowedFile); string(got) != "package pkg\nvar x = 2\n" {
		t.Fatalf("allowed file not preserved: %q", string(got))
	}
	if got, _ := os.ReadFile(forbiddenFile); string(got) != "package main\nvar keep = true\n" {
		t.Fatalf("staged forbidden file was not reverted: %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(repo, "staged_new.go")); !os.IsNotExist(err) {
		t.Fatalf("staged new out-of-scope file survived: err=%v", err)
	}
	if got, _ := os.ReadFile(preUntracked); string(got) != "keep-me\n" {
		t.Fatalf("deleted pre-existing untracked file not restored: %q", string(got))
	}
	if got, _ := os.ReadFile(preDirty); string(got) != "package main\nvar v = 1\n" {
		t.Fatalf("pre-existing dirty file changed: %q", string(got))
	}
}
