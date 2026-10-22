package proxy

import (
	"strings"

	"kiro-proxy/config"
)

type apiKeyUpstreamView struct {
	Enabled          bool   `json:"enabled"`
	BaseURL          string `json:"baseUrl"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

type apiKeyUpstreamRequest struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"`
	ClearAPIKey bool   `json:"clearApiKey,omitempty"`
}

func upstreamView(c *config.UpstreamForwardConfig) *apiKeyUpstreamView {
	if c == nil {
		return nil
	}
	return &apiKeyUpstreamView{Enabled: c.Enabled, BaseURL: c.BaseURL, APIKeyConfigured: c.APIKey != ""}
}

func mergeUpstreamConfig(old *config.UpstreamForwardConfig, req *apiKeyUpstreamRequest) *config.UpstreamForwardConfig {
	if req == nil {
		return nil
	}
	c := &config.UpstreamForwardConfig{Enabled: req.Enabled, BaseURL: strings.TrimSpace(req.BaseURL)}
	if old != nil {
		c.APIKey = old.APIKey
	}
	if req.ClearAPIKey {
		c.APIKey = ""
	}
	if req.APIKey != "" {
		c.APIKey = strings.TrimSpace(req.APIKey)
	}
	return c
}
