package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

type staticFullUsageProvider struct {
	full usage.Full
}

func (s staticFullUsageProvider) FetchFull(_ context.Context, _ string, _ *agy.TokenFile) (usage.Full, error) {
	return s.full, nil
}

func makeCodexIDToken(t *testing.T, email, sub, ws, plan string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"email": email,
		"sub":   sub,
		"exp":   testNow.Add(2 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": ws,
			"chatgpt_plan_type":  plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestCodexRotateAndMultiProviderRotate(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))

	ws1 := "ws-1"
	ws2 := "ws-2"
	_, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "c1@example.com", "sub-1", ws1, "team"),
		AccessToken:  "tok-exhausted",
		RefreshToken: "ref-1",
		AccountID:    &ws1,
	}, "CodexOne", true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "c2@example.com", "sub-2", ws2, "team"),
		AccessToken:  "tok-healthy",
		RefreshToken: "ref-2",
		AccountID:    &ws2,
	}, "CodexTwo", false, testNow)
	if err != nil {
		t.Fatal(err)
	}

	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used := 15.0
		if r.Header.Get("Authorization") == "Bearer tok-exhausted" {
			used = 100.0
		}
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

	resumedCalled := false
	nopOps := &codex.ProcessOps{
		Inspect: func() codex.ProcessSummary {
			return codex.ProcessSummary{DesktopPIDs: []int{4242}, DesktopBundles: []string{"/Applications/Codex.app"}}
		},
		QuitDesktop:       func([]string, []int) error { return nil },
		StopCLI:           func([]int) error { return nil },
		RestartAppServers: func([]int) int { return 0 },
		ReopenDesktop:     func([]string) error { return nil },
		CaptureRecovery: func(string, []int, time.Time) []codex.RecoveryTarget {
			return []codex.RecoveryTarget{{SessionID: "thr-99", Path: "/tmp/x.jsonl", Size: 128}}
		},
		ResumeRecovery: func(_ string, targets []codex.RecoveryTarget) []string {
			resumedCalled = true
			return []string{targets[0].SessionID}
		},
	}

	codexUsage := codex.NewUsageClient()
	codexUsage.UsageURL = whamSrv.URL
	codexUsage.Now = func() time.Time { return testNow }

	// Seed Antigravity store so `--provider=all` exercises both providers cleanly.
	appAdd, _, _ := env.app("")
	if code := appAdd.Run([]string{"add", "--alias", "agy-one"}); code != 0 {
		t.Fatalf("agy add failed: %d", code)
	}

	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.CodexUsage = codexUsage
	app.CodexOps = nopOps
	app.UsageFull = staticFullUsageProvider{
		full: usage.Full{
			Snapshot: usage.Snapshot{
				Plan:      "Pro",
				FiveHour:  &usage.Window{RemainingPercent: 80, Group: "Claude and GPT models"},
				UpdatedAt: testNow,
			},
			Groups: makePriorityGroups(80, 100, 100),
		},
	}

	if code := app.Run([]string{"rotate", "--provider=all", "--relaunch", "--no-auto-continue=false"}); code != 0 {
		t.Fatalf("rotate --provider=all failed: code=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "Rotated Codex to CodexTwo") {
		t.Fatalf("expected Codex rotation output, got %q", out.String())
	}
	if !resumedCalled {
		t.Fatal("expected Codex session auto-continue to be called")
	}

	loaded, err := codexStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveAccountID == nil || !strings.Contains(out.String(), "c2@example.com") {
		t.Fatalf("unexpected post-rotate state: active=%v out=%s", loaded.ActiveAccountID, out.String())
	}

	// Verify `quotaswitch list --provider=all` lists both sections.
	appList, listOut, listErr := env.app("")
	appList.CodexStore = codexStore
	appList.CodexUsage = codexUsage
	appList.CodexOps = nopOps
	if code := appList.Run([]string{"list", "--provider=all", "--no-usage"}); code != 0 {
		t.Fatalf("list --provider=all failed: %d (%s)", code, listErr.String())
	}
	if !strings.Contains(listOut.String(), "Antigravity (agy)") || !strings.Contains(listOut.String(), "OpenAI Codex (codex)") {
		t.Fatalf("expected both provider headers in list --provider=all: %s", listOut.String())
	}
}

