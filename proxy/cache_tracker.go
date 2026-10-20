package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"kiro-proxy/config"
	refcache "kiro-proxy/internal/sub2apiref/cache"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPromptCacheTTL = 5 * time.Minute
const maxPromptCacheTTL = time.Hour

const promptCacheRatioVariation = 0.05

const (
	//! Anthropic ignores cache prefixes below the model-specific token floor.
	defaultMinCacheableTokens = 1024
	opusMinCacheableTokens    = 4096
)

type promptCacheUsage struct {
	lifecycle      bool
	lifecycleReads *lifecycleReadSnapshot
	exactPlan      *refcache.Plan
	exactInput     *int
	referenceMode  bool
	totalCacheRate bool
	totalRatio     float64
	// The positive-rate lookup must remain valid when a varied write threshold is zero.
	cacheAdmissionChecked      bool
	cacheWriteAdmitted         bool
	startedAt                  time.Time
	matchedFingerprint         [32]byte
	matchedTTL                 time.Duration
	matchedPrefixTokens        int
	matchedCachedTokens        int
	finalCacheable             int
	finalMatched               int
	limitation                 string
	prefixReadWrite            bool
	rawCacheable               int
	rawRead                    int
	rawCreation                int
	rawCreation5m              int
	creationRatio              float64
	readRatio                  float64
	snapshot                   bool
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
	Position         int
}

type promptCacheProfile struct {
	lifecycle        bool
	exactProfile     *refcache.Profile
	referenceMode    bool
	Breakpoints      []promptCacheBreakpoint
	LookupCandidates []promptCacheBreakpoint
	TotalInputTokens int
	// Lifecycle-only content breakdown used by diagnostics and regression tests.
	// These values are in the same token space as TotalInputTokens and do not
	// alter cache admission or billing.
	LifecycleToolTokens    int
	LifecycleSystemTokens  int
	LifecycleMessageTokens int
	Model            string
	InputFingerprint [32]byte
}

func minCacheableTokensForModel(model string) int {
	lower := strings.ToLower(model)
	if strings.Contains(strings.ReplaceAll(lower, ".", "-"), "haiku-4-5") {
		return 4096
	}
	if strings.Contains(lower, "opus") {
		return opusMinCacheableTokens
	}
	return defaultMinCacheableTokens
}

type promptCacheEntry struct {
	ExpiresAt time.Time
	TTL       time.Duration
	// Only total-mode successful writes certify these immutable counts.
	PrefixTokens int
	CachedTokens int
}

type promptCacheTracker struct {
	exactTracker          *refcache.Tracker
	mu                    sync.Mutex
	entriesByAccount      map[string]map[[32]byte]promptCacheEntry
	maxSupportedTTL       time.Duration
	diagnostics           []promptCacheDiagnostic
	diagnosticSeq         uint64
	lifecycleAdmissionSeq uint64 // guarded by mu; one decision per cold dispatch
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
	if maxTTL <= 0 {
		maxTTL = maxPromptCacheTTL
	}
	maxTTL = normalizePromptCacheTTL(maxTTL)
	return &promptCacheTracker{
		entriesByAccount: make(map[string]map[[32]byte]promptCacheEntry),
		exactTracker:     refcache.NewTracker(),
		maxSupportedTTL:  maxTTL,
	}
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int) *promptCacheProfile {
	return t.buildClaudeProfile(req, totalInputTokens, false, false, false)
}

func (t *promptCacheTracker) BuildClaudeProfileWithSettings(req *ClaudeRequest, totalInputTokens int, settings config.PromptCacheSettings) *promptCacheProfile {
	if settings.Mode == config.PromptCacheModeLifecycle {
		return buildLifecycleProfile(req)
	}
	if settings.Mode == config.PromptCacheModeSub2API {
		if req == nil {
			return nil
		}
		body, _ := json.Marshal(req)
		return buildExactCacheProfile("anthropic", body, req.Model, totalInputTokens)
	}
	totalMode := settings.Mode == config.PromptCacheModePrefixTotal
	referenceMode := settings.Mode == config.PromptCacheModeSub2API
	if (totalMode || referenceMode) && req != nil {
		cacheReq := *req
		cacheReq.Model = totalCacheModelIdentity(req.Model)
		req = &cacheReq
	}
	return t.buildClaudeProfile(req, totalInputTokens, settings.Mode == config.PromptCacheModePrefix || totalMode, totalMode, referenceMode)
}

