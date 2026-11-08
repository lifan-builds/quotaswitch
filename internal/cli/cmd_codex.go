package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fikrilal/agy-auth/internal/codex"
	"github.com/fikrilal/agy-auth/internal/usage"
)

const codexUsageTimeout = 45 * time.Second
const codexUsageWorkers = 6

func (a *App) codexOps() *codex.ProcessOps {
	if a.CodexOps != nil {
		return a.CodexOps
	}
	return codex.DefaultProcessOps()
}

func (a *App) cmdCodex(args []string) int {
	if len(args) == 0 {
		a.printCommandHelp("codex")
		return 0
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "help", "-h", "--help":
		a.printCommandHelp("codex")
		return 0
	case "list", "ls":
		a.Command = "codex.list"
		return a.cmdCodexList(rest)
	case "usage":
		a.Command = "codex.usage"
		return a.cmdCodexUsage(rest)
	case "switch", "use":
		a.Command = "codex.switch"
		return a.cmdCodexSwitch(rest)
	case "rotate":
		a.Command = "codex.rotate"
		return a.cmdCodexRotate(rest)
	case "add":
		a.Command = "codex.add"
		return a.cmdCodexAdd(rest)
	case "login":
		a.Command = "codex.login"
		return a.cmdCodexLogin(rest)
	case "warmup":
		a.Command = "codex.warmup"
		return a.cmdCodexWarmup(rest)
	case "status":
		a.Command = "codex.status"
		return a.cmdCodexStatus(rest)
	default:
		return a.usageFor("codex", "unknown codex subcommand %q (expected list, usage, switch, rotate, add, login, warmup, or status)", sub)
	}
}

func (a *App) requireCodexStore() (*codex.Store, int) {
	if a.CodexStore == nil {
		return nil, a.failCode("codex_unavailable", "Set CODEX_SWITCHER_HOME or check HOME directory.", "Codex account store is not configured")
	}
	return a.CodexStore, 0
}

func (a *App) buildCodexAccountRows(store *codex.Store, data *codex.AccountsStore, now time.Time, fetchUsage bool) []accountRow {
	activeID := ""
	if data.ActiveAccountID != nil {
		activeID = *data.ActiveAccountID
	}
	proc := a.codexOps().Inspect()
	codexRunning := proc.Running()

	emailCounts := make(map[string]int, len(data.Accounts))
	for _, acct := range data.Accounts {
		if e := strings.ToLower(acct.EmailString()); e != "" {
			emailCounts[e]++
		}
	}

	rows := make([]accountRow, len(data.Accounts))
	for i, acct := range data.Accounts {
		alias := ""
		email := acct.EmailString()
		if acct.Name != "" && !strings.EqualFold(acct.Name, email) {
			alias = acct.Name
		} else if email == "" {
			alias = acct.Name
		} else if emailCounts[strings.ToLower(email)] > 1 {
			alias = acct.WorkspaceLabel()
		}
		claims := codex.ParseIDTokenClaims(acct.AuthData.IDToken)
		var expiresAt *time.Time
		expired := false
		if claims.ExpiresAt != nil {
			t := *claims.ExpiresAt
			expiresAt = &t
			expired = !t.After(now)
		}
		created := acct.CreatedAt
		rows[i] = accountRow{
			Number:     i + 1,
			ID:         acct.ID,
			Active:     acct.ID == activeID,
			Stored:     true,
			Email:      email,
			Alias:      alias,
			AuthMethod: acct.AuthMode,
			ExpiresAt:  expiresAt,
			Expired:    expired,
			AddedAt:    &created,
			LastUsedAt: acct.LastUsedAt,
			Plan:       acct.PlanString(),
		}
		if rows[i].Active {
			rows[i].Source = "auth.json"
		}
	}

	if fetchUsage && a.CodexUsage != nil && len(data.Accounts) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), codexUsageTimeout)
		defer cancel()
		jobs := make(chan int, len(data.Accounts))
		workers := codexUsageWorkers
		if len(data.Accounts) < workers {
			workers = len(data.Accounts)
		}
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range jobs {
					acct := data.Accounts[idx]
					snap, err := a.CodexUsage.FetchUsage(ctx, store, acct, rows[idx].Active && codexRunning)
					if err != nil {
						rows[idx].UsageError = usage.ShortError(err)
						continue
					}
					if snap.Plan != "" {
						rows[idx].Plan = snap.Plan
					}
					uSnap := snap.ToUsageSnapshot()
					rows[idx].Usage = &uSnap
				}
			}()
		}
		for i := range data.Accounts {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}

	return rows
}

