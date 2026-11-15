package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const codexBundleIdentifier = "com.openai.codex"

type ProcessSummary struct {
	CLIPIDs           []int    `json:"cli_pids,omitempty"`
	DesktopPIDs       []int    `json:"desktop_pids,omitempty"`
	DesktopHelperPIDs []int    `json:"desktop_helper_pids,omitempty"`
	DesktopBundles    []string `json:"desktop_bundles,omitempty"`
	AppServerPIDs     []int    `json:"app_server_pids,omitempty"`
}

func (p ProcessSummary) Running() bool {
	return len(p.CLIPIDs) > 0 || len(p.DesktopPIDs) > 0
}

func (p ProcessSummary) SessionPIDs() []int {
	out := make([]int, 0, len(p.DesktopPIDs)+len(p.DesktopHelperPIDs)+len(p.CLIPIDs))
	out = append(out, p.DesktopPIDs...)
	out = append(out, p.DesktopHelperPIDs...)
	out = append(out, p.CLIPIDs...)
	return out
}

func (p ProcessSummary) DesktopQuitPIDs() []int {
	out := make([]int, 0, len(p.DesktopPIDs)+len(p.DesktopHelperPIDs))
	out = append(out, p.DesktopPIDs...)
	out = append(out, p.DesktopHelperPIDs...)
	return out
}

type RecoveryTarget struct {
	SessionID    string
	Path         string
	Size         int64
	QuotaStopped bool
}

type ProcessOps struct {
	Inspect           func() ProcessSummary
	QuitDesktop       func(bundles []string, pids []int) error
	StopCLI           func(pids []int) error
	RestartAppServers func(pids []int) int
	ReopenDesktop     func(bundles []string) error
	CaptureRecovery   func(codexHome string, pids []int, now time.Time) []RecoveryTarget
	ResumeRecovery    func(codexHome string, targets []RecoveryTarget) []string
}

func DefaultProcessOps() *ProcessOps {
	return &ProcessOps{
		Inspect:           inspectCodexProcesses,
		QuitDesktop:       quitDesktopBundles,
		StopCLI:           stopProcesses,
		RestartAppServers: restartAppServerProcesses,
		ReopenDesktop:     reopenDesktopBundles,
		CaptureRecovery:   captureInterruptedSessions,
		ResumeRecovery:    resumeInterruptedSessions,
	}
}

func inspectCodexProcesses() ProcessSummary {
	raw, err := exec.Command("ps", "-axo", "pid=,tty=,command=").Output()
	if err != nil {
		return ProcessSummary{}
	}
	return parseCodexProcessOutput(raw, os.Getpid(), readMacBundleIdentifier)
}

func parseCodexProcessOutput(raw []byte, selfPID int, bundleID func(string) string) ProcessSummary {
	var out ProcessSummary
	bundleSet := map[string]bool{}
	bundleIDCache := map[string]string{}
	cachedBundleID := func(bundle string) string {
		if v, ok := bundleIDCache[bundle]; ok {
			return v
		}
		var v string
		if bundleID != nil {
			v = bundleID(bundle)
		}
		bundleIDCache[bundle] = v
		return v
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		tty := fields[1]
		cmd := strings.Join(fields[2:], " ")

		if strings.Contains(cmd, "openai.chatgpt-") && strings.Contains(cmd, "app-server") {
			out.AppServerPIDs = append(out.AppServerPIDs, pid)
			continue
		}
		if bundle := extractDesktopBundle(cmd, cachedBundleID); bundle != "" {
			out.DesktopPIDs = append(out.DesktopPIDs, pid)
			if !bundleSet[bundle] {
				bundleSet[bundle] = true
				out.DesktopBundles = append(out.DesktopBundles, bundle)
			}
			continue
		}
		if isDesktopBundledAppServer(cmd, cachedBundleID) {
			out.DesktopHelperPIDs = append(out.DesktopHelperPIDs, pid)
			continue
		}
		exe := fields[2]
		base := filepath.Base(exe)
		if base == "codex" && tty != "??" && tty != "?" && !strings.Contains(cmd, "app-server") && !strings.Contains(cmd, "quotaswitch") && !strings.Contains(cmd, "agy-auth") {
			out.CLIPIDs = append(out.CLIPIDs, pid)
		}
	}
	return out
}

