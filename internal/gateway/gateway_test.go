package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func TestResolveModelAndCatalog(t *testing.T) {
	tmp := t.TempDir()
	cacheJSON := `{
		"client_version": "0.167.0",
		"models": [
			{"slug": "gpt-6.1-sol", "display_name": "gpt-6.1-sol", "description": "Frontier model", "context_window": 500000, "input_modalities": ["text", "image"]},
			{"slug": "gpt-6-astra", "display_name": "gpt-6-astra", "description": "Fast model", "context_window": 272000, "input_modalities": ["text", "image"]}
		]
	}`
	if err := os.WriteFile(filepath.Join(tmp, "models_cache.json"), []byte(cacheJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	entries := CombinedModelEntries(tmp)
	if len(entries) < 6 {
		t.Fatalf("expected combined entries >= 6, got %d", len(entries))
	}

	backend, upstream, effort := ResolveModel("gpt-6.1-sol-high", tmp)
	if backend != BackendCodex || upstream != "gpt-6.1-sol" || effort != "high" {
		t.Fatalf("unexpected ResolveModel for gpt-6.1-sol-high: backend=%q upstream=%q effort=%q", backend, upstream, effort)
	}

	backend, upstream, _ = ResolveModel("gpt-5.4", tmp)
	if backend != BackendCodex || upstream != "gpt-6.1-sol" {
		t.Fatalf("expected gpt-5.4 alias to resolve to gpt-6.1-sol, got backend=%q upstream=%q", backend, upstream)
	}

	backend, upstream, _ = ResolveModel("claude-opus-4-6-thinking", tmp)
	if backend != BackendAgy || upstream != "claude-opus-4-6-thinking" {
		t.Fatalf("expected claude-opus-4-6-thinking to resolve to agy backend, got backend=%q upstream=%q", backend, upstream)
	}
}

func TestEnableAndDisableAntigravitySettings(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, ".antigravity", "settings.json")

	cfg, err := EnableAntigravitySettings(settingsPath, "http://127.0.0.1:18455", "test-key", "gpt-6.1-sol", tmp)
	if err != nil {
		t.Fatalf("EnableAntigravitySettings failed: %v", err)
	}
	if cfg.Gateway == nil || !cfg.Gateway.Enabled || cfg.Gateway.WireProtocol != "openai" {
		t.Fatalf("unexpected gateway config: %+v", cfg.Gateway)
	}
	if cfg.Models == nil || cfg.Models.DefaultModelID != "gpt-6.1-sol" || len(cfg.Models.List) == 0 {
		t.Fatalf("unexpected models config: %+v", cfg.Models)
	}

	loaded, err := LoadAntigravitySettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadAntigravitySettings failed: %v", err)
	}
	if loaded.Gateway == nil || !loaded.Gateway.Enabled {
		t.Fatalf("expected loaded gateway to be enabled")
	}

	if err := DisableAntigravitySettings(settingsPath); err != nil {
		t.Fatalf("DisableAntigravitySettings failed: %v", err)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("expected settings file to be removed when empty after disable, got err=%v", err)
	}
}

func TestCodexChatCompletionsFailoverAndStreaming(t *testing.T) {
	tmp := t.TempDir()
	codexStore := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	acc1ID := "acc-1"
	acct1 := "chatgpt-acct-1"
	acct2 := "chatgpt-acct-2"
	now := time.Now().UTC()
	data := &codex.AccountsStore{
		ActiveAccountID: &acc1ID,
		Accounts: []codex.StoredAccount{
			{
				ID:       "acc-1",
				Name:     "user1@example.com",
				AuthMode: codex.AuthModeChatGPT,
				AuthData: codex.AuthData{
					AccessToken: "tok-1",
					AccountID:   &acct1,
				},
				CreatedAt:  now,
				LastUsedAt: &now,
			},
			{
				ID:       "acc-2",
				Name:     "user2@example.com",
				AuthMode: codex.AuthModeChatGPT,
				AuthData: codex.AuthData{
					AccessToken: "tok-2",
					AccountID:   &acct2,
				},
				CreatedAt:  now,
				LastUsedAt: &now,
			},
		},
	}
	if err := codexStore.Save(data); err != nil {
		t.Fatal(err)
	}

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer tok-1" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"usage_limit_reached"}}`))
			return
		}
		if auth != "Bearer tok-2" {
			t.Errorf("unexpected auth header on call %d: %s", n, auth)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_test_123"}}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.reasoning_summary_text.delta","delta":"Thinking step..."}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"Hello from GPT-6.1-Sol!"}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_item.added","item":{"id":"fc_1","call_id":"call_abc","type":"function_call","name":"run_cmd"}}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"cmd\":\"ls\"}"}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_test_123","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`)
	}))
	defer upstream.Close()

	pool := NewPool(codexStore, nil, nil, nil)
	srv := NewServer(pool, tmp, "test-key")
	srv.CodexResponsesURL = upstream.URL

	// 1. Test streaming request with failover from acc-1 (429) -> acc-2 (200)
	reqBody := `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotAcct := rec.Header().Get("X-Quotaswitch-Account"); gotAcct != "user2@example.com" {
		t.Fatalf("expected failover to user2@example.com, got %q", gotAcct)
	}

	scanner := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
	var sawText, sawReasoning, sawTool, sawDone bool
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("invalid chunk JSON %q: %v", payload, err)
		}
		if len(chunk.Choices) == 0 {
			t.Fatalf("every chunk must have len(choices) >= 1 for language_server compatibility, got 0 in %s", payload)
		}
		d := chunk.Choices[0].Delta
		if strings.Contains(d.Content, "Hello from GPT-6.1-Sol!") {
			sawText = true
		}
		if strings.Contains(d.ReasoningContent, "Thinking step...") {
			sawReasoning = true
		}
		if len(d.ToolCalls) > 0 && d.ToolCalls[0].Function.Name == "run_cmd" {
			sawTool = true
		}
	}
	if !sawText || !sawReasoning || !sawTool || !sawDone {
		t.Fatalf("missing expected stream events: text=%v reasoning=%v tool=%v done=%v\nbody:\n%s",
			sawText, sawReasoning, sawTool, sawDone, rec.Body.String())
	}
}
