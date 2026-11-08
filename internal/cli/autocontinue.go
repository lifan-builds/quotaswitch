package cli

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	autoContinueMaxAge        = 2 * time.Hour
	autoContinueReloadTimeout = 22 * time.Second
	maxConsecutiveContinues   = 2
)

func defaultAntigravityDataDir() string {
	if env := strings.TrimSpace(os.Getenv("AGY_ANTIGRAVITY_DATA_DIR")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity")
}

type hubModelSpec struct {
	BaseModel string `json:"base_model,omitempty"`
	Effort    string `json:"effort,omitempty"`
}

func (m hubModelSpec) Empty() bool {
	return strings.TrimSpace(m.BaseModel) == ""
}

func (m hubModelSpec) DisplayName() string {
	if m.Empty() {
		return ""
	}
	eff := strings.TrimSpace(m.Effort)
	if eff == "" {
		return m.BaseModel
	}
	return m.BaseModel + " " + strings.ToUpper(eff[:1]) + strings.ToLower(eff[1:])
}

func argonHighModelSpec() hubModelSpec {
	return hubModelSpec{BaseModel: "Gemini 4 Argon", Effort: "high"}
}

func defaultClaudeModelSpec() hubModelSpec {
	return hubModelSpec{BaseModel: "Claude Opus 5.5", Effort: "high"}
}

type activeQuotaSnapshot struct {
	AccountID             string    `json:"account_id"`
	ClaudePct             int       `json:"claude_pct"`
	HasClaude             bool      `json:"has_claude"`
	ArgonPct              int       `json:"argon_pct"`
	HasArgon              bool      `json:"has_argon"`
	DrainedGroup          string    `json:"drained_group,omitempty"`
	PreferredClaudeBase   string    `json:"preferred_claude_base,omitempty"`
	PreferredClaudeEffort string    `json:"preferred_claude_effort,omitempty"`
	ClaudeDrainedAt       time.Time `json:"claude_drained_at,omitempty"`
	ClaudeSwitchedToArgon bool      `json:"claude_switched_to_argon,omitempty"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func loadActiveQuotaSnapshot(storeDir string) (activeQuotaSnapshot, bool) {
	var snap activeQuotaSnapshot
	if storeDir == "" {
		return snap, false
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "active_quota_state.json"))
	if err != nil {
		return snap, false
	}
	if json.Unmarshal(raw, &snap) != nil || snap.AccountID == "" {
		return snap, false
	}
	return snap, true
}

func saveActiveQuotaSnapshot(storeDir string, snap activeQuotaSnapshot) {
	if storeDir == "" {
		return
	}
	if encoded, err := json.Marshal(snap); err == nil {
		_ = os.WriteFile(filepath.Join(storeDir, "active_quota_state.json"), encoded, 0o600)
	}
}

func preferredClaudeModelSpec(storeDir, desktopConfigDir string) hubModelSpec {
	if desktopConfigDir != "" {
		if label := queryHubModelSelectorLabel(desktopConfigDir); label != "" {
			if spec, group := parseHubModelSelectorLabel(label); group == "claude" && !spec.Empty() {
				return spec
			}
		}
	}
	if snap, ok := loadActiveQuotaSnapshot(storeDir); ok && snap.PreferredClaudeBase != "" {
		eff := snap.PreferredClaudeEffort
		if eff == "" {
			eff = "high"
		}
		return hubModelSpec{BaseModel: snap.PreferredClaudeBase, Effort: eff}
	}
	return defaultClaudeModelSpec()
}

func targetModelForAccountRow(row *accountRow, storeDir, desktopConfigDir string) hubModelSpec {
	if row == nil {
		return hubModelSpec{}
	}
	cPct, hasC, aPct, hasA := priorityGroupQuotas(row)
	if (hasC && cPct > 0) || row.UsageError == "forbidden" {
		return preferredClaudeModelSpec(storeDir, desktopConfigDir)
	}
	if hasA && aPct > 0 {
		return argonHighModelSpec()
	}
	return hubModelSpec{}
}

func parseHubModelSelectorLabel(label string) (hubModelSpec, string) {
	raw := strings.TrimSpace(label)
	const prefix = "Select model, current:"
	if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(prefix)) {
		raw = strings.TrimSpace(raw[len(prefix):])
	}
	group := classifyModelGroupLabel(raw)
	if raw == "" {
		return hubModelSpec{}, group
	}
	effort := "high"
	base := raw
	parts := strings.Fields(raw)
	if len(parts) >= 2 {
		last := strings.ToLower(parts[len(parts)-1])
		if last == "low" || last == "medium" || last == "high" {
			effort = last
			base = strings.Join(parts[:len(parts)-1], " ")
		}
	}
	return hubModelSpec{BaseModel: base, Effort: effort}, group
}

// reconcileActiveAccountModelAndContinue enforces the same-account model policy:
//  1. Use Claude quota first when Claude > 0%.
//  2. Once Claude hits 0% and Gemini 4 Argon > 0% on the same account, automatically
//     switch the Hub model selector to "Gemini 4 Argon High" and auto-continue any
//     conversations that stopped due to the Claude quota exhaustion.
func reconcileActiveAccountModelAndContinue(storeDir, desktopConfigDir, agyDataDir string, active *accountRow, now time.Time, noAutoContinue bool) (string, []string) {
	if active == nil || active.ID == "" || !quotaDataFresh(active) || quotaExhausted(active) {
		return "", nil
	}
	claudePct, hasClaude, argonPct, hasArgon := priorityGroupQuotas(active)
	if !hasClaude && !hasArgon {
		return "", nil
	}

	prev, hasPrev := loadActiveQuotaSnapshot(storeDir)
	accountChanged := !hasPrev || prev.AccountID != active.ID

	curSpec, activeGroup := detectActiveModelSpecAndGroup(desktopConfigDir, agyDataDir)
	prefClaudeBase := prev.PreferredClaudeBase
	prefClaudeEffort := prev.PreferredClaudeEffort
	if activeGroup == "claude" && !curSpec.Empty() {
		prefClaudeBase = curSpec.BaseModel
		prefClaudeEffort = curSpec.Effort
	}
	if prefClaudeBase == "" {
		def := defaultClaudeModelSpec()
		prefClaudeBase = def.BaseModel
		prefClaudeEffort = def.Effort
	}

	claudeJustDrained := hasPrev && !accountChanged && prev.HasClaude && prev.ClaudePct > 0 && hasClaude && claudePct <= 0
	claudeJustReset := hasPrev && !accountChanged && prev.HasClaude && prev.ClaudePct <= 0 && hasClaude && claudePct > 0

	claudeDrainedAt := prev.ClaudeDrainedAt
	claudeSwitched := prev.ClaudeSwitchedToArgon
	if accountChanged || (hasClaude && claudePct > 0) {
		claudeDrainedAt = time.Time{}
		claudeSwitched = false
	} else if claudeJustDrained {
		claudeDrainedAt = now.UTC()
		claudeSwitched = false
	}

	var switchedModel string
	var autoContinued []string

	// Policy 1: Claude is exhausted (0%) while Gemini 4 Argon is available (>0%) on the same account.
	// Auto-switch Hub model to "Gemini 4 Argon High" and auto-continue stopped conversations.
	if (!hasClaude || claudePct <= 0) && hasArgon && argonPct > 0 {
		recentDrainPending := !claudeSwitched && !claudeDrainedAt.IsZero() && now.Sub(claudeDrainedAt) <= 3*time.Minute
		needArgonSwitch := activeGroup == "claude" || claudeJustDrained || recentDrainPending
		if needArgonSwitch && (desktopConfigDir != "" || agyDataDir != "") {
			target := argonHighModelSpec()
			modelChanged, resumed, stillRunning := switchModelAndContinueInCurrentHub(desktopConfigDir, agyDataDir, target, now, noAutoContinue)
			if modelChanged || activeGroup == "claude" || claudeJustDrained {
				switchedModel = target.DisplayName()
			}
			autoContinued = resumed
			if !stillRunning || len(resumed) > 0 {
				claudeSwitched = true
			}
		}
	} else if hasClaude && claudePct > 0 {
		// Policy 2: Claude quota is available (>0%) on this account — use Claude first!
		// Switch back to Claude if Argon is exhausted, if we just switched to this account, or if Claude just reset.
		needClaudeSwitch := activeGroup == "argon" && ((hasArgon && argonPct <= 0) || accountChanged || claudeJustReset)
		if needClaudeSwitch && (desktopConfigDir != "" || agyDataDir != "") {
			target := hubModelSpec{BaseModel: prefClaudeBase, Effort: prefClaudeEffort}
			modelChanged, resumed, _ := switchModelAndContinueInCurrentHub(desktopConfigDir, agyDataDir, target, now, noAutoContinue)
			if modelChanged || activeGroup == "argon" {
				switchedModel = target.DisplayName()
			}
			autoContinued = resumed
		}
	}

	cur := activeQuotaSnapshot{
		AccountID:             active.ID,
		ClaudePct:             claudePct,
		HasClaude:             hasClaude,
		ArgonPct:              argonPct,
		HasArgon:              hasArgon,
		PreferredClaudeBase:   prefClaudeBase,
		PreferredClaudeEffort: prefClaudeEffort,
		ClaudeDrainedAt:       claudeDrainedAt,
		ClaudeSwitchedToArgon: claudeSwitched,
		UpdatedAt:             now.UTC(),
	}
	saveActiveQuotaSnapshot(storeDir, cur)
	return switchedModel, autoContinued
}

// detectActiveModelSpecAndGroup inspects the live Antigravity Hub model selector (via
// DevToolsActivePort) or ~/.gemini/antigravity/antigravity_state.pbtxt.
func detectActiveModelSpecAndGroup(desktopConfigDir, agyDataDir string) (hubModelSpec, string) {
	if desktopConfigDir != "" {
		if label := queryHubModelSelectorLabel(desktopConfigDir); label != "" {
			if spec, g := parseHubModelSelectorLabel(label); g != "" {
				return spec, g
			}
		}
	}
	if agyDataDir != "" {
		if raw, err := os.ReadFile(filepath.Join(agyDataDir, "antigravity_state.pbtxt")); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "last_selected_agent_model:") {
					val := strings.TrimSpace(strings.TrimPrefix(line, "last_selected_agent_model:"))
					if spec, g := classifyModelEnumSpec(val); g != "" {
						return spec, g
					}
				}
			}
		}
	}
	return hubModelSpec{}, ""
}

func detectActiveModelGroup(desktopConfigDir, agyDataDir string) string {
	_, g := detectActiveModelSpecAndGroup(desktopConfigDir, agyDataDir)
	return g
}

func classifyModelGroupLabel(label string) string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "argon"):
		return "argon"
	case strings.Contains(lower, "claude") || strings.Contains(lower, "gpt") || strings.Contains(lower, "sonnet") || strings.Contains(lower, "opus") || strings.Contains(lower, "haiku"):
		return "claude"
	case strings.Contains(lower, "gemini"):
		return "gemini"
	default:
		return ""
	}
}

func classifyModelEnumSpec(enumVal string) (hubModelSpec, string) {
	upper := strings.ToUpper(strings.TrimSpace(enumVal))
	switch {
	case strings.Contains(upper, "PLACEHOLDER_M417") || strings.Contains(upper, "ARGON"):
		return hubModelSpec{BaseModel: "Gemini 4 Argon", Effort: "high"}, "argon"
	case strings.Contains(upper, "PLACEHOLDER_M418"):
		return hubModelSpec{BaseModel: "Gemini 4 Argon", Effort: "medium"}, "argon"
	case strings.Contains(upper, "PLACEHOLDER_M419"):
		return hubModelSpec{BaseModel: "Gemini 4 Argon", Effort: "low"}, "argon"
	case strings.Contains(upper, "PLACEHOLDER_M400"):
		return hubModelSpec{BaseModel: "Claude Opus 5.5", Effort: "low"}, "claude"
	case strings.Contains(upper, "PLACEHOLDER_M401"):
		return hubModelSpec{BaseModel: "Claude Opus 5.5", Effort: "medium"}, "claude"
	case strings.Contains(upper, "PLACEHOLDER_M402"):
		return hubModelSpec{BaseModel: "Claude Opus 5.5", Effort: "high"}, "claude"
	case strings.Contains(upper, "PLACEHOLDER_M403"):
		return hubModelSpec{BaseModel: "Claude Sonnet 5.5", Effort: "low"}, "claude"
	case strings.Contains(upper, "PLACEHOLDER_M404"):
		return hubModelSpec{BaseModel: "Claude Sonnet 5.5", Effort: "medium"}, "claude"
	case strings.Contains(upper, "PLACEHOLDER_M405"):
		return hubModelSpec{BaseModel: "Claude Sonnet 5.5", Effort: "high"}, "claude"
	case strings.Contains(upper, "CLAUDE") || strings.Contains(upper, "GPT") || strings.Contains(upper, "OPENAI") || strings.Contains(upper, "OPUS") || strings.Contains(upper, "SONNET"):
		return defaultClaudeModelSpec(), "claude"
	default:
		return hubModelSpec{}, ""
	}
}

func modelEnumForSpec(spec hubModelSpec) string {
	lowerBase := strings.ToLower(spec.BaseModel)
	lowerEff := strings.ToLower(spec.Effort)
	switch {
	case strings.Contains(lowerBase, "argon"):
		switch lowerEff {
		case "low":
			return "MODEL_PLACEHOLDER_M419"
		case "medium":
			return "MODEL_PLACEHOLDER_M418"
		default:
			return "MODEL_PLACEHOLDER_M417"
		}
	case strings.Contains(lowerBase, "sonnet"):
		switch lowerEff {
		case "low":
			return "MODEL_PLACEHOLDER_M403"
		case "medium":
			return "MODEL_PLACEHOLDER_M404"
		default:
			return "MODEL_PLACEHOLDER_M405"
		}
	case strings.Contains(lowerBase, "claude") || strings.Contains(lowerBase, "opus"):
		switch lowerEff {
		case "low":
			return "MODEL_PLACEHOLDER_M400"
		case "medium":
			return "MODEL_PLACEHOLDER_M401"
		default:
			return "MODEL_PLACEHOLDER_M402"
		}
	default:
		return ""
	}
}

func updateAntigravityStateModelEnum(agyDataDir string, target hubModelSpec) bool {
	if agyDataDir == "" || target.Empty() {
		return false
	}
	enumVal := modelEnumForSpec(target)
	if enumVal == "" {
		return false
	}
	statePath := filepath.Join(agyDataDir, "antigravity_state.pbtxt")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return false
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "last_selected_agent_model:") {
			found = true
			wantLine := "last_selected_agent_model: " + enumVal
			if trimmed != wantLine {
				lines[i] = wantLine
				changed = true
			}
		}
	}
	if !found {
		lines = append(lines, "last_selected_agent_model: "+enumVal)
		changed = true
	}
	if changed {
		_ = os.WriteFile(statePath, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return changed
}

type preSwitchHubState struct {
	HasDevToolsPort bool
	OldOrigin       string
	OriginalPath    string
	ToContinueIDs   []string
	TargetModel     hubModelSpec
}

type hubDaemonInfo struct {
	HTTPSPort int `json:"httpsPort"`
}

func readDaemonOrigin(agyDataDir string) string {
	if agyDataDir == "" {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(agyDataDir, "daemon"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "hub_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(agyDataDir, "daemon", e.Name()))
		if err != nil {
			continue
		}
		var info hubDaemonInfo
		if json.Unmarshal(raw, &info) == nil && info.HTTPSPort > 0 {
			return fmt.Sprintf("https://127.0.0.1:%d", info.HTTPSPort)
		}
	}
	return ""
}

func capturePreSwitchHubState(desktopConfigDir, agyDataDir string, now time.Time) preSwitchHubState {
	var state preSwitchHubState
	runningInUI := map[string]bool{}

	if desktopConfigDir != "" {
		if port, ok := readDevToolsPort(desktopConfigDir); ok {
			state.HasDevToolsPort = true
			if snap, err := inspectCurrentHubPage(port, 1200*time.Millisecond); err == nil {
				state.OldOrigin = snap.Origin
				state.OriginalPath = snap.PathAndQuery
				for _, id := range snap.RunningIDs {
					runningInUI[id] = true
				}
			}
		}
	}
	if state.OldOrigin == "" {
		state.OldOrigin = readDaemonOrigin(agyDataDir)
	}

	seen := map[string]bool{}
	var candidates []string

	if agyDataDir != "" {
		brainDir := filepath.Join(agyDataDir, "brain")
		if entries, err := os.ReadDir(brainDir); err == nil {
			type convEntry struct {
				id      string
				modTime time.Time
			}
			var recent []convEntry
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				id := e.Name()
				tPath := filepath.Join(brainDir, id, ".system_generated", "logs", "transcript.jsonl")
				info, err := os.Stat(tPath)
				if err != nil {
					continue
				}
				if now.Sub(info.ModTime()) > autoContinueMaxAge && !runningInUI[id] {
					continue
				}
				if shouldAutoContinueTranscript(tPath, runningInUI[id]) {
					recent = append(recent, convEntry{id: id, modTime: info.ModTime()})
				}
			}
			sort.Slice(recent, func(i, j int) bool {
				return recent[i].modTime.After(recent[j].modTime)
			})
			for _, r := range recent {
				if !seen[r.id] {
					seen[r.id] = true
					candidates = append(candidates, r.id)
				}
			}
		}
	}

	for id := range runningInUI {
		if !seen[id] {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}

	state.ToContinueIDs = candidates
	return state
}

type transcriptStep struct {
	StepIndex int    `json:"step_index"`
	Source    string `json:"source"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Name string `json:"name"`
	} `json:"tool_calls"`
}

