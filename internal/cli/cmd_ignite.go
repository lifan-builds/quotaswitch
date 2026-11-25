// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/backup"
	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/registry"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

const (
	defaultIgniteInterval = 5 * time.Minute
	igniteCooldown        = 10 * time.Minute
	igniteTimeout         = 60 * time.Second
)

type igniteItem struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Group    string `json:"group"`
	Window   string `json:"window,omitempty"`
	Model    string `json:"model,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type igniteReport struct {
	Provider      string       `json:"provider"`
	DryRun        bool         `json:"dry_run,omitempty"`
	Force         bool         `json:"force,omitempty"`
	Ignited       []igniteItem `json:"ignited"`
	AlreadyActive []igniteItem `json:"already_active"`
	Skipped       []igniteItem `json:"skipped"`
	Errors        []igniteItem `json:"errors"`
}

func (a *App) cmdIgnite(args []string) int {
	fs := a.newFlagSet("ignite")
	provider := fs.String("provider", "all", "provider pool to ignite: agy, codex, or all")
	force := fs.Bool("force", false, "ignite even when windows are already ticking or recently ignited")
	dryRun := fs.Bool("dry-run", false, "report untouched 100% windows without sending requests")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		return a.usageFor("ignite", "ignite accepts at most one account query")
	}

	prov := strings.ToLower(strings.TrimSpace(*provider))
	switch prov {
	case "agy", "codex", "all":
	default:
		return a.usageFor("ignite", "provider must be 'agy', 'codex', or 'all'")
	}

	query := ""
	if fs.NArg() == 1 {
		query = strings.TrimSpace(fs.Arg(0))
	}

	report := a.ignitePools(prov, query, *force, *dryRun)

	exit := 0
	if len(report.Ignited) == 0 && len(report.Errors) > 0 && len(report.AlreadyActive) == 0 {
		exit = 1
	}
	if a.JSON {
		return a.emitJSONExit(exit, report)
	}

	for _, item := range report.Ignited {
		verb := "Ignited"
		if *dryRun {
			verb = "Would ignite"
		}
		if item.Model != "" {
			fmt.Fprintf(a.Out, "%s %s %s [%s, %s] via %s.\n", verb, item.Provider, item.Account, item.Group, item.Window, item.Model)
		} else {
			fmt.Fprintf(a.Out, "%s %s %s [%s, %s].\n", verb, item.Provider, item.Account, item.Group, item.Window)
		}
	}
	if len(report.Ignited) == 0 && len(report.Errors) == 0 {
		if len(report.AlreadyActive) > 0 || len(report.Skipped) > 0 {
			fmt.Fprintf(a.Out, "All %d eligible quota window(s) are already counting down.\n", len(report.AlreadyActive)+len(report.Skipped))
		} else {
			fmt.Fprintln(a.Out, "No eligible accounts found to ignite.")
		}
	}
	for _, item := range report.Errors {
		fmt.Fprintf(a.Err, "warning: ignite %s %s [%s]: %s\n", item.Provider, item.Account, item.Group, item.Error)
	}
	return exit
}

func (a *App) runWatchIgnite(prov string) {
	report := a.ignitePools(prov, "", false, false)
	for _, item := range report.Ignited {
		if item.Model != "" {
			fmt.Fprintf(a.Out, "Auto-ignited %s %s [%s, %s] via %s.\n", item.Provider, item.Account, item.Group, item.Window, item.Model)
		} else {
			fmt.Fprintf(a.Out, "Auto-ignited %s %s [%s, %s].\n", item.Provider, item.Account, item.Group, item.Window)
		}
	}
	for _, item := range report.Errors {
		fmt.Fprintf(a.Err, "warning: auto-ignite %s %s [%s]: %s\n", item.Provider, item.Account, item.Group, item.Error)
	}
}

func (a *App) ignitePools(prov, query string, force, dryRun bool) igniteReport {
	report := igniteReport{
		Provider:      prov,
		DryRun:        dryRun,
		Force:         force,
		Ignited:       []igniteItem{},
		AlreadyActive: []igniteItem{},
		Skipped:       []igniteItem{},
		Errors:        []igniteItem{},
	}

	storeDir := ""
	if a.Store != nil {
		storeDir = a.Store.Dir
	}
	now := a.now()
	state := loadIgniteState(storeDir)
	dirty := false

	ctx, cancel := context.WithTimeout(context.Background(), igniteTimeout)
	defer cancel()

	if prov == "agy" || prov == "all" {
		if a.igniteAgyPool(ctx, query, force, dryRun, now, state, &report) {
			dirty = true
		}
	}
	if prov == "codex" || prov == "all" {
		if a.igniteCodexPool(ctx, query, force, dryRun, now, state, &report) {
			dirty = true
		}
	}

	if dirty && !dryRun && storeDir != "" {
		saveIgniteState(storeDir, state, now)
	}
	return report
}

func (a *App) igniteAgyPool(ctx context.Context, query string, force, dryRun bool, now time.Time, state map[string]time.Time, report *igniteReport) bool {
	if a.Store == nil || a.UsageGroups == nil {
		return false
	}
	reg, err := a.Store.Load()
	if err != nil || len(reg.Accounts) == 0 {
		return false
	}

	accounts := reg.Accounts
	if query != "" {
		matched := reg.Resolve(query)
		if len(matched) == 0 {
			return false
		}
		accounts = matched
	}

	type agyJob struct {
		acct   registry.Account
		token  *agy.TokenFile
		groups usage.Groups
		err    error
	}
	jobs := make([]agyJob, len(accounts))
	for i, acct := range accounts {
		jobs[i].acct = acct
		raw, loadErr := a.Store.LoadToken(acct.ID)
		if loadErr != nil {
			jobs[i].err = loadErr
			continue
		}
		tf, parseErr := agy.ParseTokenFile(raw)
		if parseErr != nil {
			jobs[i].err = parseErr
			continue
		}
		jobs[i].token = tf
	}

	workers := usageGroupWorkers
	if len(jobs) < workers {
		workers = len(jobs)
	}
	ch := make(chan int, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				if jobs[idx].token == nil {
					continue
				}
				groups, gErr := a.UsageGroups.FetchGroups(ctx, jobs[idx].token)
				jobs[idx].groups = groups
				jobs[idx].err = gErr
			}
		}()
	}
	for i := range jobs {
		if jobs[i].token != nil {
			ch <- i
		}
	}
	close(ch)
	wg.Wait()

	dirty := false
	for _, job := range jobs {
		label := job.acct.DisplayName()
		if job.err != nil {
			if errors.Is(job.err, usage.ErrForbidden) {
				report.Skipped = append(report.Skipped, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    "all",
					Status:   "skipped",
					Error:    "unactivated (forbidden)",
				})
			} else {
				report.Errors = append(report.Errors, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    "all",
					Status:   "error",
					Error:    usage.ShortError(job.err),
				})
			}
			continue
		}
		for _, group := range job.groups.Groups {
			if len(usage.IgniteModelsForGroup(group)) == 0 {
				continue
			}
			needs, win := usage.GroupNeedsIgnite(group, now)
			if !force && !needs {
				report.AlreadyActive = append(report.AlreadyActive, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    group.Name,
					Status:   "already_active",
				})
				continue
			}
			if !needs {
				win = "forced"
			}
			key := igniteStateKey("agy", job.acct.ID, group.Name)
			if !force {
				if last, ok := state[key]; ok && now.Sub(last) < igniteCooldown {
					report.Skipped = append(report.Skipped, igniteItem{
						Provider: "agy",
						Account:  label,
						Group:    group.Name,
						Window:   win,
						Status:   "cooldown",
					})
					continue
				}
			}
			if dryRun {
				models := usage.IgniteModelsForGroup(group)
				model := ""
				if len(models) > 0 {
					model = models[0]
				}
				report.Ignited = append(report.Ignited, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    group.Name,
					Window:   win,
					Model:    model,
					Status:   "would_ignite",
				})
				continue
			}
			if a.UsageIgniter == nil {
				report.Errors = append(report.Errors, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    group.Name,
					Window:   win,
					Status:   "error",
					Error:    "Antigravity igniter is unavailable",
				})
				continue
			}
			modelUsed, iErr := a.UsageIgniter.IgniteGroup(ctx, job.token, group)
			if iErr != nil {
				report.Errors = append(report.Errors, igniteItem{
					Provider: "agy",
					Account:  label,
					Group:    group.Name,
					Window:   win,
					Status:   "error",
					Error:    iErr.Error(),
				})
				continue
			}
			state[key] = now.UTC()
			dirty = true
			report.Ignited = append(report.Ignited, igniteItem{
				Provider: "agy",
				Account:  label,
				Group:    group.Name,
				Window:   win,
				Model:    modelUsed,
				Status:   "ignited",
			})
		}
	}
	return dirty
}

func (a *App) igniteCodexPool(ctx context.Context, query string, force, dryRun bool, now time.Time, state map[string]time.Time, report *igniteReport) bool {
	if a.CodexStore == nil || !a.CodexStore.Exists() || a.CodexUsage == nil {
		return false
	}
	data, err := a.CodexStore.Load()
	if err != nil || len(data.Accounts) == 0 {
		return false
	}
	_, _ = a.CodexStore.SyncActiveFromLiveAuth(data, now)

	activeID := ""
	if data.ActiveAccountID != nil {
		activeID = *data.ActiveAccountID
	}
	codexRunning := a.codexOps().Inspect().Running()

	var targets []codex.StoredAccount
	if query != "" {
		resolved, rErr := codex.Resolve(data, query)
		if rErr != nil {
			return false
		}
		if resolved.AuthMode != codex.AuthModeAPIKey {
			targets = []codex.StoredAccount{*resolved}
		}
	} else {
		for _, acct := range data.Accounts {
			if acct.AuthMode == codex.AuthModeAPIKey {
				continue
			}
			targets = append(targets, acct)
		}
	}
	if len(targets) == 0 {
		return false
	}

	type codexJob struct {
		acct codex.StoredAccount
		snap codex.QuotaSnapshot
		err  error
	}
	jobs := make([]codexJob, len(targets))
	for i, acct := range targets {
		jobs[i].acct = acct
	}

	workers := codexUsageWorkers
	if len(jobs) < workers {
		workers = len(jobs)
	}
	ch := make(chan int, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				acct := jobs[idx].acct
				snap, uErr := a.CodexUsage.FetchUsage(ctx, a.CodexStore, acct, acct.ID == activeID && codexRunning)
				jobs[idx].snap = snap
				jobs[idx].err = uErr
			}
		}()
	}
	for i := range jobs {
		ch <- i
	}
	close(ch)
	wg.Wait()

	// Reload store in case FetchUsage rotated single-use refresh tokens on disk.
	freshByID := make(map[string]codex.StoredAccount, len(targets))
	if freshData, loadErr := a.CodexStore.Load(); loadErr == nil && freshData != nil {
		for _, acct := range freshData.Accounts {
			freshByID[acct.ID] = acct
		}
	}

	dirty := false
	for _, job := range jobs {
		acct := job.acct
		if fresh, ok := freshByID[acct.ID]; ok {
			acct = fresh
		}
		label := acct.DisplayName()
		if job.err != nil {
			report.Errors = append(report.Errors, igniteItem{
				Provider: "codex",
				Account:  label,
				Group:    "Codex",
				Status:   "error",
				Error:    usage.ShortError(job.err),
			})
			continue
		}
		needs, win := job.snap.NeedsIgnite(now)
		if !force && !needs {
			report.AlreadyActive = append(report.AlreadyActive, igniteItem{
				Provider: "codex",
				Account:  label,
				Group:    "Codex",
				Status:   "already_active",
			})
			continue
		}
		if !needs {
			win = "forced"
		}
		key := igniteStateKey("codex", acct.ID, "codex")
		if !force {
			if last, ok := state[key]; ok && now.Sub(last) < igniteCooldown {
				report.Skipped = append(report.Skipped, igniteItem{
					Provider: "codex",
					Account:  label,
					Group:    "Codex",
					Window:   win,
					Status:   "cooldown",
				})
				continue
			}
		}
		if dryRun {
			report.Ignited = append(report.Ignited, igniteItem{
				Provider: "codex",
				Account:  label,
				Group:    "Codex",
				Window:   win,
				Model:    "gpt-6.2-sol",
				Status:   "would_ignite",
			})
			continue
		}
		if wErr := a.CodexUsage.WarmupAccount(ctx, a.CodexStore, acct, acct.ID == activeID && codexRunning); wErr != nil {
			report.Errors = append(report.Errors, igniteItem{
				Provider: "codex",
				Account:  label,
				Group:    "Codex",
				Window:   win,
				Status:   "error",
				Error:    wErr.Error(),
			})
			continue
		}
		state[key] = now.UTC()
		dirty = true
		report.Ignited = append(report.Ignited, igniteItem{
			Provider: "codex",
			Account:  label,
			Group:    "Codex",
			Window:   win,
			Model:    "gpt-6.2-sol",
			Status:   "ignited",
		})
	}
	return dirty
}

func igniteStateKey(provider, accountID, group string) string {
	return provider + ":" + accountID + ":" + strings.ToLower(strings.TrimSpace(group))
}

func loadIgniteState(storeDir string) map[string]time.Time {
	out := make(map[string]time.Time)
	if storeDir == "" {
		return out
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "ignite_state.json"))
	if err != nil {
		return out
	}
	var parsed map[string]string
	if json.Unmarshal(raw, &parsed) != nil {
		return out
	}
	for k, v := range parsed {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			out[k] = ts.UTC()
		}
	}
	return out
}

func saveIgniteState(storeDir string, state map[string]time.Time, now time.Time) {
	if storeDir == "" {
		return
	}
	encoded := make(map[string]string, len(state))
	cutoff := now.Add(-24 * time.Hour)
	for k, ts := range state {
		if ts.After(cutoff) {
			encoded[k] = ts.UTC().Format(time.RFC3339)
		}
	}
	raw, err := json.MarshalIndent(encoded, "", "  ")
	if err != nil {
		return
	}
	_ = backup.WriteFileAtomic(filepath.Join(storeDir, "ignite_state.json"), raw, 0o600)
}
