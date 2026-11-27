// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/usage"
)

const (
	DefaultUsageURL              = "https://chatgpt.com/backend-api/wham/usage"
	DefaultResetCreditsURL       = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	DefaultConsumeResetCreditURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	DefaultAccountsCheckURL      = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"
	DefaultSubscriptionsURL      = "https://chatgpt.com/backend-api/subscriptions"
	DefaultWarmupURL             = "https://chatgpt.com/backend-api/codex/responses"
	DefaultAPIWarmupURL          = "https://api.openai.com/v1/responses"
	browserUserAgent             = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	codexCLIUserAgent            = "codex_cli_rs/0.167.0-alpha.3.2 (Mac OS 26.0; arm64)"
	codexIgniteResetSlack        = 2 * time.Minute
)

type ResetCredit struct {
	ID                string     `json:"id"`
	ResetType         string     `json:"reset_type,omitempty"`
	IsSupportedByPlan bool       `json:"is_supported_by_plan,omitempty"`
	Status            string     `json:"status,omitempty"`
	Title             string     `json:"title,omitempty"`
	GrantedAt         *time.Time `json:"granted_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

type ResetCredits struct {
	AvailableCount                 int           `json:"available_count"`
	TotalEarnedCount               int           `json:"total_earned_count,omitempty"`
	ImmediateResetPurchaseEligible bool          `json:"immediate_reset_purchase_eligible,omitempty"`
	NextExpiresAt                  *time.Time    `json:"next_expires_at,omitempty"`
	Credits                        []ResetCredit `json:"credits,omitempty"`
}

func (r *ResetCredits) HasAvailable(now time.Time) bool {
	if r == nil {
		return false
	}
	if r.NextExpiringCredit(now) != nil {
		return true
	}
	if len(r.Credits) == 0 && r.AvailableCount > 0 {
		return r.NextExpiresAt == nil || r.NextExpiresAt.After(now)
	}
	return false
}

func (r *ResetCredits) NextExpiringCredit(now time.Time) *ResetCredit {
	if r == nil {
		return nil
	}
	var best *ResetCredit
	for i := range r.Credits {
		c := &r.Credits[i]
		if !strings.EqualFold(strings.TrimSpace(c.Status), "available") {
			continue
		}
		if c.ExpiresAt != nil && !c.ExpiresAt.After(now) {
			continue
		}
		if best == nil {
			best = c
			continue
		}
		if c.IsSupportedByPlan != best.IsSupportedByPlan {
			if c.IsSupportedByPlan {
				best = c
			}
			continue
		}
		if c.ExpiresAt != nil && (best.ExpiresAt == nil || c.ExpiresAt.Before(*best.ExpiresAt)) {
			best = c
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

type ConsumeResetResult struct {
	Outcome      string   `json:"outcome"`
	CreditID     string   `json:"credit_id,omitempty"`
	WindowsReset []string `json:"windows_reset,omitempty"`
}

func (r *ConsumeResetResult) Redeemed() bool {
	if r == nil {
		return false
	}
	return r.Outcome == "reset" || r.Outcome == "already_redeemed"
}

type QuotaSnapshot struct {
	Plan             string        `json:"plan,omitempty"`
	FiveHour         *Window       `json:"five_hour,omitempty"`
	Weekly           *Window       `json:"weekly,omitempty"`
	HasCredits       bool          `json:"has_credits,omitempty"`
	UnlimitedCredits bool          `json:"unlimited_credits,omitempty"`
	CreditBalance    string        `json:"credit_balance,omitempty"`
	ResetCredits     *ResetCredits `json:"reset_credits,omitempty"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type Window struct {
	RemainingPercent int        `json:"remaining_percent"`
	UsedPercent      float64    `json:"used_percent"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	WindowSeconds    int64      `json:"window_seconds,omitempty"`
}

func (q QuotaSnapshot) Exhausted() bool {
	if q.UnlimitedCredits {
		return false
	}
	if q.FiveHour != nil && q.FiveHour.RemainingPercent <= 0 {
		return !q.hasPositiveCreditBalance()
	}
	if q.Weekly != nil && q.Weekly.RemainingPercent <= 0 {
		return !q.hasPositiveCreditBalance()
	}
	return false
}

func (q QuotaSnapshot) CandidateAvailable() bool {
	if q.UnlimitedCredits || q.hasPositiveCreditBalance() {
		return true
	}
	if q.FiveHour == nil && q.Weekly == nil {
		return false
	}
	if q.FiveHour != nil && q.FiveHour.RemainingPercent < 2 {
		return false
	}
	if q.Weekly != nil && q.Weekly.RemainingPercent < 2 {
		return false
	}
	return true
}

func (q QuotaSnapshot) MinRemainingPercent() int {
	minPct := 100
	has := false
	if q.FiveHour != nil {
		minPct = q.FiveHour.RemainingPercent
		has = true
	}
	if q.Weekly != nil && (!has || q.Weekly.RemainingPercent < minPct) {
		minPct = q.Weekly.RemainingPercent
		has = true
	}
	if !has {
		return 0
	}
	return minPct
}

// NeedsIgnite reports whether a subscription Codex account has an untouched
// 100% rolling window whose refresh timer has not started ticking yet.
func (q QuotaSnapshot) NeedsIgnite(now time.Time) (bool, string) {
	if strings.EqualFold(strings.TrimSpace(q.Plan), "api_key") || q.Exhausted() {
		return false, ""
	}
	need5h := codexWindowNeedsIgnite(q.FiveHour, 5*time.Hour, now)
	needWeekly := codexWindowNeedsIgnite(q.Weekly, 7*24*time.Hour, now)
	switch {
	case need5h && needWeekly:
		return true, "5h+weekly"
	case need5h:
		return true, "5h"
	case needWeekly:
		return true, "weekly"
	default:
		return false, ""
	}
}

func codexWindowNeedsIgnite(w *Window, defaultDur time.Duration, now time.Time) bool {
	if w == nil || w.UsedPercent > 0 || w.RemainingPercent < 100 {
		return false
	}
	dur := defaultDur
	if w.WindowSeconds > 0 {
		dur = time.Duration(w.WindowSeconds) * time.Second
	}
	if w.ResetsAt != nil && w.ResetsAt.Sub(now) < dur-codexIgniteResetSlack {
		return false
	}
	return true
}

func (q QuotaSnapshot) hasPositiveCreditBalance() bool {
	if !q.HasCredits {
		return false
	}
	bal := strings.TrimSpace(q.CreditBalance)
	if bal == "" || bal == "0" || bal == "0.0" || bal == "0.00" {
		return false
	}
	return !strings.HasPrefix(bal, "-")
}

func (q QuotaSnapshot) ToUsageSnapshot() usage.Snapshot {
	snap := usage.Snapshot{
		Plan:      q.Plan,
		UpdatedAt: q.UpdatedAt,
	}
	if q.FiveHour != nil {
		snap.FiveHour = &usage.Window{
			RemainingPercent: q.FiveHour.RemainingPercent,
			ResetsAt:         q.FiveHour.ResetsAt,
			Group:            "Codex Session",
		}
	}
	if q.Weekly != nil {
		group := "Codex Weekly"
		if q.Plan == "api_key" && strings.TrimSpace(q.CreditBalance) != "" {
			group = "Credits " + strings.TrimSpace(q.CreditBalance)
		}
		snap.Weekly = &usage.Window{
			RemainingPercent: q.Weekly.RemainingPercent,
			ResetsAt:         q.Weekly.ResetsAt,
			Group:            group,
		}
	}
	return snap
}

type UsageClient struct {
	UsageURL              string
	ResetCreditsURL       string
	ConsumeResetCreditURL string
	AccountsCheckURL      string
	SubscriptionsURL      string
	WarmupURL             string
	APIWarmupURL          string
	OpenAIBaseURL         string
	OAuth                 OAuthConfig
	HTTPClient            *http.Client
	Now                   func() time.Time

	mu sync.Mutex
}

func NewUsageClient() *UsageClient {
	return &UsageClient{
		UsageURL:              DefaultUsageURL,
		ResetCreditsURL:       DefaultResetCreditsURL,
		ConsumeResetCreditURL: DefaultConsumeResetCreditURL,
		AccountsCheckURL:      DefaultAccountsCheckURL,
		SubscriptionsURL:      DefaultSubscriptionsURL,
		WarmupURL:             DefaultWarmupURL,
		APIWarmupURL:          DefaultAPIWarmupURL,
		OpenAIBaseURL:         DefaultOpenAIBaseURL,
		OAuth:                 DefaultOAuthConfig(),
		HTTPClient:            &http.Client{Timeout: 12 * time.Second},
		Now:                   time.Now,
	}
}

func (c *UsageClient) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now().Round(0)
	}
	return time.Now().Round(0)
}

func (c *UsageClient) FetchUsage(ctx context.Context, store *Store, account StoredAccount, activeRunning bool) (QuotaSnapshot, error) {
	return c.fetchUsage(ctx, store, account, activeRunning, false)
}

func (c *UsageClient) FetchUsageWithResets(ctx context.Context, store *Store, account StoredAccount, activeRunning bool) (QuotaSnapshot, error) {
	return c.fetchUsage(ctx, store, account, activeRunning, true)
}

func (c *UsageClient) fetchUsage(ctx context.Context, store *Store, account StoredAccount, activeRunning, includeResets bool) (QuotaSnapshot, error) {
	now := c.now()
	if account.AuthMode == AuthModeAPIKey {
		return c.fetchAPIKeyCreditSnapshot(ctx, store, account, now), nil
	}
	if strings.TrimSpace(account.AuthData.AccessToken) == "" {
		return QuotaSnapshot{}, usage.ErrMissingAuth
	}

	if !activeRunning && NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		refreshed, err := c.refreshAndPersist(ctx, store, account.ID)
		if err != nil {
			return QuotaSnapshot{}, err
		}
		account = *refreshed
	}

	var (
		snap   QuotaSnapshot
		status int
		err    error
		resets *ResetCredits
	)
	if includeResets && c.resetCreditsEndpoint() != "" {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			snap, status, err = c.requestUsage(ctx, account, now)
		}()
		go func() {
			defer wg.Done()
			if rc, _, rErr := c.requestResetCredits(ctx, account); rErr == nil {
				resets = rc
			}
		}()
		wg.Wait()
	} else {
		snap, status, err = c.requestUsage(ctx, account, now)
	}

	if status == http.StatusUnauthorized && !activeRunning && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, rErr := c.refreshAndPersist(ctx, store, account.ID); rErr == nil {
			account = *refreshed
			snap, _, err = c.requestUsage(ctx, account, now)
			if err == nil && includeResets && c.resetCreditsEndpoint() != "" {
				if rc, _, rcErr := c.requestResetCredits(ctx, account); rcErr == nil {
					resets = rc
				}
			}
		}
	}
	if err == nil && resets != nil {
		snap.ResetCredits = resets
	}
	return snap, err
}

func (c *UsageClient) resetCreditsEndpoint() string {
	if c == nil {
		return DefaultResetCreditsURL
	}
	ep := strings.TrimSpace(c.ResetCreditsURL)
	if ep != "" && ep != DefaultResetCreditsURL {
		return ep
	}
	// When UsageURL is pointed at a test server without an explicit ResetCreditsURL,
	// avoid hitting real chatgpt.com from unit tests.
	if u := strings.TrimSpace(c.UsageURL); u != "" && u != DefaultUsageURL {
		return ""
	}
	return DefaultResetCreditsURL
}

func (c *UsageClient) FetchResetCredits(ctx context.Context, account StoredAccount) (*ResetCredits, error) {
	if account.AuthMode == AuthModeAPIKey {
		return nil, nil
	}
	if strings.TrimSpace(account.AuthData.AccessToken) == "" {
		return nil, usage.ErrMissingAuth
	}
	rc, _, err := c.requestResetCredits(ctx, account)
	return rc, err
}

func (c *UsageClient) consumeResetCreditEndpoint() string {
	if c == nil {
		return DefaultConsumeResetCreditURL
	}
	if ep := strings.TrimSpace(c.ConsumeResetCreditURL); ep != "" && ep != DefaultConsumeResetCreditURL {
		return ep
	}
	if ep := strings.TrimSpace(c.ResetCreditsURL); ep != "" && ep != DefaultResetCreditsURL {
		return strings.TrimRight(ep, "/") + "/consume"
	}
	if u := strings.TrimSpace(c.UsageURL); u != "" && u != DefaultUsageURL {
		return ""
	}
	return DefaultConsumeResetCreditURL
}

func newRedeemRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("qs-reset-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *UsageClient) ConsumeResetCredit(ctx context.Context, store *Store, account StoredAccount, creditID string, activeRunning bool) (*ConsumeResetResult, error) {
	if account.AuthMode == AuthModeAPIKey {
		return nil, errors.New("API-key accounts do not support rate-limit reset credits")
	}
	endpoint := c.consumeResetCreditEndpoint()
	if endpoint == "" {
		return nil, errors.New("reset credit consume endpoint is not configured")
	}
	now := c.now()
	if !activeRunning && NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		refreshed, err := c.refreshAndPersist(ctx, store, account.ID)
		if err != nil {
			return nil, err
		}
		account = *refreshed
	}
	if strings.TrimSpace(account.AuthData.AccessToken) == "" {
		return nil, usage.ErrMissingAuth
	}

	reqID := newRedeemRequestID()
	res, status, err := c.requestConsumeResetCredit(ctx, endpoint, account, strings.TrimSpace(creditID), reqID)
	if status == http.StatusUnauthorized && !activeRunning && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, rErr := c.refreshAndPersist(ctx, store, account.ID); rErr == nil {
			account = *refreshed
			res, _, err = c.requestConsumeResetCredit(ctx, endpoint, account, strings.TrimSpace(creditID), reqID)
		}
	}
	return res, err
}

func (c *UsageClient) requestConsumeResetCredit(ctx context.Context, endpoint string, account StoredAccount, creditID, redeemRequestID string) (*ConsumeResetResult, int, error) {
	payload := map[string]any{
		"redeem_request_id": redeemRequestID,
	}
	if creditID != "" {
		payload["credit_id"] = creditID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	applyChatGPTHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.StatusCode, usage.ErrAuth
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, usage.ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return nil, resp.StatusCode, fmt.Errorf("%w: status %d: %s", usage.ErrHTTP, resp.StatusCode, msg)
		}
		return nil, resp.StatusCode, fmt.Errorf("%w: status %d", usage.ErrHTTP, resp.StatusCode)
	}
	res, err := parseConsumeResetCredit(body, creditID)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return res, resp.StatusCode, nil
}

func normalizeConsumeOutcome(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "reset":
		return "reset"
	case "already_redeemed", "alreadyredeemed":
		return "already_redeemed"
	case "nothing_to_reset", "nothingtoreset":
		return "nothing_to_reset"
	case "no_credit", "nocredit":
		return "no_credit"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func parseConsumeResetCredit(raw []byte, requestedCreditID string) (*ConsumeResetResult, error) {
	var p struct {
		Code         string   `json:"code"`
		Status       string   `json:"status"`
		Outcome      string   `json:"outcome"`
		CreditID     string   `json:"credit_id"`
		WindowsReset []string `json:"windows_reset"`
		Credit       *struct {
			ID string `json:"id"`
		} `json:"credit"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", usage.ErrInvalidResponse, err)
	}
	outcome := p.Code
	if strings.TrimSpace(outcome) == "" {
		outcome = p.Status
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = p.Outcome
	}
	outcome = normalizeConsumeOutcome(outcome)
	if outcome == "" {
		return nil, fmt.Errorf("%w: missing consume outcome", usage.ErrInvalidResponse)
	}
	cid := strings.TrimSpace(p.CreditID)
	if p.Credit != nil && strings.TrimSpace(p.Credit.ID) != "" {
		cid = strings.TrimSpace(p.Credit.ID)
	}
	if cid == "" {
		cid = requestedCreditID
	}
	return &ConsumeResetResult{
		Outcome:      outcome,
		CreditID:     cid,
		WindowsReset: p.WindowsReset,
	}, nil
}

