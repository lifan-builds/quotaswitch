package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/backup"
)

const DefaultOpenAIBaseURL = "https://api.openai.com"

var creditLedgerMu sync.Mutex

// CreditLedger persists zero-browser OpenAI API credit grants, baseline usage,
// and live gateway token/multimodal metering in <SwitcherDir>/credit_ledger.json.
type CreditLedger struct {
	Version  int                    `json:"version"`
	Accounts map[string]CreditEntry `json:"accounts"`
}

type CreditEntry struct {
	AccountID       string     `json:"account_id"`
	Alias           string     `json:"alias,omitempty"`
	GrantUSD        float64    `json:"grant_usd"`
	BaselineUsedUSD float64    `json:"baseline_used_usd"`
	MeteredUSD      float64    `json:"metered_usd"`
	Requests        int64      `json:"requests"`
	InputTokens     int64      `json:"input_tokens"`
	CachedTokens    int64      `json:"cached_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	EmbeddingTokens int64      `json:"embedding_tokens,omitempty"`
	Images          int64      `json:"images,omitempty"`
	AudioChars      int64      `json:"audio_chars,omitempty"`
	Exhausted       bool       `json:"exhausted,omitempty"`
	LastModel       string     `json:"last_model,omitempty"`
	LastSyncedAt    *time.Time `json:"last_synced_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type CreditCallDelta struct {
	Model           string
	CostUSD         float64
	Requests        int64
	InputTokens     int64
	CachedTokens    int64
	OutputTokens    int64
	EmbeddingTokens int64
	Images          int64
	AudioChars      int64
}

type CreditCalibration struct {
	GrantUSD     *float64
	UsedUSD      *float64
	RemainingUSD *float64
	ResetMetered bool
	Exhausted    *bool
	Synced       bool
}

func (e CreditEntry) UsedUSD() float64 {
	used := e.BaselineUsedUSD + e.MeteredUSD
	if used < 0 {
		used = 0
	}
	if e.Exhausted && e.GrantUSD > used {
		return e.GrantUSD
	}
	return used
}

func (e CreditEntry) RemainingUSD(now time.Time, expiresAt *time.Time) float64 {
	if e.Exhausted || e.GrantUSD <= 0 {
		return 0
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return 0
	}
	rem := e.GrantUSD - (e.BaselineUsedUSD + e.MeteredUSD)
	if rem < 0 {
		return 0
	}
	return rem
}

