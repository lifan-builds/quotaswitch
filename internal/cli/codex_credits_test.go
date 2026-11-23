// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func TestCodexCreditsCLIAndListIntegration(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	exp := testNow.Add(15 * 24 * time.Hour).UTC()
	key := "test-api-key-cli"
	email := "credits@example.com"

	if err := codexStore.Save(&codex.AccountsStore{
		Version: 1,
		Accounts: []codex.StoredAccount{
			{
				ID:                    "acct-cli-credit",
				Name:                  "api-credits-exp1130",
				Email:                 &email,
				SubscriptionExpiresAt: &exp,
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &key},
				CreatedAt:             testNow,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	usageClient := codex.NewUsageClient()
	usageClient.Now = func() time.Time { return testNow }

	app, outBuf, errBuf := env.app("test-admin-key-from-stdin\n")
	app.CodexStore = codexStore
	app.CodexUsage = usageClient

	if code := app.Run([]string{
		"codex", "credits",
		"--set", "api-credits-exp1130",
		"--grant", "120",
		"--used", "0.07",
		"--admin-key-stdin",
	}); code != 0 {
		t.Fatalf("codex credits --set exit %d: %s", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "$119.93 / $120.00 remaining") {
		t.Fatalf("unexpected calibration output: %s", outBuf.String())
	}

	// Verify table output of `quotaswitch codex credits`
	app2, out2, err2 := env.app("")
	app2.CodexStore = codexStore
	app2.CodexUsage = usageClient
	if code := app2.Run([]string{"codex", "credits"}); code != 0 {
		t.Fatalf("codex credits exit %d: %s", code, err2.String())
	}
	if !strings.Contains(out2.String(), "$119.93") || !strings.Contains(out2.String(), "Pool Total: $119.93 / $120.00") {
		t.Fatalf("unexpected codex credits table:\n%s", out2.String())
	}

	// Verify `quotaswitch list --provider=codex` displays Credits $119.93/$120 without any browser/network calls!
	app3, out3, err3 := env.app("")
	app3.CodexStore = codexStore
	offlineUsage := codex.NewUsageClient()
	offlineUsage.Now = func() time.Time { return testNow }
	offlineUsage.OpenAIBaseURL = "http://127.0.0.1:1"
	app3.CodexUsage = offlineUsage
	if code := app3.Run([]string{"list", "--provider=codex"}); code != 0 {
		t.Fatalf("list --provider=codex exit %d: %s", code, err3.String())
	}
	listOut := out3.String()
	if !strings.Contains(listOut, "Credits $119.93/$120") || !strings.Contains(listOut, "100%") {
		t.Fatalf("expected Credits $119.93/$120 and 100%% in list output:\n%s", listOut)
	}
}

func TestCodexRotateFallsBackToEarliestAPIKeyAndReturnsToOAuth(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))

	ws := "ws-oauth"
	oauthAcct, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "oauth@example.com", "sub-oauth", ws, "team"),
		AccessToken:  "tok-oauth",
		RefreshToken: "ref-oauth",
		AccountID:    &ws,
	}, "oauth-seat", true, testNow)
	if err != nil {
		t.Fatal(err)
	}

	expLater := testNow.Add(20 * 24 * time.Hour).UTC()
	expSoon := testNow.Add(10 * 24 * time.Hour).UTC()
	keyLater := "sk-later"
	keySoon := "sk-soon"
	data, err := codexStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	data.Accounts = append(data.Accounts,
		codex.StoredAccount{
			ID:                    "api-later",
			Name:                  "api-exp1210",
			SubscriptionExpiresAt: &expLater,
			AuthMode:              codex.AuthModeAPIKey,
			AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &keyLater},
			CreatedAt:             testNow,
		},
		codex.StoredAccount{
			ID:                    "api-soon",
			Name:                  "api-exp1129",
			SubscriptionExpiresAt: &expSoon,
			AuthMode:              codex.AuthModeAPIKey,
			AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &keySoon},
			CreatedAt:             testNow,
		},
	)
	if err := codexStore.Save(data); err != nil {
		t.Fatal(err)
	}

	var oauthUsed atomic.Int64
	oauthUsed.Store(100)
	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		used := float64(oauthUsed.Load())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         used,
					"limit_window_seconds": 18000,
					"reset_at":             testNow.Add(time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         20.0,
					"limit_window_seconds": 604800,
					"reset_at":             testNow.Add(24 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer whamSrv.Close()

	usageClient := codex.NewUsageClient()
	usageClient.UsageURL = whamSrv.URL
	usageClient.Now = func() time.Time { return testNow }
	idleOps := &codex.ProcessOps{
		Inspect:         func() codex.ProcessSummary { return codex.ProcessSummary{} },
		CaptureRecovery: func(string, []int, time.Time) []codex.RecoveryTarget { return nil },
		ResumeRecovery:  func(string, []codex.RecoveryTarget) []string { return nil },
	}

	for _, name := range []string{"api-exp1210", "api-exp1129"} {
		appCal, _, errCal := env.app("")
		appCal.CodexStore = codexStore
		appCal.CodexUsage = usageClient
		if code := appCal.Run([]string{"codex", "credits", "--set", name, "--grant", "40", "--used", "2"}); code != 0 {
			t.Fatalf("calibrate %s failed: %s", name, errCal.String())
		}
	}

	// 1. OAuth exhausted -> rotates to earliest-expiring API-key seat (api-exp1129).
	app1, out1, err1 := env.app("")
	app1.CodexStore = codexStore
	app1.CodexUsage = usageClient
	app1.CodexOps = idleOps
	if code := app1.Run([]string{"codex", "rotate"}); code != 0 {
		t.Fatalf("codex rotate to api_key failed (%d): %s", code, err1.String())
	}
	if !strings.Contains(out1.String(), "api-exp1129") {
		t.Fatalf("expected earliest-expiry api-exp1129, got: %s", out1.String())
	}
	liveAuth, err := codexStore.ReadLiveAuth()
	if err != nil || liveAuth.OpenAIAPIKey == nil || *liveAuth.OpenAIAPIKey != keySoon {
		t.Fatalf("expected live auth.json OPENAI_API_KEY=%q, got %+v (err=%v)", keySoon, liveAuth, err)
	}

	// 2. While OAuth stays exhausted, active API-key seat stays active without churning.
	app2, out2, err2 := env.app("")
	app2.CodexStore = codexStore
	app2.CodexUsage = usageClient
	app2.CodexOps = idleOps
	if code := app2.Run([]string{"codex", "rotate"}); code != 0 {
		t.Fatalf("codex rotate while on healthy api_key failed (%d): %s", code, err2.String())
	}
	if !strings.Contains(out2.String(), "still has credits") {
		t.Fatalf("expected stay-on-credits message, got: %s", out2.String())
	}

	// 3. Once OAuth 5h window resets, rotate automatically switches back to OAuth!
	oauthUsed.Store(0)
	app3, out3, err3 := env.app("")
	app3.CodexStore = codexStore
	app3.CodexUsage = usageClient
	app3.CodexOps = idleOps
	if code := app3.Run([]string{"codex", "rotate"}); code != 0 {
		t.Fatalf("codex rotate back to oauth failed (%d): %s", code, err3.String())
	}
	if !strings.Contains(out3.String(), oauthAcct.DisplayName()) {
		t.Fatalf("expected return to %s, got: %s", oauthAcct.DisplayName(), out3.String())
	}
}
