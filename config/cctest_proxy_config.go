package config

import (
	"errors"
	"net/url"
	"strings"
)

// CCTestTextHandling returns the per-entry opt-in, defaulting old configs to off.
func (entry ApiKeyEntry) CCTestTextHandling() bool {
	return entry.CCTestTextHandlingEnabled != nil && *entry.CCTestTextHandlingEnabled
}

// CCTestProxyConfig is exclusively the global destination for matched probes.
type CCTestProxyConfig struct {
	BaseURL string
	APIKey  string
}

// GetCCTestProxyConfig returns an atomic copy of the global upstream pair.
// Callers validate completeness only after matching a probe: an incomplete
// configuration must not break normal requests on the shared client key.
func GetCCTestProxyConfig() CCTestProxyConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return CCTestProxyConfig{}
	}
	return CCTestProxyConfig{BaseURL: cfg.CCTestProxyBaseURL, APIKey: cfg.CCTestProxyAPIKey}
}

// ValidateCCTestProxyConfig never includes supplied values in errors.
func ValidateCCTestProxyConfig(c CCTestProxyConfig, requireComplete bool) error {
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("invalid upstream API key")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(c.BaseURL, "\r\n") {
			return errors.New("upstream base URL must be HTTPS without credentials, query or fragment")
		}
	}
	if requireComplete && (c.BaseURL == "" || strings.TrimSpace(c.APIKey) == "") {
		return errors.New("probe upstream requires base URL and API key")
	}
	return nil
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

	if err := ValidateCCTestProxyConfig(CCTestProxyConfig{BaseURL: updatedURL, APIKey: updatedKey}, false); err != nil {
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