func (c *UsageClient) requestResetCredits(ctx context.Context, account StoredAccount) (*ResetCredits, int, error) {
	endpoint := c.resetCreditsEndpoint()
	if endpoint == "" {
		return nil, 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	applyChatGPTHeaders(req, account)

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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
	rc, err := parseWhamResetCredits(body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return rc, resp.StatusCode, nil
}

type whamResetCreditItem struct {
	ID                string  `json:"id"`
	ResetType         string  `json:"reset_type"`
	IsSupportedByPlan bool    `json:"is_supported_by_plan"`
	Status            string  `json:"status"`
	Title             string  `json:"title"`
	GrantedAt         *string `json:"granted_at"`
	ExpiresAt         *string `json:"expires_at"`
}

type whamResetCreditsPayload struct {
	AvailableCount                 int                   `json:"available_count"`
	TotalEarnedCount               int                   `json:"total_earned_count"`
	ImmediateResetPurchaseEligible bool                  `json:"immediate_reset_purchase_eligible"`
	Credits                        []whamResetCreditItem `json:"credits"`
}

func parseWhamResetCredits(raw []byte) (*ResetCredits, error) {
	var p whamResetCreditsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", usage.ErrInvalidResponse, err)
	}
	out := &ResetCredits{
		AvailableCount:                 p.AvailableCount,
		TotalEarnedCount:               p.TotalEarnedCount,
		ImmediateResetPurchaseEligible: p.ImmediateResetPurchaseEligible,
	}
	parseTS := func(s *string) *time.Time {
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
	for _, item := range p.Credits {
		rc := ResetCredit{
			ID:                item.ID,
			ResetType:         item.ResetType,
			IsSupportedByPlan: item.IsSupportedByPlan,
			Status:            item.Status,
			Title:             item.Title,
			GrantedAt:         parseTS(item.GrantedAt),
			ExpiresAt:         parseTS(item.ExpiresAt),
		}
		out.Credits = append(out.Credits, rc)
		if strings.EqualFold(item.Status, "available") && rc.ExpiresAt != nil {
			if out.NextExpiresAt == nil || rc.ExpiresAt.Before(*out.NextExpiresAt) {
				t := *rc.ExpiresAt
				out.NextExpiresAt = &t
			}
		}
	}
	return out, nil
}

func (c *UsageClient) fetchAPIKeyCreditSnapshot(ctx context.Context, store *Store, account StoredAccount, now time.Time) QuotaSnapshot {
	snap := QuotaSnapshot{
		Plan:      "api_key",
		UpdatedAt: now,
	}
	if store == nil {
		return snap
	}
	ledger, err := store.LoadCreditLedger()
	if err != nil {
		return snap
	}
	entry, ok := ledger.Lookup(account)

	if account.AuthData.AdminKey != nil && strings.TrimSpace(*account.AuthData.AdminKey) != "" {
		stale := !ok || entry.LastSyncedAt == nil || now.Sub(*entry.LastSyncedAt) >= 60*time.Second
		if stale {
			if updated, sErr := c.SyncOrgCosts(ctx, store, account, now); sErr == nil {
				entry = updated
				ok = true
			}
		}
	}

	if !ok || entry.GrantUSD <= 0 {
		return snap
	}
	rem := entry.RemainingUSD(now, account.SubscriptionExpiresAt)
	pct := entry.RemainingPercent(now, account.SubscriptionExpiresAt)
	snap.HasCredits = rem > 0.004
	snap.CreditBalance = FormatCreditGrantLabel(rem, entry.GrantUSD)
	snap.Weekly = &Window{
		RemainingPercent: pct,
		UsedPercent:      float64(100 - pct),
		ResetsAt:         account.SubscriptionExpiresAt,
	}
	return snap
}

func (c *UsageClient) SyncOrgCosts(ctx context.Context, store *Store, account StoredAccount, now time.Time) (CreditEntry, error) {
	if account.AuthData.AdminKey == nil || strings.TrimSpace(*account.AuthData.AdminKey) == "" {
		return CreditEntry{}, errors.New("empty OpenAI admin key")
	}
	startTime := now.Add(-60 * 24 * time.Hour).Unix()
	if !account.CreatedAt.IsZero() && account.CreatedAt.Add(-30*24*time.Hour).Unix() < startTime {
		startTime = account.CreatedAt.Add(-30 * 24 * time.Hour).Unix()
	}
	baseURL := DefaultOpenAIBaseURL
	var client *http.Client
	if c != nil {
		if strings.TrimSpace(c.OpenAIBaseURL) != "" {
			baseURL = c.OpenAIBaseURL
		}
		client = c.HTTPClient
	}
	used, err := FetchOpenAIOrgCosts(ctx, client, baseURL, *account.AuthData.AdminKey, startTime)
	if err != nil {
		return CreditEntry{}, err
	}
	return store.CalibrateCredit(account, CreditCalibration{
		UsedUSD:      &used,
		ResetMetered: true,
		Synced:       true,
	}, now)
}

func (c *UsageClient) refreshAndPersist(ctx context.Context, store *Store, accountID string) (*StoredAccount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if store == nil {
		return nil, errors.New("missing Codex store")
	}
	data, err := store.Load()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i := range data.Accounts {
		if data.Accounts[i].ID == accountID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNotFound
	}
	acct := &data.Accounts[idx]
	tokens, err := RefreshTokens(ctx, c.OAuth, acct.AuthData.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", usage.ErrRefresh, err)
	}
	acct.AuthData.AccessToken = tokens.AccessToken
	acct.AuthData.RefreshToken = tokens.RefreshToken
	if tokens.IDToken != "" {
		acct.AuthData.IDToken = tokens.IDToken
	}
	if tokens.AccountID != nil && strings.TrimSpace(*tokens.AccountID) != "" {
		acct.AuthData.AccountID = tokens.AccountID
	}
	claims := ParseIDTokenClaims(acct.AuthData.IDToken)
	if claims.PlanType != "" {
		v := claims.PlanType
		acct.PlanType = &v
	}
	if claims.SubscriptionExpiresAt != nil {
		acct.SubscriptionExpiresAt = claims.SubscriptionExpiresAt
	}
	if err := store.Save(data); err != nil {
		return nil, err
	}
	if data.ActiveAccountID != nil && *data.ActiveAccountID == acct.ID {
		_ = store.WriteLiveAuth(*acct, c.now())
	}
	cp := *acct
	return &cp, nil
}

func (c *UsageClient) requestUsage(ctx context.Context, account StoredAccount, now time.Time) (QuotaSnapshot, int, error) {
	endpoint := c.UsageURL
	if endpoint == "" {
		endpoint = DefaultUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return QuotaSnapshot{}, 0, err
	}
	applyChatGPTHeaders(req, account)

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return QuotaSnapshot{}, 0, usage.ErrTimeout
		}
		return QuotaSnapshot{}, 0, usage.ErrNetwork
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return QuotaSnapshot{}, resp.StatusCode, usage.ErrAuth
	}
	if resp.StatusCode == http.StatusForbidden {
		return QuotaSnapshot{}, resp.StatusCode, usage.ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QuotaSnapshot{}, resp.StatusCode, fmt.Errorf("%w: status %d", usage.ErrHTTP, resp.StatusCode)
	}

	snap, err := parseWhamUsage(body, account.PlanString(), now)
	if err != nil {
		return QuotaSnapshot{}, resp.StatusCode, err
	}
	return snap, resp.StatusCode, nil
}

