package proxy

import (
	"kiro-proxy/config"
	"testing"
	"time"
)

func TestExactReferenceShortPrefixNotCached(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: "Hello"}}}
	settings := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeSub2API, Ratio: .9}
	profile := tracker.BuildClaudeProfileWithSettings(req, 14, settings)
	usage := tracker.ComputeWithSettings("account", profile, settings)
	if usage.CacheCreationInputTokens != 0 || usage.CacheReadInputTokens != 0 {
		t.Fatalf("reference minimum must reject short prefix: %+v", usage)
	}
}
