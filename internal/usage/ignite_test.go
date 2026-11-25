// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package usage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGroupNeedsIgnite(t *testing.T) {
	now := time.Date(2026, 11, 24, 2, 50, 0, 0, time.UTC)

	tests := []struct {
		name     string
		group    Group
		wantNeed bool
		wantWin  string
	}{
		{
			name: "untouched 5h and weekly",
			group: Group{
				Name: "Gemini Models",
				Buckets: []GroupBucket{
					{ID: "gemini-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
					{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
				},
			},
			wantNeed: true,
			wantWin:  "5h+weekly",
		},
		{
			name: "ticking fraction under 1.0 even when percent rounds to 100",
			group: Group{
				Name: "Gemini Models",
				Buckets: []GroupBucket{
					{ID: "gemini-5h", Window: "5h", RemainingFraction: 0.99978, RemainingPercent: 100, ResetTime: now.Add(5*time.Hour - 30*time.Second).Format(time.RFC3339)},
					{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.99994, RemainingPercent: 100, ResetTime: now.Add(7*24*time.Hour - 30*time.Second).Format(time.RFC3339)},
				},
			},
			wantNeed: false,
		},
		{
			name: "fraction 1.0 but reset already ticked past slack",
			group: Group{
				Name: "Claude and GPT models",
				Buckets: []GroupBucket{
					{ID: "3p-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(4*time.Hour + 50*time.Minute).Format(time.RFC3339)},
					{ID: "3p-weekly", Window: "weekly", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(7*24*time.Hour - 10*time.Minute).Format(time.RFC3339)},
				},
			},
			wantNeed: false,
		},
		{
			name: "5h untouched while weekly already ticking",
			group: Group{
				Name: "Claude and GPT models",
				Buckets: []GroupBucket{
					{ID: "3p-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
					{ID: "3p-weekly", Window: "weekly", RemainingFraction: 0.97, RemainingPercent: 97, ResetTime: now.Add(4 * 24 * time.Hour).Format(time.RFC3339)},
				},
			},
			wantNeed: true,
			wantWin:  "5h",
		},
		{
			name: "weekly exhausted vetoes untouched 5h",
			group: Group{
				Name: "Claude and GPT models",
				Buckets: []GroupBucket{
					{ID: "3p-5h", Window: "5h", RemainingFraction: 1, RemainingPercent: 100, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
					{ID: "3p-weekly", Window: "weekly", RemainingFraction: 0, RemainingPercent: 0, ResetTime: now.Add(2 * 24 * time.Hour).Format(time.RFC3339)},
				},
			},
			wantNeed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotNeed, gotWin := GroupNeedsIgnite(tc.group, now)
			if gotNeed != tc.wantNeed || gotWin != tc.wantWin {
				t.Fatalf("GroupNeedsIgnite() = (%v, %q), want (%v, %q)", gotNeed, gotWin, tc.wantNeed, tc.wantWin)
			}
		})
	}
}

func TestIgniteGroupFallbackAndPayload(t *testing.T) {
	var seenModels []string
	var lastMaxOut float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "proj-test",
			})
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			model, _ := body["model"].(string)
			seenModels = append(seenModels, model)
			if reqObj, ok := body["request"].(map[string]any); ok {
				if genCfg, ok := reqObj["generationConfig"].(map[string]any); ok {
					lastMaxOut, _ = genCfg["maxOutputTokens"].(float64)
				}
			}
			if model == "gpt-oss-120b-medium" {
				http.Error(w, "model not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		Endpoint:   server.URL + "/v1internal",
		ProjectID:  FallbackProjectID,
		UserAgent:  "antigravity/test",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return usageTestNow },
	}
	tok := usageTestToken(usageTestNow.Add(time.Hour))
	model, err := client.IgniteGroup(context.Background(), tok, Group{Name: "Claude and GPT models"})
	if err != nil {
		t.Fatalf("IgniteGroup error: %v", err)
	}
	if model != "claude-sonnet-5-5-low" {
		t.Fatalf("model = %q, want claude-sonnet-5-5-low", model)
	}
	if len(seenModels) != 2 || seenModels[0] != "gpt-oss-120b-medium" || seenModels[1] != "claude-sonnet-5-5-low" {
		t.Fatalf("seenModels = %v", seenModels)
	}
	if lastMaxOut != 1 {
		t.Fatalf("maxOutputTokens = %v, want 1", lastMaxOut)
	}
}

func TestIgniteGroupStopsOnRateLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "proj-test"})
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			calls++
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		Endpoint:   server.URL + "/v1internal",
		ProjectID:  "proj-test",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return usageTestNow },
	}
	_, err := client.IgniteGroup(context.Background(), usageTestToken(usageTestNow.Add(time.Hour)), Group{Name: "Gemini Models"})
	if !errors.Is(err, ErrHTTP) {
		t.Fatalf("err = %v, want ErrHTTP", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (should not fall back on 429)", calls)
	}
}

func TestIgniteGroupFallsBackOnServiceUnavailable(t *testing.T) {
	var seenModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "proj-test"})
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			model, _ := body["model"].(string)
			seenModels = append(seenModels, model)
			if model == "gemini-3.8-flash-low" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":503,"message":"No capacity available for model gemini-3.8-flash-low on the server","status":"UNAVAILABLE"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		Endpoint:   server.URL + "/v1internal",
		ProjectID:  "proj-test",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return usageTestNow },
	}
	model, err := client.IgniteGroup(context.Background(), usageTestToken(usageTestNow.Add(time.Hour)), Group{Name: "Gemini Models"})
	if err != nil {
		t.Fatalf("IgniteGroup error: %v", err)
	}
	if model != "gemini-4-argon-low" {
		t.Fatalf("model = %q, want gemini-4-argon-low", model)
	}
	if len(seenModels) != 2 || seenModels[0] != "gemini-3.8-flash-low" || seenModels[1] != "gemini-4-argon-low" {
		t.Fatalf("seenModels = %v, want [gemini-3.8-flash-low gemini-4-argon-low]", seenModels)
	}
}
