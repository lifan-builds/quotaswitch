// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuotaSnapshotNeedsIgnite(t *testing.T) {
	now := time.Date(2026, 11, 24, 2, 50, 0, 0, time.UTC)
	full5h := now.Add(5 * time.Hour)
	ticking5h := now.Add(4*time.Hour + 50*time.Minute)
	fullWeekly := now.Add(7 * 24 * time.Hour)
	tickingWeekly := now.Add(5 * 24 * time.Hour)

	tests := []struct {
		name    string
		snap    QuotaSnapshot
		want    bool
		wantWin string
	}{
		{
			name: "5h untouched while weekly already ticking",
			snap: QuotaSnapshot{
				Plan:     "team",
				FiveHour: &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &full5h, WindowSeconds: 18000},
				Weekly:   &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &tickingWeekly, WindowSeconds: 604800},
			},
			want:    true,
			wantWin: "5h",
		},
		{
			name: "both untouched",
			snap: QuotaSnapshot{
				Plan:     "team",
				FiveHour: &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &full5h, WindowSeconds: 18000},
				Weekly:   &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &fullWeekly, WindowSeconds: 604800},
			},
			want:    true,
			wantWin: "5h+weekly",
		},
		{
			name: "both ticking at 100 percent",
			snap: QuotaSnapshot{
				Plan:     "team",
				FiveHour: &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &ticking5h, WindowSeconds: 18000},
				Weekly:   &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &tickingWeekly, WindowSeconds: 604800},
			},
			want: false,
		},
		{
			name: "api_key credit grant is never ignited",
			snap: QuotaSnapshot{
				Plan:   "api_key",
				Weekly: &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &fullWeekly},
			},
			want: false,
		},
		{
			name: "exhausted weekly vetoes untouched 5h",
			snap: QuotaSnapshot{
				Plan:     "team",
				FiveHour: &Window{RemainingPercent: 100, UsedPercent: 0, ResetsAt: &full5h, WindowSeconds: 18000},
				Weekly:   &Window{RemainingPercent: 0, UsedPercent: 100, ResetsAt: &tickingWeekly, WindowSeconds: 604800},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, win := tc.snap.NeedsIgnite(now)
			if got != tc.want || win != tc.wantWin {
				t.Fatalf("NeedsIgnite() = (%v, %q), want (%v, %q)", got, win, tc.want, tc.wantWin)
			}
		})
	}
}

func TestWarmupAccountStreamErrorAndHeaders(t *testing.T) {
	var gotOriginator, gotAccept, gotAcctID string
	failStream := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOriginator = r.Header.Get("originator")
		gotAccept = r.Header.Get("Accept")
		gotAcctID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("Content-Type", "text/event-stream")
		if failStream {
			_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"usage_limit_reached\"}}}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer server.Close()

	acctID := "ws-123"
	acct := StoredAccount{
		ID:       "acct-1",
		AuthMode: AuthModeChatGPT,
		AuthData: AuthData{
			AccessToken: "tok-live",
			AccountID:   &acctID,
		},
	}
	client := &UsageClient{
		WarmupURL:  server.URL,
		HTTPClient: server.Client(),
		Now:        time.Now,
	}

	if err := client.WarmupAccount(context.Background(), nil, acct, true); err != nil {
		t.Fatalf("WarmupAccount ok case failed: %v", err)
	}
	if gotOriginator != "codex_cli_rs" || gotAccept != "text/event-stream" || gotAcctID != "ws-123" {
		t.Fatalf("unexpected headers: originator=%q accept=%q acct=%q", gotOriginator, gotAccept, gotAcctID)
	}

	failStream = true
	err := client.WarmupAccount(context.Background(), nil, acct, true)
	if err == nil || !strings.Contains(err.Error(), "usage_limit_reached") {
		t.Fatalf("expected stream error, got: %v", err)
	}
}