func (t *promptCacheTracker) buildClaudeProfile(req *ClaudeRequest, totalInputTokens int, strictBreakpoints, totalCacheRate, referenceMode bool) *promptCacheProfile {
	if t == nil || req == nil {
		return nil
	}
	blocks := flattenClaudeCacheBlocks(req)
	if len(blocks) == 0 {
		return nil
	}

	hasher := sha256.New()
	breakpoints := make([]promptCacheBreakpoint, 0)
	candidates := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	position := 0
	previousGroup := ""
	var activeTTL time.Duration
	hasExplicitBreakpoint := false
	for _, block := range blocks {
		if block.TTL > 0 {
			hasExplicitBreakpoint = true
			break
		}
	}

	for blockIndex, block := range blocks {
		canonical := canonicalizeCacheValue(block.Value)
		writeHashChunk(hasher, canonical)
		cumulativeTokens += block.Tokens
		if !block.IsMetadata {
			group := promptCacheBlockGroup(block.Value)
			if group == "" || group != previousGroup {
				position++
			}
			previousGroup = group
			if block.IsMessageEnd {
				previousGroup = ""
			}
			var fingerprint [32]byte
			copy(fingerprint[:], hasher.Sum(nil))
			candidates = append(candidates, promptCacheBreakpoint{Fingerprint: fingerprint, CumulativeTokens: cumulativeTokens, Position: position})
		}

		// Determine whether this block acts as a cache breakpoint:
		//   1) Explicit cache_control on the block itself.
		//   2) Once any explicit breakpoint has been seen, every message-end
		//      boundary becomes an implicit breakpoint so that multi-turn
		//      conversations can hit earlier stored prefixes.
		breakpointTTL := time.Duration(0)
		if !hasExplicitBreakpoint {
			// Local emulation retains earlier block boundaries so growing
			// conversations can reuse a prefix without explicit cache_control.
			if !referenceMode || block.IsMessageEnd || blockIndex == len(blocks)-1 {
				breakpointTTL = defaultPromptCacheTTL
			}
		} else if block.TTL > 0 {
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if !strictBreakpoints && block.IsMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		var fingerprint [32]byte
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
			Position:         position,
		})
	}

	semanticTotal := estimateClaudeRequestInputTokens(req)
	if totalInputTokens <= 0 {
		totalInputTokens = semanticTotal
	}
	// Normalize estimated cumulative tokens into the caller's total-input
	// token space. Fingerprint metadata must not inflate the caller's count.
	if len(breakpoints) > 0 {
		normalizationBase := semanticTotal
		if referenceMode {
			normalizationBase = breakpoints[len(breakpoints)-1].CumulativeTokens
		}
		if normalizationBase > 0 && totalInputTokens != normalizationBase {
			for i := range breakpoints {
				n := int(math.Round(float64(breakpoints[i].CumulativeTokens) * float64(totalInputTokens) / float64(normalizationBase)))
				if i > 0 && n < breakpoints[i-1].CumulativeTokens {
					n = breakpoints[i-1].CumulativeTokens
				}
				breakpoints[i].CumulativeTokens = minInt(n, totalInputTokens)
			}
		}
	}
	if totalCacheRate {
		for i := range candidates {
			if semanticTotal > 0 && totalInputTokens != semanticTotal {
				candidates[i].CumulativeTokens = minInt(totalInputTokens, int(math.Round(float64(candidates[i].CumulativeTokens)*float64(totalInputTokens)/float64(semanticTotal))))
			}
		}
		candidates = promptCacheLookbackCandidates(candidates, breakpoints)
	} else {
		candidates = nil
	}

	var inputFingerprint [32]byte
	copy(inputFingerprint[:], hasher.Sum(nil))
	return &promptCacheProfile{
		referenceMode:    referenceMode,
		Breakpoints:      breakpoints,
		LookupCandidates: candidates,
		TotalInputTokens: totalInputTokens,
		Model:            req.Model,
		InputFingerprint: inputFingerprint,
	}
}

