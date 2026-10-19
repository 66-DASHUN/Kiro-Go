package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"

	refcompat "kiro-proxy/internal/sub2apiref/apicompat"
)

// exactSSEUsageWriter observes only the two bounded usage events produced by
// the pinned translator. Its writeEvent function writes one complete SSE event
// per Write call. Content events are discarded immediately without decoding or
// retaining any text/tool arguments. This contract is deliberately specific to
// the pinned in-process writer, not a general network SSE decoder.
type exactSSEUsageWriter struct {
	usage refcompat.AnthropicUsage
}

func (w *exactSSEUsageWriter) Write(p []byte) (int, error) {
	start := bytes.HasPrefix(p, []byte("event: message_start\n"))
	delta := bytes.HasPrefix(p, []byte("event: message_delta\n"))
	if !start && !delta {
		return len(p), nil
	}
	i := bytes.Index(p, []byte("\ndata: "))
	if i < 0 {
		return 0, fmt.Errorf("reference usage event missing data")
	}
	var event refcompat.AnthropicStreamEvent
	if err := json.Unmarshal(bytes.TrimSpace(p[i+7:]), &event); err != nil {
		return 0, fmt.Errorf("reference usage event: %w", err)
	}
	if start && event.Message != nil {
		w.merge(event.Message.Usage)
	}
	if delta && event.Usage != nil {
		w.merge(*event.Usage)
	}
	return len(p), nil
}

// This is mergeAnthropicUsage's ordinary Anthropic branch from the pinned
// gateway_forward_as_responses.go. Kiro's SSE encoder emits only these canonical
// fields, never the alternate provider prompt/cache aliases. Both the buffered
// gateway collector and its stream normalizer preserve earlier positive buckets
// when a later event supplies zero or negative values.
func (w *exactSSEUsageWriter) merge(src refcompat.AnthropicUsage) {
	if src.InputTokens > 0 {
		w.usage.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		w.usage.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens > 0 {
		w.usage.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		w.usage.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
}

func (w *exactSSEUsageWriter) convertedUsage() *refcompat.ResponsesUsage {
	return refcompat.AnthropicToResponsesResponse(&refcompat.AnthropicResponse{Usage: w.usage}).Usage
}
