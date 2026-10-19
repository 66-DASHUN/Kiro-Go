package proxy

import (
	"kiro-proxy/config"
	"math"
	"time"
)

const sub2APICacheRuleVersion = "sub2api-prefix-v1"

// Follow Sub2API's prefix matching and token-bucket scaling. Unlike the legacy
// admission policy, ratio never decides whether a successful prefix is stored.
func (t *promptCacheTracker) computeSub2APICacheUsage(scope string, profile *promptCacheProfile, settings config.PromptCacheSettings) promptCacheUsage {
	usage := promptCacheUsage{referenceMode: true, snapshot: true, totalRatio: settings.Ratio, creationRatio: settings.Ratio, readRatio: settings.Ratio}
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || scope == "" || !settings.Enabled {
		return promptCacheUsage{}
	}
	ratio := settings.Ratio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		ratio = 0
	}
	ratio = math.Max(0, math.Min(1, ratio))
	usage.creationRatio, usage.readRatio, usage.totalRatio = ratio, ratio, ratio
	if ratio == 0 {
		usage.limitation = "zero_rate"
	}
	now := time.Now()
	t.mu.Lock()
	t.pruneExpiredLocked(now)
	entries := t.entriesByAccount[scope]
	eligible := minInt(profile.TotalInputTokens, profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens)
	matched := 0
	for i, n := len(profile.Breakpoints)-1, 0; i >= 0 && n < 10; i, n = i-1, n+1 {
		bp := profile.Breakpoints[i]
		if e, ok := entries[bp.Fingerprint]; ok && e.ExpiresAt.After(now) {
			matched = minInt(bp.CumulativeTokens, eligible)
			usage.matchedTTL = e.TTL
			break
		}
	}
	t.mu.Unlock()
	usage.rawCacheable, usage.rawRead = eligible, matched
	usage.rawCreation = maxInt(eligible-matched, 0)
	usage.rawCreation5m, _ = computePromptCacheTTLBreakdownLimited(profile, matched, usage.rawCreation)
	return scaleSub2APICacheUsage(usage, profile, profile.TotalInputTokens)
}

// Completion reuses the dispatch snapshot; overlapping requests cannot turn a
// cold dispatch into a warm response. Rounding is identical for SSE and JSON.
func scaleSub2APICacheUsage(usage promptCacheUsage, profile *promptCacheProfile, total int) promptCacheUsage {
	scale := func(n int) int {
		if profile == nil || profile.TotalInputTokens <= 0 || total <= 0 {
			return 0
		}
		return minInt(total, maxInt(0, int(math.Round(float64(n)*float64(total)/float64(profile.TotalInputTokens)))))
	}
	eligible := scale(usage.rawCacheable)
	read := minInt(scale(usage.rawRead), eligible)
	creation := eligible - read
	usage.finalCacheable, usage.finalMatched = eligible, read
	usage.matchedPrefixTokens = read
	usage.CacheReadInputTokens = minInt(total, int(math.Round(float64(read)*usage.readRatio)))
	usage.matchedCachedTokens = usage.CacheReadInputTokens
	usage.CacheCreationInputTokens = minInt(maxInt(total-usage.CacheReadInputTokens, 0), int(math.Round(float64(creation)*usage.creationRatio)))
	// Preserve the actual TTL interval split instead of assigning all creation
	// to the final boundary's TTL when the request mixes 5m and 1h blocks.
	short := minInt(scale(usage.rawCreation5m), creation)
	usage.CacheCreation5mInputTokens = minInt(usage.CacheCreationInputTokens, int(math.Round(float64(short)*usage.creationRatio)))
	usage.CacheCreation1hInputTokens = usage.CacheCreationInputTokens - usage.CacheCreation5mInputTokens
	return usage
}