// BuildOpenAIProfile creates a conservative, local-only cache profile for an
// OpenAI request after it has been translated to Kiro's wire payload. Prefixes
// cover the effective input, including the current turn. Transport identifiers
// are excluded, and user messages retain the same shape when moved into history.
func (t *promptCacheTracker) BuildOpenAIProfile(payload *KiroPayload, totalInputTokens int) *promptCacheProfile {
	return t.buildOpenAIProfile(payload, totalInputTokens, false)
}

func (t *promptCacheTracker) BuildOpenAIProfileWithSettings(payload *KiroPayload, totalInputTokens int, settings config.PromptCacheSettings) *promptCacheProfile {
	if settings.Mode == config.PromptCacheModeLifecycle {
		if payload == nil {
			return nil
		}
		return buildLifecycleProfile(payload.lifecycleRequest)
	}
	if settings.Mode == config.PromptCacheModeSub2API {
		if payload == nil || payload.exactMeter == nil {
			return nil
		}
		m := payload.exactMeter
		return buildExactCacheProfile(m.Protocol, m.Body, m.Model, totalInputTokens)
	}
	profile := t.buildOpenAIProfile(payload, totalInputTokens, settings.Mode == config.PromptCacheModePrefixTotal || settings.Mode == config.PromptCacheModeSub2API)
	if profile != nil {
		profile.referenceMode = settings.Mode == config.PromptCacheModeSub2API
	}
	return profile
}

func (t *promptCacheTracker) buildOpenAIProfile(payload *KiroPayload, totalInputTokens int, includeShortProfile bool) *promptCacheProfile {
	if t == nil || payload == nil || totalInputTokens <= 0 {
		return nil
	}

	current := payload.ConversationState.CurrentMessage.UserInputMessage
	model := current.ModelID
	if includeShortProfile {
		model = totalCacheModelIdentity(model)
	}
	var tools []KiroToolWrapper
	if current.UserInputMessageContext != nil {
		tools = current.UserInputMessageContext.Tools
	}
	stable := map[string]interface{}{
		"model":            model,
		"inference_config": payload.InferenceConfig,
		"model_context":    payload.AdditionalModelRequestFields,
		"tools":            tools,
	}

	hasher := sha256.New()
	cumulativeTokens := 0
	appendStableBlock := func(value interface{}, tokens int) {
		canonical := canonicalizeCacheValue(value)
		writeHashChunk(hasher, canonical)
		cumulativeTokens += tokens
	}
	toolTokens := 0
	for _, tool := range tools {
		toolTokens += estimateKiroToolTokens(tool)
	}
	appendStableBlock(stable, toolTokens)

	breakpoints := make([]promptCacheBreakpoint, 0, len(payload.ConversationState.History)+2)
	appendBreakpoint := func() {
		fingerprint := [32]byte{}
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              defaultPromptCacheTTL,
		})
	}
	appendBreakpoint()
	for _, historyMessage := range payload.ConversationState.History {
		if historyMessage.UserInputMessage != nil {
			appendStableBlock(openAIUserCacheValue(*historyMessage.UserInputMessage), estimateKiroUserInputTokens(historyMessage.UserInputMessage, false))
			appendBreakpoint()
		}
		if historyMessage.AssistantResponseMessage != nil {
			appendStableBlock(map[string]interface{}{
				"role": "assistant", "message": historyMessage.AssistantResponseMessage,
			}, estimateClaudeOutputTokens(historyMessage.AssistantResponseMessage.Content, "", historyMessage.AssistantResponseMessage.ToolUses))
			appendBreakpoint()
		}
	}
	appendStableBlock(openAIUserCacheValue(current), estimateKiroUserInputTokens(&current, false))
	appendBreakpoint()
	if cumulativeTokens <= 0 || (!includeShortProfile && totalInputTokens < minCacheableTokensForModel(current.ModelID)) {
		return nil
	}
	for i := range breakpoints {
		breakpoints[i].CumulativeTokens = minInt(totalInputTokens, int(math.Round(float64(breakpoints[i].CumulativeTokens)*float64(totalInputTokens)/float64(cumulativeTokens))))
	}

	var inputFingerprint [32]byte
	copy(inputFingerprint[:], hasher.Sum(nil))
	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            model,
		InputFingerprint: inputFingerprint,
	}
}

