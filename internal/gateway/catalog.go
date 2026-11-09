package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultListenAddr = "127.0.0.1:18455"
	DefaultGatewayURL = "http://127.0.0.1:18455"
	DefaultAPIKey     = "quotaswitch-local"
	BackendCodex      = "codex"
	BackendAgy        = "agy"
)

// ModelEntry matches admincontrols.ModelEntry in Antigravity's language_server.
type ModelEntry struct {
	ModelID            string          `json:"modelId"`
	DisplayName        string          `json:"displayName,omitempty"`
	Description        string          `json:"description,omitempty"`
	Provider           string          `json:"provider,omitempty"`
	MaxTokens          int             `json:"maxTokens,omitempty"`
	MaxOutputTokens    int             `json:"maxOutputTokens,omitempty"`
	SupportsThinking   *bool           `json:"supportsThinking,omitempty"`
	ThinkingBudget     int             `json:"thinkingBudget,omitempty"`
	SupportedMimeTypes map[string]bool `json:"supportedMimeTypes,omitempty"`
	SupportsImages     *bool           `json:"supportsImages,omitempty"`
	SupportsVideo      *bool           `json:"supportsVideo,omitempty"`
	SupportsPdf        *bool           `json:"supportsPdf,omitempty"`
}

// TieredModelConfig matches admincontrols.TieredModelConfig in language_server.
type TieredModelConfig struct {
	FlashLite []string `json:"flashLite,omitempty"`
	Flash     []string `json:"flash,omitempty"`
	Pro       []string `json:"pro,omitempty"`
}

// ModelsConfig matches admincontrols.ModelsConfig in language_server.
type ModelsConfig struct {
	List           []ModelEntry       `json:"list,omitempty"`
	ListSource     string             `json:"listSource,omitempty"`
	DefaultModelID string             `json:"defaultModelId,omitempty"`
	Tiers          *TieredModelConfig `json:"tiers,omitempty"`
}

// GatewayConfig matches admincontrols.GatewayConfig in language_server.
type GatewayConfig struct {
	Enabled       bool              `json:"enabled"`
	URL           string            `json:"url,omitempty"`
	WireProtocol  string            `json:"wireProtocol,omitempty"`
	APIKey        string            `json:"apiKey,omitempty"`
	CustomHeaders map[string]string `json:"customHeaders,omitempty"`
	RouteWebFetch bool              `json:"routeWebFetch,omitempty"`
}

// AdminSettings matches admincontrols.AdminSettings (~/.antigravity/settings.json).
type AdminSettings struct {
	CACertPath string         `json:"caCertPath,omitempty"`
	Gateway    *GatewayConfig `json:"gateway,omitempty"`
	Models     *ModelsConfig  `json:"models,omitempty"`
}

type codexModelsCache struct {
	ClientVersion string            `json:"client_version"`
	Models        []codexCacheModel `json:"models"`
}

type codexCacheModel struct {
	Slug                   string   `json:"slug"`
	DisplayName            string   `json:"display_name"`
	Description            string   `json:"description"`
	DefaultReasoningLevel  string   `json:"default_reasoning_level"`
	Visibility             string   `json:"visibility"`
	SupportedInAPI         bool     `json:"supported_in_api"`
	InputModalities        []string `json:"input_modalities"`
	ContextWindow          int      `json:"context_window"`
	EffectiveContextWindow int      `json:"effective_context_window_percent"`
}

func boolPtr(v bool) *bool {
	return &v
}

func defaultImageMimeTypes() map[string]bool {
	return map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/webp": true,
		"image/gif":  true,
	}
}

// LoadCodexModelEntries loads available Codex models from ~/.codex/models_cache.json,
// falling back to the built-in Codex model catalog when the cache file is absent.
func LoadCodexModelEntries(codexHome string) []ModelEntry {
	if strings.TrimSpace(codexHome) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if codexHome != "" {
		cachePath := filepath.Join(codexHome, "models_cache.json")
		if raw, err := os.ReadFile(cachePath); err == nil {
			var cache codexModelsCache
			if json.Unmarshal(raw, &cache) == nil && len(cache.Models) > 0 {
				entries := make([]ModelEntry, 0, len(cache.Models))
				seen := make(map[string]bool, len(cache.Models))
				for _, m := range cache.Models {
					slug := strings.TrimSpace(m.Slug)
					if slug == "" || seen[slug] || strings.EqualFold(m.Visibility, "hide") {
						continue
					}
					seen[slug] = true
					maxTokens := m.ContextWindow
					if maxTokens <= 0 {
						maxTokens = 272000
					}
					display := strings.TrimSpace(m.DisplayName)
					if display == "" {
						display = slug
					}
					if !strings.Contains(strings.ToLower(display), "codex") {
						display += " (Codex Pool)"
					}
					desc := strings.TrimSpace(m.Description)
					if desc == "" {
						desc = "OpenAI Codex pooled model via quotaswitch gateway"
					}
					supportsImages := false
					for _, mod := range m.InputModalities {
						if strings.EqualFold(mod, "image") {
							supportsImages = true
							break
						}
					}
					var mimes map[string]bool
					if supportsImages {
						mimes = defaultImageMimeTypes()
					}
					entries = append(entries, ModelEntry{
						ModelID:            slug,
						DisplayName:        display,
						Description:        desc,
						Provider:           "openai",
						MaxTokens:          maxTokens,
						MaxOutputTokens:    65536,
						SupportsThinking:   boolPtr(true),
						ThinkingBudget:     32768,
						SupportedMimeTypes: mimes,
						SupportsImages:     boolPtr(supportsImages),
						SupportsVideo:      boolPtr(false),
						SupportsPdf:        boolPtr(false),
					})
				}
				if len(entries) > 0 {
					return entries
				}
			}
		}
	}
	return BuiltinCodexModelEntries()
}

