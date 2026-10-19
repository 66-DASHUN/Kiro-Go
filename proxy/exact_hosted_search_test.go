package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"kiro-proxy/config"
	refcache "kiro-proxy/internal/sub2apiref/cache"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
)

func TestExactReferenceHostedSearchUsageAfterOriginalStreamFilter(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				calls := 0
				finalFrames := append(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>private plan</thinking>final answer"}), meteringFrame(t)...)
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls == 1 {
						writeFrames(w, awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "search-id", "name": "web_search", "input": `{"query":"test"}`, "stop": true}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 9999, "outputTokens": 99}))
					} else {
						writeFrames(w, finalFrames)
					}
				})
				stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
				key := exactHTTPKey(t, 1)
				h.pool.Reload()
				body := exactHTTPRequest(path, stream, []map[string]interface{}{{"role": "user", "content": "Search for a recent fact."}})
				body["max_tokens"] = 1 // Dedicated search does not pass a local output cap.
				if path == "/v1/messages" {
					body["tools"] = []ClaudeTool{nativeWebSearchTool()}
				} else {
					body["tools"] = []map[string]interface{}{{"type": "web_search"}}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				initial, err := refcache.InputTokens(context.Background(), exactHTTPProtocol(path), raw)
				if err != nil {
					t.Fatal(err)
				}
				var want refkiro.Usage
				var wantJSONUsage map[string]interface{}
				if stream || path != "/v1/messages" {
					var rawSSE bytes.Buffer
					_, err := refkiro.StreamEventStreamAsAnthropicWithContext(context.Background(), bytes.NewReader(finalFrames), &rawSSE, "claude-sonnet-4.5", initial, refkiro.KiroRequestContext{})
					if err != nil {
						t.Fatal(err)
					}
					// Use the actual pinned filter, including its final-round bug:
					// all inner usage disappears, leaving only the outer start.
					for _, chunk := range bytes.Split(rawSSE.Bytes(), []byte("\n\n")) {
						filtered, forward := refkiro.AdjustSSEChunk(chunk, 2)
						if forward && (bytes.Contains(filtered, []byte(`"usage"`)) || bytes.Contains(filtered, []byte(`"message_stop"`))) {
							t.Fatalf("reference filter forwarded terminal/usage: %s", filtered)
						}
					}
					want = refkiro.Usage{InputTokens: initial}
				} else {
					result, err := refkiro.ParseNonStreamingEventStreamWithContext(bytes.NewReader(finalFrames), "claude-sonnet-4.5", refkiro.KiroRequestContext{EstimatedInputTokens: initial})
					if err != nil {
						t.Fatal(err)
					}
					want = result.Usage
					var response struct {
						Usage map[string]interface{} `json:"usage"`
					}
					if err := json.Unmarshal(result.ResponseBody, &response); err != nil {
						t.Fatal(err)
					}
					wantJSONUsage = response.Usage
				}
				rec := localUsageJSONRequest(t, h, path, body, &key)
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				usage := runtimeProtocolUsage(t, rec, stream)
				if path == "/v1/messages" {
					if _, ok := usage["server_tool_use"]; ok {
						t.Fatalf("dedicated reference does not add server-tool usage: %v", usage)
					}
					if !stream && !reflect.DeepEqual(usage, wantJSONUsage) {
						t.Fatalf("buffered usage=%v reference=%v", usage, wantJSONUsage)
					}
					if stream && (!strings.Contains(rec.Body.String(), "event: message_stop") || usage["_sub2api_kiro_credits"] != nil || usage["cache_creation"] != nil) {
						t.Fatalf("stream must retain closure without filtered native usage: %s", rec.Body.String())
					}
				}
				input, output, _, _ := localUsageTotals(path, usage)
				if calls != 2 || input != want.InputTokens || output != want.OutputTokens {
					t.Fatalf("calls=%d usage=%v want exposed input/output %d/%d", calls, usage, want.InputTokens, want.OutputTokens)
				}
			})
		}
	}
}

func TestExactReferenceHostedSearchInitialCacheAndEmptyStreamContext(t *testing.T) {
	ctx := context.Background()
	settings := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeSub2API, Ratio: 1}
	for _, mixed := range []bool{false, true} {
		tools := []ClaudeTool{nativeWebSearchTool()}
		if mixed {
			tools = append(tools, ClaudeTool{Name: "other", InputSchema: map[string]interface{}{"type": "object"}})
		}
		body, _ := json.Marshal(map[string]interface{}{"model": "claude-sonnet-4.5", "tools": tools, "messages": []map[string]interface{}{{"role": "user", "content": strings.Repeat("cacheable prefix ", 600)}}})
		for _, stream := range []bool{false, true} {
			meter, err := newExactRequestMeter(ctx, settings, "anthropic", body, "claude-sonnet-4.5", stream, true, 1)
			if err != nil {
				t.Fatal(err)
			}
			meter.StopSequences = []string{"answer"}
			state := newExactHostedSearchState(meter)
			tracker := newPromptCacheTracker(time.Hour)
			first := &KiroPayload{}
			initial := state.prepare(first, tracker, "account", settings)
			if initial.CacheCreationInputTokens != meter.EstimatedInput {
				t.Fatalf("initial cache=%+v", initial)
			}
			first.exactMeter.Commit() // HTTP acceptance, before stream parsing.
			frames := exactHTTPCompleteFrames(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>private planning text</thinking>a sufficiently long final answer"}))
			if _, err := parseExactEventStreamContext(ctx, io.NopCloser(bytes.NewReader(frames)), first, &KiroStreamCallback{}); err != nil {
				t.Fatal(err)
			}
			followup := &KiroPayload{}
			reused := state.prepare(followup, tracker, "account", settings)
			if reused.CacheCreationInputTokens != initial.CacheCreationInputTokens || reused.CacheReadInputTokens != 0 {
				t.Fatal("follow-up must reuse initial cold plan")
			}
			if _, err := parseExactEventStreamContext(ctx, io.NopCloser(bytes.NewReader(frames)), followup, &KiroStreamCallback{}); err != nil {
				t.Fatal(err)
			}
			input, output, usage, ok := exactHostedRoundUsage(followup, reused)
			if !ok || ((!stream || mixed) && output <= 1) || (stream && !mixed && output != 0) {
				t.Fatalf("dedicated context must ignore thinking/max/stop settings: %d/%d %+v", input, output, usage)
			}
			wantInput, wantOrdinary := meter.EstimatedInput*2, meter.EstimatedInput
			if stream && !mixed {
				wantInput, wantOrdinary = meter.EstimatedInput, 0
			}
			if input != wantInput || *usage.exactInput != wantOrdinary {
				t.Fatalf("reference full-cache fallback/stream merge: %d %+v", input, usage)
			}
			fresh := newExactHostedSearchState(meter)
			warm := fresh.prepare(&KiroPayload{}, tracker, "account", settings)
			if warm.CacheReadInputTokens != meter.EstimatedInput {
				t.Fatal("first HTTP acceptance must commit initial prefix")
			}
		}
	}
}

