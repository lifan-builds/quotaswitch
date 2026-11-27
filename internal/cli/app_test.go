// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHelpJSONMatchesText(t *testing.T) {
	env := newTestEnv(t, false)
	for _, args := range [][]string{{"version", "--help"}, {"help", "version"}, {"help"}, {"--help"}, {"list", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, plain, errOut := env.app("")
			if code := app.Run(args); code != 0 || !strings.Contains(plain.String(), "Usage:") || errOut.Len() != 0 {
				t.Fatalf("text help exit %d, stdout %q, stderr %q", code, plain, errOut)
			}
			app, out, errOut := env.app("")
			if code := app.Run(append(args, "--json")); code != 0 || errOut.Len() != 0 {
				t.Fatalf("JSON help exit %d, stderr %q", code, errOut)
			}
			env := decodeTestEnvelope(t, out)
			if env.SchemaVersion != 1 || env.Command != canonicalCommand(args[0]) || env.Error != nil {
				t.Fatalf("unexpected envelope %+v", env)
			}
			var data struct{ Help string }
			if err := json.Unmarshal(env.Data, &data); err != nil || data.Help != plain.String() {
				t.Fatalf("JSON help differs from text help: %s", env.Data)
			}
		})
	}
}

func TestUnknownHelpTopic(t *testing.T) {
	env := newTestEnv(t, false)
	args := []string{"help", "nonesuch"}
	app, out, errOut := env.app("")
	if code := app.Run(args); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), `unknown help topic "nonesuch"`) {
		t.Fatalf("text help exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	app, out, errOut = env.app("")
	if code := app.Run(append(args, "--json")); code != 2 || errOut.Len() != 0 {
		t.Fatalf("JSON help exit %d, stderr %q", code, errOut)
	}
	envelope := decodeTestEnvelope(t, out)
	if envelope.Error == nil || envelope.Error.Code != "usage" || !strings.Contains(envelope.Error.Message, `unknown help topic "nonesuch"`) {
		t.Fatalf("unexpected error %+v", envelope.Error)
	}
}
