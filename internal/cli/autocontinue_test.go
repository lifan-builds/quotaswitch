package cli

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/desktop"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

func writeTranscriptFile(t *testing.T, brainDir, convID string, modTime time.Time, lines []string) string {
	t.Helper()
	logDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(logDir, "transcript.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(p, modTime, modTime)
	}
	return p
}

func TestShouldAutoContinueTranscript(t *testing.T) {
	brainDir := filepath.Join(t.TempDir(), "brain")
	step0User := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>Build feature</USER_REQUEST>"}`
	step0Subagent := `{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"Subagent task"}`

	t.Run("stopped after GENERIC tool output", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-generic", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file"}]}`,
			`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"file contents"}`,
		})
		if !shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected GENERIC tail to auto-continue")
		}
	})

	t.Run("stopped after USER_INPUT with no response", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-user", time.Time{}, []string{step0User})
		if !shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected unanswered USER_INPUT to auto-continue")
		}
	})

	t.Run("cleanly finished PLANNER_RESPONSE", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-done", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"All done!"}`,
		})
		if shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected completed PLANNER_RESPONSE not to auto-continue")
		}
		if !shouldAutoContinueTranscript(p, true) {
			t.Fatal("expected conversation actively running in UI at switch time to auto-continue")
		}
	})

	t.Run("subagent excluded even if running", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-subagent", time.Time{}, []string{
			step0Subagent,
			`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"output"}`,
		})
		if shouldAutoContinueTranscript(p, true) {
			t.Fatal("expected subagent transcript never to auto-continue directly")
		}
	})

	t.Run("canceled by user excluded", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-canceled", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"CANCELED","content":""}`,
		})
		if shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected CANCELED step not to auto-continue")
		}
	})

	t.Run("ask_question tool call excluded", func(t *testing.T) {
		p := writeTranscriptFile(t, brainDir, "c-question", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"ask_question"}]}`,
		})
		if shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected ask_question tool call not to auto-continue")
		}
	})

	t.Run("anti-loop guard stops after 2 consecutive unprogressed Continues", func(t *testing.T) {
		cont1 := `{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nContinue\n</USER_REQUEST>"}`
		cont2 := `{"step_index":3,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\ncontinue\n</USER_REQUEST>"}`
		pOne := writeTranscriptFile(t, brainDir, "c-loop-1", time.Time{}, []string{step0User, cont1})
		if !shouldAutoContinueTranscript(pOne, false) {
			t.Fatal("expected single Continue to still allow one retry")
		}
		pTwo := writeTranscriptFile(t, brainDir, "c-loop-2", time.Time{}, []string{step0User, cont1, cont2})
		if shouldAutoContinueTranscript(pTwo, false) {
			t.Fatal("expected 2 consecutive bare Continues without progress to stop")
		}
		pProgressed := writeTranscriptFile(t, brainDir, "c-loop-ok", time.Time{}, []string{
			step0User,
			cont1,
			`{"step_index":3,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"tool progressed"}`,
			cont2,
		})
		if !shouldAutoContinueTranscript(pProgressed, false) {
			t.Fatal("expected Continue after real tool progress to remain eligible")
		}
	})

	t.Run("large file >96KB tail seek", func(t *testing.T) {
		padding := strings.Repeat("x", 110*1024)
		p := writeTranscriptFile(t, brainDir, "c-large", time.Time{}, []string{
			step0User,
			fmt.Sprintf(`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":%q}`, padding),
			`{"step_index":2,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"short tail"}`,
		})
		if !shouldAutoContinueTranscript(p, false) {
			t.Fatal("expected large transcript ending in GENERIC to auto-continue")
		}
	})

	t.Run("parent waiting on subagent or background task", func(t *testing.T) {
		pDelegated := writeTranscriptFile(t, brainDir, "c-parent-subagent", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"send_message"}]}`,
			`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Message sent to subagent"}`,
			`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"I have notified DeepCoder and am waiting for completion."}`,
		})
		if !shouldAutoContinueTranscript(pDelegated, false) {
			t.Fatal("expected parent conversation waiting on subagent to auto-continue")
		}

		pWokeAndFinished := writeTranscriptFile(t, brainDir, "c-parent-finished", time.Time{}, []string{
			step0User,
			`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"invoke_subagent"}]}`,
			`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created subagent"}`,
			`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"Waiting on subagent."}`,
			`{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"Subagent finished"}`,
			`{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"All work from the subagent is complete."}`,
		})
		if shouldAutoContinueTranscript(pWokeAndFinished, false) {
			t.Fatal("expected parent conversation that already processed subagent wakeup and finished not to auto-continue")
		}
	})
}

