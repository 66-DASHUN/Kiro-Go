// Package cache adapts nianzs/sub2api's Kiro cache emulator to standalone callers.
// The source revision and narrowly scoped adaptations are recorded in README.md.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"kiro-proxy/internal/sub2apiref/anthropictokenizer"
	"kiro-proxy/internal/sub2apiref/apicompat"
)

type Usage = kiroCacheEmulationUsage
type Tracker = kiroCacheTracker

// Profile keeps the builder's reference token space and the input argument used
// by the service's prepare wrapper. The Chat Completions wrapper alone replaces
// a nonpositive input argument with the profile total in the pinned source.
type Profile struct {
	*kiroCacheProfile
	inputTokens int
}

// Policy contains the effective values otherwise supplied by reference Group.
// Mode "uniform" scales every bucket by Ratio; other modes use the two ratios.
type Policy struct {
	Enabled       bool
	Mode          string
	Ratio         float64
	CreationRatio float64
	ReadRatio     float64
}

type Plan struct {
	usage    *Usage
	cacheKey string
	profile  *kiroCacheProfile
	tracker  *Tracker
}

func NewTracker() *Tracker {
	return &Tracker{entries: make(map[string]map[[32]byte]kiroCacheEntry)}
}

func (p *Plan) Usage() *Usage {
	if p == nil {
		return nil
	}
	return p.usage
}

// Commit has the reference semantics: compute can renew an existing matched
// entry; only Commit writes the current request's complete prefix chain.
func (p *Plan) Commit() {
	if p == nil || p.profile == nil || p.cacheKey == "" {
		return
	}
	p.tracker.update(p.cacheKey, p.profile)
}

func (t *kiroCacheTracker) Prepare(accountID string, profile *Profile, policy Policy) *Plan {
	if !policy.Enabled || t == nil || accountID == "" || profile == nil || profile.kiroCacheProfile == nil {
		return nil
	}
	result := t.compute(accountID, profile.kiroCacheProfile)
	if policy.Mode == "uniform" {
		result.CacheReadInputTokens = scaleKiroCacheTokens(result.CacheReadInputTokens, policy.Ratio)
		result.CacheCreationInputTokens = scaleKiroCacheTokens(result.CacheCreationInputTokens, policy.Ratio)
		result.CacheCreation5mInputTokens = scaleKiroCacheTokens(result.CacheCreation5mInputTokens, policy.Ratio)
		result.CacheCreation1hInputTokens = scaleKiroCacheTokens(result.CacheCreation1hInputTokens, policy.Ratio)
	} else {
		result.CacheReadInputTokens = scaleKiroCacheTokens(result.CacheReadInputTokens, policy.ReadRatio)
		result.CacheCreationInputTokens = scaleKiroCacheTokens(result.CacheCreationInputTokens, policy.CreationRatio)
		result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens = scaleKiroCacheCreationTTLTokens(
			result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens,
			result.CacheCreationInputTokens, policy.CreationRatio,
		)
	}
	result.InputTokens = profile.inputTokens - result.CacheReadInputTokens - result.CacheCreationInputTokens
	if result.InputTokens < 0 {
		result.InputTokens = 0
	}
	if result.CacheReadInputTokens == 0 && result.CacheCreationInputTokens == 0 {
		result = nil
	}
	return &Plan{usage: result, cacheKey: accountID, profile: profile.kiroCacheProfile, tracker: t}
}

// BuildProfile uses the reference protocol's raw request and exact official
// algorithm, including its original initialization failure fallback.
func BuildProfile(ctx context.Context, protocol string, body []byte, model string, totalTokens int) (*Profile, bool) {
	var profile *kiroCacheProfile
	var ok bool
	switch protocol {
	case "anthropic":
		profile, ok = buildKiroCacheProfile(ctx, anthropictokenizer.AlgorithmOfficial, body, model, totalTokens)
	case "chat_completions":
		profile, ok = buildKiroChatCompletionsCacheProfile(ctx, anthropictokenizer.AlgorithmOfficial, body, model, totalTokens)
	case "responses":
		var err error
		body, _, err = normalizeOpenAIResponsesLegacyIngress(body)
		if err != nil {
			return nil, false
		}
		profile, ok = buildKiroResponsesCacheProfile(ctx, anthropictokenizer.AlgorithmOfficial, body, model, totalTokens)
	default:
		return nil, false
	}
	if !ok {
		return nil, false
	}
	inputTokens := totalTokens
	if protocol == "chat_completions" && inputTokens <= 0 {
		inputTokens = profile.totalInputTokens
	}
	return &Profile{kiroCacheProfile: profile, inputTokens: inputTokens}, true
}

