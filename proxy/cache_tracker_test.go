package proxy

import (
	"kiro-go/config"
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

func TestPromptCacheTrackerAddsDefaultBreakpointWithoutCacheControl(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   strings.Repeat("system prompt ", 500),
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	profile := tracker.BuildClaudeProfile(req, 2000)
	if profile == nil {
		t.Fatal("expected default cache breakpoint")
	}
	tracker.Update("acct-default", profile)
	usage := tracker.ComputeWithSettings("acct-default", profile, config.PromptCacheSettings{
		Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1, CreationRatio: 1, ReadRatio: 1,
	})
	if usage.CacheReadInputTokens != profile.TotalInputTokens {
		t.Fatalf("read=%d, want full prefix %d", usage.CacheReadInputTokens, profile.TotalInputTokens)
	}
}

func TestPromptCacheTrackerDoesNotAddDefaultBreakpointWithoutMessageBlock(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: strings.Repeat("system prompt ", 500),
	}
	if profile := tracker.BuildClaudeProfile(req, 2000); profile != nil {
		t.Fatalf("expected no profile without a message block, got %+v", profile)
	}
}

func TestPromptCacheTrackerNormalizesLastBreakpointToTotalInput(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   strings.Repeat("system prompt ", 100),
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	profile := tracker.BuildClaudeProfile(req, 3000)
	if profile == nil || len(profile.Breakpoints) == 0 {
		t.Fatal("expected profile with breakpoint")
	}
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	if last.CumulativeTokens != profile.TotalInputTokens {
		t.Fatalf("last breakpoint=%d, total=%d", last.CumulativeTokens, profile.TotalInputTokens)
	}
}

func TestPromptCacheTrackerNormalizesExplicitBreakpointToTotalInput(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("cached system ", 200),
				"cache_control": map[string]interface{}{"type": "ephemeral"},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("uncached tail ", 100)}},
	}
	profile := tracker.BuildClaudeProfile(req, 5000)
	if profile == nil || len(profile.Breakpoints) == 0 {
		t.Fatal("expected profile with breakpoint")
	}
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	if last.CumulativeTokens != profile.TotalInputTokens {
		t.Fatalf("last breakpoint=%d, total=%d", last.CumulativeTokens, profile.TotalInputTokens)
	}
}

func TestPromptCacheTrackerLimitsLookbackToTenBreakpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var old [32]byte
	old[0] = 1
	first := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 12000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: old, CumulativeTokens: 2000, TTL: time.Hour}}}
	tracker.Update("acct-lookback", first)

	breakpoints := make([]promptCacheBreakpoint, 11)
	breakpoints[0] = first.Breakpoints[0]
	for i := 1; i < len(breakpoints); i++ {
		var fingerprint [32]byte
		fingerprint[0] = byte(i + 10)
		breakpoints[i] = promptCacheBreakpoint{Fingerprint: fingerprint, CumulativeTokens: 2000 + i*1000, TTL: time.Hour}
	}
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 12000, Breakpoints: breakpoints}
	usage := tracker.ComputeWithSettings("acct-lookback", profile, config.PromptCacheSettings{
		Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 1, CreationRatio: 1, ReadRatio: 1,
	})
	if usage.CacheReadInputTokens != 0 {
		t.Fatalf("read=%d, want no match outside ten-breakpoint lookback", usage.CacheReadInputTokens)
	}
}

func TestPromptCacheTrackerScalesCacheUsageByRatio(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   strings.Repeat("system prompt ", 500),
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	profile := tracker.BuildClaudeProfile(req, 2000)
	if profile == nil {
		t.Fatal("expected profile")
	}
	tracker.Update("acct-ratio", profile)
	usage := tracker.ComputeWithSettings("acct-ratio", profile, config.PromptCacheSettings{
		Enabled: true, Mode: config.PromptCacheModeUniform, Ratio: 0.5, CreationRatio: 1, ReadRatio: 1,
	})
	if usage.CacheReadInputTokens != profile.TotalInputTokens/2 {
		t.Fatalf("read=%d, want %d", usage.CacheReadInputTokens, profile.TotalInputTokens/2)
	}
	if usage.CacheCreationInputTokens != 0 {
		t.Fatalf("creation=%d, want 0", usage.CacheCreationInputTokens)
	}
}

func TestPromptCacheTrackerSupportsIndependentRatiosAndDisable(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   strings.Repeat("system prompt ", 500),
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	profile := tracker.BuildClaudeProfile(req, 2000)
	if profile == nil {
		t.Fatal("expected profile")
	}

	disabled := tracker.ComputeWithSettings("acct-independent", profile, config.PromptCacheSettings{
		Enabled: false, Mode: config.PromptCacheModeIndependent, CreationRatio: 0.25, ReadRatio: 0.75,
	})
	if disabled != (promptCacheUsage{}) {
		t.Fatalf("disabled policy should report no cache usage, got %+v", disabled)
	}

	first := tracker.ComputeWithSettings("acct-independent", profile, config.PromptCacheSettings{
		Enabled: true, Mode: config.PromptCacheModeIndependent, CreationRatio: 0.25, ReadRatio: 0.75,
	})
	if first.CacheCreationInputTokens != profile.TotalInputTokens/4 {
		t.Fatalf("creation=%d, want %d", first.CacheCreationInputTokens, profile.TotalInputTokens/4)
	}
	tracker.Update("acct-independent", profile)
	second := tracker.ComputeWithSettings("acct-independent", profile, config.PromptCacheSettings{
		Enabled: true, Mode: config.PromptCacheModeIndependent, CreationRatio: 0.25, ReadRatio: 0.75,
	})
	if second.CacheReadInputTokens != profile.TotalInputTokens*3/4 {
		t.Fatalf("read=%d, want %d", second.CacheReadInputTokens, profile.TotalInputTokens*3/4)
	}
}