func openAIUserCacheValue(message KiroUserInputMessage) map[string]interface{} {
	value := map[string]interface{}{
		"role": "user", "content": message.Content,
		"images": message.Images, "documents": message.Documents,
	}
	var results []KiroToolResult
	if message.UserInputMessageContext != nil {
		results = message.UserInputMessageContext.ToolResults
	}
	value["tool_results"] = results
	return value
}

// Compute uses a full-ratio policy; all request paths use the effective key policy.
func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) promptCacheUsage {
	return t.ComputeWithSettings(accountID, profile, config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1})
}

func (t *promptCacheTracker) ComputeWithSettings(accountID string, profile *promptCacheProfile, settings config.PromptCacheSettings) promptCacheUsage {
	if settings.Mode == config.PromptCacheModeLifecycle {
		return t.computeLifecycleUsage(accountID, profile, settings)
	}
	if settings.Mode == config.PromptCacheModeSub2API {
		return t.computeExactCacheUsage(accountID, profile, settings)
	}
	if settings.Mode == config.PromptCacheModePrefixTotal {
		return t.computeTotalCacheUsage(accountID, profile, settings)
	}
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" || !settings.Enabled {
		return promptCacheUsage{}
	}
	if settings.Mode != config.PromptCacheModeIndependent {
		settings.CreationRatio = settings.Ratio
		settings.ReadRatio = settings.Ratio
	}
	clampRatio := func(r float64) float64 {
		if math.IsNaN(r) || math.IsInf(r, 0) || r < 0 {
			return 0
		}
		if r > 1 {
			return 1
		}
		return r
	}
	createRatio, readRatio := clampRatio(settings.CreationRatio), clampRatio(settings.ReadRatio)
	createRatio = variedPromptCacheRatio(createRatio, profile)
	readRatio = variedPromptCacheRatio(readRatio, profile)
	minTokens := minCacheableTokensForModel(profile.Model)
	lastTokens := minInt(profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens, profile.TotalInputTokens)
	if lastTokens < minTokens {
		return promptCacheUsage{}
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)
	entries := t.entriesByAccount[accountID]
	matched := 0
	for i, lookback := len(profile.Breakpoints)-1, 0; i >= 0 && lookback < 10; i, lookback = i-1, lookback+1 {
		bp := profile.Breakpoints[i]
		if bp.CumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[bp.Fingerprint]
		if !ok || !entry.ExpiresAt.After(now) {
			continue
		}
		matched = minInt(bp.CumulativeTokens, lastTokens)
		break
	}
	rawCreation := maxInt(lastTokens-matched, 0)
	if settings.Mode == config.PromptCacheModePrefix {
		usage := promptCacheUsage{prefixReadWrite: true, rawCacheable: lastTokens, rawRead: matched, readRatio: variedPrefixReadRatio(settings.Ratio, profile), snapshot: true}
		return computePrefixReadWriteUsage(usage, profile, lastTokens, matched)
	}
	cache5m, cache1h := computePromptCacheTTLBreakdownLimited(profile, matched, rawCreation)
	creation := int(float64(rawCreation) * createRatio)
	write5m := int(float64(cache5m) * createRatio)
	// Assign rounding remainder to the second bucket so the breakdown always sums.
	write1h := creation - write5m
	if cache1h == 0 {
		write5m = creation
		write1h = 0
	}
	read := minInt(int(float64(matched)*readRatio), profile.TotalInputTokens)
	return promptCacheUsage{CacheCreationInputTokens: creation, CacheReadInputTokens: read, CacheCreation5mInputTokens: write5m, CacheCreation1hInputTokens: write1h, rawRead: matched, rawCreation: rawCreation, rawCreation5m: cache5m, creationRatio: createRatio, readRatio: readRatio, snapshot: true}
}

