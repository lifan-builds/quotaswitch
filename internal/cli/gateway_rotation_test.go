package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayRotationPreservesSession(t *testing.T) {
	env := newTestEnv(t, true)
	app, out, errOut := env.app("")
	app.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	if err := os.WriteFile(app.GatewaySettingsPath, []byte(`{"gateway":{"enabled":true,"url":"http://127.0.0.1:18455"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No registry accounts or usage provider are needed: gateway mode must return
	// before reading credentials, inspecting the Hub, or restarting processes.
	if code := app.Run([]string{"rotate", "--json", "--relaunch"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	envelope := decodeTestEnvelope(t, out)
	var result map[string]any
	if err := json.Unmarshal(envelope.Data, &result); err != nil {
		t.Fatal(err)
	}
	if result["rotated"] != false || result["reason"] != "gateway_managed" {
		t.Fatalf("unexpected result: %v", result)
	}
	current, err := os.ReadFile(env.tokenPath)
	if err != nil || string(current) != string(env.raw) {
		t.Fatalf("live credential changed: %v", err)
	}
	state := &liveState{}
	if got := app.maybeAutoSwitch(state, liveModeSwitch); got != "" || state.autoSwitched {
		t.Fatalf("live view attempted automatic switching: %q", got)
	}
	// Read the setting again on each cycle; disabling restores normal rotation.
	if err := os.WriteFile(app.GatewaySettingsPath, []byte(`{"gateway":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	app.JSON = false
	if code := app.Run([]string{"rotate"}); code == 0 || !strings.Contains(errOut.String(), "no stored accounts") {
		t.Fatalf("native rotation did not resume: exit=%d stderr=%s", code, errOut)
	}
}

func TestGatewayRotationAllowsExplicitForce(t *testing.T) {
	env := newTestEnv(t, false)
	app, _, errOut := env.app("")
	app.GatewaySettingsPath = filepath.Join(env.dir, "settings.json")
	if err := os.WriteFile(app.GatewaySettingsPath, []byte(`{"gateway":{"enabled":true,"url":"http://127.0.0.1:18455"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"rotate", "--force"}); code == 0 || !strings.Contains(errOut.String(), "no stored accounts") {
		t.Fatalf("explicit rotation was skipped: exit=%d stderr=%s", code, errOut)
	}
}
