// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package usage

import (
	"context"
	"net/http"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
)

type GroupBucket struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Description       string  `json:"description,omitempty"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remaining_fraction"`
	RemainingPercent  int     `json:"remaining_percent"`
	ResetTime         string  `json:"reset_time,omitempty"`
}

func (b GroupBucket) ResetAt() (time.Time, bool) {
	return parseReset(b.ResetTime)
}

type Group struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Buckets     []GroupBucket `json:"buckets"`
}

type Groups struct {
	Description string    `json:"description,omitempty"`
	Groups      []Group   `json:"groups"`
	UpdatedAt   time.Time `json:"updated_at"`
	Stale       bool      `json:"stale,omitempty"`
}

type GroupsProvider interface {
	FetchGroups(ctx context.Context, token *agy.TokenFile) (Groups, error)
}

func (c *Client) FetchGroups(ctx context.Context, token *agy.TokenFile) (Groups, error) {
	accessToken, project, err := c.ResolveProject(ctx, token)
	if err != nil {
		return Groups{}, err
	}
	var summary quotaSummaryResponse
	status, err := c.post(ctx, "retrieveUserQuotaSummary", quotaSummaryRequest{Project: project}, accessToken, &summary)
	if err == nil && status == http.StatusUnauthorized && token != nil && token.HasRefreshToken() {
		c.evictCachedAccessToken(token)
		if refreshed, rErr := c.accessToken(ctx, token); rErr == nil {
			summary = quotaSummaryResponse{}
			status, err = c.post(ctx, "retrieveUserQuotaSummary", quotaSummaryRequest{Project: project}, refreshed, &summary)
		}
	}
	if err != nil {
		return Groups{}, err
	}
	if status < http.StatusOK || status > 299 {
		return Groups{}, statusError(status)
	}

	return groupsFromSummary(summary, c.now().UTC()), nil
}

func groupsFromSummary(summary quotaSummaryResponse, now time.Time) Groups {
	groups := Groups{
		Description: summary.Description,
		Groups:      []Group{},
		UpdatedAt:   now,
	}
	for _, source := range summary.Groups {
		group := Group{Name: source.DisplayName, Description: source.Description, Buckets: []GroupBucket{}}
		for _, bucket := range source.Buckets {
			if bucket.RemainingFraction == nil {
				continue
			}
			group.Buckets = append(group.Buckets, GroupBucket{
				ID:                bucket.BucketID,
				Name:              bucket.DisplayName,
				Description:       bucket.Description,
				Window:            bucket.Window,
				RemainingFraction: *bucket.RemainingFraction,
				RemainingPercent:  percent(*bucket.RemainingFraction),
				ResetTime:         bucket.ResetTime,
			})
		}
		groups.Groups = append(groups.Groups, group)
	}
	return groups
}
