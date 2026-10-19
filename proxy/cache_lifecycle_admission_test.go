package proxy

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestLifecycleAdmissionWholePrefixesAndRetry(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = 0
	p := buildLifecycleProfile(&ClaudeRequest{Model: "claude-sonnet-4.5", System: "short stable prefix", Messages: []ClaudeMessage{{Role: "user", Content: "question"}}})
	rejected := tracker.ComputeWithSettings("a", p, s)
	if rejected.CacheCreationInputTokens != 0 || rejected.CacheReadInputTokens != 0 || rejected.limitation != "cache_admission_miss" {
		t.Fatalf("0%% must reject a whole cold prefix: %+v", rejected)
	}
	tracker.UpdateWithSettings("a", p, s, rejected)
	s.Ratio = .9
	admitted, sawMiss := false, false
	for attempt := 0; attempt < 500; attempt++ {
		u := tracker.ComputeWithSettings("a", p, s)
		if u.totalRatio < .85-1e-12 || u.totalRatio > .95+1e-12 {
			t.Fatalf("outside 90%% ±5pp: %v", u.totalRatio)
		}
		if u.CacheReadInputTokens != 0 {
			t.Fatal("uncommitted attempt warmed cache")
		}
		if u.CacheCreationInputTokens == 0 {
			sawMiss = true
			tracker.UpdateWithSettings("a", p, s, u)
			continue
		}
		if !sawMiss {
			// Keep dispatches cold until a .9 rejection has actually occurred,
			// then verify that a subsequent attempt admits that same prefix.
			continue
		}
		if u.CacheCreationInputTokens != p.TotalInputTokens-claudeGenerationPrefixTokens {
			t.Fatalf("partial prefix write: %+v", u)
		}
		tracker.UpdateWithSettings("a", p, s, u)
		admitted = true
		break
	}
	if !sawMiss || !admitted {
		t.Fatal("cold prefix could never be admitted on retry")
	}
	// Setting 0 prevents new cold admissions, without destroying genuine reads
	// or the normal read-old/write-new lifecycle of an admitted conversation.
	s.Ratio = 0
	warm := tracker.ComputeWithSettings("a", p, s)
	if warm.CacheReadInputTokens != p.TotalInputTokens-claudeGenerationPrefixTokens || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("warm read scaled by new target: %+v", warm)
	}
	next := buildLifecycleProfile(&ClaudeRequest{Model: "claude-sonnet-4-5", System: "short stable prefix", Messages: []ClaudeMessage{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "new question"}}})
	appended := tracker.ComputeWithSettings("a", next, s)
	if appended.CacheReadInputTokens != warm.CacheReadInputTokens || appended.CacheCreationInputTokens != next.TotalInputTokens-p.TotalInputTokens {
		t.Fatalf("suffix lifecycle was ratio-scaled: %+v", appended)
	}
	tracker.UpdateWithSettings("a", next, s, appended)
	repeat := tracker.ComputeWithSettings("a", next, s)
	if repeat.CacheReadInputTokens != appended.CacheReadInputTokens+appended.CacheCreationInputTokens || repeat.CacheCreationInputTokens != 0 {
		t.Fatalf("repeat lifecycle broken: %+v", repeat)
	}
}

func TestLifecycleAdmissionDistributionAndEndpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = .9
	const attempts = 10000
	accepted, rejected := 0, 0
	minRatio, maxRatio := 1.0, 0.0
	for i := 0; i < attempts; i++ {
		p := buildLifecycleProfile(&ClaudeRequest{Model: "any-future-model", System: fmt.Sprintf("prefix %d", i)})
		u := tracker.ComputeWithSettings("a", p, s)
		if u.totalRatio < .85-1e-12 || u.totalRatio > .95+1e-12 {
			t.Fatalf("out of range: %+v", u)
		}
		minRatio, maxRatio = math.Min(minRatio, u.totalRatio), math.Max(maxRatio, u.totalRatio)
		if u.CacheCreationInputTokens == 0 {
			rejected++
		} else {
			accepted++
			if u.CacheCreationInputTokens != p.TotalInputTokens-claudeGenerationPrefixTokens {
				t.Fatalf("ratio sliced tokens: %+v", u)
			}
		}
	}
	if accepted < 8700 || accepted > 9300 || rejected == 0 || minRatio > .86 || maxRatio < .94 {
		t.Fatalf("bad deterministic sample distribution: accepted=%d rejected=%d range=%v..%v", accepted, rejected, minRatio, maxRatio)
	}
	t.Logf("cold admission: accepted=%d/%d, probability range=%.6f..%.6f", accepted, attempts, minRatio, maxRatio)
	p := buildLifecycleProfile(&ClaudeRequest{Model: "model", System: "endpoint"})
	for _, ratio := range []float64{0, 1} {
		s.Ratio = ratio
		for i := 0; i < 20; i++ {
			u := tracker.ComputeWithSettings("a", p, s)
			want := 0
			if ratio == 1 {
				want = p.TotalInputTokens - claudeGenerationPrefixTokens
			}
			if u.CacheCreationInputTokens != want || u.totalRatio != ratio {
				t.Fatalf("endpoint %v: %+v", ratio, u)
			}
		}
	}
}

func TestLifecycleAdmissionDiagnostic(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.Ratio = 0
	p := buildLifecycleProfile(&ClaudeRequest{Model: "model", System: "prefix"})
	u := tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d := tracker.diagnostics[len(tracker.diagnostics)-1]
	if d.Rule != "prefix-lifecycle-v2" || !d.AdmissionChecked || d.WriteAdmitted || d.Limitation != "cache_admission_miss" || d.Ordinary != p.TotalInputTokens {
		t.Fatalf("rejected cold diagnostic is misleading: %+v", d)
	}
	s.Ratio = 1
	u = tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d = tracker.diagnostics[len(tracker.diagnostics)-1]
	if !d.AdmissionChecked || !d.WriteAdmitted || d.Ratio != 1 || d.Write <= 0 {
		t.Fatalf("accepted cold diagnostic: %+v", d)
	}
	u = tracker.ComputeWithSettings("a", p, s)
	tracker.UpdateWithSettings("a", p, s, u)
	d = tracker.diagnostics[len(tracker.diagnostics)-1]
	if d.AdmissionChecked || d.Read <= 0 || d.Write != 0 {
		t.Fatalf("warm diagnostic invented an admission check: %+v", d)
	}
}
