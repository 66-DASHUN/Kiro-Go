package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"kiro-proxy/config"
	"math"
	"time"
)

const lifecycleCacheRuleVersion = "prefix-lifecycle-v3"

// Independent content/scope/attempt samples vary the cold admission probability
// and decide whether to store the whole prefix. A fresh attempt permits retries;
// the threshold never scales token buckets or applies to an existing read.
func lifecycleAdmission(ratio float64, scope string, fingerprint [32]byte, attempt uint64) (float64, bool) {
	if ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0, false
	}
	if ratio >= 1 {
		return 1, true
	}
	sample := func(domain string) float64 {
		h := sha256.New()
		writeHashChunk(h, domain)
		writeHashChunk(h, scope)
		h.Write(fingerprint[:])
		var nonce [8]byte
		binary.BigEndian.PutUint64(nonce[:], attempt)
		h.Write(nonce[:])
		// Exactly [0,1), using 53 bits representable by float64.
		return float64(binary.BigEndian.Uint64(h.Sum(nil)[:8])>>11) / (1 << 53)
	}
	lower := math.Max(0, ratio-promptCacheRatioVariation)
	upper := math.Min(1, ratio+promptCacheRatioVariation)
	effective := lower + (upper-lower)*sample("lifecycle-admission-threshold-v1")
	return effective, sample("lifecycle-admission-choice-v1") < effective
}

type lifecycleReadCheckpoint struct {
	fingerprint [32]byte
	tokens      int
	ttl         time.Duration
}

type lifecycleReadSnapshot struct {
	checkpoints []lifecycleReadCheckpoint
}

// One additive, public-input token space. Prefixes never depend on the size of
// later messages or native provider input estimates. Metadata scopes hashes but
// does not count as content. The pending assistant header is not cacheable.
func buildLifecycleProfile(req *ClaudeRequest) *promptCacheProfile {
	if req == nil {
		return nil
	}
	copyReq := *req
	copyReq.Model = totalCacheModelIdentity(req.Model)
	blocks := flattenClaudeCacheBlocks(&copyReq)
	// Count the public tool definition in the same serialized form used by
	// the reference cache profiler. Semantic fields alone omit the outer keys
	// and JSON structure. Keep legacy modes' metering unchanged, and exclude
	// cache markers plus our internal fingerprint wrapper from token content.
	for i, tool := range copyReq.Tools {
		wireTool := map[string]interface{}{
			"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema,
		}
		if tool.Type != "" {
			wireTool["type"] = tool.Type
		}
		if tool.MaxUses != 0 {
			wireTool["max_uses"] = tool.MaxUses
		}
		blocks[i+1].Tokens = estimateApproxTokens(canonicalizeCacheValue(wireTool))
	}
	p := &promptCacheProfile{lifecycle: true, Model: copyReq.Model}
	explicit := false
	for _, b := range blocks {
		if b.TTL > 0 {
			explicit = true
			break
		}
	}
	h := sha256.New()
	for _, b := range blocks {
		writeHashChunk(h, canonicalizeCacheValue(b.Value))
		// Anthropic meters the public block envelope as part of the prompt.
		// Legacy cache modes intentionally retain their historical semantic-only
		// accounting; lifecycle mode uses the complete stable JSON structure so
		// read/write values do not systematically undercount tool and message
		// prefixes. Cache-control and internal position markers are removed by
		// canonicalizeCacheValue before counting.
		if wrapper, ok := b.Value.(map[string]interface{}); ok {
			switch wrapper["kind"] {
			case "system", "message":
				if block, exists := wrapper["block"]; exists {
					b.Tokens = estimateApproxTokens(canonicalizeCacheValue(block))
					if wrapper["kind"] == "message" && b.Tokens > 0 {
						// Keep the message role framing already accounted by
						// appendMessageCacheBlocks.
						b.Tokens += claudeMessageFramingTokens
					}
				}
			}
		}
		p.TotalInputTokens += b.Tokens
		if wrapper, ok := b.Value.(map[string]interface{}); ok {
			switch wrapper["kind"] {
			case "tool":
				p.LifecycleToolTokens += b.Tokens
			case "system":
				p.LifecycleSystemTokens += b.Tokens
			case "message":
				p.LifecycleMessageTokens += b.Tokens
			}
		}
		if b.IsMetadata || p.TotalInputTokens <= 0 {
			continue
		}
		bp := promptCacheBreakpoint{CumulativeTokens: p.TotalInputTokens, TTL: b.TTL}
		copy(bp.Fingerprint[:], h.Sum(nil))
		p.LookupCandidates = append(p.LookupCandidates, bp)
		if !explicit {
			bp.TTL = defaultPromptCacheTTL
		}
		if bp.TTL > 0 {
			p.Breakpoints = append(p.Breakpoints, bp)
		}
	}
	copy(p.InputFingerprint[:], h.Sum(nil))
	p.TotalInputTokens += claudeGenerationPrefixTokens
	return p
}