func TestSameAccountClaudeToArgonHighAndCrossAccountRotationEndToEnd(t *testing.T) {
	env := newTestEnv(t, false)
	app, _, _ := env.app("")

	tok1 := makeCLIToken(t, "acct1@example.com", "sub-1", "ref-1")
	tok2 := makeCLIToken(t, "acct2@example.com", "sub-2", "ref-2")
	r1, _ := app.Store.Upsert(tok1, "acct1", true)
	r2, _ := app.Store.Upsert(tok2, "acct2", false)
	_ = os.MkdirAll(filepath.Dir(env.tokenPath), 0o700)
	_ = os.WriteFile(env.tokenPath, tok1, 0o600)

	mockCDP := newMockHubCDPServer(t)
	mockCDP.runningIDs = map[string]bool{}
	mockCDP.modelLabel = "Select model, current: Claude Sonnet 5.5 High"
	_ = os.MkdirAll(app.Desktop.ConfigDir, 0o700)
	_ = os.WriteFile(filepath.Join(app.Desktop.ConfigDir, "DevToolsActivePort"), []byte(fmt.Sprintf("%d\n/devtools/browser/mock\n", mockCDP.wsPort)), 0o600)

	agyDir := filepath.Join(env.dir, "antigravity_data")
	brainDir := filepath.Join(agyDir, "brain")
	_ = os.MkdirAll(agyDir, 0o700)
	_ = os.WriteFile(filepath.Join(agyDir, "antigravity_state.pbtxt"), []byte("last_selected_agent_model: MODEL_PLACEHOLDER_M405\n"), 0o600)

	prov := &mapFullUsageProvider{
		byID: map[string]usage.Full{
			// Start with Claude=40%, Argon=100% on acct1
			r1.Account.ID: {Groups: makePriorityGroups(40, 100, 100)},
			r2.Account.ID: {Groups: makePriorityGroups(100, 100, 100)},
		},
	}

	// 1. Poll while Claude=40%, Argon=100% -> stays on acct1, keeps Claude Sonnet 5.5 High.
	app, out, errOut := env.app("")
	app.AntigravityDataDir = agyDir
	app.UsageFull = prov
	if code := app.Run([]string{"rotate"}); code != 0 {
		t.Fatalf("rotate step 1 exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No rotation needed") {
		t.Fatalf("expected healthy message, got: %s", out.String())
	}

	// 2. Claude hits 0% on acct1 while Argon=100%, and conv-claude-stop stops mid-turn!
	// Must stay on acct1, auto-switch model to Gemini 4 Argon High, and auto-continue conv-claude-stop!
	prov.byID[r1.Account.ID] = usage.Full{Groups: makePriorityGroups(0, 100, 100)}
	step0 := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>Code</USER_REQUEST>"}`
	writeTranscriptFile(t, brainDir, "conv-claude-stop", testNow.Add(-1*time.Minute), []string{
		step0,
		`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"stopped when claude hit 0%"}`,
	})

	app, out, errOut = env.app("")
	app.AntigravityDataDir = agyDir
	app.UsageFull = prov
	if code := app.Run([]string{"rotate"}); code != 0 {
		t.Fatalf("rotate step 2 exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Switched model to Gemini 4 Argon High on acct1") {
		t.Fatalf("expected same-account model switch to Gemini 4 Argon High, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "Auto-continued 1 stopped conversation(s).") {
		t.Fatalf("expected auto-continue on same account, got: %s", out.String())
	}
	mockCDP.mu.Lock()
	if mockCDP.modelLabel != "Select model, current: Gemini 4 Argon High" {
		t.Fatalf("expected Hub model label switched to Gemini 4 Argon High, got %q", mockCDP.modelLabel)
	}
	if got := mockCDP.submittedMsgs["conv-claude-stop"]; len(got) != 1 || got[0] != "Continue" {
		t.Fatalf("expected conv-claude-stop to receive Continue, got %v", got)
	}
	mockCDP.mu.Unlock()
	pbRaw, _ := os.ReadFile(filepath.Join(agyDir, "antigravity_state.pbtxt"))
	if !strings.Contains(string(pbRaw), "MODEL_PLACEHOLDER_M417") {
		t.Fatalf("expected pbtxt updated to MODEL_PLACEHOLDER_M417, got %s", string(pbRaw))
	}

	// 3. Subsequent poll while acct1 has Claude=0%, Argon=85% -> already on Gemini 4 Argon High, stays quiet.
	prov.byID[r1.Account.ID] = usage.Full{Groups: makePriorityGroups(0, 85, 100)}
	app, out, errOut = env.app("")
	app.AntigravityDataDir = agyDir
	app.UsageFull = prov
	if code := app.Run([]string{"rotate"}); code != 0 {
		t.Fatalf("rotate step 3 exit=%d err=%s", code, errOut.String())
	}
	if strings.Contains(out.String(), "Switched model") || strings.Contains(out.String(), "Auto-continued") {
		t.Fatalf("expected no duplicate model switch or continue on step 3, got: %s", out.String())
	}

	// 4. Now acct1 hits Claude=0%, Argon=0% -> rotates to acct2 (Claude=100%, Argon=100%),
	// restores user's preferred Claude model ("Claude Sonnet 5.5 High") so acct2 uses Claude first,
	// and auto-continues the stopped conversation!
	prov.byID[r1.Account.ID] = usage.Full{Groups: makePriorityGroups(0, 0, 100)}
	app, out, errOut = env.app("")
	app.AntigravityDataDir = agyDir
	app.AutoContinueTimeout = 5 * time.Second
	app.UsageFull = prov
	app.Desktop.Ops = &desktop.ProcessOps{
		FindProcesses: func() ([]int, error) { return nil, nil },
		KillProcess:   func(pid int) error { return nil },
		StartProcess: func(bin string) error {
			mockCDP.mu.Lock()
			mockCDP.hubOrigin = "https://127.0.0.1:50009"
			mockCDP.currentPath = "/"
			mockCDP.runningIDs = map[string]bool{}
			mockCDP.mu.Unlock()
			return nil
		},
	}
	if code := app.Run([]string{"rotate", "--relaunch"}); code != 0 {
		t.Fatalf("rotate step 4 exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Rotated to acct2") || !strings.Contains(out.String(), "Selected model Claude Sonnet 5.5 High") {
		t.Fatalf("expected rotation to acct2 with Claude Sonnet 5.5 High selected, got: %s", out.String())
	}
	mockCDP.mu.Lock()
	if mockCDP.modelLabel != "Select model, current: Claude Sonnet 5.5 High" {
		t.Fatalf("expected Hub model label restored to Claude Sonnet 5.5 High on acct2, got %q", mockCDP.modelLabel)
	}
	if got := mockCDP.submittedMsgs["conv-claude-stop"]; len(got) != 2 {
		t.Fatalf("expected conv-claude-stop continued again after rotation to acct2, got %v", got)
	}
	mockCDP.mu.Unlock()
}

// mockHubCDPServer simulates Electron's DevToolsActivePort (/json/list + RFC 6455 WebSocket CDP)
// and tracks Hub DOM navigation, drafts, and submitted messages across port changes.
type mockHubCDPServer struct {
	mu            sync.Mutex
	srv           *httptest.Server
	wsPort        int
	hubOrigin     string
	currentPath   string
	modelLabel    string
	runningIDs    map[string]bool
	drafts        map[string]string
	submittedMsgs map[string][]string
}

func newMockHubCDPServer(t *testing.T) *mockHubCDPServer {
	t.Helper()
	m := &mockHubCDPServer{
		hubOrigin:     "https://127.0.0.1:50001",
		currentPath:   "/c/conv-viewing?project=agent",
		modelLabel:    "Select model, current: Gemini 4 Argon High",
		runningIDs:    map[string]bool{"conv-running": true},
		drafts:        map[string]string{"conv-draft": "wait hold on"},
		submittedMsgs: map[string][]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		pageURL := m.hubOrigin + m.currentPath
		wsURL := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/hub-1", m.wsPort)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]cdpTargetInfo{
			{
				ID:                   "hub-1",
				Type:                 "page",
				Title:                "Antigravity",
				URL:                  pageURL,
				WebSocketDebuggerURL: wsURL,
			},
		})
	})
	mux.HandleFunc("/devtools/page/hub-1", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		wsKey := r.Header.Get("Sec-WebSocket-Key")
		accept := computeWSAccept(wsKey)
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		_ = bufrw.Flush()
		m.serveCDPConn(conn, bufrw.Reader)
	})

	m.srv = httptest.NewServer(mux)
	u, err := url.Parse(m.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(u.Host)
	fmt.Sscanf(portStr, "%d", &m.wsPort)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockHubCDPServer) activeConvIDLocked() string {
	p := m.currentPath
	if idx := strings.Index(p, "?"); idx >= 0 {
		p = p[:idx]
	}
	return strings.TrimPrefix(p, "/c/")
}

func (m *mockHubCDPServer) serveCDPConn(conn net.Conn, br *bufio.Reader) {
	for {
		payload, err := readServerWSFrame(br)
		if err != nil {
			return
		}
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(payload, &req) != nil {
			continue
		}
		resVal := m.handleCDPMethod(req.Method, req.Params)
		respBytes, _ := json.Marshal(map[string]any{
			"id":     req.ID,
			"result": resVal,
		})
		if writeServerWSFrame(conn, respBytes) != nil {
			return
		}
	}
}

func (m *mockHubCDPServer) handleCDPMethod(method string, params map[string]any) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch method {
	case "Page.navigate":
		rawURL, _ := params["url"].(string)
		if u, err := url.Parse(rawURL); err == nil {
			m.currentPath = u.RequestURI()
		}
		return map[string]any{"frameId": "main"}

	case "Input.insertText":
		txt, _ := params["text"].(string)
		cid := m.activeConvIDLocked()
		m.drafts[cid] += txt
		return map[string]any{}

	case "Input.dispatchKeyEvent":
		evType, _ := params["type"].(string)
		key, _ := params["key"].(string)
		if evType == "keyDown" && key == "Enter" {
			cid := m.activeConvIDLocked()
			msg := strings.TrimSpace(m.drafts[cid])
			if msg != "" {
				m.submittedMsgs[cid] = append(m.submittedMsgs[cid], msg)
				m.drafts[cid] = ""
				m.runningIDs[cid] = true
			}
		}
		return map[string]any{}

	case "Runtime.evaluate":
		expr, _ := params["expression"].(string)
		val := m.evalExpressionLocked(expr)
		return map[string]any{
			"result": map[string]any{
				"type":  "object",
				"value": val,
			},
		}
	}
	return map[string]any{}
}

