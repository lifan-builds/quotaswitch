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

func TestPoolAPIKeyExpiryFirstAndCooldown(t *testing.T) {
	tmp := t.TempDir()
	codexStore := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	now := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour)
	soon := now.Add(14 * 24 * time.Hour)
	later := now.Add(25 * 24 * time.Hour)

	activeID := "oauth-active"
	oauthAcct := "chatgpt-1"
	keyExpired := "test-key-expired"
	keyLater := "test-key-later"
	keySoon := "test-key-soon"

	storeData := &codex.AccountsStore{
		ActiveAccountID: &activeID,
		Accounts: []codex.StoredAccount{
			{
				ID:        "oauth-active",
				Name:      "active@example.com",
				AuthMode:  codex.AuthModeChatGPT,
				AuthData:  codex.AuthData{AccessToken: "oauth-tok-1", AccountID: &oauthAcct},
				CreatedAt: now,
			},
			{
				ID:                    "api-expired",
				Name:                  "api-expired",
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Key: &keyExpired},
				SubscriptionExpiresAt: &past,
				CreatedAt:             now,
			},
			{
				ID:                    "api-later",
				Name:                  "api-later",
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Key: &keyLater},
				SubscriptionExpiresAt: &later,
				CreatedAt:             now,
			},
			{
				ID:                    "api-soon",
				Name:                  "api-soon",
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Key: &keySoon},
				SubscriptionExpiresAt: &soon,
				CreatedAt:             now,
			},
		},
	}
	if err := codexStore.Save(storeData); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(codexStore, nil, nil, nil)
	pool.Now = func() time.Time { return now }

	cands, err := pool.CodexCandidates()
	if err != nil {
		t.Fatalf("CodexCandidates failed: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("expected 3 non-expired candidates, got %d", len(cands))
	}
	// LLM default: OAuth subscription seats FIRST, then non-expired API keys ordered by earliest expiry.
	wantOrder := []string{"oauth-active", "api-soon", "api-later"}
	for i, want := range wantOrder {
		if cands[i].ID != want {
			t.Fatalf("candidate[%d] = %q, want %q", i, cands[i].ID, want)
		}
	}

	// Explicit api_key mode (or Platform endpoints): earliest-expiry API keys FIRST.
	apiFirst, err := pool.CodexCandidatesWithMode("", "api_key")
	if err != nil {
		t.Fatalf("CodexCandidatesWithMode(api_key) failed: %v", err)
	}
	if apiFirst[0].ID != "api-soon" || apiFirst[1].ID != "api-later" || apiFirst[2].ID != "oauth-active" {
		t.Fatalf("unexpected api_key mode order: %q, %q, %q", apiFirst[0].ID, apiFirst[1].ID, apiFirst[2].ID)
	}

	// Mark insufficient_quota (6h cooldown) on api-soon and cooldown oauth-active -> api-later leads!
	pool.MarkCooldown(BackendCodex, "oauth-active", defaultCooldown)
	pool.MarkCooldown(BackendCodex, "api-soon", cooldownForError(http.StatusTooManyRequests, "insufficient_quota: exceeded your current quota"))
	if !pool.isCoolingDown(BackendCodex, "api-soon", now.Add(5*time.Hour)) {
		t.Fatalf("expected api-soon to remain on 6h cooldown after insufficient_quota")
	}
	candsAfter, err := pool.CodexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if candsAfter[0].ID != "api-later" || candsAfter[1].ID != "oauth-active" || candsAfter[2].ID != "api-soon" {
		t.Fatalf("unexpected candidate order after cooldown: %q, %q, %q", candsAfter[0].ID, candsAfter[1].ID, candsAfter[2].ID)
	}
}

func TestOpenAIPlatformPassthroughAndPinning(t *testing.T) {
	tmp := t.TempDir()
	codexStore := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	now := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	soon := now.Add(14 * 24 * time.Hour)
	later := now.Add(25 * 24 * time.Hour)
	keySoon := "test-key-soon"
	keyLater := "test-key-later"

	if err := codexStore.Save(&codex.AccountsStore{
		Accounts: []codex.StoredAccount{
			{
				ID:                    "id-soon",
				Name:                  "api-soon",
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Key: &keySoon},
				SubscriptionExpiresAt: &soon,
				CreatedAt:             now,
			},
			{
				ID:                    "id-later",
				Name:                  "api-later",
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Key: &keyLater},
				SubscriptionExpiresAt: &later,
				CreatedAt:             now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Quotaswitch-Account") != "" {
			t.Errorf("X-Quotaswitch-Account header should be stripped upstream")
		}
		auth := r.Header.Get("Authorization")
		if r.URL.Path == "/v1/images/generations" {
			if auth == "Bearer test-key-soon" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`))
				return
			}
			if auth != "Bearer test-key-later" {
				t.Errorf("unexpected image auth: %s", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
			return
		}
		if r.URL.Path == "/v1/batches" {
			if auth != "Bearer test-key-later" {
				t.Errorf("expected pinned batch call to use test-key-later, got %s", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"batch_123","status":"validating"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	pool := NewPool(codexStore, nil, nil, nil)
	pool.Now = func() time.Time { return now }
	srv := NewServer(pool, tmp, "quotaswitch-local")
	srv.OpenAIBaseURL = upstream.URL

	// 1. /v1/images/generations fails over from api-soon (insufficient_quota) -> api-later
	imgReq := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2.6-sunburst","prompt":"mountain"}`))
	imgReq.Header.Set("Content-Type", "application/json")
	imgRec := httptest.NewRecorder()
	srv.ServeHTTP(imgRec, imgReq)
	if imgRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on images failover, got %d: %s", imgRec.Code, imgRec.Body.String())
	}
	if got := imgRec.Header().Get("X-Quotaswitch-Account-ID"); got != "api-later" {
		t.Fatalf("expected X-Quotaswitch-Account-ID=api-later, got %q", got)
	}

	// 2. /v1/batches with X-Quotaswitch-Account pinning routes directly to api-later
	batchReq := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(`{"input_file_id":"file_1","endpoint":"/v1/responses","completion_window":"24h"}`))
	batchReq.Header.Set("Content-Type", "application/json")
	batchReq.Header.Set("X-Quotaswitch-Account", "api-later")
	batchRec := httptest.NewRecorder()
	srv.ServeHTTP(batchRec, batchReq)
	if batchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on pinned batch create, got %d: %s", batchRec.Code, batchRec.Body.String())
	}
	if got := batchRec.Header().Get("X-Quotaswitch-Account-ID"); got != "api-later" {
		t.Fatalf("expected X-Quotaswitch-Account-ID=api-later, got %q", got)
	}
}
