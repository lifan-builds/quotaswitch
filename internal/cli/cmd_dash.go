// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/gateway"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

const (
	creditExpiryWarnWindow     = 5 * 24 * time.Hour
	creditExpiryCriticalWindow = 24 * time.Hour
)

type dashReport struct {
	Antigravity  []accountRow                  `json:"antigravity"`
	CodexSeats   []accountRow                  `json:"codex_seats"`
	Codex        codexSummary                  `json:"codex"`
	CreditGrants []gateway.AccountCreditStatus `json:"credit_grants"`
	Gateway      gatewayStatus                 `json:"gateway"`
	UpdatedAt    time.Time                     `json:"updated_at"`
}

func (a *App) cmdDash(args []string) int {
	fs := a.newFlagSet("dash")
	watch := fs.Bool("watch", false, "redraw the dashboard every live refresh interval")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("dash", "dash takes no arguments")
	}
	if !*watch {
		return a.dashOnce(nil)
	}
	interval := a.liveInterval()
	if cache, ok := a.Usage.(*usage.Cache); ok {
		cache.SetTTL(interval / 2)
	}
	var prev []string
	for {
		if code := a.dashOnce(&prev); code != 0 {
			return code
		}
		time.Sleep(interval)
	}
}

// dashOnce renders one dashboard. A non-nil frame switches text output to an
// in-place ANSI repaint that rewrites only lines that changed since the last frame.
func (a *App) dashOnce(frame *[]string) int {
	report, err := a.collectDash()
	if err != nil {
		return a.failCode("registry_error", "Run `quotaswitch status` or `quotaswitch path` to check storage.", "could not build the dashboard: %v", err)
	}
	if a.JSON {
		return a.okJSON(report)
	}
	lines := a.dashLines(report)
	if frame != nil {
		if height := a.tableHeight(); len(lines) > height {
			lines = append(lines[:height-1], a.dim(fmt.Sprintf("… %d more line(s); enlarge the terminal", len(lines)-height+1)))
		}
	}
	if frame == nil {
		fmt.Fprintln(a.Out, strings.Join(lines, "\n"))
		return 0
	}
	var buf strings.Builder
	if *frame == nil {
		buf.WriteString("\x1b[H\x1b[2J")
	}
	for i, line := range lines {
		if i < len(*frame) && (*frame)[i] == line {
			continue
		}
		fmt.Fprintf(&buf, "\x1b[%d;1H%s\x1b[K", i+1, line)
	}
	for i := len(lines); i < len(*frame); i++ {
		fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[K", i+1)
	}
	fmt.Fprint(a.Out, buf.String())
	*frame = lines
	return 0
}

func (a *App) collectDash() (dashReport, error) {
	now := a.now()
	report := dashReport{UpdatedAt: now.UTC(), CreditGrants: []gateway.AccountCreditStatus{}}
	var agyErr, creditsErr error
	var codexRows []accountRow
	var credits *gateway.CreditsReport
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		report.Antigravity, agyErr = a.agyAccountRows(true)
	}()
	go func() {
		defer wg.Done()
		var store *codex.Store
		if a.CodexStore != nil && a.CodexStore.Exists() {
			store = a.CodexStore
			if data, err := store.Load(); err == nil {
				_, _ = store.SyncActiveFromLiveAuth(data, now)
				codexRows = a.buildCodexAccountRows(store, data, now, true)
			}
		}
		pool := gateway.NewPool(store, a.Store, a.Storage, nil)
		pool.Now = a.now
		credits, creditsErr = pool.CreditsSummary()
	}()
	go func() {
		defer wg.Done()
		report.Gateway = gatewayStatusFor(gateway.DefaultListenAddr, a.gatewaySettingsPath(""))
	}()
	wg.Wait()
	if agyErr != nil {
		return report, agyErr
	}
	if creditsErr != nil {
		return report, creditsErr
	}
	report.Codex = summarizeCodex(codexRows, credits, now)
	report.Codex.UsageFetched = true
	report.CodexSeats = []accountRow{}
	for _, row := range codexRows {
		if row.AuthMethod == codex.AuthModeChatGPT {
			report.CodexSeats = append(report.CodexSeats, row)
		}
	}
	report.CreditGrants = credits.Accounts
	return report, nil
}

