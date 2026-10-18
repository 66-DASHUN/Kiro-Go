package proxy

import (
	"testing"
	"time"
)

func stablePrefixFixture(total int, seed byte, breakpoints ...promptCacheBreakpoint) *promptCacheProfile {
	return &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: total, InputFingerprint: [32]byte{seed}, Breakpoints: breakpoints}
}

func TestStablePrefixAppendAlwaysAdmitsAfterMatch(t *testing.T) {
	for _, reference := range []float64{.95, .8, .2} {
		tracker := newPromptCacheTracker(time.Hour)
		old := promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}
		first := stablePrefixFixture(10000, 31, old)
		coldPolicy := totalCachePolicy(1)
		cold := tracker.ComputeWithSettings("account", first, coldPolicy)
		tracker.UpdateWithSettings("account", first, coldPolicy, cold)
		for seed := byte(32); seed < 40; seed++ {
			next := stablePrefixFixture(11000, seed, old, promptCacheBreakpoint{Fingerprint: [32]byte{seed}, CumulativeTokens: 11000, TTL: 5 * time.Minute})
			policy := totalCachePolicy(reference)
			got := tracker.ComputeWithSettings("account", next, policy)
			if got.CacheReadInputTokens != cold.CacheCreationInputTokens {
				t.Fatalf("reference %.2f seed %d read=%d, want the full live cached prefix %d", reference, seed, got.CacheReadInputTokens, cold.CacheCreationInputTokens)
			}
			if got.CacheCreationInputTokens != 1000 || got.CacheCreation5mInputTokens != 1000 || got.CacheCreation1hInputTokens != 0 || got.limitation != "" {
				t.Fatalf("a live prefix hit should write the new stable tail at reference %.2f, seed %d: %+v", reference, seed, got)
			}
			ordinary := billedClaudeInputTokens(11000, got)
			if ordinary != 0 || ordinary+got.CacheReadInputTokens+got.CacheCreationInputTokens != 11000 {
				t.Fatal("input is not conserved")
			}
		}
	}
}

func TestStablePrefixSuccessiveAppendsReadPriorCachedTotal(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	var breakpoints []promptCacheBreakpoint
	previousCache := 0
	for round := 0; round < 8; round++ {
		total := 10000 + 1000*round
		breakpoints = append(breakpoints, promptCacheBreakpoint{Fingerprint: [32]byte{byte(round + 1)}, CumulativeTokens: total, TTL: time.Hour})
		profile := stablePrefixFixture(total, byte(50+round), breakpoints...)
		usage := tracker.ComputeWithSettings("account", profile, policy)
		if usage.CacheReadInputTokens != previousCache {
			t.Fatalf("round %d read=%d, prior committed total=%d", round, usage.CacheReadInputTokens, previousCache)
		}
		previousCache = usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		tracker.UpdateWithSettings("account", profile, policy, usage)
		repeat := tracker.ComputeWithSettings("account", profile, policy)
		if repeat.CacheCreationInputTokens != 0 || repeat.CacheReadInputTokens != previousCache {
			t.Fatalf("round %d repeat: %+v", round, repeat)
		}
	}
}

func TestStablePrefixNativeCorrectionKeepsSavedShare(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	first := stablePrefixFixture(10000, 31, promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour})
	cold := tracker.ComputeWithSettings("account", first, policy)
	tracker.UpdateWithSettings("account", first, policy, cold)
	// The next dispatch underestimates every raw position by half. Completion
	// restores the true count; the saved cache share must not be doubled.
	next := stablePrefixFixture(5500, 47, promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 5000, TTL: time.Hour}, promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: 5500, TTL: time.Hour})
	snapshot := tracker.ComputeWithSettings("account", next, policy)
	final := scalePromptCacheUsage(snapshot, next, 11000)
	if final.CacheReadInputTokens != cold.CacheCreationInputTokens {
		t.Fatalf("native correction rescaled committed read: %+v, want %d", final, cold.CacheCreationInputTokens)
	}
	if final.CacheCreationInputTokens != 1000 {
		t.Fatalf("native correction did not isolate the uncached tail: %+v", final)
	}
}