// Prefix mode allocates the entire eligible interval to read/write. Only the
// content beyond the final cache breakpoint remains ordinary input.
func computePrefixReadWriteUsage(usage promptCacheUsage, profile *promptCacheProfile, eligible, matched int) promptCacheUsage {
	usage.CacheReadInputTokens = minInt(int(float64(matched)*usage.readRatio), int(.95*float64(eligible)))
	usage.CacheCreationInputTokens = eligible - usage.CacheReadInputTokens
	usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens = computePromptCacheTTLBreakdownLimited(profile, usage.CacheReadInputTokens, usage.CacheCreationInputTokens)
	return usage
}

func variedPrefixReadRatio(ratio float64, profile *promptCacheProfile) float64 {
	if ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0
	}
	if ratio >= 1 {
		return 1
	}
	reference := math.Min(.95, ratio)
	lower, upper := math.Max(0, reference-promptCacheRatioVariation), math.Min(.95, reference+promptCacheRatioVariation)
	return lower + (upper-lower)*(promptCacheVariationUnit(profile)+1)/2
}

// This is a local usage simulation, not native upstream cache telemetry.
// Use the request input as a deterministic seed, keeping identical inputs
// stable between cold/warm requests and dispatch/completion. Keep endpoint
// settings exact and shrink the symmetric range near 0% and 100%.
func variedPromptCacheRatio(ratio float64, profile *promptCacheProfile) float64 {
	if ratio <= 0 || ratio >= 1 || profile == nil {
		return ratio
	}
	spread := math.Min(promptCacheRatioVariation, math.Min(ratio, 1-ratio))
	return math.Max(0, math.Min(1, ratio+promptCacheVariationUnit(profile)*spread))
}

func promptCacheVariationUnit(profile *promptCacheProfile) float64 {
	if profile == nil {
		return 0
	}
	fingerprint := profile.InputFingerprint
	if fingerprint == ([32]byte{}) && len(profile.Breakpoints) > 0 {
		fingerprint = profile.Breakpoints[len(profile.Breakpoints)-1].Fingerprint
	}
	hasher := sha256.New()
	hasher.Write(fingerprint[:])
	writeHashChunk(hasher, profile.Model)
	writeHashChunk(hasher, strconv.Itoa(profile.TotalInputTokens))
	digest := hasher.Sum(nil)
	return float64(binary.BigEndian.Uint16(digest[:2]))/65535*2 - 1
}

// Cache estimates are scaled into native upstream input token space at completion.
func cacheProfileWithInputTokens(profile *promptCacheProfile, inputTokens int) *promptCacheProfile {
	if profile != nil && profile.lifecycle {
		return profile
	}
	if profile != nil && profile.exactProfile != nil {
		return profile
	}
	if profile == nil || inputTokens <= 0 || profile.TotalInputTokens <= 0 {
		return profile
	}
	result := *profile
	result.TotalInputTokens = inputTokens
	result.Breakpoints = append([]promptCacheBreakpoint(nil), profile.Breakpoints...)
	result.LookupCandidates = append([]promptCacheBreakpoint(nil), profile.LookupCandidates...)
	for i := range result.Breakpoints {
		result.Breakpoints[i].CumulativeTokens = minInt(inputTokens, int(math.Round(float64(profile.Breakpoints[i].CumulativeTokens)*float64(inputTokens)/float64(profile.TotalInputTokens))))
	}
	for i := range result.LookupCandidates {
		result.LookupCandidates[i].CumulativeTokens = minInt(inputTokens, int(math.Round(float64(profile.LookupCandidates[i].CumulativeTokens)*float64(inputTokens)/float64(profile.TotalInputTokens))))
	}
	return &result
}

