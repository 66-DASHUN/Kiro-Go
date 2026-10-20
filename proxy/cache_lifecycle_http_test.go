package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLifecycleHTTPToolSchemaReadMatchesCountTokens(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeKiroTextResponse(t, w, "ok") })
	mode, enabled, ratio := "prefix_lifecycle", true, 1.0
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "serialized-tools", Key: "sk-lifecycle-serialized-tools", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	if err := config.UpdatePromptCacheSettings(true, mode, ratio, 1, 1); err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{
		"model": "claude-opus-5.5", "max_tokens": 256,
		"tools": []interface{}{map[string]interface{}{
			"name": "read_file", "description": "Read a file from the workspace and return its contents.",
			"input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string", "description": "Workspace-relative path"}}, "required": []interface{}{"path"}},
		}},
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Use the tool."}},
	}
	count := localUsageJSONRequest(t, h, "/v1/messages/count_tokens", body, &key)
	var estimated struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(count.Body.Bytes(), &estimated); err != nil {
		t.Fatal(err)
	}
	first := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/messages", body, &key), false)
	second := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/messages", body, &key), false)
	written := first["cache_creation_input_tokens"].(float64)
	if first["input_tokens"] != float64(2) || first["cache_read_input_tokens"] != float64(0) || written <= 0 {
		t.Fatalf("cold tool usage: %v", first)
	}
	if second["input_tokens"] != float64(2) || second["cache_creation_input_tokens"] != float64(0) || second["cache_read_input_tokens"] != written || float64(estimated.InputTokens) != written+2 {
		t.Fatalf("warm tool usage=%v, cold=%v, count=%d", second, first, estimated.InputTokens)
	}
}

func TestLifecycleHTTPProtocols(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 99999, "outputTokens": 137}))
				})
				mode, enabled, ratio := "prefix_lifecycle", true, 1.0
				if err := config.UpdateSettingsPatch(&enabled, ""); err != nil {
					t.Fatal(err)
				}
				key, err := config.AddApiKey(config.ApiKeyEntry{ID: "lifecycle", Key: "sk-lifecycle-http", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
				if err != nil {
					t.Fatal(err)
				}
				h.pool.Reload()
				msgs := []map[string]interface{}{{"role": "user", "content": strings.Repeat("unchanged prefix ", 80)}}
				previous := float64(0)
				for round := 0; round < 5; round++ {
					model := "claude-sonnet-4.5"
					if round > 0 {
						model = "claude-sonnet-4-5"
					}
					if round == 2 {
						msgs = append(msgs, map[string]interface{}{"role": "assistant", "content": "ok"}, map[string]interface{}{"role": "user", "content": "a new question"})
					}
					if round == 3 {
						ratio = 0
						if err := config.UpdateApiKey(key.ID, config.ApiKeyEntry{Enabled: true, PromptCacheEmulationRatio: &ratio}); err != nil {
							t.Fatal(err)
						}
					}
					if round == 4 {
						enabled = false
						if err := config.UpdateApiKey(key.ID, config.ApiKeyEntry{Enabled: true, PromptCacheEmulationEnabled: &enabled}); err != nil {
							t.Fatal(err)
						}
					}
					body := map[string]interface{}{"model": model, "max_tokens": 256, "stream": stream}
					if path == "/v1/responses" {
						body["input"], body["store"] = msgs, false
					} else {
						body["messages"] = msgs
					}
					raw, _ := json.Marshal(body)
					r := withApiKeyContext(httptest.NewRequest("POST", path, strings.NewReader(string(raw))), &key)
					rec := httptest.NewRecorder()
					switch path {
					case "/v1/messages":
						h.handleClaudeMessages(rec, r)
					case "/v1/chat/completions":
						h.handleOpenAIChat(rec, r)
					default:
						h.handleOpenAIResponses(rec, r)
					}
					if rec.Code != 200 {
						t.Fatalf("round %d: %s", round, rec.Body.String())
					}
					u := runtimeProtocolUsage(t, rec, stream)
					var read, write, total, output float64
					if path == "/v1/messages" {
						read, _ = u["cache_read_input_tokens"].(float64)
						write, _ = u["cache_creation_input_tokens"].(float64)
						total = u["input_tokens"].(float64) + read + write
						output = u["output_tokens"].(float64)
						countBody := map[string]interface{}{"model": model, "messages": msgs}
						countRec := localUsageJSONRequest(t, h, "/v1/messages/count_tokens", countBody, &key)
						var count map[string]float64
						if err := json.Unmarshal(countRec.Body.Bytes(), &count); err != nil {
							t.Fatal(err)
						}
						if count["input_tokens"] != total {
							t.Fatalf("count_tokens=%v usage=%v", count, u)
						}
					} else {
						field, in, out := "prompt_tokens_details", "prompt_tokens", "completion_tokens"
						if path == "/v1/responses" {
							field, in, out = "input_tokens_details", "input_tokens", "output_tokens"
						}
						if d, ok := u[field].(map[string]interface{}); ok {
							read, _ = d["cached_tokens"].(float64)
							write, _ = d["cache_write_tokens"].(float64)
						}
						total, output = u[in].(float64), u[out].(float64)
					}
					if total >= 99999 || total <= 0 || output != 137 || read+write > total {
						t.Fatalf("invalid counters: %v", u)
					}
					if round < 4 && total-read-write != claudeGenerationPrefixTokens {
						t.Fatalf("unexpected ordinary tail: %v", u)
					}
					if (round == 0 && (read != 0 || write <= 0)) || (round == 1 && (read != previous || write != 0)) || (round == 2 && (read != previous || write <= 0)) || (round == 3 && (read != 0 || write <= 0)) || (round == 4 && read+write != 0) {
						t.Fatalf("round %d previous=%v usage=%v", round, previous, u)
					}
					previous = read + write
					t.Logf("round %d total=%.0f ordinary=%.0f read=%.0f create=%.0f output=%.0f", round, total, total-read-write, read, write, output)
				}
			})
		}
	}
}

