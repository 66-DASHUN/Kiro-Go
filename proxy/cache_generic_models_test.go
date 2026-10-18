package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenericCacheModelLifecycle(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "qwen3-coder-next", "deepseek-3.2", "minimax-m2.5", "glm-5", "kimi-k3", "future/new-family.v1", "claude-fable-9"} {
		for _, protocol := range []string{"claude", "openai"} {
			for _, total := range []int{12, 5000} {
				t.Run(fmt.Sprintf("%s/%s/%d", model, protocol, total), func(t *testing.T) {
					tracker := newPromptCacheTracker(time.Hour)
					policy := totalCachePolicy(1)
					build := func(tail string, tokens int) *promptCacheProfile {
						if protocol == "claude" {
							return tracker.BuildClaudeProfileWithSettings(&ClaudeRequest{Model: model, Messages: []ClaudeMessage{{Role: "user", Content: "stable first"}, {Role: "assistant", Content: "ok"}, {Role: "user", Content: tail}}}, tokens, policy)
						}
						payload := estimatedOpenAIPayload(tail)
						payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
						return tracker.BuildOpenAIProfileWithSettings(payload, tokens, policy)
					}
					profile := build("stable tail", total)
					cold := tracker.ComputeWithSettings("a", profile, policy)
					if cold.CacheCreationInputTokens <= 0 || cold.CacheReadInputTokens != 0 {
						t.Fatalf("new model should write on successful completion: %+v", cold)
					}
					if len(tracker.entriesByAccount["a"]) != 0 {
						t.Fatal("dispatch committed an uncertified prefix")
					}
					tracker.UpdateWithSettings("a", profile, policy, cold)
					warm := tracker.ComputeWithSettings("a", profile, policy)
					if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 {
						t.Fatalf("repeat did not read its committed share: %+v", warm)
					}
					if other := tracker.ComputeWithSettings("b", profile, policy); other.CacheReadInputTokens != 0 {
						t.Fatal("different account reused a prefix")
					}
					if zero := tracker.ComputeWithSettings("a", profile, totalCachePolicy(0)); zero.CacheReadInputTokens+zero.CacheCreationInputTokens != 0 {
						t.Fatal("zero policy did not disable simulation")
					}
					// Appending one turn must read earlier content and write the new suffix.
					next := *profile
					next.TotalInputTokens += 10
					next.Breakpoints = append(append([]promptCacheBreakpoint(nil), profile.Breakpoints...), promptCacheBreakpoint{Fingerprint: [32]byte{99}, CumulativeTokens: total + 10, TTL: time.Hour})
					next.LookupCandidates = nil
					added := tracker.ComputeWithSettings("a", &next, policy)
					if added.CacheReadInputTokens != cold.CacheCreationInputTokens || added.CacheCreationInputTokens <= 0 || added.CacheCreation1hInputTokens != added.CacheCreationInputTokens || added.CacheCreation5mInputTokens != 0 || billedClaudeInputTokens(next.TotalInputTokens, added)+added.CacheReadInputTokens+added.CacheCreationInputTokens != next.TotalInputTokens {
						t.Fatalf("append or conservation failed: %+v", added)
					}
					for fingerprint, entry := range tracker.entriesByAccount["a"] {
						entry.ExpiresAt = time.Now().Add(-time.Second)
						tracker.entriesByAccount["a"][fingerprint] = entry
					}
					if expired := tracker.ComputeWithSettings("a", profile, policy); expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens <= 0 {
						t.Fatal("expired model prefix was reused")
					}
				})
			}
		}
	}
}

func TestGenericCacheModelIdentityDoesNotMergeProviderIDs(t *testing.T) {
	for _, protocol := range []string{"claude", "openai"} {
		for _, pair := range [][2]string{{"future.model.v1", "future-model-v1"}, {"anthropic.future-v1", "future-v1"}, {"future/v1.2", "future/v1-2"}, {"future-v1", "future-v2"}} {
			t.Run(protocol+"/"+pair[0], func(t *testing.T) {
				tracker := newPromptCacheTracker(time.Hour)
				policy := totalCachePolicy(1)
				build := func(model string) *promptCacheProfile {
					if protocol == "claude" {
						return tracker.BuildClaudeProfileWithSettings(&ClaudeRequest{Model: model, Messages: []ClaudeMessage{{Role: "user", Content: "stable input"}}}, 12, policy)
					}
					payload := testKiroPayload()
					payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
					return tracker.BuildOpenAIProfileWithSettings(payload, 12, policy)
				}
				first, second := build(pair[0]), build(pair[1])
				if first.InputFingerprint == second.InputFingerprint {
					t.Fatal("distinct provider model identifiers share a fingerprint")
				}
				cold := tracker.ComputeWithSettings("a", first, policy)
				tracker.UpdateWithSettings("a", first, policy, cold)
				if got := tracker.ComputeWithSettings("a", second, policy); got.CacheReadInputTokens != 0 {
					t.Fatal("another model reused the prefix")
				}
			})
		}
	}
}