func (a *App) dashLines(r dashReport) []string {
	now := a.now()
	width := a.tableWidth()
	var buf bytes.Buffer
	section := func(title, detail string) {
		line := a.bold(title)
		if detail != "" {
			line += "  " + a.dim(detail)
		}
		fmt.Fprintf(&buf, "\n%s\n", line)
	}
	fmt.Fprintln(&buf, a.bold("quotaswitch dash")+"  "+a.dim(now.Local().Format("2006-01-02 15:04:05")))

	section("Antigravity", fmt.Sprintf("%d account(s)%s", len(r.Antigravity), activeSuffix(r.Antigravity)))
	if len(r.Antigravity) > 0 {
		a.renderDashSeats(&buf, width, r.Antigravity, now)
	}

	o := r.Codex.OAuth
	detail := fmt.Sprintf("%d/%d with headroom · %.2f seat-equivalents", o.SeatsWithHeadroom, o.Seats, o.HeadroomSeats)
	if o.SeatsExhausted > 0 {
		detail += fmt.Sprintf(" · %d exhausted", o.SeatsExhausted)
	}
	if o.SeatsUnknown > 0 {
		detail += fmt.Sprintf(" · %d unknown", o.SeatsUnknown)
	}
	if o.NextResetAt != nil {
		detail += " · next reset " + humanUntil(*o.NextResetAt, now)
	}
	section("Codex OAuth", detail)
	if warn := a.dashResetExpiryWarning(r.CodexSeats, now); warn != "" {
		fmt.Fprintln(&buf, warn)
	}
	if len(r.CodexSeats) > 0 {
		a.renderDashSeats(&buf, width, r.CodexSeats, now)
	}

	c := r.Codex.APICredits
	detail = fmt.Sprintf("$%.2f across %d/%d active org(s)", c.TotalRemainingUSD, c.ActiveOrgs, c.Orgs)
	if e := c.EarliestExpiry; e != nil {
		detail += fmt.Sprintf(" · next expiry %s %s", e.Name, humanUntil(e.ExpiresAt, now))
	}
	section("API credits", detail)
	if warn := a.dashCreditExpiryWarning(r.CreditGrants, now); warn != "" {
		fmt.Fprintln(&buf, warn)
	}
	if len(r.CreditGrants) > 0 {
		a.renderDashCredits(&buf, width, r.CreditGrants, now)
	}

	fmt.Fprintf(&buf, "\n%s  %s\n", a.bold("Gateway"), a.dashGatewayLine(r.Gateway))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = truncateStyled(lines[i], width)
	}
	return lines
}

func truncateStyled(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if visibleWidth(value) <= width {
		return value
	}
	var b strings.Builder
	visible := 0
	inEscape := false
	for _, r := range value {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape:
			inEscape = !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		case visible == width-1:
			b.WriteString("…")
			if strings.Contains(value, "\x1b[") {
				b.WriteString("\x1b[0m")
			}
			return b.String()
		default:
			visible++
		}
		b.WriteRune(r)
	}
	return b.String()
}

func activeSuffix(rows []accountRow) string {
	for _, row := range rows {
		if row.Active {
			return " · active " + row.displayName()
		}
	}
	return ""
}