func TestLifecycleHTTPReadHitSettings(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 99999, "outputTokens": 137}))
				})
				body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 256, "stream": stream, "store": false}
				messages := []map[string]interface{}{{"role": "user", "content": "short prefix for read-hit testing"}}
				if path == "/v1/responses" {
					body["input"] = messages
				} else {
					body["messages"] = messages
				}
				for _, ratio := range []float64{0, 1, .9} {
					if err := config.UpdatePromptCacheSettings(true, "prefix_lifecycle", ratio, 1, 1); err != nil {
						t.Fatal(err)
					}
					u := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), stream)
					total, output, read, write := localUsageTotals(path, u)
					if total <= 0 || total >= 99999 || output != 137 {
						t.Fatalf("metering changed: %v", u)
					}
					switch ratio {
					case 0:
						if read != 0 || write != total-claudeGenerationPrefixTokens {
							t.Fatalf("cold prefix was not created at 0%% read target for %s: %v", path, u)
						}
					case 1:
						if read != total-claudeGenerationPrefixTokens || write != 0 {
							t.Fatalf("100%% warm read target: %v", u)
						}
					case .9:
						if read+write != total-claudeGenerationPrefixTokens || (read == 0 && write == 0) || (read > 0 && write > 0) {
							t.Fatalf("90%% read target changed prefix accounting: %v", u)
						}
					}
				}
			})
		}
	}
}

func TestLifecycleHTTPFailureDoesNotWarm(t *testing.T) {
	fail := true
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"message":"fixture rejected"}`))
			return
		}
		writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	})
	mode := "prefix_lifecycle"
	enabled := true
	if err := config.UpdateSettingsPatch(&enabled, ""); err != nil {
		t.Fatal(err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "lifecycle", Key: "sk-lifecycle-failure", Enabled: true, PromptCacheEmulationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 100, "messages": []map[string]interface{}{{"role": "user", "content": "hello"}}}
	raw, _ := json.Marshal(body)
	failed := httptest.NewRecorder()
	h.handleClaudeMessages(failed, withApiKeyContext(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))), &key))
	if failed.Code == 200 {
		t.Fatal("fixture must reject first request")
	}
	fail = false
	got := localUsageJSONRequest(t, h, "/v1/messages", body, &key)
	u := runtimeProtocolUsage(t, got, false)
	if u["cache_read_input_tokens"] != float64(0) || u["cache_creation_input_tokens"].(float64) <= 0 {
		t.Fatalf("failed request warmed cache: %v", u)
	}
}

func TestLifecycleHTTPExplicitBoundary(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) { writeKiroTextResponse(t, w, "ok") })
			if err := config.UpdatePromptCacheSettings(true, "prefix_lifecycle", 1, 1, 1); err != nil {
				t.Fatal(err)
			}
			messages := []interface{}{map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "cached prefix", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}},
				map[string]interface{}{"type": "text", "text": strings.Repeat("ordinary tail ", 20)},
			}}}
			body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 128}
			if path == "/v1/responses" {
				body["input"], body["store"] = messages, false
			} else {
				body["messages"] = messages
			}
			cold := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), false)
			total, _, read, write := localUsageTotals(path, cold)
			if read != 0 || write <= 0 || total-read-write <= claudeGenerationPrefixTokens {
				t.Fatalf("explicit boundary lost: %v", cold)
			}
			warm := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), false)
			_, _, read, created := localUsageTotals(path, warm)
			if read != write || created != 0 {
				t.Fatalf("explicit repeat: cold=%v warm=%v", cold, warm)
			}
		})
	}
}

func TestLifecycleHostedFailureDoesNotWarm(t *testing.T) {
	var calls atomic.Int32
	var reject atomic.Bool
	reject.Store(true)
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1)%2 == 1 {
			writeFrames(w, webSearchToolUseFrame(t, "search", "go release"), meteringFrame(t))
			return
		}
		if reject.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"second round rejected"}`))
			return
		}
		writeKiroTextResponse(t, w, "Go release found.")
	})
	if err := config.UpdatePromptCacheSettings(true, "prefix_lifecycle", 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
	body := `{"model":"claude-sonnet-4.5","max_tokens":1024,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"What is the latest Go release?"}]}`
	failed := postClaudeMessages(t, h, body)
	if failed.Code == http.StatusOK || calls.Load() != 2 {
		t.Fatalf("expected later-round rejection: calls=%d status=%d body=%s", calls.Load(), failed.Code, failed.Body.String())
	}
	h.promptCache.mu.Lock()
	entries := len(h.promptCache.entriesByAccount)
	h.promptCache.mu.Unlock()
	if entries != 0 {
		t.Fatalf("failed hosted request left %d cache scopes", entries)
	}
	reject.Store(false)
	got := postClaudeMessages(t, h, body)
	if got.Code != http.StatusOK {
		t.Fatalf("hosted success: %d %s", got.Code, got.Body.String())
	}
	u := runtimeProtocolUsage(t, got, false)
	if u["cache_read_input_tokens"] != float64(0) || u["cache_creation_input_tokens"].(float64) <= 0 {
		t.Fatalf("failed hosted request warmed next attempt: %v", u)
	}
	repeat := postClaudeMessages(t, h, body)
	if repeat.Code != http.StatusOK {
		t.Fatalf("hosted repeat: %s", repeat.Body.String())
	}
	u = runtimeProtocolUsage(t, repeat, false)
	if u["cache_read_input_tokens"].(float64) <= 0 || u["cache_creation_input_tokens"] != float64(0) {
		t.Fatalf("successful hosted request did not persist its prefixes: %v", u)
	}
}

