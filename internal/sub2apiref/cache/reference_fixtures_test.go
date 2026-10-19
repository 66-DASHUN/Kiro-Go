// Fixtures and counter tests copied unchanged from reference kiro_cache_emulation_test.go.
package cache

import (
	"context"
	"fmt"
	"kiro-proxy/internal/sub2apiref/anthropictokenizer"
	"strings"
	"testing"
)

func kiroCacheRequestBody(label string, oneHour bool) []byte {
	ttl := ""
	if oneHour {
		ttl = `,"ttl":"1h"`
	}
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"%s}}]}]}`, strings.Repeat("cacheable prompt chunk "+label+" ", 512), ttl))
}

func kiroCacheMultiMessageBody(prefixLabel, tailLabel string) []byte {
	prefix := strings.Repeat("cacheable prompt chunk "+prefixLabel+" ", 512)
	tail := strings.Repeat("conversation growth chunk "+tailLabel+" ", 160)
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":%q}]}]}`, prefix, tail))
}

func kiroChatCompletionsConversationBody(messages []string) []byte {
	items := make([]string, 0, len(messages)+1)
	items = append(items, `{"role":"system","content":"You are a precise assistant."}`)
	for _, message := range messages {
		items = append(items, fmt.Sprintf(`{"role":"user","content":%q}`, message))
	}
	return []byte(fmt.Sprintf(`{"model":"gpt-5","tool_choice":"auto","tools":[{"type":"function","function":{"name":"lookup","description":"lookup data","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}],"messages":[%s]}`, strings.Join(items, ",")))
}

func kiroResponsesConversationRequestBody(promptCacheKey string, messages []string) []byte {
	items := make([]string, 0, len(messages))
	for _, message := range messages {
		items = append(items, fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, message))
	}
	return []byte(fmt.Sprintf(`{"model":"gpt-5","instructions":"You are a precise assistant.","prompt_cache_key":%q,"tool_choice":"auto","tools":[{"type":"function","name":"lookup","description":"lookup data","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],"input":[%s]}`, promptCacheKey, strings.Join(items, ",")))
}

func TestKiroInputTokenEstimateIgnoresClientMetadata(t *testing.T) {
	bodyWithoutMetadata := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello world"}]}`)
	bodyWithMetadata := []byte(`{"model":"claude-sonnet-4-6","metadata":{"input_tokens":999999},"messages":[{"role":"user","content":"hello world"}]}`)
	withoutMetadata := estimateKiroInputTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, bodyWithoutMetadata)
	withMetadata := estimateKiroInputTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, bodyWithMetadata)
	if withMetadata == 999999 {
		t.Fatal("client metadata.input_tokens must not be trusted")
	}
	if withMetadata <= 0 || withoutMetadata <= 0 || withMetadata > withoutMetadata*2 {
		t.Fatalf("unexpected estimates without=%d with=%d", withoutMetadata, withMetadata)
	}
}

func TestKiroTokenCountersMatchReferenceRules(t *testing.T) {
	if got := anthropictokenizer.CountTokens("abc def"); got != 1 {
		t.Fatalf("english tokens = %d, want 1", got)
	}
	if got := anthropictokenizer.CountTokens("你好世界"); got != 1 {
		t.Fatalf("cjk tokens = %d, want 1", got)
	}
	if kiroTokensPerTool != 150 {
		t.Fatalf("tool tokens = %d, want 150", kiroTokensPerTool)
	}
	if got := countKiroMessageContentTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, map[string]any{"thinking": "abc def"}); got != 1 {
		t.Fatalf("thinking tokens = %d, want 1", got)
	}
	if got := countKiroMessageContentTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, map[string]any{"input": map[string]any{"path": "/tmp/a.txt"}}); got <= 0 {
		t.Fatalf("tool input tokens should be positive, got %d", got)
	}
	if got := countKiroMessageContentTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, map[string]any{"content": []any{map[string]any{"text": "abc"}, map[string]any{"text": "你好"}}}); got != 2 {
		t.Fatalf("tool result content tokens = %d, want 2", got)
	}
}
