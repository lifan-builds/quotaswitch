package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/gateway"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

func TestSummarizeCodexSeatHeadroomAndEarliestCreditExpiry(t *testing.T) {
	soonReset := testNow.Add(time.Hour)
	laterReset := testNow.Add(3 * time.Hour)
	rows := []accountRow{
		{ID: "a", Email: "a@example.com", AuthMethod: codex.AuthModeChatGPT, Active: true, Usage: &usage.Snapshot{
			FiveHour: &usage.Window{RemainingPercent: 80}, Weekly: &usage.Window{RemainingPercent: 40},
		}},
		{ID: "b", AuthMethod: codex.AuthModeChatGPT, Usage: &usage.Snapshot{
			FiveHour: &usage.Window{RemainingPercent: 0, ResetsAt: &laterReset}, Weekly: &usage.Window{RemainingPercent: 50},
		}},
		{ID: "c", AuthMethod: codex.AuthModeChatGPT, Usage: &usage.Snapshot{
			FiveHour: &usage.Window{RemainingPercent: 0, ResetsAt: &soonReset}, Weekly: &usage.Window{RemainingPercent: 70},
		}},
		{ID: "d", AuthMethod: codex.AuthModeChatGPT, UsageError: "timeout"},
		{ID: "k", AuthMethod: codex.AuthModeAPIKey},
	}
	expired := testNow.Add(-time.Hour)
	soon := testNow.Add(48 * time.Hour)
	later := testNow.Add(96 * time.Hour)
	credits := &gateway.CreditsReport{
		Accounts: []gateway.AccountCreditStatus{
			{ID: "x", Alias: "expired", ExpiresAt: &expired, Expired: true, RemainingUSD: 10},
			{ID: "y", Alias: "drained", ExpiresAt: &soon, Exhausted: true},
			{ID: "z", Alias: "later", ExpiresAt: &later, RemainingUSD: 30},
			{ID: "w", Alias: "soon", ExpiresAt: &soon, RemainingUSD: 12.5},
		},
		TotalRemainingUSD:  42.5,
		ActivePoolAccounts: 2,
	}

	got := summarizeCodex(rows, credits, testNow)
	o := got.OAuth
	if o.Seats != 4 || o.SeatsWithHeadroom != 1 || o.SeatsExhausted != 2 || o.SeatsUnknown != 1 || o.HeadroomSeats != 0.4 {
		t.Fatalf("unexpected oauth summary: %+v", o)
	}
	if o.NextResetAt == nil || !o.NextResetAt.Equal(soonReset) {
		t.Fatalf("next_reset_at = %v, want %v", o.NextResetAt, soonReset)
	}
	if o.Active == nil || o.Active.ID != "a" || *o.Active.WeeklyRemainingPercent != 40 {
		t.Fatalf("unexpected active seat: %+v", o.Active)
	}
	e := got.APICredits.EarliestExpiry
	if e == nil || e.ID != "w" || e.ExpiresInHours != 48 || e.RemainingUSD != 12.5 {
		t.Fatalf("unexpected earliest expiry: %+v", e)
	}
	if got.APICredits.Orgs != 4 || got.APICredits.ActiveOrgs != 2 || got.APICredits.TotalRemainingUSD != 42.5 {
		t.Fatalf("unexpected api credit summary: %+v", got.APICredits)
	}
}

func TestCodexSummaryJSONContract(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	exp := testNow.Add(24 * time.Hour).UTC()
	key := "sk-summary"
	if err := codexStore.Save(&codex.AccountsStore{Version: 1, Accounts: []codex.StoredAccount{{
		ID: "api-1", Name: "api-exp", SubscriptionExpiresAt: &exp, AuthMode: codex.AuthModeAPIKey,
		AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &key}, CreatedAt: testNow,
	}}}); err != nil {
		t.Fatal(err)
	}
	cal, _, calErr := env.app("")
	cal.CodexStore = codexStore
	if code := cal.Run([]string{"codex", "credits", "--set", "api-exp", "--grant", "20", "--used", "5"}); code != 0 {
		t.Fatalf("calibrate failed: %s", calErr.String())
	}

	app, out, errBuf := env.app("")
	app.CodexStore = codexStore
	if code := app.Run([]string{"--json", "codex", "summary", "--no-usage"}); code != 0 {
		t.Fatalf("codex summary exit %d: %s", code, errBuf.String())
	}
	var envelope struct {
		Command string       `json:"command"`
		Data    codexSummary `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	e := envelope.Data.APICredits.EarliestExpiry
	if envelope.Command != "codex.summary" || envelope.Data.UsageFetched || e == nil || e.Name != "api-exp" || e.RemainingUSD != 15 {
		t.Fatalf("unexpected summary: %s", out.String())
	}
}
