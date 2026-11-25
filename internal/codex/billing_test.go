// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestFetchBillingDeduplicatesAndMergesWorkspaces(t *testing.T) {
	ownerEmail := "owner@example.com"
	adminEmail := "admin@example.com"
	apiKey := "sk-test"

	var mu sync.Mutex
	var checkCalls int
	var subAuthByWS = make(map[string]string)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/accounts/check":
			mu.Lock()
			checkCalls++
			mu.Unlock()
			if auth == "Bearer tok-owner" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accounts": map[string]any{
						"default": map[string]any{},
						"personal-free": map[string]any{
							"account":     map[string]any{"name": "Personal", "structure": "personal", "account_user_id": "user-owner__personal-free"},
							"entitlement": map[string]any{"has_active_subscription": false},
						},
						"ws-uk": map[string]any{
							"account": map[string]any{
								"name":              "UK Workspace",
								"structure":         "workspace",
								"plan_type":         "team",
								"is_deactivated":    false,
								"account_user_role": "account-owner",
								"account_user_id":   "user-owner__ws-uk",
								"account_owner_id":  "user-owner",
							},
							"entitlement": map[string]any{"has_active_subscription": true},
						},
						"ws-us": map[string]any{
							"account": map[string]any{
								"name":              "US Workspace",
								"structure":         "workspace",
								"plan_type":         "self_serve_business_prolite",
								"is_deactivated":    false,
								"account_user_role": "account-owner",
								"account_user_id":   "user-owner__ws-us",
								"account_owner_id":  "user-owner",
							},
							"entitlement": map[string]any{"has_active_subscription": true},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accounts": map[string]any{
					"ws-us": map[string]any{
						"account": map[string]any{
							"name":              "US Workspace",
							"structure":         "workspace",
							"plan_type":         "team",
							"is_deactivated":    false,
							"account_user_role": "account-admin",
							"account_user_id":   "user-admin__ws-us",
							"account_owner_id":  "user-owner",
						},
						"entitlement": map[string]any{"has_active_subscription": true},
					},
					"ws-old": map[string]any{
						"account": map[string]any{
							"name":              "Old Workspace",
							"structure":         "workspace",
							"plan_type":         "team",
							"is_deactivated":    true,
							"account_user_role": "account-owner",
							"account_user_id":   "user-admin__ws-old",
							"account_owner_id":  "user-admin",
						},
						"entitlement": map[string]any{"has_active_subscription": false},
					},
				},
			})
		case "/subscriptions":
			wsID := r.URL.Query().Get("account_id")
			mu.Lock()
			subAuthByWS[wsID] = auth
			mu.Unlock()
			switch wsID {
			case "ws-uk":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"plan_type":            "team",
					"is_active":            true,
					"seats_in_use":         2,
					"seats_entitled":       2,
					"billing_period":       "monthly",
					"billing_currency":     "GBP",
					"price_country":        "GB",
					"pricing_scope":        "country",
					"will_renew":           false,
					"cancellation_outcome": "deactivate",
					"entitlement": map[string]any{
						"has_active_subscription": true,
						"subscription_plan":       "chatgptteamplan",
						"billing_currency":        "GBP",
						"cancels_at":              "2026-12-10T04:53:23Z",
						"discount": map[string]any{
							"discount_type":        "fixed",
							"amount":               25.0,
							"duration_num_periods": 48,
							"discount_expires_at":  "2030-05-10T04:53:23Z",
							"promo_campaign_id":    "smb-channel-partner-codestone-GB-GBP",
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
					"seats_in_use":     2,
					"seats_entitled":   2,
					"billing_period":   "monthly",
					"billing_currency": "USD",
					"pricing_scope":    "default",
					"will_renew":       true,
					"entitlement": map[string]any{
						"has_active_subscription": true,
						"subscription_plan":       "chatgptteamplan",
						"billing_currency":        "USD",
						"renews_at":               "2026-11-29T19:00:15Z",
						"discount": map[string]any{
							"discount_type":     "fixed",
							"amount":            30.0,
							"promo_campaign_id": "smb-channel-partner-skyfaith-US-USD",
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

	client := NewUsageClient()
	client.AccountsCheckURL = srv.URL + "/accounts/check"
	client.SubscriptionsURL = srv.URL + "/subscriptions"
	client.Now = func() time.Time { return time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC) }

	accounts := []StoredAccount{
		{ID: "acct-1", Email: &ownerEmail, AuthMode: AuthModeChatGPT, AuthData: AuthData{AccessToken: "tok-owner"}},
		{ID: "acct-2", Email: &ownerEmail, AuthMode: AuthModeChatGPT, AuthData: AuthData{AccessToken: "tok-owner-dup"}},
		{ID: "acct-3", Email: &adminEmail, AuthMode: AuthModeChatGPT, AuthData: AuthData{AccessToken: "tok-admin"}},
		{ID: "acct-api", Email: &ownerEmail, AuthMode: AuthModeAPIKey, AuthData: AuthData{Key: &apiKey}},
	}

	report, err := client.FetchBilling(context.Background(), nil, accounts, "acct-1", true)
	if err != nil {
		t.Fatalf("FetchBilling failed: %v", err)
	}
	if checkCalls != 2 {
		t.Fatalf("accounts/check calls = %d, want 2 (deduplicated by email)", checkCalls)
	}
	if len(report.Workspaces) != 3 {
		t.Fatalf("workspaces = %d, want 3 (free personal skipped)", len(report.Workspaces))
	}
	if subAuthByWS["ws-us"] != "Bearer tok-owner" {
		t.Fatalf("ws-us fetched with %q, want owner token", subAuthByWS["ws-us"])
	}

	uk := report.Workspaces[0]
	if uk.WorkspaceID != "ws-uk" || uk.BillingCurrency != "GBP" || uk.PriceCountry != "GB" || uk.WillRenew || uk.CancelsAt == nil {
		t.Fatalf("unexpected UK workspace: %+v", uk)
	}
	if uk.Discount == nil || uk.Discount.Amount != 25 || uk.Discount.PromoCampaignID != "smb-channel-partner-codestone-GB-GBP" {
		t.Fatalf("unexpected UK discount: %+v", uk.Discount)
	}

	us := report.Workspaces[1]
	if us.WorkspaceID != "ws-us" || us.OwnerEmail != ownerEmail || len(us.Members) != 2 || us.Members[0].Role != "account-owner" {
		t.Fatalf("unexpected US workspace ownership/members: %+v", us)
	}
	if len(us.SeatCapacity) != 2 || len(us.SeatCapacity[1].PendingOccupiedDowngrades) != 1 {
		t.Fatalf("unexpected US seat capacity: %+v", us.SeatCapacity)
	}

	old := report.Workspaces[2]
	if old.WorkspaceID != "ws-old" || old.Active || !old.Deactivated {
		t.Fatalf("unexpected deactivated workspace: %+v", old)
	}
}