// BuiltinCodexModelEntries returns the default Codex models when models_cache.json is missing.
func BuiltinCodexModelEntries() []ModelEntry {
	mimes := defaultImageMimeTypes()
	return []ModelEntry{
		{
			ModelID:            "gpt-6.1-sol",
			DisplayName:        "GPT-6.1-Sol (Codex Pool)",
			Description:        "Latest frontier workhorse model for coding and everyday work (500k context)",
			Provider:           "openai",
			MaxTokens:          500000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     32768,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
		{
			ModelID:            "gpt-6.1-terra",
			DisplayName:        "GPT-6.1 Terra (Codex Pool)",
			Description:        "Fast, high-context Codex model (500k context)",
			Provider:           "openai",
			MaxTokens:          500000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     32768,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
		{
			ModelID:            "gpt-6-astra",
			DisplayName:        "GPT-6-Astra (Codex Pool)",
			Description:        "Fast subagent and coding model (272k context)",
			Provider:           "openai",
			MaxTokens:          272000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     16384,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
		{
			ModelID:            "gpt-6-sol",
			DisplayName:        "GPT-6-Sol (Codex Pool)",
			Description:        "Frontier coding and reasoning model (272k context)",
			Provider:           "openai",
			MaxTokens:          272000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     32768,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
		{
			ModelID:            "gpt-6-luna",
			DisplayName:        "GPT-6-Luna (Codex Pool)",
			Description:        "Frontier coding model (272k context)",
			Provider:           "openai",
			MaxTokens:          272000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     32768,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
		{
			ModelID:            "gpt-5.6-sol",
			DisplayName:        "GPT-5.6-Sol (Codex Pool)",
			Description:        "GPT-5.6-Sol coding model (272k context)",
			Provider:           "openai",
			MaxTokens:          272000,
			MaxOutputTokens:    65536,
			SupportsThinking:   boolPtr(true),
			ThinkingBudget:     32768,
			SupportedMimeTypes: mimes,
			SupportsImages:     boolPtr(true),
			SupportsVideo:      boolPtr(false),
			SupportsPdf:        boolPtr(false),
		},
	}
}

// BuiltinAgyModelEntries returns the Antigravity agent models (Gemini, Claude, GPT-OSS)
// used when the live fetchAvailableModels list is unavailable.
func BuiltinAgyModelEntries() []ModelEntry {
	mimes := defaultImageMimeTypes()
	entry := func(id, name, provider string, maxTokens, maxOut int, images bool) ModelEntry {
		e := ModelEntry{
			ModelID:          id,
			DisplayName:      name,
			Description:      name + " (Antigravity Pool)",
			Provider:         provider,
			MaxTokens:        maxTokens,
			MaxOutputTokens:  maxOut,
			SupportsThinking: boolPtr(true),
			ThinkingBudget:   32768,
			SupportsImages:   boolPtr(images),
		}
		if images {
			e.SupportedMimeTypes = mimes
		}
		return e
	}
	return []ModelEntry{
		entry("gemini-4-argon-medium", "Gemini 4 Argon (Medium)", "google", 1048576, 65536, true),
		entry("gemini-4-argon-high", "Gemini 4 Argon (High)", "google", 1048576, 65536, true),
		entry("gemini-4-argon-low", "Gemini 4 Argon (Low)", "google", 1048576, 65536, true),
		entry("gemini-3.8-flash-high", "Gemini 3.8 Flash (High)", "google", 1048576, 65536, true),
		entry("gemini-3.8-flash-medium", "Gemini 3.8 Flash (Medium)", "google", 1048576, 65536, true),
		entry("gemini-3.8-flash-low", "Gemini 3.8 Flash (Low)", "google", 1048576, 65536, true),
		entry("claude-opus-5-5-low", "Claude Opus 5.5 (Low)", "anthropic", 1000000, 128000, true),
		entry("claude-opus-5-5-medium", "Claude Opus 5.5 (Medium)", "anthropic", 1000000, 128000, true),
		entry("claude-opus-5-5-high", "Claude Opus 5.5 (High)", "anthropic", 1000000, 128000, true),
		entry("claude-sonnet-5-5-low", "Claude Sonnet 5.5 (Low)", "anthropic", 1000000, 128000, true),
		entry("claude-sonnet-5-5-medium", "Claude Sonnet 5.5 (Medium)", "anthropic", 1000000, 128000, true),
		entry("claude-sonnet-5-5-high", "Claude Sonnet 5.5 (High)", "anthropic", 1000000, 128000, true),
		entry("gpt-oss-120b-medium", "GPT-OSS 120B (Medium)", "openai", 131072, 32768, false),
	}
}

// AgyModelSource supplies the live Antigravity model list (nil means use the built-in list).
var AgyModelSource func() []ModelEntry

func agyModelEntries() []ModelEntry {
	if AgyModelSource != nil {
		if live := AgyModelSource(); len(live) > 0 {
			return live
		}
	}
	return BuiltinAgyModelEntries()
}

// CombinedModelEntries merges Codex and Antigravity model entries for ~/.antigravity/settings.json.
func CombinedModelEntries(codexHome string) []ModelEntry {
	codexModels := LoadCodexModelEntries(codexHome)
	agyModels := agyModelEntries()
	out := make([]ModelEntry, 0, len(codexModels)+len(agyModels))
	seen := make(map[string]bool, len(codexModels)+len(agyModels))
	for _, m := range codexModels {
		if !seen[m.ModelID] {
			seen[m.ModelID] = true
			out = append(out, m)
		}
	}
	for _, m := range agyModels {
		if !seen[m.ModelID] {
			seen[m.ModelID] = true
			out = append(out, m)
		}
	}
	return out
}

// ResolveModel maps an incoming model ID to its backend ("codex" or "agy"),
// the canonical upstream model slug, and an optional default reasoning effort.
func ResolveModel(modelID, codexHome string) (backend string, upstreamModel string, effort string) {
	cleaned := strings.TrimSpace(modelID)
	cleaned = strings.TrimPrefix(cleaned, "models/")
	lower := strings.ToLower(cleaned)

	// Antigravity CloudCode models
	switch {
	case strings.HasPrefix(lower, "claude-"),
		strings.HasPrefix(lower, "gemini-"),
		strings.HasPrefix(lower, "gpt-oss-"):
		return BackendAgy, cleaned, ""
	}

	// Check optional reasoning effort suffix on Codex models (e.g. gpt-6.1-sol-high)
	for _, eff := range []string{"xhigh", "high", "medium", "low", "minimal"} {
		suffix := "-" + eff
		if strings.HasSuffix(lower, suffix) {
			base := cleaned[:len(cleaned)-len(suffix)]
			// Only strip suffix if base is not itself a known model with that suffix
			if base != "" && !strings.EqualFold(base, "gpt-3.1-pro") && !strings.EqualFold(base, "gemini-3.1-pro") {
				cleaned = base
				lower = strings.ToLower(cleaned)
				effort = eff
				if effort == "xhigh" {
					effort = "high"
				}
				break
			}
		}
	}

	codexEntries := LoadCodexModelEntries(codexHome)
	primaryCodex := "gpt-6.1-sol"
	fastCodex := "gpt-6-astra"
	if len(codexEntries) > 0 {
		primaryCodex = codexEntries[0].ModelID
		for _, e := range codexEntries {
			if strings.Contains(e.ModelID, "astra") || strings.Contains(e.ModelID, "terra") || strings.Contains(e.ModelID, "mini") {
				fastCodex = e.ModelID
				break
			}
		}
	}

	// Standard OpenAI model aliases -> map to user's available Codex models if not directly in cache
	knownCodex := make(map[string]string, len(codexEntries))
	for _, e := range codexEntries {
		knownCodex[strings.ToLower(e.ModelID)] = e.ModelID
	}
	if exact, ok := knownCodex[lower]; ok {
		if effort == "" {
			effort = defaultEffortForModel(exact)
		}
		return BackendCodex, exact, effort
	}

	switch lower {
	case "gpt-5.4", "gpt-5.3-codex", "gpt-5.2-codex", "gpt-5.2", "gpt-5.1-codex-max", "gpt-5.1-codex", "gpt-5", "codex", "o3", "o4-mini":
		if effort == "" {
			effort = defaultEffortForModel(primaryCodex)
		}
		return BackendCodex, primaryCodex, effort
	case "gpt-5.4-mini", "gpt-5.1-codex-mini", "gpt-5-mini", "gpt-4.1-mini", "gpt-4o-mini":
		if effort == "" {
			effort = defaultEffortForModel(fastCodex)
		}
		return BackendCodex, fastCodex, effort
	}

	if effort == "" {
		effort = defaultEffortForModel(cleaned)
	}
	if cleaned == "" {
		cleaned = primaryCodex
	}
	return BackendCodex, cleaned, effort
}

func defaultEffortForModel(modelID string) string {
	lower := strings.ToLower(modelID)
	if strings.Contains(lower, "astra") || strings.Contains(lower, "terra") {
		return "low"
	}
	return "medium"
}
