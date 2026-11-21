package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultExecModel        = "gpt-6.2-sol"
	defaultBoundedCodeModel = "gpt-6.2-astra"
	defaultAPIKeyExecModel  = "gpt-5.4"
)

const codexExecWorkerContract = `You are executing a bounded task delegated via quotaswitch.
Strict execution rules:
1. Follow the exact goal directly with zero scope creep. Touch ONLY the listed Allowed Output Files (or leave the workspace strictly READ-ONLY when none are listed; any out-of-scope edits are reverted automatically).
2. Zero defensive code: never add speculative nil/type/bounds guards for trusted internal callers, fallback shims, backwards-compatibility layers, wrapper structs/classes, retry wrappers, or unrequested CLI/env/config flags.
3. Verify your work by running the real CLI, build, or test command. Stop only for external human gates (CAPTCHA, missing login/2FA, unapproved payment). Never print secrets, tokens, passwords, or OTPs.
4. Report pure factual results directly with zero hedging, caveats, or disclaimers.`

const defaultCodexExecSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["completed","blocked","failed"]},"summary":{"type":"string"},"commands_run":{"type":"array","items":{"type":"string"}},"data":{"type":"string","description":"Concrete outputs, changed files, test results, or exact failure/block reason."}},"required":["status","summary","commands_run","data"],"additionalProperties":false}`

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*f = append(*f, trimmed)
		}
	}
	return nil
}

type gitEntrySnapshot struct {
	XY      string
	Exists  bool
	Content []byte
}

func (a *App) cmdCodexExec(args []string) int {
	fs := a.newFlagSet("codex")
	var rawAllow stringListFlag
	fs.Var(&rawAllow, "allow", "allowed workspace output file paths or globs (repeatable; omit for strictly read-only workspace)")
	model := fs.String("model", "", "optional Codex model override (default: gpt-6.2-sol, or gpt-6.2-astra with --allow)")
	modelShort := fs.String("m", "", "shorthand for --model")
	cd := fs.String("cd", ".", "working directory")
	cdShort := fs.String("C", "", "shorthand for --cd")
	schema := fs.String("schema", "", "optional JSON Schema file for --output-schema")
	jsonFlag := fs.Bool("json", false, "enforce structured JSON output")
	codexBin := fs.String("codex-bin", "codex", "path to codex CLI binary")
	timeout := fs.Duration("timeout", 10*time.Minute, "subprocess timeout")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}

	reqModel := strings.TrimSpace(*model)
	if reqModel == "" {
		reqModel = strings.TrimSpace(*modelShort)
	}
	workDirRaw := *cd
	if strings.TrimSpace(*cdShort) != "" {
		workDirRaw = *cdShort
	}
	workDir, err := filepath.Abs(workDirRaw)
	if err != nil {
		return a.failCode("invalid_path", "", "could not resolve working directory %q: %v", workDirRaw, err)
	}

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" && a.In != nil && !a.TTY {
		if raw, rErr := io.ReadAll(io.LimitReader(a.In, 1<<20)); rErr == nil {
			goal = strings.TrimSpace(string(raw))
		}
	}
	if goal == "" {
		return a.usageFor("codex", "codex exec requires a goal argument or stdin")
	}

	gitRoot := gitTopLevel(workDir)
	inGit := gitRoot != ""
	allowed := normalizeAllowPatterns(rawAllow, workDir, gitRoot)
	var before map[string]gitEntrySnapshot
	if inGit {
		before, err = snapshotGitStatus(gitRoot)
		if err != nil {
			return a.failCode("git_error", "", "could not snapshot git status: %v", err)
		}
	}

	wantJSON := a.JSON || *jsonFlag
	prompt := buildCodexExecPrompt(goal, allowed)
	codexHome := a.gatewayCodexHome()

	tmpDir, err := os.MkdirTemp("", "quotaswitch-codex-exec-*")
	if err != nil {
		return a.failCode("io_error", "", "could not create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	outMsgPath := filepath.Join(tmpDir, "last_message.txt")
	schemaPath := ""
	if strings.TrimSpace(*schema) != "" {
		schemaPath, err = filepath.Abs(strings.TrimSpace(*schema))
		if err != nil {
			return a.failCode("invalid_path", "", "could not resolve schema path: %v", err)
		}
	} else if wantJSON {
		schemaPath = filepath.Join(tmpDir, "exec_schema.json")
		if err := os.WriteFile(schemaPath, []byte(defaultCodexExecSchema), 0o600); err != nil {
			return a.failCode("io_error", "", "could not write schema file: %v", err)
		}
	}

	var reverted []string
	defer func() {
		if inGit {
			_, reverted = enforceAllowedScope(gitRoot, before, allowed)
			if len(reverted) > 0 {
				fmt.Fprintf(a.Err, "quotaswitch: reverted out-of-scope file(s): %s\n", strings.Join(reverted, ", "))
			}
		}
	}()

	runOnce := func() (string, string, int, bool) {
		useAPIKey := liveCodexUsesAPIKey(codexHome)
		cmdArgs := buildCodexExecArgs(workDir, inGit, resolveCodexExecModel(reqModel, len(allowed) > 0, useAPIKey), useAPIKey, outMsgPath, schemaPath, prompt)
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, *codexBin, cmdArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", stderr.String(), 124, true
		}
		exitCode := 0
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = 1
				if stderr.Len() == 0 {
					stderr.WriteString(runErr.Error())
				}
			}
		}
		finalMsg := ""
		if raw, rErr := os.ReadFile(outMsgPath); rErr == nil {
			finalMsg = strings.TrimSpace(string(raw))
		}
		if finalMsg == "" {
			finalMsg = strings.TrimSpace(stdout.String())
		}
		return finalMsg, stderr.String(), exitCode, false
	}

	finalMsg, errText, exitCode, timedOut := runOnce()
	if !timedOut && exitCode != 0 && isCodexQuotaError(finalMsg+"\n"+errText) && a.CodexStore != nil {
		if a.trySilentCodexRotate() {
			_ = os.Remove(outMsgPath)
			finalMsg, errText, exitCode, timedOut = runOnce()
		}
	}

	if timedOut {
		fmt.Fprintf(a.Err, "error: quotaswitch codex exec timed out after %s\n", timeout.String())
		return 124
	}
	if finalMsg != "" {
		fmt.Fprintln(a.Out, finalMsg)
	}
	if exitCode != 0 && strings.TrimSpace(errText) != "" {
		trimmed := strings.TrimSpace(errText)
		if len(trimmed) > 2000 {
			trimmed = trimmed[len(trimmed)-2000:]
		}
		fmt.Fprintln(a.Err, trimmed)
	}
	return exitCode
}

func (a *App) trySilentCodexRotate() bool {
	origOut, origErr, origJSON := a.Out, a.Err, a.JSON
	var sink bytes.Buffer
	a.Out = &sink
	a.Err = &sink
	a.JSON = false
	defer func() {
		a.Out, a.Err, a.JSON = origOut, origErr, origJSON
	}()
	return a.rotateCodexOnce(false, false, false, true) == 0
}

func isCodexQuotaError(text string) bool {
	lower := strings.ToLower(text)
	for _, tok := range []string{"out of credits", "usage_limit_exceeded", "usage limit"} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

func liveCodexUsesAPIKey(codexHome string) bool {
	if strings.TrimSpace(codexHome) == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	val, _ := doc["OPENAI_API_KEY"].(string)
	return strings.TrimSpace(val) != ""
}

func resolveCodexExecModel(requested string, hasAllow, useAPIKey bool) string {
	eff := strings.TrimSpace(requested)
	if useAPIKey {
		if eff == "" || strings.HasPrefix(strings.ToLower(eff), "gpt-6") {
			return defaultAPIKeyExecModel
		}
		return eff
	}
	if eff != "" {
		return eff
	}
	if hasAllow {
		return defaultBoundedCodeModel
	}
	return defaultExecModel
}

func buildCodexExecPrompt(goal string, allowed []string) string {
	var scopeBlock string
	if len(allowed) > 0 {
		var b strings.Builder
		b.WriteString("Allowed Output Files (touch no other workspace files):\n")
		for _, p := range allowed {
			b.WriteString("- ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		scopeBlock = strings.TrimRight(b.String(), "\n")
	} else {
		scopeBlock = "Workspace Files: READ-ONLY. Do not create, modify, or delete repository files."
	}
	return fmt.Sprintf("%s\n\n%s\n\nGoal:\n%s\n", codexExecWorkerContract, scopeBlock, strings.TrimSpace(goal))
}

func buildCodexExecArgs(workDir string, inGit bool, model string, useAPIKey bool, outMsgPath, schemaPath, prompt string) []string {
	args := []string{
		"exec",
		"--ephemeral",
		"-C", workDir,
		"-s", "danger-full-access",
		"-c", "mcp_servers.node_repl.enabled=false",
		"-c", `plugins."computer-use@openai-bundled".enabled=false`,
		"-c", `plugins."browser@openai-bundled".enabled=false`,
		"-c", `plugins."chrome@openai-bundled".enabled=false`,
		"-c", `plugins."unified-computer-use@openai-bundled".enabled=false`,
		"-c", `plugins."codex-app-tools@openai-bundled".enabled=false`,
	}
	if useAPIKey {
		args = append(args,
			"-c", fmt.Sprintf(`agents.default_subagent_model="%s"`, defaultAPIKeyExecModel),
			"-c", `agents.default_subagent_reasoning_effort="high"`,
		)
	}
	args = append(args, "-o", outMsgPath)
	if !inGit {
		args = append(args, "--skip-git-repo-check")
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args, prompt)
	return args
}

func gitTopLevel(workDir string) string {
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return ""
	}
	if resolved, rErr := filepath.EvalSymlinks(top); rErr == nil {
		return resolved
	}
	return top
}

func normalizeAllowPatterns(raw []string, workDir, gitRoot string) []string {
	base := workDir
	if gitRoot != "" {
		base = gitRoot
	}
	if resolvedWork, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolvedWork
	}
	var out []string
	for _, pat := range raw {
		clean := strings.TrimSpace(pat)
		if clean == "" {
			continue
		}
		hasTrailingSlash := strings.HasSuffix(clean, "/")
		p := clean
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.ToSlash(rel)
		} else {
			p = filepath.ToSlash(filepath.Clean(clean))
		}
		p = strings.TrimPrefix(p, "./")
		if hasTrailingSlash && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

func matchesAllowedPattern(rel string, patterns []string) bool {
	norm := strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, pat := range patterns {
		clean := strings.TrimPrefix(strings.TrimSpace(pat), "./")
		if clean == "" {
			continue
		}
		if norm == clean {
			return true
		}
		if strings.HasSuffix(clean, "/") && strings.HasPrefix(norm, clean) {
			return true
		}
		if ok, err := filepath.Match(clean, norm); err == nil && ok {
			return true
		}
	}
	return false
}

func snapshotGitStatus(gitRoot string) (map[string]gitEntrySnapshot, error) {
	out, err := exec.Command("git", "-C", gitRoot, "status", "--porcelain=v1", "-z", "-uall").Output()
	if err != nil {
		return nil, err
	}
	entries := make(map[string]gitEntrySnapshot)
	parts := bytes.Split(out, []byte{0})
	for i := 0; i < len(parts); i++ {
		raw := parts[i]
		if len(raw) < 4 {
			continue
		}
		xy := string(raw[:2])
		rel := string(raw[3:])
		if strings.ContainsAny(xy, "RC") {
			i++
		}
		full := filepath.Join(gitRoot, filepath.FromSlash(rel))
		info, statErr := os.Lstat(full)
		snap := gitEntrySnapshot{XY: xy}
		if statErr == nil && info.Mode().IsRegular() {
			if data, rErr := os.ReadFile(full); rErr == nil {
				snap.Exists = true
				snap.Content = data
			}
		}
		entries[rel] = snap
	}
	return entries, nil
}

func enforceAllowedScope(gitRoot string, before map[string]gitEntrySnapshot, allowed []string) ([]string, []string) {
	after, err := snapshotGitStatus(gitRoot)
	if err != nil {
		return nil, nil
	}
	allKeys := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		allKeys[k] = struct{}{}
	}
	for k := range after {
		allKeys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(allKeys))
	for k := range allKeys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var kept, reverted []string
	for _, rel := range sorted {
		prev, hadPrev := before[rel]
		curr, hasCurr := after[rel]
		if hadPrev == hasCurr && (!hadPrev || (prev.XY == curr.XY && prev.Exists == curr.Exists && bytes.Equal(prev.Content, curr.Content))) {
			continue
		}
		if matchesAllowedPattern(rel, allowed) {
			kept = append(kept, rel)
			continue
		}

		full := filepath.Join(gitRoot, filepath.FromSlash(rel))
		if !hadPrev {
			_ = exec.Command("git", "-C", gitRoot, "reset", "-q", "HEAD", "--", rel).Run()
			tracked := exec.Command("git", "-C", gitRoot, "ls-files", "--error-unmatch", "--", rel).Run() == nil
			if !tracked || (hasCurr && curr.XY == "??") {
				_ = os.Remove(full)
			} else {
				_ = exec.Command("git", "-C", gitRoot, "checkout", "--", rel).Run()
			}
		} else {
			if hasCurr && len(curr.XY) > 0 && len(prev.XY) > 0 && curr.XY[0] != prev.XY[0] {
				_ = exec.Command("git", "-C", gitRoot, "reset", "-q", "HEAD", "--", rel).Run()
			}
			if !prev.Exists {
				_ = os.Remove(full)
			} else {
				_ = os.MkdirAll(filepath.Dir(full), 0o755)
				_ = os.WriteFile(full, prev.Content, 0o644)
			}
		}
		reverted = append(reverted, rel)
	}
	return kept, reverted
}
