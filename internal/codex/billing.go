// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/usage"
)

type WorkspaceMember struct {
	Email  string `json:"email"`
	Role   string `json:"role,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

type PendingSeatDowngrade struct {
	ToType   string `json:"to_type"`
	Quantity int    `json:"quantity"`
}

type SeatCapacity struct {
	Type                        string                 `json:"type"`
	Paid                        int                    `json:"paid"`
	Held                        int                    `json:"held,omitempty"`
	RenewalRequested            *int                   `json:"renewal_requested,omitempty"`
	Available                   int                    `json:"available,omitempty"`
	PendingOccupiedDowngrades   []PendingSeatDowngrade `json:"pending_occupied_downgrades,omitempty"`
	PendingUnoccupiedDowngrades []PendingSeatDowngrade `json:"pending_unoccupied_downgrades,omitempty"`
}

type SubscriptionDiscount struct {
	DiscountType       string     `json:"discount_type,omitempty"`
	Amount             float64    `json:"amount,omitempty"`
	DurationNumPeriods int        `json:"duration_num_periods,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	PromoCampaignID    string     `json:"promo_campaign_id,omitempty"`
}

type WorkspaceBilling struct {
	WorkspaceID         string                `json:"workspace_id"`
	Name                string                `json:"name"`
	Structure           string                `json:"structure,omitempty"`
	PlanType            string                `json:"plan_type,omitempty"`
	SubscriptionPlan    string                `json:"subscription_plan,omitempty"`
	Active              bool                  `json:"active"`
	Deactivated         bool                  `json:"deactivated"`
	OwnerID             string                `json:"owner_id,omitempty"`
	OwnerEmail          string                `json:"owner_email,omitempty"`
	Members             []WorkspaceMember     `json:"members,omitempty"`
	BillingCurrency     string                `json:"billing_currency,omitempty"`
	PriceCountry        string                `json:"price_country,omitempty"`
	PricingScope        string                `json:"pricing_scope,omitempty"`
	BillingPeriod       string                `json:"billing_period,omitempty"`
	WillRenew           bool                  `json:"will_renew"`
	CancellationOutcome string                `json:"cancellation_outcome,omitempty"`
	ActiveStart         *time.Time            `json:"active_start,omitempty"`
	ActiveUntil         *time.Time            `json:"active_until,omitempty"`
	RenewsAt            *time.Time            `json:"renews_at,omitempty"`
	CancelsAt           *time.Time            `json:"cancels_at,omitempty"`
	SeatsInUse          int                   `json:"seats_in_use,omitempty"`
	SeatsEntitled       int                   `json:"seats_entitled,omitempty"`
	SeatCapacity        []SeatCapacity        `json:"seat_capacity,omitempty"`
	Discount            *SubscriptionDiscount `json:"discount,omitempty"`
	Error               string                `json:"error,omitempty"`
}