func shouldAutoContinueTranscript(transcriptPath string, wasRunningInUI bool) bool {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return false
	}
	defer f.Close()

	// Verify step 0 is an explicit top-level user conversation (excludes subagents).
	br := bufio.NewReader(f)
	firstLine, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	firstLine = bytes.TrimSpace(firstLine)
	if len(firstLine) == 0 {
		return false
	}
	var firstStep transcriptStep
	if err := json.Unmarshal(firstLine, &firstStep); err != nil {
		return false
	}
	if firstStep.Source != "USER_EXPLICIT" || firstStep.Type != "USER_INPUT" {
		return false
	}

	// Read tail (up to last 96KB) to inspect recent steps efficiently.
	info, err := f.Stat()
	if err != nil {
		return false
	}
	const tailWindow = 96 * 1024
	offset := int64(0)
	if info.Size() > tailWindow {
		offset = info.Size() - tailWindow
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	tailBytes, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	lines := bytes.Split(tailBytes, []byte("\n"))
	if offset > 0 && len(lines) > 1 {
		// Drop potentially partial first line when seeking mid-file.
		lines = lines[1:]
	}

	var steps []transcriptStep
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var s transcriptStep
		if json.Unmarshal(line, &s) == nil {
			steps = append(steps, s)
		}
	}
	if len(steps) == 0 {
		return false
	}

	last := steps[len(steps)-1]
	upperStatus := strings.ToUpper(strings.TrimSpace(last.Status))
	if upperStatus == "CANCELED" || upperStatus == "CANCELLED" {
		return false
	}

	// Anti-loop guard: abort if we already sent >= maxConsecutiveContinues
	// "Continue" prompts without any tool execution or model output in between.
	consecutiveContinues := 0
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		switch s.Type {
		case "USER_INPUT":
			if isBareContinueUserInput(s.Content) {
				consecutiveContinues++
			} else {
				i = -1
			}
		case "GENERIC":
			i = -1
		case "PLANNER_RESPONSE":
			txt := strings.TrimSpace(s.Content)
			if len(s.ToolCalls) > 0 || (txt != "" && !strings.Contains(txt, "blocked by Gemini's filters")) {
				i = -1
			}
		}
	}
	if consecutiveContinues >= maxConsecutiveContinues {
		return false
	}

	if last.Type == "PLANNER_RESPONSE" {
		for _, tc := range last.ToolCalls {
			if tc.Name == "ask_question" {
				return false
			}
		}
	}

	if wasRunningInUI {
		return true
	}

	switch last.Type {
	case "GENERIC", "USER_INPUT", "SYSTEM_MESSAGE":
		return true
	case "PLANNER_RESPONSE":
		if len(last.ToolCalls) > 0 || upperStatus == "RUNNING" || upperStatus == "ERROR" {
			return true
		}
		return turnWaitingOnAsyncTask(steps)
	default:
		return false
	}
}

