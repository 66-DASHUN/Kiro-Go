package proxy

import (
	"bytes"
	"context"
	"io"
	"testing"

	refcompat "kiro-proxy/internal/sub2apiref/apicompat"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
)

func TestExactReferenceConvertedFinalizersPreservePositiveStart(t *testing.T) {
	for _, protocol := range []string{"chat_completions", "responses"} {
		raw := exactOutputFrames(t)
		raw = append(raw, awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": 0, "outputTokens": 3}})...)
		meter := &exactRequestMeter{Protocol: protocol, EstimatedInput: 47, Thinking: true}
		payload := &KiroPayload{exactMeter: meter}
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), payload, &KiroStreamCallback{})
		if err != nil {
			t.Fatal(err)
		}
		input, output := finalizePayloadUsage(payload, 0, 3, 999, 999)
		if input != 47 || output != 3 {
			t.Fatalf("%s finalize=%d/%d", protocol, input, output)
		}
		details := finishOpenAICacheUsage(nil, "", openAICacheSnapshot{meter: meter}, input)
		if details == nil || !details.exactReference || details.CachedTokens != 0 || details.CacheWriteTokens != 0 {
			t.Fatalf("%s details=%+v", protocol, details)
		}
		usage := exactResponsesUsage(input, output, details)
		if usage.InputTokens != 47 || usage.TotalTokens != 50 {
			t.Fatalf("%s public=%+v", protocol, usage)
		}
		bindExactCache(payload, promptCacheUsage{})
		if meter.Result != nil || meter.ConvertedUsage != nil {
			t.Fatal("retry reset retained stale usage")
		}
	}
}

func TestExactReferenceConvertedFinalizersKeepAnthropicRawUsage(t *testing.T) {
	for _, protocol := range []string{"anthropic", "responses"} {
		meter := &exactRequestMeter{Protocol: protocol, Result: &refkiro.ParseResult{Usage: refkiro.Usage{OutputTokens: 3}}, ConvertedUsage: &refcompat.ResponsesUsage{InputTokens: 47, OutputTokens: 3, TotalTokens: 50}}
		input, output := finalizePayloadUsage(&KiroPayload{exactMeter: meter}, 0, 0, 999, 999)
		want := 47
		if protocol == "anthropic" {
			want = 0
		}
		if input != want || output != 3 {
			t.Fatalf("%s finalize=%d/%d", protocol, input, output)
		}
	}
}
