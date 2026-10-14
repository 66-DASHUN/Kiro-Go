package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"math"
	"net/http"
	"strings"
	"time"
)

const totalCacheRuleVersion = "prefix-total-v1"
const promptCacheDiagnosticCapacity = 200
const promptCacheDiagnosticRetention = 24 * time.Hour

// Unknown models are deliberately ineligible for the new statistical mode.
func totalCacheMinTokens(model string) int {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "anthropic.")
	model = strings.ReplaceAll(model, ".", "-")
	for name, floor := range map[string]int{
		"claude-sonnet-4-5": 1024,
		"claude-haiku-4-5":  4096,
		"claude-opus-4-5":   4096,
		"claude-opus-4-6":   4096,
	} {
		if model == name || strings.HasPrefix(model, name+"-") {
			return floor
		}
	}
	return math.MaxInt
}

func promptCacheBlockGroup(value interface{}) string {
	wrapper, ok := value.(map[string]interface{})
	if !ok || wrapper["kind"] != "message" {
		return ""
	}
	block, ok := wrapper["block"].(map[string]interface{})
	if !ok {
		return ""
	}
	kind, _ := block["type"].(string)
	if kind == "tool_use" || kind == "tool_result" {
		return kind
	}
	return ""
}

func promptCacheLookbackCandidates(candidates, breakpoints []promptCacheBreakpoint) []promptCacheBreakpoint {
	result := make([]promptCacheBreakpoint, 0)
	for _, candidate := range candidates {
		for _, bp := range breakpoints {
			if candidate.Position <= bp.Position && bp.Position-candidate.Position < 20 && candidate.CumulativeTokens <= bp.CumulativeTokens {
				result = append(result, candidate)
				break
			}
		}
	}
	return result
}

func (t *promptCacheTracker) computeTotalCacheUsage(accountID string, profile *promptCacheProfile, settings config.PromptCacheSettings) promptCacheUsage {
	usage := promptCacheUsage{totalCacheRate: true, snapshot: true, startedAt: time.Now()}
	if profile == nil || len(profile.Breakpoints) == 0 {
		usage.limitation = "no_cache_breakpoint"
		return usage
	}
	usage.rawCacheable = maxInt(0, minInt(profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens, profile.TotalInputTokens))
	usage.totalRatio = variedPrefixReadRatio(settings.Ratio, profile)
	if !settings.Enabled {
		usage.totalRatio = 0
		usage.limitation = "disabled"
		return usage
	}
	if usage.totalRatio <= 0 {
		usage.limitation = "zero_rate"
		return usage
	}
	floor := totalCacheMinTokens(profile.Model)
	if floor == math.MaxInt {
		usage.limitation = "unsupported_model"
		return usage
	}
	if usage.rawCacheable < floor {
		usage.limitation = "below_model_minimum"
	}
	if t == nil || accountID == "" {
		usage.limitation = "no_account"
		return usage
	}
	now := usage.startedAt
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)
	entries := t.entriesByAccount[accountID]
	candidates := profile.LookupCandidates
	if len(candidates) == 0 {
		candidates = profile.Breakpoints
	}
	for _, bp := range candidates {
		// Preserve potential matches even when the dispatch estimate is below
		// the minimum; the final native count may make this prefix eligible.
		if bp.CumulativeTokens <= 0 || bp.CumulativeTokens > usage.rawCacheable || bp.CumulativeTokens <= usage.rawRead {
			continue
		}
		entry, ok := entries[bp.Fingerprint]
		if !ok || !entry.ExpiresAt.After(now) {
			continue
		}
		usage.rawRead = bp.CumulativeTokens
		usage.matchedFingerprint = bp.Fingerprint
		usage.matchedTTL = entry.TTL
	}
	return allocateTotalCacheUsage(usage, profile, usage.rawCacheable, usage.rawRead)
}

// This scales usage categories, not a physical subset of cached tokens.
func allocateTotalCacheUsage(usage promptCacheUsage, profile *promptCacheProfile, eligible, matched int) promptCacheUsage {
	usage.CacheCreationInputTokens = 0
	usage.CacheReadInputTokens = 0
	usage.CacheCreation5mInputTokens = 0
	usage.CacheCreation1hInputTokens = 0
	if profile == nil {
		return usage
	}
	eligible = maxInt(0, minInt(eligible, profile.TotalInputTokens))
	matched = maxInt(0, minInt(matched, eligible))
	// A native count adjustment can make the old matched prefix ineligible
	// even while the final write breakpoint still meets the model minimum.
	if matched < totalCacheMinTokens(profile.Model) {
		matched = 0
	}
	usage.finalCacheable, usage.finalMatched = eligible, matched
	if usage.limitation == "disabled" || usage.limitation == "zero_rate" || usage.limitation == "unsupported_model" || usage.limitation == "no_account" || usage.limitation == "no_cache_breakpoint" {
		return usage
	}
	if eligible < totalCacheMinTokens(profile.Model) {
		usage.limitation = "below_model_minimum"
		return usage
	}
	budget := minInt(eligible, int(math.Floor(float64(profile.TotalInputTokens)*usage.totalRatio)))
	if budget <= 0 {
		return usage
	}
	usage.CacheReadInputTokens = int(int64(budget) * int64(matched) / int64(eligible))
	usage.CacheCreationInputTokens = budget - usage.CacheReadInputTokens
	raw5m, raw1h := computePromptCacheTTLBreakdownLimited(profile, matched, eligible-matched)
	usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens = allocateCacheWriteTTL(usage.CacheCreationInputTokens, raw5m, raw1h)
	usage.limitation = ""
	if eligible < int(math.Floor(float64(profile.TotalInputTokens)*usage.totalRatio)) {
		usage.limitation = "breakpoint_coverage"
	}
	return usage
}

