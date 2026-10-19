package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func outputBudgetClaudeContent(t *testing.T, body string, stream bool) (text, thinking, signature, stop string, tools []KiroToolUse) {
	t.Helper()
	if !stream {
		var response ClaudeResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		for _, block := range response.Content {
			text += block.Text
			thinking += block.Thinking
			signature += block.Signature
			if block.Type == "tool_use" {
				input, _ := block.Input.(map[string]interface{})
				tools = append(tools, KiroToolUse{Name: block.Name, Input: input})
			}
		}
		return text, thinking, signature, response.StopReason, tools
	}
	for _, event := range parseResponsesSSE(t, body) {
		switch event.Name {
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if s, ok := delta["partial_json"].(string); ok && len(tools) > 0 {
				if err := json.Unmarshal([]byte(s), &tools[len(tools)-1].Input); err != nil {
					t.Fatalf("broken tool JSON: %q", s)
				}
			}
			if s, ok := delta["text"].(string); ok {
				text += s
			}
			if s, ok := delta["thinking"].(string); ok {
				thinking += s
			}
			if s, ok := delta["signature"].(string); ok {
				signature += s
			}
		case "content_block_start":
			block, _ := event.Data["content_block"].(map[string]interface{})
			if block["type"] == "tool_use" {
				tools = append(tools, KiroToolUse{Name: scalarString(block["name"])})
			}
		case "message_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			stop, _ = delta["stop_reason"].(string)
		}
	}
	return
}

func TestOutputBudgetChunkIndependence(t *testing.T) {
	for _, text := range []string{strings.Repeat("hello ", 3000), strings.Repeat("中文🙂 e\u0301 token ", 500), strings.Repeat("x", 128)} {
		var expected string
		for _, chunkRunes := range []int{1, 7, 128, 100000} {
			var got strings.Builder
			budget := newOutputBudget(1000)
			cb := wrapOutputBudget(&KiroStreamCallback{OnText: func(s string, _ bool) { got.WriteString(s) }}, budget)
			runes := []rune(text)
			for start := 0; start < len(runes); start += chunkRunes {
				cb.OnText(string(runes[start:min(start+chunkRunes, len(runes))]), false)
			}
			cb.flushOutput()
			if !utf8.ValidString(got.String()) || estimateBudgetTokens(got.String()) > 1000 {
				t.Fatalf("invalid or over-budget Unicode output: %q", got.String())
			}
			if chunkRunes == 1 {
				expected = got.String()
			} else if got.String() != expected {
				t.Fatalf("chunk=%d changed the returned prefix", chunkRunes)
			}
		}
	}
}

func TestOutputBudgetThinkingSignaturesToolsAndNativeUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"thinking", "inline", "tool", "structured", "native"} {
			t.Run(fmt.Sprintf("%v/%s", stream, mode), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					if mode == "native" {
						writeFrames(w, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 0.25, "tokenUsage": map[string]interface{}{"inputTokens": 123, "outputTokens": 99}}))
					}
					switch mode {
					case "thinking":
						writeFrames(w, awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": strings.Repeat("hello ", 100), "signature": "must-not-sign-truncated-thought"}))
					case "inline":
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>hello hello</thinking>" + strings.Repeat(" world", 100)}))
					case "tool":
						writeFrames(w, awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t1", "name": "read", "input": "{}", "stop": true}), awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t2", "name": "write", "input": `{"text":"` + strings.Repeat("hello ", 100) + `"}`, "stop": true}))
					case "structured":
						writeKiroTextResponse(t, w, `{"answer":"`+strings.Repeat("hello ", 100)+`"}`)
					default:
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("hello ", 100)}), meteringFrame(t))
					}
				})
				body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 8, "stream": stream, "messages": []map[string]interface{}{{"role": "user", "content": "long answer"}}}
				if mode == "thinking" || mode == "inline" {
					body["thinking"] = map[string]interface{}{"type": "adaptive"}
				}
				if mode == "structured" {
					body["output_config"] = map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "schema": map[string]interface{}{"type": "object"}}}
				}
				rec := localUsageJSONRequest(t, h, "/v1/messages", body, nil)
				text, thinking, signature, stop, tools := outputBudgetClaudeContent(t, rec.Body.String(), stream)
				if estimateClaudeOutputTokens(text, thinking, tools) > 8 || stop != "max_tokens" {
					t.Fatalf("budget/stop mismatch: %s", rec.Body.String())
				}
				if mode == "thinking" && (signature != "" || thinking == "") {
					t.Fatalf("truncated thinking signature/content invalid: %s", rec.Body.String())
				}
				if mode == "tool" && (len(tools) != 1 || tools[0].Name != "read") {
					t.Fatalf("tool must be complete or omitted: %s", rec.Body.String())
				}
				if mode == "structured" && json.Valid([]byte(text)) {
					t.Fatalf("short structured output unexpectedly repaired: %q", text)
				}
				if mode == "native" {
					usage := runtimeProtocolUsage(t, rec, stream)
					input, output, _, _ := localUsageTotals("/v1/messages", usage)
					if input != estimateClaudeRequestInputTokens(&ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "long answer"}}}) || output != 99 {
						t.Fatalf("public usage must use semantic input while preserving output: %v", usage)
					}
				}
			})
		}
	}
}

func TestOutputBudgetSharedAcrossSearchRounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			t.Run(fmt.Sprintf("%s/%v", path, stream), func(t *testing.T) {
				var calls atomic.Int32
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					if calls.Add(1) == 1 {
						writeFrames(w, webSearchToolUseFrame(t, "s1", "test"), meteringFrame(t))
					} else {
						writeKiroTextResponse(t, w, strings.Repeat("hello ", 100))
					}
				})
				stub := stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
				toolCost := estimateBudgetTokens("web_search") + estimateBudgetTokens(`{"query":"test"}`)
				body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": toolCost + 4, "max_output_tokens": toolCost + 4, "stream": stream, "messages": []map[string]interface{}{{"role": "user", "content": "find current information"}}, "tools": []map[string]interface{}{{"type": "web_search_20250305", "name": "web_search"}}}
				if path != "/v1/messages" {
					body["tools"] = []map[string]interface{}{{"type": "web_search_preview"}}
				}
				if path == "/v1/responses" {
					body["input"] = "find current information"
				}
				rec := localUsageJSONRequest(t, h, path, body, nil)
				usage := runtimeProtocolUsage(t, rec, stream)
				_, out, _, _ := localUsageTotals(path, usage)
				if out > toolCost+4 {
					t.Fatalf("search reset output budget: output=%d limit=%d body=%s", out, toolCost+4, rec.Body.String())
				}
				if calls.Load() != 2 || stub.calls.Load() != 1 {
					t.Fatalf("calls model=%d search=%d", calls.Load(), stub.calls.Load())
				}
			})
		}
	}
}

func TestOutputBudgetClientCancellationIsStillCancellation(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeKiroTextResponse(t, w, "answer") })
	_ = h
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := testKiroPayload()
	payload.InferenceConfig = &InferenceConfig{MaxTokens: 8}
	if err := CallKiroAPIContext(ctx, testAccount(t), payload, &KiroStreamCallback{}); err != context.Canceled {
		t.Fatalf("cancellation replaced with local success: %v", err)
	}
}

func TestOutputBudgetCancellationDuringEmissionWins(t *testing.T) {
	newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeKiroTextResponse(t, w, strings.Repeat("hello ", 100))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := testKiroPayload()
	payload.InferenceConfig = &InferenceConfig{MaxTokens: 8}
	err := CallKiroAPIContext(ctx, testAccount(t), payload, &KiroStreamCallback{OnText: func(string, bool) { cancel() }})
	if err != context.Canceled {
		t.Fatalf("client cancellation overridden by local completion: %v", err)
	}
}

func TestOutputBudgetPreservesSignatureAtExactThinkingBoundary(t *testing.T) {
	var text, signature string
	budget := newOutputBudget(1)
	callback := wrapOutputBudget(&KiroStreamCallback{OnText: func(s string, _ bool) { text += s }, OnThinkingSignature: func(s string) { signature += s }}, budget)
	callback.OnText("hello", true)
	callback.OnThinkingSignature("genuine-signature")
	if text != "hello" || signature != "genuine-signature" || !budget.stopped {
		t.Fatalf("unchanged complete thinking lost signature: text=%q signature=%q stopped=%v", text, signature, budget.stopped)
	}
}

func TestOutputBudgetStreamsShortTextWithBudgetHeadroom(t *testing.T) {
	var got string
	cb := wrapOutputBudget(&KiroStreamCallback{OnText: func(s string, _ bool) { got += s }}, newOutputBudget(1000))
	cb.OnText("hello", false)
	if got != "hello" {
		t.Fatalf("short text withheld despite budget headroom: %q", got)
	}
	cb.flushOutput()
	if got != "hello" {
		t.Fatalf("already streamed text repeated at flush: %q", got)
	}
}

func TestOutputBudgetStopsSearchBeforePaidFollowup(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeFrames(w, webSearchToolUseFrame(t, "s1", "test"), meteringFrame(t))
			})
			stub := stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
			cost := estimateBudgetTokens("web_search") + estimateBudgetTokens(`{"query":"test"}`)
			body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": cost, "max_output_tokens": cost, "messages": []map[string]interface{}{{"role": "user", "content": "current information"}}, "tools": []map[string]interface{}{{"type": "web_search_20250305", "name": "web_search"}}}
			if path != "/v1/messages" {
				body["tools"] = []map[string]interface{}{{"type": "web_search_preview"}}
			}
			if path == "/v1/responses" {
				body["input"] = "current information"
			}
			localUsageJSONRequest(t, h, path, body, nil)
			if calls.Load() != 1 || stub.calls.Load() != 0 {
				t.Fatalf("exhausted budget spent again: model=%d search=%d", calls.Load(), stub.calls.Load())
			}
		})
	}
}

