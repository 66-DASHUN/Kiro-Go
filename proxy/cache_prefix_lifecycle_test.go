package proxy

import (
	"encoding/json"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheOptimizationToolMarkersSurviveDecoding(t *testing.T) {
	var req ClaudeRequest
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-sonnet-4.5",
		"tools": []interface{}{map[string]interface{}{
			"name": "lookup", "description": strings.Repeat("Stable tool documentation. ", 700),
			"input_schema":  map[string]interface{}{"type": "object"},
			"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"},
		}},
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "lookup one"}},
	})
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	profile := tracker.BuildClaudeProfile(&req, 10000)
	usage := tracker.Compute("account", profile)
	if usage.CacheCreation1hInputTokens <= 0 || usage.CacheCreation5mInputTokens != 0 {
		t.Fatalf("tool's one-hour marker was lost: %+v", usage)
	}
	tracker.Update("account", profile)
	req.Messages[0].Content = "lookup two"
	if got := tracker.Compute("account", tracker.BuildClaudeProfile(&req, 10000)); got.CacheReadInputTokens <= 0 {
		t.Fatalf("unchanged explicit tool prefix was not reused: %+v", got)
	}
	tracker.Update("account", tracker.BuildClaudeProfile(&req, 10000))
	req.Tools[0].CacheControl["ttl"] = "5m"
	if got := tracker.Compute("account", tracker.BuildClaudeProfile(&req, 10000)); got.CacheReadInputTokens != 10000 {
		t.Fatalf("TTL metadata must not change a content fingerprint: %+v", got)
	}
}

func TestCacheOptimizationReasoningContextIsolated(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("stable input ", 1000)}}}
	tracker.Update("account", tracker.BuildClaudeProfile(req, 10000))
	req.Thinking = &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048}
	if got := tracker.Compute("account", tracker.BuildClaudeProfile(req, 10000)); got.CacheReadInputTokens != 0 {
		t.Fatalf("different Claude thinking context reused cache: %+v", got)
	}
	payload := estimatedOpenAIPayload("unchanged current input")
	tracker.Update("account", tracker.BuildOpenAIProfile(payload, 10000))
	payload.AdditionalModelRequestFields = map[string]interface{}{"reasoning_effort": "high"}
	if got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000)); got.CacheReadInputTokens != 0 {
		t.Fatalf("different OpenAI reasoning context reused cache: %+v", got)
	}
}

func TestCacheOptimizationLookupDoesNotRenew(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
	original := promptCacheEntry{ExpiresAt: time.Now().Add(time.Minute), TTL: time.Hour}
	tracker.entriesByAccount["account"] = map[[32]byte]promptCacheEntry{{1}: original}
	if got := tracker.Compute("account", profile); got.CacheReadInputTokens != 10000 {
		t.Fatalf("live prefix should read: %+v", got)
	}
	if stored := tracker.entriesByAccount["account"][[32]byte{1}]; stored != original {
		t.Fatalf("lookup renewed before successful completion: before=%+v after=%+v", original, stored)
	}
	tracker.Update("account", profile)
	if !tracker.entriesByAccount["account"][[32]byte{1}].ExpiresAt.After(original.ExpiresAt) {
		t.Fatal("successful completion did not renew")
	}
}

func TestCacheOptimizationShorterTTLDoesNotReplaceLonger(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: time.Hour}}}
	tracker.Update("account", profile)
	original := tracker.entriesByAccount["account"][[32]byte{2}]
	profile.Breakpoints[0].TTL = 5 * time.Minute
	tracker.Update("account", profile)
	stored := tracker.entriesByAccount["account"][[32]byte{2}]
	if stored.TTL != time.Hour || stored.ExpiresAt.Before(original.ExpiresAt) {
		t.Fatalf("short TTL downgraded live hour entry: before=%+v after=%+v", original, stored)
	}
}

func TestCacheOptimizationTrackerMaximumTTL(t *testing.T) {
	tracker := newPromptCacheTracker(5 * time.Minute)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{3}, CumulativeTokens: 10000, TTL: 2 * time.Hour}}}
	tracker.Update("account", profile)
	if got := tracker.entriesByAccount["account"][[32]byte{3}].TTL; got != 5*time.Minute {
		t.Fatalf("maximum TTL ignored: %v", got)
	}
}

