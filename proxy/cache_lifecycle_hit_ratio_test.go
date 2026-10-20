package proxy

import (
	"fmt"
	"testing"
	"time"
)

func TestLifecycleReadHitRatioKeepsColdAndMissInputLow(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	settings := lifecyclePolicy()
	settings.Ratio = 0.9
	profile := buildLifecycleProfile(&ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   "stable system prefix",
		Messages: []ClaudeMessage{{Role: "user", Content: "question"}},
	})
	cacheable := profile.TotalInputTokens - claudeGenerationPrefixTokens

	cold := tracker.ComputeWithSettings("account", profile, settings)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != cacheable || billedClaudeInputTokens(profile.TotalInputTokens, cold) != claudeGenerationPrefixTokens {
		t.Fatalf("90%% target must not reject a cold prefix: total=%d usage=%+v", profile.TotalInputTokens, cold)
	}
	tracker.UpdateWithSettings("account", profile, settings, cold)

	const attempts = 10000
	reads, rewrites := 0, 0
	for i := 0; i < attempts; i++ {
		usage := tracker.ComputeWithSettings("account", profile, settings)
		ordinary := billedClaudeInputTokens(profile.TotalInputTokens, usage)
		if ordinary != claudeGenerationPrefixTokens {
			t.Fatalf("warm attempt %d inflated ordinary input to %d: %+v", i, ordinary, usage)
		}
		switch {
		case usage.CacheReadInputTokens == cacheable && usage.CacheCreationInputTokens == 0:
			reads++
		case usage.CacheReadInputTokens == 0 && usage.CacheCreationInputTokens == cacheable:
			rewrites++
		default:
			t.Fatalf("warm request was partially read or written: %+v", usage)
		}
		tracker.UpdateWithSettings("account", profile, settings, usage)
	}
	rate := float64(reads) / attempts
	if rate < 0.85 || rate > 0.95 || reads+rewrites != attempts {
		t.Fatalf("90%% read target must vary within 85-95%%, reads=%d rewrites=%d rate=%.4f", reads, rewrites, rate)
	}
	t.Logf("warm cache read rate %.2f%% (%d reads, %d simulated evictions)", rate*100, reads, rewrites)
}

func TestLifecycleReadHitRatioEndpoints(t *testing.T) {
	for _, ratio := range []float64{0, 1} {
		t.Run(fmt.Sprintf("ratio_%v", ratio), func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			settings := lifecyclePolicy()
			settings.Ratio = ratio
			profile := buildLifecycleProfile(&ClaudeRequest{Model: "future-model", System: "stable prefix"})
			cacheable := profile.TotalInputTokens - claudeGenerationPrefixTokens

			cold := tracker.ComputeWithSettings("account", profile, settings)
			if cold.CacheCreationInputTokens != cacheable || billedClaudeInputTokens(profile.TotalInputTokens, cold) != claudeGenerationPrefixTokens {
				t.Fatalf("cold prefix must be created independently of read target: %+v", cold)
			}
			tracker.UpdateWithSettings("account", profile, settings, cold)

			warm := tracker.ComputeWithSettings("account", profile, settings)
			if ratio == 0 {
				if warm.CacheReadInputTokens != 0 || warm.CacheCreationInputTokens != cacheable {
					t.Fatalf("0%% reads must recache without inflating ordinary input: %+v", warm)
				}
			} else if warm.CacheReadInputTokens != cacheable || warm.CacheCreationInputTokens != 0 {
				t.Fatalf("100%% reads must use the stored prefix: %+v", warm)
			}
			if billedClaudeInputTokens(profile.TotalInputTokens, warm) != claudeGenerationPrefixTokens {
				t.Fatalf("read target changed ordinary input: %+v", warm)
			}
		})
	}
}
