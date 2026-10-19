package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLegacyClaudeCacheKeepsSerializationOutsideSemanticBoundary(t *testing.T) {
	for _, text := range []string{"hello", strings.Repeat("stable context ", 2000)} {
		tracker := newPromptCacheTracker(time.Hour)
		req := &ClaudeRequest{Model: "claude-opus-5.5", Messages: []ClaudeMessage{{Role: "user", Content: text}}}
		total := estimateClaudeRequestInputTokens(req)
		if total <= estimateApproxTokens(text)+4 {
			t.Fatalf("message framing and assistant prefix must be counted exactly once: total=%d content=%d", total, estimateApproxTokens(text))
		}
		profile := tracker.BuildClaudeProfileWithSettings(req, total, totalCachePolicy(1))
		eligible := estimateApproxTokens(text) + 2
		ordinary := total - eligible
		cold := tracker.ComputeWithSettings("account", profile, totalCachePolicy(1))
		if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != eligible || billedClaudeInputTokens(total, cold) != ordinary {
			t.Fatalf("cold breakdown: %+v", cold)
		}
		tracker.UpdateWithSettings("account", profile, totalCachePolicy(1), cold)
		warm := tracker.ComputeWithSettings("account", profile, totalCachePolicy(1))
		if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(total, warm) != ordinary {
			t.Fatalf("warm breakdown: %+v", warm)
		}
		changed := *req
		changed.Messages = append(append([]ClaudeMessage{}, req.Messages...), ClaudeMessage{Role: "assistant", Content: "ok"}, ClaudeMessage{Role: "user", Content: "continue"})
		nextTotal := estimateClaudeRequestInputTokens(&changed)
		nextProfile := tracker.BuildClaudeProfileWithSettings(&changed, nextTotal, totalCachePolicy(1))
		next := tracker.ComputeWithSettings("account", nextProfile, totalCachePolicy(1))
		if next.CacheReadInputTokens != warm.CacheReadInputTokens || next.CacheCreationInputTokens != estimateApproxTokens("ok")+estimateApproxTokens("continue")+4 || billedClaudeInputTokens(nextTotal, next) != nextTotal-next.CacheReadInputTokens-next.CacheCreationInputTokens {
			t.Fatalf("append breakdown: %+v", next)
		}
	}
}

func TestTotalCacheColdAdmissionFallsBackToEarlierBoundary(t *testing.T) {
	model := "claude-opus-5.5"
	var admitted, rejected [32]byte
	for n := 0; n < 500; n++ {
		fp := [32]byte{}
		copy(fp[:], fmt.Sprint(n))
		if totalCacheAdmission(totalCacheAdmissionRatio(.9, model, fp), model, fp) {
			admitted = fp
		} else {
			rejected = fp
		}
		if admitted != ([32]byte{}) && rejected != ([32]byte{}) {
			break
		}
	}
	if admitted == ([32]byte{}) || rejected == ([32]byte{}) {
		t.Fatal("missing fixture")
	}
	profile := &promptCacheProfile{Model: model, TotalInputTokens: 10100, Breakpoints: []promptCacheBreakpoint{{Fingerprint: admitted, CumulativeTokens: 9800, TTL: defaultPromptCacheTTL, Position: 1}, {Fingerprint: rejected, CumulativeTokens: 10000, TTL: time.Hour, Position: 2}}}
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(.9)
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if cold.CacheCreationInputTokens != 9800 || cold.CacheReadInputTokens != 0 || cold.CacheCreation1hInputTokens != 0 || billedClaudeInputTokens(profile.TotalInputTokens, cold) != 300 {
		t.Fatalf("earlier admitted prefix lost: %+v", cold)
	}
	calibrated := scalePromptCacheUsage(cold, profile, 20200)
	if calibrated.CacheCreationInputTokens != 19600 || calibrated.CacheReadInputTokens != 0 {
		t.Fatalf("admission fallback lost during calibration: %+v", calibrated)
	}
	tracker.UpdateWithSettings("account", profile, policy, cold)
	if _, ok := tracker.entriesByAccount["account"][rejected]; ok {
		t.Fatal("unwritten rejected final boundary was stored")
	}
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens != 9800 || warm.CacheCreationInputTokens != 200 || warm.CacheCreation1hInputTokens != 200 {
		t.Fatalf("matched prefix must read and new tail write: %+v", warm)
	}
}