func TestGenericCacheEveryProtocolUsesEffectiveModelAndKeyPolicy(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "qwen3-coder-next", "future/new-family.v1"} {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%v", model, path, stream), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						var payload KiroPayload
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
							return
						}
						if got := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID; got != model {
							t.Errorf("upstream model %q, want %q", got, model)
						}
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "seven"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 12, "outputTokens": 1, "stopReason": "end_turn"}))
					})
					if err := config.UpdateModelMappings([]config.ModelMappingRule{{Key: "my-alias", Value: model}}); err != nil {
						t.Fatal(err)
					}
					if err := config.UpdatePromptCacheSettings(true, "prefix_total", 1, 1, 1); err != nil {
						t.Fatal(err)
					}
					h.pool.SetModelList("only", []string{model})
					t.Cleanup(func() { h.pool.SetModelList("only", nil) })
					zero, mode := 0.0, "prefix_total"
					key, err := config.AddApiKey(config.ApiKeyEntry{ID: "zero", Key: "sk-generic-zero", Enabled: true, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &zero})
					if err != nil {
						t.Fatal(err)
					}
					if err := config.UpdateAccountApiKeyID("only", key.ID); err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					required := true
					if err := config.UpdateSettingsPatch(&required, ""); err != nil {
						t.Fatal(err)
					}
					body := map[string]interface{}{"model": "my-alias", "stream": stream, "max_tokens": 64}
					if path == "/v1/responses" {
						body["input"], body["store"] = "Reply with seven.", false
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": "Reply with seven."}}
					}
					usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
					_, _, read, write := localUsageTotals(path, usage)
					if read+write != 0 || len(h.promptCache.entriesByAccount["only"]) != 0 {
						t.Fatal("per-key zero override stored a hidden prefix")
					}
					if err := config.UpdateAccountApiKeyID("only", ""); err != nil {
						t.Fatal(err)
					}
					required = false
					if err := config.UpdateSettingsPatch(&required, ""); err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					coldWrite := 0
					for round := 0; round < 2; round++ {
						if round == 1 {
							body["model"] = model
						}
						usage = runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), stream)
						input, output, read, write := localUsageTotals(path, usage)
						expectedInput := 12
						if path == "/v1/messages" {
							expectedInput = estimateApproxTokens("Reply with seven.") + 4
						}
						if input != expectedInput || output != 1 || read+write > input || (round == 0 && (write <= 0 || read != 0)) || (round == 1 && (read != coldWrite || write != 0)) {
							t.Fatalf("round %d: %v", round, usage)
						}
						if round == 0 {
							coldWrite = write
						}
					}
				})
			}
		}
	}
}

func TestGenericCacheEveryProtocolRejectsUnavailableModelsWithoutStorage(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, knownUnavailable := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%v/listed=%v", path, stream, knownUnavailable), func(t *testing.T) {
					calls := 0
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						calls++
						http.Error(w, `{"message":"model not found"}`, 400)
					})
					if err := config.UpdateModelMappings([]config.ModelMappingRule{}); err != nil {
						t.Fatal(err)
					}
					if err := config.UpdatePromptCacheSettings(true, "prefix_total", 1, 1, 1); err != nil {
						t.Fatal(err)
					}
					if knownUnavailable {
						h.pool.SetModelList("only", []string{"another-model"})
					} else {
						h.pool.SetModelList("only", nil)
					}
					t.Cleanup(func() { h.pool.SetModelList("only", nil) })
					body := map[string]interface{}{"model": "future/unavailable-model", "stream": stream, "max_tokens": 64}
					if path == "/v1/responses" {
						body["input"], body["store"] = "hello", false
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": "hello"}}
					}
					raw, _ := json.Marshal(body)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(string(raw))))
					if rec.Code == 200 || len(h.promptCache.entriesByAccount["only"]) > 0 || len(h.promptCache.diagnostics) > 0 || (knownUnavailable && calls > 0) || (!knownUnavailable && calls == 0) {
						t.Fatalf("failed model returned success or stored usage: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
					}
				})
			}
		}
	}
}
