// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/backup"
)

func makeTestIDToken(t *testing.T, email, sub, acctID, plan string, exp time.Time) string {
	t.Helper()
	claims := map[string]any{
		"email": email,
		"sub":   sub,
		"exp":   exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": acctID,
			"chatgpt_plan_type":  plan,
		},
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestStoreSyncAndResolve(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)

	id1 := makeTestIDToken(t, "one@example.com", "sub-1", "ws-1", "team", now.Add(time.Hour))
	ws1 := "ws-1"
	acct1, added, err := store.UpsertTokens(TokenData{
		IDToken:      id1,
		AccessToken:  "acc-1",
		RefreshToken: "ref-1",
		AccountID:    &ws1,
	}, "Work1", true, now)
	if err != nil || !added {
		t.Fatalf("UpsertTokens 1: added=%v err=%v", added, err)
	}

	id2 := makeTestIDToken(t, "one@example.com", "sub-1", "ws-2", "team", now.Add(time.Hour))
	ws2 := "ws-2"
	acct2, added, err := store.UpsertTokens(TokenData{
		IDToken:      id2,
		AccessToken:  "acc-2",
		RefreshToken: "ref-2",
		AccountID:    &ws2,
	}, "Work2", false, now.Add(time.Minute))
	if err != nil || !added {
		t.Fatalf("UpsertTokens 2 (distinct workspace same email): added=%v err=%v", added, err)
	}

	// Simulate Codex rotating single-use refresh token in ~/.codex/auth.json for ws-1.
	rotated := AuthDotJson{
		Tokens: &TokenData{
			IDToken:      id1,
			AccessToken:  "acc-1-rotated",
			RefreshToken: "ref-1-rotated",
			AccountID:    &ws1,
		},
	}
	rawAuth, _ := json.Marshal(rotated)
	if err := backup.WriteFileAtomic(store.LiveAuthPath(), rawAuth, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.SyncActiveFromLiveAuth(loaded, now.Add(2*time.Minute))
	if err != nil || !changed {
		t.Fatalf("SyncActiveFromLiveAuth changed=%v err=%v", changed, err)
	}
	if loaded.Accounts[0].AuthData.RefreshToken != "ref-1-rotated" {
		t.Fatalf("expected rotated refresh token, got %q", loaded.Accounts[0].AuthData.RefreshToken)
	}

	// Resolve by alias, workspace ID/label, index, and '-'
	if got, err := Resolve(loaded, "Work2"); err != nil || got.ID != acct2.ID {
		t.Fatalf("Resolve Work2: got=%v err=%v", got, err)
	}
	if got, err := Resolve(loaded, "ws-2"); err != nil || got.ID != acct2.ID {
		t.Fatalf("Resolve ws-2: got=%v err=%v", got, err)
	}
	if got, err := Resolve(loaded, acct2.WorkspaceLabel()); err != nil || got.ID != acct2.ID {
		t.Fatalf("Resolve WorkspaceLabel (%s): got=%v err=%v", acct2.WorkspaceLabel(), got, err)
	}
	if got, err := Resolve(loaded, "1"); err != nil || got.ID != acct1.ID {
		t.Fatalf("Resolve 1: got=%v err=%v", got, err)
	}
	if got, err := Resolve(loaded, "-"); err != nil || got.ID != acct2.ID {
		t.Fatalf("Resolve -: got=%v err=%v", got, err)
	}
}

func TestStoreKeepsUnknownUsersInSharedWorkspaceSeparate(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	now := time.Now().UTC()
	first := TokenData{
		IDToken:      makeTestIDToken(t, "first@example.com", "first", "shared", "team", now.Add(time.Hour)),
		AccessToken:  "first-access",
		RefreshToken: "first-refresh",
	}
	account, _, err := store.UpsertTokens(first, "First", true, now)
	if err != nil {
		t.Fatal(err)
	}
	second := TokenData{
		IDToken:      makeTestIDToken(t, "", "", "shared", "team", now.Add(2*time.Hour)),
		AccessToken:  "second-access",
		RefreshToken: "second-refresh",
	}
	signedInAt := now.Add(time.Minute)
	raw, err := json.Marshal(AuthDotJson{Tokens: &second, LastRefresh: &signedInAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteFileAtomic(store.LiveAuthPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.SyncActiveFromLiveAuth(data, signedInAt); err != nil || changed {
		t.Fatalf("sync imported an unknown user: changed=%t, err=%v", changed, err)
	}
	addedAccount, added, err := store.AddFromLiveAuth("Second", signedInAt)
	if err != nil || !added || addedAccount.ID == account.ID {
		t.Fatalf("import did not create a distinct seat: added=%t, err=%v", added, err)
	}
	data, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Accounts) != 2 || data.Accounts[0].AuthData.RefreshToken != first.RefreshToken {
		t.Fatal("import changed the first seat's credentials")
	}
}

func TestAddFromLiveAuthMissingRefreshPreservesAccount(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	now := time.Now().UTC()
	tokens := TokenData{
		IDToken:      makeTestIDToken(t, "user@example.com", "sub", "workspace", "team", now.Add(time.Hour)),
		AccessToken:  "stored-access",
		RefreshToken: "stored-refresh",
	}
	if _, _, err := store.UpsertTokens(tokens, "Stored", true, now); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	tokens.AccessToken, tokens.RefreshToken = "live-access", ""
	raw, err := json.Marshal(AuthDotJson{Tokens: &tokens})
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteFileAtomic(store.LiveAuthPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddFromLiveAuth("New alias", now.Add(time.Minute)); !errors.Is(err, ErrMissingTokens) {
		t.Fatalf("expected missing tokens error, got %v", err)
	}
	after, err := os.ReadFile(store.AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("incomplete live auth modified the saved account")
	}
}

func TestSyncActiveTokenFreshness(t *testing.T) {
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                string
		accessJWT           bool
		expiry, lastRefresh time.Duration
		wantImport          bool
	}{
		{"older access JWT", true, -time.Minute, time.Minute, false},
		{"older ID JWT", false, -time.Minute, time.Minute, false},
		{"equal timestamps with different refresh tokens", true, 0, 0, false},
		{"older refresh with equal expiry", true, 0, -time.Minute, false},
		{"older refresh with longer lifetime", true, time.Minute, -time.Minute, false},
		{"newer refresh with equal expiry", true, 0, time.Minute, true},
		{"newer access JWT", true, time.Minute, time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore(filepath.Join(dir, "accounts"), filepath.Join(dir, "live"))
			expiry := now.Add(time.Hour)
			jwt := func(exp time.Time) string {
				return makeTestIDToken(t, "user@example.com", "subject", "workspace", "team", exp)
			}
			stored := TokenData{AccessToken: "stored", IDToken: jwt(expiry), RefreshToken: "stored"}
			live := TokenData{AccessToken: "live", IDToken: jwt(expiry.Add(tc.expiry)), RefreshToken: "live"}
			if tc.accessJWT {
				stored.AccessToken, live.AccessToken = stored.IDToken, live.IDToken
				live.IDToken = stored.IDToken
			}
			if _, _, err := store.UpsertTokens(stored, "", true, now); err != nil {
				t.Fatal(err)
			}
			refreshed := now.Add(tc.lastRefresh)
			raw, err := json.Marshal(AuthDotJson{Tokens: &live, LastRefresh: &refreshed})
			if err != nil {
				t.Fatal(err)
			}
			if err := backup.WriteFileAtomic(store.LiveAuthPath(), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			changed, err := store.SyncActiveFromLiveAuth(data, now.Add(time.Hour))
			if err != nil || changed != tc.wantImport {
				t.Fatalf("sync changed=%t, err=%v", changed, err)
			}
			saved, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := stored
			if tc.wantImport {
				want = live
			}
			for _, account := range []StoredAccount{data.Accounts[0], saved.Accounts[0]} {
				got := account.AuthData
				if got.RefreshToken != want.RefreshToken || got.AccessToken != want.AccessToken || got.IDToken != want.IDToken {
					t.Fatal("incorrect credential generation after sync")
				}
			}
		})
	}
}

func TestConcurrentAccountUpserts(t *testing.T) {
	const perWorker = 8
	if dir := concurrentStoreWorkerDir(t); dir != "" {
		store := NewStore(dir, filepath.Join(dir, "live"))
		for i := 0; i < perWorker; i++ {
			workspace := fmt.Sprintf("worker-%d-%d", os.Getpid(), i)
			_, added, err := store.UpsertTokens(TokenData{AccessToken: "access", RefreshToken: "refresh", AccountID: &workspace}, workspace, false, time.Now())
			if err != nil || !added {
				t.Fatalf("upsert added=%t, err=%v", added, err)
			}
		}
		return
	}
	dir := t.TempDir()
	const workers = 3
	runConcurrentStoreWorkers(t, t.Name(), dir, workers)
	data, err := NewStore(dir, filepath.Join(dir, "live")).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(data.Accounts); got != workers*perWorker {
		t.Fatalf("persisted %d accounts, want %d", got, workers*perWorker)
	}
}

func TestPersistRefreshedAccountRejectsReplacedToken(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	now := time.Now().UTC()
	tokens := TokenData{IDToken: makeTestIDToken(t, "user@example.com", "sub", "workspace", "team", now.Add(time.Hour)), AccessToken: "first", RefreshToken: "first"}
	account, _, err := store.UpsertTokens(tokens, "", true, now)
	if err != nil {
		t.Fatal(err)
	}
	account.AuthData.RefreshToken = "rotated"
	if err := store.PersistRefreshedAccount(*account, tokens.RefreshToken, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	account.AuthData.RefreshToken = "stale"
	if err := store.PersistRefreshedAccount(*account, tokens.RefreshToken, now); err == nil {
		t.Fatal("accepted a refresh for a replaced token")
	}
	live, err := store.ReadLiveAuth()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if live.Tokens.RefreshToken != "rotated" || stored.Accounts[0].AuthData.RefreshToken != "rotated" {
		t.Fatal("lost persisted refresh")
	}
}

func TestStaleAccountsSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, filepath.Join(dir, "live"))
	stale, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tokens := TokenData{IDToken: makeTestIDToken(t, "user@example.com", "sub", "workspace", "team", now.Add(time.Hour)), AccessToken: "initial", RefreshToken: "initial"}
	if _, _, err := store.UpsertTokens(tokens, "test", true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(stale); err == nil {
		t.Fatal("saved a snapshot that predates account creation")
	}
	if _, err := store.SyncActiveFromLiveAuth(stale, now); err != nil {
		t.Fatal(err)
	}
	if len(stale.Accounts) != 1 || stale.Accounts[0].Name != "test" {
		t.Fatal("sync lost the concurrently added account")
	}
}

func TestUsageRefreshOn401AndWindowParsing(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)

	id1 := makeTestIDToken(t, "user@example.com", "sub-1", "ws-1", "team", now.Add(time.Hour))
	ws1 := "ws-1"
	acct, _, err := store.UpsertTokens(TokenData{
		IDToken:      id1,
		AccessToken:  "stale-access",
		RefreshToken: "valid-refresh",
		AccountID:    &ws1,
	}, "Primary", true, now)
	if err != nil {
		t.Fatal(err)
	}

	refreshCalls := 0
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":      id1,
			"access_token":  "fresh-access",
			"refresh_token": "rotated-refresh",
		})
	}))
	defer oauthSrv.Close()

	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         35.0,
					"limit_window_seconds": 18000,
					"reset_at":             now.Add(2 * time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         80.0,
					"limit_window_seconds": 604800,
					"reset_at":             now.Add(48 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer usageSrv.Close()

	client := NewUsageClient()
	client.UsageURL = usageSrv.URL
	client.OAuth.Issuer = oauthSrv.URL
	client.Now = func() time.Time { return now }

	snap, err := client.FetchUsage(context.Background(), store, *acct, false)
	if err != nil {
		t.Fatalf("FetchUsage failed: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected 1 refresh call on 401, got %d", refreshCalls)
	}
	if snap.FiveHour == nil || snap.FiveHour.RemainingPercent != 65 {
		t.Fatalf("expected 5h remaining 65%%, got %+v", snap.FiveHour)
	}
	if snap.Weekly == nil || snap.Weekly.RemainingPercent != 20 {
		t.Fatalf("expected weekly remaining 20%%, got %+v", snap.Weekly)
	}

	reloaded, _ := store.Load()
	if reloaded.Accounts[0].AuthData.RefreshToken != "rotated-refresh" {
		t.Fatalf("expected rotated-refresh persisted, got %q", reloaded.Accounts[0].AuthData.RefreshToken)
	}
}

func TestInspectRolloutForRecovery(t *testing.T) {
	dir := t.TempDir()
	quotaFile := filepath.Join(dir, "quota.jsonl")
	content := `{"type":"session_meta","payload":{"id":"thr-123","agent_path":"/root"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"turn_started"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit."}}` + "\n"
	if err := os.WriteFile(quotaFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ok, qs := inspectRolloutForRecovery(quotaFile, false)
	if !ok || !qs || id != "thr-123" {
		t.Fatalf("expected thr-123 quota recovery target, got id=%q ok=%v qs=%v", id, ok, qs)
	}

	// Structured task_complete error ("Your workspace is out of credits.") must be detected.
	creditsFile := filepath.Join(dir, "credits.jsonl")
	creditsContent := `{"type":"session_meta","payload":{"id":"thr-credits","agent_path":"/root"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"Your workspace is out of credits. Add credits to continue.","codex_error_info":"usage_limit_exceeded"}}}` + "\n"
	if err := os.WriteFile(creditsFile, []byte(creditsContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, ok, qs := inspectRolloutForRecovery(creditsFile, false); !ok || !qs || id != "thr-credits" {
		t.Fatalf("expected thr-credits recovery target, got id=%q ok=%v qs=%v", id, ok, qs)
	}

	// Mentioning usage_limit_exceeded in tool output or completing a later clean turn must NOT trigger recovery.
	cleanFile := filepath.Join(dir, "clean.jsonl")
	cleanContent := creditsContent +
		`{"type":"event_msg","payload":{"type":"turn_started"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","output":"usage_limit_exceeded in grep"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	if err := os.WriteFile(cleanFile, []byte(cleanContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := inspectRolloutForRecovery(cleanFile, false); ok {
		t.Fatal("completed clean turn after recovery must not remain marked quotaStopped")
	}

	subFile := filepath.Join(dir, "sub.jsonl")
	subContent := `{"type":"session_meta","payload":{"id":"thr-sub","agent_path":"/root/worker"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"error","message":"usage_limit_exceeded"}}` + "\n"
	if err := os.WriteFile(subFile, []byte(subContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := inspectRolloutForRecovery(subFile, false); ok {
		t.Fatal("subagent rollout must not be selected for recovery")
	}
}

func TestParseCodexProcessOutputChatGPTBundle(t *testing.T) {
	psRaw := []byte(
		" 1484 ??       /Applications/ChatGPT.app/Contents/MacOS/ChatGPT\n" +
			" 1812 ??       /Applications/ChatGPT.app/Contents/Frameworks/Codex Framework.framework/Helpers/Codex (Aperitif).app/Contents/MacOS/Codex (Aperitif) --type=gpu-process\n" +
			" 1900 ??       /Applications/ChatGPT.app/Contents/Frameworks/Codex Framework.framework/Helpers/Codex (Renderer).app/Contents/MacOS/Codex (Renderer) --app-executable /Applications/ChatGPT.app/Contents/MacOS/ChatGPT --type=renderer\n" +
			" 2033 ??       /Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex -c features.code_mode_host=true app-server\n" +
			" 3001 ??       /Other/ChatGPT.app/Contents/MacOS/ChatGPT\n" +
			" 4001 s002     /opt/homebrew/bin/codex\n" +
			" 5001 ??       /Users/dev/.vscode/extensions/openai.chatgpt-0.5.0/bin/codex app-server\n",
	)
	got := parseCodexProcessOutput(psRaw, 9999, func(bundle string) string {
		if bundle == "/Applications/ChatGPT.app" {
			return "com.openai.codex"
		}
		return "com.openai.chat"
	})
	if len(got.DesktopPIDs) != 1 || got.DesktopPIDs[0] != 1484 {
		t.Fatalf("expected DesktopPIDs [1484], got %v", got.DesktopPIDs)
	}
	if len(got.DesktopBundles) != 1 || got.DesktopBundles[0] != "/Applications/ChatGPT.app" {
		t.Fatalf("expected DesktopBundles [/Applications/ChatGPT.app], got %v", got.DesktopBundles)
	}
	if len(got.DesktopHelperPIDs) != 1 || got.DesktopHelperPIDs[0] != 2033 {
		t.Fatalf("expected DesktopHelperPIDs [2033], got %v", got.DesktopHelperPIDs)
	}
	if len(got.CLIPIDs) != 1 || got.CLIPIDs[0] != 4001 {
		t.Fatalf("expected CLIPIDs [4001], got %v", got.CLIPIDs)
	}
	if len(got.AppServerPIDs) != 1 || got.AppServerPIDs[0] != 5001 {
		t.Fatalf("expected AppServerPIDs [5001], got %v", got.AppServerPIDs)
	}
}

func TestFilterUnrecoveredTargetsReceipts(t *testing.T) {
	home := t.TempDir()
	receiptDir := filepath.Join(home, "switcher-recovery")
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receiptDir, "thr-plain.offset"), []byte("200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receiptDir, "thr-json.offset"), []byte(`{"offset":300,"queued":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	targets := []RecoveryTarget{
		{SessionID: "thr-plain", Size: 200, QuotaStopped: true},
		{SessionID: "thr-json", Size: 250, QuotaStopped: true},
		{SessionID: "thr-grew", Size: 500, QuotaStopped: true},
	}
	if err := os.WriteFile(filepath.Join(receiptDir, "thr-grew.offset"), []byte("200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := FilterUnrecoveredTargets(home, targets)
	if len(got) != 1 || got[0].SessionID != "thr-grew" {
		t.Fatalf("expected only thr-grew pending, got %+v", got)
	}
}

func TestFetchUsageWithResets(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)

	ws := "ws-reset"
	acct, _, err := store.UpsertTokens(TokenData{
		IDToken:      makeTestIDToken(t, "reset@example.com", "sub-r", ws, "team", now.Add(2*time.Hour)),
		AccessToken:  "valid-access",
		RefreshToken: "valid-refresh",
		AccountID:    &ws,
	}, "reset-acct", true, now)
	if err != nil {
		t.Fatal(err)
	}

	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "team",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         10.0,
					"limit_window_seconds": 18000,
					"reset_at":             now.Add(2 * time.Hour).Unix(),
				},
				"secondary_window": map[string]any{
					"used_percent":         40.0,
					"limit_window_seconds": 604800,
					"reset_at":             now.Add(48 * time.Hour).Unix(),
				},
			},
		})
	}))
	defer usageSrv.Close()

	resetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("chatgpt-account-id") != ws {
			t.Errorf("expected chatgpt-account-id %q, got %q", ws, r.Header.Get("chatgpt-account-id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available_count":                   2,
			"total_earned_count":                0,
			"immediate_reset_purchase_eligible": false,
			"credits": []map[string]any{
				{
					"id":                   "RateLimitResetCredit_2",
					"reset_type":           "codex_rate_limits",
					"is_supported_by_plan": true,
					"status":               "available",
					"expires_at":           "2026-12-12T22:34:57.152562Z",
					"title":                "Full reset (Weekly + 5 hr)",
				},
				{
					"id":                   "RateLimitResetCredit_1",
					"reset_type":           "codex_rate_limits",
					"is_supported_by_plan": true,
					"status":               "available",
					"expires_at":           "2026-11-30T20:06:00.918126Z",
					"title":                "Full reset (Weekly + 5 hr)",
				},
				{
					"id":         "RateLimitResetCredit_used",
					"status":     "redeemed",
					"expires_at": "2026-11-22T00:00:00Z",
				},
			},
		})
	}))
	defer resetSrv.Close()

	client := NewUsageClient()
	client.UsageURL = usageSrv.URL
	client.ResetCreditsURL = resetSrv.URL
	client.Now = func() time.Time { return now }

	snap, err := client.FetchUsageWithResets(context.Background(), store, *acct, false)
	if err != nil {
		t.Fatalf("FetchUsageWithResets failed: %v", err)
	}
	if snap.ResetCredits == nil {
		t.Fatal("expected ResetCredits to be populated")
	}
	if snap.ResetCredits.AvailableCount != 2 {
		t.Fatalf("expected available_count=2, got %d", snap.ResetCredits.AvailableCount)
	}
	if snap.ResetCredits.NextExpiresAt == nil || snap.ResetCredits.NextExpiresAt.Format("2006-01-02") != "2026-11-30" {
		t.Fatalf("expected earliest available expiry 2026-11-30, got %v", snap.ResetCredits.NextExpiresAt)
	}

	// Best-effort degradation: 429 on reset-credits must still return usage cleanly.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer errSrv.Close()
	client.ResetCreditsURL = errSrv.URL
	snap2, err := client.FetchUsageWithResets(context.Background(), store, *acct, false)
	if err != nil {
		t.Fatalf("expected usage to succeed when reset-credits returns 429: %v", err)
	}
	if snap2.ResetCredits != nil {
		t.Fatalf("expected nil ResetCredits on 429, got %+v", snap2.ResetCredits)
	}
}