func (a *App) renderDashSeats(buf *bytes.Buffer, width int, rows []accountRow, now time.Time) {
	withResets := false
	for _, row := range rows {
		if row.ResetCredits != nil {
			withResets = true
			break
		}
	}
	columns := []tableColumn{
		{Header: "#", Min: 2, Max: 2, Right: true, Required: true},
		{Header: "", Min: 1, Max: 1, Required: true},
		{Header: "ACCOUNT", Min: 12, Max: 48, Required: true, Flex: true},
		{Header: "PLAN", Min: 4, Max: 12, Priority: 3},
		{Header: "5H", Min: 4, Max: 5, Right: true, Required: true},
		{Header: "WEEKLY", Min: 6, Max: 6, Right: true, Required: true},
	}
	if withResets {
		columns = append(columns, tableColumn{Header: "RESETS", Min: 6, Max: 11, Right: true, Priority: 1})
	}
	columns = append(columns, tableColumn{Header: "RESET", Min: 5, Max: 24, Priority: 2})
	table := make([]tableRow, len(rows))
	for i, row := range rows {
		base := a.accountTableRow(row, now, nil, false, false, false)
		cells := append(base.Cells, a.planCell(row), a.usageCell(row, false), a.usageCell(row, true))
		if withResets {
			cells = append(cells, a.resetCreditsCell(row))
		}
		cells = append(cells, a.dashResetCell(row, now))
		table[i] = tableRow{Cells: cells}
	}
	a.renderTable(buf, width, columns, table, "\n")
}

func (a *App) dashResetExpiryWarning(rows []accountRow, now time.Time) string {
	var (
		totalExpiring int
		seatCount     int
		earliestName  string
		earliestUntil time.Duration
		earliestAt    time.Time
	)
	for _, row := range rows {
		if row.ResetCredits == nil || !row.ResetCredits.HasAvailable(now) {
			continue
		}
		seatExpiring := 0
		var seatSoonest *time.Time
		for _, c := range row.ResetCredits.Credits {
			if !strings.EqualFold(strings.TrimSpace(c.Status), "available") || c.ExpiresAt == nil {
				continue
			}
			until := c.ExpiresAt.Sub(now)
			if until <= 0 || until >= creditExpiryWarnWindow {
				continue
			}
			seatExpiring++
			if seatSoonest == nil || c.ExpiresAt.Before(*seatSoonest) {
				t := *c.ExpiresAt
				seatSoonest = &t
			}
		}
		if seatExpiring == 0 && len(row.ResetCredits.Credits) == 0 && row.ResetCredits.NextExpiresAt != nil {
			until := row.ResetCredits.NextExpiresAt.Sub(now)
			if until > 0 && until < creditExpiryWarnWindow {
				seatExpiring = row.ResetCredits.AvailableCount
				t := *row.ResetCredits.NextExpiresAt
				seatSoonest = &t
			}
		}
		if seatExpiring == 0 || seatSoonest == nil {
			continue
		}
		seatCount++
		totalExpiring += seatExpiring
		until := seatSoonest.Sub(now)
		if seatCount == 1 || until < earliestUntil {
			earliestUntil = until
			earliestAt = *seatSoonest
			earliestName = row.displayName()
		}
	}
	if totalExpiring == 0 {
		return ""
	}
	var msg string
	if seatCount == 1 {
		msg = fmt.Sprintf("WARNING: %d reset credit(s) expiring <5d (%s %s)", totalExpiring, earliestName, humanUntil(earliestAt, now))
	} else {
		msg = fmt.Sprintf("WARNING: %d reset credit(s) expiring <5d (%d seats; next %s %s)", totalExpiring, seatCount, earliestName, humanUntil(earliestAt, now))
	}
	if earliestUntil <= creditExpiryCriticalWindow {
		return a.bold(a.red(msg))
	}
	return a.bold(a.yellow(msg))
}

// dashResetCell shows when the most constrained quota window refills, or the
// fetch error when quota could not be read.
func (a *App) dashResetCell(row accountRow, now time.Time) string {
	if row.UsageError != "" {
		return a.red(row.UsageError)
	}
	if row.Usage == nil {
		return "-"
	}
	var binding *usage.Window
	for _, w := range []*usage.Window{row.Usage.FiveHour, row.Usage.Weekly} {
		if w != nil && w.ResetsAt != nil && (binding == nil || w.RemainingPercent < binding.RemainingPercent) {
			binding = w
		}
	}
	if binding == nil {
		return "-"
	}
	return humanUntil(*binding.ResetsAt, now)
}

func creditExpiryUntil(g gateway.AccountCreditStatus, now time.Time) (time.Duration, bool) {
	if g.ExpiresAt == nil || g.Expired || g.Exhausted || g.RemainingUSD <= 0 {
		return 0, false
	}
	until := g.ExpiresAt.Sub(now)
	if until <= 0 || until >= creditExpiryWarnWindow {
		return 0, false
	}
	return until, true
}

