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
	for _, want := range []string{"Antigravity  1 account(s) · active personal (user@example.com)", "Codex OAuth  0/0 with headroom", "API credits  $15.00 across 1/1 active org(s) · next expiry api-exp in 20h", "api-exp", "$15.00", "in 20h", "Gateway  ", "on http://127.0.0.1:18455"} {
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

func TestTruncateStyledKeepsEscapesOutOfWidth(t *testing.T) {
	got := truncateStyled("\x1b[1mGateway\x1b[0m  running on http://127.0.0.1:18455", 12)
	if visibleWidth(got) != 12 || !strings.HasPrefix(got, "\x1b[1mGateway\x1b[0m  ") || !strings.HasSuffix(got, "…\x1b[0m") {
		t.Fatalf("truncateStyled = %q (width %d)", got, visibleWidth(got))
	}
	if got := truncateStyled("short", 12); got != "short" {
		t.Fatalf("short value changed: %q", got)
	}
}
