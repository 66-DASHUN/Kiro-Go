package proxy

import (
	"math"
	"regexp"
	"strings"
)

var claudeCacheAliasPattern = regexp.MustCompile(`^claude-(sonnet|opus|haiku)-[0-9]+(?:-[0-9]+)*(?:-latest)?(?:-thinking)?$`)

// Cache identity normalization never changes the model sent to the upstream.
// Keep version, date and thinking suffixes so distinct models remain isolated.
func totalCacheModelIdentity(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	claudeAlias := strings.ReplaceAll(strings.TrimPrefix(model, "anthropic."), ".", "-")
	if claudeCacheAliasPattern.MatchString(claudeAlias) {
		return claudeAlias
	}
	// Other providers may use dots, hyphens or namespaces for distinct models.
	// Only configured routing aliases may merge those identifiers.
	return model
}

// Local simulation has no model-family whitelist. Availability is enforced by
// account routing and the upstream; only successful generations commit cache.
// This minimum is not a claim about the provider's real prompt cache.
func totalCacheMinTokens(model string) int {
	if strings.TrimSpace(model) == "" {
		return math.MaxInt
	}
	return 1
}
