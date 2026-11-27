// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultExecModel         = "gpt-6.2-astra"
	defaultBoundedCodeModel  = "gpt-6.2-astra"
	defaultAPIKeyExecModel   = "gpt-5.4"
	maxCodexExecQuotaRetries = 4
	codexRotateLockWait      = 15 * time.Second
	codexRotateLockStale     = 60 * time.Second
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
	Mode    os.FileMode
}

func (a *App) cmdCodexExec(args []string) int {
	fs := a.newFlagSet("codex")
	var rawAllow stringListFlag
	fs.Var(&rawAllow, "allow", "allowed workspace output file paths or globs (repeatable; omit for strictly read-only workspace)")
	var config []string
	addConfig := func(value string) error {
		config = append(config, value)
		return nil
	}
	fs.Func("config", "Codex configuration override (repeatable)", addConfig)
	fs.Func("c", "shorthand for --config", addConfig)
	rawPrompt := fs.Bool("raw-prompt", false, "pass the goal verbatim as the Codex prompt")
	model := fs.String("model", "", "optional Codex model override (default: gpt-6.2-astra, or gpt-5.4 on API keys)")
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

	goal := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(goal) == "" && a.In != nil && !a.TTY {
		input := a.In
		if !*rawPrompt {
			input = io.LimitReader(input, 1<<20)
		}
		if raw, rErr := io.ReadAll(input); rErr == nil {
			goal = string(raw)
		}
	}
	if strings.TrimSpace(goal) == "" {
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
	prompt := goal
	if !*rawPrompt {
		prompt = buildCodexExecPrompt(goal, allowed)
	}
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

	runOnce := func(remaining time.Duration) (string, string, int, bool) {
		if remaining <= 0 {
			return "", "", 124, true
		}
		useAPIKey := liveCodexUsesAPIKey(codexHome)
		cmdArgs := buildCodexExecArgs(workDir, inGit, resolveCodexExecModel(reqModel, len(allowed) > 0, useAPIKey), useAPIKey, outMsgPath, schemaPath, config, prompt)
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		defer cancel()

		cmd := exec.CommandContext(ctx, *codexBin, cmdArgs...)
		cmd.WaitDelay = 2 * time.Second
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

	deadline := time.Now().Add(*timeout)
	var finalMsg, errText string
	var exitCode int
	var timedOut bool
	for attempt := 0; ; attempt++ {
		seatBefore := a.activeCodexAccountID()
		finalMsg, errText, exitCode, timedOut = runOnce(time.Until(deadline))
		if timedOut || exitCode == 0 || !isCodexQuotaError(finalMsg+"\n"+errText) || a.CodexStore == nil {
			break
		}
		if attempt >= maxCodexExecQuotaRetries || time.Until(deadline) <= 5*time.Second {
			break
		}
		if !a.rotateCodexAfterQuotaError(seatBefore) {
			break
		}
		_ = os.Remove(outMsgPath)
	}

	if timedOut {
		if trimmed := strings.TrimSpace(errText); trimmed != "" {
			if len(trimmed) > 2000 {
				trimmed = trimmed[len(trimmed)-2000:]
			}
			fmt.Fprintln(a.Err, trimmed)
		}
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

func (a *App) activeCodexAccountID() string {
	if a.CodexStore == nil {
		return ""
	}
	data, err := a.CodexStore.Load()
	if err != nil || data.ActiveAccountID == nil {
		return ""
	}
	return strings.TrimSpace(*data.ActiveAccountID)
}

func acquireCodexRotateLock(lockDir string) func() {
	if strings.TrimSpace(lockDir) == "" {
		return func() {}
	}
	_ = os.MkdirAll(lockDir, 0o700)
	lockPath := filepath.Join(lockDir, "codex-exec-rotate.lock")
	deadline := time.Now().Add(codexRotateLockWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > codexRotateLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (a *App) rotateCodexAfterQuotaError(failedSeatID string) bool {
	if a.CodexStore == nil {
		return false
	}
	release := acquireCodexRotateLock(a.CodexStore.SwitcherDir)
	defer release()

	if failedSeatID != "" {
		if current := a.activeCodexAccountID(); current != "" && current != failedSeatID {
			return true
		}
	}
	if !a.trySilentCodexRotate() {
		return false
	}
	if failedSeatID != "" {
		if after := a.activeCodexAccountID(); after == "" || after == failedSeatID {
			return false
		}
	}
	return true
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
	return a.rotateCodexOnce(false, false, true, true) == 0
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

func buildCodexExecArgs(workDir string, inGit bool, model string, useAPIKey bool, outMsgPath, schemaPath string, config []string, prompt string) []string {
	args := []string{
		"exec",
		"--ephemeral",
		"-C", workDir,
		"-s", "danger-full-access",
		"-c", `service_tier="fast"`,
		"-c", "features.memories=false",
		"-c", "notify=[]",
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
	for _, value := range config {
		args = append(args, "-c", value)
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
	args = append(args, "--", prompt)
	return args
}

var codexGitOverrideVars = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_AUTHOR_DATE":                  true,
	"GIT_AUTHOR_EMAIL":                 true,
	"GIT_AUTHOR_NAME":                  true,
	"GIT_COMMON_DIR":                   true,
	"GIT_COMMITTER_DATE":               true,
	"GIT_COMMITTER_EMAIL":              true,
	"GIT_COMMITTER_NAME":               true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NAMESPACE":                    true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_QUARANTINE_PATH":              true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_SUPER_PREFIX":                 true,
	"GIT_WORK_TREE":                    true,
}

func cleanGitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if codexGitOverrideVars[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanGitEnv()
	return cmd
}

func gitTopLevel(workDir string) string {
	out, err := gitCommand(workDir, "rev-parse", "--show-toplevel").Output()
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
			if workDir != base && isFilenameGlob(p) {
				p = filepath.Join("**", p)
			}
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

func isFilenameGlob(pattern string) bool {
	return !strings.Contains(filepath.ToSlash(pattern), "/") && strings.ContainsAny(pattern, "*?[")
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
		name := norm
		if isFilenameGlob(clean) {
			name = path.Base(norm)
		}
		if matchPathGlob(clean, name) {
			return true
		}
	}
	return false
}

func matchPathGlob(pattern, name string) bool {
	part, rest, more := strings.Cut(pattern, "/")
	component, tail, nested := strings.Cut(name, "/")
	if part == "**" {
		if !more || matchPathGlob(rest, name) {
			return true
		}
		return nested && matchPathGlob(pattern, tail)
	}
	if matched, _ := path.Match(part, component); !matched || more != nested {
		return false
	}
	return !more || matchPathGlob(rest, tail)
}

func snapshotGitStatus(gitRoot string) (map[string]gitEntrySnapshot, error) {
	out, err := gitCommand(gitRoot, "status", "--porcelain=v1", "-z", "-uall").Output()
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
				snap.Mode = info.Mode().Perm()
				if snap.Mode == 0 {
					snap.Mode = 0o644
				}
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
		if hadPrev == hasCurr && (!hadPrev || (prev.XY == curr.XY && prev.Exists == curr.Exists && prev.Mode == curr.Mode && bytes.Equal(prev.Content, curr.Content))) {
			continue
		}
		if matchesAllowedPattern(rel, allowed) {
			kept = append(kept, rel)
			continue
		}

		full := filepath.Join(gitRoot, filepath.FromSlash(rel))
		if !hadPrev {
			_ = gitCommand(gitRoot, "reset", "-q", "HEAD", "--", rel).Run()
			tracked := gitCommand(gitRoot, "ls-files", "--error-unmatch", "--", rel).Run() == nil
			if !tracked || (hasCurr && curr.XY == "??") {
				_ = os.Remove(full)
			} else {
				_ = gitCommand(gitRoot, "checkout", "--", rel).Run()
			}
		} else {
			if hasCurr && len(curr.XY) > 0 && len(prev.XY) > 0 && curr.XY[0] != prev.XY[0] {
				_ = gitCommand(gitRoot, "reset", "-q", "HEAD", "--", rel).Run()
			}
			if !prev.Exists {
				_ = os.Remove(full)
			} else {
				_ = os.MkdirAll(filepath.Dir(full), 0o755)
				if !curr.Exists {
					if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
						continue
					}
				} else if curr.Mode&0o200 == 0 {
					_ = os.Chmod(full, curr.Mode|0o200)
				}
				if err := os.WriteFile(full, prev.Content, prev.Mode); err == nil {
					_ = os.Chmod(full, prev.Mode)
				}
			}
		}
		reverted = append(reverted, rel)
	}
	return kept, reverted
}
