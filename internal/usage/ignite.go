// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
)

const (
	igniteResetSlack  = 2 * time.Minute
	igniteHTTPTimeout = 25 * time.Second
)

// GroupIgniter triggers a minimal inference request for an Antigravity quota
// group so its rolling 5-hour / weekly refresh timer starts ticking.
type GroupIgniter interface {
	IgniteGroup(ctx context.Context, token *agy.TokenFile, group Group) (string, error)
}

// GroupNeedsIgnite reports whether a quota group has an untouched 100% rolling
// bucket whose refresh countdown has not started yet, and none of the group's
// buckets are exhausted.
func GroupNeedsIgnite(group Group, now time.Time) (bool, string) {
	need5h := false
	needWeekly := false
	for _, b := range group.Buckets {
		win := bucketWindow(b.Window, b.ID)
		if win == "" {
			continue
		}
		if b.RemainingFraction <= 0 || b.RemainingPercent <= 0 {
			return false, ""
		}
		if b.RemainingFraction < 1.0 {
			continue
		}
		winDur := 5 * time.Hour
		if win == "weekly" {
			winDur = 7 * 24 * time.Hour
		}
		if resetAt, ok := b.ResetAt(); ok && resetAt.Sub(now) < winDur-igniteResetSlack {
			continue
		}
		if win == "5h" {
			need5h = true
		} else if win == "weekly" {
			needWeekly = true
		}
	}
	switch {
	case need5h && needWeekly:
		return true, "5h+weekly"
	case need5h:
		return true, "5h"
	case needWeekly:
		return true, "weekly"
	default:
		return false, ""
	}
}

// IgniteModelsForGroup returns the lowest-cost candidate model IDs for a quota
// group in preference order.
func IgniteModelsForGroup(group Group) []string {
	name := strings.ToLower(strings.TrimSpace(group.Name))
	desc := strings.ToLower(strings.TrimSpace(group.Description))
	hasGeminiBucket := false
	hasThirdPartyBucket := false
	for _, b := range group.Buckets {
		id := strings.ToLower(strings.TrimSpace(b.ID))
		if strings.HasPrefix(id, "gemini-") {
			hasGeminiBucket = true
		}
		if strings.HasPrefix(id, "3p-") {
			hasThirdPartyBucket = true
		}
	}

	switch {
	case strings.Contains(name, "argon") && !strings.Contains(desc, "flash"):
		return []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-neon-low"}
	case strings.Contains(name, "gemini") || hasGeminiBucket:
		return []string{"gemini-3.8-flash-low", "gemini-4-argon-low", "gemini-4-neon-low"}
	case strings.Contains(name, "claude") || strings.Contains(name, "gpt") || hasThirdPartyBucket:
		return []string{"gpt-oss-120b-medium", "claude-sonnet-5-5-low", "claude-opus-5-5-low"}
	default:
		return nil
	}
}

// IgniteGroup sends a single 1-token request to the cheapest model in group to
// start its rolling quota refresh window.
func (c *Client) IgniteGroup(ctx context.Context, token *agy.TokenFile, group Group) (string, error) {
	models := IgniteModelsForGroup(group)
	if len(models) == 0 {
		return "", ErrUnsupported
	}
	accessToken, project, err := c.ResolveProject(ctx, token)
	if err != nil {
		return "", err
	}

	var lastErr error
	for _, model := range models {
		status, reqErr := c.sendIgniteRequest(ctx, accessToken, project, model)
		if status == http.StatusUnauthorized && token != nil && token.HasRefreshToken() {
			c.evictCachedAccessToken(token)
			if refreshedToken, refreshedProj, rErr := c.ResolveProject(ctx, token); rErr == nil {
				accessToken = refreshedToken
				project = refreshedProj
				status, reqErr = c.sendIgniteRequest(ctx, accessToken, project, model)
			}
		}
		if reqErr == nil {
			return model, nil
		}
		lastErr = reqErr
		if shouldFallbackIgniteStatus(status) {
			continue
		}
		return "", reqErr
	}
	if lastErr == nil {
		lastErr = errors.New("no ignite model succeeded")
	}
	return "", lastErr
}

func shouldFallbackIgniteStatus(status int) bool {
	switch status {
	case http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (c *Client) sendIgniteRequest(ctx context.Context, accessToken, project, model string) (int, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	payload := map[string]any{
		"project":   project,
		"requestId": fmt.Sprintf("qs-ignite-%d", c.now().UnixNano()),
		"model":     model,
		"userAgent": "antigravity",
		"request": map[string]any{
			"contents": []map[string]any{
				{
					"role":  "user",
					"parts": []map[string]any{{"text": "hi"}},
				},
			},
			"generationConfig": map[string]any{
				"maxOutputTokens": 1,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, ErrInvalidResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+":streamGenerateContent?alt=sse", bytes.NewReader(raw))
	if err != nil {
		return 0, ErrNetwork
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: igniteHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, transportError(err)
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, statusError(resp.StatusCode)
		}
		msg := strings.TrimSpace(string(bodyBytes))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return resp.StatusCode, fmt.Errorf("%w: %s", statusError(resp.StatusCode), msg)
		}
		return resp.StatusCode, statusError(resp.StatusCode)
	}
	return resp.StatusCode, nil
}
