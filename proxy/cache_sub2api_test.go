package proxy

import (
	"kiro-proxy/config"
	"math"
	"testing"
	"time"
)

func sub2apiPolicy(r float64) config.PromptCacheSettings {
	return config.PromptCacheSettings{Enabled: true, Mode: "sub2api", Ratio: r}
}

func TestSub2APICacheColdWarmAppendAndScopes(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	settings := sub2apiPolicy(.9)
	req := &ClaudeRequest{Model: "claude-opus-5.5", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	first := tracker.BuildClaudeProfileWithSettings(req, 101, settings)
	cold := tracker.ComputeWithSettings("key-a/account", first, settings)
	if cold.CacheCreationInputTokens != 91 || cold.CacheReadInputTokens != 0 || billedClaudeInputTokens(101, cold) != 10 {
		t.Fatalf("cold: %+v", cold)
	}
	// No successful completion yet: dispatch must not create a hidden hit.
	if u := tracker.ComputeWithSettings("key-a/account", first, settings); u.CacheReadInputTokens != 0 {
		t.Fatal("failed/in-flight call populated cache")
	}
	tracker.UpdateWithSettings("key-a/account", first, settings, cold)
	warm := tracker.ComputeWithSettings("key-a/account", first, settings)
	if warm.CacheReadInputTokens != 91 || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("warm: %+v", warm)
	}
	if u := tracker.ComputeWithSettings("key-b/account", first, settings); u.CacheReadInputTokens != 0 {
		t.Fatal("cross-key cache hit")
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "hi"}, ClaudeMessage{Role: "user", Content: "more"})
	next := tracker.BuildClaudeProfileWithSettings(req, 211, settings)
	u := tracker.ComputeWithSettings("key-a/account", next, settings)
	matched := next.Breakpoints[0].CumulativeTokens
	if u.CacheReadInputTokens != int(math.Round(float64(matched)*.9)) || u.CacheCreationInputTokens != int(math.Round(float64(211-matched)*.9)) {
		t.Fatalf("append does not scale prefix/suffix independently: %+v matched=%d", u, matched)
	}
	if u.CacheCreation5mInputTokens+u.CacheCreation1hInputTokens != u.CacheCreationInputTokens {
		t.Fatal("TTL sum")
	}
	req.Model = "claude-sonnet-5.5"
	if u := tracker.ComputeWithSettings("key-a/account", tracker.BuildClaudeProfileWithSettings(req, 211, settings), settings); u.CacheReadInputTokens != 0 {
		t.Fatal("different model hit")
	}
	tracker.mu.Lock()
	for id, entries := range tracker.entriesByAccount {
		for fp, e := range entries {
			e.ExpiresAt = time.Now().Add(-time.Second)
			tracker.entriesByAccount[id][fp] = e
		}
	}
	tracker.mu.Unlock()
	if u := tracker.ComputeWithSettings("key-a/account", first, settings); u.CacheReadInputTokens != 0 {
		t.Fatal("expired prefix hit")
	}
}

func TestSub2APICacheExplicitTTLAndCalibration(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: []interface{}{map[string]interface{}{"type": "text", "text": "system", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}}}, Messages: []ClaudeMessage{{Role: "user", Content: "question"}}}
	for _, ratio := range []float64{0, .9, 1} {
		settings := sub2apiPolicy(ratio)
		profile := tracker.BuildClaudeProfileWithSettings(req, 101, settings)
		if n := len(profile.Breakpoints); n != 2 || profile.Breakpoints[n-1].CumulativeTokens != 101 || profile.Breakpoints[n-1].TTL != time.Hour {
			t.Fatalf("explicit TTL not propagated to final message: %+v", profile)
		}
		cold := tracker.ComputeWithSettings("cold", profile, settings)
		for _, total := range []int{0, 1, 101, 203} {
			u := scalePromptCacheUsage(cold, profile, total)
			want := int(math.Round(float64(total) * ratio))
			if u.CacheCreationInputTokens != want || u.CacheCreation1hInputTokens != want || u.CacheReadInputTokens != 0 || billedClaudeInputTokens(total, u)+u.CacheCreationInputTokens != total {
				t.Fatalf("ratio=%v total=%d got=%+v", ratio, total, u)
			}
			result := buildClaudeUsageMap(total, 7, u, true)
			if result["input_tokens"] != total-want || result["output_tokens"] != 7 {
				t.Fatalf("protocol usage: %+v", result)
			}
		}
	}
}

func TestSub2APICacheMixedTTLAndLookback(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := sub2apiPolicy(.9)
	profile := &promptCacheProfile{referenceMode: true, Model: "claude-sonnet-5.5", TotalInputTokens: 10000,
		Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 4000, TTL: time.Hour}, {Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: 5 * time.Minute}}}
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if cold.CacheCreation5mInputTokens != 5400 || cold.CacheCreation1hInputTokens != 3600 {
		t.Fatalf("mixed TTL: %+v", cold)
	}
	tracker.UpdateWithSettings("account", profile, policy, cold)
	for i := 3; i <= 12; i++ {
		profile.Breakpoints = append(profile.Breakpoints, promptCacheBreakpoint{Fingerprint: [32]byte{byte(i)}, CumulativeTokens: 10000 + i*100, TTL: 5 * time.Minute})
	}
	profile.TotalInputTokens = 11200
	// Eleven entries now follow the first prefix; it is outside the last ten.
	tracker.mu.Lock()
	delete(tracker.entriesByAccount["account"], [32]byte{2})
	tracker.mu.Unlock()
	u := tracker.ComputeWithSettings("account", profile, policy)
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("lookback exceeded ten boundaries: %+v", u)
	}
	zero := tracker.ComputeWithSettings("account", profile, sub2apiPolicy(0))
	if zero.limitation != "zero_rate" || zero.CacheReadInputTokens+zero.CacheCreationInputTokens != 0 {
		t.Fatalf("zero ratio diagnosis: %+v", zero)
	}
}
