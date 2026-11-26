// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func newDashEnv(t *testing.T) (*testEnv, *codex.Store) {
	t.Helper()
	env := newTestEnv(t, true)
	app, _, errOut := env.app("")
	if code := app.Run([]string{"add", "--alias", "personal"}); code != 0 {
		t.Fatalf("add failed: %s", errOut)
	}
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	exp := testNow.Add(20 * time.Hour).UTC()
	key := "sk-dash"
	if err := codexStore.Save(&codex.AccountsStore{Version: 1, Accounts: []codex.StoredAccount{{
		ID: "api-1", Name: "api-exp", SubscriptionExpiresAt: &exp, AuthMode: codex.AuthModeAPIKey,
		AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &key}, CreatedAt: testNow,
	}}}); err != nil {
		t.Fatal(err)
	}
	cal, _, calErr := env.app("")
	cal.CodexStore = codexStore
	if code := cal.Run([]string{"codex", "credits", "--set", "api-exp", "--grant", "20", "--used", "5"}); code != 0 {
		t.Fatalf("calibrate failed: %s", calErr)
	}
	return env, codexStore
}

func TestDashJSONContract(t *testing.T) {
	env, codexStore := newDashEnv(t)
	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	if code := app.Run([]string{"--json", "dash"}); code != 0 {
		t.Fatalf("dash exit %d: %s", code, errOut)
	}
	var envelope struct {
		Command string     `json:"command"`
		Data    dashReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	d := envelope.Data
	if envelope.Command != "dash" || len(d.Antigravity) != 1 || !d.Antigravity[0].Active || d.Antigravity[0].Alias != "personal" {
		t.Fatalf("unexpected antigravity rows: %s", out)
	}
	if len(d.CodexSeats) != 0 || len(d.CreditGrants) != 1 || d.CreditGrants[0].RemainingUSD != 15 {
		t.Fatalf("unexpected codex data: %s", out)
	}
	if e := d.Codex.APICredits.EarliestExpiry; e == nil || e.Name != "api-exp" || e.ExpiresInHours != 20 {
		t.Fatalf("unexpected earliest expiry: %s", out)
	}
	if d.Gateway.Listen == "" || d.Gateway.SettingsActive {
		t.Fatalf("unexpected gateway status: %+v", d.Gateway)
	}
}

func TestDashTextAndWatchRepaint(t *testing.T) {
	env, codexStore := newDashEnv(t)
	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	app.Width = 100
	if code := app.Run([]string{"dash"}); code != 0 {
		t.Fatalf("dash exit %d: %s", code, errOut)
	}
	text := out.String()
	for _, want := range []string{"Antigravity  1 account(s) · active personal (user@example.com)", "Codex OAuth  0/0 with headroom", "API credits  $15.00 across 1/1 active org(s) · next expiry api-exp in 20h", "WARNING: $15.00 expiring <5d (api-exp in 20h)", "api-exp", "$15.00", "in 20h !", "Gateway  ", "on http://127.0.0.1:18455"} {
		if !strings.Contains(text, want) {
			t.Errorf("dash output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Errorf("non-watch, non-TTY output contains ANSI escapes:\n%q", text)
	}

	out.Reset()
	var frame []string
	if code := app.dashOnce(&frame); code != 0 {
		t.Fatalf("first frame exit %d: %s", code, errOut)
	}
	if !strings.HasPrefix(out.String(), "\x1b[H\x1b[2J") || len(frame) == 0 {
		t.Fatalf("first frame should clear the screen: %q", out.String())
	}
	out.Reset()
	if code := app.dashOnce(&frame); code != 0 {
		t.Fatalf("second frame exit %d: %s", code, errOut)
	}
	if got := out.String(); got != "" {
		t.Fatalf("unchanged frame repaint = %q, want no output", got)
	}

	out.Reset()
	app.Height = 5
	if code := app.dashOnce(&frame); code != 0 {
		t.Fatalf("clipped frame exit %d: %s", code, errOut)
	}
	if len(frame) != 5 || !strings.Contains(frame[4], "more line(s)") || !strings.Contains(out.String(), "\x1b[6;1H\x1b[K") {
		t.Fatalf("clipped frame = %q, output %q", frame, out.String())
	}
}

func TestDashCreditExpiryWarningThresholds(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	expSoon := testNow.Add(91*time.Hour + 30*time.Minute).UTC()   // 3d 19h (<5d)
	expSoon2 := testNow.Add(115*time.Hour + 30*time.Minute).UTC() // 4d 19h (<5d)
	expBoundary := testNow.Add(5 * 24 * time.Hour).UTC()          // exactly 5d (not <5d)
	expLater := testNow.Add(14 * 24 * time.Hour).UTC()            // 14d
	expExhausted := testNow.Add(48 * time.Hour).UTC()             // 2d but exhausted
	k1, k2, k3, k4, k5 := "sk-1", "sk-2", "sk-3", "sk-4", "sk-5"
	if err := codexStore.Save(&codex.AccountsStore{Version: 1, Accounts: []codex.StoredAccount{
		{ID: "api-1", Name: "api-soon", SubscriptionExpiresAt: &expSoon, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k1}, CreatedAt: testNow},
		{ID: "api-1b", Name: "api-soon2", SubscriptionExpiresAt: &expSoon2, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k5}, CreatedAt: testNow},
		{ID: "api-2", Name: "api-5d", SubscriptionExpiresAt: &expBoundary, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k2}, CreatedAt: testNow},
		{ID: "api-3", Name: "api-later", SubscriptionExpiresAt: &expLater, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k3}, CreatedAt: testNow},
		{ID: "api-4", Name: "api-empty", SubscriptionExpiresAt: &expExhausted, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k4}, CreatedAt: testNow},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"codex", "credits", "--set", "api-soon", "--grant", "94", "--remaining", "55.43"},
		{"codex", "credits", "--set", "api-soon2", "--grant", "120", "--remaining", "119.93"},
		{"codex", "credits", "--set", "api-5d", "--grant", "40", "--remaining", "40"},
		{"codex", "credits", "--set", "api-later", "--grant", "40", "--remaining", "40"},
		{"codex", "credits", "--set", "api-empty", "--grant", "40", "--used", "40", "--exhausted", "true"},
	} {
		cal, _, calErr := env.app("")
		cal.CodexStore = codexStore
		if code := cal.Run(args); code != 0 {
			t.Fatalf("calibrate %v failed: %s", args, calErr)
		}
	}

	app, out, errOut := env.app("")
	app.CodexStore = codexStore
	app.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	app.Width = 100
	if code := app.Run([]string{"dash"}); code != 0 {
		t.Fatalf("dash exit %d: %s", code, errOut)
	}
	plain := out.String()
	if !strings.Contains(plain, "WARNING: $175.36 expiring <5d (2 orgs; next api-soon in 3d 19h)") {
		t.Fatalf("missing <5d warning banner:\n%s", plain)
	}
	if !strings.Contains(plain, "in 3d 19h !") || !strings.Contains(plain, "in 4d 19h !") {
		t.Fatalf("expiring rows missing '!' marker:\n%s", plain)
	}
	if strings.Contains(plain, "in 5d !") || strings.Contains(plain, "in 14d !") {
		t.Fatalf("non-expiring row unexpectedly flagged:\n%s", plain)
	}

	// Verify ANSI color highlight on TTY: yellow for <5d (>24h), red for <=24h.
	out.Reset()
	app.OutTTY = true
	if code := app.Run([]string{"dash"}); code != 0 {
		t.Fatalf("tty dash exit %d: %s", code, errOut)
	}
	colored := out.String()
	if !strings.Contains(colored, "\x1b[1m\x1b[33mWARNING: $175.36 expiring <5d") {
		t.Fatalf("expected bold yellow warning for 3d19h expiry:\n%q", colored)
	}
	if !strings.Contains(colored, "\x1b[1m\x1b[33mapi-soon\x1b[0m\x1b[0m") || !strings.Contains(colored, "\x1b[1m\x1b[33min 3d 19h !\x1b[0m\x1b[0m") {
		t.Fatalf("expected bold yellow row highlight for api-soon:\n%q", colored)
	}
	if !strings.Contains(colored, "\x1b[1m\x1b[33mapi-soon2\x1b[0m\x1b[0m") || !strings.Contains(colored, "\x1b[1m\x1b[33min 4d 19h !\x1b[0m\x1b[0m") {
		t.Fatalf("expected bold yellow row highlight for api-soon2:\n%q", colored)
	}

	// Verify <=24h escalates to bold red.
	envRed, storeRed := newDashEnv(t)
	appRed, outRed, errRed := envRed.app("")
	appRed.CodexStore = storeRed
	appRed.GatewaySettingsPath = filepath.Join(envRed.dir, "settings.json")
	appRed.Width = 100
	appRed.OutTTY = true
	if code := appRed.Run([]string{"dash"}); code != 0 {
		t.Fatalf("red tty dash exit %d: %s", code, errRed)
	}
	if !strings.Contains(outRed.String(), "\x1b[1m\x1b[31mWARNING: $15.00 expiring <5d (api-exp in 20h)\x1b[0m\x1b[0m") {
		t.Fatalf("expected bold red warning for 20h expiry:\n%q", outRed.String())
	}

	// Verify no warning when all active grants have >=5d remaining.
	codexStoreSafe := codex.NewStore(filepath.Join(env.dir, "codex-safe"), filepath.Join(env.dir, "codex-home-safe"))
	if err := codexStoreSafe.Save(&codex.AccountsStore{Version: 1, Accounts: []codex.StoredAccount{
		{ID: "api-safe", Name: "api-later", SubscriptionExpiresAt: &expLater, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Type: codex.AuthModeAPIKey, Key: &k3}, CreatedAt: testNow},
	}}); err != nil {
		t.Fatal(err)
	}
	calSafe, _, calSafeErr := env.app("")
	calSafe.CodexStore = codexStoreSafe
	if code := calSafe.Run([]string{"codex", "credits", "--set", "api-later", "--grant", "40", "--remaining", "40"}); code != 0 {
		t.Fatalf("calibrate safe failed: %s", calSafeErr)
	}
	appSafe, outSafe, errSafe := env.app("")
	appSafe.CodexStore = codexStoreSafe
	appSafe.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	appSafe.Width = 100
	if code := appSafe.Run([]string{"dash"}); code != 0 {
		t.Fatalf("safe dash exit %d: %s", code, errSafe)
	}
	if strings.Contains(outSafe.String(), "WARNING:") {
		t.Fatalf("unexpected warning for >=5d credit:\n%s", outSafe.String())
	}
}

func TestTruncateStyledKeepsEscapesOutOfWidth(t *testing.T) {
	got := truncateStyled("\x1b[1mGateway\x1b[0m  running on http://127.0.0.1:18455", 12)
	if visibleWidth(got) != 12 || !strings.HasPrefix(got, "\x1b[1mGateway\x1b[0m  ") || !strings.HasSuffix(got, "…\x1b[0m") {
		t.Fatalf("truncateStyled = %q (width %d)", got, visibleWidth(got))
	}
	if got := truncateStyled("short", 12); got != "short" {
		t.Fatalf("short value changed: %q", got)
	}
}