func applyChatGPTHeaders(req *http.Request, account StoredAccount) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(account.AuthData.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", browserUserAgent)
	acctID := ""
	if account.AuthData.AccountID != nil {
		acctID = strings.TrimSpace(*account.AuthData.AccountID)
	}
	if acctID == "" {
		acctID = ParseIDTokenClaims(account.AuthData.IDToken).ChatGPTAccountID
	}
	if acctID != "" {
		req.Header.Set("chatgpt-account-id", acctID)
	}
}

type whamRateWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type whamPayload struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		PrimaryWindow   *whamRateWindow `json:"primary_window"`
		SecondaryWindow *whamRateWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool `json:"has_credits"`
		Unlimited  bool `json:"unlimited"`
		Balance    any  `json:"balance"`
	} `json:"credits"`
}

func parseWhamUsage(raw []byte, fallbackPlan string, now time.Time) (QuotaSnapshot, error) {
	var p whamPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return QuotaSnapshot{}, fmt.Errorf("%w: %v", usage.ErrInvalidResponse, err)
	}
	plan := strings.TrimSpace(p.PlanType)
	if plan == "" {
		plan = fallbackPlan
	}
	snap := QuotaSnapshot{
		Plan:      plan,
		UpdatedAt: now.UTC(),
	}
	if p.RateLimit != nil {
		assignWindow := func(w *whamRateWindow, defaultWeekly bool) {
			if w == nil {
				return
			}
			rem := 100 - int(math.Round(w.UsedPercent))
			if rem < 0 {
				rem = 0
			}
			if rem > 100 {
				rem = 100
			}
			var resetPtr *time.Time
			if w.ResetAt > 0 {
				t := time.Unix(w.ResetAt, 0).UTC()
				resetPtr = &t
			}
			win := &Window{
				RemainingPercent: rem,
				UsedPercent:      w.UsedPercent,
				ResetsAt:         resetPtr,
				WindowSeconds:    w.LimitWindowSeconds,
			}
			switch {
			case w.LimitWindowSeconds >= 86400:
				snap.Weekly = win
			case w.LimitWindowSeconds > 0 && w.LimitWindowSeconds < 86400:
				snap.FiveHour = win
			case defaultWeekly:
				snap.Weekly = win
			default:
				snap.FiveHour = win
			}
		}
		assignWindow(p.RateLimit.PrimaryWindow, false)
		assignWindow(p.RateLimit.SecondaryWindow, true)
	}
	if p.Credits != nil {
		snap.HasCredits = p.Credits.HasCredits
		snap.UnlimitedCredits = p.Credits.Unlimited
		if p.Credits.Balance != nil {
			snap.CreditBalance = fmt.Sprintf("%v", p.Credits.Balance)
		}
	}
	return snap, nil
}