type BillingReport struct {
	Workspaces []WorkspaceBilling `json:"workspaces"`
	Errors     []string           `json:"errors,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

func (c *UsageClient) accountsCheckEndpoint() string {
	if c != nil && strings.TrimSpace(c.AccountsCheckURL) != "" {
		return strings.TrimSpace(c.AccountsCheckURL)
	}
	return DefaultAccountsCheckURL
}

func (c *UsageClient) subscriptionsEndpoint() string {
	if c != nil && strings.TrimSpace(c.SubscriptionsURL) != "" {
		return strings.TrimSpace(c.SubscriptionsURL)
	}
	return DefaultSubscriptionsURL
}

func (c *UsageClient) FetchBilling(ctx context.Context, store *Store, accounts []StoredAccount, activeID string, activeRunning bool) (*BillingReport, error) {
	now := c.now().UTC()
	report := &BillingReport{
		Workspaces: []WorkspaceBilling{},
		UpdatedAt:  now,
	}

	unique := deduplicateChatGPTAccountsByEmail(accounts, activeID)
	if len(unique) == 0 {
		return report, nil
	}

	type checkResult struct {
		account StoredAccount
		email   string
		payload accountsCheckPayload
		err     error
	}

	results := make([]checkResult, len(unique))
	var wg sync.WaitGroup
	for i, acct := range unique {
		wg.Add(1)
		go func(idx int, a StoredAccount) {
			defer wg.Done()
			isActiveRunning := activeRunning && a.ID == activeID
			updated, payload, err := c.fetchAccountsCheckWithRefresh(ctx, store, a, isActiveRunning, now)
			results[idx] = checkResult{
				account: updated,
				email:   updated.EmailString(),
				payload: payload,
				err:     err,
			}
		}(i, acct)
	}
	wg.Wait()

	userIDToEmail := make(map[string]string)
	type workspaceEntry struct {
		billing     WorkspaceBilling
		fetchAcct   StoredAccount
		fetchRole   string
		seenMembers map[string]bool
	}
	workspaces := make(map[string]*workspaceEntry)

	for _, res := range results {
		if res.err != nil {
			label := res.email
			if label == "" {
				label = res.account.DisplayName()
			}
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", label, usage.ShortError(res.err)))
			continue
		}
		for wsID, item := range res.payload.Accounts {
			if wsID == "" || wsID == "default" {
				continue
			}
			uid := extractUserID(item.Account.AccountUserID)
			if uid != "" && res.email != "" {
				userIDToEmail[uid] = res.email
			}
			if strings.EqualFold(item.Account.Structure, "personal") && !item.Entitlement.HasActiveSubscription {
				continue
			}

			entry, ok := workspaces[wsID]
			if !ok {
				entry = &workspaceEntry{
					billing: WorkspaceBilling{
						WorkspaceID: wsID,
						Name:        strings.TrimSpace(item.Account.Name),
						Structure:   strings.TrimSpace(item.Account.Structure),
						PlanType:    strings.TrimSpace(item.Account.PlanType),
						Active:      item.Entitlement.HasActiveSubscription,
						Deactivated: item.Account.IsDeactivated,
						OwnerID:     strings.TrimSpace(item.Account.AccountOwnerID),
					},
					seenMembers: make(map[string]bool),
				}
				workspaces[wsID] = entry
			} else {
				if entry.billing.Name == "" {
					entry.billing.Name = strings.TrimSpace(item.Account.Name)
				}
				if entry.billing.OwnerID == "" {
					entry.billing.OwnerID = strings.TrimSpace(item.Account.AccountOwnerID)
				}
				if item.Entitlement.HasActiveSubscription {
					entry.billing.Active = true
				}
				if item.Account.IsDeactivated {
					entry.billing.Deactivated = true
				}
			}

			memberKey := strings.ToLower(res.email)
			if memberKey == "" {
				memberKey = uid
			}
			if memberKey != "" && !entry.seenMembers[memberKey] {
				entry.seenMembers[memberKey] = true
				entry.billing.Members = append(entry.billing.Members, WorkspaceMember{
					Email:  res.email,
					Role:   strings.TrimSpace(item.Account.AccountUserRole),
					UserID: uid,
				})
			}

			role := strings.TrimSpace(item.Account.AccountUserRole)
			if entry.fetchAcct.ID == "" || (role == "account-owner" && entry.fetchRole != "account-owner") {
				entry.fetchAcct = res.account
				entry.fetchRole = role
			}
		}
	}

	entries := make([]*workspaceEntry, 0, len(workspaces))
	for _, entry := range workspaces {
		if email, ok := userIDToEmail[entry.billing.OwnerID]; ok {
			entry.billing.OwnerEmail = email
		}
		sortWorkspaceMembers(entry.billing.Members)
		entries = append(entries, entry)
	}

	var subWG sync.WaitGroup
	for _, entry := range entries {
		subWG.Add(1)
		go func(e *workspaceEntry) {
			defer subWG.Done()
			sub, err := c.requestSubscription(ctx, e.fetchAcct, e.billing.WorkspaceID)
			if err != nil {
				e.billing.Error = usage.ShortError(err)
				return
			}
			applySubscriptionPayload(&e.billing, sub)
		}(entry)
	}
	subWG.Wait()

	for _, entry := range entries {
		report.Workspaces = append(report.Workspaces, entry.billing)
	}
	sort.Slice(report.Workspaces, func(i, j int) bool {
		a, b := report.Workspaces[i], report.Workspaces[j]
		if a.Active != b.Active {
			return a.Active
		}
		if a.Deactivated != b.Deactivated {
			return !a.Deactivated
		}
		an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if an != bn {
			return an < bn
		}
		return a.WorkspaceID < b.WorkspaceID
	})
	sort.Strings(report.Errors)
	return report, nil
}

func deduplicateChatGPTAccountsByEmail(accounts []StoredAccount, activeID string) []StoredAccount {
	byKey := make(map[string]int)
	out := make([]StoredAccount, 0, len(accounts))
	for _, acct := range accounts {
		if acct.AuthMode == AuthModeAPIKey {
			continue
		}
		if strings.TrimSpace(acct.AuthData.AccessToken) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(acct.EmailString()))
		if key == "" {
			key = acct.ID
		}
		if idx, exists := byKey[key]; exists {
			if acct.ID == activeID {
				out[idx] = acct
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, acct)
	}
	return out
}

func extractUserID(accountUserID string) string {
	if idx := strings.Index(accountUserID, "__"); idx > 0 {
		return strings.TrimSpace(accountUserID[:idx])
	}
	return ""
}

func roleRank(role string) int {
	switch role {
	case "account-owner":
		return 0
	case "account-admin":
		return 1
	default:
		return 2
	}
}

func sortWorkspaceMembers(members []WorkspaceMember) {
	sort.Slice(members, func(i, j int) bool {
		ri, rj := roleRank(members[i].Role), roleRank(members[j].Role)
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(members[i].Email) < strings.ToLower(members[j].Email)
	})
}

func (c *UsageClient) fetchAccountsCheckWithRefresh(ctx context.Context, store *Store, account StoredAccount, activeRunning bool, now time.Time) (StoredAccount, accountsCheckPayload, error) {
	if !activeRunning && NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		refreshed, err := c.refreshAndPersist(ctx, store, account.ID)
		if err != nil {
			return account, accountsCheckPayload{}, err
		}
		account = *refreshed
	}
	payload, status, err := c.requestAccountsCheck(ctx, account)
	if status == http.StatusUnauthorized && !activeRunning && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, rErr := c.refreshAndPersist(ctx, store, account.ID); rErr == nil {
			account = *refreshed
			payload, _, err = c.requestAccountsCheck(ctx, account)
		}
	}
	return account, payload, err
}

func (c *UsageClient) requestAccountsCheck(ctx context.Context, account StoredAccount) (accountsCheckPayload, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.accountsCheckEndpoint(), nil)
	if err != nil {
		return accountsCheckPayload{}, 0, err
	}
	applyChatGPTBearerHeaders(req, account.AuthData.AccessToken, "")

	body, status, err := c.doChatGPTJSON(req)
	if err != nil {
		return accountsCheckPayload{}, status, err
	}
	var payload accountsCheckPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return accountsCheckPayload{}, status, fmt.Errorf("%w: %v", usage.ErrInvalidResponse, err)
	}
	return payload, status, nil
}

func (c *UsageClient) requestSubscription(ctx context.Context, account StoredAccount, workspaceID string) (subscriptionPayload, error) {
	endpoint, err := url.Parse(c.subscriptionsEndpoint())
	if err != nil {
		return subscriptionPayload{}, err
	}
	q := endpoint.Query()
	q.Set("account_id", workspaceID)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return subscriptionPayload{}, err
	}
	applyChatGPTBearerHeaders(req, account.AuthData.AccessToken, workspaceID)

	body, _, err := c.doChatGPTJSON(req)
	if err != nil {
		return subscriptionPayload{}, err
	}
	var payload subscriptionPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return subscriptionPayload{}, fmt.Errorf("%w: %v", usage.ErrInvalidResponse, err)
	}
	return payload, nil
}

func applyChatGPTBearerHeaders(req *http.Request, accessToken, workspaceID string) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", browserUserAgent)
	if strings.TrimSpace(workspaceID) != "" {
		req.Header.Set("chatgpt-account-id", strings.TrimSpace(workspaceID))
	}
}

func (c *UsageClient) doChatGPTJSON(req *http.Request) ([]byte, int, error) {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, usage.ErrTimeout
		}
		return nil, 0, usage.ErrNetwork
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.StatusCode, usage.ErrAuth
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, usage.ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("%w: status %d", usage.ErrHTTP, resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

type accountsCheckPayload struct {
	Accounts map[string]struct {
		Account struct {
			Name            string `json:"name"`
			Structure       string `json:"structure"`
			PlanType        string `json:"plan_type"`
			IsDeactivated   bool   `json:"is_deactivated"`
			AccountUserRole string `json:"account_user_role"`
			AccountUserID   string `json:"account_user_id"`
			AccountOwnerID  string `json:"account_owner_id"`
		} `json:"account"`
		Entitlement struct {
			HasActiveSubscription bool `json:"has_active_subscription"`
		} `json:"entitlement"`
	} `json:"accounts"`
}

type subscriptionDiscountPayload struct {
	DiscountType       string  `json:"discount_type"`
	Amount             float64 `json:"amount"`
	DurationNumPeriods int     `json:"duration_num_periods"`
	DiscountExpiresAt  *string `json:"discount_expires_at"`
	PromoCampaignID    string  `json:"promo_campaign_id"`
}

type subscriptionPayload struct {
	PlanType            string  `json:"plan_type"`
	IsActive            bool    `json:"is_active"`
	SeatsInUse          int     `json:"seats_in_use"`
	SeatsEntitled       int     `json:"seats_entitled"`
	BillingPeriod       string  `json:"billing_period"`
	BillingCurrency     string  `json:"billing_currency"`
	PriceCountry        string  `json:"price_country"`
	PricingScope        string  `json:"pricing_scope"`
	WillRenew           bool    `json:"will_renew"`
	CancellationOutcome string  `json:"cancellation_outcome"`
	ActiveStart         *string `json:"active_start"`
	ActiveUntil         *string `json:"active_until"`
	SeatCapacity        []struct {
		Type                        string                 `json:"type"`
		Paid                        int                    `json:"paid"`
		Held                        int                    `json:"held"`
		RenewalRequested            *int                   `json:"renewal_requested"`
		Available                   int                    `json:"available"`
		PendingOccupiedDowngrades   []PendingSeatDowngrade `json:"pending_occupied_downgrades"`
		PendingUnoccupiedDowngrades []PendingSeatDowngrade `json:"pending_unoccupied_downgrades"`
	} `json:"seat_capacity"`
	Entitlement *struct {
		HasActiveSubscription bool                         `json:"has_active_subscription"`
		SubscriptionPlan      string                       `json:"subscription_plan"`
		BillingCurrency       string                       `json:"billing_currency"`
		BillingPeriod         string                       `json:"billing_period"`
		RenewsAt              *string                      `json:"renews_at"`
		CancelsAt             *string                      `json:"cancels_at"`
		Discount              *subscriptionDiscountPayload `json:"discount"`
	} `json:"entitlement"`
}

func parseOptionalRFC3339(s *string) *time.Time {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, strings.TrimSpace(*s)); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

func applySubscriptionPayload(dst *WorkspaceBilling, src subscriptionPayload) {
	if strings.TrimSpace(src.PlanType) != "" && dst.PlanType == "" {
		dst.PlanType = strings.TrimSpace(src.PlanType)
	}
	dst.Active = src.IsActive
	dst.SeatsInUse = src.SeatsInUse
	dst.SeatsEntitled = src.SeatsEntitled
	dst.BillingPeriod = strings.TrimSpace(src.BillingPeriod)
	dst.BillingCurrency = strings.TrimSpace(src.BillingCurrency)
	dst.PriceCountry = strings.TrimSpace(src.PriceCountry)
	dst.PricingScope = strings.TrimSpace(src.PricingScope)
	dst.WillRenew = src.WillRenew
	dst.CancellationOutcome = strings.TrimSpace(src.CancellationOutcome)
	dst.ActiveStart = parseOptionalRFC3339(src.ActiveStart)
	dst.ActiveUntil = parseOptionalRFC3339(src.ActiveUntil)

	if src.Entitlement != nil {
		if src.Entitlement.HasActiveSubscription {
			dst.Active = true
		}
		dst.SubscriptionPlan = strings.TrimSpace(src.Entitlement.SubscriptionPlan)
		if dst.BillingCurrency == "" {
			dst.BillingCurrency = strings.TrimSpace(src.Entitlement.BillingCurrency)
		}
		if dst.BillingPeriod == "" {
			dst.BillingPeriod = strings.TrimSpace(src.Entitlement.BillingPeriod)
		}
		dst.RenewsAt = parseOptionalRFC3339(src.Entitlement.RenewsAt)
		dst.CancelsAt = parseOptionalRFC3339(src.Entitlement.CancelsAt)
		if d := src.Entitlement.Discount; d != nil {
			dst.Discount = &SubscriptionDiscount{
				DiscountType:       strings.TrimSpace(d.DiscountType),
				Amount:             d.Amount,
				DurationNumPeriods: d.DurationNumPeriods,
				ExpiresAt:          parseOptionalRFC3339(d.DiscountExpiresAt),
				PromoCampaignID:    strings.TrimSpace(d.PromoCampaignID),
			}
		}
	}

	if len(src.SeatCapacity) > 0 {
		dst.SeatCapacity = make([]SeatCapacity, 0, len(src.SeatCapacity))
		for _, sc := range src.SeatCapacity {
			dst.SeatCapacity = append(dst.SeatCapacity, SeatCapacity{
				Type:                        strings.TrimSpace(sc.Type),
				Paid:                        sc.Paid,
				Held:                        sc.Held,
				RenewalRequested:            sc.RenewalRequested,
				Available:                   sc.Available,
				PendingOccupiedDowngrades:   sc.PendingOccupiedDowngrades,
				PendingUnoccupiedDowngrades: sc.PendingUnoccupiedDowngrades,
			})
		}
	}
}
