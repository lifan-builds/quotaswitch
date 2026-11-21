package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
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

func newCodexExecRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
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
	return repo, runGit
}

func writeFakeCodexExec(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex executable uses sh")
	}
	path := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexExecEnforcesScopeAndRevertsStagedOrDeletedFiles(t *testing.T) {
	fakeCodex := writeFakeCodexExec(t, `out=""
model=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -m) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cd "$CODEX_EXEC_TEST_REPO"
printf 'package pkg\nvar x = 2\n' > pkg/allowed.go
printf 'package main\nvar slop = true\n' > forbidden.go
git add forbidden.go
printf 'extra\n' > staged_new.go
git add staged_new.go
rm -f untracked.sh
chmod 644 dirty.sh
printf '{"status":"completed","summary":"ran %s","commands_run":["sh"],"data":"ok"}\n' "$model" > "$out"
`)
	repo, runGit := newCodexExecRepo(t)
	runGit("config", "core.filemode", "true")
	t.Setenv("CODEX_EXEC_TEST_REPO", repo)
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(sub, "allowed.go")
	forbiddenFile := filepath.Join(repo, "forbidden.go")
	preDirty := filepath.Join(repo, "dirty.sh")
	preUntracked := filepath.Join(repo, "untracked.sh")

	for p, body := range map[string]string{
		allowedFile:   "package pkg\nvar x = 1\n",
		forbiddenFile: "package main\nvar keep = true\n",
		preDirty:      "#!/bin/sh\necho original\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", ".")
	runGit("commit", "-qm", "init")

	if err := os.WriteFile(preDirty, []byte("#!/bin/sh\necho local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(preDirty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preUntracked, []byte("#!/bin/sh\necho keep-me\n"), 0o755); err != nil {
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
	for path, want := range map[string]string{
		preDirty:     "#!/bin/sh\necho local\n",
		preUntracked: "#!/bin/sh\necho keep-me\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s restored as %q (error %v), want %q", filepath.Base(path), got, err, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Errorf("%s mode = %04o, want 0755", filepath.Base(path), mode)
		}
	}
}

func TestCodexExecForwardsRawPromptAndConfig(t *testing.T) {
	fakeCodex := writeFakeCodexExec(t, `printf '%s\000' "$@" > "$CODEX_EXEC_TEST_ARGS"
printf 'allowed\n' > "$CODEX_EXEC_TEST_REPO/allowed.txt"
printf 'forbidden\n' > "$CODEX_EXEC_TEST_REPO/forbidden.txt"
`)
	prompt := " \tReturn this exact instruction.\nKeep both newlines.\n\n "
	configs := []string{
		`mcp_servers.worker.args=["a,b", "c"]`,
		`mcp_servers.worker.env={ONE="d,e", TWO="f"}`,
		`mcp_servers.worker.command = "run,g"  `,
		`mcp_servers.worker.cwd="folder,h"`,
	}
	for _, tc := range []struct {
		name      string
		stdin     string
		args      []string
		apiKey    string
		wantModel string
	}{
		{name: "argument", args: []string{prompt}, wantModel: "gpt-6.2-astra"},
		{name: "stdin", stdin: prompt, apiKey: "test-api-key", wantModel: "gpt-5.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, runGit := newCodexExecRepo(t)
			runGit("commit", "--allow-empty", "-qm", "init")
			argsPath := filepath.Join(t.TempDir(), "args")
			t.Setenv("CODEX_EXEC_TEST_ARGS", argsPath)
			t.Setenv("CODEX_EXEC_TEST_REPO", repo)

			env := newTestEnv(t, false)
			app, _, stderr := env.app(tc.stdin)
			if tc.apiKey != "" {
				store := codex.NewStore(filepath.Join(env.dir, "store"), filepath.Join(env.dir, "codex-home"))
				if err := store.WriteLiveAuth(codex.StoredAccount{
					AuthMode: codex.AuthModeAPIKey,
					AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &tc.apiKey},
				}, testNow); err != nil {
					t.Fatal(err)
				}
				app.CodexStore = store
			}
			args := []string{
				"codex", "exec", "-C", repo, "--codex-bin", fakeCodex,
				"--allow", "allowed.txt", "--raw-prompt",
				"-c", configs[0], "--config", configs[1], "-c", configs[2], "--config=" + configs[3],
			}
			if code := app.Run(append(args, tc.args...)); code != 0 {
				t.Fatalf("codex exec exit = %d, stderr = %s", code, stderr.String())
			}
			raw, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			forwarded := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			if got := forwarded[len(forwarded)-1]; got != prompt {
				t.Errorf("forwarded prompt = %q, want %q", got, prompt)
			}
			var gotConfigs []string
			var gotModel string
			for i, arg := range forwarded[:len(forwarded)-1] {
				if (arg == "-c" || arg == "--config") && strings.HasPrefix(forwarded[i+1], "mcp_servers.worker.") {
					gotConfigs = append(gotConfigs, forwarded[i+1])
				}
				if arg == "-m" || arg == "--model" {
					gotModel = forwarded[i+1]
				}
			}
			if !slices.Equal(gotConfigs, configs) {
				t.Errorf("forwarded config = %q, want %q", gotConfigs, configs)
			}
			if gotModel != tc.wantModel {
				t.Errorf("forwarded model = %q, want %q", gotModel, tc.wantModel)
			}
			if got, err := os.ReadFile(filepath.Join(repo, "allowed.txt")); err != nil || string(got) != "allowed\n" {
				t.Errorf("allowed file = %q, error = %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(repo, "forbidden.txt")); !os.IsNotExist(err) {
				t.Errorf("forbidden file survived raw prompt scope enforcement: %v", err)
			}
		})
	}
}

