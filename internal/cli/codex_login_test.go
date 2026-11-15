package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

// Complete the local mock OAuth callback when login prints its authorization URL.
type codexLoginOutput struct {
	bytes.Buffer
	t *testing.T
}

func (w *codexLoginOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	for _, field := range strings.Fields(string(p)) {
		if !strings.Contains(field, "/oauth/authorize?") {
			continue
		}
		authURL, parseErr := url.Parse(field)
		if parseErr != nil {
			w.t.Fatal(parseErr)
		}
		callback, parseErr := url.Parse(authURL.Query().Get("redirect_uri"))
		if parseErr != nil {
			w.t.Fatal(parseErr)
		}
		q := callback.Query()
		q.Set("state", authURL.Query().Get("state"))
		q.Set("code", "test-authorization-code")
		callback.RawQuery = q.Encode()
		client := &http.Client{Timeout: 5 * time.Second}
		resp, callErr := client.Get(callback.String())
		if callErr != nil {
			w.t.Fatal(callErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			w.t.Fatalf("mock callback status %d", resp.StatusCode)
		}
	}
	return n, err
}

func TestCodexLoginActivation(t *testing.T) {
	for _, noActivate := range []bool{false, true} {
		name := "activate"
		if noActivate {
			name = "save-only"
		}
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, true)
			store := codex.NewStore(filepath.Join(env.dir, "accounts"), filepath.Join(env.dir, "codex"))
			firstWS, targetWS := "ws-active", "ws-target"
			first, _, err := store.UpsertTokens(codex.TokenData{
				IDToken:     makeCodexIDToken(t, "first@example.com", "first", firstWS, "team"),
				AccessToken: "first-access", RefreshToken: "first-refresh", AccountID: &firstWS,
			}, "First", true, testNow)
			if err != nil {
				t.Fatal(err)
			}
			target, _, err := store.UpsertTokens(codex.TokenData{
				IDToken:     makeCodexIDToken(t, "target@example.com", "target", targetWS, "team"),
				AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: &targetWS,
			}, "Target", false, testNow)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.LiveAuthPath())
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id_token":     makeCodexIDToken(t, "target@example.com", "target", targetWS, "team"),
					"access_token": "new-access", "refresh_token": "new-refresh",
				})
			}))
			defer server.Close()
			app, _, errOut := env.app("")
			app.CodexStore = store
			app.CodexUsage = codex.NewUsageClient()
			app.CodexUsage.OAuth.Issuer = server.URL
			out := &codexLoginOutput{t: t}
			app.Out = out
			args := []string{"codex", "login", "--no-browser"}
			if noActivate {
				args = append(args, "--no-activate")
			}
			if code := app.Run(args); code != 0 {
				t.Fatalf("login failed: %d %s", code, errOut.String())
			}
			data, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(data.Accounts) != 2 || data.Accounts[1].ID != target.ID || data.Accounts[1].Name != target.Name || !data.Accounts[1].CreatedAt.Equal(target.CreatedAt) {
				t.Fatal("re-login changed existing account identity or metadata")
			}
			if data.Accounts[1].AuthData.RefreshToken != "new-refresh" {
				t.Fatal("re-login did not update the target tokens")
			}
			after, err := os.ReadFile(store.LiveAuthPath())
			if err != nil {
				t.Fatal(err)
			}
			if noActivate {
				if *data.ActiveAccountID != first.ID || !bytes.Equal(before, after) || data.Accounts[1].LastUsedAt != nil {
					t.Fatal("save-only login changed live auth, active selection, or last use")
				}
				if !strings.Contains(out.String(), "without activating") {
					t.Fatal("save-only success message missing")
				}
			} else if *data.ActiveAccountID != target.ID || bytes.Equal(before, after) {
				t.Fatal("default login did not activate the renewed account")
			}
		})
	}
}

var _ io.Writer = (*codexLoginOutput)(nil)
