package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type fetchModelsResponse struct {
	Models map[string]struct {
		DisplayName        string          `json:"displayName"`
		SupportsImages     bool            `json:"supportsImages"`
		SupportsThinking   bool            `json:"supportsThinking"`
		MaxTokens          int             `json:"maxTokens"`
		MaxOutputTokens    int             `json:"maxOutputTokens"`
		ModelProvider      string          `json:"modelProvider"`
		SupportedMimeTypes map[string]bool `json:"supportedMimeTypes"`
	} `json:"models"`
	AgentModelSorts []struct {
		Groups []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	} `json:"agentModelSorts"`
}

// FetchAgyModelEntries asks CloudCode for the agent models available to the pooled
// Antigravity accounts, in Antigravity's recommended order.
func (p *Pool) FetchAgyModelEntries(ctx context.Context, endpoint string, client *http.Client) ([]ModelEntry, error) {
	cands, err := p.AgyCandidates()
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		base = DefaultAgyEndpoint
	}
	var lastErr error
	for i := range cands {
		cand := cands[i]
		tok, proj, err := p.ResolveAgySession(ctx, &cand, false)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := json.Marshal(map[string]string{"project": proj})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1internal:fetchAvailableModels", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", agyCLIUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("fetchAvailableModels HTTP %d", resp.StatusCode)
			continue
		}
		var parsed fetchModelsResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		return agyEntriesFromFetch(parsed), nil
	}
	return nil, lastErr
}

func agyEntriesFromFetch(parsed fetchModelsResponse) []ModelEntry {
	var order []string
	for _, sort := range parsed.AgentModelSorts {
		for _, g := range sort.Groups {
			order = append(order, g.ModelIDs...)
		}
		if len(order) > 0 {
			break
		}
	}
	out := make([]ModelEntry, 0, len(order))
	for _, id := range order {
		m, ok := parsed.Models[id]
		if !ok || strings.TrimSpace(m.DisplayName) == "" {
			continue
		}
		provider := strings.ToLower(strings.TrimPrefix(m.ModelProvider, "MODEL_PROVIDER_"))
		e := ModelEntry{
			ModelID:          id,
			DisplayName:      m.DisplayName,
			Description:      m.DisplayName + " (Antigravity Pool)",
			Provider:         provider,
			MaxTokens:        m.MaxTokens,
			MaxOutputTokens:  m.MaxOutputTokens,
			SupportsThinking: boolPtr(m.SupportsThinking),
			SupportsImages:   boolPtr(m.SupportsImages),
		}
		if m.SupportsThinking {
			e.ThinkingBudget = 32768
		}
		if m.SupportsImages {
			e.SupportedMimeTypes = defaultImageMimeTypes()
		}
		out = append(out, e)
	}
	return out
}

// CachedAgyModelSource returns an AgyModelSource that refreshes the live list at most every ttl.
func CachedAgyModelSource(p *Pool, endpoint string, ttl time.Duration) func() []ModelEntry {
	var mu sync.Mutex
	var cached []ModelEntry
	var fetchedAt time.Time
	return func() []ModelEntry {
		mu.Lock()
		defer mu.Unlock()
		if len(cached) > 0 && time.Since(fetchedAt) < ttl {
			return cached
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if live, err := p.FetchAgyModelEntries(ctx, endpoint, nil); err == nil && len(live) > 0 {
			cached = live
			fetchedAt = time.Now()
		}
		return cached
	}
}