func TestCodexExecRotatesAfterQuotaErrorOnHealthyActiveSeat(t *testing.T) {
	fakeCodex := writeFakeCodexExec(t, `if grep -Fq 'exec-active-access' "$CODEX_HOME/auth.json"; then
  printf 'active\n' >> "$CODEX_EXEC_TEST_ATTEMPTS"
  printf 'usage_limit_exceeded\n' >&2
  exit 1
fi
grep -Fq 'exec-next-access' "$CODEX_HOME/auth.json"
printf 'next\n' >> "$CODEX_EXEC_TEST_ATTEMPTS"
printf 'retried on next seat\n'
`)
	env := newTestEnv(t, false)
	store := codex.NewStore(filepath.Join(env.dir, "store"), filepath.Join(env.dir, "codex-home"))
	attemptsPath := filepath.Join(env.dir, "attempts")
	t.Setenv("CODEX_HOME", store.CodexHome)
	t.Setenv("CODEX_EXEC_TEST_ATTEMPTS", attemptsPath)
	var nextID string
	for _, alias := range []string{"active", "next"} {
		workspace := "exec-" + alias
		acct, _, err := store.UpsertTokens(codex.TokenData{
			IDToken:      makeCodexIDToken(t, alias+"@example.com", alias, workspace, "team"),
			AccessToken:  workspace + "-access",
			RefreshToken: workspace + "-refresh",
			AccountID:    &workspace,
		}, alias, alias == "active", testNow)
		if err != nil {
			t.Fatal(err)
		}
		if alias == "next" {
			nextID = acct.ID
		}
	}
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`)
	}))
	defer usageServer.Close()

	app, out, stderr := env.app("")
	app.CodexStore = store
	app.CodexUsage = codex.NewUsageClient()
	app.CodexUsage.UsageURL = usageServer.URL
	app.CodexUsage.Now = func() time.Time { return testNow }
	app.CodexOps = &codex.ProcessOps{Inspect: func() codex.ProcessSummary { return codex.ProcessSummary{} }}
	if code := app.Run([]string{"codex", "exec", "-C", env.dir, "--codex-bin", fakeCodex, "Retry on a usable seat"}); code != 0 {
		t.Fatalf("codex exec exit = %d, stdout = %q, stderr = %q", code, out.String(), stderr.String())
	}
	if got, err := os.ReadFile(attemptsPath); err != nil || string(got) != "active\nnext\n" {
		t.Errorf("Codex attempts = %q, error = %v, want one attempt per seat", got, err)
	}
	if out.String() != "retried on next seat\n" || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q", out.String(), stderr.String())
	}
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if data.ActiveAccountID == nil || *data.ActiveAccountID != nextID {
		t.Error("retry seat was not saved as the active account")
	}
}

func TestCodexExecEnforcesRecursiveAllowGlobs(t *testing.T) {
	fakeCodex := writeFakeCodexExec(t, `cd "$CODEX_EXEC_TEST_REPO"
mkdir -p pkg/deeper/dir other
for path in a.go pkg/a.go pkg/deeper/dir/a.go pkg/a.txt pkg/deeper/dir/a.txt other/a.go other/a.txt; do
  printf 'edited\n' > "$path"
done
`)
	for _, tc := range []struct {
		name    string
		workDir string
		pattern string
		want    []string
	}{
		{name: "globstar", workDir: ".", pattern: "pkg/**/*.txt", want: []string{"pkg/a.txt", "pkg/deeper/dir/a.txt"}},
		{name: "root_basename", workDir: ".", pattern: "*.go", want: []string{"a.go", "pkg/a.go", "pkg/deeper/dir/a.go", "other/a.go"}},
		{name: "scoped_basename", workDir: "pkg", pattern: "*.go", want: []string{"pkg/a.go", "pkg/deeper/dir/a.go"}},
		{name: "exact_basename", workDir: "pkg", pattern: "a.go", want: []string{"pkg/a.go"}},
		{name: "directory_prefix", workDir: "pkg", pattern: "deeper/", want: []string{"pkg/deeper/dir/a.go", "pkg/deeper/dir/a.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, runGit := newCodexExecRepo(t)
			runGit("commit", "--allow-empty", "-qm", "init")
			workDir := filepath.Join(repo, tc.workDir)
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_EXEC_TEST_REPO", repo)
			env := newTestEnv(t, false)
			app, _, stderr := env.app("")
			if code := app.Run([]string{
				"codex", "exec", "-C", workDir, "--codex-bin", fakeCodex,
				"--allow", tc.pattern, "Edit the allowed files",
			}); code != 0 {
				t.Fatalf("codex exec exit = %d, stderr = %s", code, stderr.String())
			}
			for _, file := range []string{
				"a.go", "pkg/a.go", "pkg/deeper/dir/a.go", "pkg/a.txt", "pkg/deeper/dir/a.txt", "other/a.go", "other/a.txt",
			} {
				got, err := os.ReadFile(filepath.Join(repo, file))
				if slices.Contains(tc.want, file) {
					if err != nil || string(got) != "edited\n" {
						t.Errorf("allowed file %s = %q, error = %v", file, got, err)
					}
				} else if !os.IsNotExist(err) {
					t.Errorf("out-of-scope file %s survived, error = %v", file, err)
				}
			}
		})
	}
}