// InputTokens follows the reference runtime count path. OpenAI requests are
// normalized through the vendored compatibility converters before counting.
func InputTokens(ctx context.Context, protocol string, body []byte) (int, error) {
	return InputTokensForModel(ctx, protocol, body, "")
}

// InputTokensForModel supplies the account's mapped model at the same point as
// the reference gateway: after CC conversion, before Anthropic conversion.
func InputTokensForModel(ctx context.Context, protocol string, body []byte, mappedModel string) (int, error) {
	normalized, err := AnthropicBodyForModel(protocol, body, mappedModel)
	if err != nil {
		return 0, err
	}
	return estimateKiroInputTokens(ctx, anthropictokenizer.AlgorithmOfficial, normalized), nil
}

// AnthropicBodyForModel shares the runtime normalization with hosted-search
// dispatch, whose sole-tool predicate runs after compatibility conversion.
func AnthropicBodyForModel(protocol string, body []byte, mappedModel string) ([]byte, error) {
	if protocol == "anthropic" {
		return body, nil
	}
	var responses *apicompat.ResponsesRequest
	switch protocol {
	case "chat_completions":
		var chat apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &chat); err != nil {
			return nil, err
		}
		var err error
		responses, err = apicompat.ChatCompletionsToResponses(&chat)
		if err != nil {
			return nil, err
		}
	case "responses":
		var err error
		body, _, err = normalizeOpenAIResponsesLegacyIngress(body)
		if err != nil {
			return nil, err
		}
		adapted, _, err := adaptResponsesClientToolsForAnthropic(body)
		if err != nil {
			return nil, err
		}
		responses = &apicompat.ResponsesRequest{}
		if err := json.Unmarshal(adapted, responses); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported reference protocol %q", protocol)
	}
	if mappedModel != "" {
		responses.Model = mappedModel
	}
	anthropic, err := apicompat.ResponsesToAnthropicRequest(responses)
	if err != nil {
		return nil, err
	}
	anthropic.Stream = true
	normalized, err := json.Marshal(anthropic)
	if err != nil {
		return nil, err
	}
	normalized = enforceCacheControlLimit(normalized)
	return normalized, nil
}

// Copied unchanged from service/kiro_runtime.go.
func estimateKiroInputTokens(ctx context.Context, alg anthropictokenizer.Algorithm, body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		return countKiroInputTokensFromPayload(ctx, alg, payload)
	}
	tokens := len(body) / 4
	if tokens == 0 {
		return 1
	}
	return tokens
}

// Breakpoint exposes read-only diagnostics; Fingerprint is the reference SHA256.
type Breakpoint struct {
	BlockIndex       int
	CumulativeTokens int
	Tokens           int
	TTL              time.Duration
	Fingerprint      [32]byte
}

func (p *Profile) Breakpoints() []Breakpoint {
	if p == nil || p.kiroCacheProfile == nil {
		return nil
	}
	resolved := p.cacheableBreakpoints()
	out := make([]Breakpoint, 0, len(resolved))
	for _, b := range resolved {
		out = append(out, Breakpoint{b.blockIndex, b.cumulativeTokens,
			p.cacheTokensForBreakpoint(b.cumulativeTokens), b.ttl, p.blocks[b.blockIndex].prefixFingerprint})
	}
	return out
}

func (p *Profile) TotalInputTokens() int {
	if p == nil || p.kiroCacheProfile == nil {
		return 0
	}
	return p.totalInputTokens
}

func (p *Profile) MinimumCacheableTokens() int {
	if p == nil || p.kiroCacheProfile == nil {
		return 0
	}
	return p.minCacheable
}

func MinimumCacheableTokens(model string) int {
	return kiroMinimumCacheableTokens(model)
}
