// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func TestGatewayLiveCreditMeteringAndExhaustionFailover(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)
	expEarly := time.Date(2026, 11, 29, 23, 59, 59, 0, time.UTC)
	expLater := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)

	codexStore := codex.NewStore(filepath.Join(dir, "codex-switcher"), filepath.Join(dir, "codex-home"))
	kEarly := "test-api-key-early"
	kLater := "test-api-key-later"
	acctEarly := codex.StoredAccount{
		ID:                    "id-early",
		Name:                  "api-early-exp1129",
		SubscriptionExpiresAt: &expEarly,
		AuthMode:              codex.AuthModeAPIKey,
		AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &kEarly},
		CreatedAt:             now,
	}
	acctLater := codex.StoredAccount{
		ID:                    "id-later",
		Name:                  "api-later-exp1130",
		SubscriptionExpiresAt: &expLater,
		AuthMode:              codex.AuthModeAPIKey,
		AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &kLater},
		CreatedAt:             now,
	}
	if err := codexStore.Save(&codex.AccountsStore{
		Version:  1,
		Accounts: []codex.StoredAccount{acctEarly, acctLater},
	}); err != nil {
		t.Fatal(err)
	}
	gEarly, gLater := 40.0, 120.0
	uZero := 0.0
	if _, err := codexStore.CalibrateCredit(acctEarly, codex.CreditCalibration{GrantUSD: &gEarly, UsedUSD: &uZero, ResetMetered: true}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := codexStore.CalibrateCredit(acctLater, codex.CreditCalibration{GrantUSD: &gLater, UsedUSD: &uZero, ResetMetered: true}, now); err != nil {
		t.Fatal(err)
	}

	earlyExhausted := false
	embedding := strings.Repeat("AAAA", 1+2*1024*1024/4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("gateway X-API-Key reached the upstream")
		}
		auth := r.Header.Get("Authorization")
		if auth == "Bearer test-api-key-early" && earlyExhausted {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"type":"response.created","response":{"id":"resp_1"}}`)
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"ok"}`)
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":200000,"input_tokens_details":{"cached_tokens":100000},"output_tokens":50000,"total_tokens":250000}}}`)
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprintln(w, `data: [DONE]`)
			_, _ = fmt.Fprintln(w)
		case "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"object":"list","model":"text-embedding-3-large","data":[{"object":"embedding","index":0,"embedding":%q}],"usage":{"prompt_tokens":100000,"total_tokens":100000}}`, embedding)
		case "/v1/images/generations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`))
		case "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("MP3DATA"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	pool := NewPool(codexStore, nil, nil, nil)
	pool.Now = func() time.Time { return now }
	srv := NewServer(pool, filepath.Join(dir, "codex-home"), "gateway-test-key")
	srv.CodexResponsesURL = upstream.URL + "/v1/responses"
	srv.OpenAIBaseURL = upstream.URL
	srv.HTTPClient = upstream.Client()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-API-Key", "gateway-test-key")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// 1. Chat completion meters tokens onto api-early-exp1129.
	chatReq := `{"model":"gpt-5.4","messages":[{"role":"user","content":"ping"}]}`
	rec := request(http.MethodPost, "/v1/chat/completions", chatReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Embeddings + Image + TTS also meter onto api-early-exp1129.
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/v1/embeddings", `{"model":"text-embedding-3-large","input":"hello"}`},
		{"/v1/images/generations", `{"model":"gpt-image-1","size":"1536x1024","quality":"high","n":2}`},
		{"/v1/audio/speech", `{"model":"gpt-4o-mini-tts","input":"Hello world!"}`},
	} {
		pRec := request(http.MethodPost, tc.path, tc.body)
		if pRec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", tc.path, pRec.Code, pRec.Body.String())
		}
	}

	// 3. Now simulate upstream insufficient_quota on api-early-exp1129 -> fails over to api-later-exp1130 and marks early exhausted!
	earlyExhausted = true
	rec2 := request(http.MethodPost, "/v1/chat/completions", chatReq)
	if rec2.Code != http.StatusOK {
		t.Fatalf("failover chat status = %d: %s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Quotaswitch-Account-ID"); got != "api-later-exp1130" {
		t.Fatalf("failover account = %q, want api-later-exp1130", got)
	}

	// 4. Check GET /v1/credits zero-browser JSON output!
	credRec := request(http.MethodGet, "/v1/credits", "")
	if credRec.Code != http.StatusOK {
		t.Fatalf("/v1/credits status = %d: %s", credRec.Code, credRec.Body.String())
	}
	var report CreditsReport
	if err := json.Unmarshal(credRec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode /v1/credits: %v", err)
	}
	if len(report.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2", len(report.Accounts))
	}
	if !report.Accounts[0].Exhausted || report.Accounts[0].RemainingUSD != 0 {
		t.Fatalf("expected early account exhausted with $0 remaining, got %+v", report.Accounts[0])
	}
	if report.Accounts[0].Requests != 4 || report.Accounts[0].Images != 2 || report.Accounts[0].EmbeddingTokens != 100000 {
		t.Fatalf("unexpected early counters: %+v", report.Accounts[0])
	}
	if report.Accounts[1].Requests != 1 || report.Accounts[1].InputTokens != 200000 || report.Accounts[1].CachedTokens != 100000 {
		t.Fatalf("unexpected later counters: %+v", report.Accounts[1])
	}
	if report.ActivePoolAccounts != 1 {
		t.Fatalf("ActivePoolAccounts = %d, want 1", report.ActivePoolAccounts)
	}
}