func (e CreditEntry) RemainingPercent(now time.Time, expiresAt *time.Time) int {
	if e.GrantUSD <= 0 {
		return 0
	}
	rem := e.RemainingUSD(now, expiresAt)
	pct := int(math.Round((rem / e.GrantUSD) * 100))
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func FormatCreditGrantLabel(rem, grant float64) string {
	if math.Abs(grant-math.Round(grant)) < 0.005 {
		return fmt.Sprintf("$%.2f/$%.0f", rem, grant)
	}
	return fmt.Sprintf("$%.2f/$%.2f", rem, grant)
}

func (l *CreditLedger) Lookup(acct StoredAccount) (CreditEntry, bool) {
	if l == nil || len(l.Accounts) == 0 {
		return CreditEntry{}, false
	}
	if acct.ID != "" {
		if entry, ok := l.Accounts[acct.ID]; ok {
			return entry, true
		}
	}
	if acct.Name != "" {
		if entry, ok := l.Accounts[acct.Name]; ok {
			return entry, true
		}
	}
	for _, entry := range l.Accounts {
		if acct.ID != "" && strings.EqualFold(entry.AccountID, acct.ID) {
			return entry, true
		}
		if acct.Name != "" && strings.EqualFold(entry.Alias, acct.Name) {
			return entry, true
		}
	}
	return CreditEntry{}, false
}

func (l *CreditLedger) upsert(acct StoredAccount, entry CreditEntry) {
	if l.Accounts == nil {
		l.Accounts = make(map[string]CreditEntry)
	}
	key := acct.ID
	if key == "" {
		key = acct.Name
	}
	if acct.Name != "" && acct.Name != key {
		delete(l.Accounts, acct.Name)
	}
	entry.AccountID = acct.ID
	if acct.Name != "" {
		entry.Alias = acct.Name
	}
	l.Accounts[key] = entry
}

func (s *Store) LoadCreditLedger() (*CreditLedger, error) {
	if s == nil {
		return &CreditLedger{Version: 1, Accounts: map[string]CreditEntry{}}, nil
	}
	creditLedgerMu.Lock()
	defer creditLedgerMu.Unlock()
	return s.loadCreditLedgerUnlocked()
}

func (s *Store) loadCreditLedgerUnlocked() (*CreditLedger, error) {
	raw, err := os.ReadFile(s.CreditLedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return &CreditLedger{Version: 1, Accounts: map[string]CreditEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var ledger CreditLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("invalid Codex credit_ledger.json: %w", err)
	}
	if ledger.Version == 0 {
		ledger.Version = 1
	}
	if ledger.Accounts == nil {
		ledger.Accounts = map[string]CreditEntry{}
	}
	return &ledger, nil
}

func (s *Store) saveCreditLedgerUnlocked(ledger *CreditLedger) error {
	if ledger.Version == 0 {
		ledger.Version = 1
	}
	if ledger.Accounts == nil {
		ledger.Accounts = map[string]CreditEntry{}
	}
	encoded, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return backup.WriteFileAtomic(s.CreditLedgerPath(), encoded, 0o600)
}

func (s *Store) RecordCreditCall(acct StoredAccount, delta CreditCallDelta, now time.Time) error {
	if s == nil {
		return errors.New("missing Codex store")
	}
	creditLedgerMu.Lock()
	defer creditLedgerMu.Unlock()

	ledger, err := s.loadCreditLedgerUnlocked()
	if err != nil {
		return err
	}
	entry, _ := ledger.Lookup(acct)
	reqs := delta.Requests
	if reqs <= 0 {
		reqs = 1
	}
	entry.Requests += reqs
	entry.MeteredUSD += math.Max(0, delta.CostUSD)
	entry.InputTokens += delta.InputTokens
	entry.CachedTokens += delta.CachedTokens
	entry.OutputTokens += delta.OutputTokens
	entry.EmbeddingTokens += delta.EmbeddingTokens
	entry.Images += delta.Images
	entry.AudioChars += delta.AudioChars
	if strings.TrimSpace(delta.Model) != "" {
		entry.LastModel = strings.TrimSpace(delta.Model)
	}
	entry.UpdatedAt = now.UTC()
	ledger.upsert(acct, entry)
	return s.saveCreditLedgerUnlocked(ledger)
}

func (s *Store) MarkCreditExhausted(acct StoredAccount, now time.Time) error {
	if s == nil {
		return errors.New("missing Codex store")
	}
	creditLedgerMu.Lock()
	defer creditLedgerMu.Unlock()

	ledger, err := s.loadCreditLedgerUnlocked()
	if err != nil {
		return err
	}
	entry, _ := ledger.Lookup(acct)
	entry.Exhausted = true
	entry.UpdatedAt = now.UTC()
	ledger.upsert(acct, entry)
	return s.saveCreditLedgerUnlocked(ledger)
}

func (s *Store) CalibrateCredit(acct StoredAccount, cal CreditCalibration, now time.Time) (CreditEntry, error) {
	if s == nil {
		return CreditEntry{}, errors.New("missing Codex store")
	}
	creditLedgerMu.Lock()
	defer creditLedgerMu.Unlock()

	ledger, err := s.loadCreditLedgerUnlocked()
	if err != nil {
		return CreditEntry{}, err
	}
	entry, _ := ledger.Lookup(acct)
	if cal.GrantUSD != nil {
		entry.GrantUSD = math.Max(0, *cal.GrantUSD)
	}
	if cal.ResetMetered {
		entry.MeteredUSD = 0
	}
	if cal.UsedUSD != nil {
		entry.BaselineUsedUSD = math.Max(0, *cal.UsedUSD)
		if cal.ResetMetered {
			entry.MeteredUSD = 0
		}
	} else if cal.RemainingUSD != nil && entry.GrantUSD > 0 {
		rem := math.Max(0, *cal.RemainingUSD)
		used := math.Max(0, entry.GrantUSD-rem)
		if cal.ResetMetered {
			entry.BaselineUsedUSD = used
			entry.MeteredUSD = 0
		} else {
			entry.BaselineUsedUSD = math.Max(0, used-entry.MeteredUSD)
		}
	}
	if cal.Exhausted != nil {
		entry.Exhausted = *cal.Exhausted
	} else if cal.RemainingUSD != nil && *cal.RemainingUSD > 0.005 {
		entry.Exhausted = false
	} else if cal.UsedUSD != nil && entry.GrantUSD > 0 && *cal.UsedUSD+0.005 < entry.GrantUSD {
		entry.Exhausted = false
	}
	ts := now.UTC()
	if cal.Synced {
		entry.LastSyncedAt = &ts
		if entry.GrantUSD > 0 && entry.BaselineUsedUSD+0.005 >= entry.GrantUSD {
			entry.Exhausted = true
		}
	}
	entry.UpdatedAt = ts
	ledger.upsert(acct, entry)
	if err := s.saveCreditLedgerUnlocked(ledger); err != nil {
		return CreditEntry{}, err
	}
	return entry, nil
}

// EstimateTokenCostUSD estimates OpenAI API USD spend from prompt, cached, and completion tokens.
func EstimateTokenCostUSD(model string, inputTokens, cachedTokens, outputTokens int, isBatch bool) float64 {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	uncached := inputTokens - cachedTokens

	m := strings.ToLower(strings.TrimSpace(model))
	var inPerM, cachedPerM, outPerM float64
	switch {
	case strings.Contains(m, "nano") || strings.Contains(m, "4o-mini"):
		inPerM, cachedPerM, outPerM = 0.15, 0.0375, 0.60
	case strings.Contains(m, "mini"):
		inPerM, cachedPerM, outPerM = 0.75, 0.1875, 3.00
	case strings.HasSuffix(m, "-pro") || strings.Contains(m, "-pro-"):
		inPerM, cachedPerM, outPerM = 15.00, 3.75, 60.00
	default:
		inPerM, cachedPerM, outPerM = 2.50, 0.625, 10.00
	}

	cost := (float64(uncached)*inPerM + float64(cachedTokens)*cachedPerM + float64(outputTokens)*outPerM) / 1_000_000.0
	if isBatch {
		cost *= 0.5
	}
	return cost
}

func EstimateEmbeddingCostUSD(model string, tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	m := strings.ToLower(strings.TrimSpace(model))
	perM := 0.13
	if strings.Contains(m, "small") {
		perM = 0.02
	}
	return (float64(tokens) * perM) / 1_000_000.0
}

func EstimateImageCostUSD(model, quality, size string, count int) float64 {
	if count <= 0 {
		count = 1
	}
	q := strings.ToLower(strings.TrimSpace(quality))
	sz := strings.ToLower(strings.TrimSpace(size))
	perImg := 0.17
	switch q {
	case "low":
		perImg = 0.015
	case "medium":
		perImg = 0.05
	default:
		if sz == "1536x1024" || sz == "1024x1536" || sz == "1792x1024" || sz == "1024x1792" {
			perImg = 0.20
		}
	}
	_ = model
	return float64(count) * perImg
}

func EstimateAudioSpeechCostUSD(model string, chars int) float64 {
	if chars <= 0 {
		return 0
	}
	m := strings.ToLower(strings.TrimSpace(model))
	perM := 15.0
	if strings.Contains(m, "tts-1-hd") {
		perM = 30.0
	}
	return (float64(chars) * perM) / 1_000_000.0
}

// FetchOpenAIOrgCosts queries OpenAI's official GET /v1/organization/costs endpoint
// using an optional sk-admin-... Admin API key (zero browser, ~150 ms).
func FetchOpenAIOrgCosts(ctx context.Context, client *http.Client, baseURL, adminKey string, startUnix int64) (float64, error) {
	key := strings.TrimSpace(adminKey)
	if key == "" {
		return 0, errors.New("empty OpenAI admin key")
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	var total float64
	page := ""
	for iter := 0; iter < 12; iter++ {
		q := url.Values{}
		q.Set("start_time", strconv.FormatInt(startUnix, 10))
		q.Set("limit", "180")
		if page != "" {
			q.Set("page", page)
		}
		endpoint := base + "/v1/organization/costs?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, fmt.Errorf("organization costs HTTP %d", resp.StatusCode)
		}

		var doc struct {
			Data []struct {
				Results []struct {
					Amount struct {
						Value any `json:"value"`
					} `json:"amount"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool   `json:"has_more"`
			NextPage string `json:"next_page"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, fmt.Errorf("decode organization costs: %w", err)
		}
		for _, bucket := range doc.Data {
			for _, res := range bucket.Results {
				switch v := res.Amount.Value.(type) {
				case float64:
					total += v
				case string:
					if parsed, pErr := strconv.ParseFloat(strings.TrimSpace(v), 64); pErr == nil {
						total += parsed
					}
				}
			}
		}
		if !doc.HasMore || strings.TrimSpace(doc.NextPage) == "" {
			break
		}
		page = strings.TrimSpace(doc.NextPage)
	}
	return math.Max(0, total), nil
}
