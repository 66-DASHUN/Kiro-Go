package proxy

import (
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"testing"
	"time"
)

func TestTotalCacheShortProtocolAccounting(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.5", "claude-opus-5.5", "claude-haiku-4.5"} {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, path, stream), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "seven"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 12, "outputTokens": 1, "stopReason": "end_turn"}))
					})
					if err := config.UpdatePromptCacheSettings(true, "prefix_total", 1, 1, 1); err != nil {
						t.Fatal(err)
					}
					body := map[string]interface{}{"model": model, "stream": stream, "max_tokens": 64}
					if path == "/v1/responses" {
						body["input"], body["store"] = "Reply with seven.", false
					} else {
						body["messages"] = []map[string]interface{}{{"role": "user", "content": "Reply with seven."}}
					}
					expectedInput := 12
					if path == "/v1/messages" {
						expectedInput = estimateApproxTokens("Reply with seven.") + 4
					}
					coldWrite := 0
					for round := 0; round < 3; round++ {
						if round == 2 {
							if err := config.UpdatePromptCacheSettings(true, "prefix_total", 0, 1, 0); err != nil {
								t.Fatal(err)
							}
						}
						usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, nil), stream)
						input, output, read, write := localUsageTotals(path, usage)
						if input != expectedInput || output != 1 || read+write > input || (round == 0 && (read != 0 || write < 1 || write > expectedInput)) || (round == 1 && (read != coldWrite || write != 0)) || (round == 2 && read+write != 0) {
							t.Fatalf("round %d: input/output/cache not conserved: %v", round, usage)
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

func TestTotalCacheShortInputLifecycle(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.5", "claude-opus-5.5", "claude-haiku-4.5"} {
		for _, tokens := range []int{1, 2, 3, 10, 100, 1023, 4095} {
			t.Run(fmt.Sprintf("%s/%d", model, tokens), func(t *testing.T) {
				tracker := newPromptCacheTracker(time.Hour)
				policy := totalCachePolicy(1)
				bp := promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: tokens, TTL: 5 * time.Minute}
				profile := &promptCacheProfile{Model: model, TotalInputTokens: tokens, Breakpoints: []promptCacheBreakpoint{bp}}
				cold := tracker.ComputeWithSettings("account", profile, policy)
				if cold.CacheCreationInputTokens != tokens || cold.CacheReadInputTokens != 0 {
					t.Fatalf("admitted cold prefix should write its full content: %+v", cold)
				}
				// A dispatch or failed request must not make a prefix readable.
				if again := tracker.ComputeWithSettings("account", profile, policy); again.CacheReadInputTokens != 0 {
					t.Fatal("uncommitted prefix was readable")
				}
				tracker.UpdateWithSettings("account", profile, policy, cold)
				warm := tracker.ComputeWithSettings("account", profile, policy)
				if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 {
					t.Fatalf("short repeat must read its committed write: %+v", warm)
				}
				profile.TotalInputTokens++
				profile.Breakpoints = append(profile.Breakpoints, promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: tokens + 1, TTL: 5 * time.Minute})
				next := tracker.ComputeWithSettings("account", profile, policy)
				if next.CacheReadInputTokens != cold.CacheCreationInputTokens || next.CacheCreationInputTokens != 1 || next.CacheCreation1hInputTokens != 0 || next.CacheCreation5mInputTokens != 1 {
					t.Fatalf("single new token should participate without rewriting old content: %+v", next)
				}
				if billedClaudeInputTokens(profile.TotalInputTokens, next)+next.CacheReadInputTokens+next.CacheCreationInputTokens != profile.TotalInputTokens {
					t.Fatal("input not conserved")
				}
				tracker.UpdateWithSettings("account", profile, policy, next)
				if zero := tracker.ComputeWithSettings("account", profile, totalCachePolicy(0)); zero.CacheReadInputTokens+zero.CacheCreationInputTokens != 0 {
					t.Fatalf("zero rate should disable a short warm prefix: %+v", zero)
				}
				for fp, entry := range tracker.entriesByAccount["account"] {
					entry.ExpiresAt = time.Now().Add(-time.Second)
					tracker.entriesByAccount["account"][fp] = entry
				}
				if expired := tracker.ComputeWithSettings("account", profile, policy); expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens <= 0 {
					t.Fatalf("expired short prefix must be rewritten: %+v", expired)
				}
			})
		}
	}
}

func TestTotalCacheShortIntegerAllocation(t *testing.T) {
	for _, tokens := range []int{1, 2, 3, 7, 19, 99} {
		for _, ratio := range []float64{0, .01, .2, .5, .8, .9, .95, 1} {
			profile := &promptCacheProfile{Model: "claude-opus-5.5", TotalInputTokens: tokens, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: tokens, TTL: time.Hour}}}
			got := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: ratio}, profile, tokens, 0)
			wantWrite := tokens
			if ratio <= 0 {
				wantWrite = 0
			}
			if got.CacheCreationInputTokens != wantWrite || got.CacheReadInputTokens != 0 || got.CacheCreation1hInputTokens != wantWrite || got.CacheCreation5mInputTokens != 0 || billedClaudeInputTokens(tokens, got)+wantWrite != tokens {
				t.Fatalf("short content did not preserve lifecycle allocation; tokens=%d ratio=%v: %+v want write=%d", tokens, ratio, got, wantWrite)
			}
		}
	}
}
