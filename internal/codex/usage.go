package codex

import (
	"bytes"
	"context"
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
	DefaultUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
	DefaultWarmupURL    = "https://chatgpt.com/backend-api/codex/responses"
	DefaultAPIWarmupURL = "https://api.openai.com/v1/responses"
	browserUserAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

type QuotaSnapshot struct {
	Plan             string    `json:"plan,omitempty"`
	FiveHour         *Window   `json:"five_hour,omitempty"`
	Weekly           *Window   `json:"weekly,omitempty"`
	HasCredits       bool      `json:"has_credits,omitempty"`
	UnlimitedCredits bool      `json:"unlimited_credits,omitempty"`
	CreditBalance    string    `json:"credit_balance,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
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
		snap.Weekly = &usage.Window{
			RemainingPercent: q.Weekly.RemainingPercent,
			ResetsAt:         q.Weekly.ResetsAt,
			Group:            "Codex Weekly",
		}
	}
	return snap
}

type UsageClient struct {
	UsageURL     string
	WarmupURL    string
	APIWarmupURL string
	OAuth        OAuthConfig
	HTTPClient   *http.Client
	Now          func() time.Time

	mu sync.Mutex
}

func NewUsageClient() *UsageClient {
	return &UsageClient{
		UsageURL:     DefaultUsageURL,
		WarmupURL:    DefaultWarmupURL,
		APIWarmupURL: DefaultAPIWarmupURL,
		OAuth:        DefaultOAuthConfig(),
		HTTPClient:   &http.Client{Timeout: 12 * time.Second},
		Now:          time.Now,
	}
}

func (c *UsageClient) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *UsageClient) FetchUsage(ctx context.Context, store *Store, account StoredAccount, activeRunning bool) (QuotaSnapshot, error) {
	now := c.now()
	if account.AuthMode == AuthModeAPIKey {
		return QuotaSnapshot{
			Plan:      "api_key",
			UpdatedAt: now,
		}, nil
	}
	if strings.TrimSpace(account.AuthData.AccessToken) == "" {
		return QuotaSnapshot{}, usage.ErrMissingAuth
	}

	if !activeRunning && NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, err := c.refreshAndPersist(ctx, store, account.ID); err == nil {
			account = *refreshed
		}
	}

	snap, status, err := c.requestUsage(ctx, account, now)
	if status == http.StatusUnauthorized && !activeRunning && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
		if refreshed, rErr := c.refreshAndPersist(ctx, store, account.ID); rErr == nil {
			snap, _, err = c.requestUsage(ctx, *refreshed, now)
		}
	}
	return snap, err
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
	now := c.now()
	var endpoint string
	bodyMap := map[string]any{
		"model": "gpt-5-codex",
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Thanks"},
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
		if NeedsRefresh(account.AuthData.IDToken, account.AuthData.AccessToken, now) && strings.TrimSpace(account.AuthData.RefreshToken) != "" {
			if refreshed, err := c.refreshAndPersist(ctx, store, account.ID); err == nil {
				account = *refreshed
			}
		}
		endpoint = c.WarmupURL
		if endpoint == "" {
			endpoint = DefaultWarmupURL
		}
		bodyMap["instructions"] = "Reply with OK."
		bodyMap["stream"] = true
	}

	encoded, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if account.AuthMode == AuthModeAPIKey {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*account.AuthData.Key))
	} else {
		applyChatGPTHeaders(req, account)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Codex warm-up failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}