func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	if profile.referenceMode {
		minTokens = 1
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)

	entries := t.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[[32]byte]promptCacheEntry)
		t.entriesByAccount[accountID] = entries
	}

	for _, breakpoint := range profile.Breakpoints {

		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		ttl := min(normalizePromptCacheTTL(breakpoint.TTL), t.maxSupportedTTL)
		if ttl <= 0 {
			continue
		}
		entry := entries[breakpoint.Fingerprint]
		ttl = max(ttl, entry.TTL)
		expiresAt := now.Add(ttl)
		if entry.ExpiresAt.After(expiresAt) {
			expiresAt = entry.ExpiresAt
		}
		entries[breakpoint.Fingerprint] = promptCacheEntry{ExpiresAt: expiresAt, TTL: ttl}
	}
}

func (t *promptCacheTracker) pruneExpiredLocked(now time.Time) {
	for accountID, entries := range t.entriesByAccount {
		for fingerprint, entry := range entries {
			if !entry.ExpiresAt.After(now) {
				delete(entries, fingerprint)
			}
		}
		if len(entries) == 0 {
			delete(t.entriesByAccount, accountID)
		}
	}
}

type cacheablePromptBlock struct {
	Value        interface{}
	Tokens       int
	TTL          time.Duration
	IsMessageEnd bool
	IsMetadata   bool
}

func flattenClaudeCacheBlocks(req *ClaudeRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
			"type":         tool.Type,
			"max_uses":     tool.MaxUses,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		blocks = append(blocks, cacheablePromptBlock{
			Value:  fingerprintValue,
			Tokens: estimateClaudeToolTokens(tool),
			TTL:    normalizePromptCacheTTL(extractPromptCacheTTL(tool)),
		})
	}

	appendSystemCacheBlocks(&blocks, req.System)
	if req.structuredOutput != nil {
		appendSystemCacheBlocks(&blocks, []interface{}{map[string]interface{}{
			"type": "text", "text": req.structuredOutput.instruction,
		}})
	}

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg)
	}

	return blocks
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
		"thinking":    req.Thinking,
		"output":      req.OutputConfig,
	}
	return cacheablePromptBlock{
		Value:      prelude,
		Tokens:     0,
		IsMetadata: true,
	}
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, false)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, false)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, false)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage) {
	first := len(*blocks)
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true)
		}
	}
	// The role header belongs to the message prefix. The pending assistant
	// generation header is counted only in the total, outside every breakpoint.
	if len(*blocks) > first {
		(*blocks)[first].Tokens += claudeMessageFramingTokens
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isMessageEnd bool) {
	blockValue := wrapper["block"]
	ttl := normalizePromptCacheTTL(extractPromptCacheTTL(blockValue))

	//! Billing headers drift per request and must not affect prompt-cache fingerprints.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	fingerprintValue := stripCachePositionKeys(wrapper)
	*blocks = append(*blocks, cacheablePromptBlock{
		Value:        fingerprintValue,
		Tokens:       estimateClaudeValueTokens(blockValue),
		TTL:          ttl,
		IsMessageEnd: isMessageEnd,
	})
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}) time.Duration {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}

	if ttl, ok := parsePromptCacheTTLValue(cacheControl["ttl"]); ok {
		return ttl
	}
	return defaultPromptCacheTTL
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

