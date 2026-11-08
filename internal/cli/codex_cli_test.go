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

	"github.com/fikrilal/agy-auth/internal/agy"
	"github.com/fikrilal/agy-auth/internal/codex"
	"github.com/fikrilal/agy-auth/internal/usage"
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

	// Verify `agy-auth list --provider=all` lists both sections.
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
