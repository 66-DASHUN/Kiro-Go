package proxy

import "kiro-proxy/config"

// OpenAITokenDetails contains the cache components OpenAI-compatible clients
// understand. CachedTokens and CacheWriteTokens are subsets of the top-level
// input/prompt token count.
type OpenAITokenDetails struct {
	exactReference   bool
	CachedTokens     int  `json:"cached_tokens"`
	CacheWriteTokens int  `json:"cache_write_tokens"`
	Estimated        bool `json:"estimated"`
}

func resolveOpenAICacheUsage(tracker *promptCacheTracker, accountID string, payload *KiroPayload, inputTokens int) *OpenAITokenDetails {
	return resolveOpenAICacheUsageWithSettings(tracker, accountID, payload, inputTokens, config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1})
}

func resolveOpenAICacheUsageWithSettings(tracker *promptCacheTracker, accountID string, payload *KiroPayload, inputTokens int, settings config.PromptCacheSettings) *OpenAITokenDetails {
	if tracker == nil || !settings.Enabled {
		return nil
	}
	profile := tracker.BuildOpenAIProfileWithSettings(payload, inputTokens, settings)
	if profile == nil {
		return nil
	}
	estimated := tracker.ComputeWithSettings(accountID, profile, settings)
	// Store only after a successful upstream response.
	tracker.UpdateWithSettings(accountID, profile, settings, estimated)
	if estimated.CacheReadInputTokens == 0 && estimated.CacheCreationInputTokens == 0 {
		return nil
	}
	return &OpenAITokenDetails{
		CachedTokens:     estimated.CacheReadInputTokens,
		CacheWriteTokens: estimated.CacheCreationInputTokens,
		Estimated:        true,
	}
}

func addResponsesCacheUsage(response map[string]interface{}, details *OpenAITokenDetails) {
	if details != nil && details.exactReference {
		if values, ok := response["usage"].(map[string]int); ok {
			response["usage"] = exactResponsesUsage(values["input_tokens"], values["output_tokens"], details)
		}
		return
	}

	if details == nil {
		return
	}
	values, ok := response["usage"].(map[string]int)
	if !ok {
		return
	}
	usage := make(map[string]interface{}, len(values)+1)
	for key, value := range values {
		usage[key] = value
	}
	usage["input_tokens_details"] = details
	response["usage"] = usage
}

func buildOpenAIUsage(inputTokens, outputTokens int, details *OpenAITokenDetails) map[string]interface{} {
	if details != nil && details.exactReference {
		return exactChatUsage(inputTokens, outputTokens, details)
	}

	usage := map[string]interface{}{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
	if details != nil {
		usage["prompt_tokens_details"] = details
	}
	return usage
}

// Dispatch and completion are split so concurrent cold requests cannot turn
// into cache hits merely because another identical request completed first.
type openAICacheSnapshot struct {
	meter    *exactRequestMeter
	profile  *promptCacheProfile
	usage    promptCacheUsage
	settings config.PromptCacheSettings
}

func beginOpenAICacheUsage(tracker *promptCacheTracker, accountID string, payload *KiroPayload, inputTokens int, settings config.PromptCacheSettings) openAICacheSnapshot {
	if tracker == nil || !settings.Enabled {
		bindExactCache(payload, promptCacheUsage{})
		return openAICacheSnapshot{meter: payload.exactMeter}
	}
	profile := tracker.BuildOpenAIProfileWithSettings(payload, inputTokens, settings)
	usage := tracker.ComputeWithSettings(accountID, profile, settings)
	bindExactCache(payload, usage)
	return openAICacheSnapshot{meter: payload.exactMeter, profile: profile, usage: usage, settings: settings}
}

func finishOpenAICacheUsage(tracker *promptCacheTracker, accountID string, snapshot openAICacheSnapshot, inputTokens int) *OpenAITokenDetails {
	if snapshot.meter != nil && snapshot.meter.ConvertedUsage != nil {
		v := snapshot.meter.ConvertedUsage
		d := &OpenAITokenDetails{CacheWriteTokens: v.CacheCreationInputTokens, exactReference: true}
		if v.InputTokensDetails != nil {
			d.CachedTokens = v.InputTokensDetails.CachedTokens
		}
		return d
	}
	if snapshot.meter != nil && snapshot.meter.Result != nil {
		v := snapshot.meter.Result.Usage
		return &OpenAITokenDetails{CachedTokens: v.CacheReadInputTokens, CacheWriteTokens: v.CacheCreationInputTokens, exactReference: true}
	}

	if tracker == nil || snapshot.profile == nil || !snapshot.settings.Enabled {
		return nil
	}
	usage := scalePromptCacheUsage(snapshot.usage, snapshot.profile, inputTokens)
	tracker.UpdateWithSettings(accountID, cacheProfileWithInputTokens(snapshot.profile, inputTokens), snapshot.settings, usage)
	if usage.CacheCreationInputTokens == 0 && usage.CacheReadInputTokens == 0 {
		return nil
	}
	return &OpenAITokenDetails{CachedTokens: usage.CacheReadInputTokens, CacheWriteTokens: usage.CacheCreationInputTokens, Estimated: true}
}
