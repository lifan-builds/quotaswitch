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

type ProcessSummary struct {
	CLIPIDs        []int    `json:"cli_pids,omitempty"`
	DesktopPIDs    []int    `json:"desktop_pids,omitempty"`
	DesktopBundles []string `json:"desktop_bundles,omitempty"`
	AppServerPIDs  []int    `json:"app_server_pids,omitempty"`
}

func (p ProcessSummary) Running() bool {
	return len(p.CLIPIDs) > 0 || len(p.DesktopPIDs) > 0
}

type RecoveryTarget struct {
	SessionID string
	Path      string
	Size      int64
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
	var out ProcessSummary
	raw, err := exec.Command("ps", "-axo", "pid=,tty=,command=").Output()
	if err != nil {
		return out
	}
	selfPID := os.Getpid()
	bundleSet := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
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
		if bundle := extractDesktopBundle(cmd); bundle != "" {
			if !strings.Contains(cmd, "Helper") && !strings.Contains(cmd, "app-server") {
				out.DesktopPIDs = append(out.DesktopPIDs, pid)
				if !bundleSet[bundle] {
					bundleSet[bundle] = true
					out.DesktopBundles = append(out.DesktopBundles, bundle)
				}
			}
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

func extractDesktopBundle(cmd string) string {
	for _, marker := range []string{"/Codex.app/Contents/MacOS/Codex", "/Applications/Codex.app/Contents/MacOS/Codex"} {
		if idx := strings.Index(cmd, marker); idx >= 0 {
			end := idx + len("/Codex.app")
			if strings.HasPrefix(marker, "/Applications/") {
				end = idx + len("/Applications/Codex.app")
			}
			return strings.TrimSpace(cmd[:end])
		}
	}
	return ""
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
	for _, bundle := range bundles {
		if strings.TrimSpace(bundle) == "" {
			continue
		}
		if err := exec.Command("/usr/bin/open", "-a", bundle).Run(); err != nil {
			return err
		}
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
		sessionID, shouldResume := inspectRolloutForRecovery(path, openPaths[path])
		if shouldResume && sessionID != "" && !seen[sessionID] {
			seen[sessionID] = true
			targets = append(targets, RecoveryTarget{
				SessionID: sessionID,
				Path:      path,
				Size:      st.Size(),
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

func inspectRolloutForRecovery(path string, isOpen bool) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
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
		case "event_msg":
			var ev struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(entry.Payload, &ev) == nil {
				switch ev.Type {
				case "task_started", "turn_started":
					inTurn = true
					quotaStopped = false
				case "task_complete", "turn_complete", "turn_aborted":
					inTurn = false
				case "error":
					if strings.Contains(ev.Message, "usage limit") || strings.Contains(ev.Message, "usage_limit_exceeded") {
						quotaStopped = true
						inTurn = false
					}
				}
			}
		}
		if strings.Contains(line, "usage_limit_exceeded") || strings.Contains(line, "You've hit your usage limit") {
			quotaStopped = true
		}
	}
	if isSubagent || sessionID == "" {
		return "", false
	}
	if quotaStopped || (isOpen && inTurn) {
		return sessionID, true
	}
	return "", false
}

func resumeInterruptedSessions(codexHome string, targets []RecoveryTarget) []string {
	if len(targets) == 0 {
		return nil
	}
	codexBin := discoverCodexBinary()
	if codexBin == "" {
		return nil
	}
	receiptDir := filepath.Join(codexHome, "switcher-recovery")
	_ = os.MkdirAll(receiptDir, 0o700)

	var resumed []string
	for _, t := range targets {
		receiptPath := filepath.Join(receiptDir, t.SessionID+".offset")
		if raw, err := os.ReadFile(receiptPath); err == nil {
			if prev, pErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); pErr == nil && prev >= t.Size {
				continue
			}
		}
		prompt := fmt.Sprintf("Continue the previous task.\n\n[Account Switcher recovery: %s]", newUUID())
		if err := exec.Command(codexBin, "queue", "--thread", t.SessionID, "--message", prompt).Run(); err != nil {
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
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	if runtime.GOOS == "darwin" {
		candidate := "/Applications/Codex.app/Contents/Resources/codex"
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}
