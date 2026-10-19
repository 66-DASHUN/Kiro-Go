package proxy

import (
	"context"
	"encoding/json"
	"sync"

	"kiro-proxy/config"
	refcompat "kiro-proxy/internal/sub2apiref/apicompat"
	refcache "kiro-proxy/internal/sub2apiref/cache"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
)

// These are transport adapters only. All counting, prefix admission, matching,
// TTL classification and rounding live in the pinned reference namespace.
func buildExactCacheProfile(protocol string, body []byte, model string, total int) *promptCacheProfile {
	p, ok := refcache.BuildProfile(context.Background(), protocol, body, model, total)
	if !ok {
		return nil
	}
	out := &promptCacheProfile{exactProfile: p, referenceMode: true, Model: model, TotalInputTokens: total}
	for _, b := range p.Breakpoints() {
		out.Breakpoints = append(out.Breakpoints, promptCacheBreakpoint{Fingerprint: b.Fingerprint, CumulativeTokens: b.Tokens, TTL: b.TTL, Position: b.BlockIndex})
	}
	return out
}

func (t *promptCacheTracker) computeExactCacheUsage(account string, profile *promptCacheProfile, settings config.PromptCacheSettings) promptCacheUsage {
	if t == nil || profile == nil || profile.exactProfile == nil {
		return promptCacheUsage{}
	}
	plan := t.exactTracker.Prepare(account, profile.exactProfile, refcache.Policy{Enabled: settings.Enabled, Mode: "uniform", Ratio: settings.Ratio})
	u := promptCacheUsage{referenceMode: true, exactPlan: plan, snapshot: true, totalRatio: settings.Ratio}
	if v := plan.Usage(); v != nil {
		u.CacheCreationInputTokens = v.CacheCreationInputTokens
		u.CacheReadInputTokens = v.CacheReadInputTokens
		u.CacheCreation5mInputTokens = v.CacheCreation5mInputTokens
		u.CacheCreation1hInputTokens = v.CacheCreation1hInputTokens
		u.finalCacheable = profile.TotalInputTokens
		u.finalMatched = v.CacheReadInputTokens
	}
	return u
}

func newExactRequestMeter(ctx context.Context, settings config.PromptCacheSettings, protocol string, body []byte, model string, stream, thinking bool, maxOutput int) (*exactRequestMeter, error) {
	if settings.Mode != config.PromptCacheModeSub2API {
		return nil, nil
	}
	n, err := refcache.InputTokensForModel(ctx, protocol, body, model)
	if err != nil {
		return nil, err
	}
	var options struct {
		StopSequences []string `json:"stop_sequences"`
	}
	_ = json.Unmarshal(body, &options)
	return &exactRequestMeter{Protocol: protocol, Body: body, Model: model, Stream: stream, Thinking: thinking, EstimatedInput: n, MaxOutput: maxOutput, StopSequences: options.StopSequences}, nil
}

func bindExactCache(payload *KiroPayload, usage promptCacheUsage) {
	if payload == nil || payload.exactMeter == nil {
		return
	}
	m := payload.exactMeter
	m.Result = nil
	m.ConvertedUsage = nil
	m.CacheUsage = nil
	m.Commit = nil
	if usage.exactPlan == nil {
		return
	}
	var once sync.Once
	m.Commit = func() { once.Do(usage.exactPlan.Commit) }
	if u := usage.exactPlan.Usage(); u != nil {
		m.CacheUsage = &refkiro.Usage{InputTokens: u.InputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, CacheCreation5mInputTokens: u.CacheCreation5mInputTokens, CacheCreation1hInputTokens: u.CacheCreation1hInputTokens}
	}
}

// Return total input to existing accounting call sites, but keep the ordinary
// input explicitly: reference JSON can re-add the estimate after a full hit.
func exactClaudeUsage(payload *KiroPayload, old promptCacheUsage) (int, int, promptCacheUsage, bool) {
	if payload == nil || payload.exactMeter == nil || payload.exactMeter.Result == nil {
		return 0, 0, old, false
	}
	v := payload.exactMeter.Result.Usage
	old.CacheCreationInputTokens = v.CacheCreationInputTokens
	old.CacheReadInputTokens = v.CacheReadInputTokens
	old.CacheCreation5mInputTokens = v.CacheCreation5mInputTokens
	old.CacheCreation1hInputTokens = v.CacheCreation1hInputTokens
	input := v.InputTokens
	old.exactInput = &input
	return input + v.CacheCreationInputTokens + v.CacheReadInputTokens, v.OutputTokens, old, true
}