func TestOutputBudgetDirectSearchCapsGeneratedOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) { t.Error("direct search called model") })
			stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
			rec := postClaudeMessages(t, h, fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":8,"stream":%v,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: go release"}]}`, stream))
			text, thinking, _, stop, tools := outputBudgetClaudeContent(t, rec.Body.String(), stream)
			if estimateClaudeOutputTokens(text, thinking, tools) > 8 || stop != "max_tokens" {
				t.Fatalf("direct search exceeded cap: %s", rec.Body.String())
			}
		})
	}
}

func TestOutputBudgetRestoresDiscardedSearchAttempt(t *testing.T) {
	var calls atomic.Int32
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "discarded discarded discarded discarded"}))
			return
		}
		writeKiroTextResponse(t, w, strings.Repeat("hello ", 100))
	})
	rec := postClaudeMessages(t, h, `{"model":"claude-sonnet-4.5","max_tokens":12,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"current information"}]}`)
	text, _, _, stop, _ := outputBudgetClaudeContent(t, rec.Body.String(), false)
	if calls.Load() != 2 || estimateBudgetTokens(text) != 12 || strings.Contains(text, "discarded") || stop != "max_tokens" {
		t.Fatalf("discarded attempt consumed returned budget: calls=%d %s", calls.Load(), rec.Body.String())
	}
}

func TestOutputBudgetCompletesBeforeFollowingUpstreamException(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("hello ", 15)}), awsEventStreamFrameWithHeaders(t, map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException"}, map[string]interface{}{"message": "after output limit"}))
			})
			rec := localUsageJSONRequest(t, h, "/v1/messages", map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 8, "stream": stream, "messages": []map[string]interface{}{{"role": "user", "content": "long answer"}}}, nil)
			text, _, _, stop, _ := outputBudgetClaudeContent(t, rec.Body.String(), stream)
			if calls.Load() != 1 || estimateBudgetTokens(text) > 8 || stop != "max_tokens" {
				t.Fatalf("post-limit exception broke local completion: %s", rec.Body.String())
			}
		})
	}
}

func TestOutputBudgetPreservesOnlyObservedMetering(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				meter := awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 0.25, "tokenUsage": map[string]interface{}{"inputTokens": 123, "outputTokens": 99}})
				content := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("hello ", 100)})
				if observed {
					writeFrames(w, meter, content)
				} else {
					writeFrames(w, content, meter)
				}
			})
			payload := testKiroPayload()
			payload.InferenceConfig = &InferenceConfig{MaxTokens: 8}
			var in, out int
			var credits float64
			var stop string
			err := CallKiroAPIContext(context.Background(), testAccount(t), payload, &KiroStreamCallback{OnComplete: func(i, o int) { in, out = i, o }, OnCredits: func(c float64) { credits = c }, OnStopReason: func(s string) { stop = s }})
			if err != nil || stop != "max_tokens" {
				t.Fatalf("local stop failed: err=%v stop=%q", err, stop)
			}
			if observed {
				if in != 123 || out != 99 || credits != 0.25 {
					t.Fatalf("observed native usage lost: %d/%d/%g", in, out, credits)
				}
			} else if in != 0 || out != 0 || credits != 0 {
				t.Fatalf("unseen usage invented: %d/%d/%g", in, out, credits)
			}
		})
	}
}

func TestOutputBudgetEnforcesReturnedText(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload KiroPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.InferenceConfig == nil || payload.InferenceConfig.MaxTokens != 8 {
					t.Error("native max_tokens forwarding lost")
				}
				writeKiroTextResponse(t, w, strings.TrimSpace(strings.Repeat("hello ", 100)))
			})
			rec := localUsageJSONRequest(t, h, "/v1/messages", map[string]interface{}{
				"model": "claude-sonnet-4.5", "max_tokens": 8, "stream": stream,
				"messages": []map[string]interface{}{{"role": "user", "content": "A long answer, please."}},
			}, nil)
			text, thinking, _, stop, tools := outputBudgetClaudeContent(t, rec.Body.String(), stream)
			if got := estimateClaudeOutputTokens(text, thinking, tools); got > 8 || got == 0 {
				t.Fatalf("returned %d semantic tokens for max_tokens=8: %q", got, text)
			}
			if stop != "max_tokens" {
				t.Fatalf("stop_reason=%q, want max_tokens", stop)
			}
			if calls.Load() != 1 {
				t.Fatalf("local stop retried upstream %d times", calls.Load())
			}
			requests := getObserveStore().RecentRequests(1)
			if len(requests) != 1 || !requests[0].Success || requests[0].Status != 200 || requests[0].Credits != 0 {
				t.Fatalf("intentional stop mis-accounted: %+v", requests)
			}
			if account := testAccount(t); !account.Enabled || account.BanStatus != "" {
				t.Fatalf("local limit harmed account: %+v", account)
			}
			usage := runtimeProtocolUsage(t, rec, stream)
			if usage["output_tokens"].(float64) > 8 {
				t.Fatalf("local usage exceeds returned budget: %v", usage)
			}
		})
	}
}