func extractDesktopBundle(cmd string, bundleID func(string) string) string {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, "/") || strings.Contains(cmd, "Helper") || strings.Contains(cmd, "--type=") || strings.Contains(cmd, "app-server") {
		return ""
	}
	for _, spec := range []struct {
		execSuffix   string
		bundleSuffix string
		checkID      bool
	}{
		{execSuffix: "/Codex.app/Contents/MacOS/Codex", bundleSuffix: "/Codex.app", checkID: false},
		{execSuffix: "/ChatGPT.app/Contents/MacOS/ChatGPT", bundleSuffix: "/ChatGPT.app", checkID: true},
	} {
		idx := strings.Index(cmd, spec.execSuffix)
		if idx < 0 {
			continue
		}
		after := idx + len(spec.execSuffix)
		if after < len(cmd) && cmd[after] != ' ' && cmd[after] != '\t' {
			continue
		}
		bundle := strings.TrimSpace(cmd[:idx+len(spec.bundleSuffix)])
		if bundle == "" {
			continue
		}
		if spec.checkID && (bundleID == nil || bundleID(bundle) != codexBundleIdentifier) {
			continue
		}
		return bundle
	}
	return ""
}

func isDesktopBundledAppServer(cmd string, bundleID func(string) string) bool {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, "/") || !strings.Contains(cmd, "app-server") {
		return false
	}
	for _, spec := range []struct {
		resPrefix    string
		bundleSuffix string
		checkID      bool
	}{
		{resPrefix: "/Codex.app/Contents/Resources/", bundleSuffix: "/Codex.app", checkID: false},
		{resPrefix: "/ChatGPT.app/Contents/Resources/", bundleSuffix: "/ChatGPT.app", checkID: true},
	} {
		idx := strings.Index(cmd, spec.resPrefix)
		if idx < 0 {
			continue
		}
		bundle := strings.TrimSpace(cmd[:idx+len(spec.bundleSuffix)])
		if bundle == "" {
			continue
		}
		if spec.checkID && (bundleID == nil || bundleID(bundle) != codexBundleIdentifier) {
			continue
		}
		return true
	}
	return false
}

func readMacBundleIdentifier(bundle string) string {
	plistPath := filepath.Join(bundle, "Contents", "Info.plist")
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return ""
	}
	if id := parseXMLBundleIdentifier(raw); id != "" {
		return id
	}
	if runtime.GOOS == "darwin" && bytes.HasPrefix(raw, []byte("bplist")) {
		out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", plistPath).Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

func parseXMLBundleIdentifier(raw []byte) string {
	const keyTag = "<key>CFBundleIdentifier</key>"
	s := string(raw)
	idx := strings.Index(s, keyTag)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(keyTag):]
	openIdx := strings.Index(rest, "<string>")
	if openIdx < 0 {
		return ""
	}
	rest = rest[openIdx+len("<string>"):]
	closeIdx := strings.Index(rest, "</string>")
	if closeIdx < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:closeIdx])
}

func quitDesktopBundles(_ []string, pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("/usr/bin/osascript", "-e", `tell application id "com.openai.codex" to quit`).Run()
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !anyAlive(pids) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return stopProcesses(pids)
}

func stopProcesses(pids []int) error {
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !anyAlive(pids) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGKILL)
		}
	}
	return nil
}

func restartAppServerProcesses(pids []int) int {
	count := 0
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			if p.Signal(syscall.SIGTERM) == nil {
				count++
			}
		}
	}
	return count
}

func reopenDesktopBundles(bundles []string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	opened := false
	for _, bundle := range bundles {
		if strings.TrimSpace(bundle) == "" {
			continue
		}
		if err := exec.Command("/usr/bin/open", "-a", bundle).Run(); err != nil {
			return err
		}
		opened = true
	}
	if opened {
		// Wait for the desktop app to spawn its bundled codex app-server before
		// dispatching `codex://threads/<id>` URLs so queued turns start immediately.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			proc := inspectCodexProcesses()
			if len(proc.DesktopPIDs) > 0 && len(proc.DesktopHelperPIDs) > 0 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return nil
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			if p.Signal(syscall.Signal(0)) == nil {
				return true
			}
		}
	}
	return false
}

