package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func prefixFixture() *promptCacheProfile {
	return &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10200, InputFingerprint: [32]byte{19}, Breakpoints: []promptCacheBreakpoint{
		{Fingerprint: [32]byte{11}, CumulativeTokens: 4000, TTL: 5 * time.Minute},
		{Fingerprint: [32]byte{19}, CumulativeTokens: 10000, TTL: time.Hour},
	}}
}

func TestPrefixColdWarmAndNativeAccounting(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := prefixFixture()
	policy := config.PromptCacheSettings{Enabled: true, Mode: "prefix", Ratio: .8}
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != 10000 || billedClaudeInputTokens(10200, cold) != 200 || cold.CacheCreation5mInputTokens != 4000 || cold.CacheCreation1hInputTokens != 6000 {
		t.Fatalf("cold request should write the eligible prefix: %+v", cold)
	}
	tracker.Update("account", profile)
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens < 7500 || warm.CacheReadInputTokens > 8500 || warm.CacheCreationInputTokens != 10000-warm.CacheReadInputTokens || billedClaudeInputTokens(10200, warm) != 200 || warm.CacheCreation5mInputTokens != 0 {
		t.Fatalf("warm request must keep uncached input independent of ratio: %+v", warm)
	}
	for _, original := range []promptCacheUsage{cold, warm} {
		for _, total := range []int{20400, 1800, 1045, 500} {
			usage := scalePromptCacheUsage(original, profile, total)
			eligible := int(float64(10000)*float64(total)/10200 + .5)
			if total == 500 {
				if usage != (promptCacheUsage{}) {
					t.Fatalf("native short input cached: %+v", usage)
				}
				continue
			}
			if usage.CacheReadInputTokens+usage.CacheCreationInputTokens != eligible || billedClaudeInputTokens(total, usage) != total-eligible || usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens || usage.CacheReadInputTokens > int(.95*float64(eligible)) {
				t.Fatalf("native total %d violated prefix accounting: %+v", total, usage)
			}
		}
	}
	t.Logf("cold: normal=200 read=0 write=%d; warm: normal=200 read=%d write=%d", cold.CacheCreationInputTokens, warm.CacheReadInputTokens, warm.CacheCreationInputTokens)
}

func TestPrefixVariationAndKeyPolicies(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	minimum, maximum := 10000, 0
	for i := 1; i <= 40; i++ {
		profile := prefixFixture()
		profile.InputFingerprint = [32]byte{byte(i)}
		tracker.Update("account", profile)
		policy := config.PromptCacheSettings{Enabled: true, Mode: "prefix", Ratio: .8}
		usage := tracker.ComputeWithSettings("account", profile, policy)
		if usage.CacheReadInputTokens < 7500 || usage.CacheReadInputTokens > 8500 || usage.CacheCreationInputTokens+usage.CacheReadInputTokens != 10000 {
			t.Fatalf("80%% reference ignored: %+v", usage)
		}
		if again := tracker.ComputeWithSettings("account", profile, policy); again != usage {
			t.Fatal("same input varied")
		}
		minimum, maximum = minInt(minimum, usage.CacheReadInputTokens), maxInt(maximum, usage.CacheReadInputTokens)
		for _, ratio := range []float64{.95, 1} {
			policy.Ratio = ratio
			capped := tracker.ComputeWithSettings("account", profile, policy)
			if capped.CacheReadInputTokens < 9000 || capped.CacheReadInputTokens > 9500 || capped.CacheCreationInputTokens != 10000-capped.CacheReadInputTokens {
				t.Fatalf("read cap ignored: %+v", capped)
			}
		}
		policy.Ratio = 0
		if zero := tracker.ComputeWithSettings("account", profile, policy); zero.CacheReadInputTokens != 0 || zero.CacheCreationInputTokens != 10000 {
			t.Fatalf("zero read reference must retain writes: %+v", zero)
		}
		policy.Enabled = false
		if disabled := tracker.ComputeWithSettings("account", profile, policy); disabled != (promptCacheUsage{}) {
			t.Fatal("disabled emulation produced cache")
		}
	}
	if minimum >= 8000 || maximum <= 8000 {
		t.Fatalf("80%% reference did not vary across inputs: %d..%d", minimum, maximum)
	}
	profile := prefixFixture()
	profile.TotalInputTokens = 500
	if short := tracker.ComputeWithSettings("account", profile, config.PromptCacheSettings{Enabled: true, Mode: "prefix", Ratio: .95}); short != (promptCacheUsage{}) {
		t.Fatal("short request cached")
	}
	t.Logf("80%% reference: observed %.2f%%..%.2f%%", float64(minimum)/100, float64(maximum)/100)
}

func TestPrefixPartialHitAndIsolation(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := prefixFixture()
	earlier := &promptCacheProfile{Model: profile.Model, TotalInputTokens: 4000, Breakpoints: profile.Breakpoints[:1]}
	tracker.Update("account", earlier)
	policy := config.PromptCacheSettings{Enabled: true, Mode: "prefix", Ratio: .8}
	partial := tracker.ComputeWithSettings("account", profile, policy)
	if partial.CacheReadInputTokens < 3000 || partial.CacheReadInputTokens > 3400 || partial.CacheCreationInputTokens != 10000-partial.CacheReadInputTokens || partial.CacheCreation1hInputTokens != 6000 || partial.CacheCreation5mInputTokens != 4000-partial.CacheReadInputTokens {
		t.Fatalf("partial prefix was scaled over new input: %+v", partial)
	}
	if other := tracker.ComputeWithSettings("other", profile, policy); other.CacheReadInputTokens != 0 || other.CacheCreationInputTokens != 10000 {
		t.Fatalf("another account reused cache: %+v", other)
	}
	tracker.entriesByAccount["account"][[32]byte{11}] = promptCacheEntry{ExpiresAt: time.Now().Add(-time.Second), TTL: 5 * time.Minute}
	if expired := tracker.ComputeWithSettings("account", profile, policy); expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens != 10000 {
		t.Fatalf("expired prefix reused: %+v", expired)
	}
}

func TestPrefixConcurrentComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := prefixFixture()
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModePrefix, Ratio: .8}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-start
			account := fmt.Sprintf("account-%d", worker%4)
			for request := 0; request < 64; request++ {
				snapshot := tracker.ComputeWithSettings(account, profile, policy)
				tracker.Update(account, profile)
				usage := scalePromptCacheUsage(snapshot, profile, 20400)
				if usage.CacheReadInputTokens+usage.CacheCreationInputTokens != 20000 || billedClaudeInputTokens(20400, usage) != 400 || usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens || usage.CacheReadInputTokens > 19000 {
					t.Errorf("concurrent request violated accounting: %+v", usage)
					return
				}
				if repeated := scalePromptCacheUsage(snapshot, profile, 20400); repeated != usage {
					t.Errorf("concurrent update changed dispatch snapshot: %+v != %+v", repeated, usage)
					return
				}
			}
		}(worker)
	}
	close(start)
	workers.Wait()
}

func TestPrefixClaudeCountsSemanticContentAtBreakpoint(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	system := strings.Repeat("stable system instructions ", 600)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: []interface{}{
		map[string]interface{}{"type": "text", "text": "x-anthropic-billing-header: cc_version=1; cch=abc;"},
		map[string]interface{}{"type": "text", "text": system, "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}},
	}, Tools: []ClaudeTool{{Name: "inspect", Description: "inspect a file", InputSchema: map[string]interface{}{"type": "object"}}}, Messages: []ClaudeMessage{{Role: "user", Content: "current uncached message"}}}
	total := estimateClaudeRequestInputTokens(req)
	eligible := estimateApproxTokens(system) + estimateApproxTokens("inspect") + estimateApproxTokens("inspect a file") + estimateJSONTokens(req.Tools[0].InputSchema)
	profile := tracker.BuildClaudeProfileWithSettings(req, total, config.PromptCacheSettings{Mode: "prefix"})
	if len(profile.Breakpoints) != 1 || profile.Breakpoints[0].CumulativeTokens != eligible {
		t.Fatalf("fingerprint wrappers changed breakpoint: %+v, wanted %d", profile, eligible)
	}
	cold := tracker.ComputeWithSettings("account", profile, config.PromptCacheSettings{Enabled: true, Mode: "prefix", Ratio: .95})
	uncachedUserTokens := total - eligible
	if cold.CacheCreationInputTokens != eligible || billedClaudeInputTokens(total, cold) != uncachedUserTokens {
		t.Fatalf("tail or billing header allocated into cache: %+v", cold)
	}
}

func TestPrefixOpenAICountsSemanticContent(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("current text ", 600))
	payload.ConversationState.History = []KiroHistoryMessage{{UserInputMessage: &KiroUserInputMessage{Content: strings.Repeat("previous input ", 500)}}}
	total := estimateKiroPayloadInputTokens(payload)
	profile := tracker.BuildOpenAIProfile(payload, total)
	if profile == nil || profile.Breakpoints[0].CumulativeTokens != 0 || profile.Breakpoints[1].CumulativeTokens != estimateApproxTokens(payload.ConversationState.History[0].UserInputMessage.Content) || profile.Breakpoints[2].CumulativeTokens != total {
		t.Fatalf("OpenAI metadata changed semantic boundaries: %+v", profile)
	}
}

func TestPrefixProtocolsJSONAndSSE(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 137}))
				})
				enabled, mode, ratio := true, "prefix", .95
				key, err := config.AddApiKey(config.ApiKeyEntry{ID: "prefix-mode", Key: "sk-prefix-mode-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
				if err != nil {
					t.Fatal(err)
				}
				h.pool.Reload()
				request := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
				messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("stable input content ", 1000)}}
				if path == "/v1/responses" {
					request["input"], request["store"] = messages, false
				} else {
					request["messages"] = messages
				}
				semanticInput := estimateApproxTokens(strings.Repeat("stable input content ", 1000))
				body, _ := json.Marshal(request)
				for round := 0; round < 2; round++ {
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
					var read, write, total, output float64
					if path == "/v1/messages" {
						read, _ = usage["cache_read_input_tokens"].(float64)
						write, _ = usage["cache_creation_input_tokens"].(float64)
						total = usage["input_tokens"].(float64) + read + write
						output = usage["output_tokens"].(float64)
					} else {
						field, input, out := "prompt_tokens_details", "prompt_tokens", "completion_tokens"
						if path == "/v1/responses" {
							field, input, out = "input_tokens_details", "input_tokens", "output_tokens"
						}
						details := usage[field].(map[string]interface{})
						read, _ = details["cached_tokens"].(float64)
						write, _ = details["cache_write_tokens"].(float64)
						total, output = usage[input].(float64), usage[out].(float64)
					}
					expectedTotal := 10000.0
					expectedCacheable := expectedTotal
					if path == "/v1/messages" {
						expectedTotal = float64(meteringTestInput(path, strings.Repeat("stable input content ", 1000)))
						expectedCacheable = float64(semanticInput + 2)
					}
					if total != expectedTotal || output != 137 || read+write != expectedCacheable || (round == 0 && read != 0) || (round == 1 && (read < expectedCacheable*.9 || read > expectedCacheable*.95 || write <= 0)) {
						t.Fatalf("round %d: inconsistent usage %v", round, usage)
					}
				}
			})
		}
	}
}