func lifecycleOpenAIRequest(req *OpenAIRequest) *ClaudeRequest {
	if req == nil {
		return nil
	}
	out := openAIUsageRequest(req)
	out.Model, out.ToolChoice = req.Model, req.ToolChoice
	// The pinned reference converter drops cache-control metadata and joins
	// system messages into a string. Preserve the original content boundaries
	// here without changing that converter's accounting in other modes.
	out.Messages = nil
	var system []interface{}
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, lifecycleOpenAIBlocks(m.Content)...)
			continue
		}
		one := openAIUsageRequest(&OpenAIRequest{Messages: []OpenAIMessage{m}})
		if len(one.Messages) == 0 {
			continue
		}
		converted := one.Messages[0]
		if m.Role != "tool" {
			blocks := lifecycleOpenAIBlocks(m.Content)
			// Tool-call blocks are already normalized by the reference bridge.
			if len(m.ToolCalls) > 0 {
				all := converted.Content.([]interface{})
				blocks = append(blocks, all[len(all)-len(m.ToolCalls):]...)
			}
			converted.Content = blocks
		}
		out.Messages = append(out.Messages, converted)
	}
	out.System = system
	out.Messages = mergeUsageMessages(out.Messages)
	// Reasoning effort belongs to the cache identity, not token content.
	if req.ReasoningEffort != "" {
		out.OutputConfig = &ClaudeOutputConfig{Effort: req.ReasoningEffort}
	}
	for i, tool := range req.Tools {
		out.Tools[i] = ClaudeTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters}
		if tool.HostedWebSearch {
			out.Tools[i].Type = "web_search"
		}
	}
	return out
}

func lifecycleOpenAIBlocks(content interface{}) []interface{} {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": text}}
	}
	values, ok := content.([]interface{})
	if !ok {
		if block, ok := content.(map[string]interface{}); ok {
			values = []interface{}{block}
		}
	}
	var blocks []interface{}
	for _, value := range values {
		converted := referenceOpenAIBlock(value)
		if converted == nil {
			continue
		}
		if original, ok := value.(map[string]interface{}); ok {
			if cc, exists := original["cache_control"]; exists {
				if b, ok := converted.(map[string]interface{}); ok {
					copyBlock := make(map[string]interface{}, len(b)+1)
					for k, v := range b {
						copyBlock[k] = v
					}
					copyBlock["cache_control"] = cc
					converted = copyBlock
				}
			}
		}
		blocks = append(blocks, converted)
	}
	return blocks
}

func estimateClaudeInputWithSettings(req *ClaudeRequest, settings config.PromptCacheSettings) int {
	if settings.Mode == config.PromptCacheModeLifecycle {
		if p := buildLifecycleProfile(req); p != nil {
			return p.TotalInputTokens
		}
		return 0
	}
	return estimateClaudeRequestInputTokens(req)
}

func estimateOpenAIInputWithSettings(req *OpenAIRequest, settings config.PromptCacheSettings) int {
	if settings.Mode == config.PromptCacheModeLifecycle {
		return estimateClaudeInputWithSettings(lifecycleOpenAIRequest(req), settings)
	}
	return estimateOpenAIRequestInputTokens(req)
}

func lifecycleScope(account string, p *promptCacheProfile, s config.PromptCacheSettings) string {
	h := sha256.New()
	writeHashChunk(h, account)
	writeHashChunk(h, s.APIKeyID)
	writeHashChunk(h, p.Model)
	return fmt.Sprintf("lifecycle:%x", h.Sum(nil))
}

