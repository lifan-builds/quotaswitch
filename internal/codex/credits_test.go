package codex

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestCreditLedgerAndOrgCostsSync(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "switcher"), filepath.Join(dir, "codex"))
	now := time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)
	exp := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)
	key := "test-api-key-alpha"
	email := "builder@example.com"

	acct := StoredAccount{
		ID:                    "acct-api-1",
		Name:                  "api-builder-exp1130",
		Email:                 &email,
		SubscriptionExpiresAt: &exp,
		AuthMode:              AuthModeAPIKey,
		AuthData:              AuthData{Type: AuthModeAPIKey, Key: &key},
		CreatedAt:             now,
	}

	grant := 120.0
	used := 10.0
	entry, err := store.CalibrateCredit(acct, CreditCalibration{
		GrantUSD:     &grant,
		UsedUSD:      &used,
		ResetMetered: true,
	}, now)
	if err != nil {
		t.Fatalf("CalibrateCredit: %v", err)
	}
	if math.Abs(entry.RemainingUSD(now, &exp)-110.0) > 1e-6 {
		t.Fatalf("remaining = %.4f, want 110.0", entry.RemainingUSD(now, &exp))
	}

	// Meter a call with 1M input (500k cached) and 200k output on gpt-5.4:
	// uncached 500k * $2.50/M = $1.25; cached 500k * $0.625/M = $0.3125; out 200k * $10/M = $2.00 => $3.5625
	cost := EstimateTokenCostUSD("gpt-5.4", 1_000_000, 500_000, 200_000, false)
	if math.Abs(cost-3.5625) > 1e-6 {
		t.Fatalf("EstimateTokenCostUSD = %.6f, want 3.5625", cost)
	}
	if err := store.RecordCreditCall(acct, CreditCallDelta{
		Model:        "gpt-5.4",
		CostUSD:      cost,
		Requests:     1,
		InputTokens:  1_000_000,
		CachedTokens: 500_000,
		OutputTokens: 200_000,
	}, now); err != nil {
		t.Fatalf("RecordCreditCall: %v", err)
	}

	client := NewUsageClient()
	client.Now = func() time.Time { return now }
	snap, err := client.FetchUsage(context.Background(), store, acct, false)
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if !snap.HasCredits || snap.CreditBalance != "$106.44/$120" {
		t.Fatalf("unexpected CreditBalance: has=%v bal=%q", snap.HasCredits, snap.CreditBalance)
	}
	if snap.Weekly == nil || snap.Weekly.RemainingPercent != 89 {
		t.Fatalf("unexpected Weekly snapshot: %+v", snap.Weekly)
	}
	uSnap := snap.ToUsageSnapshot()
	if uSnap.LimitGroup() != "Credits $106.44/$120" {
		t.Fatalf("LimitGroup = %q, want Credits $106.44/$120", uSnap.LimitGroup())
	}

	// Verify optional sk-admin-... HTTP org costs sync (with pagination + string/float values).
	costsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-admin-key-alpha" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("page") == "p2" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"results": []map[string]any{{"amount": map[string]any{"value": "0.02"}}}},
				},
				"has_more": false,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"results": []map[string]any{{"amount": map[string]any{"value": 0.05}}}},
			},
			"has_more":  true,
			"next_page": "p2",
		})
	}))
	defer costsSrv.Close()

	adminKey := "test-admin-key-alpha"
	acct.AuthData.AdminKey = &adminKey
	client.OpenAIBaseURL = costsSrv.URL
	client.HTTPClient = costsSrv.Client()

	syncedSnap, err := client.FetchUsage(context.Background(), store, acct, false)
	if err != nil {
		t.Fatalf("FetchUsage with AdminKey: %v", err)
	}
	if syncedSnap.CreditBalance != "$119.93/$120" {
		t.Fatalf("synced CreditBalance = %q, want $119.93/$120", syncedSnap.CreditBalance)
	}
}