func TestExactReferenceHostedSearchOpenAIInitialCache(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, exactHTTPCompleteFrames(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer"})))
				})
				key := exactHTTPKey(t, 1)
				h.pool.Reload()
				body := exactHTTPRequest(path, stream, []map[string]interface{}{{"role": "user", "content": strings.Repeat("cacheable search context ", 600)}})
				body["tools"] = []map[string]interface{}{{"type": "web_search"}}
				raw, _ := json.Marshal(body)
				wantIn, err := refcache.InputTokens(context.Background(), exactHTTPProtocol(path), raw)
				if err != nil {
					t.Fatal(err)
				}
				for request := 0; request < 2; request++ {
					rec := localUsageJSONRequest(t, h, path, body, &key)
					if rec.Code != 200 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					usage := runtimeProtocolUsage(t, rec, stream)
					input, output, read, write := localUsageTotals(path, usage)
					if path == "/v1/responses" {
						v, _ := usage["cache_creation_input_tokens"].(float64)
						write = int(v)
					} else if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
						v, _ := details["cache_creation_tokens"].(float64)
						write = int(v)
					}
					if input != wantIn || output != 0 || read+write <= 0 || (request == 0 && write == 0) || (request == 1 && read == 0) {
						t.Fatalf("request=%d usage=%v want initial input=%d output=0", request, usage, wantIn)
					}
				}
			})
		}
	}
}

func TestExactReferenceHostedSearchSoleConvertedToolGate(t *testing.T) {
	for _, tt := range []struct {
		protocol, body string
		want           bool
	}{
		{"anthropic", `{"tools":[{"name":"google_search"}]}`, true},
		{"anthropic", `{"tools":[{"type":"web_search_20250305"},{"name":"other"}]}`, false},
		{"chat_completions", `{"messages":[{"role":"user","content":"query"}],"tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object"}}}]}`, true},
		{"responses", `{"input":"query","tools":[{"type":"web_search"},{"type":"custom","name":"other"}]}`, false},
	} {
		meter := &exactRequestMeter{Protocol: tt.protocol, Body: []byte(tt.body), Model: "claude-sonnet-4.5", Stream: true}
		if got := exactDedicatedHostedStream(meter); got != tt.want {
			t.Fatalf("%s %s gate=%t want=%t", tt.protocol, tt.body, got, tt.want)
		}
	}
}

func TestExactReferenceDirectSearchSummaryAndCache(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) { t.Error("direct search must not call the model") })
			stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
			key := exactHTTPKey(t, 1)
			h.pool.Reload()
			query := strings.TrimSpace(strings.Repeat("搜索资料 ", 1200))
			body := exactHTTPRequest("/v1/messages", stream, []map[string]interface{}{{"role": "user", "content": claudeCodeWebSearchPrefix + " " + query}})
			body["tools"] = []ClaudeTool{nativeWebSearchTool()}
			body["max_tokens"] = 1
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			wantIn, err := refcache.InputTokens(context.Background(), "anthropic", raw)
			if err != nil {
				t.Fatal(err)
			}
			results, err := parseMCPWebSearchResponse([]byte(mcpOneResult))
			if err != nil {
				t.Fatal(err)
			}
			wantOut := len(fmt.Sprintf("Here are the search results for \"%s\":\n\n1. **%s**\n   %s\n   %s\n\n", query, results[0].Title, results[0].URL, results[0].Snippet)) / 4
			for request := 0; request < 2; request++ {
				rec := localUsageJSONRequest(t, h, "/v1/messages", body, &key)
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				usage := runtimeProtocolUsage(t, rec, stream)
				if _, ok := usage["server_tool_use"]; ok {
					t.Fatalf("generic reference search does not add server_tool_use usage: %v", usage)
				}
				input, output, read, write := localUsageTotals("/v1/messages", usage)
				if input != wantIn || output != wantOut || read+write != wantIn {
					t.Fatalf("direct usage=%v want %d/%d", usage, wantIn, wantOut)
				}
				if request == 0 && write != wantIn || request == 1 && read != wantIn {
					t.Fatalf("direct request %d cache=%v", request, usage)
				}
			}
		})
	}
}
