package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAntigravitySettingsPath returns ~/.antigravity/settings.json.
func DefaultAntigravitySettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".antigravity", "settings.json")
}

// LoadAntigravitySettings reads and parses ~/.antigravity/settings.json.
func LoadAntigravitySettings(settingsPath string) (*AdminSettings, error) {
	if strings.TrimSpace(settingsPath) == "" {
		settingsPath = DefaultAntigravitySettingsPath()
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, err
	}
	var s AdminSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// EnableAntigravitySettings writes or updates ~/.antigravity/settings.json so
// Antigravity routes LLM calls through the local quotaswitch gateway and
// displays all pooled Codex + Antigravity models in its model selector.
func EnableAntigravitySettings(settingsPath, gatewayURL, apiKey, defaultModel, codexHome string) (*AdminSettings, error) {
	if strings.TrimSpace(settingsPath) == "" {
		settingsPath = DefaultAntigravitySettingsPath()
	}
	if strings.TrimSpace(gatewayURL) == "" {
		gatewayURL = DefaultGatewayURL
	}
	if strings.TrimSpace(apiKey) == "" {
		apiKey = DefaultAPIKey
	}

	allModels := CombinedModelEntries(codexHome)
	codexModels := LoadCodexModelEntries(codexHome)
	primaryCodex := "gpt-6.1-sol"
	fastCodex := "gpt-6-astra"
	if len(codexModels) > 0 {
		primaryCodex = codexModels[0].ModelID
		for _, m := range codexModels {
			if strings.Contains(m.ModelID, "astra") || strings.Contains(m.ModelID, "terra") || strings.Contains(m.ModelID, "mini") {
				fastCodex = m.ModelID
				break
			}
		}
	}
	if strings.TrimSpace(defaultModel) == "" {
		defaultModel = primaryCodex
	}

	rawMap := make(map[string]any)
	if existing, err := os.ReadFile(settingsPath); err == nil && len(existing) > 0 {
		_ = json.Unmarshal(existing, &rawMap)
		bakPath := settingsPath + ".quotaswitch.bak"
		if _, statErr := os.Stat(bakPath); os.IsNotExist(statErr) {
			_ = os.WriteFile(bakPath, existing, 0o600)
		}
	}

	gwCfg := &GatewayConfig{
		Enabled:      true,
		URL:          gatewayURL,
		WireProtocol: "openai",
		APIKey:       apiKey,
	}
	modelsCfg := &ModelsConfig{
		List:           allModels,
		DefaultModelID: defaultModel,
		Tiers: &TieredModelConfig{
			FlashLite: []string{fastCodex, "gemini-3.8-flash-low"},
			Flash:     []string{fastCodex, "gemini-3.8-flash-medium"},
			Pro:       []string{primaryCodex, "claude-opus-5-5-medium", "gemini-4-argon-medium"},
		},
	}

	rawMap["gateway"] = gwCfg
	rawMap["models"] = modelsCfg

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(rawMap, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(settingsPath, encoded, 0o600); err != nil {
		return nil, err
	}

	return &AdminSettings{
		Gateway: gwCfg,
		Models:  modelsCfg,
	}, nil
}

// DisableAntigravitySettings disables the custom gateway in ~/.antigravity/settings.json
// so Antigravity reverts to its direct CloudCode connection.
func DisableAntigravitySettings(settingsPath string) error {
	if strings.TrimSpace(settingsPath) == "" {
		settingsPath = DefaultAntigravitySettingsPath()
	}
	existing, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var rawMap map[string]any
	if err := json.Unmarshal(existing, &rawMap); err != nil {
		return os.Remove(settingsPath)
	}
	delete(rawMap, "gateway")
	delete(rawMap, "models")
	if len(rawMap) == 0 {
		return os.Remove(settingsPath)
	}
	encoded, err := json.MarshalIndent(rawMap, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(settingsPath, encoded, 0o600)
}
