package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

func TestCodexCreditsCLIAndListIntegration(t *testing.T) {
	env := newTestEnv(t, true)
	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-switcher"), filepath.Join(env.dir, "codex-home"))
	exp := testNow.Add(15 * 24 * time.Hour).UTC()
	key := "test-api-key-cli"
	email := "credits@example.com"

	if err := codexStore.Save(&codex.AccountsStore{
		Version: 1,
		Accounts: []codex.StoredAccount{
			{
				ID:                    "acct-cli-credit",
				Name:                  "api-credits-exp1130",
				Email:                 &email,
				SubscriptionExpiresAt: &exp,
				AuthMode:              codex.AuthModeAPIKey,
				AuthData:              codex.AuthData{Type: codex.AuthModeAPIKey, Key: &key},
				CreatedAt:             testNow,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	usageClient := codex.NewUsageClient()
	usageClient.Now = func() time.Time { return testNow }

	app, outBuf, errBuf := env.app("test-admin-key-from-stdin\n")
	app.CodexStore = codexStore
	app.CodexUsage = usageClient

	if code := app.Run([]string{
		"codex", "credits",
		"--set", "api-credits-exp1130",
		"--grant", "120",
		"--used", "0.07",
		"--admin-key-stdin",
	}); code != 0 {
		t.Fatalf("codex credits --set exit %d: %s", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "$119.93 / $120.00 remaining") {
		t.Fatalf("unexpected calibration output: %s", outBuf.String())
	}

	// Verify table output of `quotaswitch codex credits`
	app2, out2, err2 := env.app("")
	app2.CodexStore = codexStore
	app2.CodexUsage = usageClient
	if code := app2.Run([]string{"codex", "credits"}); code != 0 {
		t.Fatalf("codex credits exit %d: %s", code, err2.String())
	}
	if !strings.Contains(out2.String(), "$119.93") || !strings.Contains(out2.String(), "Pool Total: $119.93 / $120.00") {
		t.Fatalf("unexpected codex credits table:\n%s", out2.String())
	}

	// Verify `quotaswitch list --provider=codex` displays Credits $119.93/$120 without any browser/network calls!
	app3, out3, err3 := env.app("")
	app3.CodexStore = codexStore
	offlineUsage := codex.NewUsageClient()
	offlineUsage.Now = func() time.Time { return testNow }
	offlineUsage.OpenAIBaseURL = "http://127.0.0.1:1"
	app3.CodexUsage = offlineUsage
	if code := app3.Run([]string{"list", "--provider=codex"}); code != 0 {
		t.Fatalf("list --provider=codex exit %d: %s", code, err3.String())
	}
	listOut := out3.String()
	if !strings.Contains(listOut, "Credits $119.93/$120") || !strings.Contains(listOut, "100%") {
		t.Fatalf("expected Credits $119.93/$120 and 100%% in list output:\n%s", listOut)
	}
}
