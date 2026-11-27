// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSameOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical origin", "https://auth.openai.com/oauth/token", "https://auth.openai.com/oauth/v2/token", true},
		{"explicit default https port", "https://auth.openai.com/oauth/token", "https://auth.openai.com:443/oauth/v2/token", true},
		{"case-insensitive host and scheme", "HTTPS://AUTH.OPENAI.COM/oauth/token", "https://auth.openai.com/v2", true},
		{"https to http downgrade", "https://auth.openai.com/oauth/token", "http://auth.openai.com/oauth/token", false},
		{"different host", "https://auth.openai.com/oauth/token", "https://evil.example/oauth/token", false},
		{"suffix host spoof", "https://auth.openai.com/oauth/token", "https://auth.openai.com.evil.example/oauth/token", false},
		{"different port", "http://127.0.0.1:41001/oauth/token", "http://127.0.0.1:41002/oauth/token", false},
		{"loopback alias host mismatch", "http://127.0.0.1:41001/oauth/token", "http://localhost:41001/oauth/token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ua, err := url.Parse(tc.a)
			if err != nil {
				t.Fatal(err)
			}
			ub, err := url.Parse(tc.b)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameOrigin(ua, ub); got != tc.want {
				t.Fatalf("sameOrigin(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestOAuthRefreshTokensRedirectPolicy(t *testing.T) {
	var attackerHits atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		t.Errorf("cross-origin redirect leaked credentials to %s (body=%q)", r.URL, r.FormValue("refresh_token"))
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		for _, clientMode := range []string{"default", "nil", "custom"} {
			t.Run(fmt.Sprintf("status_%d_%s", status, clientMode), func(t *testing.T) {
				var issuerHits atomic.Int32
				issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					issuerHits.Add(1)
					http.Redirect(w, r, attacker.URL+"/steal", status)
				}))
				defer issuer.Close()

				cfg := DefaultOAuthConfig()
				cfg.Issuer = issuer.URL
				switch clientMode {
				case "nil":
					cfg.HTTPClient = nil
				case "custom":
					cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
				}

				_, err := RefreshTokens(t.Context(), cfg, "secret-refresh-token")
				if !errors.Is(err, errCrossOriginRedirect) {
					t.Fatalf("expected errCrossOriginRedirect, got %v", err)
				}
				if got := issuerHits.Load(); got != 1 {
					t.Fatalf("issuer hits = %d, want 1 (must not retry cross-origin redirect)", got)
				}
				if got := attackerHits.Load(); got != 0 {
					t.Fatalf("attacker hits = %d, want 0", got)
				}
			})
		}
	}

	t.Run("allows same-origin redirect", func(t *testing.T) {
		issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/oauth/token" {
				http.Redirect(w, r, "/oauth/v2/token", http.StatusTemporaryRedirect)
				return
			}
			if r.URL.Path != "/oauth/v2/token" || r.FormValue("refresh_token") != "same-origin-refresh" {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token":  "fresh-access",
				"refresh_token": "fresh-refresh",
			})
		}))
		defer issuer.Close()

		cfg := DefaultOAuthConfig()
		cfg.Issuer = issuer.URL
		toks, err := RefreshTokens(t.Context(), cfg, "same-origin-refresh")
		if err != nil {
			t.Fatalf("same-origin redirect failed: %v", err)
		}
		if toks.AccessToken != "fresh-access" || toks.RefreshToken != "fresh-refresh" {
			t.Fatalf("unexpected tokens: %+v", toks)
		}
	})
}

func TestOAuthCodeExchangeRedirectPolicy(t *testing.T) {
	var attackerHits atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		t.Errorf("cross-origin code exchange redirect leaked code=%q verifier=%q", r.FormValue("code"), r.FormValue("code_verifier"))
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer issuer.Close()

	_, err := exchangeAuthorizationCode(t.Context(), &http.Client{}, issuer.URL, DefaultClientID, "http://localhost:1455/auth/callback", "pkce-verifier", "auth-code-123")
	if !errors.Is(err, errCrossOriginRedirect) {
		t.Fatalf("expected errCrossOriginRedirect, got %v", err)
	}
	if got := attackerHits.Load(); got != 0 {
		t.Fatalf("attacker hits = %d, want 0", got)
	}

	sameOriginIssuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			http.Redirect(w, r, "/oauth/v2/token", http.StatusTemporaryRedirect)
			return
		}
		if r.FormValue("code") != "auth-code-123" || r.FormValue("code_verifier") != "pkce-verifier" {
			http.Error(w, "missing exchange params", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "code-access",
			"refresh_token": "code-refresh",
		})
	}))
	defer sameOriginIssuer.Close()

	toks, err := exchangeAuthorizationCode(t.Context(), nil, sameOriginIssuer.URL, DefaultClientID, "http://localhost:1455/auth/callback", "pkce-verifier", "auth-code-123")
	if err != nil {
		t.Fatalf("same-origin code exchange failed: %v", err)
	}
	if toks.AccessToken != "code-access" || toks.RefreshToken != "code-refresh" {
		t.Fatalf("unexpected exchanged tokens: %+v", toks)
	}
}

