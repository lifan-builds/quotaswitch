package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fikrilal/agy-auth/internal/agy"
	"github.com/fikrilal/agy-auth/internal/desktop"
	"github.com/fikrilal/agy-auth/internal/registry"
	"github.com/fikrilal/agy-auth/internal/usage"
)

func (a *App) cmdSwitch(args []string) int {
	fs := a.newFlagSet("switch")
	live := fs.Bool("live", false, "interactive live view")
	target := fs.String("target", "all", "switch target: all, cli, or desktop")
	relaunch := fs.Bool("relaunch", false, "relaunch Antigravity desktop after switching")
	noRelaunch := fs.Bool("no-relaunch", false, "do not restart Antigravity desktop language_server after switching")
	noAutoContinue := fs.Bool("no-auto-continue", false, "do not auto-continue quota-stopped conversations after relaunching Desktop")
	closeDesktop := fs.Bool("close-desktop", false, "gracefully close Antigravity desktop before switching")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		return a.usageFor("switch", "switch accepts at most one query")
	}
	if *live {
		if fs.NArg() > 0 {
			return a.usageFor("switch", "switch --live does not take a query")
		}
		return a.runLive(liveModeSwitch, false)
	}
	if a.JSON && fs.NArg() == 0 {
		return a.usageFor("switch", "switch --json requires a query")
	}

	tgt := strings.ToLower(strings.TrimSpace(*target))
	switch tgt {
	case "all", "cli", "desktop":
	default:
		return a.usageFor("switch", "target must be 'all', 'cli', or 'desktop'")
	}

	reg, err := a.Store.Load()
	if err != nil {
		return a.failCode("registry_error", "Run `agy-auth status` or `agy-auth path` to check storage.", "could not read the account registry")
	}
	if len(reg.Accounts) == 0 {
		return a.failCode("account_not_found", "Run `agy-auth add` or `agy-auth login` first.", "no stored accounts")
	}

	var account *registry.Account
	if fs.NArg() == 1 && fs.Arg(0) == "-" {
		if reg.Previous == "" {
			return a.failCode("previous_not_found", "Switch to another account first, then use `agy-auth switch -`.", "no previous account")
		}
		account = reg.FindByID(reg.Previous)
		if account == nil {
			return a.failCode("previous_not_found", "Switch to another account first, then use `agy-auth switch -`.", "the previous account is no longer stored")
		}
	} else if fs.NArg() == 1 {
		selected, code := a.resolveOneWithUsage(reg, fs.Arg(0), "Select an account to activate")
		if code != 0 {
			return code
		}
		account = selected
	} else {
		selected, err := a.chooseAccountWithUsage(reg.Accounts, reg.Active, "Select an account to activate", accountNumbers(reg))
		if errors.Is(err, errCancelled) {
			fmt.Fprintln(a.Err, "cancelled")
			return 1
		}
		if err != nil {
			return a.failCode("selection_failed", "Run `agy-auth list` and pass a query instead.", "could not select an account")
		}
		account = selected
	}

	res, code := a.switchAccount(reg, account, switchOptions{
		Target:         tgt,
		Relaunch:       *relaunch,
		NoRelaunch:     *noRelaunch,
		NoAutoContinue: *noAutoContinue,
		CloseDesktop:   *closeDesktop,
	})
	if code != 0 {
		return code
	}

	if a.JSON {
		for _, warning := range res.Warnings {
			a.warn("%s", warning)
		}
		outMap := map[string]any{
			"account":            res.Account,
			"target":             res.Target,
			"cli_switched":       res.CLISwitched,
			"desktop_switched":   res.DesktopSwitched,
			"desktop_relaunched": res.DesktopRelaunched,
		}
		if res.CLISwitched {
			outMap["written"] = res.Report.Written
		}
		if len(res.AutoContinued) > 0 {
			outMap["auto_continued"] = res.AutoContinued
		}
		if err := a.emitJSON(outMap); err != nil {
			return a.failCode("output_error", "", "could not write JSON output")
		}
		return 0
	}

	fmt.Fprintf(a.Out, "Switched to %s.\n", res.Account.DisplayName())
	if res.CLISwitched {
		fmt.Fprintf(a.Out, "Wrote %s.\n", strings.Join(res.Written, " and "))
	}
	if res.DesktopSwitched {
		fmt.Fprintln(a.Out, "Restored Antigravity Desktop profile.")
		if res.DesktopRelaunched {
			fmt.Fprintln(a.Out, "Relaunched Antigravity Desktop.")
		}
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

type switchOptions struct {
	Target         string
	Relaunch       bool
	NoRelaunch     bool
	NoAutoContinue bool
	CloseDesktop   bool
	TargetModel    hubModelSpec
	RefreshedToken *agy.TokenFile
}

func mergeRefreshedTokenBytes(raw []byte, tf *agy.TokenFile) []byte {
	if tf == nil || strings.TrimSpace(tf.Token.AccessToken) == "" {
		return raw
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	tok, ok := doc["token"].(map[string]any)
	if !ok {
		return raw
	}
	tok["access_token"] = tf.Token.AccessToken
	if strings.TrimSpace(tf.Token.Expiry) != "" {
		tok["expiry"] = tf.Token.Expiry
	}
	updated, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return updated
}

type switchResult struct {
	Account           *registry.Account
	Target            string
	Report            agy.WriteReport
	Written           []string
	CLISwitched       bool
	DesktopSwitched   bool
	DesktopRelaunched bool
	ModelSwitched     string
	AutoContinued     []string
	Warnings          []string
}

func (a *App) switchAccount(reg *registry.Registry, account *registry.Account, opts switchOptions) (*switchResult, int) {
	tgt := opts.Target
	now := a.now()
	var warnings []string
	var report agy.WriteReport
	var written []string
	cliSwitched := false

	if tgt == "all" || tgt == "cli" {
		raw, err := a.Store.LoadToken(account.ID)
		if err != nil {
			if tgt == "cli" {
				return nil, a.failCode("registry_error", "Run `agy-auth status` to check storage health.", "could not read the stored credential for %s", account.DisplayName())
			}
			warnings = append(warnings, fmt.Sprintf("no CLI credential stored for %s; skipped CLI switch", account.DisplayName()))
		} else {
			tf, err := agy.ParseTokenFile(raw)
			if err != nil {
				if tgt == "cli" {
					return nil, a.failCode("credential_error", "Remove and re-add the account, or sign in again with `agy-auth login`.", "stored credential for %s is unusable", account.DisplayName())
				}
				warnings = append(warnings, fmt.Sprintf("stored CLI credential for %s is unusable", account.DisplayName()))
			} else {
				refreshed := false
				if opts.RefreshedToken != nil && strings.TrimSpace(opts.RefreshedToken.Token.AccessToken) != "" && !opts.RefreshedToken.Expired(now) {
					if tf.Token.AccessToken != opts.RefreshedToken.Token.AccessToken || tf.Token.Expiry != opts.RefreshedToken.Token.Expiry {
						tf.Token.AccessToken = opts.RefreshedToken.Token.AccessToken
						tf.Token.Expiry = opts.RefreshedToken.Token.Expiry
						refreshed = true
					}
				} else if tf.Expired(now.Add(2*time.Minute)) && tf.HasRefreshToken() {
					if client, ok := a.UsageGroups.(*usage.Client); ok {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						if err := client.RefreshIfNeeded(ctx, tf); err == nil && !tf.Expired(now) {
							refreshed = true
						}
						cancel()
					}
				}
				if refreshed {
					raw = mergeRefreshedTokenBytes(raw, tf)
					if a.Store != nil {
						_ = a.Store.SaveToken(account.ID, raw)
					}
				}
				if tf.Expired(now) {
					if !tf.HasRefreshToken() {
						if tgt == "cli" {
							return nil, a.failCode("credential_error", "Sign in again with `agy-auth login`.", "%s has an expired access token and no refresh token", account.DisplayName())
						}
						warnings = append(warnings, fmt.Sprintf("%s has an expired access token and no refresh token", account.DisplayName()))
					} else {
						warnings = append(warnings, "the access token is expired; agy will refresh it on next start")
					}
				} else if !tf.HasRefreshToken() {
					warnings = append(warnings, "this snapshot has no refresh token; it will stop working when the access token expires")
				}

				report = a.Storage.Write(raw)
				if !report.OK() {
					if errors.Is(report.Error(), agy.ErrBackup) {
						return nil, a.failCode("backup_error", "Check the backup directory permissions.", "could not back up the current agy credential")
					}
					return nil, a.failCode("credential_write_failed", "Run `agy-auth status` to check keyring and file-mirror access.", "could not write the agy credential")
				}
				if err := report.Error(); err != nil {
					warnings = append(warnings, err.Error())
				}
				cliSwitched = true
				for _, source := range report.Written {
					if source == agy.SourceKeyring {
						written = append(written, "OS keyring")
					} else {
						written = append(written, "file mirror")
					}
				}
			}
		}
	}

	desktopSwitched := false
	desktopRelaunched := false
	var hubState preSwitchHubState
	if a.Desktop != nil && (tgt == "all" || tgt == "desktop") {
		if (!opts.NoAutoContinue || !opts.TargetModel.Empty()) && (!opts.NoRelaunch || opts.Relaunch) {
			hubState = capturePreSwitchHubState(a.Desktop.ConfigDir, a.AntigravityDataDir, now)
			hubState.TargetModel = opts.TargetModel
			if opts.NoAutoContinue {
				hubState.ToContinueIDs = nil
			}
		}
		if !opts.TargetModel.Empty() {
			updateAntigravityStateModelEnum(a.AntigravityDataDir, opts.TargetModel)
		}
		deskProfile := a.Store.DesktopProfilePath(account.ID)
		hasDeskProfile := desktop.HasProfile(deskProfile)
		if !hasDeskProfile {
			if tgt == "desktop" {
				return nil, a.failCode("desktop_profile_not_found", "Run `agy-auth add` while logged into Antigravity Desktop to capture its profile.", "no desktop profile stored for %s", account.DisplayName())
			}
			// Antigravity Hub: language_server reads the keyring token only at startup.
			// Restart it so Electron respawns it on the new account and reloads the window.
			writeDesktopUserScope(a.Desktop.ConfigDir, account.Subject)
			if a.Desktop.Ops == nil {
				if (opts.Relaunch || !opts.NoRelaunch) && cliSwitched {
					if raw, err := a.Store.LoadToken(account.ID); err == nil {
						if exec.Command("pgrep", "-x", "language_server").Run() == nil {
							if canRestartLanguageServer(a.Store.Dir, now) {
								_ = exec.Command("pkill", "-x", "language_server").Run()
								time.Sleep(400 * time.Millisecond)
								_ = a.Storage.Write(raw)
								desktopRelaunched = true
							} else {
								warnings = append(warnings, "skipped language_server restart to avoid Electron's 3-restarts-per-60s limit; wait 60s or reload the Antigravity window")
							}
						}
					}
				}
			} else if opts.Relaunch && cliSwitched {
				if err := a.Desktop.Relaunch(); err != nil {
					warnings = append(warnings, fmt.Sprintf("could not relaunch Antigravity Desktop: %v", err))
				} else {
					desktopRelaunched = true
				}
			}
		} else {
			running, pid, _ := a.Desktop.IsRunning()
			if running {
				if opts.Relaunch || opts.CloseDesktop {
					if err := a.Desktop.Terminate(pid, 5*time.Second); err != nil {
						warnings = append(warnings, fmt.Sprintf("failed to stop Antigravity Desktop (PID %d): %v", pid, err))
					}
				} else {
					if tgt == "desktop" {
						return nil, a.failCode("desktop_running", "Close Antigravity Desktop first, or run with --relaunch.", "Antigravity Desktop is currently running (PID %d)", pid)
					}
					warnings = append(warnings, fmt.Sprintf("Antigravity Desktop is currently running (PID %d); desktop profile was not modified to prevent database corruption. Run with --relaunch to restart Desktop.", pid))
				}
			}

			if stillRunning, _, _ := a.Desktop.IsRunning(); !stillRunning {
				if err := a.Desktop.Restore(deskProfile); err != nil {
					if tgt == "desktop" {
						return nil, a.failCode("desktop_restore_failed", "Check directory permissions for Antigravity desktop profile.", "could not restore desktop profile: %v", err)
					}
					warnings = append(warnings, fmt.Sprintf("could not restore desktop profile: %v", err))
				} else {
					desktopSwitched = true
					if opts.Relaunch {
						if err := a.Desktop.Relaunch(); err != nil {
							warnings = append(warnings, fmt.Sprintf("could not relaunch Antigravity Desktop: %v", err))
						} else {
							desktopRelaunched = true
						}
					}
				}
			}
		}
	}

	for i := range reg.Accounts {
		if reg.Accounts[i].ID == account.ID {
			reg.Accounts[i].LastUsedAt = now.UTC()
		}
	}
	if reg.Active != "" && reg.Active != account.ID {
		reg.Previous = reg.Active
	}
	if reg.Previous == account.ID {
		reg.Previous = ""
	}
	reg.Active = account.ID
	if err := a.Store.Save(reg); err != nil {
		message := "the credential was written but the registry could not be updated"
		if errors.Is(err, registry.ErrBackup) {
			message = "the credential was written but the registry could not be backed up"
		}
		return nil, a.errorDoc(1, ErrorDoc{
			Code:    "state_uncertain",
			Message: message,
			Hint:    "Run `agy-auth list` to verify the active account before retrying.",
		})
	}

	var autoContinued []string
	var modelSwitched string
	if !opts.TargetModel.Empty() {
		modelSwitched = opts.TargetModel.DisplayName()
	}
	if desktopRelaunched && (!opts.NoAutoContinue || !opts.TargetModel.Empty()) && hubState.HasDevToolsPort && a.Desktop != nil {
		resumed, resumeErr := resumeConversationsInHub(a.Desktop.ConfigDir, hubState, a.AutoContinueTimeout, a.Desktop.Ops == nil)
		autoContinued = resumed
		if resumeErr != nil && len(hubState.ToContinueIDs) > 0 {
			warnings = append(warnings, fmt.Sprintf("could not auto-continue stopped conversation(s): %v", resumeErr))
		}
	}

	return &switchResult{
		Account:           account,
		Target:            tgt,
		Report:            report,
		Written:           written,
		CLISwitched:       cliSwitched,
		DesktopSwitched:   desktopSwitched,
		DesktopRelaunched: desktopRelaunched,
		ModelSwitched:     modelSwitched,
		AutoContinued:     autoContinued,
		Warnings:          warnings,
	}, 0
}

func writeDesktopUserScope(configDir, subject string) {
	if configDir == "" || subject == "" {
		return
	}
	if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
		return
	}
	sum := sha256.Sum256([]byte(subject))
	_ = os.WriteFile(filepath.Join(configDir, "credential-user-scope"), []byte("u"+hex.EncodeToString(sum[:])[:16]), 0o600)
}

// canRestartLanguageServer caps restarts at 2 per 65s so Electron never hits
// its MAX_RESTARTS = 3 within 60s give-up threshold.
func canRestartLanguageServer(storeDir string, now time.Time) bool {
	if storeDir == "" {
		return true
	}
	statePath := filepath.Join(storeDir, "ls_restarts.json")
	var timestamps []int64
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &timestamps)
	}
	cutoff := now.Add(-65 * time.Second).UnixMilli()
	recent := make([]int64, 0, len(timestamps)+1)
	for _, ts := range timestamps {
		if ts >= cutoff {
			recent = append(recent, ts)
		}
	}
	if len(recent) >= 2 {
		return false
	}
	recent = append(recent, now.UnixMilli())
	if encoded, err := json.Marshal(recent); err == nil {
		_ = os.WriteFile(statePath, encoded, 0o600)
	}
	return true
}
