package proxy

import (
	"math"
	"regexp"
	"strings"
)

var totalCacheClaudeModelPattern = regexp.MustCompile(`^claude-(sonnet|opus|haiku)-[0-9]+(?:-[0-9]+)*(?:-latest)?(?:-thinking)?$`)

// Cache identity normalization never changes the model sent to the upstream.
// Keep version, date and thinking suffixes so distinct models remain isolated.
func totalCacheModelIdentity(model string) string {
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic.")
	return strings.ReplaceAll(model, ".", "-")
}

// These are local simulation defaults, not measured upstream cache limits.
// Recognized Claude families inherit a default across numeric versions; an
// unknown family or malformed name must not silently fabricate cache usage.
func totalCacheMinTokens(model string) int {
	match := totalCacheClaudeModelPattern.FindStringSubmatch(totalCacheModelIdentity(model))
	if match == nil {
		return math.MaxInt
	}
	if match[1] == "sonnet" {
		return defaultMinCacheableTokens
	}
	return opusMinCacheableTokens
}