const (
	refreshWorkerIssuerEnv  = "QUOTASWITCH_TEST_REFRESH_ISSUER"
	refreshWorkerAccountEnv = "QUOTASWITCH_TEST_REFRESH_ACCOUNT"
)

func TestStoreRefreshAccountConcurrentProcessesRedeemOnce(t *testing.T) {
	if dir := concurrentStoreWorkerDir(t); dir != "" {
		issuer := os.Getenv(refreshWorkerIssuerEnv)
		accountID := os.Getenv(refreshWorkerAccountEnv)
		store := NewStore(dir, filepath.Join(dir, "live"))
		cfg := DefaultOAuthConfig()
		cfg.Issuer = issuer
		acct, err := store.RefreshAccount(t.Context(), RefreshAccountOptions{
			AccountID:       accountID,
			OAuth:           cfg,
			Force:           false,
			Now:             time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC),
			PreviousAccess:  "expired-access",
			PreviousRefresh: "single-use-proc-refresh",
		})
		if err != nil {
			t.Fatalf("worker RefreshAccount failed: %v", err)
		}
		if acct.AuthData.RefreshToken != "rotated-proc-refresh" {
			t.Fatalf("worker got refresh token %q, want rotated-proc-refresh", acct.AuthData.RefreshToken)
		}
		return
	}

	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	expiredID := makeTestIDToken(t, "proc@example.com", "sub-proc", "ws-proc", "team", now.Add(-time.Hour))
	freshID := makeTestIDToken(t, "proc@example.com", "sub-proc", "ws-proc", "team", now.Add(time.Hour))
	ws := "ws-proc"

	account, _, err := store.UpsertTokens(TokenData{
		IDToken:      expiredID,
		AccessToken:  "expired-access",
		RefreshToken: "single-use-proc-refresh",
		AccountID:    &ws,
	}, "ProcSeat", true, now)
	if err != nil {
		t.Fatal(err)
	}

	var redemptions atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redemptions.Add(1) != 1 || r.FormValue("refresh_token") != "single-use-proc-refresh" {
			http.Error(w, "refresh token already redeemed", http.StatusUnauthorized)
			return
		}
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token":      freshID,
			"access_token":  freshID,
			"refresh_token": "rotated-proc-refresh",
		})
	}))
	defer oauth.Close()

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	const workers = 4
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	started := make(chan struct{}, workers)
	results := make(chan error, workers)
	for range workers {
		go func() {
			cmd := exec.CommandContext(ctx, binary, "-test.run=^"+t.Name()+"$")
			cmd.Env = append(cmd.Environ(),
				concurrentStoreWorkerEnv+"="+dir,
				refreshWorkerIssuerEnv+"="+oauth.URL,
				refreshWorkerAccountEnv+"="+account.ID,
			)
			cmd.Stdin = reader
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Start()
			started <- struct{}{}
			if err == nil {
				err = cmd.Wait()
			}
			if err != nil {
				err = fmt.Errorf("refresh worker: %w\n%s", err, out.Bytes())
			}
			results <- err
		}()
	}
	for range workers {
		<-started
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}

	if got := redemptions.Load(); got != 1 {
		t.Fatalf("OAuth redemptions across %d OS processes = %d, want 1", workers, got)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Accounts[0].AuthData; got.AccessToken != freshID || got.RefreshToken != "rotated-proc-refresh" {
		t.Fatalf("persisted tokens = (%q, %q), want fresh pair", got.AccessToken, got.RefreshToken)
	}
	live, err := store.ReadLiveAuth()
	if err != nil {
		t.Fatal(err)
	}
	if live.Tokens == nil || live.Tokens.RefreshToken != "rotated-proc-refresh" {
		t.Fatalf("live auth.json did not receive rotated refresh token: %+v", live.Tokens)
	}
}

func TestStoreRefreshAccountPreservesOtherAuthFields(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	expiredID := makeTestIDToken(t, "user@example.com", "sub-1", "ws-orig", "team", now.Add(-time.Hour))
	ws := "ws-orig"
	account, _, err := store.UpsertTokens(TokenData{
		IDToken:      expiredID,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		AccountID:    &ws,
	}, "Seat", false, now)
	if err != nil {
		t.Fatal(err)
	}

	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
	}))
	defer oauth.Close()

	cfg := DefaultOAuthConfig()
	cfg.Issuer = oauth.URL
	refreshed, err := store.RefreshAccount(t.Context(), RefreshAccountOptions{
		AccountID:       account.ID,
		OAuth:           cfg,
		Now:             now,
		PreviousAccess:  "old-access",
		PreviousRefresh: "old-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AuthData.IDToken != expiredID {
		t.Fatalf("IDToken overwritten when refresh omitted it: got %q", refreshed.AuthData.IDToken)
	}
	if refreshed.AuthData.AccountID == nil || *refreshed.AuthData.AccountID != "ws-orig" {
		t.Fatalf("AccountID lost during refresh: got %v", refreshed.AuthData.AccountID)
	}
	if refreshed.AuthData.Type != AuthModeChatGPT {
		t.Fatalf("AuthData.Type lost during refresh: got %q", refreshed.AuthData.Type)
	}
}
