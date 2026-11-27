// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"fmt"
	"strings"
)

func (a *App) printHelp() int {
	var help strings.Builder
	fmt.Fprintln(&help, "quotaswitch - quota-driven multi-account switcher for Antigravity (agy) and OpenAI Codex")
	fmt.Fprintln(&help)
	fmt.Fprintln(&help, "Usage:")
	fmt.Fprintln(&help, "  quotaswitch <command> [options]")

	width := 20
	for _, spec := range commandSpecs {
		if len(spec.Synopsis) > width {
			width = len(spec.Synopsis)
		}
	}
	group := ""
	for _, spec := range commandSpecs {
		if spec.Group != group {
			fmt.Fprintf(&help, "\n%s:\n", spec.Group)
			group = spec.Group
		}
		fmt.Fprintf(&help, "  %-*s  %s\n", width, spec.Synopsis, spec.Summary)
	}

	fmt.Fprintln(&help)
	fmt.Fprintln(&help, "Global options:")
	fmt.Fprintf(&help, "  %-*s  %s\n", 12, "--json", "Machine-readable output")
	fmt.Fprintf(&help, "  %-*s  %s\n", 12, "--no-color", "Disable ANSI colors")

	fmt.Fprintln(&help)
	fmt.Fprintln(&help, "Examples:")
	for _, example := range []string{
		"quotaswitch add",
		"quotaswitch list",
		"quotaswitch switch personal",
		"quotaswitch login --alias work",
		"quotaswitch switch 2",
	} {
		fmt.Fprintf(&help, "  %s\n", example)
	}

	fmt.Fprintln(&help)
	fmt.Fprint(&help, helpFooter)
	return a.writeHelp(help.String())
}

func (a *App) printCommandHelp(command string) int {
	spec := specForName(command)
	if spec == nil {
		return a.usageTop("unknown help topic %q", command)
	}
	var help strings.Builder
	fmt.Fprintln(&help, "Usage:")
	for _, line := range spec.Usage {
		fmt.Fprintf(&help, "  %s\n", line)
	}
	if spec.Description != "" {
		fmt.Fprintln(&help)
		fmt.Fprintln(&help, spec.Description)
	}
	if len(spec.Options) > 0 {
		fmt.Fprintln(&help)
		fmt.Fprintln(&help, "Options:")
		width := optionWidth(spec.Options)
		for _, option := range spec.Options {
			fmt.Fprintf(&help, "  %-*s  %s\n", width, option.Flag, option.Description)
		}
	}
	if len(spec.Examples) > 0 {
		fmt.Fprintln(&help)
		fmt.Fprintln(&help, "Examples:")
		for _, example := range spec.Examples {
			fmt.Fprintf(&help, "  %s\n", example)
		}
	}
	if len(spec.Notes) > 0 {
		fmt.Fprintln(&help)
		fmt.Fprintln(&help, "Notes:")
		for _, note := range spec.Notes {
			fmt.Fprintf(&help, "  %s\n", note)
		}
	}
	return a.writeHelp(help.String())
}

func (a *App) writeHelp(help string) int {
	if a.JSON {
		return a.okJSON(map[string]string{"help": help})
	}
	fmt.Fprint(a.Out, help)
	return 0
}

func (a *App) usage(spec *CommandSpec, format string, args ...any) int {
	if spec == nil {
		return a.usageTop(format, args...)
	}
	message := fmt.Sprintf(format, args...)
	hint := fmt.Sprintf("Run `quotaswitch %s --help` for help and examples.", spec.Name)
	if a.JSON {
		return a.emitJSONError(2, ErrorDoc{Code: "usage", Message: message, Hint: hint})
	}
	fmt.Fprintf(a.Err, "error: %s\n\n", message)
	fmt.Fprintln(a.Err, "Usage:")
	for _, line := range spec.Usage {
		fmt.Fprintf(a.Err, "  %s\n", line)
	}
	fmt.Fprintln(a.Err)
	fmt.Fprintf(a.Err, "hint: %s\n", hint)
	return 2
}

func (a *App) usageFor(name, format string, args ...any) int {
	return a.usage(specForName(name), format, args...)
}

func (a *App) usageTop(format string, args ...any) int {
	message := fmt.Sprintf(format, args...)
	hint := "Run `quotaswitch help` for usage."
	if a.JSON {
		return a.emitJSONError(2, ErrorDoc{Code: "usage", Message: message, Hint: hint})
	}
	fmt.Fprintf(a.Err, "error: %s\n\n", message)
	fmt.Fprintln(a.Err, "Usage:")
	fmt.Fprintln(a.Err, "  quotaswitch <command> [options]")
	fmt.Fprintln(a.Err)
	fmt.Fprintf(a.Err, "hint: %s\n", hint)
	return 2
}

func optionWidth(options []OptionSpec) int {
	width := 12
	for _, option := range options {
		if len(option.Flag) > width {
			width = len(option.Flag)
		}
	}
	return width
}

const helpFooter = `Antigravity CLI keeps one credential in the OS keyring (service "gemini",
account "antigravity") with a file fallback at
~/.gemini/antigravity-cli/antigravity-oauth-token. quotaswitch stores additional
accounts in its own registry and writes the selected account back to agy.
Restart agy after switching.
`