func captureInterruptedSessions(codexHome string, pids []int, now time.Time) []RecoveryTarget {
	sessionsDir := filepath.Join(codexHome, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		return nil
	}

	openPaths := findOpenSessionFiles(pids)
	cutoff := now.Add(-24 * time.Hour)
	seen := map[string]bool{}
	var targets []RecoveryTarget

	_ = filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		st, err := d.Info()
		if err != nil || st.ModTime().Before(cutoff) {
			return nil
		}
		sessionID, shouldResume, quotaStopped := inspectRolloutForRecovery(path, openPaths[path])
		if shouldResume && sessionID != "" && !seen[sessionID] {
			seen[sessionID] = true
			targets = append(targets, RecoveryTarget{
				SessionID:    sessionID,
				Path:         path,
				Size:         st.Size(),
				QuotaStopped: quotaStopped,
			})
		}
		return nil
	})

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Path > targets[j].Path
	})
	if len(targets) > 5 {
		targets = targets[:5]
	}
	return targets
}

func findOpenSessionFiles(pids []int) map[string]bool {
	result := map[string]bool{}
	if len(pids) == 0 {
		return result
	}
	strs := make([]string, 0, len(pids))
	for _, pid := range pids {
		strs = append(strs, strconv.Itoa(pid))
	}
	args := []string{"-n", "-P", "-a", "-p", strings.Join(strs, ","), "-Fn"}
	raw, err := exec.Command("lsof", args...).Output()
	if err != nil {
		return result
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "n") && strings.HasSuffix(line, ".jsonl") {
			result[line[1:]] = true
		}
	}
	return result
}