func TestGatewayRequestAuthentication(t *testing.T) {
	srv := NewServer(NewPool(nil, nil, nil, nil), t.TempDir(), "gateway-test-key")
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/models"},
		{http.MethodGet, "/v1/credits"},
		{http.MethodGet, "/credits"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/chat/completions"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodPost, "/responses"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodGet, "/unknown"},
		{http.MethodHead, "/health"},
		{http.MethodPost, "/"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(route.method, route.path, nil))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`) {
			t.Errorf("%s %s unauthenticated status = %d: %s", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/", "/health", "/healthz"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
	}
	for _, tc := range []struct {
		name   string
		bearer string
		key    string
		status int
	}{
		{"Bearer", "bEaReR gateway-test-key", "", http.StatusOK},
		{"API key", "Bearer wrong-key", "gateway-test-key", http.StatusOK},
		{"wrong key", "Bearer gateway-test-keY", "wrong-key", http.StatusUnauthorized},
		{"wrong scheme", "Basic gateway-test-key", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", tc.bearer)
			req.Header.Set("X-API-Key", tc.key)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
	srv.APIKey = " \t"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models with blank server key status = %d, want 200", rec.Code)
	}
}

func TestGatewayUpstreamHeaderTimeout(t *testing.T) {
	client := NewServer(nil, t.TempDir(), "").HTTPClient
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport || transport.ResponseHeaderTimeout != 10*time.Minute || client.Timeout != 0 {
		t.Fatalf("client = %+v, want a cloned transport with a 10 minute header timeout and zero client timeout", client)
	}
}

func TestResponsesPassthroughStreamOutcome(t *testing.T) {
	largeDelta := `data: {"type":"response.output_text.delta","delta":"credit_balance_exhausted ` + strings.Repeat("x", 4*1024*1024) + `"}` + "\n\n"
	for _, tc := range []struct {
		name      string
		body      string
		completed bool
		exhausted bool
		cooldown  bool
	}{
		{"usage after large event", largeDelta + `data: {"type":"response.completed","response":{"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":8},"output_tokens":3}}}` + "\n\ndata: [DONE]\n\n", true, false, false},
		{"exhausted after large event", largeDelta + `data: {"type":"error","code":"credit_balance_exhausted"}` + "\n\n", false, true, false},
		{"failed response quota", `data: {"type":"response.failed","response":{"error":{"code":"insufficient_quota"}}}` + "\n\n", false, true, false},
		{"named rate limit event", "event: error\ndata: {\"code\":\"rate_limit_exceeded\"}\n\n", false, false, true},
		{"rate limit message", `data: {"type":"error","message":"rate_limit_exceeded"}` + "\n\n", false, false, true},
		{"other failure", `data: {"type":"response.failed","response":{"error":{"code":"server_error"}}}` + "\n\n", false, false, false},
		{"missing completion", `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\ndata: [DONE]\n\n", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)
			store := codex.NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
			key := "test-upstream-key"
			acct := codex.StoredAccount{
				ID:        "stream-account",
				Name:      "stream-account",
				AuthMode:  codex.AuthModeAPIKey,
				AuthData:  codex.AuthData{Type: codex.AuthModeAPIKey, Key: &key},
				CreatedAt: now,
			}
			if err := store.Save(&codex.AccountsStore{Accounts: []codex.StoredAccount{acct}}); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			pool := NewPool(store, nil, nil, nil)
			pool.Now = func() time.Time { return now }
			srv := NewServer(pool, filepath.Join(dir, "codex"), "")
			srv.CodexResponsesURL = upstream.URL

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true}`))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte(tc.body)) || !rec.Flushed {
				t.Fatalf("passthrough status = %d, body length = %d, flushed = %t", rec.Code, rec.Body.Len(), rec.Flushed)
			}
			if got := pool.preferredCodex == acct.ID; got != tc.completed {
				t.Errorf("marked healthy = %t, want %t", got, tc.completed)
			}
			if got := pool.isCoolingDown(BackendCodex, acct.ID, now); got != tc.cooldown {
				t.Errorf("cooling down = %t, want %t", got, tc.cooldown)
			}
			ledger, err := store.LoadCreditLedger()
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := ledger.Lookup(acct)
			if !ok || entry.Exhausted != tc.exhausted {
				t.Fatalf("ledger entry exists = %t, exhausted = %t, want exhausted = %t", ok, entry.Exhausted, tc.exhausted)
			}
			if tc.completed && (entry.InputTokens != 12 || entry.CachedTokens != 8 || entry.OutputTokens != 3 || entry.MeteredUSD <= 0) {
				t.Fatalf("large response usage was not metered: %+v", entry)
			}
		})
	}
}