func computePromptCacheTTLBreakdown(profile *promptCacheProfile, matchedTokens int) (int, int) {
	if profile == nil {
		return 0, 0
	}
	return computePromptCacheTTLBreakdownLimited(profile, matchedTokens, maxInt(profile.TotalInputTokens-matchedTokens, 0))
}
func computePromptCacheTTLBreakdownLimited(profile *promptCacheProfile, matchedTokens, creation int) (int, int) {
	if profile == nil || len(profile.Breakpoints) == 0 {
		return 0, 0
	}

	cache5m := 0
	cache1h := 0
	previous := matchedTokens
	for _, breakpoint := range profile.Breakpoints {
		current := minInt(breakpoint.CumulativeTokens, minInt(profile.TotalInputTokens, matchedTokens+creation))
		if current <= previous {
			continue
		}
		delta := current - previous
		if breakpoint.TTL >= time.Hour {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

func billedClaudeInputTokens(inputTokens int, usage promptCacheUsage) int {
	if usage.exactInput != nil {
		return *usage.exactInput
	}
	return maxInt(inputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens, 0)
}

func buildClaudeUsageMap(inputTokens, outputTokens int, usage promptCacheUsage, includeCache bool) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  billedClaudeInputTokens(inputTokens, usage),
		"output_tokens": outputTokens,
	}
	if !includeCache {
		return result
	}
	result["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	result["cache_read_input_tokens"] = usage.CacheReadInputTokens
	result["cache_creation"] = map[string]int{
		"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
		"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
	}
	return result
}

// Anthropic's message_delta usage is an output increment. Input and cache
// counters belong to message_start and the buffered message response.
func buildClaudeMessageDeltaUsage(outputTokens int) map[string]interface{} {
	return map[string]interface{}{"output_tokens": outputTokens}
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Completion scales the dispatch snapshot without querying cache entries again.
func scalePromptCacheUsage(usage promptCacheUsage, profile *promptCacheProfile, inputTokens int) promptCacheUsage {
	if usage.lifecycle {
		return usage
	}
	if usage.exactPlan != nil || (profile != nil && profile.exactProfile != nil) {
		return usage
	}
	if usage.referenceMode {
		return usage
	}
	if usage.totalCacheRate && inputTokens <= 0 && profile != nil {
		zero := *profile
		zero.TotalInputTokens = 0
		return allocateTotalCacheUsage(usage, &zero, 0, 0)
	}
	if profile == nil || inputTokens <= 0 || profile.TotalInputTokens <= 0 {
		return usage
	}
	scale := func(v int) int {
		return int(math.Round(float64(v) * float64(inputTokens) / float64(profile.TotalInputTokens)))
	}
	if usage.totalCacheRate {
		calibrated := cacheProfileWithInputTokens(profile, inputTokens)
		return allocateTotalCacheUsage(usage, calibrated, minInt(scale(usage.rawCacheable), inputTokens), minInt(scale(usage.rawRead), inputTokens))
	}
	if len(profile.Breakpoints) == 0 || scale(profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens) < minCacheableTokensForModel(profile.Model) {
		return promptCacheUsage{}
	}
	if usage.snapshot {
		if usage.prefixReadWrite {
			eligible := minInt(scale(usage.rawCacheable), inputTokens)
			return computePrefixReadWriteUsage(usage, cacheProfileWithInputTokens(profile, inputTokens), eligible, minInt(scale(usage.rawRead), eligible))
		}
		usage.CacheReadInputTokens = minInt(int(float64(scale(usage.rawRead))*usage.readRatio), inputTokens)
		usage.CacheCreationInputTokens = minInt(int(float64(scale(usage.rawCreation))*usage.creationRatio), inputTokens-usage.CacheReadInputTokens)
		usage.CacheCreation5mInputTokens = minInt(int(float64(scale(usage.rawCreation5m))*usage.creationRatio), usage.CacheCreationInputTokens)
		usage.CacheCreation1hInputTokens = usage.CacheCreationInputTokens - usage.CacheCreation5mInputTokens
		return usage
	}
	usage.CacheReadInputTokens = minInt(scale(usage.CacheReadInputTokens), inputTokens)
	usage.CacheCreationInputTokens = minInt(scale(usage.CacheCreationInputTokens), inputTokens-usage.CacheReadInputTokens)
	usage.CacheCreation5mInputTokens = minInt(scale(usage.CacheCreation5mInputTokens), usage.CacheCreationInputTokens)
	usage.CacheCreation1hInputTokens = usage.CacheCreationInputTokens - usage.CacheCreation5mInputTokens
	return usage
}
