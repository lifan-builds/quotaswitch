// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func makeGatewayTestJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestChatPromptCacheKeyAndSessionHeadersOnWire(t *testing.T) {
	for _, tc := range []struct {
		name           string
		authPref       string
		cacheKey       string
		sessionID      string
		conversationID string
		wantKey        bool
		wantHeaders    bool
	}{
		{
			name:           "oauth forwards prompt_cache_key and session headers across failover",
			authPref:       "oauth",
			cacheKey:       "team/α:07",
			sessionID:      "sess-abc-123",
			conversationID: "conv-xyz-789",
			wantKey:        true,
			wantHeaders:    true,
		},
		{
			name:        "oauth omits prompt_cache_key and session headers when absent",
			authPref:    "oauth",
			wantKey:     false,
			wantHeaders: false,
		},
		{
			name:           "api key forwards prompt_cache_key without oauth session headers",
			authPref:       "api_key",
			cacheKey:       "team/api:09",
			sessionID:      "sess-should-not-forward",
			conversationID: "conv-should-not-forward",
			wantKey:        true,
			wantHeaders:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			store := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
			now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
			exp := now.Add(24 * time.Hour)
			activeID := "oauth-1"
			ws1, ws2 := "ws-1", "ws-2"
			apiKey := "sk-test-key"
			if err := store.Save(&codex.AccountsStore{
				ActiveAccountID: &activeID,
				Accounts: []codex.StoredAccount{
					{
						ID:       "oauth-1",
						Name:     "oauth-1",
						AuthMode: codex.AuthModeChatGPT,
						AuthData: codex.AuthData{AccessToken: "tok-1", AccountID: &ws1},
					},
					{
						ID:       "oauth-2",
						Name:     "oauth-2",
						AuthMode: codex.AuthModeChatGPT,
						AuthData: codex.AuthData{AccessToken: "tok-2", AccountID: &ws2},
					},
					{
						ID:                    "api-1",
						Name:                  "api-1",
						AuthMode:              codex.AuthModeAPIKey,
						AuthData:              codex.AuthData{Key: &apiKey},
						SubscriptionExpiresAt: &exp,
					},
				},
			}); err != nil {
				t.Fatal(err)
			}

			type observedReq struct {
				auth           string
				sessionID      string
				conversationID string
				cookie         string
				acctFilter     string
				body           map[string]any
			}
			var mu sync.Mutex
			var seen []observedReq
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Errorf("decode upstream body: %v", err)
				}
				mu.Lock()
				seen = append(seen, observedReq{
					auth:           r.Header.Get("Authorization"),
					sessionID:      r.Header.Get("session_id"),
					conversationID: r.Header.Get("conversation_id"),
					cookie:         r.Header.Get("Cookie"),
					acctFilter:     r.Header.Get("X-Quotaswitch-Account"),
					body:           decoded,
				})
				mu.Unlock()
				if r.Header.Get("Authorization") == "Bearer tok-1" && tc.wantKey {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"message":"rate_limit_exceeded"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_cache_1"}}`)
				_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"ok"}`)
				_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_cache_1","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`)
			}))
			defer upstream.Close()

			pool := NewPool(store, nil, nil, nil)
			pool.Now = func() time.Time { return now }
			srv := NewServer(pool, tmp, "gw-secret")
			srv.CodexResponsesURL = upstream.URL

			reqMap := map[string]any{
				"model":    "gpt-6.2-astra",
				"stream":   false,
				"messages": []map[string]any{{"role": "user", "content": "ping"}},
			}
			if tc.cacheKey != "" {
				reqMap["prompt_cache_key"] = tc.cacheKey
			}
			reqBytes, err := json.Marshal(reqMap)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBytes)))
			req.Header.Set("Authorization", "Bearer gw-secret")
			req.Header.Set("X-Quotaswitch-Auth", tc.authPref)
			req.Header.Set("Cookie", "gw_session=secret")
			if tc.sessionID != "" {
				req.Header.Set("session_id", tc.sessionID)
			}
			if tc.conversationID != "" {
				req.Header.Set("conversation_id", tc.conversationID)
			}

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			wantAttempts := 1
			if tc.authPref == "oauth" && tc.wantKey {
				wantAttempts = 2
			}
			if len(seen) != wantAttempts {
				t.Fatalf("upstream attempts = %d, want %d", len(seen), wantAttempts)
			}
			for i, got := range seen {
				if got.cookie != "" || got.acctFilter != "" || got.auth == "Bearer gw-secret" {
					t.Errorf("attempt %d leaked gateway headers: cookie=%q acctFilter=%q auth=%q", i+1, got.cookie, got.acctFilter, got.auth)
				}
				rawVal, hasKey := got.body["prompt_cache_key"]
				if tc.wantKey {
					if !hasKey || rawVal != tc.cacheKey {
						t.Errorf("attempt %d prompt_cache_key = %#v (present=%t), want %q", i+1, rawVal, hasKey, tc.cacheKey)
					}
				} else if hasKey {
					t.Errorf("attempt %d unexpectedly emitted prompt_cache_key = %#v", i+1, rawVal)
				}
				if tc.wantHeaders {
					if got.sessionID != tc.sessionID || got.conversationID != tc.conversationID {
						t.Errorf("attempt %d session headers = (%q, %q), want (%q, %q)", i+1, got.sessionID, got.conversationID, tc.sessionID, tc.conversationID)
					}
				} else if got.sessionID != "" || got.conversationID != "" {
					t.Errorf("attempt %d unexpectedly forwarded session headers (%q, %q)", i+1, got.sessionID, got.conversationID)
				}
			}
		})
	}
}

