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

// All recognized Claude families use the same local simulation rule: any
// nonempty prefix is eligible. These are not measured upstream cache limits.
// Unknown families and malformed names still fail closed.
func totalCacheMinTokens(model string) int {
	match := totalCacheClaudeModelPattern.FindStringSubmatch(totalCacheModelIdentity(model))
	if match == nil {
		return math.MaxInt
	}
	return 1
}
