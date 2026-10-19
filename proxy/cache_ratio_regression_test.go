package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPromptCacheConfiguredRatioFluctuatesByInput(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 0.8}
	minRead, maxRead := 10000, 0
	for i := 0; i < 48; i++ {
		req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("Stable reference record. ", 900) + fmt.Sprint(i)}}}
		profile := tracker.BuildClaudeProfile(req, 10000)
		cold := tracker.ComputeWithSettings("account", profile, policy)
		if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens < int(float64(profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens)*.75) || cold.CacheCreationInputTokens > 8500 {
			t.Fatalf("cold request %d: %+v", i, cold)
		}
		tracker.Update("account", profile)
		warm := tracker.ComputeWithSettings("account", profile, policy)
		if warm.CacheCreationInputTokens != 0 || warm.CacheReadInputTokens < int(float64(profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens)*.75) || warm.CacheReadInputTokens > 8500 {
			t.Fatalf("warm request %d: %+v", i, warm)
		}
		if repeat := tracker.ComputeWithSettings("account", profile, policy); repeat != warm {
			t.Fatalf("same input changed: first=%+v repeat=%+v", warm, repeat)
		}
		minRead = minInt(minRead, warm.CacheReadInputTokens)
		maxRead = maxInt(maxRead, warm.CacheReadInputTokens)
	}
	if minRead >= 8000 || maxRead <= 8000 {
		t.Fatalf("expected variation on both sides of 80%%, observed %.2f%%–%.2f%%", float64(minRead)/100, float64(maxRead)/100)
	}
	t.Logf("configured 80%%, warm requests %.2f%%–%.2f%%", float64(minRead)/100, float64(maxRead)/100)
}

func TestPromptCacheRatioEndpointsAndNativeAccounting(t *testing.T) {
	for _, ratio := range []float64{0, 0.02, 0.8, 0.98, 1} {
		t.Run(fmt.Sprint(ratio), func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{7}, CumulativeTokens: 10000, TTL: time.Hour}}}
			policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: ratio}
			cold := tracker.ComputeWithSettings("account", profile, policy)
			tracker.Update("account", profile)
			warm := tracker.ComputeWithSettings("account", profile, policy)
			for _, original := range []promptCacheUsage{cold, warm} {
				usage := scalePromptCacheUsage(original, profile, 12000)
				mapped := buildClaudeUsageMap(12000, 137, usage, true)
				if mapped["input_tokens"].(int)+usage.CacheCreationInputTokens+usage.CacheReadInputTokens != 12000 || mapped["output_tokens"] != 137 {
					t.Fatalf("native totals changed: %v", mapped)
				}
				if usage.CacheCreationInputTokens < 0 || usage.CacheReadInputTokens < 0 || usage.CacheCreationInputTokens != usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens {
					t.Fatalf("invalid cache accounting: %+v", usage)
				}
				if ratio == 0 && usage.CacheCreationInputTokens+usage.CacheReadInputTokens != 0 {
					t.Fatalf("zero ratio must disable simulated tokens: %+v", usage)
				}
				if ratio == 1 && usage.CacheCreationInputTokens+usage.CacheReadInputTokens != 12000 {
					t.Fatalf("100%% ratio must use the full eligible prefix: %+v", usage)
				}
			}
		})
	}
}

func TestPromptCacheConfiguredRatioStillRequiresUnexpiredAccountPrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{9}, CumulativeTokens: 10000, TTL: time.Hour}}}
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 0.8}
	tracker.Update("first", profile)
	if warm := tracker.ComputeWithSettings("first", profile, policy); warm.CacheReadInputTokens <= 0 {
		t.Fatal("stored prefix did not hit")
	}
	if other := tracker.ComputeWithSettings("second", profile, policy); other.CacheReadInputTokens != 0 || other.CacheCreationInputTokens <= 0 {
		t.Fatalf("another account reused the prefix: %+v", other)
	}
	tracker.entriesByAccount["first"][[32]byte{9}] = promptCacheEntry{ExpiresAt: time.Now().Add(-time.Second), TTL: time.Hour}
	if expired := tracker.ComputeWithSettings("first", profile, policy); expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens <= 0 {
		t.Fatalf("expired prefix hit: %+v", expired)
	}
	short := cacheProfileWithInputTokens(profile, 500)
	if got := tracker.ComputeWithSettings("first", short, policy); got != (promptCacheUsage{}) {
		t.Fatalf("short input must bypass simulated cache: %+v", got)
	}
}

