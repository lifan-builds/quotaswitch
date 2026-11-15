package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = w.Write([]byte(`{"object":"list","model":"text-embedding-3-large","data":[],"usage":{"prompt_tokens":100000,"total_tokens":100000}}`))
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
	srv := NewServer(pool, filepath.Join(dir, "codex-home"), "")
	srv.CodexResponsesURL = upstream.URL + "/v1/responses"
	srv.OpenAIBaseURL = upstream.URL
	srv.HTTPClient = upstream.Client()

	// 1. Chat completion meters tokens onto api-early-exp1129.
	chatReq := `{"model":"gpt-5.4","messages":[{"role":"user","content":"ping"}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatReq)))
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
		pRec := httptest.NewRecorder()
		srv.ServeHTTP(pRec, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
		if pRec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", tc.path, pRec.Code, pRec.Body.String())
		}
	}

	// 3. Now simulate upstream insufficient_quota on api-early-exp1129 -> fails over to api-later-exp1130 and marks early exhausted!
	earlyExhausted = true
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatReq)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("failover chat status = %d: %s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Quotaswitch-Account-ID"); got != "api-later-exp1130" {
		t.Fatalf("failover account = %q, want api-later-exp1130", got)
	}

	// 4. Check GET /v1/credits zero-browser JSON output!
	credRec := httptest.NewRecorder()
	srv.ServeHTTP(credRec, httptest.NewRequest(http.MethodGet, "/v1/credits", nil))
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
