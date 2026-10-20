package proxy

import (
	"math"
	"testing"
	"time"
)

func TestLifecycleReadMissRewritesPrefixAndKeepsInputLow(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = 0
	p := buildLifecycleProfile(&ClaudeRequest{Model: "claude-sonnet-4.5", System: "short stable prefix", Messages: []ClaudeMessage{{Role: "user", Content: "question"}}})
	cacheable := p.TotalInputTokens - claudeGenerationPrefixTokens

	cold := tracker.ComputeWithSettings("a", p, s)
	if cold.CacheCreationInputTokens != cacheable || cold.CacheReadInputTokens != 0 || billedClaudeInputTokens(p.TotalInputTokens, cold) != claudeGenerationPrefixTokens {
		t.Fatalf("cold prefix must be written regardless of the read target: %+v", cold)
	}
	tracker.UpdateWithSettings("a", p, s, cold)
	miss := tracker.ComputeWithSettings("a", p, s)
	if miss.CacheReadInputTokens != 0 || miss.CacheCreationInputTokens != cacheable || miss.limitation != "cache_read_miss" || billedClaudeInputTokens(p.TotalInputTokens, miss) != claudeGenerationPrefixTokens {
		t.Fatalf("0%% read target must rewrite a warm prefix without inflating input: %+v", miss)
	}
	s.Ratio = 1
	warm := tracker.ComputeWithSettings("a", p, s)
	if warm.CacheReadInputTokens != cacheable || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("100%% read target must use the existing cache: %+v", warm)
	}
	tracker.UpdateWithSettings("a", p, s, warm)
	next := buildLifecycleProfile(&ClaudeRequest{Model: "claude-sonnet-4-5", System: "short stable prefix", Messages: []ClaudeMessage{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "new question"}}})
	appended := tracker.ComputeWithSettings("a", next, s)
	if appended.CacheReadInputTokens != cacheable || appended.CacheCreationInputTokens != next.TotalInputTokens-p.TotalInputTokens {
		t.Fatalf("suffix lifecycle was ratio-scaled: %+v", appended)
	}
	tracker.UpdateWithSettings("a", next, s, appended)
	repeat := tracker.ComputeWithSettings("a", next, s)
	if repeat.CacheReadInputTokens != appended.CacheReadInputTokens+appended.CacheCreationInputTokens || repeat.CacheCreationInputTokens != 0 {
		t.Fatalf("repeat lifecycle broken: %+v", repeat)
	}
}

func TestLifecycleReadHitDistributionAndEndpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = .9
	profile := buildLifecycleProfile(&ClaudeRequest{Model: "any-future-model", System: "stable prefix"})
	cacheable := profile.TotalInputTokens - claudeGenerationPrefixTokens
	cold := tracker.ComputeWithSettings("a", profile, s)
	if cold.CacheCreationInputTokens != cacheable {
		t.Fatalf("90%% target rejected cold prefix: %+v", cold)
	}
	tracker.UpdateWithSettings("a", profile, s, cold)

	const attempts = 10000
	hits, misses := 0, 0
	minRatio, maxRatio := 1.0, 0.0
	for i := 0; i < attempts; i++ {
		u := tracker.ComputeWithSettings("a", profile, s)
		if u.totalRatio < .85-1e-12 || u.totalRatio > .95+1e-12 {
			t.Fatalf("outside 90%% ±5pp: %+v", u)
		}
		minRatio, maxRatio = math.Min(minRatio, u.totalRatio), math.Max(maxRatio, u.totalRatio)
		if u.lifecycleReadHit {
			hits++
			if u.CacheReadInputTokens != cacheable || u.CacheCreationInputTokens != 0 {
				t.Fatalf("read hit did not use the whole saved prefix: %+v", u)
			}
		} else {
			misses++
			if u.CacheReadInputTokens != 0 || u.CacheCreationInputTokens != cacheable || u.limitation != "cache_read_miss" {
				t.Fatalf("read miss did not rewrite the whole prefix: %+v", u)
			}
		}
		if billedClaudeInputTokens(profile.TotalInputTokens, u) != claudeGenerationPrefixTokens {
			t.Fatalf("ratio inflated ordinary input at attempt %d: %+v", i, u)
		}
		tracker.UpdateWithSettings("a", profile, s, u)
	}
	if hits < 8700 || hits > 9300 || misses == 0 || minRatio > .86 || maxRatio < .94 {
		t.Fatalf("bad deterministic read-hit sample: hits=%d misses=%d range=%.4f..%.4f", hits, misses, minRatio, maxRatio)
	}
	t.Logf("warm read hits=%d/%d; effective target range=%.6f..%.6f", hits, attempts, minRatio, maxRatio)

	for _, ratio := range []float64{0, 1} {
		endpoint := newPromptCacheTracker(time.Hour)
		s.Ratio = ratio
		first := endpoint.ComputeWithSettings("a", profile, s)
		if first.CacheCreationInputTokens != cacheable {
			t.Fatalf("cold endpoint %v must create: %+v", ratio, first)
		}
		endpoint.UpdateWithSettings("a", profile, s, first)
		warm := endpoint.ComputeWithSettings("a", profile, s)
		wantRead, wantWrite := 0, cacheable
		if ratio == 1 {
			wantRead, wantWrite = cacheable, 0
		}
		if warm.CacheReadInputTokens != wantRead || warm.CacheCreationInputTokens != wantWrite || warm.totalRatio != ratio {
			t.Fatalf("warm endpoint %v: %+v", ratio, warm)
		}
	}
}

func TestLifecycleReadHitDiagnostic(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = 0
	p := buildLifecycleProfile(&ClaudeRequest{Model: "model", System: "prefix"})
	u := tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d := tracker.diagnostics[len(tracker.diagnostics)-1]
	if d.Rule != lifecycleCacheRuleVersion || d.ReadChecked || d.ReadHit || d.Write <= 0 || d.Ordinary != claudeGenerationPrefixTokens {
		t.Fatalf("cold write diagnostic is misleading: %+v", d)
	}
	u = tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d = tracker.diagnostics[len(tracker.diagnostics)-1]
	if !d.ReadChecked || d.ReadHit || d.Ratio != 0 || d.Read != 0 || d.Write <= 0 || d.Limitation != "cache_read_miss" {
		t.Fatalf("miss diagnostic: %+v", d)
	}
	s.Ratio = 1
	u = tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d = tracker.diagnostics[len(tracker.diagnostics)-1]
	if !d.ReadChecked || !d.ReadHit || d.Ratio != 1 || d.Read <= 0 || d.Write != 0 {
		t.Fatalf("hit diagnostic: %+v", d)
	}
}
