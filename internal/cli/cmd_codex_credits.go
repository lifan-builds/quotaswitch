package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/gateway"
)

func (a *App) cmdCodexCredits(args []string) int {
	fs := a.newFlagSet("codex")
	syncNow := fs.Bool("sync", false, "sync server-side spend via GET /v1/organization/costs for accounts with an admin key")
	setQuery := fs.String("set", "", "calibrate credit grant/balance for the matching API-key account")
	grantStr := fs.String("grant", "", "total promotional credit grant in USD (e.g. 120)")
	usedStr := fs.String("used", "", "current used USD baseline (e.g. 0.07)")
	remStr := fs.String("remaining", "", "current remaining USD balance (e.g. 119.93)")
	resetMetered := fs.Bool("reset-metered", true, "reset incremental gateway metered_usd when calibrating --used/--remaining")
	exhaustedStr := fs.String("exhausted", "", "explicitly mark account exhausted (true or false)")
	adminKeyStdin := fs.Bool("admin-key-stdin", false, "read an optional sk-admin-... key from stdin (or empty stdin to clear)")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex credits takes flags only (use --set <query> to calibrate)")
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

	if strings.TrimSpace(*setQuery) != "" {
		return a.applyCodexCreditCalibration(
			store,
			data,
			strings.TrimSpace(*setQuery),
			*grantStr,
			*usedStr,
			*remStr,
			*exhaustedStr,
			*resetMetered,
			*adminKeyStdin,
			*syncNow,
			now,
		)
	}

	if *syncNow {
		if exitCode := a.syncCodexAdminCredits(store, data, now); exitCode != 0 {
			return exitCode
		}
	}

	pool := gateway.NewPool(store, a.Store, a.Storage, nil)
	pool.Now = a.now
	report, err := pool.CreditsSummary()
	if err != nil {
		return a.failCode("codex_credits_error", "", "could not load Codex credit ledger: %v", err)
	}

	if a.JSON {
		if err := a.emitJSON(report); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	if len(report.Accounts) == 0 {
		fmt.Fprintln(a.Out, "No OpenAI API-key accounts stored in Codex pool.")
		return 0
	}

	columns := []tableColumn{
		{Header: "ACCOUNT", Min: 16, Max: 36, Priority: 0, Required: true, Flex: true},
		{Header: "GRANT", Min: 7, Max: 10, Priority: 1, Right: true, Required: true},
		{Header: "USED", Min: 7, Max: 10, Priority: 2, Right: true, Required: true},
		{Header: "REMAINING", Min: 9, Max: 12, Priority: 3, Right: true, Required: true},
		{Header: "LEFT%", Min: 5, Max: 6, Priority: 4, Right: true},
		{Header: "CALLS", Min: 5, Max: 9, Priority: 5, Right: true},
		{Header: "EXPIRES", Min: 10, Max: 16, Priority: 6},
		{Header: "SOURCE", Min: 7, Max: 12, Priority: 7},
	}

	tableRows := make([]tableRow, 0, len(report.Accounts))
	for _, st := range report.Accounts {
		label := st.Alias
		if label == "" {
			label = st.Email
		} else if st.Email != "" && !strings.EqualFold(label, st.Email) {
			label = fmt.Sprintf("%s (%s)", label, st.Email)
		}
		expStr := "-"
		if st.ExpiresAt != nil {
			expStr = st.ExpiresAt.UTC().Format("2006-01-02")
		}
		src := "unseeded"
		switch {
		case st.Expired:
			src = "expired"
		case st.Exhausted:
			src = a.red("exhausted")
		case st.LastSyncedAt != nil:
			src = a.green("admin-api")
		case st.GrantUSD > 0:
			src = "metered"
		}
		pctStr := "-"
		if st.GrantUSD > 0 {
			pctStr = fmt.Sprintf("%d%%", st.RemainingPercent)
			if st.RemainingPercent <= 10 {
				pctStr = a.red(pctStr)
			} else if st.RemainingPercent <= 25 {
				pctStr = a.yellow(pctStr)
			} else {
				pctStr = a.green(pctStr)
			}
		}
		tableRows = append(tableRows, tableRow{
			Cells: []string{
				label,
				fmt.Sprintf("$%.2f", st.GrantUSD),
				fmt.Sprintf("$%.2f", st.UsedUSD),
				fmt.Sprintf("$%.2f", st.RemainingUSD),
				pctStr,
				strconv.FormatInt(st.Requests, 10),
				expStr,
				src,
			},
		})
	}

	a.renderTable(a.Out, a.tableWidth(), columns, tableRows, "\n")
	fmt.Fprintf(
		a.Out,
		"\nPool Total: $%.2f / $%.2f remaining (%d%%, $%.2f used) across %d active API-key org(s).\n",
		report.TotalRemainingUSD,
		report.TotalGrantUSD,
		report.RemainingPercent,
		report.TotalUsedUSD,
		report.ActivePoolAccounts,
	)
	return 0
}

func (a *App) applyCodexCreditCalibration(
	store *codex.Store,
	data *codex.AccountsStore,
	query, grantStr, usedStr, remStr, exhaustedStr string,
	resetMetered, adminKeyStdin, syncNow bool,
	now time.Time,
) int {
	target, err := codex.Resolve(data, query)
	if err != nil {
		return a.failCode("account_not_found", "Run `quotaswitch codex list` to view stored accounts.", "%v", err)
	}
	if target.AuthMode != codex.AuthModeAPIKey {
		return a.failCode("invalid_account_mode", "", "account %s uses %s (credits calibration applies to api_key accounts)", target.DisplayName(), target.AuthMode)
	}

	var cal codex.CreditCalibration
	cal.ResetMetered = resetMetered

	if strings.TrimSpace(grantStr) != "" {
		v, pErr := parseUSDFlag(grantStr)
		if pErr != nil {
			return a.failCode("invalid_argument", "", "invalid --grant value %q: %v", grantStr, pErr)
		}
		cal.GrantUSD = &v
	}
	if strings.TrimSpace(usedStr) != "" {
		v, pErr := parseUSDFlag(usedStr)
		if pErr != nil {
			return a.failCode("invalid_argument", "", "invalid --used value %q: %v", usedStr, pErr)
		}
		cal.UsedUSD = &v
	}
	if strings.TrimSpace(remStr) != "" {
		v, pErr := parseUSDFlag(remStr)
		if pErr != nil {
			return a.failCode("invalid_argument", "", "invalid --remaining value %q: %v", remStr, pErr)
		}
		cal.RemainingUSD = &v
	}
	if strings.TrimSpace(exhaustedStr) != "" {
		b, pErr := strconv.ParseBool(strings.TrimSpace(exhaustedStr))
		if pErr != nil {
			return a.failCode("invalid_argument", "", "invalid --exhausted value %q (expected true or false)", exhaustedStr)
		}
		cal.Exhausted = &b
	}

	if adminKeyStdin {
		if a.In == nil {
			return a.failCode("invalid_argument", "", "stdin is unavailable for --admin-key-stdin")
		}
		rawKey, rErr := io.ReadAll(io.LimitReader(a.In, 16*1024))
		if rErr != nil {
			return a.failCode("input_error", "", "could not read admin key from stdin: %v", rErr)
		}
		trimmed := strings.TrimSpace(string(rawKey))
		for i := range data.Accounts {
			if data.Accounts[i].ID == target.ID {
				if trimmed == "" {
					data.Accounts[i].AuthData.AdminKey = nil
				} else {
					data.Accounts[i].AuthData.AdminKey = &trimmed
				}
				target = &data.Accounts[i]
				break
			}
		}
		if err := store.Save(data); err != nil {
			return a.failCode("codex_store_error", "", "could not save Codex accounts: %v", err)
		}
	}

	entry, err := store.CalibrateCredit(*target, cal, now)
	if err != nil {
		return a.failCode("codex_credits_error", "", "could not update credit ledger: %v", err)
	}

	if syncNow && target.AuthData.AdminKey != nil && strings.TrimSpace(*target.AuthData.AdminKey) != "" {
		if exitCode := a.syncSingleAdminCredit(store, *target, now); exitCode != 0 {
			return exitCode
		}
		if ledger, lErr := store.LoadCreditLedger(); lErr == nil {
			if refreshed, ok := ledger.Lookup(*target); ok {
				entry = refreshed
			}
		}
	}

	rem := entry.RemainingUSD(now, target.SubscriptionExpiresAt)
	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"account":           target.DisplayName(),
			"id":                target.ID,
			"grant_usd":         entry.GrantUSD,
			"used_usd":          entry.UsedUSD(),
			"remaining_usd":     rem,
			"remaining_percent": entry.RemainingPercent(now, target.SubscriptionExpiresAt),
			"exhausted":         entry.Exhausted,
			"has_admin_key":     target.AuthData.AdminKey != nil && strings.TrimSpace(*target.AuthData.AdminKey) != "",
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(
		a.Out,
		"Updated credits for %s: $%.2f / $%.2f remaining (%d%%, $%.2f used).\n",
		target.DisplayName(),
		rem,
		entry.GrantUSD,
		entry.RemainingPercent(now, target.SubscriptionExpiresAt),
		entry.UsedUSD(),
	)
	return 0
}

func (a *App) syncCodexAdminCredits(store *codex.Store, data *codex.AccountsStore, now time.Time) int {
	for _, acct := range data.Accounts {
		if acct.AuthMode != codex.AuthModeAPIKey || acct.AuthData.AdminKey == nil || strings.TrimSpace(*acct.AuthData.AdminKey) == "" {
			continue
		}
		if code := a.syncSingleAdminCredit(store, acct, now); code != 0 {
			return code
		}
	}
	return 0
}

func (a *App) syncSingleAdminCredit(store *codex.Store, acct codex.StoredAccount, now time.Time) int {
	baseURL := codex.DefaultOpenAIBaseURL
	var client *http.Client
	if a.CodexUsage != nil {
		if strings.TrimSpace(a.CodexUsage.OpenAIBaseURL) != "" {
			baseURL = a.CodexUsage.OpenAIBaseURL
		}
		client = a.CodexUsage.HTTPClient
	}
	startTime := now.Add(-60 * 24 * time.Hour).Unix()
	if !acct.CreatedAt.IsZero() && acct.CreatedAt.Add(-30*24*time.Hour).Unix() < startTime {
		startTime = acct.CreatedAt.Add(-30 * 24 * time.Hour).Unix()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	used, err := codex.FetchOpenAIOrgCosts(ctx, client, baseURL, *acct.AuthData.AdminKey, startTime)
	if err != nil {
		return a.failCode("codex_credits_sync_failed", "Verify the sk-admin-... key has api.usage.read permission.", "could not sync org costs for %s: %v", acct.DisplayName(), err)
	}
	if _, err := store.CalibrateCredit(acct, codex.CreditCalibration{
		UsedUSD:      &used,
		ResetMetered: true,
		Synced:       true,
	}, now); err != nil {
		return a.failCode("codex_credits_error", "", "could not save synced credits for %s: %v", acct.DisplayName(), err)
	}
	return 0
}

func parseUSDFlag(raw string) (float64, error) {
	cleaned := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "$"))
	return strconv.ParseFloat(cleaned, 64)
}
