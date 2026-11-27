// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

type OptionSpec struct {
	Flag        string
	Description string
}

type CommandSpec struct {
	Name        string
	Aliases     []string
	Group       string
	Synopsis    string
	Summary     string
	Usage       []string
	Description string
	Options     []OptionSpec
	Examples    []string
	Notes       []string
}

var commandSpecs = []CommandSpec{
	{
		Name:     "list",
		Aliases:  []string{"ls"},
		Group:    "Account management",
		Synopsis: "list, ls",
		Summary:  "List stored accounts and the live agy account",
		Usage:    []string{"quotaswitch list [--provider <agy|codex|all>] [--active] [--no-sync] [--no-usage] [--live]"},
		Description: "List stored accounts. The live agy credential is marked with *.\n" +
			"The active account snapshot is refreshed automatically when agy has rotated\n" +
			"its tokens; pass --no-sync to keep this command read-only. Plan and shared\n" +
			"5-hour/weekly quota are fetched when available; --no-usage skips that lookup.\n" +
			"--live opens a refreshing interactive view.",
		Options: []OptionSpec{
			{Flag: "--provider <name>", Description: "Provider to list: agy, codex, or all (default \"agy\")"},
			{Flag: "--active", Description: "Only show the active account"},
			{Flag: "--no-sync", Description: "Do not update the stored snapshot of the live account"},
			{Flag: "--no-usage", Description: "Do not fetch plan and quota data"},
			{Flag: "--live", Description: "Interactive live view"},
		},
		Examples: []string{"quotaswitch list", "quotaswitch list --active", "quotaswitch list --no-usage", "quotaswitch list --live"},
	},
	{
		Name:     "add",
		Group:    "Account management",
		Synopsis: "add",
		Summary:  "Save the account currently signed in to agy",
		Usage:    []string{"quotaswitch add [--alias <alias>] [--target <target>]"},
		Description: "Save the account currently signed in to agy and Antigravity Desktop. This does not change the active\n" +
			"account or contact Google. Use \"quotaswitch login\" to add another account.",
		Options: []OptionSpec{
			{Flag: "--alias <alias>", Description: "Short name used by switch/remove queries"},
			{Flag: "--target <target>", Description: "Capture target: all, cli, or desktop (default \"all\")"},
		},
	},
	{
		Name:     "login",
		Group:    "Account management",
		Synopsis: "login",
		Summary:  "Add an account through Google OAuth",
		Usage: []string{
			"quotaswitch login [--alias <alias>] [--auth-method <method>]",
			"               [--no-browser] [--via-agy] [--agy <path>]",
		},
		Description: "Add an account through Google OAuth. The default flow prints an authorization\n" +
			"URL, opens a browser when possible, and asks for the code shown by\n" +
			"antigravity.google/oauth-callback.",
		Options: []OptionSpec{
			{Flag: "--alias <alias>", Description: "Short name used by switch/remove queries"},
			{Flag: "--auth-method <name>", Description: "Stored auth_method value (default \"consumer\")"},
			{Flag: "--no-browser", Description: "Only print the authorization URL"},
			{Flag: "--via-agy", Description: "Run agy in an isolated login directory; import only on success"},
			{Flag: "--agy <path>", Description: "agy executable for OAuth client discovery or --via-agy (default: search PATH)"},
		},
		Examples: []string{"quotaswitch login --alias work"},
	},
	{
		Name:     "switch",
		Aliases:  []string{"use"},
		Group:    "Account management",
		Synopsis: "switch, use [query]",
		Summary:  "Switch the active agy account",
		Usage:    []string{"quotaswitch switch [<query>|-] [--live] [--target <target>] [--relaunch] [--no-relaunch] [--no-auto-continue] [--close-desktop]"},
		Description: "Switch the active agy and Antigravity Desktop credentials. Without a query, an interactive picker is\n" +
			"shown. Queries accept a row number from \"quotaswitch list\", an alias, an exact\n" +
			"email, an email substring, or an account ID prefix. --live opens a refreshing\n" +
			"view and switches on Enter. When Antigravity Desktop restarts, conversations\n" +
			"stopped by quota exhaustion are automatically continued unless --no-auto-continue is passed.",
		Options: []OptionSpec{
			{Flag: "--target <target>", Description: "Switch target: all, cli, or desktop (default \"all\")"},
			{Flag: "--relaunch", Description: "Relaunch Antigravity Desktop after switching"},
			{Flag: "--no-relaunch", Description: "Do not restart Antigravity Desktop language_server after switching"},
			{Flag: "--no-auto-continue", Description: "Do not auto-continue stopped conversations after restarting Desktop"},
			{Flag: "--close-desktop", Description: "Gracefully close Antigravity Desktop before switching"},
			{Flag: "--live", Description: "Interactive live view; select and press Enter to switch"},
		},
		Examples: []string{
			"quotaswitch switch work",
			"quotaswitch switch work --relaunch",
			"quotaswitch switch work --target=cli",
			"quotaswitch switch 2",
			"quotaswitch switch --live",
		},
		Notes: []string{
			"Restart agy after switching; an already-running agy process keeps the old",
			"credential in memory.",
		},
	},
	{
		Name:     "rotate",
		Group:    "Account management",
		Synopsis: "rotate",
		Summary:  "Automatically switch to an account with available quota",
		Usage:    []string{"quotaswitch rotate [--provider <agy|codex|all>] [--target <target>] [--relaunch] [--no-relaunch] [--no-auto-continue] [--close-desktop] [--force] [--watch] [--interval <duration>]"},
		Description: "Check 5-hour quota across all stored accounts and automatically switch to\n" +
			"the healthiest account with remaining quota when the active account or active model\n" +
			"group is exhausted, then auto-continue stopped conversations in Antigravity and/or Codex Desktop.\n" +
			"Use --provider=all with --watch to monitor both Antigravity and Codex in one daemon.\n" +
			"Use --force to switch even if the active account is not exhausted.",
		Options: []OptionSpec{
			{Flag: "--provider <name>", Description: "Provider to rotate: agy, codex, or all (default \"agy\")"},
			{Flag: "--target <target>", Description: "Switch target: all, cli, or desktop (default \"all\")"},
			{Flag: "--relaunch", Description: "Relaunch Antigravity Desktop after switching"},
			{Flag: "--no-relaunch", Description: "Do not restart Antigravity Desktop language_server after switching"},
			{Flag: "--no-auto-continue", Description: "Do not auto-continue stopped conversations after restarting Desktop"},
			{Flag: "--close-desktop", Description: "Gracefully close Antigravity Desktop before switching"},
			{Flag: "--force", Description: "Rotate even if the active account still has remaining quota"},
			{Flag: "--watch", Description: "Continuously monitor quota and auto-rotate in the background"},
			{Flag: "--interval <duration>", Description: "Poll interval when --watch is enabled (default 1m0s)"},
			{Flag: "--no-ignite", Description: "Disable automatic ignition of untouched 100% rolling windows in --watch"},
			{Flag: "--ignite-interval <duration>", Description: "Interval between full-pool ignition checks in --watch (default 5m0s)"},
		},
		Examples: []string{
			"quotaswitch rotate",
			"quotaswitch rotate --relaunch",
			"quotaswitch rotate --force",
			"quotaswitch rotate --target=cli",
		},
		Notes: []string{
			"Restart agy after switching; an already-running agy process keeps the old",
			"credential in memory.",
		},
	},
	{
		Name:     "ignite",
		Aliases:  []string{"warmup"},
		Group:    "Account management",
		Synopsis: "ignite, warmup [query]",
		Summary:  "Start rolling 5h/weekly refresh timers on untouched 100% quota pools",
		Usage:    []string{"quotaswitch ignite [<query>] [--provider <agy|codex|all>] [--dry-run] [--force]"},
		Description: "Inspect rolling quota windows across Antigravity (Gemini + Claude/GPT groups)\n" +
			"and OpenAI Codex OAuth seats. Whenever an eligible pool is at an untouched 100%\n" +
			"(refresh timer not ticking yet), send a minimal 1-token request to start its\n" +
			"5-hour and weekly countdown clocks. API-key credit grants are never touched.\n" +
			"`quotaswitch rotate --watch` also runs this automatically every 5 minutes.",
		Options: []OptionSpec{
			{Flag: "--provider <name>", Description: "Provider pool to ignite: agy, codex, or all (default \"all\")"},
			{Flag: "--dry-run", Description: "Report untouched 100% windows without sending requests"},
			{Flag: "--force", Description: "Ignite even when windows are already counting down or in cooldown"},
		},
		Examples: []string{
			"quotaswitch ignite",
			"quotaswitch ignite --provider=agy",
			"quotaswitch ignite --dry-run",
			"quotaswitch ignite clfgame1 --force",
		},
	},
	{
		Name:     "remove",
		Aliases:  []string{"rm"},
		Group:    "Account management",
		Synopsis: "remove, rm [query]",
		Summary:  "Remove stored accounts",
		Usage:    []string{"quotaswitch remove [<query>...] [--all] [--logout] [--yes] [--live]"},
		Description: "Remove stored account snapshots. Removing an account does not sign agy out\n" +
			"unless --logout is passed. With no query, an interactive prompt is shown.\n" +
			"--live opens a checkbox view and removes checked accounts on Enter.",
		Options: []OptionSpec{
			{Flag: "--all", Description: "Remove every stored account"},
			{Flag: "--logout", Description: "Also delete the live agy credential when removing the active account"},
			{Flag: "--yes", Description: "Skip the confirmation prompt"},
			{Flag: "--live", Description: "Interactive live view; Space toggles and Enter removes"},
		},
		Examples: []string{"quotaswitch remove work", "quotaswitch remove --all --yes", "quotaswitch remove --live"},
	},
	{
		Name:     "alias",
		Group:    "Account management",
		Synopsis: "alias set|clear",
		Summary:  "Manage account aliases",
		Usage: []string{
			"quotaswitch alias set <query> <alias>",
			"quotaswitch alias clear <query>",
		},
		Description: "Aliases are short, whitespace-free names used by switch and remove.",
		Examples:    []string{"quotaswitch alias set work 2", "quotaswitch alias clear work"},
	},
	{
		Name:        "current",
		Group:       "Account management",
		Synopsis:    "current",
		Summary:     "Print the live agy account",
		Usage:       []string{"quotaswitch current"},
		Description: "Print the account currently stored in agy's credential store.",
	},
	{
		Name:     "status",
		Group:    "Account management",
		Synopsis: "status",
		Summary:  "Show credential storage health",
		Usage:    []string{"quotaswitch status"},
		Description: "Show the live agy account, token expiry, registry paths, keyring/file mirror\n" +
			"state, and whether agy is currently running.",
	},
	{
		Name:     "dash",
		Group:    "Account management",
		Synopsis: "dash [--watch]",
		Summary:  "Show Antigravity, Codex, API credit, and gateway status at a glance",
		Usage:    []string{"quotaswitch dash [--watch]"},
		Description: "Show one compact table per pool: Antigravity accounts with 5-hour/weekly quota,\n" +
			"Codex OAuth seat headroom, API credit grants ordered by expiry, and the local\n" +
			"gateway on 127.0.0.1:18455. --watch repaints in place every live refresh\n" +
			"interval (`quotaswitch config live`); with --json it emits one document per refresh.",
		Options:  []OptionSpec{{Flag: "--watch", Description: "Refresh continuously until interrupted"}},
		Examples: []string{"quotaswitch dash", "quotaswitch dash --watch", "quotaswitch --json dash"},
	},
	{
		Name:     "usage",
		Group:    "Account management",
		Synopsis: "usage [query] [--all]",
		Summary:  "Show Antigravity quota groups",
		Usage: []string{
			"quotaswitch usage [<query>]",
			"quotaswitch usage --all",
		},
		Description: "Show the same quota groups as `agy /usage`: Gemini Models, Claude and GPT\n" +
			"models, and any tier-specific group such as Ultra. Each group shows its 5-hour\n" +
			"and weekly remaining percentage plus reset times. --all compares every stored\n" +
			"account; a query selects one account.",
		Options:  []OptionSpec{{Flag: "--all", Description: "Show every stored account"}},
		Examples: []string{"quotaswitch usage", "quotaswitch usage work", "quotaswitch usage --all", "quotaswitch usage --json"},
	},
	{
		Name:     "codex",
		Group:    "Account management",
		Synopsis: "codex <list|usage|credits|billing|summary|exec|switch|rotate|reset|add|login|warmup|status>",
		Summary:  "Manage and auto-rotate OpenAI Codex accounts",
		Usage: []string{
			"quotaswitch codex list [--active] [--no-sync] [--no-usage]",
			"quotaswitch codex usage [<query> | --all]",
			"quotaswitch codex credits [--sync] [--set <query> --grant <usd> --used <usd> --remaining <usd> --admin-key-stdin]",
			"quotaswitch codex billing [--all] [--no-sync]",
			"quotaswitch codex summary [--no-usage]",
			"quotaswitch codex exec [--allow <path>] [-m <model>] [-C <dir>] [--json] [--timeout <duration>] <goal>",
			"quotaswitch codex switch <query|-> [--relaunch] [--close-desktop] [--no-auto-continue]",
			"quotaswitch codex rotate [--relaunch] [--close-desktop] [--no-auto-continue] [--force] [--watch] [--interval <duration>]",
			"quotaswitch codex reset [<query>] [--credit <id>] [--dry-run]",
			"quotaswitch codex add [--alias <name>]",
			"quotaswitch codex login [--alias <name>] [--no-browser] [--no-activate]",
			"quotaswitch codex warmup [<query> | --all]",
			"quotaswitch codex status",
		},
		Description: "Manage OpenAI Codex accounts stored in the Quotaswitch config home's\n" +
			"codex/accounts.json and ~/.codex/auth.json. Existing Codex Switcher stores\n" +
			"remain supported; `codex status` shows the selected paths. Reconciles\n" +
			"single-use refresh tokens automatically, fetches 5-hour and weekly ChatGPT\n" +
			"quota windows, automatically redeems soonest-expiring rate-limit reset credits\n" +
			"when a seat exhausts its quota, restarts IDE codex app-server extensions,\n" +
			"relaunches Codex Desktop, and auto-continues quota-stopped sessions via `codex queue`.",
		Options: []OptionSpec{
			{Flag: "--no-activate", Description: "Save a login without changing live auth or the active account"},
			{Flag: "--relaunch", Description: "Gracefully quit and reopen Codex Desktop when switching/rotating"},
			{Flag: "--close-desktop", Description: "Gracefully close running Codex processes before switching"},
			{Flag: "--no-auto-continue", Description: "Do not auto-continue interrupted Codex sessions"},
			{Flag: "--force", Description: "Rotate even if the active Codex account still has quota"},
			{Flag: "--watch", Description: "Continuously monitor Codex quota and auto-rotate"},
		},
		Examples: []string{
			"quotaswitch codex list",
			"quotaswitch codex usage --all",
			"quotaswitch codex switch 2 --relaunch",
			"quotaswitch codex rotate --relaunch",
			"quotaswitch rotate --provider=all --watch --interval=30s --relaunch",
		},
	},
	{
		Name:     "gateway",
		Group:    "Account management",
		Synopsis: "gateway <serve|enable|disable|status|models>",
		Summary:  "Run a local LLM gateway so Antigravity can use pooled Codex GPT models",
		Usage: []string{
			"quotaswitch gateway serve [--listen <addr>] [--api-key <key>]",
			"quotaswitch gateway enable [--listen <addr>] [--default-model <id>] [--no-service]",
			"quotaswitch gateway disable [--keep-service]",
			"quotaswitch gateway status",
			"quotaswitch gateway models",
		},
		Description: "Serve an OpenAI-compatible /v1/chat/completions endpoint that routes GPT\n" +
			"models to the pooled Codex accounts and Claude/Gemini models to the pooled\n" +
			"Antigravity accounts, failing over to the next account on quota errors.\n" +
			"`enable` writes ~/.antigravity/settings.json (gateway + model list) and\n" +
			"installs a LaunchAgent that keeps the gateway running on 127.0.0.1:18455.",
		Options: []OptionSpec{
			{Flag: "--listen <addr>", Description: "Listen address (default \"127.0.0.1:18455\")"},
			{Flag: "--api-key <key>", Description: "Key Antigravity sends to the gateway (default \"quotaswitch-local\")"},
			{Flag: "--default-model <id>", Description: "Default model in Antigravity (default: first Codex model)"},
			{Flag: "--settings <path>", Description: "Antigravity settings file (default ~/.antigravity/settings.json)"},
			{Flag: "--no-service", Description: "Write settings without installing the LaunchAgent"},
			{Flag: "--keep-service", Description: "Leave the LaunchAgent running when disabling"},
		},
		Examples: []string{
			"quotaswitch gateway enable",
			"quotaswitch gateway status",
			"quotaswitch gateway models",
			"quotaswitch gateway disable",
		},
	},
	{
		Name:     "import",
		Group:    "Import and export",
		Synopsis: "import <path> | --purge",
		Summary:  "Import credential JSON or rebuild the registry",
		Usage: []string{
			"quotaswitch import <file-or-directory> [--alias <alias>] [--activate]",
			"quotaswitch import --purge",
		},
		Description: "Import agy credential JSON files (the same format as\n" +
			"~/.gemini/antigravity-cli/antigravity-oauth-token). Invalid files are skipped\n" +
			"and reported. --alias is only valid for a single file. Imported accounts do not\n" +
			"become active unless --activate is passed (the first account is always selected).\n" +
			"--purge rebuilds registry.json from stored account snapshots, preserving aliases\n" +
			"and active/previous where possible.",
		Options: []OptionSpec{
			{Flag: "--alias <alias>", Description: "Alias for a single imported file"},
			{Flag: "--activate", Description: "Activate a single imported file"},
			{Flag: "--purge", Description: "Rebuild the registry from stored account snapshots"},
		},
		Examples: []string{"quotaswitch import ./token.json --alias work", "quotaswitch import --purge"},
	},
	{
		Name:     "export",
		Group:    "Import and export",
		Synopsis: "export [--dir <dir>]",
		Summary:  "Export stored credential JSON files",
		Usage:    []string{"quotaswitch export [--dir <directory>] [<query>...]"},
		Description: "Write stored credential JSON files to a directory (default: current directory)\n" +
			"with 0600 permissions. Export every account when no query is given. Per-file\n" +
			"results and a summary are printed.",
		Options: []OptionSpec{
			{Flag: "--dir <directory>", Description: "Destination directory (default: current directory)"},
		},
		Examples: []string{"quotaswitch export --dir ./backup"},
	},
	{
		Name:        "path",
		Aliases:     []string{"paths"},
		Group:       "Other",
		Synopsis:    "path, paths",
		Summary:     "Print storage paths",
		Usage:       []string{"quotaswitch path"},
		Description: "Print the registry, credential file, and keyring locations.",
	},
	{
		Name:     "clean",
		Group:    "Other",
		Synopsis: "clean [--dry-run]",
		Summary:  "Prune managed backups and orphan snapshots",
		Usage:    []string{"quotaswitch clean [--dry-run]"},
		Description: "Remove managed backup files beyond retention and snapshot files not\n" +
			"referenced by the registry. Snapshots are preserved when registry.json is\n" +
			"missing so the registry can be rebuilt.",
		Options: []OptionSpec{
			{Flag: "--dry-run", Description: "Show what would be removed"},
		},
		Examples: []string{"quotaswitch clean --dry-run", "quotaswitch clean"},
	},
	{
		Name:     "config",
		Group:    "Other",
		Synopsis: "config live [--interval <seconds>]",
		Summary:  "Show or set live refresh settings",
		Usage:    []string{"quotaswitch config live [--interval <seconds>]"},
		Description: "Show the effective live refresh interval, or set it to 5-3600 seconds.\n" +
			"The setting is stored in registry.json and applies to the live refresh loop,\n" +
			"dash --watch, and the quota cache.",
		Options:  []OptionSpec{{Flag: "--interval <seconds>", Description: "Live refresh interval (5-3600)"}},
		Examples: []string{"quotaswitch config live", "quotaswitch config live --interval 30"},
	},
	{
		Name:        "version",
		Group:       "Other",
		Synopsis:    "version [--check]",
		Summary:     "Print the version",
		Usage:       []string{"quotaswitch version", "quotaswitch version --check"},
		Description: "Print the quotaswitch version. --check queries the npm registry for a newer release.",
		Options:     []OptionSpec{{Flag: "--check", Description: "Check the latest published version"}},
		Examples:    []string{"quotaswitch version", "quotaswitch version --check"},
	},
	{
		Name:        "help",
		Group:       "Other",
		Synopsis:    "help [command]",
		Summary:     "Show help",
		Usage:       []string{"quotaswitch help [command]"},
		Description: "Show top-level help, or help for one command.",
	},
}

var specIndex = buildSpecIndex()

func buildSpecIndex() map[string]int {
	index := make(map[string]int, len(commandSpecs))
	for i := range commandSpecs {
		index[commandSpecs[i].Name] = i
		for _, alias := range commandSpecs[i].Aliases {
			index[alias] = i
		}
	}
	return index
}

func specForName(name string) *CommandSpec {
	index, ok := specIndex[name]
	if !ok {
		return nil
	}
	return &commandSpecs[index]
}

func canonicalCommand(name string) string {
	switch name {
	case "--version", "-v":
		return "version"
	case "-h", "--help":
		return "help"
	}
	if spec := specForName(name); spec != nil {
		return spec.Name
	}
	return name
}