func TestCodexSplitBrainSyncAndResume(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))

	ws := "ws-healthy"
	_, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "healthy@example.com", "sub-1", ws, "team"),
		AccessToken:  "tok-healthy",
		RefreshToken: "ref-healthy",
		AccountID:    &ws,
	}, "Glf22", true, testNow)
	if err != nil {
		t.Fatal(err)
	}

	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         0.0,
					"limit_window_seconds": 18000,
					"reset_at":             testNow.Add(time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         0.0,
					"limit_window_seconds": 604800,
					"reset_at":             testNow.Add(24 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer whamSrv.Close()

	quitCalled := false
	reopenCalled := false
	resumedCalled := false
	ops := &codex.ProcessOps{
		Inspect: func() codex.ProcessSummary {
			return codex.ProcessSummary{
				DesktopPIDs:       []int{1484},
				DesktopHelperPIDs: []int{2033},
				DesktopBundles:    []string{"/Applications/ChatGPT.app"},
			}
		},
		QuitDesktop: func(_ []string, pids []int) error {
			quitCalled = len(pids) == 2
			return nil
		},
		StopCLI:           func([]int) error { return nil },
		RestartAppServers: func([]int) int { return 0 },
		ReopenDesktop: func(bundles []string) error {
			reopenCalled = len(bundles) == 1 && bundles[0] == "/Applications/ChatGPT.app"
			return nil
		},
		CaptureRecovery: func(string, []int, time.Time) []codex.RecoveryTarget {
			return []codex.RecoveryTarget{{SessionID: "thr-split", Path: "/tmp/split.jsonl", Size: 512, QuotaStopped: true}}
		},
		ResumeRecovery: func(_ string, targets []codex.RecoveryTarget) []string {
			resumedCalled = len(targets) == 1 && targets[0].SessionID == "thr-split"
			return []string{"thr-split"}
		},
	}

	codexUsage := codex.NewUsageClient()
	codexUsage.UsageURL = whamSrv.URL
	codexUsage.Now = func() time.Time { return testNow }

	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.CodexUsage = codexUsage
	app.CodexOps = ops

	if code := app.Run([]string{"codex", "rotate", "--relaunch"}); code != 0 {
		t.Fatalf("codex rotate --relaunch failed: code=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}
	if !quitCalled || !reopenCalled || !resumedCalled {
		t.Fatalf("expected desktop quit+reopen+resume on split-brain, got quit=%v reopen=%v resume=%v", quitCalled, reopenCalled, resumedCalled)
	}
	if !strings.Contains(out.String(), "Synced running Codex session to active account Glf22") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func makeCodexIDTokenAt(t *testing.T, email, sub, ws, plan string, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"email": email,
		"sub":   sub,
		"exp":   exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": ws,
			"chatgpt_plan_type":  plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestCodexRotateSkipsInvalidRefreshAndPreservesSingleUseToken(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))

	wsActive := "ws-active"
	wsStale := "ws-stale"
	wsGood := "ws-good"

	// Candidate B: expired id_token + revoked refresh_token, even though access_token would return 100% usage.
	_, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDTokenAt(t, "stale@example.com", "sub-stale", wsStale, "team", testNow.Add(-time.Hour)),
		AccessToken:  makeCodexIDTokenAt(t, "stale@example.com", "sub-stale", wsStale, "team", testNow.Add(24*time.Hour)),
		RefreshToken: "ref-revoked",
		AccountID:    &wsStale,
	}, "stale-acct", false, testNow)
	if err != nil {
		t.Fatal(err)
	}

	// Candidate C: expired id_token + single-use refresh_token ("ref-once-1" -> "ref-once-2").
	goodAcct, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDTokenAt(t, "good@example.com", "sub-good", wsGood, "team", testNow.Add(-time.Hour)),
		AccessToken:  makeCodexIDTokenAt(t, "good@example.com", "sub-good", wsGood, "team", testNow.Add(24*time.Hour)),
		RefreshToken: "ref-once-1",
		AccountID:    &wsGood,
	}, "good-acct", false, testNow)
	if err != nil {
		t.Fatal(err)
	}

	// Active account: exhausted (0%).
	_, _, err = codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "active@example.com", "sub-active", wsActive, "team"),
		AccessToken:  "tok-active",
		RefreshToken: "ref-active",
		AccountID:    &wsActive,
	}, "active-acct", true, testNow)
	if err != nil {
		t.Fatal(err)
	}

	usedSingleUse := false
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		refreshTok := r.FormValue("refresh_token")
		if refreshTok == "ref-once-1" && !usedSingleUse {
			usedSingleUse = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":      makeCodexIDTokenAt(t, "good@example.com", "sub-good", wsGood, "team", testNow.Add(2*time.Hour)),
				"access_token":  makeCodexIDTokenAt(t, "good@example.com", "sub-good", wsGood, "team", testNow.Add(48*time.Hour)),
				"refresh_token": "ref-once-2",
			})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer oauthSrv.Close()

	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedPct := 10.0
		if r.Header.Get("ChatGPT-Account-Id") == wsActive {
			usedPct = 100.0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         usedPct,
					"limit_window_seconds": 18000,
					"reset_at":             testNow.Add(time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         10.0,
					"limit_window_seconds": 604800,
					"reset_at":             testNow.Add(24 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer whamSrv.Close()

	codexUsage := codex.NewUsageClient()
	codexUsage.UsageURL = whamSrv.URL
	codexUsage.OAuth.Issuer = oauthSrv.URL
	codexUsage.Now = func() time.Time { return testNow }

	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.CodexUsage = codexUsage
	app.CodexOps = &codex.ProcessOps{
		Inspect:           func() codex.ProcessSummary { return codex.ProcessSummary{} },
		QuitDesktop:       func([]string, []int) error { return nil },
		StopCLI:           func([]int) error { return nil },
		RestartAppServers: func([]int) int { return 0 },
		ReopenDesktop:     func([]string) error { return nil },
		CaptureRecovery:   func(string, []int, time.Time) []codex.RecoveryTarget { return nil },
		ResumeRecovery:    func(string, []codex.RecoveryTarget) []string { return nil },
	}

	if code := app.Run([]string{"codex", "rotate", "--relaunch"}); code != 0 {
		t.Fatalf("codex rotate failed: code=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}

	loaded, err := codexStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != goodAcct.ID {
		t.Fatalf("expected active account %s, got %v", goodAcct.ID, loaded.ActiveAccountID)
	}
	for _, a := range loaded.Accounts {
		if a.ID == goodAcct.ID && a.AuthData.RefreshToken != "ref-once-2" {
			t.Fatalf("expected rotated single-use refresh token ref-once-2 preserved, got %q", a.AuthData.RefreshToken)
		}
	}
}

func TestCodexListShowsManualResetCredits(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))

	ws := "ws-resets"
	_, _, err := codexStore.UpsertTokens(codex.TokenData{
		IDToken:      makeCodexIDToken(t, "resets@example.com", "sub-r", ws, "team"),
		AccessToken:  "tok-resets",
		RefreshToken: "ref-resets",
		AccountID:    &ws,
	}, "ResetSeat", true, testNow)
	if err != nil {
		t.Fatal(err)
	}

	whamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         20.0,
					"limit_window_seconds": 18000,
					"reset_at":             testNow.Add(time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         50.0,
					"limit_window_seconds": 604800,
					"reset_at":             testNow.Add(24 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer whamSrv.Close()

	resetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available_count": 3,
			"credits": []map[string]any{
				{
					"id":         "RateLimitResetCredit_a",
					"status":     "available",
					"expires_at": "2026-11-30T20:06:00Z",
					"title":      "Full reset (Weekly + 5 hr)",
				},
			},
		})
	}))
	defer resetSrv.Close()

	codexUsage := codex.NewUsageClient()
	codexUsage.UsageURL = whamSrv.URL
	codexUsage.ResetCreditsURL = resetSrv.URL
	codexUsage.Now = func() time.Time { return testNow }

	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.CodexUsage = codexUsage
	if code := app.Run([]string{"codex", "list"}); code != 0 {
		t.Fatalf("codex list failed: code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "RESETS") || !strings.Contains(out.String(), "3 (11-30)") {
		t.Fatalf("expected RESETS column with '3 (11-30)', got:\n%s", out.String())
	}

	appJSON, jsonOut, jsonErr := env.app("")
	appJSON.CodexStore = codexStore
	appJSON.CodexUsage = codexUsage
	if code := appJSON.Run([]string{"--json", "codex", "list"}); code != 0 {
		t.Fatalf("codex list --json failed: code=%d stderr=%s", code, jsonErr.String())
	}
	if !strings.Contains(jsonOut.String(), `"available_count": 3`) {
		t.Fatalf("expected available_count in JSON output, got:\n%s", jsonOut.String())
	}
}