func TestCacheOptimizationOpenAICompleteRepeatAndNewTail(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("current turn ", 600))
	profile := tracker.BuildOpenAIProfile(payload, 10000)
	tracker.Update("account", profile)
	if got := tracker.Compute("account", profile); got.CacheReadInputTokens != 10000 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("complete repeat must cover full input: %+v", got)
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("different turn ", 1200)
	changed := tracker.BuildOpenAIProfile(payload, 16000)
	got := tracker.Compute("account", changed)
	if got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens <= 0 || got.CacheReadInputTokens+got.CacheCreationInputTokens != 16000 {
		t.Fatalf("changed tail must read old prefix and create new tail: %+v", got)
	}
	if other := tracker.Compute("other-account", changed); other.CacheReadInputTokens != 0 {
		t.Fatalf("cross-account reuse: %+v", other)
	}
}

func TestCacheOptimizationOpenAISingleTurnBecomesHistory(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := testKiroPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("first large input ", 1000)
	tracker.Update("account", tracker.BuildOpenAIProfile(payload, 10000))
	previous := payload.ConversationState.CurrentMessage.UserInputMessage
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &previous},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "ok"}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("next input ", 600)
	got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 15000))
	if got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens <= 0 || got.CacheReadInputTokens+got.CacheCreationInputTokens != 15000 {
		t.Fatalf("single-turn prefix lost when moved into history: %+v", got)
	}
}

func TestCacheOptimizationOpenAIToolResultAndImageTail(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("current prompt ", 600))
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	current.UserInputMessageContext = &UserInputMessageContext{ToolResults: []KiroToolResult{{ToolUseID: "call-one", Content: []KiroResultContent{{Text: "result one"}}}}}
	current.Images = []KiroImage{{Format: "png"}}
	current.Images[0].Source.Bytes = "first-image"
	tracker.Update("account", tracker.BuildOpenAIProfile(payload, 10000))
	current.UserInputMessageContext.ToolResults[0].Content[0].Text = "result two"
	current.Images[0].Source.Bytes = "second-image"
	got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000))
	if got.CacheReadInputTokens <= 0 || got.CacheReadInputTokens >= 10000 || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("changed semantic tail must create tokens: %+v", got)
	}
}

func TestCacheOptimizationOpenAIEmptyToolContextKeepsPrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("current input ", 600))
	tracker.Update("account", tracker.BuildOpenAIProfile(payload, 10000))
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{{ToolUseID: "new-call", Content: []KiroResultContent{{Text: "new result"}}}},
	}
	if got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000)); got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("absent tool definitions must retain earlier history: %+v", got)
	}
}

func TestCacheOptimizationNativeShortInputBypassesStorage(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload("short current")
	settings := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1}
	snapshot := beginOpenAICacheUsage(tracker, "account", payload, 10000, settings)
	if got := finishOpenAICacheUsage(tracker, "account", snapshot, 500); got != nil {
		t.Fatalf("native short input should have no cache detail: %+v", got)
	}
	if len(tracker.entriesByAccount["account"]) != 0 {
		t.Fatal("native short input stored a prefix")
	}
}

