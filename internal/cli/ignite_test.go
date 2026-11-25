// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/registry"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

type fakeIgniteUsage struct {
	mu      sync.Mutex
	groups  map[string]usage.Groups
	errors  map[string]error
	ignited []string
}

func (f *fakeIgniteUsage) FetchGroups(_ context.Context, token *agy.TokenFile) (usage.Groups, error) {
	email := token.Email()
	if err := f.errors[email]; err != nil {
		return usage.Groups{}, err
	}
	return f.groups[email], nil
}

func (f *fakeIgniteUsage) IgniteGroup(_ context.Context, token *agy.TokenFile, group usage.Group) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ignited = append(f.ignited, token.Email()+":"+group.Name)
	models := usage.IgniteModelsForGroup(group)
	if len(models) > 0 {
		return models[0], nil
	}
	return "mock-model", nil
}

func TestCmdIgniteAcrossAgyAndCodexPools(t *testing.T) {
	now := time.Date(2026, 11, 24, 2, 50, 0, 0, time.UTC)
	env := newTestEnv(t, false)
	agyStore := registry.NewStore(env.storeDir)
	for _, seed := range []struct {
		email  string
		sub    string
		alias  string
		active bool
	}{
		{"ultra@gmail.com", "sub-ultra", "ultra", true},
		{"free@gmail.com", "sub-free", "free", false},
		{"unactivated@gmail.com", "sub-unact", "unact", false},
	} {
		if _, err := agyStore.Upsert(makeCLIToken(t, seed.email, seed.sub, "ref-"+seed.alias), seed.alias, seed.active); err != nil {
			t.Fatal(err)
		}
	}

	fakeAgy := &fakeIgniteUsage{
		groups: map[string]usage.Groups{
			"ultra@gmail.com": {
				UpdatedAt: now,
				Groups: []usage.Group{
					{
						Name: "Gemini Models",
						Buckets: []usage.GroupBucket{
							{ID: "gemini-5h", Window: "5h", RemainingFraction: 0.999, RemainingPercent: 100, ResetTime: now.Add(4 * time.Hour).Format(time.RFC3339)},
						},
					},
					{
						Name: "Claude and GPT models",
						Buckets: []usage.GroupBucket{
							{ID: "3p-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
							{ID: "3p-weekly", Window: "weekly", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
						},
					},
				},
			},
			"free@gmail.com": {
				UpdatedAt: now,
				Groups: []usage.Group{
					{
						Name: "Gemini Models",
						Buckets: []usage.GroupBucket{
							{ID: "gemini-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
						},
					},
				},
			},
		},
		errors: map[string]error{
			"unactivated@gmail.com": usage.ErrForbidden,
		},
	}

	var codexWarmups []string
	codexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acctID := r.Header.Get("chatgpt-account-id")
		switch r.URL.Path {
		case "/usage":
			w.Header().Set("Content-Type", "application/json")
			if acctID == "ws-idle" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"plan_type": "team",
					"rate_limit": map[string]any{
						"primary_window":   map[string]any{"used_percent": 0, "limit_window_seconds": 18000, "reset_at": now.Add(5 * time.Hour).Unix()},
						"secondary_window": map[string]any{"used_percent": 30, "limit_window_seconds": 604800, "reset_at": now.Add(3 * 24 * time.Hour).Unix()},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "team",
				"rate_limit": map[string]any{
					"primary_window":   map[string]any{"used_percent": 5, "limit_window_seconds": 18000, "reset_at": now.Add(3 * time.Hour).Unix()},
					"secondary_window": map[string]any{"used_percent": 20, "limit_window_seconds": 604800, "reset_at": now.Add(4 * 24 * time.Hour).Unix()},
				},
			})
		case "/warmup":
			codexWarmups = append(codexWarmups, acctID)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer codexSrv.Close()

	codexStore := codex.NewStore(filepath.Join(env.dir, "codex-store"), filepath.Join(env.dir, "codex-home"))
	wsIdle := "ws-idle"
	wsBusy := "ws-busy"
	apiKey := "sk-test-grant"
	idleEmail := "idle@codex.test"
	busyEmail := "busy@codex.test"
	apiEmail := "api@codex.test"
	if err := codexStore.Save(&codex.AccountsStore{
		Version: 1,
		Accounts: []codex.StoredAccount{
			{ID: "c-idle", Name: "idle-ws", Email: &idleEmail, AuthMode: codex.AuthModeChatGPT, AuthData: codex.AuthData{AccessToken: "tok-idle", AccountID: &wsIdle}},
			{ID: "c-busy", Name: "busy-ws", Email: &busyEmail, AuthMode: codex.AuthModeChatGPT, AuthData: codex.AuthData{AccessToken: "tok-busy", AccountID: &wsBusy}},
			{ID: "c-api", Name: "api-grant", Email: &apiEmail, AuthMode: codex.AuthModeAPIKey, AuthData: codex.AuthData{Key: &apiKey}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	makeApp := func(at time.Time) (*App, *bytes.Buffer, *bytes.Buffer) {
		app, out, errOut := env.app("")
		app.Now = func() time.Time { return at }
		app.UsageGroups = fakeAgy
		app.UsageIgniter = fakeAgy
		app.CodexStore = codexStore
		app.CodexOps = &codex.ProcessOps{Inspect: func() codex.ProcessSummary { return codex.ProcessSummary{} }}
		app.CodexUsage = &codex.UsageClient{
			UsageURL:   codexSrv.URL + "/usage",
			WarmupURL:  codexSrv.URL + "/warmup",
			HTTPClient: codexSrv.Client(),
			Now:        func() time.Time { return at },
		}
		return app, out, errOut
	}

	// Dry run must not invoke upstream igniters.
	app, out, errOut := makeApp(now)
	if code := app.Run([]string{"ignite", "--dry-run", "--json"}); code != 0 {
		t.Fatalf("dry-run exit=%d err=%s", code, errOut)
	}
	if len(fakeAgy.ignited) != 0 || len(codexWarmups) != 0 {
		t.Fatalf("dry-run invoked igniters: agy=%v codex=%v", fakeAgy.ignited, codexWarmups)
	}
	env1 := decodeTestEnvelope(t, out)
	var rep1 igniteReport
	if err := json.Unmarshal(env1.Data, &rep1); err != nil {
		t.Fatal(err)
	}
	if len(rep1.Ignited) != 3 {
		t.Fatalf("dry-run ignited count = %d, want 3: %+v", len(rep1.Ignited), rep1.Ignited)
	}

	// Live run ignites untouched pools and skips already-ticking, forbidden, and api_key.
	app, out, errOut = makeApp(now)
	if code := app.Run([]string{"ignite", "--json"}); code != 0 {
		t.Fatalf("ignite exit=%d err=%s", code, errOut)
	}
	if len(fakeAgy.ignited) != 2 {
		t.Fatalf("agy ignited = %v, want 2", fakeAgy.ignited)
	}
	if len(codexWarmups) != 1 || codexWarmups[0] != "ws-idle" {
		t.Fatalf("codex warmups = %v, want [ws-idle]", codexWarmups)
	}

	// Immediate re-run (within cooldown) must not re-ignite.
	app, out, errOut = makeApp(now.Add(1 * time.Minute))
	if code := app.Run([]string{"ignite", "--json"}); code != 0 {
		t.Fatalf("second ignite exit=%d err=%s", code, errOut)
	}
	if len(fakeAgy.ignited) != 2 || len(codexWarmups) != 1 {
		t.Fatalf("cooldown failed: agy=%v codex=%v", fakeAgy.ignited, codexWarmups)
	}

	// --force bypasses cooldown and already-ticking state.
	app, out, errOut = makeApp(now.Add(1 * time.Minute))
	if code := app.Run([]string{"ignite", "--provider=codex", "--force", "idle-ws", "--json"}); code != 0 {
		t.Fatalf("force ignite exit=%d err=%s", code, errOut)
	}
	if len(codexWarmups) != 2 {
		t.Fatalf("force ignite did not warm up codex: %v", codexWarmups)
	}
}

func TestAppNowStripsMonotonic(t *testing.T) {
	if got := (&App{}).now().String(); strings.Contains(got, " m=") {
		t.Fatalf("(&App{}).now() retained monotonic clock: %q", got)
	}
	if got := (&App{Now: time.Now}).now().String(); strings.Contains(got, " m=") {
		t.Fatalf("(&App{Now: time.Now}).now() retained monotonic clock: %q", got)
	}
}
