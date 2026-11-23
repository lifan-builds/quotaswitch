// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"fmt"
	"math"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/gateway"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

type codexSummary struct {
	OAuth        oauthSeatSummary `json:"oauth"`
	APICredits   apiCreditSummary `json:"api_credits"`
	UsageFetched bool             `json:"usage_fetched"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

type oauthSeatSummary struct {
	Seats             int               `json:"seats"`
	SeatsWithHeadroom int               `json:"seats_with_headroom"`
	SeatsExhausted    int               `json:"seats_exhausted"`
	SeatsUnknown      int               `json:"seats_unknown"`
	HeadroomSeats     float64           `json:"headroom_seats"`
	NextResetAt       *time.Time        `json:"next_reset_at"`
	Active            *activeSeatStatus `json:"active"`
}

type activeSeatStatus struct {
	ID                       string `json:"id"`
	Name                     string `json:"name"`
	AuthMode                 string `json:"auth_mode"`
	FiveHourRemainingPercent *int   `json:"five_hour_remaining_percent"`
	WeeklyRemainingPercent   *int   `json:"weekly_remaining_percent"`
}

type apiCreditSummary struct {
	Orgs              int                `json:"orgs"`
	ActiveOrgs        int                `json:"active_orgs"`
	TotalRemainingUSD float64            `json:"total_remaining_usd"`
	EarliestExpiry    *creditGrantExpiry `json:"earliest_expiry"`
}

type creditGrantExpiry struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExpiresInHours float64   `json:"expires_in_hours"`
	RemainingUSD   float64   `json:"remaining_usd"`
}

func (a *App) cmdCodexSummary(args []string) int {
	fs := a.newFlagSet("codex")
	noUsage := fs.Bool("no-usage", false, "skip the live 5-hour/weekly quota fetch (seat headroom reported as unknown)")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex summary takes no arguments")
	}
	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	now := a.now()
	_, _ = store.SyncActiveFromLiveAuth(data, now)

	pool := gateway.NewPool(store, a.Store, a.Storage, nil)
	pool.Now = a.now
	credits, err := pool.CreditsSummary()
	if err != nil {
		return a.failCode("codex_credits_error", "", "could not load Codex credit ledger: %v", err)
	}

	summary := summarizeCodex(a.buildCodexAccountRows(store, data, now, !*noUsage), credits, now)
	summary.UsageFetched = !*noUsage
	if a.JSON {
		return a.okJSON(summary)
	}

	o := summary.OAuth
	fmt.Fprintf(a.Out, "OAuth seats: %d with headroom / %d total (%d exhausted, %d unknown), %.2f seat-equivalents left\n",
		o.SeatsWithHeadroom, o.Seats, o.SeatsExhausted, o.SeatsUnknown, o.HeadroomSeats)
	if o.NextResetAt != nil {
		fmt.Fprintf(a.Out, "Next reset:  %s (%s)\n", o.NextResetAt.Local().Format("2006-01-02 15:04 MST"), humanUntil(*o.NextResetAt, now))
	}
	if o.Active != nil {
		fmt.Fprintf(a.Out, "Active:      %s (%s)\n", o.Active.Name, o.Active.AuthMode)
	}
	c := summary.APICredits
	fmt.Fprintf(a.Out, "API credits: $%.2f across %d/%d active org(s)\n", c.TotalRemainingUSD, c.ActiveOrgs, c.Orgs)
	if e := c.EarliestExpiry; e != nil {
		fmt.Fprintf(a.Out, "Expires next: %s $%.2f on %s (%s)\n", e.Name, e.RemainingUSD, e.ExpiresAt.UTC().Format("2006-01-02"), humanUntil(e.ExpiresAt, now))
	}
	return 0
}

func summarizeCodex(rows []accountRow, credits *gateway.CreditsReport, now time.Time) codexSummary {
	out := codexSummary{UpdatedAt: now.UTC()}
	headroomPercent := 0
	for _, row := range rows {
		if row.Active {
			out.OAuth.Active = &activeSeatStatus{ID: row.ID, Name: row.displayName(), AuthMode: row.AuthMethod}
			if row.Usage != nil && row.Usage.FiveHour != nil {
				out.OAuth.Active.FiveHourRemainingPercent = &row.Usage.FiveHour.RemainingPercent
			}
			if row.Usage != nil && row.Usage.Weekly != nil {
				out.OAuth.Active.WeeklyRemainingPercent = &row.Usage.Weekly.RemainingPercent
			}
		}
		if row.AuthMethod != codex.AuthModeChatGPT {
			continue
		}
		out.OAuth.Seats++
		if row.Usage == nil || (row.Usage.FiveHour == nil && row.Usage.Weekly == nil) {
			out.OAuth.SeatsUnknown++
			continue
		}
		headroom := 100
		var resetAt *time.Time
		for _, w := range []*usage.Window{row.Usage.FiveHour, row.Usage.Weekly} {
			if w == nil {
				continue
			}
			headroom = min(headroom, w.RemainingPercent)
			if w.RemainingPercent <= 0 && w.ResetsAt != nil && (resetAt == nil || w.ResetsAt.After(*resetAt)) {
				resetAt = w.ResetsAt
			}
		}
		if headroom <= 0 {
			out.OAuth.SeatsExhausted++
			if resetAt != nil && (out.OAuth.NextResetAt == nil || resetAt.Before(*out.OAuth.NextResetAt)) {
				t := resetAt.UTC()
				out.OAuth.NextResetAt = &t
			}
			continue
		}
		out.OAuth.SeatsWithHeadroom++
		headroomPercent += headroom
	}
	out.OAuth.HeadroomSeats = float64(headroomPercent) / 100

	out.APICredits.Orgs = len(credits.Accounts)
	out.APICredits.ActiveOrgs = credits.ActivePoolAccounts
	out.APICredits.TotalRemainingUSD = credits.TotalRemainingUSD
	for _, st := range credits.Accounts {
		if st.ExpiresAt == nil || st.Expired || st.Exhausted || st.RemainingUSD <= 0 {
			continue
		}
		if e := out.APICredits.EarliestExpiry; e != nil && !st.ExpiresAt.Before(e.ExpiresAt) {
			continue
		}
		name := st.Alias
		if name == "" {
			name = st.Email
		}
		out.APICredits.EarliestExpiry = &creditGrantExpiry{
			ID:             st.ID,
			Name:           name,
			ExpiresAt:      st.ExpiresAt.UTC(),
			ExpiresInHours: math.Round(st.ExpiresAt.Sub(now).Hours()*10) / 10,
			RemainingUSD:   st.RemainingUSD,
		}
	}
	return out
}
