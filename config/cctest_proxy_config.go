package config

import (
	"errors"
	"strings"
)

// CCTestTextHandling returns the per-entry opt-in, defaulting old configs to off.
func (entry ApiKeyEntry) CCTestTextHandling() bool {
	return entry.CCTestTextHandlingEnabled != nil && *entry.CCTestTextHandlingEnabled
}

func validateApiKeyProbeRouting(entry ApiKeyEntry) error {
	if entry.CCTestTextHandling() && entry.UpstreamForward != nil && entry.UpstreamForward.Enabled {
		return errors.New("probe-only forwarding and full-key forwarding cannot both be enabled")
	}
	return nil
}

// GetCCTestProxyConfig returns an atomic copy of the global upstream pair.
// Callers validate completeness only after matching a probe: an incomplete
// configuration must not break normal requests on the shared client key.
func GetCCTestProxyConfig() UpstreamForwardConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return UpstreamForwardConfig{Enabled: true}
	}
	return UpstreamForwardConfig{Enabled: true, BaseURL: cfg.CCTestProxyBaseURL, APIKey: cfg.CCTestProxyAPIKey}
}

// UpdateCCTestProxyConfig saves the global upstream configuration used by the
// CCTest-specific route. A nil field leaves its current value unchanged.
func UpdateCCTestProxyConfig(baseURL, apiKey *string, clearAPIKey bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}

	updatedURL := cfg.CCTestProxyBaseURL
	updatedKey := cfg.CCTestProxyAPIKey
	if baseURL != nil {
		updatedURL = strings.TrimSpace(*baseURL)
	}
	if clearAPIKey {
		updatedKey = ""
	} else if apiKey != nil && strings.TrimSpace(*apiKey) != "" {
		updatedKey = strings.TrimSpace(*apiKey)
	}

	if err := ValidateUpstreamForward(&UpstreamForwardConfig{BaseURL: updatedURL, APIKey: updatedKey}); err != nil {
		return err
	}

	oldURL, oldKey := cfg.CCTestProxyBaseURL, cfg.CCTestProxyAPIKey
	cfg.CCTestProxyBaseURL, cfg.CCTestProxyAPIKey = updatedURL, updatedKey
	if err := Save(); err != nil {
		cfg.CCTestProxyBaseURL, cfg.CCTestProxyAPIKey = oldURL, oldKey
		return err
	}
	return nil
}
