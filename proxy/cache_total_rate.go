package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"math"
	"net/http"
	"time"
)

const totalCacheRuleVersion = "prefix-total-v10"
const promptCacheDiagnosticCapacity = 200
const promptCacheDiagnosticRetention = 24 * time.Hour

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
	shareProfile := *profile
	shareProfile.InputFingerprint = profile.Breakpoints[len(profile.Breakpoints)-1].Fingerprint
	usage.totalRatio = variedPrefixReadRatio(settings.Ratio, &shareProfile)
	if !settings.Enabled {
		usage.totalRatio = 0
		usage.limitation = "disabled"
		return usage
	}
	if settings.Ratio <= 0 || math.IsNaN(settings.Ratio) || math.IsInf(settings.Ratio, 0) {
		usage.limitation = "zero_rate"
		return usage
	}
	floor := totalCacheMinTokens(profile.Model)
	if floor == math.MaxInt {
		usage.limitation = "empty_model"
		return usage
	}
	if usage.rawCacheable < floor {
		usage.limitation = "empty_cache_prefix"
		return usage
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
		// Preserve dispatch matches for final native token calibration.
		if bp.CumulativeTokens <= 0 || bp.CumulativeTokens > usage.rawCacheable || bp.CumulativeTokens <= usage.rawRead {
			continue
		}
		entry, ok := entries[bp.Fingerprint]
		if !ok || !entry.ExpiresAt.After(now) || entry.PrefixTokens <= 0 || entry.CachedTokens <= 0 || entry.CachedTokens > entry.PrefixTokens {
			continue
		}
		usage.rawRead = bp.CumulativeTokens
		usage.matchedFingerprint = bp.Fingerprint
		usage.matchedTTL = entry.TTL
		usage.matchedPrefixTokens = entry.PrefixTokens
		usage.matchedCachedTokens = entry.CachedTokens
	}
	usage.cacheAdmissionChecked = true
	usage.cacheWriteAdmitted = usage.rawCacheable <= usage.rawRead || usage.rawRead > 0
	if usage.rawCacheable > usage.rawRead && usage.rawRead == 0 {
		for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
			bp := profile.Breakpoints[i]
			if bp.CumulativeTokens <= usage.rawRead || bp.CumulativeTokens > usage.rawCacheable {
				continue
			}
			admissionRatio := totalCacheAdmissionRatio(settings.Ratio, profile.Model, bp.Fingerprint)
			usage.cacheWriteAdmitted = totalCacheAdmission(admissionRatio, profile.Model, bp.Fingerprint)
			break
		}
		if !usage.cacheWriteAdmitted {
			usage.limitation = "cache_admission_miss"
		}
	}
	return allocateTotalCacheUsage(usage, profile, usage.rawCacheable, usage.rawRead)
}

func totalCacheAdmissionRatio(ratio float64, model string, fingerprint [32]byte) float64 {
	if ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0
	}
	if ratio >= 1 {
		return 1
	}
	reference := math.Min(.95, ratio)
	lower := math.Max(0, reference-promptCacheRatioVariation)
	upper := math.Min(.95, reference+promptCacheRatioVariation)
	hasher := sha256.New()
	hasher.Write(fingerprint[:])
	writeHashChunk(hasher, model)
	writeHashChunk(hasher, "prefix-total-admission-ratio-v1")
	digest := hasher.Sum(nil)
	unit := float64(binary.BigEndian.Uint16(digest[:2])) / 65535
	return lower + (upper-lower)*unit
}

