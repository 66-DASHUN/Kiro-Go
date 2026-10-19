package proxy

import (
	"crypto/sha256"
	"fmt"
	"kiro-proxy/config"
	"time"
)

const lifecycleCacheRuleVersion = "prefix-lifecycle-v1"

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
		p.TotalInputTokens += b.Tokens
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
	u.lifecycleReads = &lifecycleReadSnapshot{}
	t.mu.Lock()
	t.pruneExpiredLocked(u.startedAt)
	entries := t.entriesByAccount[lifecycleScope(account, p, s)]
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
	t.mu.Unlock()
	u.CacheReadInputTokens = u.finalMatched
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
