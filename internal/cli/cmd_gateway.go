package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/gateway"
)

const gatewayLaunchAgentLabel = "com.lfan.quotaswitch-gateway"

func (a *App) cmdGateway(args []string) int {
	if len(args) == 0 {
		a.printCommandHelp("gateway")
		return 0
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "help", "-h", "--help":
		a.printCommandHelp("gateway")
		return 0
	case "serve":
		a.Command = "gateway.serve"
		return a.cmdGatewayServe(rest)
	case "enable":
		a.Command = "gateway.enable"
		return a.cmdGatewayEnable(rest)
	case "disable":
		a.Command = "gateway.disable"
		return a.cmdGatewayDisable(rest)
	case "status":
		a.Command = "gateway.status"
		return a.cmdGatewayStatus(rest)
	case "models":
		a.Command = "gateway.models"
		return a.cmdGatewayModels(rest)
	default:
		return a.usageFor("gateway", "unknown gateway subcommand %q (expected serve, enable, disable, status, or models)", sub)
	}
}

func (a *App) gatewayCodexHome() string {
	if a.CodexStore != nil {
		return a.CodexStore.CodexHome
	}
	return ""
}

func (a *App) newGatewayServer(apiKey string) *gateway.Server {
	pool := gateway.NewPool(a.CodexStore, a.Store, a.Storage, nil)
	pool.CodexRunning = func() bool { return a.codexOps().Inspect().Running() }
	gateway.AgyModelSource = gateway.CachedAgyModelSource(pool, gateway.DefaultAgyEndpoint, 30*time.Minute)
	srv := gateway.NewServer(pool, a.gatewayCodexHome(), apiKey)
	srv.Logf = func(format string, args ...any) {
		fmt.Fprintf(a.Err, "%s %s\n", a.now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	return srv
}

func (a *App) cmdGatewayServe(args []string) int {
	fs := a.newFlagSet("gateway")
	listen := fs.String("listen", gateway.DefaultListenAddr, "")
	apiKey := fs.String("api-key", gateway.DefaultAPIKey, "")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	srv := a.newGatewayServer(*apiKey)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return a.failCode("gateway_listen_failed", "Another gateway may already be running; check `quotaswitch gateway status`.", "could not listen on %s: %v", *listen, err)
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	codexCount, agyCount := srv.Pool.Summary()
	fmt.Fprintf(a.Err, "quotaswitch gateway listening on http://%s (codex accounts: %d, agy accounts: %d)\n", ln.Addr(), codexCount, agyCount)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return a.failCode("gateway_failed", "", "gateway stopped: %v", err)
	}
	return 0
}

func gatewayLaunchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", gatewayLaunchAgentLabel+".plist")
}

func gatewayLaunchAgentPlist(binary, listen, apiKey string) string {
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, "Library", "Application Support", "quotaswitch")
	_ = os.MkdirAll(logDir, 0o700)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>gateway</string>
    <string>serve</string>
    <string>--listen=%s</string>
    <string>--api-key=%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>HOME</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, gatewayLaunchAgentLabel, binary, listen, apiKey, home,
		filepath.Join(logDir, "gateway.log"), filepath.Join(logDir, "gateway.err.log"))
}

func launchctlDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func (a *App) gatewaySettingsPath(flagVal string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal
	}
	if strings.TrimSpace(a.GatewaySettingsPath) != "" {
		return a.GatewaySettingsPath
	}
	return gateway.DefaultAntigravitySettingsPath()
}

func gatewayFallbackStatePath(storeDir string) string {
	if strings.TrimSpace(storeDir) == "" {
		return ""
	}
	return filepath.Join(storeDir, "gateway_fallback_state.json")
}

