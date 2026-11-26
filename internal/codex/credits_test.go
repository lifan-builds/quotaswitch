// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestCreditLedgerKeepsReusedAliasAccountsSeparate(t *testing.T) {
	const alias = "API Key account"
	oldAccount := StoredAccount{ID: "old-seat", Name: alias}
	replacement := StoredAccount{ID: "new-seat", Name: alias}
	for _, key := range []string{oldAccount.ID, alias} {
		t.Run(key, func(t *testing.T) {
			oldEntry := CreditEntry{AccountID: oldAccount.ID, Alias: alias, GrantUSD: 100, Exhausted: true}
			ledger := &CreditLedger{Accounts: map[string]CreditEntry{key: oldEntry}}
			if _, ok := ledger.Lookup(replacement); ok {
				t.Error("replacement inherited the old account's credit entry")
			}
			ledger.upsert(replacement, CreditEntry{Requests: 1})
			if entry, ok := ledger.Lookup(oldAccount); !ok || entry != oldEntry {
				t.Error("writing the replacement's credit entry changed the old account's entry")
			}
			if entry, ok := ledger.Lookup(replacement); !ok || entry.AccountID != replacement.ID || entry.Requests != 1 || entry.Exhausted {
				t.Errorf("replacement credit entry = %+v, found = %t", entry, ok)
			}
		})
	}
}

func TestCreditLedgerConcurrentGoroutines(t *testing.T) {
	const workers = 16
	store := NewStore(t.TempDir(), "")
	account := StoredAccount{ID: "shared-api-account"}
	now := time.Unix(0, 0)
	start := make(chan struct{})
	errs := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			errs <- store.RecordCreditCall(account, CreditCallDelta{}, now)
		}()
	}
	close(start)
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("RecordCreditCall: %v", err)
		}
	}
	ledger, err := store.LoadCreditLedger()
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := ledger.Lookup(account); entry.Requests != workers {
		t.Errorf("metered requests = %d, want %d", entry.Requests, workers)
	}
}

func TestCreditLedgerConcurrentProcesses(t *testing.T) {
	const workers = 4
	const calls = 24
	const grant = 100.0
	account := StoredAccount{ID: "shared-api-account"}
	now := time.Unix(0, 0)
	delta := CreditCallDelta{CostUSD: 0.125, InputTokens: 17}
	if dir := concurrentStoreWorkerDir(t); dir != "" {
		store := NewStore(dir, "")
		for range calls {
			if err := store.RecordCreditCall(account, delta, now); err != nil {
				t.Fatal(err)
			}
		}
		grantUSD := grant
		if _, err := store.CalibrateCredit(account, CreditCalibration{GrantUSD: &grantUSD}, now); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCreditExhausted(account, now); err != nil {
			t.Fatal(err)
		}
		return
	}
	store := NewStore(filepath.Join(t.TempDir(), "switcher"), "")
	runConcurrentStoreWorkers(t, t.Name(), store.SwitcherDir, workers)
	ledger, err := store.LoadCreditLedger()
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := ledger.Lookup(account)
	if entry.Requests != workers*calls || entry.InputTokens != workers*calls*delta.InputTokens || entry.MeteredUSD != workers*calls*delta.CostUSD {
		t.Errorf("metered totals = %d requests, %d input tokens, $%.2f", entry.Requests, entry.InputTokens, entry.MeteredUSD)
	}
	if entry.GrantUSD != grant || !entry.Exhausted {
		t.Errorf("grant = $%.2f, exhausted = %v", entry.GrantUSD, entry.Exhausted)
	}
}

const concurrentStoreWorkerEnv = "QUOTASWITCH_TEST_STORE_DIR"

func concurrentStoreWorkerDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(concurrentStoreWorkerEnv)
	if dir != "" {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runConcurrentStoreWorkers(t *testing.T, testName, storeDir string, workers int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	started := make(chan struct{}, workers)
	results := make(chan error, workers)
	for range workers {
		go func() {
			cmd := exec.CommandContext(ctx, binary, "-test.run=^"+testName+"$")
			cmd.Env = append(cmd.Environ(), concurrentStoreWorkerEnv+"="+storeDir)
			cmd.Stdin = reader
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			err := cmd.Start()
			started <- struct{}{}
			if err == nil {
				err = cmd.Wait()
			}
			if err != nil {
				err = fmt.Errorf("store worker: %w\n%s", err, output.Bytes())
			}
			results <- err
		}()
	}
	for range workers {
		<-started
	}
	if err := writer.Close(); err != nil {
		t.Error(err)
	}
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
