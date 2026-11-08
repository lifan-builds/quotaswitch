package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/fikrilal/agy-auth/internal/agy"
	"github.com/fikrilal/agy-auth/internal/registry"
	"github.com/fikrilal/agy-auth/internal/usage"
)

func (a *App) cmdRotate(args []string) int {
	fs := a.newFlagSet("rotate")
	target := fs.String("target", "all", "switch target: all, cli, or desktop")
	relaunch := fs.Bool("relaunch", false, "relaunch Antigravity desktop after switching")
	noRelaunch := fs.Bool("no-relaunch", false, "do not restart Antigravity desktop language_server after switching")
	noAutoContinue := fs.Bool("no-auto-continue", false, "do not auto-continue quota-stopped conversations after relaunching Desktop")
	closeDesktop := fs.Bool("close-desktop", false, "gracefully close Antigravity desktop before switching")
	force := fs.Bool("force", false, "rotate even if the active account is not exhausted")
	watch := fs.Bool("watch", false, "continuously monitor quota and auto-rotate when the active model group or priority quota is exhausted")
	interval := fs.Duration("interval", 60*time.Second, "poll interval when --watch is enabled")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("rotate", "rotate does not accept arguments")
	}

	tgt := strings.ToLower(strings.TrimSpace(*target))
	switch tgt {
	case "all", "cli", "desktop":
	default:
		return a.usageFor("rotate", "target must be 'all', 'cli', or 'desktop'")
	}

	if *watch {
		poll := *interval
		if poll < 10*time.Second {
			poll = 10 * time.Second
		}
		if cache, ok := a.Usage.(*usage.Cache); ok {
			cache.SetTTL(poll / 2)
		}
		if cache, ok := a.UsageFull.(*usage.FullCache); ok {
			cache.SetTTL(poll / 2)
		}
		a.Out = &timestampWriter{w: a.Out, now: a.now}
		a.Err = &timestampWriter{w: a.Err, now: a.now}
		lastCode := 0
		for {
			code := a.rotateOnce(tgt, *relaunch, *noRelaunch, *closeDesktop, *force, *noAutoContinue)
			if code != 0 && lastCode == 0 {
				notifyDesktop("agy-auth", "No activated Antigravity account has quota left. Sign in to an unverified account or wait for a reset.")
			}
			lastCode = code
			time.Sleep(poll)
		}
	}

	return a.rotateOnce(tgt, *relaunch, *noRelaunch, *closeDesktop, *force, *noAutoContinue)
}

// timestampWriter prefixes each output line with a local timestamp so the
// background watch log shows when each check and rotation happened.
type timestampWriter struct {
	w       io.Writer
	now     func() time.Time
	midLine bool
}