func isGatewayAutoFallback(storeDir string) bool {
	p := gatewayFallbackStatePath(storeDir)
	if p == "" {
		return false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var doc struct {
		AutoFallback bool `json:"auto_fallback"`
	}
	return json.Unmarshal(raw, &doc) == nil && doc.AutoFallback
}

func setGatewayAutoFallback(storeDir string, active bool) {
	p := gatewayFallbackStatePath(storeDir)
	if p == "" {
		return
	}
	if !active {
		_ = os.Remove(p)
		return
	}
	raw, _ := json.Marshal(map[string]bool{"auto_fallback": true})
	_ = os.WriteFile(p, raw, 0o600)
}

func (a *App) ensureGatewayLaunchAgent(listen, apiKey string) bool {
	binary, err := os.Executable()
	if err == nil {
		if resolved, rErr := filepath.EvalSymlinks(binary); rErr == nil {
			binary = resolved
		}
	}
	if brew := "/opt/homebrew/bin/quotaswitch"; fileExists(brew) {
		binary = brew
	}
	plistPath := gatewayLaunchAgentPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(plistPath, []byte(gatewayLaunchAgentPlist(binary, listen, apiKey)), 0o644); err != nil {
		return false
	}
	_ = exec.Command("launchctl", "bootout", launchctlDomain()+"/"+gatewayLaunchAgentLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", launchctlDomain(), plistPath).CombinedOutput(); err != nil {
		a.warn("could not load gateway LaunchAgent: %v %s", err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

func (a *App) restartAntigravityLanguageServer(noRelaunch, noAutoContinue bool, targetModel hubModelSpec) ([]string, bool) {
	if noRelaunch || a.Desktop == nil || a.Desktop.Ops != nil {
		return nil, false
	}
	now := a.now()
	storeDir := ""
	if a.Store != nil {
		storeDir = a.Store.Dir
	}
	var hubState preSwitchHubState
	if !noAutoContinue || !targetModel.Empty() {
		hubState = capturePreSwitchHubState(a.Desktop.ConfigDir, a.AntigravityDataDir, now)
		hubState.TargetModel = targetModel
		if noAutoContinue {
			hubState.ToContinueIDs = nil
		}
	}
	if !targetModel.Empty() {
		updateAntigravityStateModelEnum(a.AntigravityDataDir, targetModel)
	}
	if exec.Command("pgrep", "-x", "language_server").Run() != nil {
		return nil, false
	}
	if !canRestartLanguageServer(storeDir, now) {
		return nil, false
	}
	_ = exec.Command("pkill", "-x", "language_server").Run()
	time.Sleep(400 * time.Millisecond)
	if a.Storage != nil {
		if res, err := a.Storage.Read(); err == nil && len(res.Raw) > 0 {
			_ = a.Storage.Write(res.Raw)
		}
	}
	var resumed []string
	if (!noAutoContinue || !targetModel.Empty()) && hubState.HasDevToolsPort {
		resumed, _ = resumeConversationsInHub(a.Desktop.ConfigDir, hubState, a.AutoContinueTimeout, true)
	}
	return resumed, true
}

func (a *App) cmdGatewayEnable(args []string) int {
	fs := a.newFlagSet("gateway")
	listen := fs.String("listen", gateway.DefaultListenAddr, "")
	apiKey := fs.String("api-key", gateway.DefaultAPIKey, "")
	defaultModel := fs.String("default-model", "", "")
	settingsPath := fs.String("settings", "", "")
	noService := fs.Bool("no-service", false, "")
	noRelaunch := fs.Bool("no-relaunch", false, "")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	sPath := a.gatewaySettingsPath(*settingsPath)
	gatewayURL := "http://" + *listen
	a.useLiveAgyModels()
	cfg, err := gateway.EnableAntigravitySettings(sPath, gatewayURL, *apiKey, *defaultModel, a.gatewayCodexHome())
	if err != nil {
		return a.failCode("gateway_settings_failed", "Check permissions on ~/.antigravity.", "could not write Antigravity settings: %v", err)
	}
	if a.Store != nil {
		setGatewayAutoFallback(a.Store.Dir, false)
	}

	serviceInstalled := false
	if !*noService {
		serviceInstalled = a.ensureGatewayLaunchAgent(*listen, *apiKey)
	}
	_, relaunched := a.restartAntigravityLanguageServer(*noRelaunch, true, hubModelSpec{})

	if a.JSON {
		_ = a.emitJSON(map[string]any{
			"settings_path":     sPath,
			"gateway_url":       gatewayURL,
			"default_model":     cfg.Models.DefaultModelID,
			"models":            len(cfg.Models.List),
			"service_installed": serviceInstalled,
			"relaunched":        relaunched,
		})
		return 0
	}
	fmt.Fprintf(a.Out, "Antigravity gateway enabled\n  settings: %s\n  url:      %s\n  default:  %s\n  models:   %d\n",
		sPath, gatewayURL, cfg.Models.DefaultModelID, len(cfg.Models.List))
	if serviceInstalled {
		fmt.Fprintf(a.Out, "  service:  %s (running)\n", gatewayLaunchAgentLabel)
	}
	if relaunched {
		fmt.Fprintln(a.Out, "Reloaded Antigravity language_server.")
	} else {
		fmt.Fprintln(a.Out, "Restart Antigravity to load the new model list.")
	}
	return 0
}

func (a *App) cmdGatewayDisable(args []string) int {
	fs := a.newFlagSet("gateway")
	settingsPath := fs.String("settings", "", "")
	keepService := fs.Bool("keep-service", false, "")
	noRelaunch := fs.Bool("no-relaunch", false, "")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	sPath := a.gatewaySettingsPath(*settingsPath)
	if err := gateway.DisableAntigravitySettings(sPath); err != nil {
		return a.failCode("gateway_settings_failed", "", "could not update Antigravity settings: %v", err)
	}
	if a.Store != nil {
		setGatewayAutoFallback(a.Store.Dir, false)
	}
	if !*keepService {
		_ = exec.Command("launchctl", "bootout", launchctlDomain()+"/"+gatewayLaunchAgentLabel).Run()
		_ = os.Remove(gatewayLaunchAgentPath())
	}
	_, relaunched := a.restartAntigravityLanguageServer(*noRelaunch, true, hubModelSpec{})
	if a.JSON {
		_ = a.emitJSON(map[string]any{"disabled": true, "settings_path": sPath, "relaunched": relaunched})
		return 0
	}
	if relaunched {
		fmt.Fprintln(a.Out, "Antigravity gateway disabled and language_server reloaded into native Google Account sign-in.")
	} else {
		fmt.Fprintln(a.Out, "Antigravity gateway disabled. Restart Antigravity to return to its built-in models.")
	}
	return 0
}

func (a *App) cmdGatewayStatus(args []string) int {
	fs := a.newFlagSet("gateway")
	listen := fs.String("listen", gateway.DefaultListenAddr, "")
	settingsPath := fs.String("settings", gateway.DefaultAntigravitySettingsPath(), "")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	status := map[string]any{
		"settings_path":   *settingsPath,
		"settings_active": false,
		"service_file":    fileExists(gatewayLaunchAgentPath()),
		"running":         false,
	}
	if s, err := gateway.LoadAntigravitySettings(*settingsPath); err == nil && s.Gateway != nil {
		status["settings_active"] = s.Gateway.Enabled
		status["gateway_url"] = s.Gateway.URL
		if s.Models != nil {
			status["default_model"] = s.Models.DefaultModelID
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	if resp, err := client.Get("http://" + *listen + "/healthz"); err == nil {
		var health map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&health)
		_ = resp.Body.Close()
		status["running"] = resp.StatusCode == http.StatusOK
		status["health"] = health
	}
	if a.JSON {
		_ = a.emitJSON(status)
		return 0
	}
	fmt.Fprintf(a.Out, "settings:  %s (gateway enabled: %v)\n", *settingsPath, status["settings_active"])
	fmt.Fprintf(a.Out, "service:   %v\n", status["service_file"])
	fmt.Fprintf(a.Out, "running:   %v on http://%s\n", status["running"], *listen)
	if h, ok := status["health"].(map[string]any); ok {
		fmt.Fprintf(a.Out, "accounts:  codex=%v agy=%v, models=%v\n", h["codex_accounts"], h["agy_accounts"], h["models_count"])
	}
	return 0
}

func (a *App) cmdGatewayModels(args []string) int {
	fs := a.newFlagSet("gateway")
	if ok, code := a.parseFlags(fs, args); !ok {
		return code
	}
	codexHome := a.gatewayCodexHome()
	a.useLiveAgyModels()
	entries := gateway.CombinedModelEntries(codexHome)
	if a.JSON {
		_ = a.emitJSON(entries)
		return 0
	}
	for _, e := range entries {
		backend, _, _ := gateway.ResolveModel(e.ModelID, codexHome)
		fmt.Fprintf(a.Out, "%-28s %-6s %s\n", e.ModelID, backend, e.DisplayName)
	}
	return 0
}

func (a *App) useLiveAgyModels() {
	pool := gateway.NewPool(a.CodexStore, a.Store, a.Storage, nil)
	gateway.AgyModelSource = gateway.CachedAgyModelSource(pool, gateway.DefaultAgyEndpoint, 30*time.Minute)
}

func (a *App) findHealthyCodexAccount() (string, int, bool) {
	if a.CodexStore == nil || !a.CodexStore.Exists() {
		return "", 0, false
	}
	data, err := a.CodexStore.Load()
	if err != nil || len(data.Accounts) == 0 {
		return "", 0, false
	}
	now := a.now()
	_, _ = a.CodexStore.SyncActiveFromLiveAuth(data, now)
	if a.CodexUsage == nil {
		return data.Accounts[0].DisplayName(), 100, true
	}
	codexRunning := a.codexOps().Inspect().Running()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Check active Codex account first.
	if data.ActiveAccountID != nil {
		for _, acct := range data.Accounts {
			if acct.ID == *data.ActiveAccountID {
				if snap, uErr := a.CodexUsage.FetchUsage(ctx, a.CodexStore, acct, codexRunning); uErr == nil && snap.CandidateAvailable() {
					return acct.DisplayName(), snap.MinRemainingPercent(), true
				}
				break
			}
		}
	}
	for _, acct := range data.Accounts {
		if data.ActiveAccountID != nil && acct.ID == *data.ActiveAccountID {
			continue
		}
		if snap, uErr := a.CodexUsage.FetchUsage(ctx, a.CodexStore, acct, false); uErr == nil && snap.CandidateAvailable() {
			return acct.DisplayName(), snap.MinRemainingPercent(), true
		}
	}
	return "", 0, false
}

func (a *App) activateGatewayFallback(noRelaunch, noAutoContinue bool) (string, []string, bool, error) {
	sPath := a.gatewaySettingsPath("")
	a.useLiveAgyModels()
	cfg, err := gateway.EnableAntigravitySettings(sPath, gateway.DefaultGatewayURL, gateway.DefaultAPIKey, "", a.gatewayCodexHome())
	if err != nil {
		return "", nil, false, err
	}
	if a.Store != nil {
		setGatewayAutoFallback(a.Store.Dir, true)
	}
	if a.Desktop != nil && a.Desktop.Ops == nil {
		_ = a.ensureGatewayLaunchAgent(gateway.DefaultListenAddr, gateway.DefaultAPIKey)
	}
	defaultModel := "gpt-6.1-sol"
	if cfg.Models != nil && cfg.Models.DefaultModelID != "" {
		defaultModel = cfg.Models.DefaultModelID
	}
	resumed, relaunched := a.restartAntigravityLanguageServer(noRelaunch, noAutoContinue, hubModelSpec{})
	return defaultModel, resumed, relaunched, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