func (t *promptCacheTracker) computeLifecycleUsage(account string, p *promptCacheProfile, s config.PromptCacheSettings) promptCacheUsage {
	u := promptCacheUsage{lifecycle: true, snapshot: true, startedAt: time.Now()}
	if t == nil || p == nil || !p.lifecycle || account == "" || !s.Enabled {
		u.limitation = "disabled"
		return u
	}
	if len(p.Breakpoints) == 0 {
		u.limitation = "no_cache_breakpoint"
		return u
	}
	u.finalCacheable = p.Breakpoints[len(p.Breakpoints)-1].CumulativeTokens
	u.totalRatio = s.Ratio
	u.lifecycleReads = &lifecycleReadSnapshot{}
	t.mu.Lock()
	t.pruneExpiredLocked(u.startedAt)
	scope := lifecycleScope(account, p, s)
	entries := t.entriesByAccount[scope]
	for _, bp := range p.LookupCandidates {
		if bp.CumulativeTokens > u.finalCacheable || bp.CumulativeTokens <= u.finalMatched {
			continue
		}
		e, ok := entries[bp.Fingerprint]
		if !ok || !e.ExpiresAt.After(u.startedAt) || e.PrefixTokens != bp.CumulativeTokens || e.CachedTokens != e.PrefixTokens {
			continue
		}
		u.finalMatched, u.matchedPrefixTokens, u.matchedCachedTokens = e.PrefixTokens, e.PrefixTokens, e.CachedTokens
		u.matchedFingerprint, u.matchedTTL = bp.Fingerprint, e.TTL
		u.lifecycleReads.checkpoints = append(u.lifecycleReads.checkpoints, lifecycleReadCheckpoint{bp.Fingerprint, e.PrefixTokens, e.TTL})
	}
	if u.finalMatched == 0 {
		t.lifecycleAdmissionSeq++
		u.cacheAdmissionChecked = true
		last := p.Breakpoints[len(p.Breakpoints)-1]
		u.totalRatio, u.cacheWriteAdmitted = lifecycleAdmission(s.Ratio, scope, last.Fingerprint, t.lifecycleAdmissionSeq)
	}
	t.mu.Unlock()
	u.CacheReadInputTokens = u.finalMatched
	if u.cacheAdmissionChecked && !u.cacheWriteAdmitted {
		u.limitation = "cache_admission_miss"
		return u
	}
	u.CacheCreationInputTokens = u.finalCacheable - u.finalMatched
	u.CacheCreation5mInputTokens, u.CacheCreation1hInputTokens = computePromptCacheTTLBreakdownLimited(p, u.finalMatched, u.CacheCreationInputTokens)
	return u
}

// Only successful completion calls this function. Reads never mutate entries;
// in-flight requests retain their dispatch snapshot even if another completes.
func (t *promptCacheTracker) updateLifecycle(account string, p *promptCacheProfile, s config.PromptCacheSettings, u promptCacheUsage) {
	if p == nil || !p.lifecycle || !u.lifecycle || account == "" || !s.Enabled {
		return
	}
	t.recordTotalCacheDiagnostic(p, u)
	if u.CacheCreationInputTokens+u.CacheReadInputTokens == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)
	scope := lifecycleScope(account, p, s)
	entries := t.entriesByAccount[scope]
	if entries == nil {
		entries = make(map[[32]byte]promptCacheEntry)
		t.entriesByAccount[scope] = entries
	}
	store := func(fp [32]byte, tokens int, ttl time.Duration) {
		ttl = min(normalizePromptCacheTTL(ttl), t.maxSupportedTTL)
		if ttl <= 0 || tokens <= 0 {
			return
		}
		expires := now.Add(ttl)
		if old := entries[fp]; old.ExpiresAt.After(expires) {
			expires = old.ExpiresAt
			ttl = max(ttl, old.TTL)
		}
		entries[fp] = promptCacheEntry{ExpiresAt: expires, TTL: ttl, PrefixTokens: tokens, CachedTokens: tokens}
	}
	// Prefix checkpoints depend on every preceding segment. Conservatively cap
	// their lifetime at the shortest required segment; no phantom long-lived hit.
	dependentTTL := t.maxSupportedTTL
	if u.finalMatched > 0 {
		// The caller can move or change cache markers between requests. Reading
		// an existing prefix does not upgrade its stored lifetime to a new TTL.
		dependentTTL = min(dependentTTL, normalizePromptCacheTTL(u.matchedTTL))
	}
	for _, bp := range p.Breakpoints {
		dependentTTL = min(dependentTTL, normalizePromptCacheTTL(bp.TTL))
		if bp.CumulativeTokens > u.finalMatched && bp.CumulativeTokens <= u.finalCacheable {
			store(bp.Fingerprint, bp.CumulativeTokens, dependentTTL)
		}
	}
	// Reading a longer prefix also reads its earlier checkpoints. Renew them so
	// a later edit of the suffix can still reuse the unchanged earlier content.
	// Use the dispatch snapshot: checkpoints can expire while a long request
	// is running, but the completed read still renews every checkpoint it used.
	if u.lifecycleReads != nil {
		for _, checkpoint := range u.lifecycleReads.checkpoints {
			store(checkpoint.fingerprint, checkpoint.tokens, checkpoint.ttl)
		}
	}
	if u.finalMatched > 0 {
		store(u.matchedFingerprint, u.matchedPrefixTokens, u.matchedTTL)
	}
}