// A prefix-specific threshold admits whole content prefixes across requests.
// It must not split one stable prefix into cached and ordinary token slices.
func totalCacheAdmission(ratio float64, model string, fingerprint [32]byte) bool {
	if ratio <= 0 {
		return false
	}
	if ratio >= 1 {
		return true
	}
	if fingerprint == ([32]byte{}) {
		return false
	}
	hasher := sha256.New()
	hasher.Write(fingerprint[:])
	writeHashChunk(hasher, model)
	writeHashChunk(hasher, "prefix-total-admission-v1")
	digest := hasher.Sum(nil)
	sample := float64(binary.BigEndian.Uint32(digest[:4])) / float64(^uint32(0))
	return sample < ratio
}

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
	// A native count adjustment may round a matched prefix down to zero.
	if matched < totalCacheMinTokens(profile.Model) {
		matched = 0
	}
	usage.finalCacheable, usage.finalMatched = eligible, matched
	if usage.limitation == "disabled" || usage.limitation == "zero_rate" || usage.limitation == "empty_model" || usage.limitation == "no_account" || usage.limitation == "no_cache_breakpoint" {
		return usage
	}
	if eligible < totalCacheMinTokens(profile.Model) {
		usage.limitation = "empty_cache_prefix"
		return usage
	}
	if usage.totalRatio <= 0 && !usage.cacheAdmissionChecked {
		return usage
	}
	usage.CacheReadInputTokens = maxInt(0, minInt(usage.matchedCachedTokens, matched))
	if usage.cacheAdmissionChecked && !usage.cacheWriteAdmitted && eligible > matched {
		usage.limitation = "cache_admission_miss"
		return usage
	}
	usage.CacheCreationInputTokens = maxInt(eligible-matched, 0)
	raw5m, raw1h := computePromptCacheTTLBreakdownLimited(profile, matched, eligible-matched)
	usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens = allocateCacheWriteTTL(usage.CacheCreationInputTokens, raw5m, raw1h)
	usage.limitation = ""
	if eligible < profile.TotalInputTokens {
		usage.limitation = "breakpoint_coverage"
	}
	if matched > 0 && usage.matchedCachedTokens > matched {
		usage.limitation = "matched_share_clamped"
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
	store := func(fingerprint [32]byte, ttl time.Duration, prefixTokens, cachedTokens int) {
		if prefixTokens <= 0 || cachedTokens <= 0 || cachedTokens > prefixTokens {
			return
		}
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
		// Concurrent completions may have committed this prefix since dispatch.
		// The first live certified share remains the accounting basis.
		if old.PrefixTokens > 0 && old.CachedTokens > 0 && old.CachedTokens <= old.PrefixTokens {
			prefixTokens, cachedTokens = old.PrefixTokens, old.CachedTokens
		}
		entries[fingerprint] = promptCacheEntry{ExpiresAt: expires, TTL: ttl, PrefixTokens: prefixTokens, CachedTokens: cachedTokens}
	}
	for _, bp := range profile.Breakpoints {
		if usage.CacheCreationInputTokens > 0 && bp.CumulativeTokens > usage.finalMatched && bp.CumulativeTokens >= floor && bp.CumulativeTokens <= usage.finalCacheable {
			newShare := int(int64(usage.CacheCreationInputTokens) * int64(bp.CumulativeTokens-usage.finalMatched) / int64(usage.finalCacheable-usage.finalMatched))
			if newShare > 0 {
				store(bp.Fingerprint, bp.TTL, bp.CumulativeTokens, usage.CacheReadInputTokens+newShare)
			}
		}
	}
	if usage.finalMatched >= floor && usage.matchedTTL > 0 {
		store(usage.matchedFingerprint, usage.matchedTTL, usage.matchedPrefixTokens, usage.matchedCachedTokens)
	}
}

type promptCacheDiagnostic struct {
	ID                  string    `json:"id"`
	Time                time.Time `json:"time"`
	Rule                string    `json:"rule"`
	Total               int       `json:"total"`
	Cacheable           int       `json:"cacheable"`
	Matched             int       `json:"matched"`
	MatchedShare        int       `json:"matchedShare"`
	MatchedTTLSeconds   int       `json:"matchedTtlSeconds,omitempty"`
	MatchedPrefixTokens int       `json:"matchedPrefixTokens,omitempty"`
	Ratio               float64   `json:"ratio"`
	Budget              int       `json:"budget"`
	Read                int       `json:"read"`
	Write               int       `json:"write"`
	Ordinary            int       `json:"ordinary"`
	Write5m             int       `json:"write5m"`
	Write1h             int       `json:"write1h"`
	Limitation          string    `json:"limitation,omitempty"`
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
	t.diagnostics = append(t.diagnostics, promptCacheDiagnostic{ID: fmt.Sprintf("cache-%d", t.diagnosticSeq), Time: now, Rule: totalCacheRuleVersion, Total: profile.TotalInputTokens, Cacheable: usage.finalCacheable, Matched: usage.finalMatched, MatchedShare: usage.matchedCachedTokens, MatchedTTLSeconds: int(usage.matchedTTL / time.Second), MatchedPrefixTokens: usage.matchedPrefixTokens, Ratio: usage.totalRatio, Budget: usage.CacheReadInputTokens + usage.CacheCreationInputTokens, Read: usage.CacheReadInputTokens, Write: usage.CacheCreationInputTokens, Ordinary: billedClaudeInputTokens(profile.TotalInputTokens, usage), Write5m: usage.CacheCreation5mInputTokens, Write1h: usage.CacheCreation1hInputTokens, Limitation: usage.limitation})
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