func inspectRolloutForRecovery(path string, isOpen bool) (string, bool, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, false
	}
	defer func() { _ = f.Close() }()

	var sessionID string
	isSubagent := false
	inTurn := false
	quotaStopped := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			quotaStopped = false
			continue
		}
		switch entry.Type {
		case "session_meta":
			var meta struct {
				ID        string          `json:"id"`
				AgentPath string          `json:"agent_path"`
				Source    json.RawMessage `json:"source"`
			}
			if json.Unmarshal(entry.Payload, &meta) == nil {
				if meta.ID != "" {
					sessionID = meta.ID
				}
				if meta.AgentPath != "" && meta.AgentPath != "/root" {
					isSubagent = true
				}
				if strings.Contains(string(meta.Source), `"subagent"`) {
					isSubagent = true
				}
			}
		case "response_item":
			var item struct {
				Type string `json:"type"`
				Role string `json:"role"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil && item.Type == "message" && item.Role == "user" {
				quotaStopped = false
			}
		case "event_msg":
			var ev struct {
				Type           string `json:"type"`
				Message        string `json:"message"`
				CodexErrorInfo string `json:"codex_error_info"`
				Error          *struct {
					Message        string `json:"message"`
					CodexErrorInfo string `json:"codex_error_info"`
				} `json:"error"`
			}
			if json.Unmarshal(entry.Payload, &ev) == nil {
				switch ev.Type {
				case "task_started", "turn_started":
					inTurn = true
					quotaStopped = false
				case "user_message", "turn_aborted":
					inTurn = false
					quotaStopped = false
				case "turn_complete":
					inTurn = false
				case "task_complete", "turn_failed":
					inTurn = false
					if ev.Error != nil && isUsageLimitError(ev.Error.CodexErrorInfo, ev.Error.Message) {
						quotaStopped = true
					} else {
						quotaStopped = false
					}
				case "error":
					inTurn = false
					if isUsageLimitError(ev.CodexErrorInfo, ev.Message) {
						quotaStopped = true
					}
				}
			}
		}
	}
	if isSubagent || sessionID == "" {
		return "", false, false
	}
	if quotaStopped || (isOpen && inTurn) {
		return sessionID, true, quotaStopped
	}
	return "", false, false
}

func isUsageLimitError(code, message string) bool {
	if strings.TrimSpace(code) == "usage_limit_exceeded" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "usage_limit_exceeded") ||
		strings.Contains(lower, "out of credits")
}

func FilterUnrecoveredTargets(codexHome string, targets []RecoveryTarget) []RecoveryTarget {
	if len(targets) == 0 {
		return nil
	}
	receiptDir := filepath.Join(codexHome, "switcher-recovery")
	out := make([]RecoveryTarget, 0, len(targets))
	for _, t := range targets {
		receiptPath := filepath.Join(receiptDir, t.SessionID+".offset")
		if raw, err := os.ReadFile(receiptPath); err == nil {
			if prev, ok := parseRecoveryReceiptOffset(raw); ok && prev >= t.Size {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func parseRecoveryReceiptOffset(raw []byte) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return 0, false
	}
	if v, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return v, true
	}
	var obj struct {
		Offset *int64 `json:"offset"`
	}
	if json.Unmarshal([]byte(trimmed), &obj) == nil && obj.Offset != nil {
		return *obj.Offset, true
	}
	return 0, false
}

func discoverInstalledDesktopBundles() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, _ := os.UserHomeDir()
	candidates := []struct {
		path    string
		checkID bool
	}{
		{path: "/Applications/ChatGPT.app", checkID: true},
		{path: "/Applications/Codex.app", checkID: false},
	}
	if home != "" {
		candidates = append(candidates,
			struct {
				path    string
				checkID bool
			}{path: filepath.Join(home, "Applications", "ChatGPT.app"), checkID: true},
			struct {
				path    string
				checkID bool
			}{path: filepath.Join(home, "Applications", "Codex.app"), checkID: false},
		)
	}
	var out []string
	for _, c := range candidates {
		info, err := os.Stat(c.path)
		if err != nil || !info.IsDir() {
			continue
		}
		if c.checkID && readMacBundleIdentifier(c.path) != codexBundleIdentifier {
			continue
		}
		out = append(out, c.path)
		break
	}
	return out
}

func resumeInterruptedSessions(codexHome string, targets []RecoveryTarget) []string {
	pending := FilterUnrecoveredTargets(codexHome, targets)
	if len(pending) == 0 {
		return nil
	}
	codexBin := discoverCodexBinary()
	if codexBin == "" {
		return nil
	}
	if runtime.GOOS == "darwin" {
		if proc := inspectCodexProcesses(); len(proc.DesktopPIDs) == 0 {
			if bundles := discoverInstalledDesktopBundles(); len(bundles) > 0 {
				_ = reopenDesktopBundles(bundles)
			}
		}
	}
	receiptDir := filepath.Join(codexHome, "switcher-recovery")
	_ = os.MkdirAll(receiptDir, 0o700)

	var resumed []string
	for _, t := range pending {
		receiptPath := filepath.Join(receiptDir, t.SessionID+".offset")
		prompt := fmt.Sprintf("Continue the previous task.\n\n[Account Switcher recovery: %s]", newUUID())
		cmd := exec.Command(codexBin, "queue", "--thread", t.SessionID, "--message", prompt)
		if strings.TrimSpace(codexHome) != "" {
			cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
		}
		if err := cmd.Run(); err != nil {
			continue
		}
		_ = writeFileAtomic(receiptPath, []byte(fmt.Sprintf("%d\n", t.Size)))
		if runtime.GOOS == "darwin" {
			_ = exec.Command("/usr/bin/open", "codex://threads/"+t.SessionID).Run()
		}
		resumed = append(resumed, t.SessionID)
	}
	return resumed
}

func discoverCodexBinary() string {
	if envBin := strings.TrimSpace(os.Getenv("CODEX_BIN")); envBin != "" {
		if st, err := os.Stat(envBin); err == nil && !st.IsDir() {
			return envBin
		}
	}
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	if runtime.GOOS == "darwin" {
		bundles := []string{"/Applications/ChatGPT.app", "/Applications/Codex.app"}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			bundles = append(bundles,
				filepath.Join(home, "Applications", "ChatGPT.app"),
				filepath.Join(home, "Applications", "Codex.app"),
			)
		}
		for _, bundle := range bundles {
			for _, rel := range []string{
				"Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
				"Contents/Resources/codex",
			} {
				candidate := filepath.Join(bundle, rel)
				if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
					return candidate
				}
			}
		}
	}
	return ""
}