func (c *UsageClient) Warmup(ctx context.Context, store *Store, account StoredAccount) error {
	return c.WarmupAccount(ctx, store, account, false)
}

func (c *UsageClient) WarmupAccount(ctx context.Context, store *Store, account StoredAccount, activeRunning bool) error {
	now := c.now()
	var endpoint string
	model := "gpt-6.2-astra"
	if account.AuthMode == AuthModeAPIKey {
		model = "gpt-5.4"
	}
	bodyMap := map[string]any{
		"model": model,
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "hi"},
				},
			},
		},
		"store": false,
	}

	if account.AuthMode == AuthModeAPIKey {
		if account.AuthData.Key == nil || strings.TrimSpace(*account.AuthData.Key) == "" {
			return usage.ErrMissingAuth
		}
		endpoint = c.APIWarmupURL
		if endpoint == "" {
			endpoint = DefaultAPIWarmupURL
		}
	} else {
		if !activeRunning && NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
			if refreshed, err := c.refreshAndPersist(ctx, store, account.ID); err == nil {
				account = *refreshed
			}
		}
		if strings.TrimSpace(account.AuthData.AccessToken) == "" {
			return usage.ErrMissingAuth
		}
		endpoint = c.WarmupURL
		if endpoint == "" {
			endpoint = DefaultWarmupURL
		}
		bodyMap["instructions"] = "Reply with ok."
		bodyMap["reasoning"] = map[string]any{"effort": "low"}
		bodyMap["stream"] = true
	}

	encoded, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	status, err := c.doWarmupRequest(ctx, endpoint, account, encoded)
	if status == http.StatusUnauthorized && account.AuthMode != AuthModeAPIKey && !activeRunning && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, rErr := c.refreshAndPersist(ctx, store, account.ID); rErr == nil {
			account = *refreshed
			_, err = c.doWarmupRequest(ctx, endpoint, account, encoded)
		}
	}
	return err
}

