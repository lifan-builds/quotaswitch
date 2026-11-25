// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func (a *App) cmdCodexBilling(args []string) int {
	fs := a.newFlagSet("codex")
	all := fs.Bool("all", false, "include deactivated/canceled workspaces")
	noSync := fs.Bool("no-sync", false, "do not reconcile live ~/.codex/auth.json tokens")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex billing takes no arguments")
	}

	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	if a.CodexUsage == nil {
		return a.failCode("quota_error", "", "Codex client is unavailable")
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "Check "+store.AccountsPath()+".", "could not load Codex accounts: %v", err)
	}
	now := a.now()
	if !*noSync {
		_, _ = store.SyncActiveFromLiveAuth(data, now)
	}

	activeID := ""
	if data.ActiveAccountID != nil {
		activeID = *data.ActiveAccountID
	}
	proc := a.codexOps().Inspect()

	ctx, cancel := context.WithTimeout(context.Background(), codexUsageTimeout)
	defer cancel()
	report, err := a.CodexUsage.FetchBilling(ctx, store, data.Accounts, activeID, proc.Running())
	if err != nil {
		return a.failCode("codex_billing_error", "", "could not fetch ChatGPT billing: %v", err)
	}

	workspaces := make([]codex.WorkspaceBilling, 0, len(report.Workspaces))
	for _, ws := range report.Workspaces {
		if !*all && (!ws.Active || ws.Deactivated) {
			continue
		}
		workspaces = append(workspaces, ws)
	}

	if a.JSON {
		payload := map[string]any{
			"provider":   "codex",
			"workspaces": workspaces,
			"updated_at": report.UpdatedAt,
		}
		if len(report.Errors) > 0 {
			payload["errors"] = report.Errors
		}
		return a.okJSON(payload)
	}

	if len(workspaces) == 0 {
		if len(report.Workspaces) > 0 {
			fmt.Fprintln(a.Out, "No active ChatGPT workspaces found (pass `--all` to include deactivated workspaces).")
		} else {
			fmt.Fprintln(a.Out, "No ChatGPT subscription workspaces found.")
		}
		for _, e := range report.Errors {
			fmt.Fprintf(a.Err, "warning: %s\n", e)
		}
		return 0
	}

	cols := []tableColumn{
		{Header: "WORKSPACE", Min: 10, Max: 24, Priority: 1, Required: true},
		{Header: "OWNER", Min: 12, Max: 28, Priority: 2, Required: true, Flex: true},
		{Header: "CURRENCY", Min: 8, Max: 12, Priority: 3, Required: true},
		{Header: "STATUS", Min: 12, Max: 20, Priority: 4},
		{Header: "SEATS", Min: 10, Max: 28, Priority: 5},
		{Header: "DISCOUNT", Min: 10, Max: 28, Priority: 6},
		{Header: "ID", Min: 8, Max: 36, Priority: 7},
	}
	rows := make([]tableRow, 0, len(workspaces))
	for _, ws := range workspaces {
		owner := ws.OwnerEmail
		if owner == "" {
			owner = ws.OwnerID
		}
		if owner == "" {
			owner = "-"
		}
		currency := ws.BillingCurrency
		if currency == "" {
			currency = "-"
		} else if ws.PriceCountry != "" {
			currency = fmt.Sprintf("%s (%s)", currency, ws.PriceCountry)
		}
		rows = append(rows, tableRow{
			Cells: []string{
				nonEmptyOrDash(ws.Name),
				owner,
				currency,
				formatBillingStatus(ws),
				formatBillingSeats(ws),
				formatBillingDiscount(ws),
				ws.WorkspaceID,
			},
		})
	}
	a.renderTable(a.Out, a.tableWidth(), cols, rows, "\n")
	for _, e := range report.Errors {
		fmt.Fprintf(a.Err, "warning: %s\n", e)
	}
	return 0
}

func nonEmptyOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}

func formatBillingSeats(ws codex.WorkspaceBilling) string {
	if len(ws.SeatCapacity) == 0 {
		if ws.SeatsEntitled > 0 {
			return fmt.Sprintf("%d/%d", ws.SeatsInUse, ws.SeatsEntitled)
		}
		return "-"
	}
	var curParts []string
	var nextParts []string
	changed := false
	for _, sc := range ws.SeatCapacity {
		label := seatTypeShortLabel(sc.Type)
		if sc.Paid > 0 {
			curParts = append(curParts, fmt.Sprintf("%d %s", sc.Paid, label))
		}
		if sc.RenewalRequested != nil {
			if *sc.RenewalRequested != sc.Paid {
				changed = true
			}
			if *sc.RenewalRequested > 0 {
				nextParts = append(nextParts, fmt.Sprintf("%d %s", *sc.RenewalRequested, label))
			}
		}
		if len(sc.PendingOccupiedDowngrades) > 0 || len(sc.PendingUnoccupiedDowngrades) > 0 {
			changed = true
		}
	}
	cur := strings.Join(curParts, ", ")
	if cur == "" {
		return "-"
	}
	if changed && len(nextParts) > 0 && ws.WillRenew {
		return cur + " -> " + strings.Join(nextParts, ", ")
	}
	return cur
}

func seatTypeShortLabel(seatType string) string {
	switch strings.ToLower(strings.TrimSpace(seatType)) {
	case "default":
		return "std"
	case "prolite":
		return "prem"
	default:
		return strings.TrimSpace(seatType)
	}
}

func formatBillingStatus(ws codex.WorkspaceBilling) string {
	if ws.Error != "" {
		return "err: " + ws.Error
	}
	if ws.Deactivated || !ws.Active {
		if ws.CancelsAt != nil {
			return "ended " + ws.CancelsAt.UTC().Format("2006-01-02")
		}
		return "inactive"
	}
	if !ws.WillRenew {
		if ws.CancelsAt != nil {
			return "cancels " + ws.CancelsAt.UTC().Format("2006-01-02")
		}
		if ws.ActiveUntil != nil {
			return "cancels " + ws.ActiveUntil.UTC().Format("2006-01-02")
		}
		return "canceling"
	}
	if ws.RenewsAt != nil {
		return "renews " + ws.RenewsAt.UTC().Format("2006-01-02")
	}
	return "active"
}

func formatBillingDiscount(ws codex.WorkspaceBilling) string {
	if ws.Discount == nil || ws.Discount.Amount <= 0 {
		return "-"
	}
	prefix := ws.BillingCurrency + " "
	switch ws.BillingCurrency {
	case "USD":
		prefix = "$"
	case "GBP":
		prefix = "£"
	case "EUR":
		prefix = "€"
	case "":
		prefix = ""
	}
	amt := fmt.Sprintf("%s%.0f", prefix, ws.Discount.Amount)
	if ws.Discount.Amount != float64(int(ws.Discount.Amount)) {
		amt = fmt.Sprintf("%s%.2f", prefix, ws.Discount.Amount)
	}
	if ws.Discount.PromoCampaignID != "" {
		shortPromo := strings.TrimPrefix(ws.Discount.PromoCampaignID, "smb-channel-partner-")
		return fmt.Sprintf("%s (%s)", amt, shortPromo)
	}
	return amt
}
