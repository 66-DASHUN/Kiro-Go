package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalTokenBPEKnownCounts(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"hello", 1},
		{"hello world", 2},
		{strings.TrimSpace(strings.Repeat("hello ", 100)), 100},
	} {
		if got := estimateApproxTokens(tc.text); got != tc.want {
			t.Errorf("text length %d: got %d, want %d BPE tokens", len(tc.text), got, tc.want)
		}
	}
}

func TestLocalTokenCacheProfileRespectsCallerTotal(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	profile := tracker.BuildClaudeProfile(req, 1)
	if profile.TotalInputTokens != 1 {
		t.Fatalf("fingerprint wrappers inflated the caller's input: got %d, want 1", profile.TotalInputTokens)
	}
	for _, bp := range profile.Breakpoints {
		if bp.CumulativeTokens > 1 {
			t.Fatalf("breakpoint exceeds input: %+v", bp)
		}
	}
}

func localUsageRequest(t *testing.T, h *Handler, path string, stream bool, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
	if path == "/v1/responses" {
		body["input"], body["store"] = content, false
	} else {
		body["messages"] = []map[string]interface{}{{"role": "user", "content": content}}
	}
	return localUsageJSONRequest(t, h, path, body, nil)
}

func localUsageJSONRequest(t *testing.T, h *Handler, path string, body map[string]interface{}, key *config.ApiKeyEntry) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	if key != nil {
		req.Header.Set("Authorization", "Bearer "+key.Key)
	}
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestLocalUsageCountsInjectedThinkingInEveryProtocol(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}), meteringFrame(t))
				})
				body := map[string]interface{}{"model": "claude-sonnet-4.5-thinking", "stream": stream, "max_tokens": 256}
				if path == "/v1/responses" {
					body["input"], body["store"] = "hello", false
				} else {
					body["messages"] = []map[string]interface{}{{"role": "user", "content": "hello"}}
				}
				usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), stream)
				input, _, _, _ := localUsageTotals(path, usage)
				if want := estimateApproxTokens("hello") + estimateApproxTokens(ThinkingModePrompt); input != want {
					t.Fatalf("injected thinking prompt was omitted: input=%d want=%d", input, want)
				}
			})
		}
	}
}

func TestLocalUsageColdWarmCacheWithKeyRatios(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, ratio := range []float64{0.8, 0.95} {
				t.Run(fmt.Sprintf("%s/stream=%t/ratio=%.2f", path, stream, ratio), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						writeFrames(w,
							awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
							awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
							meteringFrame(t),
						)
					})
					enabled, mode := true, config.PromptCacheModeUniform
					key, err := config.AddApiKey(config.ApiKeyEntry{ID: "local-ratio", Key: "sk-local-test", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
					if err != nil {
						t.Fatal(err)
					}
					if err := config.UpdateSettingsPatch(&enabled, ""); err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					content := strings.TrimSpace(strings.Repeat("hello ", 2000))
					body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
					if path == "/v1/responses" {
						body["input"], body["store"] = content, false
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": content}}
					}
					for call := 0; call < 2; call++ {
						usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
						input, output, read, write := localUsageTotals(path, usage)
						if input != 2000 || output != 1 || read < 0 || write < 0 || read+write > input {
							t.Fatalf("local input/output/cache total invalid: %v", usage)
						}
						cached := write
						if call == 0 && (read != 0 || write <= 0) {
							t.Fatalf("cold request must only create cache: %v", usage)
						}
						if call == 1 {
							cached = read
							if read <= 0 || write != 0 {
								t.Fatalf("warm request must only read cache: %v", usage)
							}
						}
						if rate := float64(cached) / float64(input); math.Abs(rate-ratio) > 0.05+1.0/float64(input) {
							t.Fatalf("key ratio %.2f ignored: rate=%.4f usage=%v", ratio, rate, usage)
						}
						t.Logf("call=%d input=%d output=%d read=%d write=%d", call, input, output, read, write)
					}
				})
			}
		}
	}
}