func (c *UsageClient) doWarmupRequest(ctx context.Context, endpoint string, account StoredAccount, encoded []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if account.AuthMode == AuthModeAPIKey {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*account.AuthData.Key))
	} else {
		applyChatGPTHeaders(req, account)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("User-Agent", codexCLIUserAgent)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(bodyBytes))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return resp.StatusCode, fmt.Errorf("Codex warm-up failed (HTTP %d): %s", resp.StatusCode, msg)
		}
		return resp.StatusCode, fmt.Errorf("Codex warm-up failed (HTTP %d)", resp.StatusCode)
	}
	if errMsg := warmupStreamError(bodyBytes); errMsg != "" {
		return resp.StatusCode, fmt.Errorf("Codex warm-up failed: %s", errMsg)
	}
	return resp.StatusCode, nil
}

func warmupStreamError(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		evType, _ := ev["type"].(string)
		if evType != "error" && evType != "response.failed" && ev["error"] == nil {
			continue
		}
		details := ev
		if respObj, ok := ev["response"].(map[string]any); ok {
			details = respObj
		}
		if errObj, ok := details["error"].(map[string]any); ok {
			details = errObj
		}
		if msg, ok := details["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
		if evType != "" {
			return evType
		}
		return "stream error"
	}
	return ""
}
