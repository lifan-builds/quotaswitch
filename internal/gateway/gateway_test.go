// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
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
	req.Header.Set("Authorization", "Bearer test-key")
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

	for _, tc := range []struct {
		name, filter, mode string
		want               []string
	}{
		{name: "default", want: []string{"oauth-active", "api-soon", "api-later"}},
		{name: "auto", mode: "auto", want: []string{"oauth-active", "api-soon", "api-later"}},
		{name: "oauth", mode: "oauth", want: []string{"oauth-active"}},
		{name: "api_key", mode: " API_KEY ", want: []string{"api-soon", "api-later"}},
		{name: "apikey", mode: "apikey", want: []string{"api-soon", "api-later"}},
		{name: "credits", mode: "credits", want: []string{"api-soon", "api-later"}},
		{name: "oauth cannot use a pinned key", filter: "api-soon", mode: "oauth"},
		{name: "credits cannot use a pinned seat", filter: "oauth-active", mode: "credits"},
		{name: "unknown mode", mode: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands, err := pool.CodexCandidatesWithMode(tc.filter, tc.mode)
			if tc.want == nil {
				if err == nil || len(cands) != 0 {
					t.Fatalf("got %d candidates, err = %v, want no candidates and an error", len(cands), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, cand := range cands {
				got = append(got, cand.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("candidate IDs = %v, want %v", got, tc.want)
			}
		})
	}

	// Mark insufficient_quota (6h cooldown) on api-soon and cooldown oauth-active -> api-later leads!
	pool.MarkCooldown(BackendCodex, "oauth-active", defaultCooldown)
	pool.MarkCooldown(BackendCodex, "api-soon", cooldownForError(http.StatusTooManyRequests, "insufficient_quota: exceeded your current quota"))
	if !pool.isCoolingDown(BackendCodex, "api-soon", now.Add(5*time.Hour)) {
		t.Fatalf("expected api-soon to remain on 6h cooldown after insufficient_quota")
	}
	candsAfter, err := pool.CodexCandidatesWithMode("", "")
	if err != nil {
		t.Fatal(err)
	}
	if candsAfter[0].ID != "api-later" || candsAfter[1].ID != "oauth-active" || candsAfter[2].ID != "api-soon" {
		t.Fatalf("unexpected candidate order after cooldown: %q, %q, %q", candsAfter[0].ID, candsAfter[1].ID, candsAfter[2].ID)
	}
}

func TestPoolAPIKeyRoundRobinWithinSoonestExpiry(t *testing.T) {
	tmp := t.TempDir()
	store := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	now := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	soonest := now.Add(time.Hour)
	sameDay := soonest.Add(2 * time.Hour)
	within24Hours := soonest.Add(24*time.Hour - time.Second)
	later := soonest.Add(24 * time.Hour)
	data := &codex.AccountsStore{}
	for _, key := range []struct {
		id     string
		expiry *time.Time
	}{
		{id: "later", expiry: &later},
		{id: "undated"},
		{id: "within24Hours", expiry: &within24Hours},
		{id: "soonest", expiry: &soonest},
		{id: "sameDay", expiry: &sameDay},
	} {
		value := "test-key-" + key.id
		data.Accounts = append(data.Accounts, codex.StoredAccount{
			ID:                    key.id,
			AuthMode:              codex.AuthModeAPIKey,
			AuthData:              codex.AuthData{Key: &value},
			SubscriptionExpiresAt: key.expiry,
		})
	}
	if err := store.Save(data); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(store, nil, nil, nil)
	pool.Now = func() time.Time { return now }

	for i, want := range [][]string{
		{"soonest", "sameDay", "within24Hours", "later", "undated"},
		{"sameDay", "within24Hours", "soonest", "later", "undated"},
		{"within24Hours", "soonest", "sameDay", "later", "undated"},
		{"soonest", "sameDay", "within24Hours", "later", "undated"},
	} {
		cands, err := pool.OpenAIAPIKeyCandidates("")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, cand := range cands {
			got = append(got, cand.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d candidate IDs = %v, want %v", i+1, got, want)
		}
	}
}

func platformTestServer(t *testing.T) *Server {
	t.Helper()
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

	pool := NewPool(codexStore, nil, nil, nil)
	pool.Now = func() time.Time { return now }
	return NewServer(pool, tmp, "quotaswitch-local")
}

func TestPlatformJobCreateFailover(t *testing.T) {
	for _, path := range []string{"/v1/batches", "/v1/videos"} {
		for _, tc := range []struct {
			name       string
			status     int
			body       string
			wantStatus int
			wantCalls  int
		}{
			{"server error", 503, "upstream unavailable", 503, 1},
			{"gateway timeout", 504, "quota service timed out", 504, 1},
			{"transport timeout", 0, "", 502, 1},
			{"unauthorized", 401, "invalid credential", 200, 2},
			{"forbidden", 403, "access denied", 200, 2},
			{"rate limited", 429, "rate limited", 200, 2},
			{"insufficient quota", 400, `{"error":{"code":"insufficient_quota"}}`, 200, 2},
			{"invalid request", 400, "invalid model", 400, 1},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				srv := platformTestServer(t)
				calls := 0
				srv.HTTPClient.Transport = codexEndpointTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					status, body := http.StatusOK, `{"id":"created"}`
					if calls == 1 {
						if tc.status == 0 {
							return nil, context.DeadlineExceeded
						}
						status, body = tc.status, tc.body
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
				req.Header.Set("X-API-Key", srv.APIKey)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus || calls != tc.wantCalls {
					t.Fatalf("status = %d, calls = %d, want %d and %d", rec.Code, calls, tc.wantStatus, tc.wantCalls)
				}
				if tc.wantCalls == 1 && tc.status != 0 && rec.Body.String() != tc.body {
					t.Fatalf("upstream error body = %q, want %q", rec.Body.String(), tc.body)
				}
			})
		}
	}
}

func TestPlatformResponseLifecycle(t *testing.T) {
	srv := platformTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/responses/resp_123?include=usage"},
		{http.MethodDelete, "/v1/responses/resp_123"},
		{http.MethodPost, "/v1/responses/resp_123/cancel"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			calls := 0
			srv.HTTPClient.Transport = codexEndpointTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != tc.method || r.URL.String() != "https://api.openai.com"+tc.path {
					t.Errorf("upstream received %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer test-key-later" || r.Header.Get("X-Quotaswitch-Account") != "" || r.Header.Get("X-API-Key") != "" {
					t.Error("upstream headers did not use the pinned credential exclusively")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"resp_123"}`))}, nil
			})
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("X-Quotaswitch-Account", "api-later")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || calls != 0 {
				t.Fatalf("unauthenticated status = %d, upstream calls = %d", rec.Code, calls)
			}
			req.Header.Set("X-API-Key", srv.APIKey)
			rec = httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || calls != 1 || rec.Header().Get("X-Quotaswitch-Account-ID") != "api-later" || rec.Body.String() != `{"id":"resp_123"}` {
				t.Fatalf("status = %d, calls = %d, body = %s", rec.Code, calls, rec.Body.String())
			}
		})
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/responses/resp_123"},
		{http.MethodGet, "/v1/responses/resp_123/cancel"},
		{http.MethodPost, "/v1/responses//cancel"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-API-Key", srv.APIKey)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestMultipartImageEditCreditDelta(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	image, err := form.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := image.Write([]byte{0xff, 0, 0xfe}); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"model": "gpt-image-1", "size": "1536x1024", "quality": "high", "n": "3"} {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	delta, ok := buildPlatformCreditDelta("/v1/images/edits", form.FormDataContentType(), body.Bytes(), nil)
	if !ok || delta.Model != "gpt-image-1" || delta.Requests != 1 || delta.Images != 3 || delta.CostUSD != codex.EstimateImageCostUSD("gpt-image-1", "high", "1536x1024", 3) {
		t.Fatalf("unexpected multipart edit delta %+v, recorded = %t", delta, ok)
	}
}

func TestOpenAIPlatformPassthroughAndPinning(t *testing.T) {
	srv := platformTestServer(t)
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

	srv.OpenAIBaseURL = upstream.URL

	// 1. /v1/images/generations fails over from api-soon (insufficient_quota) -> api-later
	imgReq := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2.6-sunburst","prompt":"mountain"}`))
	imgReq.Header.Set("Authorization", "Bearer quotaswitch-local")
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
	batchReq.Header.Set("Authorization", "Bearer quotaswitch-local")
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

func TestGatewayCancellationDoesNotFailOver(t *testing.T) {
	for _, route := range []struct{ name, path, model string }{
		{"codex chat", "/v1/chat/completions", "gpt-6.1-sol"},
		{"codex responses", "/v1/responses", "gpt-6.1-sol"},
		{"antigravity chat", "/v1/chat/completions", "gemini-3-flash"},
		{"platform", "/v1/embeddings", "text-embedding-3-small"},
	} {
		for _, stage := range []string{"before attempt", "during attempt", "wrapped cancellation", "error body", "response stream"} {
			t.Run(route.name+"/"+stage, func(t *testing.T) {
				srv := platformTestServer(t)
				if route.model == "gemini-3-flash" {
					addGatewayTestAgyAccount(t, srv)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantCalls := 1
				if stage == "before attempt" {
					cancel()
					wantCalls = 0
				}
				calls := 0
				srv.HTTPClient.Transport = codexEndpointTransport(func(*http.Request) (*http.Response, error) {
					calls++
					canceled := fmt.Errorf("canceled upstream operation: %w", context.Canceled)
					switch stage {
					case "during attempt":
						cancel()
						return nil, io.ErrUnexpectedEOF
					case "error body", "response stream":
						status := http.StatusOK
						if stage == "error body" {
							status = http.StatusTooManyRequests
						}
						return &http.Response{
							StatusCode: status,
							Header:     http.Header{"Content-Type": {"text/event-stream"}},
							Body:       io.NopCloser(canceledGatewayReader{canceled}),
						}, nil
					default:
						return nil, canceled
					}
				})
				body := fmt.Sprintf(`{"model":%q,"stream":true}`, route.model)
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, route.path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+srv.APIKey)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if calls != wantCalls || len(srv.Pool.cooldownUntil) != 0 || rec.Body.Len() != 0 {
					t.Fatalf("upstream calls = %d, want %d, cooldowns = %v, body = %q", calls, wantCalls, srv.Pool.cooldownUntil, rec.Body.String())
				}
			})
		}
	}
}

func TestGatewayDownstreamDisconnectStopsResponse(t *testing.T) {
	const agyReply = "data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"first"},{"text":"second"}]}}]}}` + "\n\n"
	const quotaError = "data: " + `{"type":"error","code":"rate_limit_exceeded","message":"quota unavailable"}` + "\n\n"
	for _, tc := range []struct {
		name, path, model, upstream string
		stream, failFlush           bool
	}{
		{"codex chat write", "/v1/chat/completions", "gpt-6.1-sol", completedCodexTestStream, true, false},
		{"codex chat flush", "/v1/chat/completions", "gpt-6.1-sol", completedCodexTestStream, true, true},
		{"codex collected write", "/v1/chat/completions", "gpt-6.1-sol", completedCodexTestStream, false, false},
		{"codex responses flush", "/v1/responses", "gpt-6.1-sol", quotaError, true, true},
		{"antigravity chat write", "/v1/chat/completions", "gemini-3-flash", agyReply, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := platformTestServer(t)
			if tc.model == "gemini-3-flash" {
				addGatewayTestAgyAccount(t, srv)
			}
			calls := 0
			srv.HTTPClient.Transport = codexEndpointTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(tc.upstream)),
				}, nil
			})
			body := fmt.Sprintf(`{"model":%q,"stream":%t}`, tc.model, tc.stream)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+srv.APIKey)
			w := &disconnectedGatewayWriter{ResponseRecorder: httptest.NewRecorder(), failFlush: tc.failFlush}
			srv.ServeHTTP(w, req)
			if calls != 1 || w.writes != 1 || len(srv.Pool.cooldownUntil) != 0 {
				t.Fatalf("upstream calls = %d, response writes = %d, cooldowns = %v", calls, w.writes, srv.Pool.cooldownUntil)
			}
		})
	}
}