func TestResponsesPromptCacheKeyAndSessionHeadersSurviveRewrite(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cacheKey       string
		sessionID      string
		conversationID string
		wantKey        bool
	}{
		{
			name:           "present key and headers",
			cacheKey:       "team/α:07",
			sessionID:      "sess-resp-1",
			conversationID: "conv-resp-2",
			wantKey:        true,
		},
		{
			name:    "absent key and headers",
			wantKey: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			store := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
			activeID := "oauth-1"
			ws := "ws-resp"
			if err := store.Save(&codex.AccountsStore{
				ActiveAccountID: &activeID,
				Accounts: []codex.StoredAccount{{
					ID:       activeID,
					Name:     "oauth-1",
					AuthMode: codex.AuthModeChatGPT,
					AuthData: codex.AuthData{AccessToken: "tok-resp", AccountID: &ws},
				}},
			}); err != nil {
				t.Fatal(err)
			}

			var gotBody map[string]any
			var gotSession, gotConversation string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSession = r.Header.Get("session_id")
				gotConversation = r.Header.Get("conversation_id")
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_ok","status":"completed"}`)
			}))
			defer upstream.Close()

			pool := NewPool(store, nil, nil, nil)
			srv := NewServer(pool, tmp, "gw-secret")
			srv.CodexResponsesURL = upstream.URL

			payload := map[string]any{
				"model": "gpt-6.2-astra",
				"input": "hello",
			}
			if tc.cacheKey != "" {
				payload["prompt_cache_key"] = tc.cacheKey
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer gw-secret")
			if tc.sessionID != "" {
				req.Header.Set("session_id", tc.sessionID)
			}
			if tc.conversationID != "" {
				req.Header.Set("conversation_id", tc.conversationID)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if gotBody["store"] != false || gotBody["instructions"] != defaultDirectInstructions {
				t.Fatalf("rewrite defaults missing: store=%#v instructions=%#v", gotBody["store"], gotBody["instructions"])
			}
			rawVal, hasKey := gotBody["prompt_cache_key"]
			if tc.wantKey {
				if !hasKey || rawVal != tc.cacheKey {
					t.Fatalf("prompt_cache_key = %#v (present=%t), want %q", rawVal, hasKey, tc.cacheKey)
				}
			} else if hasKey {
				t.Fatalf("unexpected prompt_cache_key = %#v", rawVal)
			}
			if gotSession != tc.sessionID || gotConversation != tc.conversationID {
				t.Fatalf("session headers = (%q, %q), want (%q, %q)", gotSession, gotConversation, tc.sessionID, tc.conversationID)
			}
		})
	}
}