func (a *App) cmdCodexList(args []string) int {
	fs := a.newFlagSet("codex")
	activeOnly := fs.Bool("active", false, "only show the active Codex account")
	noSync := fs.Bool("no-sync", false, "do not reconcile live ~/.codex/auth.json tokens")
	noUsage := fs.Bool("no-usage", false, "do not fetch ChatGPT/Codex 5-hour and weekly quota")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex list takes no arguments")
	}

	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "Check ~/.codex-switcher/accounts.json.", "could not load Codex accounts: %v", err)
	}
	now := a.now()
	if !*noSync {
		_, _ = store.SyncActiveFromLiveAuth(data, now)
	}

	rows := a.buildCodexAccountRows(store, data, now, !*noUsage)
	if *activeOnly {
		filtered := make([]accountRow, 0, 1)
		for _, r := range rows {
			if r.Active {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	if a.JSON {
		var current *accountRow
		for i := range rows {
			if rows[i].Active {
				cp := rows[i]
				current = &cp
				break
			}
		}
		if err := a.emitJSON(map[string]any{
			"provider": "codex",
			"accounts": rows,
			"current":  current,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	if len(rows) == 0 {
		fmt.Fprintln(a.Out, "No Codex accounts stored.")
		fmt.Fprintln(a.Out, "Sign in with `codex` and run `agy-auth codex add`, or run `agy-auth codex login`.")
		return 0
	}
	a.renderAccountRows(rows, now)
	return 0
}

func (a *App) cmdCodexUsage(args []string) int {
	fs := a.newFlagSet("codex")
	all := fs.Bool("all", false, "show every stored Codex account")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		return a.usageFor("codex", "codex usage accepts at most one query")
	}

	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	if a.CodexUsage == nil {
		return a.failCode("quota_error", "", "Codex quota client is unavailable")
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	now := a.now()
	_, _ = store.SyncActiveFromLiveAuth(data, now)
	if len(data.Accounts) == 0 {
		return a.failCode("account_not_found", "Run `agy-auth codex add` or `agy-auth codex login` first.", "no stored Codex accounts")
	}

	targets := data.Accounts
	if !*all {
		var target *codex.StoredAccount
		if fs.NArg() == 1 {
			resolved, rErr := codex.Resolve(data, fs.Arg(0))
			if rErr != nil {
				return a.failCode("account_not_found", "Run `agy-auth codex list` to view accounts.", "%v", rErr)
			}
			target = resolved
		} else if data.ActiveAccountID != nil {
			resolved, rErr := codex.Resolve(data, *data.ActiveAccountID)
			if rErr == nil {
				target = resolved
			}
		}
		if target == nil {
			target = &data.Accounts[0]
		}
		targets = []codex.StoredAccount{*target}
	}

	subStore := &codex.AccountsStore{
		Version:         data.Version,
		Accounts:        targets,
		ActiveAccountID: data.ActiveAccountID,
	}
	rows := a.buildCodexAccountRows(store, subStore, now, true)
	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"provider": "codex",
			"accounts": rows,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}
	a.renderAccountRows(rows, now)
	return 0
}

func (a *App) cmdCodexAdd(args []string) int {
	fs := a.newFlagSet("codex")
	alias := fs.String("alias", "", "display name for the saved Codex account")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex add takes no positional arguments")
	}
	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	acct, added, err := store.AddFromLiveAuth(*alias, a.now())
	if err != nil {
		return a.failCode("codex_add_failed", "Sign in with `codex login` or use `agy-auth codex login`.", "%v", err)
	}
	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"provider": "codex",
			"added":    added,
			"account":  acct,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}
	verb := "Updated"
	if added {
		verb = "Added"
	}
	fmt.Fprintf(a.Out, "%s Codex account %s.\n", verb, acct.DisplayName())
	return 0
}

func (a *App) cmdCodexLogin(args []string) int {
	fs := a.newFlagSet("codex")
	alias := fs.String("alias", "", "display name for the Codex account")
	noBrowser := fs.Bool("no-browser", false, "print the OpenAI authorization URL without opening a browser")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex login takes no positional arguments")
	}
	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	cfg := codex.DefaultOAuthConfig()
	if a.CodexUsage != nil && a.CodexUsage.OAuth.Issuer != "" {
		cfg = a.CodexUsage.OAuth
	}
	session, err := codex.StartPKCELogin(cfg)
	if err != nil {
		return a.failCode("oauth_error", "", "could not start Codex OAuth callback server: %v", err)
	}
	defer func() { _ = session.Close() }()

	if !a.JSON {
		fmt.Fprintf(a.Out, "Open this URL to sign in to Codex:\n  %s\n\nWaiting for callback on %s...\n", session.AuthorizeURL, session.RedirectURI)
	}
	if !*noBrowser {
		_ = openBrowser(session.AuthorizeURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tokens, err := session.Wait(ctx)
	if err != nil {
		return a.failCode("oauth_error", "", "Codex OAuth sign-in failed: %v", err)
	}
	acct, added, err := store.UpsertTokens(tokens, *alias, true, a.now())
	if err != nil {
		return a.failCode("codex_store_error", "", "could not save Codex account: %v", err)
	}
	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"provider": "codex",
			"added":    added,
			"account":  acct,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}
	fmt.Fprintf(a.Out, "Signed in and activated Codex account %s.\n", acct.DisplayName())
	return 0
}

type codexSwitchResult struct {
	Account           codex.StoredAccount
	DesktopRelaunched bool
	AppServersReload  int
	AutoContinued     []string
}

func (a *App) performCodexSwitch(store *codex.Store, data *codex.AccountsStore, target codex.StoredAccount, relaunch, closeDesktop, noAutoContinue bool) (*codexSwitchResult, int) {
	now := a.now()
	ops := a.codexOps()
	proc := ops.Inspect()

	var recoveryTargets []codex.RecoveryTarget
	if !noAutoContinue {
		allPIDs := append([]int{}, proc.DesktopPIDs...)
		allPIDs = append(allPIDs, proc.CLIPIDs...)
		recoveryTargets = ops.CaptureRecovery(store.CodexHome, allPIDs, now)
	}

	if proc.Running() {
		if !relaunch && !closeDesktop {
			return nil, a.failCode(
				"codex_running",
				"Pass `--relaunch` or `--close-desktop` to gracefully close Codex before switching.",
				"Codex is currently running (Desktop PIDs: %v, CLI PIDs: %v)",
				proc.DesktopPIDs,
				proc.CLIPIDs,
			)
		}
		if len(proc.DesktopPIDs) > 0 {
			_ = ops.QuitDesktop(proc.DesktopBundles, proc.DesktopPIDs)
		}
		if len(proc.CLIPIDs) > 0 {
			_ = ops.StopCLI(proc.CLIPIDs)
		}
	}

	_, _ = store.SyncActiveFromLiveAuth(data, now)

	// Refresh target account tokens before writing auth.json if needed.
	if target.AuthMode == codex.AuthModeChatGPT && codex.NeedsRefresh(target.AuthData.IDToken, target.AuthData.AccessToken, now) && strings.TrimSpace(target.AuthData.RefreshToken) != "" {
		cfg := codex.DefaultOAuthConfig()
		if a.CodexUsage != nil && a.CodexUsage.OAuth.Issuer != "" {
			cfg = a.CodexUsage.OAuth
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		refreshed, rErr := codex.RefreshTokens(ctx, cfg, target.AuthData.RefreshToken)
		cancel()
		if rErr != nil {
			return nil, a.failCode("token_refresh_failed", "Run `agy-auth codex login` to re-authenticate this account.", "could not refresh Codex token for %s: %v", target.DisplayName(), rErr)
		}
		target.AuthData.AccessToken = refreshed.AccessToken
		target.AuthData.RefreshToken = refreshed.RefreshToken
		if refreshed.IDToken != "" {
			target.AuthData.IDToken = refreshed.IDToken
		}
		if refreshed.AccountID != nil {
			target.AuthData.AccountID = refreshed.AccountID
		}
	}

	ts := now.UTC()
	for i := range data.Accounts {
		if data.Accounts[i].ID == target.ID {
			data.Accounts[i].AuthData = target.AuthData
			data.Accounts[i].LastUsedAt = &ts
			target = data.Accounts[i]
			break
		}
	}
	activeID := target.ID
	data.ActiveAccountID = &activeID

	if err := store.WriteLiveAuth(target, now); err != nil {
		return nil, a.failCode("codex_write_error", "", "could not write %s: %v", store.LiveAuthPath(), err)
	}
	if err := store.Save(data); err != nil {
		return nil, a.failCode("codex_store_error", "", "could not save %s: %v", store.AccountsPath(), err)
	}

	reloaded := 0
	if len(proc.AppServerPIDs) > 0 {
		reloaded = ops.RestartAppServers(proc.AppServerPIDs)
	}

	relaunched := false
	if relaunch && len(proc.DesktopBundles) > 0 {
		if ops.ReopenDesktop(proc.DesktopBundles) == nil {
			relaunched = true
		}
	}

	var autoContinued []string
	if !noAutoContinue && len(recoveryTargets) > 0 {
		autoContinued = ops.ResumeRecovery(store.CodexHome, recoveryTargets)
	}

	return &codexSwitchResult{
		Account:           target,
		DesktopRelaunched: relaunched,
		AppServersReload:  reloaded,
		AutoContinued:     autoContinued,
	}, 0
}

func (a *App) cmdCodexSwitch(args []string) int {
	fs := a.newFlagSet("codex")
	relaunch := fs.Bool("relaunch", false, "gracefully close and reopen Codex Desktop when switching")
	closeDesktop := fs.Bool("close-desktop", false, "gracefully close running Codex processes before switching")
	noAutoContinue := fs.Bool("no-auto-continue", false, "do not auto-continue interrupted Codex sessions")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return a.usageFor("codex", "codex switch requires an account query (or `-` for previous)")
	}

	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	_, _ = store.SyncActiveFromLiveAuth(data, a.now())

	target, err := codex.Resolve(data, fs.Arg(0))
	if err != nil {
		return a.failCode("account_not_found", "Run `agy-auth codex list` to view stored accounts.", "%v", err)
	}

	res, exitCode := a.performCodexSwitch(store, data, *target, *relaunch, *closeDesktop, *noAutoContinue)
	if exitCode != 0 {
		return exitCode
	}

	if a.JSON {
		payload := map[string]any{
			"provider":           "codex",
			"account":            res.Account,
			"written":            store.LiveAuthPath(),
			"desktop_relaunched": res.DesktopRelaunched,
			"app_servers_reload": res.AppServersReload,
		}
		if len(res.AutoContinued) > 0 {
			payload["auto_continued"] = res.AutoContinued
		}
		if err := a.emitJSON(payload); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(a.Out, "Switched Codex to %s.\n", res.Account.DisplayName())
	fmt.Fprintf(a.Out, "Wrote %s.\n", store.LiveAuthPath())
	if res.DesktopRelaunched {
		fmt.Fprintln(a.Out, "Relaunched Codex Desktop.")
	}
	if len(res.AutoContinued) > 0 {
		fmt.Fprintf(a.Out, "Auto-continued %d interrupted Codex session(s).\n", len(res.AutoContinued))
	}
	return 0
}

func (a *App) cmdCodexRotate(args []string) int {
	fs := a.newFlagSet("codex")
	relaunch := fs.Bool("relaunch", false, "gracefully close and reopen Codex Desktop when rotating")
	closeDesktop := fs.Bool("close-desktop", false, "gracefully close running Codex processes before rotating")
	noAutoContinue := fs.Bool("no-auto-continue", false, "do not auto-continue interrupted Codex sessions")
	force := fs.Bool("force", false, "rotate even if the active Codex account still has remaining quota")
	watch := fs.Bool("watch", false, "continuously monitor Codex quota and auto-rotate")
	interval := fs.Duration("interval", 60*time.Second, "poll interval when --watch is enabled")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex rotate takes no arguments")
	}

	if *watch {
		poll := *interval
		if poll < 10*time.Second {
			poll = 10 * time.Second
		}
		a.Out = &timestampWriter{w: a.Out, now: a.now}
		a.Err = &timestampWriter{w: a.Err, now: a.now}
		lastCode := 0
		for {
			code := a.rotateCodexOnce(*relaunch, *closeDesktop, *force, *noAutoContinue)
			if code != 0 && lastCode == 0 {
				notifyDesktop("agy-auth (Codex)", "No Codex account has available quota. Re-login or wait for a window reset.")
			}
			lastCode = code
			time.Sleep(poll)
		}
	}

	return a.rotateCodexOnce(*relaunch, *closeDesktop, *force, *noAutoContinue)
}

func (a *App) rotateCodexOnce(relaunch, closeDesktop, force, noAutoContinue bool) int {
	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	if a.CodexUsage == nil {
		return a.failCode("quota_error", "", "Codex quota client is unavailable")
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	now := a.now()
	_, _ = store.SyncActiveFromLiveAuth(data, now)
	if len(data.Accounts) == 0 {
		if _, _, addErr := store.AddFromLiveAuth("", now); addErr == nil {
			data, _ = store.Load()
		}
	}
	if len(data.Accounts) == 0 {
		return a.failCode("account_not_found", "Run `agy-auth codex add` or `agy-auth codex login` first.", "no stored Codex accounts")
	}

	proc := a.codexOps().Inspect()
	codexRunning := proc.Running()

	activeIdx := -1
	if data.ActiveAccountID != nil {
		for i := range data.Accounts {
			if data.Accounts[i].ID == *data.ActiveAccountID {
				activeIdx = i
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexUsageTimeout)
	defer cancel()

	if !force && activeIdx >= 0 {
		activeAcct := data.Accounts[activeIdx]
		snap, uErr := a.CodexUsage.FetchUsage(ctx, store, activeAcct, codexRunning)
		if uErr == nil && !snap.Exhausted() {
			var autoContinued []string
			if !noAutoContinue {
				allPIDs := append([]int{}, proc.DesktopPIDs...)
				allPIDs = append(allPIDs, proc.CLIPIDs...)
				targets := a.codexOps().CaptureRecovery(store.CodexHome, allPIDs, now)
				if len(targets) > 0 {
					autoContinued = a.codexOps().ResumeRecovery(store.CodexHome, targets)
				}
			}
			if a.JSON {
				payload := map[string]any{
					"provider":          "codex",
					"rotated":           false,
					"reason":            "active_healthy",
					"account":           activeAcct,
					"remaining_percent": snap.MinRemainingPercent(),
					"usage":             snap,
				}
				if len(autoContinued) > 0 {
					payload["auto_continued"] = autoContinued
				}
				if err := a.emitJSON(payload); err != nil {
					return a.failCode("output_error", "", "could not write JSON output")
				}
				return 0
			}
			fiveH := 100
			if snap.FiveHour != nil {
				fiveH = snap.FiveHour.RemainingPercent
			}
			weekly := 100
			if snap.Weekly != nil {
				weekly = snap.Weekly.RemainingPercent
			}
			fmt.Fprintf(a.Out, "Active Codex account %s still has quota (5h=%d%%, weekly=%d%%); no switch needed.\n", activeAcct.DisplayName(), fiveH, weekly)
			return 0
		}
	}

	// Build ring-ordered candidate indices starting right after activeIdx.
	order := make([]int, 0, len(data.Accounts))
	if activeIdx >= 0 {
		for offset := 1; offset < len(data.Accounts); offset++ {
			order = append(order, (activeIdx+offset)%len(data.Accounts))
		}
	} else {
		for i := range data.Accounts {
			order = append(order, i)
		}
	}

	bestIdx := -1
	var bestSnap codex.QuotaSnapshot
	for _, idx := range order {
		cand := data.Accounts[idx]
		snap, uErr := a.CodexUsage.FetchUsage(ctx, store, cand, false)
		if uErr != nil || !snap.CandidateAvailable() {
			continue
		}
		if bestIdx < 0 || snap.MinRemainingPercent() > bestSnap.MinRemainingPercent() {
			bestIdx = idx
			bestSnap = snap
		}
	}

	if bestIdx < 0 {
		return a.failCode("quota_exhausted", "Run `agy-auth codex usage --all` to inspect reset times.", "no other Codex account currently has available quota")
	}

	candidate := data.Accounts[bestIdx]
	res, exitCode := a.performCodexSwitch(store, data, candidate, relaunch, closeDesktop || relaunch, noAutoContinue)
	if exitCode != 0 {
		return exitCode
	}

	if a.JSON {
		payload := map[string]any{
			"provider":           "codex",
			"rotated":            true,
			"account":            res.Account,
			"remaining_percent":  bestSnap.MinRemainingPercent(),
			"usage":              bestSnap,
			"desktop_relaunched": res.DesktopRelaunched,
		}
		if len(res.AutoContinued) > 0 {
			payload["auto_continued"] = res.AutoContinued
		}
		if err := a.emitJSON(payload); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(a.Out, "Rotated Codex to %s (%d%% min remaining).\n", res.Account.DisplayName(), bestSnap.MinRemainingPercent())
	if res.DesktopRelaunched {
		fmt.Fprintln(a.Out, "Relaunched Codex Desktop.")
	}
	if len(res.AutoContinued) > 0 {
		fmt.Fprintf(a.Out, "Auto-continued %d interrupted Codex session(s).\n", len(res.AutoContinued))
	}
	return 0
}

func (a *App) cmdCodexWarmup(args []string) int {
	fs := a.newFlagSet("codex")
	all := fs.Bool("all", false, "warm up every stored Codex account")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		return a.usageFor("codex", "codex warmup accepts at most one query")
	}

	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	if a.CodexUsage == nil {
		return a.failCode("quota_error", "", "Codex quota client is unavailable")
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	_, _ = store.SyncActiveFromLiveAuth(data, a.now())
	if len(data.Accounts) == 0 {
		return a.failCode("account_not_found", "", "no stored Codex accounts")
	}

	var targets []codex.StoredAccount
	if *all {
		targets = data.Accounts
	} else if fs.NArg() == 1 {
		resolved, rErr := codex.Resolve(data, fs.Arg(0))
		if rErr != nil {
			return a.failCode("account_not_found", "", "%v", rErr)
		}
		targets = []codex.StoredAccount{*resolved}
	} else if data.ActiveAccountID != nil {
		resolved, rErr := codex.Resolve(data, *data.ActiveAccountID)
		if rErr == nil {
			targets = []codex.StoredAccount{*resolved}
		}
	}
	if len(targets) == 0 {
		targets = []codex.StoredAccount{data.Accounts[0]}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var warmed []string
	var errs []string
	for _, t := range targets {
		if wErr := a.CodexUsage.Warmup(ctx, store, t); wErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.DisplayName(), wErr))
		} else {
			warmed = append(warmed, t.DisplayName())
		}
	}

	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"provider": "codex",
			"warmed":   warmed,
			"errors":   errs,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		if len(warmed) == 0 && len(errs) > 0 {
			return 1
		}
		return 0
	}
	for _, name := range warmed {
		fmt.Fprintf(a.Out, "Warmed up Codex account %s.\n", name)
	}
	for _, e := range errs {
		fmt.Fprintf(a.Err, "warning: %s\n", e)
	}
	if len(warmed) == 0 && len(errs) > 0 {
		return 1
	}
	return 0
}

func (a *App) cmdCodexStatus(args []string) int {
	fs := a.newFlagSet("codex")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.usageFor("codex", "codex status takes no arguments")
	}
	store, code := a.requireCodexStore()
	if code != 0 {
		return code
	}
	data, err := store.Load()
	if err != nil {
		return a.failCode("codex_store_error", "", "could not load Codex accounts: %v", err)
	}
	_, _ = store.SyncActiveFromLiveAuth(data, a.now())
	proc := a.codexOps().Inspect()

	activeName := "(none)"
	if data.ActiveAccountID != nil {
		if resolved, rErr := codex.Resolve(data, *data.ActiveAccountID); rErr == nil {
			activeName = resolved.DisplayName()
		}
	}

	if a.JSON {
		if err := a.emitJSON(map[string]any{
			"provider":      "codex",
			"accounts_path": store.AccountsPath(),
			"live_auth":     store.LiveAuthPath(),
			"account_count": len(data.Accounts),
			"active":        activeName,
			"processes":     proc,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(a.Out, "Codex accounts store: %s (%d accounts)\n", store.AccountsPath(), len(data.Accounts))
	fmt.Fprintf(a.Out, "Live Codex auth:      %s\n", store.LiveAuthPath())
	fmt.Fprintf(a.Out, "Active Codex account: %s\n", activeName)
	fmt.Fprintf(a.Out, "Running processes:    Desktop=%d CLI=%d AppServer=%d\n", len(proc.DesktopPIDs), len(proc.CLIPIDs), len(proc.AppServerPIDs))
	return 0
}

func (a *App) cmdListAllProviders(activeOnly, noSync, noUsage bool) int {
	if a.JSON {
		var codexRows []accountRow
		if a.CodexStore != nil && a.CodexStore.Exists() {
			if data, err := a.CodexStore.Load(); err == nil {
				if !noSync {
					_, _ = a.CodexStore.SyncActiveFromLiveAuth(data, a.now())
				}
				codexRows = a.buildCodexAccountRows(a.CodexStore, data, a.now(), !noUsage)
				if activeOnly {
					filtered := make([]accountRow, 0, 1)
					for _, r := range codexRows {
						if r.Active {
							filtered = append(filtered, r)
						}
					}
					codexRows = filtered
				}
			}
		}
		var agyRows []accountRow
		if reg, err := a.Store.Load(); err == nil {
			activeID := reg.Active
			if read, rErr := a.Storage.Read(); rErr == nil {
				if matched, _ := reg.Match(read.Raw); matched != nil {
					activeID = matched.ID
				}
			}
			agyRows = a.pickerRows(reg.Accounts, activeID, accountNumbers(reg))
			if !noUsage {
				a.enrichUsage(agyRows)
			}
			if activeOnly {
				filtered := make([]accountRow, 0, 1)
				for _, r := range agyRows {
					if r.Active {
						filtered = append(filtered, r)
					}
				}
				agyRows = filtered
			}
		}
		if err := a.emitJSON(map[string]any{
			"agy":   agyRows,
			"codex": codexRows,
		}); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintln(a.Out, "=== Antigravity (agy) ===")
	var agyArgs []string
	if activeOnly {
		agyArgs = append(agyArgs, "--active")
	}
	if noSync {
		agyArgs = append(agyArgs, "--no-sync")
	}
	if noUsage {
		agyArgs = append(agyArgs, "--no-usage")
	}
	_ = a.cmdList(append([]string{"--provider=agy"}, agyArgs...))

	fmt.Fprintln(a.Out, "\n=== OpenAI Codex (codex) ===")
	if a.CodexStore == nil || !a.CodexStore.Exists() {
		fmt.Fprintln(a.Out, "No Codex accounts stored.")
		return 0
	}
	return a.cmdCodexList(agyArgs)
}