func addGatewayTestAgyAccount(t *testing.T, srv *Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agy-token.json")
	if err := os.WriteFile(path, []byte(`{"token":{"access_token":"test-agy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.Pool.AgyStorage = &agy.Storage{NoKeyring: true, FilePath: path}
	srv.Pool.projectCache["live"] = "test-project"
}

type canceledGatewayReader struct{ err error }

func (r canceledGatewayReader) Read([]byte) (int, error) { return 0, r.err }

type disconnectedGatewayWriter struct {
	*httptest.ResponseRecorder
	failFlush bool
	writes    int
}

func (w *disconnectedGatewayWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failFlush {
		return w.ResponseRecorder.Write(p)
	}
	return 0, io.ErrClosedPipe
}

func (w *disconnectedGatewayWriter) FlushError() error { return io.ErrClosedPipe }

func TestCodexGatewayUsesFastTierAndRedeemsResetOnQuotaError(t *testing.T) {
	tmp := t.TempDir()
	codexStore := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	accID := "acc-reset"
	ws := "ws-gw-reset"
	now := time.Now().UTC()
	if err := codexStore.Save(&codex.AccountsStore{
		ActiveAccountID: &accID,
		Accounts: []codex.StoredAccount{{
			ID:        accID,
			Name:      "reset@example.com",
			AuthMode:  codex.AuthModeChatGPT,
			AuthData:  codex.AuthData{AccessToken: "tok-gw", AccountID: &ws},
			CreatedAt: now,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var calls int32
	var seenServiceTier string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenServiceTier, _ = body["service_tier"].(string)
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"usage_limit_exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_reset_ok"}}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"ok after reset"}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_reset_ok"}}`)
	}))
	defer upstream.Close()

	var redeemed int32
	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": 100.0, "limit_window_seconds": 18000, "reset_at": now.Add(time.Hour).Unix()},
				"secondary_window": map[string]any{"used_percent": 100.0, "limit_window_seconds": 604800, "reset_at": now.Add(24 * time.Hour).Unix()},
			},
		})
	}))
	defer whamSrv.Close()

	resetMux := http.NewServeMux()
	resetMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/consume" {
			atomic.AddInt32(&redeemed, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "reset", "credit": map[string]any{"id": "gw-credit-1"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available_count": 1,
			"credits":         []map[string]any{{"id": "gw-credit-1", "status": "available", "is_supported_by_plan": true, "expires_at": now.Add(48 * time.Hour).Format(time.RFC3339)}},
		})
	})
	resetSrv := httptest.NewServer(resetMux)
	defer resetSrv.Close()

	pool := NewPool(codexStore, nil, nil, nil)
	srv := NewServer(pool, tmp, "test-key")
	srv.CodexResponsesURL = upstream.URL
	srv.CodexUsage = codex.NewUsageClient()
	srv.CodexUsage.UsageURL = whamSrv.URL
	srv.CodexUsage.ResetCreditsURL = resetSrv.URL

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.2-astra","stream":false,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seenServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority (fast mode)", seenServiceTier)
	}
	if atomic.LoadInt32(&redeemed) != 1 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("redeemed=%d calls=%d, want redeemed=1 calls=2", redeemed, calls)
	}
	if !strings.Contains(rec.Body.String(), "ok after reset") {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

func TestCodexGatewaySkipsResetWhenOnlyFiveHourExhausted(t *testing.T) {
	tmp := t.TempDir()
	codexStore := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	acc1 := "acc-5h-limited"
	acc2 := "acc-backup"
	ws1 := "ws-5h-limited"
	ws2 := "ws-backup"
	now := time.Now().UTC()
	if err := codexStore.Save(&codex.AccountsStore{
		ActiveAccountID: &acc1,
		Accounts: []codex.StoredAccount{
			{
				ID:        acc1,
				Name:      "fivehour@example.com",
				AuthMode:  codex.AuthModeChatGPT,
				AuthData:  codex.AuthData{AccessToken: "tok-1", AccountID: &ws1},
				CreatedAt: now,
			},
			{
				ID:        acc2,
				Name:      "backup@example.com",
				AuthMode:  codex.AuthModeChatGPT,
				AuthData:  codex.AuthData{AccessToken: "tok-2", AccountID: &ws2},
				CreatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("ChatGPT-Account-Id") == ws1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"usage_limit_exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_backup_ok"}}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"ok from backup"}`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_backup_ok"}}`)
	}))
	defer upstream.Close()

	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": 100.0, "limit_window_seconds": 18000, "reset_at": now.Add(time.Hour).Unix()},
				"secondary_window": map[string]any{"used_percent": 20.0, "limit_window_seconds": 604800, "reset_at": now.Add(5 * 24 * time.Hour).Unix()},
			},
		})
	}))
	defer whamSrv.Close()

	var redeemed int32
	resetMux := http.NewServeMux()
	resetMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/consume" {
			atomic.AddInt32(&redeemed, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "reset", "credit": map[string]any{"id": "gw-credit-5h"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available_count": 2,
			"credits":         []map[string]any{{"id": "gw-credit-5h", "status": "available", "is_supported_by_plan": true, "expires_at": now.Add(48 * time.Hour).Format(time.RFC3339)}},
		})
	})
	resetSrv := httptest.NewServer(resetMux)
	defer resetSrv.Close()

	pool := NewPool(codexStore, nil, nil, nil)
	srv := NewServer(pool, tmp, "test-key")
	srv.CodexResponsesURL = upstream.URL
	srv.CodexUsage = codex.NewUsageClient()
	srv.CodexUsage.UsageURL = whamSrv.URL
	srv.CodexUsage.ResetCreditsURL = resetSrv.URL

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.2-astra","stream":false,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&redeemed) != 0 {
		t.Fatalf("redeemed=%d, want 0 when weekly quota still has 80%% remaining", redeemed)
	}
	if atomic.LoadInt32(&calls) != 2 || !strings.Contains(rec.Body.String(), "ok from backup") {
		t.Fatalf("expected failover to backup seat, calls=%d body=%s", calls, rec.Body.String())
	}
}