func TestEnsureCodexTokenMultiplePoolsSerializeRefresh(t *testing.T) {
	dir := t.TempDir()
	store := codex.NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "live"))
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	expiredJWT := makeGatewayTestJWT(t, now.Add(-time.Hour))
	freshJWT := makeGatewayTestJWT(t, now.Add(time.Hour))

	account, _, err := store.UpsertTokens(codex.TokenData{
		IDToken:      expiredJWT,
		AccessToken:  expiredJWT,
		RefreshToken: "single-use-1",
	}, "OAuth seat", true, now)
	if err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refreshCalls.Add(1) != 1 || r.FormValue("refresh_token") != "single-use-1" {
			http.Error(w, "refresh token already redeemed", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token":      freshJWT,
			"access_token":  freshJWT,
			"refresh_token": "rotated-2",
		})
	}))
	defer oauth.Close()

	poolA := NewPool(store, nil, nil, nil)
	poolA.Now = func() time.Time { return now }
	poolA.CodexOAuth.Issuer = oauth.URL

	poolB := NewPool(store, nil, nil, nil)
	poolB.Now = func() time.Time { return now }
	poolB.CodexOAuth.Issuer = oauth.URL

	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		pool := poolA
		if i%2 == 1 {
			pool = poolB
		}
		candCopy := buildCodexCandidate(*account)
		wg.Add(1)
		go func(p *Pool, c CodexCandidate) {
			defer wg.Done()
			<-start
			if err := p.EnsureCodexToken(t.Context(), &c, false); err != nil {
				errs <- err
				return
			}
			if c.AccessToken != freshJWT || c.Account.AuthData.RefreshToken != "rotated-2" {
				errs <- fmt.Errorf("unexpected candidate credentials: access=%q refresh=%q", c.AccessToken, c.Account.AuthData.RefreshToken)
			}
		}(pool, candCopy)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("OAuth token exchanges = %d, want 1", calls)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Accounts[0].AuthData; got.AccessToken != freshJWT || got.RefreshToken != "rotated-2" {
		t.Fatalf("reloaded store credentials = (%q, %q), want (%q, rotated-2)", got.AccessToken, got.RefreshToken, freshJWT)
	}
}