// turnWaitingOnAsyncTask returns true when the final turn in steps ended after
// launching or messaging a background subagent, timer, or async command that
// would be terminated by a language_server restart.
func turnWaitingOnAsyncTask(steps []transcriptStep) bool {
	for i := len(steps) - 2; i >= 0; i-- {
		s := steps[i]
		if s.Type == "USER_INPUT" || s.Type == "SYSTEM_MESSAGE" {
			break
		}
		if s.Type == "PLANNER_RESPONSE" {
			for _, tc := range s.ToolCalls {
				switch tc.Name {
				case "invoke_subagent", "send_message", "schedule":
					return true
				}
			}
		}
		if s.Type == "GENERIC" && strings.Contains(s.Content, "Tool is running as a background task") {
			return true
		}
	}
	return false
}

func isBareContinueUserInput(content string) bool {
	body := content
	if start := strings.Index(body, "<USER_REQUEST>"); start >= 0 {
		body = body[start+len("<USER_REQUEST>"):]
		if end := strings.Index(body, "</USER_REQUEST>"); end >= 0 {
			body = body[:end]
		}
	}
	return strings.EqualFold(strings.TrimSpace(body), "continue")
}

type hubPageSnapshot struct {
	Origin       string   `json:"origin"`
	PathAndQuery string   `json:"pathAndQuery"`
	ModelLabel   string   `json:"modelLabel"`
	RunningIDs   []string `json:"runningIds"`
}

