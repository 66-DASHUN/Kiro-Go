package config

import (
	"encoding/json"
	"errors"
	"strings"
)

func normalizePromptCacheConfig(c *Config, data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if isMissingOrNull(fields["promptCacheEmulationEnabled"]) {
		c.PromptCacheEmulationEnabled = true
	}
	if isMissingOrNull(fields["promptCacheEmulationMode"]) || c.PromptCacheEmulationMode == "" {
		c.PromptCacheEmulationMode = PromptCacheModeSub2API
	}
	if isMissingOrNull(fields["promptCacheEmulationRatio"]) {
		c.PromptCacheEmulationRatio = 1
		if c.PromptCacheEmulationMode == PromptCacheModeSub2API || c.PromptCacheEmulationMode == PromptCacheModePrefix || c.PromptCacheEmulationMode == PromptCacheModePrefixTotal {
			c.PromptCacheEmulationRatio = .95
		}
	}
	if isMissingOrNull(fields["promptCacheCreationEmulationRatio"]) {
		c.PromptCacheCreationEmulationRatio = 1
	}
	if isMissingOrNull(fields["promptCacheReadEmulationRatio"]) {
		c.PromptCacheReadEmulationRatio = 1
	}
	return ValidatePromptCacheSettings(PromptCacheSettings{
		Enabled:       c.PromptCacheEmulationEnabled,
		Mode:          c.PromptCacheEmulationMode,
		Ratio:         c.PromptCacheEmulationRatio,
		CreationRatio: c.PromptCacheCreationEmulationRatio,
		ReadRatio:     c.PromptCacheReadEmulationRatio,
	})
}

func isMissingOrNull(value json.RawMessage) bool {
	return len(value) == 0 || strings.TrimSpace(string(value)) == "null"
}

// EffectivePromptCacheSettings resolves the policy for an API-key entry ID.
// Unknown or empty IDs use the global defaults. Per-key nil fields inherit
// their global counterparts.
func EffectivePromptCacheSettings(apiKeyID string) PromptCacheSettings {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultPromptCacheSettings()
	}
	settings := PromptCacheSettings{
		Enabled:       cfg.PromptCacheEmulationEnabled,
		Mode:          cfg.PromptCacheEmulationMode,
		Ratio:         cfg.PromptCacheEmulationRatio,
		CreationRatio: cfg.PromptCacheCreationEmulationRatio,
		ReadRatio:     cfg.PromptCacheReadEmulationRatio,
	}
	if settings.Mode == "" {
		settings.Mode = PromptCacheModeSub2API
	}
	for i := range cfg.ApiKeys {
		entry := &cfg.ApiKeys[i]
		if entry.ID != apiKeyID {
			continue
		}
		if entry.PromptCacheEmulationEnabled != nil {
			settings.Enabled = *entry.PromptCacheEmulationEnabled
		}
		if entry.PromptCacheEmulationMode != nil {
			settings.Mode = *entry.PromptCacheEmulationMode
		}
		if entry.PromptCacheEmulationRatio != nil {
			settings.Ratio = *entry.PromptCacheEmulationRatio
		}
		if entry.PromptCacheCreationEmulationRatio != nil {
			settings.CreationRatio = *entry.PromptCacheCreationEmulationRatio
		}
		if entry.PromptCacheReadEmulationRatio != nil {
			settings.ReadRatio = *entry.PromptCacheReadEmulationRatio
		}
		break
	}
	if settings.Mode == promptCacheModeUniform || settings.Mode == PromptCacheModeSub2API {
		settings.CreationRatio = settings.Ratio
		settings.ReadRatio = settings.Ratio
	}
	// These legacy fields are inactive in total mode; keep API defaults stable.
	if settings.Mode == PromptCacheModePrefix || settings.Mode == PromptCacheModePrefixTotal {
		settings.CreationRatio = 1
		settings.ReadRatio = settings.Ratio
	}
	return settings
}

func defaultPromptCacheSettings() PromptCacheSettings {
	return PromptCacheSettings{Enabled: true, Mode: PromptCacheModeSub2API, Ratio: .95, CreationRatio: .95, ReadRatio: .95}
}

// UpdatePromptCacheSettings updates and persists the global cache policy.
func UpdatePromptCacheSettings(enabled bool, mode string, ratio, creationRatio, readRatio float64) error {
	settings := PromptCacheSettings{Enabled: enabled, Mode: mode, Ratio: ratio, CreationRatio: creationRatio, ReadRatio: readRatio}
	if err := ValidatePromptCacheSettings(settings); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	original := *cfg
	cfg.PromptCacheEmulationEnabled = enabled
	cfg.PromptCacheEmulationMode = mode
	cfg.PromptCacheEmulationRatio = ratio
	cfg.PromptCacheCreationEmulationRatio = creationRatio
	cfg.PromptCacheReadEmulationRatio = readRatio
	if err := saveLocked(); err != nil {
		*cfg = original
		return err
	}
	return nil
}
