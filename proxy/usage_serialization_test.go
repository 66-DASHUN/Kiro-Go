package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func assertUsageCacheFields(t *testing.T, usage map[string]interface{}, enabled bool) {
	t.Helper()
	for _, name := range []string{"input_tokens", "output_tokens"} {
		if _, ok := usage[name].(float64); !ok {
			t.Errorf("missing numeric %s: %v", name, usage)
		}
	}
	for _, name := range []string{"cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation"} {
		if _, ok := usage[name]; ok != enabled {
			t.Errorf("cache enabled=%t field %s present=%t: %v", enabled, name, ok, usage)
		}
	}
	if !enabled {
		return
	}
	for _, name := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
		if value, ok := usage[name].(float64); !ok || value != 0 {
			t.Errorf("expected integer zero for %s: %v", name, usage)
		}
	}
	creation, ok := usage["cache_creation"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing cache_creation object: %v", usage)
	}
	for _, name := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
		if value, ok := creation[name].(float64); !ok || value != 0 {
			t.Errorf("expected integer zero for %s: %v", name, creation)
		}
	}
}

func TestUsageSerializationCacheZeroParity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, structured := range []bool{false, true} {
				t.Run(fmt.Sprintf("enabled=%t/stream=%t/structured=%t", enabled, stream, structured), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeKiroTextResponse(t, w, `{"ok":true}`) })
					if err := config.UpdatePromptCacheSettings(enabled, "uniform", 0, 0, 0); err != nil {
						t.Fatal(err)
					}
					body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 32, "stream": stream, "messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}}}
					if structured {
						body["output_config"] = map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "schema": map[string]interface{}{"type": "object"}}}
					}
					rec := localUsageJSONRequest(t, h, "/v1/messages", body, nil)
					assertUsageCacheFields(t, runtimeProtocolUsage(t, rec, stream), enabled)
					if stream {
						for _, event := range parseResponsesSSE(t, rec.Body.String()) {
							if event.Name == "message_start" {
								assertUsageCacheFields(t, event.Data["message"].(map[string]interface{})["usage"].(map[string]interface{}), enabled)
							}
						}
					}
				})
			}
		}
	}
}

func TestUsageSerializationKeepsSearchCounters(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			rec := httptest.NewRecorder()
			h := &Handler{}
			h.writeClaudeWebSearchResponse(rec, stream, "claude-sonnet-4.5", []map[string]interface{}{}, "end_turn", 12, 3, 2, promptCacheUsage{}, enabled)
			usage := runtimeProtocolUsage(t, rec, stream)
			assertUsageCacheFields(t, usage, enabled)
			if usage["server_tool_use"].(map[string]interface{})["web_search_requests"] != float64(2) {
				t.Fatalf("search counter missing: %v", usage)
			}
		}
	}
}

func TestUsageSerializationPreservesNonzeroCounts(t *testing.T) {
	usage := ClaudeUsage{InputTokens: 10, OutputTokens: 7, CacheReadInputTokens: 20, CacheCreationInputTokens: 30, CacheCreation: &ClaudeCacheCreationUsage{Ephemeral5mInputTokens: 11, Ephemeral1hInputTokens: 19}}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["input_tokens"] != float64(10) || decoded["cache_read_input_tokens"] != float64(20) || decoded["cache_creation_input_tokens"] != float64(30) {
		t.Fatalf("cache subtraction applied twice: %v", decoded)
	}
	children := decoded["cache_creation"].(map[string]interface{})
	if children["ephemeral_5m_input_tokens"] != float64(11) || children["ephemeral_1h_input_tokens"] != float64(19) {
		t.Fatalf("TTL split changed: %v", decoded)
	}
}