func allocateCacheWriteTTL(write, raw5m, raw1h int) (int, int) {
	if write <= 0 {
		return 0, 0
	}
	if raw1h <= 0 {
		return write, 0
	}
	if raw5m <= 0 {
		return 0, write
	}
	total := int64(raw5m) + int64(raw1h)
	a, b := int64(write)*int64(raw5m), int64(write)*int64(raw1h)
	short, long := int(a/total), int(b/total)
	if short+long < write {
		if a%total >= b%total {
			short++
		} else {
			long++
		}
	}
	return short, long
}

// Called only after successful upstream completion. Zero policy never stores
// hidden entries, and only an actually matched candidate can be renewed.
func (t *promptCacheTracker) UpdateWithSettings(accountID string, profile *promptCacheProfile, settings config.PromptCacheSettings, usage promptCacheUsage) {
	if t == nil || !settings.Enabled {
		return
	}
	if settings.Mode != config.PromptCacheModePrefixTotal {
		t.Update(accountID, profile)
		return
	}
	if profile == nil || accountID == "" {
		return
	}
	t.recordTotalCacheDiagnostic(profile, usage)
	if usage.CacheCreationInputTokens+usage.CacheReadInputTokens <= 0 {
		return
	}
	floor := totalCacheMinTokens(profile.Model)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)
	entries := t.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[[32]byte]promptCacheEntry)
		t.entriesByAccount[accountID] = entries
	}
	store := func(fingerprint [32]byte, ttl time.Duration) {
		ttl = min(normalizePromptCacheTTL(ttl), t.maxSupportedTTL)
		if ttl <= 0 {
			return
		}
		old := entries[fingerprint]
		ttl = max(ttl, old.TTL)
		started := usage.startedAt
		if started.IsZero() {
			started = now
		}
		expires := started.Add(ttl)
		if old.ExpiresAt.After(expires) {
			expires = old.ExpiresAt
		}
		if !expires.After(now) {
			return
		}
		entries[fingerprint] = promptCacheEntry{ExpiresAt: expires, TTL: ttl}
	}
	for _, bp := range profile.Breakpoints {
		if usage.CacheCreationInputTokens > 0 && bp.CumulativeTokens > usage.finalMatched && bp.CumulativeTokens >= floor && bp.CumulativeTokens <= profile.TotalInputTokens {
			store(bp.Fingerprint, bp.TTL)
		}
	}
	if usage.finalMatched >= floor && usage.matchedTTL > 0 {
		store(usage.matchedFingerprint, usage.matchedTTL)
	}
}

type promptCacheDiagnostic struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Rule       string    `json:"rule"`
	Total      int       `json:"total"`
	Cacheable  int       `json:"cacheable"`
	Matched    int       `json:"matched"`
	Ratio      float64   `json:"ratio"`
	Budget     int       `json:"budget"`
	Read       int       `json:"read"`
	Write      int       `json:"write"`
	Ordinary   int       `json:"ordinary"`
	Write5m    int       `json:"write5m"`
	Write1h    int       `json:"write1h"`
	Limitation string    `json:"limitation,omitempty"`
}

func (t *promptCacheTracker) recordTotalCacheDiagnostic(profile *promptCacheProfile, usage promptCacheUsage) {
	if !usage.totalCacheRate || profile == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneDiagnosticsLocked(now)
	t.diagnosticSeq++
	t.diagnostics = append(t.diagnostics, promptCacheDiagnostic{ID: fmt.Sprintf("cache-%d", t.diagnosticSeq), Time: now, Rule: totalCacheRuleVersion, Total: profile.TotalInputTokens, Cacheable: usage.finalCacheable, Matched: usage.finalMatched, Ratio: usage.totalRatio, Budget: usage.CacheReadInputTokens + usage.CacheCreationInputTokens, Read: usage.CacheReadInputTokens, Write: usage.CacheCreationInputTokens, Ordinary: billedClaudeInputTokens(profile.TotalInputTokens, usage), Write5m: usage.CacheCreation5mInputTokens, Write1h: usage.CacheCreation1hInputTokens, Limitation: usage.limitation})
	if len(t.diagnostics) > promptCacheDiagnosticCapacity {
		t.diagnostics = append([]promptCacheDiagnostic(nil), t.diagnostics[len(t.diagnostics)-promptCacheDiagnosticCapacity:]...)
	}
}

func (t *promptCacheTracker) pruneDiagnosticsLocked(now time.Time) {
	first := 0
	for first < len(t.diagnostics) && now.Sub(t.diagnostics[first].Time) > promptCacheDiagnosticRetention {
		first++
	}
	if first > 0 {
		t.diagnostics = append([]promptCacheDiagnostic(nil), t.diagnostics[first:]...)
	}
}

func (h *Handler) apiPromptCacheDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	records := []promptCacheDiagnostic{}
	if h.promptCache != nil {
		h.promptCache.mu.Lock()
		h.promptCache.pruneDiagnosticsLocked(time.Now())
		records = append(records, h.promptCache.diagnostics...)
		h.promptCache.mu.Unlock()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"records": records, "capacity": promptCacheDiagnosticCapacity, "retentionHours": 24})
}