func (m *mockHubCDPServer) evalExpressionLocked(expr string) any {
	switch {
	case strings.Contains(expr, "const baseName ="):
		base := ""
		eff := ""
		if idx := strings.Index(expr, `const baseName = "`); idx >= 0 {
			rest := expr[idx+len(`const baseName = "`):]
			if end := strings.IndexByte(rest, '"'); end >= 0 {
				base = rest[:end]
			}
		}
		if idx := strings.Index(expr, `const effort = "`); idx >= 0 {
			rest := expr[idx+len(`const effort = "`):]
			if end := strings.IndexByte(rest, '"'); end >= 0 {
				eff = rest[:end]
			}
		}
		spec := hubModelSpec{BaseModel: base, Effort: eff}
		newLabel := "Select model, current: " + spec.DisplayName()
		changed := m.modelLabel != newLabel
		m.modelLabel = newLabel
		return map[string]any{"ok": true, "changed": changed, "label": newLabel}

	case strings.Contains(expr, "runningIds"):
		var rids []string
		for id, ok := range m.runningIDs {
			if ok {
				rids = append(rids, id)
			}
		}
		return hubPageSnapshot{
			Origin:       m.hubOrigin,
			PathAndQuery: m.currentPath,
			ModelLabel:   m.modelLabel,
			RunningIDs:   rids,
		}

	case strings.Contains(expr, "ready:") && strings.Contains(expr, "location.origin"):
		return map[string]any{
			"ready":  true,
			"origin": m.hubOrigin,
		}

	case strings.Contains(expr, "a.click()"):
		// Extract href="/c/<id>"
		if idx := strings.Index(expr, `/c/`); idx >= 0 {
			rest := expr[idx:]
			end := strings.IndexAny(rest, `"'\`)
			if end > 0 {
				href := rest[:end]
				// Simulate conv-quota being inside a collapsed folder so Page.navigate fallback is also tested!
				if href == "/c/conv-quota" {
					return false
				}
				m.currentPath = href
				return true
			}
		}
		return false

	case strings.Contains(expr, "location.pathname.includes("):
		return strings.HasPrefix(m.currentPath, "/c/")

	case strings.Contains(expr, "already_running") && strings.Contains(expr, "has_draft"):
		cid := m.activeConvIDLocked()
		if m.runningIDs[cid] {
			return map[string]any{"ok": false, "reason": "already_running"}
		}
		if strings.TrimSpace(m.drafts[cid]) != "" {
			return map[string]any{"ok": false, "reason": "has_draft"}
		}
		return map[string]any{"ok": true, "reason": ""}

	case strings.Contains(expr, "return running || empty"):
		cid := m.activeConvIDLocked()
		return m.runningIDs[cid] || strings.TrimSpace(m.drafts[cid]) == ""

	case strings.TrimSpace(expr) == "location.pathname + location.search":
		return m.currentPath
	}
	return nil
}

func readServerWSFrame(br *bufio.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	opcode := hdr[0] & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}
	masked := (hdr[1] & 0x80) != 0
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, nil
}

func writeServerWSFrame(w io.Writer, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x81 // FIN + text
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := w.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func TestAutoContinueAfterRotateMockCDPServerEndToEnd(t *testing.T) {
	env := newTestEnv(t, false)
	app, _, _ := env.app("")

	tok1 := makeCLIToken(t, "exhausted@example.com", "sub-1", "ref-1")
	tok2 := makeCLIToken(t, "healthy@example.com", "sub-2", "ref-2")
	r1, _ := app.Store.Upsert(tok1, "exhausted", true)
	r2, _ := app.Store.Upsert(tok2, "healthy", false)
	_ = os.MkdirAll(filepath.Dir(env.tokenPath), 0o700)
	_ = os.WriteFile(env.tokenPath, tok1, 0o600)

	mockCDP := newMockHubCDPServer(t)
	_ = os.MkdirAll(app.Desktop.ConfigDir, 0o700)
	_ = os.WriteFile(filepath.Join(app.Desktop.ConfigDir, "DevToolsActivePort"), []byte(fmt.Sprintf("%d\n/devtools/browser/mock\n", mockCDP.wsPort)), 0o600)

	agyDir := filepath.Join(env.dir, "antigravity_data")
	brainDir := filepath.Join(agyDir, "brain")
	step0 := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>Work</USER_REQUEST>"}`

	// 1. Quota-stopped conversation (ended at GENERIC 2 minutes ago) -> MUST auto-continue
	writeTranscriptFile(t, brainDir, "conv-quota", testNow.Add(-2*time.Minute), []string{
		step0,
		`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"tool output before quota error"}`,
	})
	// 2. Conversation that was actively running in sidebar before restart -> MUST auto-continue
	writeTranscriptFile(t, brainDir, "conv-running", testNow.Add(-1*time.Minute), []string{
		step0,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"working"}`,
	})
	// 3. Cleanly finished conversation -> must NOT auto-continue
	writeTranscriptFile(t, brainDir, "conv-done", testNow.Add(-3*time.Minute), []string{
		step0,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"all finished"}`,
	})
	// 4. Stopped conversation where user typed an unsent draft -> must NOT auto-continue
	writeTranscriptFile(t, brainDir, "conv-draft", testNow.Add(-4*time.Minute), []string{
		step0,
		`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"stopped"}`,
	})
	// 5. Subagent conversation -> must NOT auto-continue
	writeTranscriptFile(t, brainDir, "conv-subagent", testNow.Add(-1*time.Minute), []string{
		`{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"subagent"}`,
		`{"step_index":1,"source":"SYSTEM","type":"GENERIC","status":"DONE","content":"stopped"}`,
	})

	prov := &mapFullUsageProvider{
		byID: map[string]usage.Full{
			r1.Account.ID: {Groups: makePriorityGroups(0, 0, 100)}, // Both Claude and Argon exhausted on r1 -> rotates to r2
			r2.Account.ID: {Groups: makePriorityGroups(100, 100, 100)},
		},
	}

	app, out, errOut := env.app("")
	app.AntigravityDataDir = agyDir
	app.AutoContinueTimeout = 5 * time.Second
	app.UsageFull = prov
	app.Desktop.Ops = &desktop.ProcessOps{
		FindProcesses: func() ([]int, error) { return nil, nil },
		KillProcess:   func(pid int) error { return nil },
		StartProcess: func(bin string) error {
			// Simulate Electron reloading Hub window on new language_server port (root "/")
			// and clearing running state on conv-running after server restart.
			mockCDP.mu.Lock()
			mockCDP.hubOrigin = "https://127.0.0.1:50002"
			mockCDP.currentPath = "/"
			mockCDP.runningIDs = map[string]bool{}
			mockCDP.mu.Unlock()
			return nil
		},
	}

	if code := app.Run([]string{"rotate", "--relaunch", "--json"}); code != 0 {
		t.Fatalf("rotate --relaunch --json failed (%d): %s", code, errOut.String())
	}

	var envelope struct {
		Data struct {
			Rotated       bool     `json:"rotated"`
			AutoContinued []string `json:"auto_continued"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal json output: %v (%s)", err, out.String())
	}
	if !envelope.Data.Rotated {
		t.Fatalf("expected rotated=true, got %s", out.String())
	}

	gotSet := map[string]bool{}
	for _, id := range envelope.Data.AutoContinued {
		gotSet[id] = true
	}
	if len(gotSet) != 2 || !gotSet["conv-quota"] || !gotSet["conv-running"] {
		t.Fatalf("expected auto_continued=[conv-quota, conv-running], got %v", envelope.Data.AutoContinued)
	}

	mockCDP.mu.Lock()
	if got := mockCDP.submittedMsgs["conv-quota"]; len(got) != 1 || got[0] != "Continue" {
		t.Errorf("conv-quota submittedMsgs = %v, want [Continue]", got)
	}
	if got := mockCDP.submittedMsgs["conv-running"]; len(got) != 1 || got[0] != "Continue" {
		t.Errorf("conv-running submittedMsgs = %v, want [Continue]", got)
	}
	if len(mockCDP.submittedMsgs["conv-done"]) != 0 {
		t.Errorf("conv-done should not have received Continue: %v", mockCDP.submittedMsgs["conv-done"])
	}
	if len(mockCDP.submittedMsgs["conv-draft"]) != 0 || mockCDP.drafts["conv-draft"] != "wait hold on" {
		t.Errorf("conv-draft draft should be preserved untouched, got msgs=%v draft=%q", mockCDP.submittedMsgs["conv-draft"], mockCDP.drafts["conv-draft"])
	}
	if len(mockCDP.submittedMsgs["conv-subagent"]) != 0 {
		t.Errorf("conv-subagent should not have received Continue: %v", mockCDP.submittedMsgs["conv-subagent"])
	}
	// Verify the user's open conversation path was restored after auto-continuing!
	if mockCDP.currentPath != "/c/conv-viewing" && mockCDP.currentPath != "/c/conv-viewing?project=agent" {
		t.Errorf("expected original path restored to /c/conv-viewing, got %q", mockCDP.currentPath)
	}
	mockCDP.runningIDs = map[string]bool{}
	mockCDP.mu.Unlock()

	// Verify --no-auto-continue skips sending any additional Continue messages.
	app2, out2, errOut2 := env.app("")
	app2.AntigravityDataDir = agyDir
	app2.AutoContinueTimeout = 2 * time.Second
	app2.Desktop.Ops = &desktop.ProcessOps{
		FindProcesses: func() ([]int, error) { return nil, nil },
		KillProcess:   func(pid int) error { return nil },
		StartProcess: func(bin string) error {
			mockCDP.mu.Lock()
			mockCDP.hubOrigin = "https://127.0.0.1:50003"
			mockCDP.currentPath = "/"
			mockCDP.mu.Unlock()
			return nil
		},
	}
	if code := app2.Run([]string{"switch", "exhausted", "--relaunch", "--no-auto-continue"}); code != 0 {
		t.Fatalf("switch --no-auto-continue failed (%d): %s", code, errOut2.String())
	}
	if strings.Contains(out2.String(), "Auto-continued") {
		t.Errorf("unexpected Auto-continued output when --no-auto-continue set: %s", out2.String())
	}
	mockCDP.mu.Lock()
	if len(mockCDP.submittedMsgs["conv-quota"]) != 1 {
		t.Errorf("expected no extra Continue with --no-auto-continue, got %v", mockCDP.submittedMsgs["conv-quota"])
	}
	mockCDP.mu.Unlock()
}

type countingFullProvider struct {
	mu    sync.Mutex
	calls map[string]int
	byID  map[string]usage.Full
}

func (c *countingFullProvider) FetchFull(_ context.Context, key string, _ *agy.TokenFile) (usage.Full, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[key]++
	full, ok := c.byID[key]
	c.mu.Unlock()
	if !ok {
		return usage.Full{}, usage.ErrUnavailable
	}
	return full, nil
}

func TestRotateHealthyActiveQueriesOnlyActiveAccount(t *testing.T) {
	env := newTestEnv(t, false)
	app, _, _ := env.app("")

	var ids []string
	var tok1 []byte
	for i := 0; i < 6; i++ {
		tok := makeCLIToken(t, fmt.Sprintf("u%d@example.com", i), fmt.Sprintf("sub-%d", i), fmt.Sprintf("ref-%d", i))
		res, err := app.Store.Upsert(tok, fmt.Sprintf("acct-%d", i), i == 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Account.ID)
		if i == 0 {
			tok1 = tok
		}
	}
	_ = os.MkdirAll(filepath.Dir(env.tokenPath), 0o700)
	_ = os.WriteFile(env.tokenPath, tok1, 0o600)

	byID := map[string]usage.Full{}
	for _, id := range ids {
		byID[id] = usage.Full{Groups: makePriorityGroups(85, 100, 100)}
	}
	prov := &countingFullProvider{byID: byID}

	app, _, errOut := env.app("")
	app.UsageFull = prov
	if code := app.Run([]string{"rotate"}); code != 0 {
		t.Fatalf("healthy rotate failed (%d): %s", code, errOut.String())
	}
	if prov.calls[ids[0]] != 1 || len(prov.calls) != 1 {
		t.Fatalf("healthy rotate should query only active account once, got calls=%v", prov.calls)
	}

	// Now exhaust active account -> should enrich each remaining candidate once without re-querying ids[0].
	prov.calls = map[string]int{}
	prov.byID[ids[0]] = usage.Full{Groups: makePriorityGroups(0, 0, 100)}
	app, _, errOut = env.app("")
	app.UsageFull = prov
	if code := app.Run([]string{"rotate", "--no-relaunch"}); code != 0 {
		t.Fatalf("exhausted rotate failed (%d): %s", code, errOut.String())
	}
	for _, id := range ids {
		if prov.calls[id] != 1 {
			t.Fatalf("expected each account queried exactly once during candidate rotation, got calls=%v", prov.calls)
		}
	}
}