func TestLocalUsageCountEndpointMatchesMessages(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}), meteringFrame(t))
	})
	body := map[string]interface{}{
		"model": "claude-sonnet-4.5-thinking", "max_tokens": 256,
		"system":   "Follow these instructions.",
		"messages": []map[string]interface{}{{"role": "user", "content": "你好"}, {"role": "assistant", "content": "hello"}, {"role": "user", "content": "continue"}},
		"tools":    []map[string]interface{}{{"name": "read_file", "description": "Read a file", "input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}}}},
	}
	count := localUsageJSONRequest(t, h, "/v1/messages/count_tokens", body, nil)
	var counted map[string]int
	if err := json.Unmarshal(count.Body.Bytes(), &counted); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		body["stream"] = stream
		usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/messages", body, nil), stream)
		input, _, _, _ := localUsageTotals("/v1/messages", usage)
		if input != counted["input_tokens"] {
			t.Fatalf("count endpoint=%v final input=%d", counted, input)
		}
	}
}

func localUsageTotals(path string, usage map[string]interface{}) (input, output, read, write int) {
	number := func(values map[string]interface{}, key string) int {
		value, _ := values[key].(float64)
		return int(value)
	}
	if path == "/v1/messages" {
		read, write = number(usage, "cache_read_input_tokens"), number(usage, "cache_creation_input_tokens")
		return number(usage, "input_tokens") + read + write, number(usage, "output_tokens"), read, write
	}
	inputField, outputField, detailsField := "prompt_tokens", "completion_tokens", "prompt_tokens_details"
	if path == "/v1/responses" {
		inputField, outputField, detailsField = "input_tokens", "output_tokens", "input_tokens_details"
	}
	details, _ := usage[detailsField].(map[string]interface{})
	return number(usage, inputField), number(usage, outputField), number(details, "cached_tokens"), number(details, "cache_write_tokens")
}

func TestLocalUsageProtocolsIgnoreContextOccupancy(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				calls := 0
				answers := []string{"hello", strings.TrimSpace(strings.Repeat("hello ", 100))}
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					answer := answers[calls]
					pct := []float64{2.05, 2.5}[calls]
					calls++
					writeFrames(w,
						awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": answer}),
						awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": pct}),
						awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 0.01}),
					)
				})
				content := "Reply with hello."
				wantInput := estimateApproxTokens(content)
				for i, answer := range answers {
					rec := localUsageRequest(t, h, path, stream, content)
					usage := runtimeProtocolUsage(t, rec, stream)
					input, output, read, write := localUsageTotals(path, usage)
					if input != wantInput || output != estimateApproxTokens(answer) {
						t.Fatalf("response %d: input=%d output=%d, want input=%d output=%d; context must not become input", i, input, output, wantInput, estimateApproxTokens(answer))
					}
					if read != 0 || write != 0 {
						t.Fatalf("short visible input must bypass cache: %+v", usage)
					}
				}
			})
		}
	}
}

func TestLocalUsageHostedSearchCountsEachRoundAndCachePrefix(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, nativeFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/nativeFirst=%t", path, stream, nativeFirst), func(t *testing.T) {
					calls, searches := 0, 0
					firstContent := strings.TrimSpace(strings.Repeat("hello ", 1200))
					secondContent := ""
					h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						calls++
						var payload KiroPayload
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if calls%2 == 1 {
							writeFrames(w, awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "search-id", "name": "web_search", "input": `{"query":"test"}`, "stop": true}))
							if nativeFirst {
								_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 1200, "outputTokens": 10}))
							}
						} else {
							secondContent = payload.ConversationState.CurrentMessage.UserInputMessage.Content
							writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}), meteringFrame(t))
						}
					})
					stubRestTransport(t, func(_ *http.Request) (*http.Response, error) {
						searches++
						result := strings.Repeat("new result ", 800) + fmt.Sprint(searches)
						return jsonResponse(200, `{"results":[{"title":"result","snippet":`+fmt.Sprintf("%q", result)+`}]}`), nil
					})
					enabled, mode, ratio := true, config.PromptCacheModeUniform, 1.0
					key, err := config.AddApiKey(config.ApiKeyEntry{ID: "hosted-local", Key: "sk-hosted-local", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
					if err != nil {
						t.Fatal(err)
					}
					if err := config.UpdateSettingsPatch(&enabled, ""); err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "store": false, "max_tokens": 256, "tools": []map[string]interface{}{{"type": "web_search"}}}
					if path == "/v1/responses" {
						body["input"] = firstContent
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": firstContent}}
					}
					for call := 0; call < 2; call++ {
						usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
						input, output, read, write := localUsageTotals(path, usage)
						minInput := 2*1200 + estimateApproxTokens(secondContent)
						minOutput := estimateApproxTokens("hello") + estimateApproxTokens("web_search") + estimateApproxTokens(`{"query":"test"}`)
						if nativeFirst {
							minOutput = 11
						}
						if input < minInput || output != minOutput {
							t.Fatalf("hosted rounds lost usage: input=%d want>=%d output=%d want=%d usage=%v", input, minInput, output, minOutput, usage)
						}
						if read+write > input || write <= 0 || (call == 1 && read <= 0) {
							t.Fatalf("changed search results must create new cache, preserve initial prefix read, and bound total: %v", usage)
						}
					}
				})
			}
		}
	}
}