func TestStablePrefixPartialExpiryRetainsAncestorShare(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	ancestor := promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 6000, TTL: time.Hour}
	full := promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: 5 * time.Minute}
	first := stablePrefixFixture(10000, 31, ancestor, full)
	cold := tracker.ComputeWithSettings("account", first, policy)
	tracker.UpdateWithSettings("account", first, policy, cold)
	ancestorOnly := stablePrefixFixture(6000, 90, ancestor)
	before := tracker.ComputeWithSettings("account", ancestorOnly, policy)
	wantAncestor := cold.CacheCreationInputTokens * 6000 / 10000
	if before.CacheReadInputTokens != wantAncestor || before.CacheCreationInputTokens != 0 {
		t.Fatalf("intermediate share changed: %+v, want %d", before, wantAncestor)
	}
	expired := tracker.entriesByAccount["account"][full.Fingerprint]
	expired.ExpiresAt = time.Now().Add(-time.Second)
	tracker.entriesByAccount["account"][full.Fingerprint] = expired
	next := stablePrefixFixture(11000, 77, ancestor, full, promptCacheBreakpoint{Fingerprint: [32]byte{3}, CumulativeTokens: 11000, TTL: 5 * time.Minute})
	got := tracker.ComputeWithSettings("account", next, policy)
	if got.CacheReadInputTokens != wantAncestor || got.finalMatched != 6000 || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("expired descendant lost live ancestor share: %+v", got)
	}
}

func TestStablePrefixDoesNotInventSharesFromLegacyEntries(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := stablePrefixFixture(10000, 31, promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour})
	tracker.Update("account", profile)
	got := tracker.ComputeWithSettings("account", profile, totalCachePolicy(1))
	if got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens == 0 {
		t.Fatalf("unmeasured legacy entry invented a cached share: %+v", got)
	}
}

func TestStablePrefixConcurrentCompletionsKeepFirstCommit(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := stablePrefixFixture(10000, 31, promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour})
	firstPolicy, secondPolicy := totalCachePolicy(1), totalCachePolicy(.2)
	first := tracker.ComputeWithSettings("account", profile, firstPolicy)
	second := tracker.ComputeWithSettings("account", profile, secondPolicy)
	tracker.UpdateWithSettings("account", profile, firstPolicy, first)
	tracker.UpdateWithSettings("account", profile, secondPolicy, second)
	warm := tracker.ComputeWithSettings("account", profile, secondPolicy)
	if first.CacheCreationInputTokens != 10000 || second.CacheReadInputTokens != 0 || second.CacheCreationInputTokens != 0 || warm.CacheReadInputTokens != 10000 || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(profile.TotalInputTokens, warm) != 0 {
		t.Fatalf("later completion replaced the committed share: first=%+v second=%+v warm=%+v", first, second, warm)
	}
}

func TestStablePrefixNativeClampAndExpiredSnapshotRenewal(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	profile := stablePrefixFixture(10000, 31, promptCacheBreakpoint{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour})
	cold := commitTotalCacheFixture(tracker, profile, policy)
	snapshot := tracker.ComputeWithSettings("account", profile, policy)
	clamped := scalePromptCacheUsage(snapshot, profile, 6000)
	if clamped.CacheReadInputTokens != 6000 || clamped.CacheCreationInputTokens != 0 || clamped.limitation != "matched_share_clamped" {
		t.Fatalf("saved share must fit authoritative native input: %+v", clamped)
	}
	// Pruning during the in-flight response must not erase the immutable
	// dispatch metadata or renew it using a recalibrated raw prefix count.
	entry := tracker.entriesByAccount["account"][[32]byte{1}]
	entry.ExpiresAt = time.Now().Add(-time.Second)
	tracker.entriesByAccount["account"][[32]byte{1}] = entry
	tracker.UpdateWithSettings("account", cacheProfileWithInputTokens(profile, 6000), policy, clamped)
	restored := tracker.entriesByAccount["account"][[32]byte{1}]
	if restored.PrefixTokens != 10000 || restored.CachedTokens != cold.CacheCreationInputTokens {
		t.Fatalf("renewal lost original accounting basis: %+v", restored)
	}
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens != cold.CacheCreationInputTokens {
		t.Fatalf("renewal changed subsequent original-size read: %+v", warm)
	}
}