func TestConsumeResetCreditAndNextExpiringSelection(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
	ws := "ws-consume"
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	acct, _, err := store.UpsertTokens(TokenData{
		IDToken:      makeTestIDToken(t, "consume@example.com", "sub-c", ws, "team", now.Add(time.Hour)),
		AccessToken:  "tok-consume",
		RefreshToken: "ref-consume",
		AccountID:    &ws,
	}, "ConsumeSeat", true, now)
	if err != nil {
		t.Fatal(err)
	}

	expSoon := time.Date(2026, 11, 30, 20, 6, 0, 0, time.UTC)
	expLater := time.Date(2026, 12, 12, 22, 34, 0, 0, time.UTC)
	expPast := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	rc := &ResetCredits{
		AvailableCount: 2,
		Credits: []ResetCredit{
			{ID: "expired", Status: "available", IsSupportedByPlan: true, ExpiresAt: &expPast},
			{ID: "later", Status: "available", IsSupportedByPlan: true, ExpiresAt: &expLater},
			{ID: "soon", Status: "available", IsSupportedByPlan: true, ExpiresAt: &expSoon},
			{ID: "used", Status: "redeemed", IsSupportedByPlan: true, ExpiresAt: &expSoon},
		},
	}
	next := rc.NextExpiringCredit(now)
	if next == nil || next.ID != "soon" {
		t.Fatalf("NextExpiringCredit = %+v, want ID=soon", next)
	}

	var gotBody map[string]any
	consumeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("chatgpt-account-id") != ws {
			t.Errorf("chatgpt-account-id = %q, want %q", r.Header.Get("chatgpt-account-id"), ws)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":          "reset",
			"credit":        map[string]any{"id": "soon"},
			"windows_reset": []string{"primary", "secondary"},
		})
	}))
	defer consumeSrv.Close()

	client := NewUsageClient()
	client.ConsumeResetCreditURL = consumeSrv.URL
	client.Now = func() time.Time { return now }

	res, err := client.ConsumeResetCredit(context.Background(), store, *acct, "soon", false)
	if err != nil {
		t.Fatalf("ConsumeResetCredit failed: %v", err)
	}
	if !res.Redeemed() || res.Outcome != "reset" || res.CreditID != "soon" || len(res.WindowsReset) != 2 {
		t.Fatalf("unexpected ConsumeResetResult: %+v", res)
	}
	if gotBody["credit_id"] != "soon" {
		t.Fatalf("request credit_id = %v, want soon", gotBody["credit_id"])
	}
	if reqID, _ := gotBody["redeem_request_id"].(string); reqID == "" {
		t.Fatalf("missing redeem_request_id in request body: %+v", gotBody)
	}
}