func TestLocalUsageToolOutputCountedOnce(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, text := range []string{"", "hello"} {
				t.Run(fmt.Sprintf("%s/stream=%t/text=%s", path, stream, text), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						if text != "" {
							_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": text}))
						}
						_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "tool-1", "name": "read_file", "input": `{"path":"a.go"}`, "stop": true}))
					})
					usage := runtimeProtocolUsage(t, localUsageRequest(t, h, path, stream, "Read the file."), stream)
					_, output, _, _ := localUsageTotals(path, usage)
					want := estimateApproxTokens(text) + estimateApproxTokens("read_file") + estimateApproxTokens(`{"path":"a.go"}`)
					if output != want {
						t.Fatalf("tool output duplicated: output=%d want=%d usage=%v", output, want, usage)
					}
				})
			}
		}
	}
}

func TestLocalUsageHostedAndClientToolsCountVisibleResultText(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, native := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/native=%t", path, stream, native), func(t *testing.T) {
					var visibleResults string
					toolTokens := 0
					h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						var payload KiroPayload
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						for _, tool := range payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools {
							name := tool.ToolSpecification.Name
							toolTokens += estimateApproxTokens(name) + estimateApproxTokens(`{"query":"test"}`)
							_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": name, "name": name, "input": `{"query":"test"}`, "stop": true}))
						}
						if native {
							_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 100, "outputTokens": 123}))
						}
					})
					stubRestTransport(t, func(_ *http.Request) (*http.Response, error) {
						results := []WebSearchResult{{Title: "result", Snippet: strings.Repeat("hello ", 100)}}
						visibleResults = buildToolResultsContinuation([]KiroToolResult{{ToolUseID: "web_search", Content: []KiroResultContent{{Text: formatWebSearchResults(results)}}, Status: "success"}})
						body, _ := json.Marshal(map[string]interface{}{"results": results})
						return jsonResponse(200, string(body)), nil
					})
					body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "store": false, "tools": []map[string]interface{}{
						{"type": "web_search"},
						{"type": "function", "name": "client_tool", "parameters": map[string]interface{}{"type": "object"}, "function": map[string]interface{}{"name": "client_tool", "parameters": map[string]interface{}{"type": "object"}}},
					}}
					if path == "/v1/responses" {
						body["input"] = "search"
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": "search"}}
					}
					usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), stream)
					_, output, _, _ := localUsageTotals(path, usage)
					want := toolTokens + estimateApproxTokens(visibleResults)
					if native {
						want = 123
					}
					if output != want {
						t.Fatalf("mixed hosted/client output=%d want=%d (native=%t)", output, want, native)
					}
				})
			}
		}
	}
}

func TestLocalUsageFailedHostedRequestDoesNotUpdateCache(t *testing.T) {
	for _, cancelMCP := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelMCP=%t", cancelMCP), func(t *testing.T) {
			calls := 0
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					writeFrames(w, awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "search-id", "name": "web_search", "input": `{"query":"test"}`, "stop": true}))
				} else {
					http.Error(w, "failed follow-up", http.StatusInternalServerError)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stubRestTransport(t, func(_ *http.Request) (*http.Response, error) {
				if cancelMCP {
					cancel()
				}
				return jsonResponse(200, `{"results":[{"title":"result","snippet":"new search result"}]}`), nil
			})
			payload := testKiroPayload()
			payload.HostedSearchTools = map[string]bool{"web_search": true}
			payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.TrimSpace(strings.Repeat("hello ", 2000))
			settings := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1}
			_, err := callKiroWithHostedSearch(ctx, testAccount(t), payload, &KiroStreamCallback{}, h.promptCache, 2000, settings, false)
			if err == nil {
				t.Fatal("expected cancelled MCP or failed follow-up")
			}
			profile := h.promptCache.BuildOpenAIProfile(payload, 2000)
			if got := h.promptCache.ComputeWithSettings("only", profile, settings); got.CacheReadInputTokens != 0 {
				t.Fatalf("failed hosted request created a readable prefix: %+v", got)
			}
		})
	}
}