type cdpTargetInfo struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func isHubPageURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://127.0.0.1:") || strings.HasPrefix(rawURL, "http://127.0.0.1:")
}

func readDevToolsPort(desktopConfigDir string) (int, bool) {
	if desktopConfigDir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(filepath.Join(desktopConfigDir, "DevToolsActivePort"))
	if err != nil {
		return 0, false
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func listCDPTargets(port int, timeout time.Duration) ([]cdpTargetInfo, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdp /json/list returned HTTP %d", resp.StatusCode)
	}
	var targets []cdpTargetInfo
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func queryHubModelSelectorLabel(desktopConfigDir string) string {
	port, ok := readDevToolsPort(desktopConfigDir)
	if !ok {
		return ""
	}
	snap, err := inspectCurrentHubPage(port, 600*time.Millisecond)
	if err != nil {
		return ""
	}
	return snap.ModelLabel
}

func inspectCurrentHubPage(port int, timeout time.Duration) (*hubPageSnapshot, error) {
	targets, err := listCDPTargets(port, timeout)
	if err != nil {
		return nil, err
	}
	var hub *cdpTargetInfo
	for i := range targets {
		if targets[i].Type == "page" && isHubPageURL(targets[i].URL) && targets[i].WebSocketDebuggerURL != "" {
			hub = &targets[i]
			break
		}
	}
	if hub == nil {
		return nil, errors.New("no Antigravity Hub page open")
	}
	ws, err := dialCDPWebSocket(hub.WebSocketDebuggerURL, timeout)
	if err != nil {
		return nil, err
	}
	defer ws.Close()

	var snap hubPageSnapshot
	err = ws.evalJSON(`(() => {
		const runningIds = [];
		for (const a of document.querySelectorAll('a[href^="/c/"]')) {
			const m = (a.getAttribute('href') || '').match(/^\/c\/([^/?#]+)/);
			if (!m) continue;
			const hasStop = Boolean(
				a.parentElement?.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]') ||
				a.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]')
			);
			if (hasStop) runningIds.push(m[1]);
		}
		const modelBtn = document.querySelector('[aria-label^="Select model, current:"]');
		return {
			origin: location.origin,
			pathAndQuery: location.pathname + location.search,
			modelLabel: modelBtn ? (modelBtn.getAttribute('aria-label') || '') : '',
			runningIds
		};
	})()`, &snap, timeout)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// switchModelAndContinueInCurrentHub switches the model selector in the live
// Antigravity Hub window (without restarting language_server) and auto-continues
// any stopped conversations on the same account.
func switchModelAndContinueInCurrentHub(desktopConfigDir, agyDataDir string, target hubModelSpec, now time.Time, noAutoContinue bool) (bool, []string, bool) {
	modelChanged := updateAntigravityStateModelEnum(agyDataDir, target)

	state := capturePreSwitchHubState(desktopConfigDir, agyDataDir, now)
	if !state.HasDevToolsPort {
		return modelChanged, nil, false
	}

	port, ok := readDevToolsPort(desktopConfigDir)
	if !ok {
		return modelChanged, nil, false
	}
	targets, err := listCDPTargets(port, 1200*time.Millisecond)
	if err != nil {
		return modelChanged, nil, false
	}
	var hub *cdpTargetInfo
	for i := range targets {
		if targets[i].Type == "page" && isHubPageURL(targets[i].URL) && targets[i].WebSocketDebuggerURL != "" {
			hub = &targets[i]
			break
		}
	}
	if hub == nil {
		return modelChanged, nil, false
	}

	ws, err := dialCDPWebSocket(hub.WebSocketDebuggerURL, 2*time.Second)
	if err != nil {
		return modelChanged, nil, false
	}
	defer ws.Close()

	var snap hubPageSnapshot
	_ = ws.evalJSON(`(() => {
		const runningIds = [];
		for (const a of document.querySelectorAll('a[href^="/c/"]')) {
			const m = (a.getAttribute('href') || '').match(/^\/c\/([^/?#]+)/);
			if (!m) continue;
			const hasStop = Boolean(
				a.parentElement?.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]') ||
				a.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]')
			);
			if (hasStop) runningIds.push(m[1]);
		}
		const modelBtn = document.querySelector('[aria-label^="Select model, current:"]');
		return {
			origin: location.origin,
			pathAndQuery: location.pathname + location.search,
			modelLabel: modelBtn ? (modelBtn.getAttribute('aria-label') || '') : '',
			runningIds
		};
	})()`, &snap, 1500*time.Millisecond)

	runningSet := map[string]bool{}
	for _, id := range snap.RunningIDs {
		runningSet[id] = true
	}

	if _, switched := selectModelInHubConn(ws, target); switched {
		modelChanged = true
	}

	var resumed []string
	if !noAutoContinue && snap.Origin != "" {
		for _, convID := range state.ToContinueIDs {
			if runningSet[convID] {
				continue
			}
			if continueSingleConversation(ws, snap.Origin, convID, target) {
				resumed = append(resumed, convID)
			}
		}
		if snap.PathAndQuery != "" && strings.HasPrefix(snap.PathAndQuery, "/c/") {
			restoreOriginalConversation(ws, snap.Origin, snap.PathAndQuery)
			_, _ = selectModelInHubConn(ws, target)
		}
	}

	return modelChanged, resumed, len(snap.RunningIDs) > 0
}

func selectModelInHubConn(ws *cdpConn, target hubModelSpec) (string, bool) {
	if ws == nil || target.Empty() {
		return "", false
	}
	eff := strings.ToLower(strings.TrimSpace(target.Effort))
	if eff == "" {
		eff = "high"
	}
	var res struct {
		OK      bool   `json:"ok"`
		Changed bool   `json:"changed"`
		Label   string `json:"label"`
	}
	err := ws.evalJSON(fmt.Sprintf(`(async () => {
		const baseName = %q;
		const effort = %q;
		const trigger = document.querySelector('[data-testid="model-selector-trigger"], [aria-label^="Select model, current:"]');
		if (!trigger) return { ok: false, changed: false, label: '' };
		const curLabel = trigger.getAttribute('aria-label') || '';
		const wantSuffix = (baseName + ' ' + effort).toLowerCase();
		if (curLabel.toLowerCase().includes(wantSuffix)) {
			return { ok: true, changed: false, label: curLabel };
		}
		if (!document.querySelector('[data-testid="model-selector-panel"]')) {
			trigger.click();
			await new Promise(r => setTimeout(r, 80));
		}
		let span = document.querySelector('[data-testid="model-selector-effort-group"][data-model-base="' + baseName + '"]');
		if (!span && baseName.toLowerCase().includes('claude')) {
			span = document.querySelector('[data-testid="model-selector-effort-group"][data-model-base^="Claude"]');
		}
		const menuItem = span?.closest('[role="menuitem"]');
		if (!menuItem) {
			if (document.querySelector('[data-testid="model-selector-panel"]')) {
				document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
			}
			return { ok: false, changed: false, label: curLabel };
		}
		menuItem.focus();
		menuItem.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
		await new Promise(r => setTimeout(r, 100));
		let effortSpan = document.querySelector('[data-testid="model-selector-effort-option"][data-effort="' + effort + '"]');
		if (!effortSpan) {
			effortSpan = document.querySelector('[data-testid="model-selector-effort-option"]');
		}
		const radio = effortSpan?.closest('[role="menuitemradio"], [role="menuitem"]');
		if (!radio) {
			menuItem.click();
		} else {
			radio.focus();
			radio.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, button: 0 }));
			radio.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
			radio.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, button: 0 }));
			radio.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, button: 0 }));
			radio.click();
			radio.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true }));
		}
		await new Promise(r => setTimeout(r, 80));
		if (document.querySelector('[data-testid="model-selector-panel"]')) {
			document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		}
		const updated = document.querySelector('[data-testid="model-selector-trigger"], [aria-label^="Select model, current:"]')?.getAttribute('aria-label') || '';
		return { ok: true, changed: updated !== curLabel, label: updated };
	})()`, target.BaseModel, eff), &res, 3*time.Second)
	if err != nil || !res.OK {
		return "", false
	}
	return res.Label, res.Changed
}

// resumeConversationsInHub waits for Antigravity Desktop's Hub UI to reload on
// the new language_server port, selects the target model, sends "Continue" into
// each stopped conversation, and restores the user's open conversation path.
func resumeConversationsInHub(desktopConfigDir string, state preSwitchHubState, timeout time.Duration, allowBackgroundOpen bool) ([]string, error) {
	if desktopConfigDir == "" || !state.HasDevToolsPort {
		return nil, nil
	}
	if len(state.ToContinueIDs) == 0 && state.OriginalPath == "" && state.TargetModel.Empty() {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = autoContinueReloadTimeout
	}

	deadline := time.Now().Add(timeout)
	startWait := time.Now()
	openedBackground := false

	var ws *cdpConn
	var newOrigin string
	for time.Now().Before(deadline) {
		port, ok := readDevToolsPort(desktopConfigDir)
		if !ok {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		targets, err := listCDPTargets(port, 1000*time.Millisecond)
		if err != nil {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		var candidate *cdpTargetInfo
		for i := range targets {
			t := &targets[i]
			if t.Type != "page" || !isHubPageURL(t.URL) || t.WebSocketDebuggerURL == "" {
				continue
			}
			if state.OldOrigin != "" && strings.HasPrefix(t.URL, state.OldOrigin) {
				continue
			}
			candidate = t
			break
		}
		if candidate == nil {
			if allowBackgroundOpen && !openedBackground && runtime.GOOS == "darwin" && len(state.ToContinueIDs) > 0 && time.Since(startWait) > 4000*time.Millisecond {
				openedBackground = true
				_ = exec.Command("/usr/bin/open", "-g", "-a", "Antigravity").Run()
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}

		conn, err := dialCDPWebSocket(candidate.WebSocketDebuggerURL, 1500*time.Millisecond)
		if err != nil {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		var ready struct {
			Ready  bool   `json:"ready"`
			Origin string `json:"origin"`
		}
		needSidebar := len(state.ToContinueIDs) > 0 || state.OriginalPath != ""
		err = conn.evalJSON(fmt.Sprintf(`(() => ({
			ready: document.readyState === 'complete' && (
				document.querySelectorAll('a[href^="/c/"]').length > 0 ||
				(!%t && document.querySelector('[aria-label="Message input"]') !== null)
			),
			origin: location.origin
		}))()`, needSidebar), &ready, 1200*time.Millisecond)
		if err == nil && ready.Ready && (state.OldOrigin == "" || ready.Origin != state.OldOrigin) {
			ws = conn
			newOrigin = ready.Origin
			break
		}
		conn.Close()
		time.Sleep(150 * time.Millisecond)
	}

	if ws == nil {
		return nil, errors.New("timed out waiting for Antigravity Hub window to reload")
	}
	defer ws.Close()

	if !state.TargetModel.Empty() {
		_, _ = selectModelInHubConn(ws, state.TargetModel)
	}

	var resumed []string
	for _, convID := range state.ToContinueIDs {
		if ok := continueSingleConversation(ws, newOrigin, convID, state.TargetModel); ok {
			resumed = append(resumed, convID)
		}
	}

	if state.OriginalPath != "" && strings.HasPrefix(state.OriginalPath, "/c/") {
		restoreOriginalConversation(ws, newOrigin, state.OriginalPath)
		if !state.TargetModel.Empty() {
			_, _ = selectModelInHubConn(ws, state.TargetModel)
		}
	}
	return resumed, nil
}

func continueSingleConversation(ws *cdpConn, origin, convID string, targetModel hubModelSpec) bool {
	linkSel := "a[href=\"/c/" + convID + "\"]"
	var clicked bool
	_ = ws.evalJSON(fmt.Sprintf(`(async () => {
		let a = document.querySelector(%q);
		if (!a) {
			const collapsed = document.querySelectorAll('button[data-testid="project-header"][aria-expanded="false"]');
			if (collapsed.length > 0) {
				collapsed.forEach(b => b.click());
				await new Promise(r => setTimeout(r, 150));
				a = document.querySelector(%q);
			}
		}
		if (!a) return false;
		a.click();
		return true;
	})()`, linkSel, linkSel), &clicked, 1500*time.Millisecond)
	if !clicked {
		_, err := ws.call("Page.navigate", map[string]any{"url": origin + "/c/" + convID}, 3*time.Second)
		if err != nil {
			return false
		}
	}

	navDeadline := time.Now().Add(5 * time.Second)
	mounted := false
	for time.Now().Before(navDeadline) {
		var ok bool
		if ws.evalJSON(fmt.Sprintf(`(() => {
			if (!location.pathname.includes(%q)) return false;
			const ed = document.querySelector('[aria-label="Message input"]');
			if (!ed) return false;
			if (document.querySelector('[data-testid="send-button-loading"]') !== null) return false;
			const fk = Object.keys(ed).find(k => k.startsWith('__reactFiber'));
			let cur = fk ? ed[fk] : null;
			while (cur) {
				const name = cur.type?.displayName || cur.type?.name || '';
				if (name === 'AgentInputBox') {
					const p = cur.memoizedProps || {};
					if (p.cascadeId && p.cascadeId !== %q) return false;
					if (p.isConvoLoading || p.isCreatingEnvironment) return false;
					break;
				}
				cur = cur.return;
			}
			return true;
		})()`, convID, convID), &ok, time.Second) == nil && ok {
			mounted = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !mounted {
		return false
	}
	time.Sleep(180 * time.Millisecond)

	if !targetModel.Empty() {
		_, _ = selectModelInHubConn(ws, targetModel)
	}

	var prep struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	err := ws.evalJSON(fmt.Sprintf(`(() => {
		const link = document.querySelector(%q);
		const runningSidebar = Boolean(
			link?.parentElement?.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]') ||
			link?.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]')
		);
		const cancelBtn = document.querySelector('[data-testid="input-cancel-button"], button[aria-label^="Cancel"]');
		if (runningSidebar || cancelBtn) return { ok: false, reason: 'already_running' };
		const approvalBtn = Array.from(document.querySelectorAll('button')).find(b => /^(approve|allow|always allow)$/i.test((b.innerText || '').trim()));
		if (approvalBtn) return { ok: false, reason: 'awaiting_approval' };
		const editor = document.querySelector('[aria-label="Message input"]');
		if (!editor) return { ok: false, reason: 'no_editor' };
		const draft = (editor.innerText || '').trim();
		if (draft.length > 0) return { ok: false, reason: 'has_draft' };
		editor.focus();
		return { ok: true, reason: '' };
	})()`, linkSel), &prep, 1500*time.Millisecond)
	if err != nil || !prep.OK {
		return false
	}

	time.Sleep(30 * time.Millisecond)
	if _, err := ws.call("Input.insertText", map[string]any{"text": "Continue"}, 1500*time.Millisecond); err != nil {
		return false
	}
	time.Sleep(80 * time.Millisecond)
	keyDown := map[string]any{
		"type":                  "keyDown",
		"key":                   "Enter",
		"code":                  "Enter",
		"windowsVirtualKeyCode": 13,
		"nativeVirtualKeyCode":  13,
	}
	keyUp := map[string]any{
		"type":                  "keyUp",
		"key":                   "Enter",
		"code":                  "Enter",
		"windowsVirtualKeyCode": 13,
		"nativeVirtualKeyCode":  13,
	}
	if _, err := ws.call("Input.dispatchKeyEvent", keyDown, 1500*time.Millisecond); err != nil {
		return false
	}
	if _, err := ws.call("Input.dispatchKeyEvent", keyUp, 1500*time.Millisecond); err != nil {
		return false
	}

	submitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(submitDeadline) {
		time.Sleep(60 * time.Millisecond)
		var submitted bool
		if ws.evalJSON(fmt.Sprintf(`(() => {
			const link = document.querySelector(%q);
			const running = Boolean(
				link?.parentElement?.querySelector('[aria-label="Stop execution"], [data-testid="status-loading-spinner"]') ||
				document.querySelector('[data-testid="input-cancel-button"], button[aria-label^="Cancel"]')
			);
			const editor = document.querySelector('[aria-label="Message input"]');
			const empty = !editor || (editor.innerText || '').trim() === '';
			if (!running && !empty) {
				const sendBtn = document.querySelector('button[data-testid="send-button"]:not([disabled])');
				if (sendBtn) sendBtn.click();
			}
			return running || empty;
		})()`, linkSel), &submitted, time.Second) == nil && submitted {
			time.Sleep(150 * time.Millisecond)
			return true
		}
	}
	return false
}

func restoreOriginalConversation(ws *cdpConn, origin, origPathAndQuery string) {
	convPath := origPathAndQuery
	if idx := strings.Index(convPath, "?"); idx >= 0 {
		convPath = convPath[:idx]
	}
	var curPath string
	if ws.evalJSON(`location.pathname + location.search`, &curPath, time.Second) == nil && (curPath == origPathAndQuery || curPath == convPath) {
		return
	}
	linkSel := "a[href=\"" + convPath + "\"]"
	var clicked bool
	_ = ws.evalJSON(fmt.Sprintf(`(async () => {
		let a = document.querySelector(%q);
		if (!a) {
			const collapsed = document.querySelectorAll('button[data-testid="project-header"][aria-expanded="false"]');
			if (collapsed.length > 0) {
				collapsed.forEach(b => b.click());
				await new Promise(r => setTimeout(r, 150));
				a = document.querySelector(%q);
			}
		}
		if (!a) return false;
		a.click();
		return true;
	})()`, linkSel, linkSel), &clicked, time.Second)
	if !clicked {
		_, _ = ws.call("Page.navigate", map[string]any{"url": origin + origPathAndQuery}, 2*time.Second)
	}
	time.Sleep(150 * time.Millisecond)
}

// Minimal RFC 6455 localhost WebSocket client for Chrome DevTools Protocol.
type cdpConn struct {
	conn   net.Conn
	reader *bufio.Reader
	seq    int
}

func dialCDPWebSocket(wsURL string, timeout time.Duration) (*cdpConn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		conn.Close()
		return nil, err
	}
	wsKey := base64.StdEncoding.EncodeToString(keyBytes[:])
	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}
	handshake := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		reqPath, u.Host, wsKey,
	)
	if _, err := io.WriteString(conn, handshake); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReaderSize(conn, 32*1024)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", strings.TrimSpace(statusLine))
	}
	acceptExpected := computeWSAccept(wsKey)
	gotAccept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Sec-WebSocket-Accept") {
			gotAccept = strings.TrimSpace(parts[1])
		}
	}
	if gotAccept != "" && gotAccept != acceptExpected {
		conn.Close()
		return nil, errors.New("websocket accept mismatch")
	}
	return &cdpConn{conn: conn, reader: br}, nil
}

func computeWSAccept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *cdpConn) Close() error {
	return c.conn.Close()
}

func (c *cdpConn) writeFrame(opcode byte, payload []byte) error {
	var header [14]byte
	header[0] = 0x80 | (opcode & 0x0f)
	hdrLen := 2
	n := len(payload)
	switch {
	case n < 126:
		header[1] = 0x80 | byte(n)
	case n <= 65535:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
		hdrLen = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
		hdrLen = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(header[hdrLen:hdrLen+4], mask[:])
	hdrLen += 4

	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(header[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

func (c *cdpConn) readMessage() ([]byte, error) {
	var msgBuf bytes.Buffer
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.reader, hdr[:]); err != nil {
			return nil, err
		}
		fin := (hdr[0] & 0x80) != 0
		opcode := hdr[0] & 0x0f
		masked := (hdr[1] & 0x80) != 0
		length := uint64(hdr[1] & 0x7f)

		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > 16*1024*1024 {
			return nil, errors.New("websocket frame too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}

		switch opcode {
		case 0x8: // Close
			return nil, io.EOF
		case 0x9: // Ping
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA: // Pong
			continue
		case 0x0, 0x1, 0x2:
			msgBuf.Write(payload)
			if fin {
				return msgBuf.Bytes(), nil
			}
		}
	}
}

type cdpEnvelope struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *cdpConn) call(method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	c.seq++
	reqID := c.seq
	_ = c.conn.SetDeadline(time.Now().Add(timeout))
	reqBytes, err := json.Marshal(map[string]any{
		"id":     reqID,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(0x1, reqBytes); err != nil {
		return nil, err
	}
	for {
		raw, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		var env cdpEnvelope
		if json.Unmarshal(raw, &env) != nil || env.ID != reqID {
			continue
		}
		if env.Error != nil {
			return nil, errors.New(env.Error.Message)
		}
		return env.Result, nil
	}
}

func (c *cdpConn) evalJSON(expression string, dest any, timeout time.Duration) error {
	res, err := c.call("Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, timeout)
	if err != nil {
		return err
	}
	var wrapper struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &wrapper); err != nil {
		return err
	}
	if dest == nil || len(wrapper.Result.Value) == 0 || string(wrapper.Result.Value) == "undefined" {
		return nil
	}
	return json.Unmarshal(wrapper.Result.Value, dest)
}