func TestCacheOptimizationProtocolMultiTurnReferenceRatios(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 137}))
				})
				enabled, mode, ratio := true, config.PromptCacheModeUniform, 0.8
				key, err := config.AddApiKey(config.ApiKeyEntry{ID: "prefix-key", Key: "sk-prefix-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
				if err != nil {
					t.Fatal(err)
				}
				h.pool.Reload()
				messages := []map[string]interface{}{
					{"role": "user", "content": strings.Repeat("stable initial input ", 1000)},
					{"role": "assistant", "content": strings.Repeat("previous answer ", 200)},
					{"role": "user", "content": strings.Repeat("current input ", 600)},
				}
				request := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
				if path == "/v1/responses" {
					request["input"], request["store"] = messages, false
				} else {
					request["messages"] = messages
				}
				body, _ := json.Marshal(request)
				semanticInput := estimateApproxTokens(strings.Repeat("stable initial input ", 1000)) + estimateApproxTokens(strings.Repeat("previous answer ", 200)) + estimateApproxTokens(strings.Repeat("current input ", 600))
				for i := 0; i < 2; i++ {
					r := withApiKeyContext(httptest.NewRequest("POST", path, strings.NewReader(string(body))), &key)
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
						t.Fatalf("request failed: %s", rec.Body.String())
					}
					usage := runtimeProtocolUsage(t, rec, stream)
					var read, creation, total, output float64
					switch path {
					case "/v1/messages":
						read, _ = usage["cache_read_input_tokens"].(float64)
						creation, _ = usage["cache_creation_input_tokens"].(float64)
						total = usage["input_tokens"].(float64) + read + creation
						output = usage["output_tokens"].(float64)
					default:
						field := "prompt_tokens_details"
						input, out := "prompt_tokens", "completion_tokens"
						if path == "/v1/responses" {
							field, input, out = "input_tokens_details", "input_tokens", "output_tokens"
						}
						details, ok := usage[field].(map[string]interface{})
						if !ok {
							t.Fatalf("cache details missing: %v", usage)
						}
						read, _ = details["cached_tokens"].(float64)
						creation, _ = details["cache_write_tokens"].(float64)
						total, output = usage[input].(float64), usage[out].(float64)
					}
					expectedTotal := 10000
					if path == "/v1/messages" {
						expectedTotal = semanticInput
					}
					if total != float64(expectedTotal) || output != 137 || read+creation < float64(expectedTotal)*.75 || read+creation > float64(expectedTotal)*.85 || (i == 0 && read != 0) || (i == 1 && creation != 0) {
						t.Fatalf("request %d has inconsistent native totals or reference ratio: %v", i, usage)
					}
					t.Logf("request %d: total=%.0f output=%.0f creation=%.0f read=%.0f", i, total, output, creation, read)
				}
			})
		}
	}
}

func TestCacheOptimizationModelToolsAndTransportIdentity(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("current text ", 600))
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{{}}}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.Name = "lookup"
	tracker.Update("account", tracker.BuildOpenAIProfile(payload, 10000))
	payload.ConversationState.ConversationID = "new-transport-id"
	payload.ProfileArn = "new-profile-routing-value"
	if got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000)); got.CacheReadInputTokens != 10000 {
		t.Fatalf("transport identity invalidated content: %+v", got)
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.Description = "changed tool definition"
	if got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000)); got.CacheReadInputTokens != 0 {
		t.Fatalf("changed tool definition reused cache: %+v", got)
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.Description = ""
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-4.5"
	if got := tracker.Compute("account", tracker.BuildOpenAIProfile(payload, 10000)); got.CacheReadInputTokens != 0 {
		t.Fatalf("different model reused cache: %+v", got)
	}
}

func TestCacheOptimizationFailureDoesNotRenewProtocols(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				var fail atomic.Bool
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					if fail.Load() {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"message":"invalid fixture request"}`))
						return
					}
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 137}))
				})
				if err := config.UpdatePromptCacheSettings(true, "prefix_total", 1, 1, 1); err != nil {
					t.Fatal(err)
				}
				if rec := runtimeProtocolRequest(t, h, path, stream, nil); rec.Code != 200 {
					t.Fatalf("warm-up failed: %s", rec.Body.String())
				}
				before := make(map[string]map[[32]byte]promptCacheEntry)
				for account, entries := range h.promptCache.entriesByAccount {
					before[account] = make(map[[32]byte]promptCacheEntry)
					for fingerprint, entry := range entries {
						before[account][fingerprint] = entry
					}
				}
				if len(before) == 0 {
					t.Fatal("warm-up did not store cache")
				}
				fail.Store(true)
				rec := runtimeProtocolRequest(t, h, path, stream, nil)
				if rec.Code < 400 && !strings.Contains(rec.Body.String(), "error") {
					t.Fatalf("expected failed upstream request: %d %s", rec.Code, rec.Body.String())
				}
				if !reflect.DeepEqual(before, h.promptCache.entriesByAccount) {
					t.Fatal("failed upstream request renewed or created cache entries")
				}
			})
		}
	}
}