func (t *timestampWriter) Write(p []byte) (int, error) {
	var buf bytes.Buffer
	for _, b := range p {
		if !t.midLine {
			buf.WriteString(t.now().Format("2006-01-02 15:04:05 "))
			t.midLine = true
		}
		buf.WriteByte(b)
		if b == '\n' {
			t.midLine = false
		}
	}
	if _, err := t.w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

func notifyDesktop(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", message, title)
	_ = exec.Command("/usr/bin/osascript", "-e", script).Run()
}

func (a *App) rotateOnce(tgt string, relaunch, noRelaunch, closeDesktop, force, noAutoContinue bool) int {
	reg, err := a.Store.Load()
	if err != nil {
		return a.failCode("registry_error", "Run `agy-auth status` or `agy-auth path` to check storage.", "could not read the account registry")
	}
	if len(reg.Accounts) == 0 {
		return a.failCode("account_not_found", "Run `agy-auth add` or `agy-auth login` first.", "no stored accounts")
	}

	var currentRaw []byte
	var current *agy.TokenFile
	read, readErr := a.Storage.Read()
	if readErr == nil {
		currentRaw = read.Raw
		current, _ = agy.ParseTokenFile(currentRaw)
	} else if !errors.Is(readErr, agy.ErrNoCredentials) {
		a.warn("%v", readErr)
	}

	var matched *registry.Account
	if current != nil {
		matched, _ = reg.Match(currentRaw)
		// If the user manually signed into a new account in Antigravity Desktop/CLI
		// that is not yet in registry.json, auto-ingest it so rotate monitors the
		// actual live account instead of overwriting it with an old registry account.
		if matched == nil && a.UsageGroups != nil && current.Email() != "" {
			if upRes, upErr := a.Store.Upsert(currentRaw, "", true); upErr == nil {
				if reloaded, loadErr := a.Store.Load(); loadErr == nil {
					reg = reloaded
					matched = reg.FindByID(upRes.Account.ID)
				}
			}
		}
	}

	activeID := reg.Active
	if matched != nil {
		activeID = matched.ID
	}

	numbers := accountNumbers(reg)
	rows := a.pickerRows(reg.Accounts, activeID, numbers)
	if !a.JSON && a.OutTTY {
		fmt.Fprintln(a.Out, "Checking quota across accounts...")
	}

	activeIdx := -1
	var activeRow *accountRow
	var activeAccount *registry.Account
	for i := range rows {
		if rows[i].ID == activeID {
			activeIdx = i
			activeRow = &rows[i]
			break
		}
	}
	if activeID != "" {
		activeAccount = reg.FindByID(activeID)
	}

	// Fast path: when not forced, check only the active account first. On healthy
	// polls (>95% of watch cycles), this avoids querying all other stored accounts.
	if !force && activeIdx >= 0 && rows[activeIdx].ID != "" {
		a.enrichUsageFull(rows[activeIdx : activeIdx+1])
	} else {
		a.enrichUsageFull(rows)
	}

	storeDir := ""
	if a.Store != nil {
		storeDir = a.Store.Dir
	}
	deskDir := ""
	if a.Desktop != nil {
		deskDir = a.Desktop.ConfigDir
	}

	if !force && activeRow != nil && activeRow.ID != "" && quotaDataFresh(activeRow) && !quotaExhausted(activeRow) {
		if matched != nil && reg.Active != matched.ID {
			reg.Active = matched.ID
			_ = a.Store.Save(reg)
		}
		switchedModel, autoContinued := reconcileActiveAccountModelAndContinue(storeDir, deskDir, a.AntigravityDataDir, activeRow, a.now(), noAutoContinue)
		claudePct, hasClaude, argonPct, hasArgon := priorityGroupQuotas(activeRow)
		if a.JSON {
			payload := map[string]any{
				"rotated": false,
				"reason":  "active_healthy",
				"account": activeAccount,
			}
			if hasClaude {
				payload["claude_remaining_percent"] = claudePct
			}
			if hasArgon {
				payload["argon_remaining_percent"] = argonPct
			}
			if switchedModel != "" {
				payload["model_switched"] = switchedModel
			}
			if len(autoContinued) > 0 {
				payload["auto_continued"] = autoContinued
			}
			if err := a.emitJSON(payload); err != nil {
				return a.failCode("output_error", "", "could not write JSON output")
			}
			return 0
		}
		if switchedModel != "" {
			fmt.Fprintf(a.Out, "Switched model to %s on %s.\n", switchedModel, activeRow.displayName())
		}
		if len(autoContinued) > 0 {
			fmt.Fprintf(a.Out, "Auto-continued %d stopped conversation(s).\n", len(autoContinued))
		}
		rem := quotaRemaining(activeRow)
		if hasClaude || hasArgon {
			fmt.Fprintf(a.Out, "Active account %s is healthy (Claude: %d%%, Gemini 4 Argon: %d%%, %d%% quota remaining). No rotation needed. Use --force to switch anyway.\n", activeRow.displayName(), claudePct, argonPct, rem)
		} else {
			fmt.Fprintf(a.Out, "Active account %s is healthy (%d%% quota remaining). No rotation needed. Use --force to switch anyway.\n", activeRow.displayName(), rem)
		}
		return 0
	}

	activeUnactivated := !force && activeRow != nil && activeRow.ID != "" && activeRow.UsageError == "forbidden"
	if !force && activeRow != nil && activeRow.ID != "" && isTransientUsageError(activeRow.UsageError) {
		if a.JSON {
			if err := a.emitJSON(map[string]any{
				"rotated":     false,
				"reason":      "usage_check_transient_error",
				"usage_error": activeRow.UsageError,
				"account":     activeAccount,
			}); err != nil {
				return a.failCode("output_error", "", "could not write JSON output")
			}
			return 0
		}
		fmt.Fprintf(a.Out, "Active account %s quota check failed transiently (%s). No rotation needed. Use --force to switch anyway.\n", activeRow.displayName(), activeRow.UsageError)
		return 0
	}

	// Active account is exhausted, unactivated, or has a hard credential failure; enrich candidate accounts now.
	if !force && activeIdx >= 0 && rows[activeIdx].ID != "" && len(rows) > 1 {
		other := make([]accountRow, 0, len(rows)-1)
		otherIdx := make([]int, 0, len(rows)-1)
		for i := range rows {
			if i == activeIdx {
				continue
			}
			other = append(other, rows[i])
			otherIdx = append(otherIdx, i)
		}
		a.enrichUsageFull(other)
		for j, i := range otherIdx {
			rows[i] = other[j]
		}
	}

	// Only switch into activated accounts with confirmed quota (>= 2%).
	// Unactivated accounts (403 VALIDATION_REQUIRED / forbidden) are rejected by
	// Antigravity's language_server at startup until one-time phone QR onboarding
	// completes, so rotate (including --force) must never switch into them.
	candidateRow := selectRotateCandidate(rows, activeID)
	if candidateRow == nil && activeUnactivated {
		if a.JSON {
			if err := a.emitJSON(map[string]any{
				"rotated": false,
				"reason":  "active_unactivated",
				"account": activeAccount,
			}); err != nil {
				return a.failCode("output_error", "", "could not write JSON output")
			}
			return 0
		}
		fmt.Fprintf(a.Out, "Active account %s is not activated in Antigravity and no activated account has quota. Sign in to it manually or wait for a reset.\n", activeRow.displayName())
		return 0
	}
	if candidateRow == nil {
		msg := "no activated accounts have remaining quota"
		if resetAcct, resetAt := earliestPriorityReset(rows); !resetAt.IsZero() {
			msg = fmt.Sprintf("no activated accounts have remaining quota (earliest reset: %s at %s)", resetAcct, resetAt.Local().Format("2006-01-02 15:04"))
		}
		return a.failCode("no_candidate_available", "Wait for quota to reset, or run `agy-auth usage --all` to inspect all accounts.", "%s", msg)
	}

	candidateAccount := reg.FindByID(candidateRow.ID)
	if candidateAccount == nil {
		return a.failCode("account_not_found", "Run `agy-auth list` to check stored accounts.", "candidate account %s not found in registry", candidateRow.ID)
	}

	targetModel := targetModelForAccountRow(candidateRow, storeDir, deskDir)

	res, code := a.switchAccount(reg, candidateAccount, switchOptions{
		Target:         tgt,
		Relaunch:       relaunch,
		NoRelaunch:     noRelaunch,
		NoAutoContinue: noAutoContinue,
		CloseDesktop:   closeDesktop,
		TargetModel:    targetModel,
		RefreshedToken: candidateRow.token,
	})
	if code != 0 {
		return code
	}

	reason := "active account exhausted"
	if force {
		reason = "forced"
	} else if activeRow == nil || activeRow.ID == "" {
		reason = "no active account"
	} else if activeUnactivated {
		reason = "active account not activated"
	} else if activeRow.UsageError != "" {
		reason = "active account error: " + activeRow.UsageError
	}

	if a.JSON {
		for _, warning := range res.Warnings {
			a.warn("%s", warning)
		}
		outMap := map[string]any{
			"rotated":            true,
			"forced":             force,
			"reason":             reason,
			"account":            res.Account,
			"target":             res.Target,
			"cli_switched":       res.CLISwitched,
			"desktop_switched":   res.DesktopSwitched,
			"desktop_relaunched": res.DesktopRelaunched,
		}
		if activeAccount != nil {
			outMap["previous_account"] = activeAccount
		}
		if res.CLISwitched {
			outMap["written"] = res.Report.Written
		}
		if res.ModelSwitched != "" {
			outMap["model_switched"] = res.ModelSwitched
		}
		if len(res.AutoContinued) > 0 {
			outMap["auto_continued"] = res.AutoContinued
		}
		if err := a.emitJSON(outMap); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(a.Out, "Rotated to %s (%s).\n", res.Account.DisplayName(), reason)
	if res.CLISwitched {
		fmt.Fprintf(a.Out, "Wrote %s.\n", strings.Join(res.Written, " and "))
	}
	if res.DesktopSwitched {
		fmt.Fprintln(a.Out, "Restored Antigravity Desktop profile.")
	}
	if res.DesktopRelaunched {
		fmt.Fprintln(a.Out, "Relaunched Antigravity Desktop.")
	}
	if res.ModelSwitched != "" {
		fmt.Fprintf(a.Out, "Selected model %s.\n", res.ModelSwitched)
	}
	if len(res.AutoContinued) > 0 {
		fmt.Fprintf(a.Out, "Auto-continued %d stopped conversation(s).\n", len(res.AutoContinued))
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(a.Err, "warning: %s\n", warning)
	}
	if res.CLISwitched {
		fmt.Fprintln(a.Out, "Restart agy for the new account to take effect (running sessions keep the old credential).")
	}
	return 0
}

// selectUnactivatedCandidate picks an account whose quota lookup is rejected
// with HTTP 403 (Google returns VALIDATION_REQUIRED until the account has been
// used once inside Antigravity). Such accounts have untouched quota, and
// switching into them lets Antigravity onboard them. The least recently used
// one is chosen so repeated failures cycle through the pool.
func selectUnactivatedCandidate(rows []accountRow, activeID string, reg *registry.Registry) *accountRow {
	var best *accountRow
	var bestUsed time.Time
	for i := range rows {
		row := &rows[i]
		if row.ID == "" || row.ID == activeID || row.UsageError != "forbidden" {
			continue
		}
		var used time.Time
		if acct := reg.FindByID(row.ID); acct != nil {
			used = acct.LastUsedAt
		}
		if best == nil || used.Before(bestUsed) {
			best = row
			bestUsed = used
		}
	}
	return best
}

func isTransientUsageError(code string) bool {
	switch code {
	case "timeout", "network", "unavailable", "invalid_response", "http_429", "http_500", "http_502", "http_503", "http_504":
		return true
	default:
		return false
	}
}

func selectRotateCandidate(rows []accountRow, activeID string) *accountRow {
	var best *accountRow
	var bestReset time.Time
	for i := range rows {
		row := &rows[i]
		if row.ID == "" || row.ID == activeID || !quotaAvailable(row) {
			continue
		}
		reset := quotaNextReset(row)
		if best == nil {
			best = row
			bestReset = reset
			continue
		}
		rClaude, rHasC, rArgon, rHasA := priorityGroupQuotas(row)
		bClaude, bHasC, bArgon, bHasA := priorityGroupQuotas(best)
		if (rHasC || rHasA) && (bHasC || bHasA) {
			rClaudeOK := rHasC && rClaude >= minCandidateQuotaPercent
			bClaudeOK := bHasC && bClaude >= minCandidateQuotaPercent
			if rClaudeOK != bClaudeOK {
				if rClaudeOK {
					best = row
					bestReset = reset
				}
				continue
			}
			if rClaudeOK && rClaude != bClaude {
				if rClaude > bClaude {
					best = row
					bestReset = reset
				}
				continue
			}
			if rArgon != bArgon {
				if rArgon > bArgon {
					best = row
					bestReset = reset
				}
				continue
			}
		}
		if !reset.IsZero() && (bestReset.IsZero() || reset.Before(bestReset)) {
			best = row
			bestReset = reset
			continue
		}
		if (reset.IsZero() && bestReset.IsZero()) || (!reset.IsZero() && reset.Equal(bestReset)) {
			if quotaRemaining(row) > quotaRemaining(best) {
				best = row
				bestReset = reset
			}
		}
	}
	return best
}

func quotaRemaining(row *accountRow) int {
	if row == nil {
		return 0
	}
	if claudePct, hasClaude, argonPct, hasArgon := priorityGroupQuotas(row); hasClaude || hasArgon {
		if hasClaude && claudePct > 0 {
			return claudePct
		}
		if hasArgon && argonPct > 0 {
			return argonPct
		}
		return 0
	}
	if groupDataUsable(row) {
		min := 100
		for _, group := range row.groups.Groups {
			for _, bucket := range group.Buckets {
				if bucket.RemainingPercent < min {
					min = bucket.RemainingPercent
				}
			}
		}
		return min
	}
	if row.Usage != nil && !row.Usage.Stale {
		if row.Usage.FiveHour != nil {
			return row.Usage.FiveHour.RemainingPercent
		}
		if row.Usage.Weekly != nil {
			return row.Usage.Weekly.RemainingPercent
		}
	}
	if row.detail != nil && !row.detail.Stale {
		lowest := row.detail.Lowest()
		if lowest != nil {
			return lowest.RemainingPercent
		}
	}
	return 0
}

func earliestPriorityReset(rows []accountRow) (string, time.Time) {
	var bestName string
	var bestTime time.Time
	for i := range rows {
		row := &rows[i]
		if row.UsageError != "" || !groupDataUsable(row) {
			continue
		}
		for _, group := range row.groups.Groups {
			name := strings.ToLower(strings.TrimSpace(group.Name))
			if !strings.Contains(name, "claude") && !strings.Contains(name, "argon") {
				continue
			}
			var unblock time.Time
			for _, bucket := range group.Buckets {
				if bucket.RemainingPercent >= minCandidateQuotaPercent {
					continue
				}
				if reset, ok := bucket.ResetAt(); ok {
					if unblock.IsZero() || reset.After(unblock) {
						unblock = reset
					}
				}
			}
			if !unblock.IsZero() && (bestTime.IsZero() || unblock.Before(bestTime)) {
				bestTime = unblock
				bestName = row.displayName()
			}
		}
	}
	return bestName, bestTime
}