func finalizePayloadUsage(payload *KiroPayload, input, output, estimatedInput, estimatedOutput int) (int, int) {
	if payload != nil && payload.exactMeter != nil {
		// Hosted loops retain the final round's exact converted counts themselves.
		if len(payload.HostedSearchTools) > 0 {
			return input, output
		}
		if payload.exactMeter.Protocol != "anthropic" && payload.exactMeter.ConvertedUsage != nil {
			v := payload.exactMeter.ConvertedUsage
			return v.InputTokens, v.OutputTokens
		}
		if v := payload.exactMeter.Result; v != nil {
			return v.Usage.InputTokens + v.Usage.CacheCreationInputTokens + v.Usage.CacheReadInputTokens, v.Usage.OutputTokens
		}
	}
	return finalizeUsageTokens(input, output, estimatedInput, estimatedOutput)
}

func exactClaudeDeltaUsage(payload *KiroPayload, input, output int, usage promptCacheUsage) map[string]interface{} {
	if payload != nil && payload.exactMeter != nil {
		return exactClaudeUsageMap(billedClaudeInputTokens(input, usage), output, usage)
	}
	return buildClaudeMessageDeltaUsage(output)
}

func applyExactClaudePresentation(payload *KiroPayload, response *ClaudeResponse) {
	if payload == nil || payload.exactMeter == nil || payload.exactMeter.Result == nil {
		return
	}
	response.Usage.exactReference = true
	if payload.exactMeter.Stream {
		return
	}
	var ref ClaudeResponse
	if json.Unmarshal(payload.exactMeter.Result.ResponseBody, &ref) != nil {
		return
	}
	response.Usage = ref.Usage
	response.Usage.exactReference = true
	// Reference caps only ordinary text on buffered responses. Keep existing
	// signature/structured-output delivery, applying the reference cap when used.
	if ref.StopReason == "max_tokens" || ref.StopReason == "stop_sequence" {
		response.Content = ref.Content
		response.StopReason = ref.StopReason
		response.StopSequence = ref.StopSequence
	}
}

func exactClaudeUsageMap(input, output int, u promptCacheUsage) map[string]interface{} {
	v := map[string]interface{}{"input_tokens": input, "output_tokens": output}
	if u.CacheReadInputTokens > 0 {
		v["cache_read_input_tokens"] = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		v["cache_creation_input_tokens"] = u.CacheCreationInputTokens
	}
	if u.CacheCreation5mInputTokens > 0 || u.CacheCreation1hInputTokens > 0 {
		v["cache_creation"] = map[string]int{"ephemeral_5m_input_tokens": u.CacheCreation5mInputTokens, "ephemeral_1h_input_tokens": u.CacheCreation1hInputTokens}
	}
	return v
}

func exactOrLegacyStartUsage(payload *KiroPayload, input int, u promptCacheUsage, enabled bool) map[string]interface{} {
	if payload != nil && payload.exactMeter != nil {
		return exactClaudeUsageMap(billedClaudeInputTokens(input, u), 0, u)
	}
	return buildClaudeUsageMap(input, 0, u, enabled)
}

func exactResponsesUsage(input, output int, d *OpenAITokenDetails) *refcompat.ResponsesUsage {
	a := &refcompat.AnthropicResponse{Usage: refcompat.AnthropicUsage{InputTokens: input - d.CachedTokens - d.CacheWriteTokens, OutputTokens: output, CacheReadInputTokens: d.CachedTokens, CacheCreationInputTokens: d.CacheWriteTokens}}
	return refcompat.AnthropicToResponsesResponse(a).Usage
}
func exactChatUsage(input, output int, d *OpenAITokenDetails) map[string]interface{} {
	response := &refcompat.ResponsesResponse{Usage: exactResponsesUsage(input, output, d)}
	usage := refcompat.ResponsesToChatCompletions(response, "").Usage
	data, _ := json.Marshal(usage)
	var result map[string]interface{}
	_ = json.Unmarshal(data, &result)
	return result
}