func TestLifecycleStructuredFailureDoesNotWarm(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/hosted=%v", stream, hosted), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) { writeKiroTextResponse(t, w, "not json") })
				if err := config.UpdatePromptCacheSettings(true, "prefix_lifecycle", 1, 1, 1); err != nil {
					t.Fatal(err)
				}
				tools := ""
				if hosted {
					tools = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
				}
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":128,"stream":%v,%s%s,"messages":[{"role":"user","content":"answer"}]}`, stream, structuredFixture, tools)
				got := postClaudeMessages(t, h, body)
				if got.Code != http.StatusBadGateway {
					t.Fatalf("invalid structure accepted: %d %s", got.Code, got.Body.String())
				}
				h.promptCache.mu.Lock()
				entries := len(h.promptCache.entriesByAccount)
				h.promptCache.mu.Unlock()
				if entries != 0 {
					t.Fatalf("failed validation warmed %d scopes", entries)
				}
			})
		}
	}
}

func TestLifecycleOpenAIHostedRounds(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				calls := 0
				fail := true
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls%2 == 1 {
						writeFrames(w, webSearchToolUseFrame(t, "search", "go release"), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 99999, "outputTokens": 7, "stopReason": "tool_use"}))
					} else if fail {
						http.Error(w, "fixture rejected", http.StatusBadRequest)
					} else {
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 88888, "outputTokens": 11, "stopReason": "end_turn"}))
					}
				})
				if err := config.UpdatePromptCacheSettings(true, "prefix_lifecycle", 1, 1, 1); err != nil {
					t.Fatal(err)
				}
				stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
				body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "store": false, "max_tokens": 256, "tools": []map[string]interface{}{{"type": "web_search"}}}
				if path == "/v1/responses" {
					body["input"] = "latest go release"
				} else {
					body["messages"] = []map[string]interface{}{{"role": "user", "content": "latest go release"}}
				}
				raw, _ := json.Marshal(body)
				failed := httptest.NewRecorder()
				r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
				if path == "/v1/responses" {
					h.handleOpenAIResponses(failed, r)
				} else {
					h.handleOpenAIChat(failed, r)
				}
				h.promptCache.mu.Lock()
				entries := len(h.promptCache.entriesByAccount)
				h.promptCache.mu.Unlock()
				if entries != 0 || calls != 2 {
					t.Fatalf("later-round failure warmed cache: entries=%d calls=%d", entries, calls)
				}
				fail = false
				previous := 0
				for repeat := 0; repeat < 2; repeat++ {
					got := localUsageJSONRequest(t, h, path, body, nil)
					u := runtimeProtocolUsage(t, got, stream)
					input, output, read, write := localUsageTotals(path, u)
					if input <= 0 || input >= 99999 || output != 18 || input-read-write != 2*claudeGenerationPrefixTokens {
						t.Fatalf("round usage lost or rescaled: %v", u)
					}
					if repeat == 0 && (read != 0 || write <= 0) || repeat == 1 && (read != previous || write != 0) {
						t.Fatalf("hosted lifecycle repeat=%d previous=%d usage=%v", repeat, previous, u)
					}
					previous = read + write
				}
			})
		}
	}
}