func TestPromptCacheConfiguredReferenceAndInputLengthBothMatter(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{6}, CumulativeTokens: 10000, TTL: time.Hour}}}
	tracker.Update("account", profile)
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 0.2}
	lower := tracker.ComputeWithSettings("account", profile, policy)
	policy.Ratio = 0.8
	higher := tracker.ComputeWithSettings("account", profile, policy)
	if higher.CacheReadInputTokens-lower.CacheReadInputTokens < 5999 || higher.CacheReadInputTokens-lower.CacheReadInputTokens > 6001 {
		t.Fatalf("configured reference not respected: 20%%=%+v 80%%=%+v", lower, higher)
	}
	baseRate := float64(higher.CacheReadInputTokens) / float64(profile.TotalInputTokens)
	different := false
	for total := 11000; total <= 15000; total += 1000 {
		changed := cacheProfileWithInputTokens(profile, total)
		read := tracker.ComputeWithSettings("account", changed, policy).CacheReadInputTokens
		rate := float64(read) / float64(total)
		if rate < 0.75 || rate > 0.85 {
			t.Fatalf("length-dependent rate out of bounds: %v", rate)
		}
		if math.Abs(rate-baseRate) > 0.001 {
			different = true
		}
	}
	if !different {
		t.Fatal("input length did not affect the simulated ratio")
	}
}

func TestPromptCacheAutomaticConversationReusesOnlyEarlierPrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 0.8}
	first := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("Stable reference record. ", 800)}}}
	tracker.Update("account", tracker.BuildClaudeProfile(first, estimateClaudeRequestInputTokens(first)))
	second := *first
	second.Messages = append(append([]ClaudeMessage(nil), first.Messages...), ClaudeMessage{Role: "assistant", Content: "OK"}, ClaudeMessage{Role: "user", Content: strings.Repeat("New input. ", 1800)})
	profile := tracker.BuildClaudeProfile(&second, estimateClaudeRequestInputTokens(&second))
	usage := tracker.ComputeWithSettings("account", profile, policy)
	if usage.CacheReadInputTokens <= 0 || usage.CacheCreationInputTokens <= 0 || usage.CacheReadInputTokens >= profile.TotalInputTokens*3/4 {
		t.Fatalf("must read the old prefix and write the new tail: total=%d usage=%+v", profile.TotalInputTokens, usage)
	}
	changed := second
	changed.Messages = append([]ClaudeMessage(nil), second.Messages...)
	changed.Messages[0].Content = "Changed prefix: " + changed.Messages[0].Content.(string)
	changedProfile := tracker.BuildClaudeProfile(&changed, estimateClaudeRequestInputTokens(&changed))
	if got := tracker.ComputeWithSettings("account", changedProfile, policy); got.CacheReadInputTokens != 0 {
		t.Fatalf("changed prefix must miss: %+v", got)
	}
}

func TestPromptCacheStreamStartUsesOneInputTokenBasis(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 137}))
	})
	enabled, mode, ratio := true, "uniform", 0.8
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "ratio-key", Key: "sk-ratio-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	for i := 0; i < 2; i++ {
		rec := runtimeProtocolRequest(t, h, "/v1/messages", true, &key)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		start := firstMessageUsage(t, rec.Body.String())
		end := runtimeProtocolUsage(t, rec, true)
		total := start["input_tokens"].(float64) + start["cache_creation_input_tokens"].(float64) + start["cache_read_input_tokens"].(float64)
		startCache := start["cache_creation_input_tokens"].(float64) + start["cache_read_input_tokens"].(float64)
		endCache := end["cache_creation_input_tokens"].(float64) + end["cache_read_input_tokens"].(float64)
		if math.Abs(startCache/total-endCache/total) > 2/total {
			t.Fatalf("stream start and completion mix token bases: start=%v final=%v", start, end)
		}
		if end["input_tokens"].(float64)+endCache != total || end["output_tokens"] != float64(137) {
			t.Fatalf("native totals changed: %v", end)
		}
	}
}

func firstMessageUsage(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "message_start" {
			return event["message"].(map[string]interface{})["usage"].(map[string]interface{})
		}
	}
	t.Fatal("message_start missing")
	return nil
}