func (a *App) dashCreditExpiryWarning(grants []gateway.AccountCreditStatus, now time.Time) string {
	var (
		count         int
		totalUSD      float64
		earliestName  string
		earliestUntil time.Duration
		earliestAt    time.Time
	)
	for _, g := range grants {
		until, ok := creditExpiryUntil(g, now)
		if !ok {
			continue
		}
		count++
		totalUSD += g.RemainingUSD
		if count == 1 || until < earliestUntil {
			earliestUntil = until
			earliestAt = *g.ExpiresAt
			earliestName = g.Alias
			if earliestName == "" {
				earliestName = g.Email
			}
		}
	}
	if count == 0 {
		return ""
	}
	var msg string
	if count == 1 {
		msg = fmt.Sprintf("WARNING: $%.2f expiring <5d (%s %s)", totalUSD, earliestName, humanUntil(earliestAt, now))
	} else {
		msg = fmt.Sprintf("WARNING: $%.2f expiring <5d (%d orgs; next %s %s)", totalUSD, count, earliestName, humanUntil(earliestAt, now))
	}
	if earliestUntil <= creditExpiryCriticalWindow {
		return a.bold(a.red(msg))
	}
	return a.bold(a.yellow(msg))
}

func (a *App) renderDashCredits(buf *bytes.Buffer, width int, grants []gateway.AccountCreditStatus, now time.Time) {
	columns := []tableColumn{
		{Header: "ORG", Min: 12, Max: 40, Required: true, Flex: true},
		{Header: "LEFT", Min: 8, Max: 10, Right: true, Required: true},
		{Header: "GRANT", Min: 8, Max: 10, Right: true, Priority: 2},
		{Header: "EXPIRES", Min: 10, Max: 10, Priority: 1},
		{Header: "IN", Min: 7, Max: 16, Required: true},
	}
	table := make([]tableRow, len(grants))
	for i, g := range grants {
		name := g.Alias
		if name == "" {
			name = g.Email
		}
		left := fmt.Sprintf("$%.2f", g.RemainingUSD)
		switch {
		case g.Expired || g.Exhausted:
			left = a.dim(left)
		case g.RemainingPercent <= 20:
			left = a.red(left)
		}
		expires, in := "-", "-"
		if g.ExpiresAt != nil {
			expires = g.ExpiresAt.UTC().Format("2006-01-02")
			in = humanUntil(*g.ExpiresAt, now)
			until := g.ExpiresAt.Sub(now)
			switch {
			case g.Expired || until <= 0:
				in = a.dim("expired")
			case g.Exhausted:
				in = a.dim("exhausted")
			case g.RemainingUSD > 0 && until <= creditExpiryCriticalWindow:
				name = a.bold(a.red(name))
				expires = a.red(expires)
				in = a.bold(a.red(in + " !"))
			case g.RemainingUSD > 0 && until < creditExpiryWarnWindow:
				name = a.bold(a.yellow(name))
				expires = a.yellow(expires)
				in = a.bold(a.yellow(in + " !"))
			}
		} else if g.Exhausted && !g.Expired {
			in = a.dim("exhausted")
		}
		table[i] = tableRow{Cells: []string{name, left, fmt.Sprintf("$%.2f", g.GrantUSD), expires, in}}
	}
	a.renderTable(buf, width, columns, table, "\n")
}

func (a *App) dashGatewayLine(g gatewayStatus) string {
	state := a.red("down")
	if g.Running {
		state = a.green("running")
	}
	parts := []string{state + " on http://" + g.Listen}
	settings := "desktop settings off"
	if g.SettingsActive {
		settings = "desktop settings on"
	}
	parts = append(parts, settings)
	if !g.ServiceFile {
		parts = append(parts, "no LaunchAgent")
	}
	if h := g.Health; h != nil {
		parts = append(parts, fmt.Sprintf("codex=%v agy=%v models=%v", h["codex_accounts"], h["agy_accounts"], h["models_count"]))
	}
	return strings.Join(parts, " · ")
}
