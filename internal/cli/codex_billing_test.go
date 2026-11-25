// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func TestCodexBillingJSONAndTableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/check":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accounts": map[string]any{
					"ws-uk": map[string]any{
						"account": map[string]any{
							"name":              "workspace",
							"structure":         "workspace",
							"plan_type":         "team",
							"is_deactivated":    false,
							"account_user_role": "account-owner",
							"account_user_id":   "user-1__ws-uk",
							"account_owner_id":  "user-1",
						},
						"entitlement": map[string]any{"has_active_subscription": true},
					},
					"ws-us": map[string]any{
						"account": map[string]any{
							"name":              "Lifan's Workspace",
							"structure":         "workspace",
							"plan_type":         "self_serve_business_prolite",
							"is_deactivated":    false,
							"account_user_role": "account-owner",
							"account_user_id":   "user-1__ws-us",
							"account_owner_id":  "user-1",
						},
						"entitlement": map[string]any{"has_active_subscription": true},
					},
					"ws-old": map[string]any{
						"account": map[string]any{
							"name":              "Old WS",
							"structure":         "workspace",
							"plan_type":         "team",
							"is_deactivated":    true,
							"account_user_role": "account-owner",
							"account_user_id":   "user-1__ws-old",
							"account_owner_id":  "user-1",
						},
						"entitlement": map[string]any{"has_active_subscription": false},
					},
				},
			})
		case "/subscriptions":
			switch r.URL.Query().Get("account_id") {
			case "ws-uk":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"plan_type":        "team",
					"is_active":        true,
					"billing_currency": "GBP",
					"price_country":    "GB",
					"will_renew":       false,
					"entitlement": map[string]any{
						"has_active_subscription": true,
						"cancels_at":              "2026-12-10T04:53:23Z",
						"discount": map[string]any{
							"amount":            25.0,
							"promo_campaign_id": "smb-channel-partner-codestone-GB-GBP",
						},
					},
					"seat_capacity": []map[string]any{
						{"type": "default", "paid": 2, "renewal_requested": 2},
					},
				})
			case "ws-us":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"plan_type":        "team",
					"is_active":        true,
					"billing_currency": "USD",
					"will_renew":       true,
					"entitlement": map[string]any{
						"has_active_subscription": true,
						"renews_at":               "2026-11-30T17:29:02Z",
						"discount": map[string]any{
							"amount":            30.0,
							"promo_campaign_id": "smb-channel-partner-datroai-US-USD",
						},
					},
					"seat_capacity": []map[string]any{
						{"type": "default", "paid": 1, "renewal_requested": 2},
						{
							"type":              "prolite",
							"paid":              1,
							"renewal_requested": 0,
							"pending_occupied_downgrades": []map[string]any{
								{"to_type": "default", "quantity": 1},
							},
						},
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"plan_type":        "team",
					"is_active":        false,
					"billing_currency": "USD",
					"will_renew":       false,
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	email := "owner@example.com"
	if err := codexStore.Save(&codex.AccountsStore{
		Version: 1,
		Accounts: []codex.StoredAccount{{
			ID:        "acct-1",
			Name:      email,
			Email:     &email,
			AuthMode:  codex.AuthModeChatGPT,
			AuthData:  codex.AuthData{Type: codex.AuthModeChatGPT, AccessToken: "tok-1"},
			CreatedAt: testNow,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	usageClient := codex.NewUsageClient()
	usageClient.AccountsCheckURL = srv.URL + "/accounts/check"
	usageClient.SubscriptionsURL = srv.URL + "/subscriptions"

	app, out, errBuf := env.app("")
	app.CodexStore = codexStore
	app.CodexUsage = usageClient
	if code := app.Run([]string{"--json", "codex", "billing"}); code != 0 {
		t.Fatalf("codex billing --json exit %d: %s", code, errBuf.String())
	}
	var envelope struct {
		Command string `json:"command"`
		Data    struct {
			Provider   string                   `json:"provider"`
			Workspaces []codex.WorkspaceBilling `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if envelope.Command != "codex.billing" || envelope.Data.Provider != "codex" || len(envelope.Data.Workspaces) != 2 {
		t.Fatalf("unexpected billing JSON: %s", out.String())
	}

	appAll, outAll, errAll := env.app("")
	appAll.CodexStore = codexStore
	appAll.CodexUsage = usageClient
	if code := appAll.Run([]string{"--json", "codex", "billing", "--all"}); code != 0 {
		t.Fatalf("codex billing --all exit %d: %s", code, errAll.String())
	}
	if err := json.Unmarshal(outAll.Bytes(), &envelope); err != nil {
		t.Fatalf("decode json --all: %v", err)
	}
	if len(envelope.Data.Workspaces) != 3 {
		t.Fatalf("billing --all workspaces = %d, want 3", len(envelope.Data.Workspaces))
	}

	appText, outText, errText := env.app("")
	appText.Width = 160
	appText.CodexStore = codexStore
	appText.CodexUsage = usageClient
	if code := appText.Run([]string{"codex", "billing"}); code != 0 {
		t.Fatalf("codex billing text exit %d: %s", code, errText.String())
	}
	rendered := outText.String()
	for _, want := range []string{"GBP (GB)", "cancels 2026-12-10", "1 std, 1 prem -> 2 std", "£25 (codestone-GB-GBP)"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered billing table missing %q:\n%s", want, rendered)
		}
	}
}
