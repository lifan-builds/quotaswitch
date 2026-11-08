package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := writeFileAtomic(store.LiveAuthPath(), rawAuth); err != nil {
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
	id, ok := inspectRolloutForRecovery(quotaFile, false)
	if !ok || id != "thr-123" {
		t.Fatalf("expected thr-123 recovery target, got id=%q ok=%v", id, ok)
	}

	subFile := filepath.Join(dir, "sub.jsonl")
	subContent := `{"type":"session_meta","payload":{"id":"thr-sub","agent_path":"/root/worker"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"error","message":"usage_limit_exceeded"}}` + "\n"
	if err := os.WriteFile(subFile, []byte(subContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := inspectRolloutForRecovery(subFile, false); ok {
		t.Fatal("subagent rollout must not be selected for recovery")
	}
}