func TestConcurrentChat401sAcrossPoolsShareForcedRefresh(t *testing.T) {
	dir := t.TempDir()
	store := codex.NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "live"))
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	oldJWT := makeGatewayTestJWT(t, now.Add(time.Hour))
	newJWT := makeGatewayTestJWT(t, now.Add(2*time.Hour))
	ws := "ws-401"

	_, _, err := store.UpsertTokens(codex.TokenData{
		IDToken:      oldJWT,
		AccessToken:  oldJWT,
		RefreshToken: "single-use-401",
		AccountID:    &ws,
	}, "Seat401", true, now)
	if err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refreshCalls.Add(1) != 1 || r.FormValue("refresh_token") != "single-use-401" {
			http.Error(w, "refresh token already redeemed", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token":      newJWT,
			"access_token":  newJWT,
			"refresh_token": "rotated-401",
		})
	}))
	defer oauth.Close()

	const workers = 8
	var oldAttempts, newAttempts atomic.Int32
	allOldArrived := make(chan struct{})
	releaseOld := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") != "sess-401-sticky" || r.Header.Get("conversation_id") != "conv-401-sticky" {
			t.Errorf("missing session headers on upstream request: session_id=%q conversation_id=%q",
				r.Header.Get("session_id"), r.Header.Get("conversation_id"))
		}
		switch r.Header.Get("Authorization") {
		case "Bearer " + oldJWT:
			if oldAttempts.Add(1) == workers {
				close(allOldArrived)
			}
			<-releaseOld
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"token revoked"}}`)
		case "Bearer " + newJWT:
			newAttempts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_401_ok"}}`)
			_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"recovered"}`)
			_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_401_ok","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`)
		default:
			t.Errorf("unexpected Authorization: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	makeSrv := func() *Server {
		p := NewPool(store, nil, nil, nil)
		p.Now = func() time.Time { return now }
		p.CodexOAuth.Issuer = oauth.URL
		s := NewServer(p, dir, "gw-key")
		s.CodexResponsesURL = upstream.URL
		return s
	}
	srvA := makeSrv()
	srvB := makeSrv()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		srv := srvA
		if i%2 == 1 {
			srv = srvB
		}
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.2-astra","stream":false,"prompt_cache_key":"k-401","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer gw-key")
			req.Header.Set("session_id", "sess-401-sticky")
			req.Header.Set("conversation_id", "conv-401-sticky")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		}(srv)
	}

	select {
	case <-allOldArrived:
		close(releaseOld)
	case <-ctx.Done():
		close(releaseOld)
		t.Fatal("timed out waiting for all old-bearer requests to reach upstream")
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := oldAttempts.Load(); got != workers {
		t.Fatalf("old-bearer attempts = %d, want %d", got, workers)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("OAuth token exchanges = %d, want 1", got)
	}
	if got := newAttempts.Load(); got != workers {
		t.Fatalf("new-bearer attempts = %d, want %d", got, workers)
	}
}

func TestGatewayRejectsOversizedBodies(t *testing.T) {
	chunk := strings.Repeat(" ", 1<<20)
	for _, tc := range []struct {
		path      string
		mebibytes int
	}{
		{"/v1/chat/completions", 32},
		{"/v1/responses", 32},
		{"/v1/files", 128},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var parts []io.Reader
			for range tc.mebibytes {
				parts = append(parts, strings.NewReader(chunk))
			}
			parts = append(parts, strings.NewReader("x"))
			req := httptest.NewRequest(http.MethodPost, tc.path, io.MultiReader(parts...))
			rec := httptest.NewRecorder()
			NewServer(nil, t.TempDir(), "").ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
		})
	}
}

func TestResponsesRejectsNull(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("null"))
	rec := httptest.NewRecorder()
	NewServer(nil, t.TempDir(), "").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestResponsesJSONPassthroughOutcome(t *testing.T) {
	completed := `{"status":"completed"}`
	atLimit := completed + strings.Repeat(" ", maxResponseCaptureBytes-len(completed))
	for _, tc := range []struct {
		name    string
		body    string
		healthy bool
	}{
		{"failed", `{"status":"failed"}`, false},
		{"incomplete", `{"status":"incomplete"}`, false},
		{"at capture limit", atLimit, true},
		{"beyond capture limit", atLimit + strings.Repeat(" ", 4096), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := platformTestServer(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()
			srv.CodexResponsesURL = up.URL
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer quotaswitch-local")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != tc.body {
				t.Fatalf("passthrough status = %d, forwarded %d of %d bytes", rec.Code, rec.Body.Len(), len(tc.body))
			}
			if got := srv.Pool.preferredCodex != ""; got != tc.healthy {
				t.Fatalf("marked healthy = %t, want %t", got, tc.healthy)
			}
		})
	}
}

func TestStreamFailureLogOmitsUpstreamMessage(t *testing.T) {
	failure := "data: " + `{"type":"error","message":"No tool output found for function call private-prompt-excerpt"}` + "\n\n"
	srv, _ := codexErrorTestServer(t, failure, "")
	var logs strings.Builder
	srv.Logf = func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	}
	rec := codexErrorTestRequest(srv, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(logs.String(), "status=400") || strings.Contains(logs.String(), "private-prompt-excerpt") {
		t.Fatal("expected stream failure with metadata-only logging")
	}
}
