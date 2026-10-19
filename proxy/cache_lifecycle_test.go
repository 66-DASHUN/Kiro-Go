package proxy

import (
	"encoding/json"
	"kiro-proxy/config"
	"strings"
	"testing"
	"time"
)

func TestLifecycleOpenAIPreservesExplicitBoundaries(t *testing.T) {
	for _, role := range []string{"system", "user"} {
		t.Run(role, func(t *testing.T) {
			var req OpenAIRequest
			body := `{"model":"claude-sonnet-4.5","messages":[{"role":"` + role + `","content":[{"type":"text","text":"short prefix","cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"role":"user","content":"ordinary tail"}]}`
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			p := buildLifecycleProfile(lifecycleOpenAIRequest(&req))
			tracker := newPromptCacheTracker(time.Hour)
			u := tracker.ComputeWithSettings("a", p, lifecyclePolicy())
			if u.CacheCreation1hInputTokens <= 0 || u.CacheCreation5mInputTokens != 0 || billedClaudeInputTokens(p.TotalInputTokens, u) <= claudeGenerationPrefixTokens {
				t.Fatalf("explicit prefix or ordinary tail lost: total=%d usage=%+v", p.TotalInputTokens, u)
			}
		})
	}
}

func TestLifecycleOpenAIToolDefinitionsInvalidatePrefix(t *testing.T) {
	var req OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"question"}],"tools":[{"type":"function","function":{"name":"lookup","description":"old description","parameters":{"type":"object"}}}]}`), &req); err != nil {
		t.Fatal(err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	p := buildLifecycleProfile(lifecycleOpenAIRequest(&req))
	tracker.UpdateWithSettings("a", p, s, tracker.ComputeWithSettings("a", p, s))
	req.Tools[0].Function.Description = "new description"
	changed := buildLifecycleProfile(lifecycleOpenAIRequest(&req))
	if u := tracker.ComputeWithSettings("a", changed, s); u.CacheReadInputTokens != 0 {
		t.Fatalf("changed tool definition reused old content: %+v", u)
	}
}

func lifecyclePolicy() config.PromptCacheSettings {
	return config.PromptCacheSettings{Enabled: true, Mode: "prefix_lifecycle", Ratio: 1}
}

func TestLifecycleColdRepeatAppendAndStableCount(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	settings := lifecyclePolicy()
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: strings.Repeat("stable system ", 80), Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	first := tracker.BuildClaudeProfileWithSettings(req, 99999, settings)
	cold := tracker.ComputeWithSettings("account", first, settings)
	if cold.CacheCreationInputTokens == 0 || cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens >= first.TotalInputTokens {
		t.Fatalf("cold must create eligible content and retain only framing/tail: total=%d usage=%+v", first.TotalInputTokens, cold)
	}
	tracker.UpdateWithSettings("account", first, settings, cold)
	warm := tracker.ComputeWithSettings("account", first, settings)
	if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("warm must read entire saved prefix: %+v", warm)
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "a longer response"}, ClaudeMessage{Role: "user", Content: strings.Repeat("new question ", 30)})
	next := tracker.BuildClaudeProfileWithSettings(req, 1, settings)
	appended := tracker.ComputeWithSettings("account", next, settings)
	if appended.CacheReadInputTokens != cold.CacheCreationInputTokens || appended.CacheCreationInputTokens != next.TotalInputTokens-first.TotalInputTokens {
		t.Fatalf("appended must preserve old counts and write only new content: first=%d next=%d usage=%+v", first.TotalInputTokens, next.TotalInputTokens, appended)
	}
	if scaled := scalePromptCacheUsage(appended, next, 333333); scaled != appended {
		t.Fatal("completion must not rescale lifecycle snapshot")
	}
	if calibrated := cacheProfileWithInputTokens(next, 333333); calibrated.TotalInputTokens != next.TotalInputTokens {
		t.Fatal("completion changed lifecycle counting space")
	}
	settings.Ratio = 0
	if got := tracker.ComputeWithSettings("account", next, settings); got.CacheReadInputTokens != appended.CacheReadInputTokens || got.CacheCreationInputTokens != appended.CacheCreationInputTokens {
		t.Fatal("cold admission target changed an existing read/suffix lifecycle")
	}
	tracker.UpdateWithSettings("account", next, settings, appended)
	if got := tracker.ComputeWithSettings("account", next, settings); got.CacheReadInputTokens != appended.CacheReadInputTokens+appended.CacheCreationInputTokens || got.CacheCreationInputTokens != 0 {
		t.Fatalf("second append repeat: %+v", got)
	}
	req.Messages[0].Content = "changed hello"
	edited := tracker.BuildClaudeProfileWithSettings(req, 1, settings)
	got := tracker.ComputeWithSettings("account", edited, settings)
	if got.CacheReadInputTokens <= 0 || got.CacheReadInputTokens >= cold.CacheCreationInputTokens || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("edit should reuse only system prefix: %+v", got)
	}
}

func TestLifecycleShortExplicitTailAndExpiry(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := lifecyclePolicy()
	prefix := map[string]interface{}{"type": "text", "text": "short", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}}
	req := &ClaudeRequest{Model: "new-model-9.9", System: []interface{}{prefix}, Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("uncached tail ", 30)}}}
	p := tracker.BuildClaudeProfileWithSettings(req, 1, policy)
	u := tracker.ComputeWithSettings("a", p, policy)
	if u.CacheCreationInputTokens != estimateApproxTokens("short") || u.CacheCreation1hInputTokens != u.CacheCreationInputTokens || billedClaudeInputTokens(p.TotalInputTokens, u) <= 2 {
		t.Fatalf("short explicit prefix and ordinary tail: %+v total=%d", u, p.TotalInputTokens)
	}
	tracker.UpdateWithSettings("a", p, policy, u)
	// Expire all entries without relying on wall-clock sleeps.
	tracker.mu.Lock()
	for scope, entries := range tracker.entriesByAccount {
		for hash, e := range entries {
			e.ExpiresAt = time.Now().Add(-time.Second)
			tracker.entriesByAccount[scope][hash] = e
		}
	}
	tracker.mu.Unlock()
	expired := tracker.ComputeWithSettings("a", p, policy)
	if expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens != u.CacheCreationInputTokens {
		t.Fatalf("expired prefix should recache: %+v", expired)
	}
	policy.Enabled = false
	if off := tracker.ComputeWithSettings("a", p, policy); off.CacheCreationInputTokens+off.CacheReadInputTokens != 0 {
		t.Fatalf("disabled: %+v", off)
	}
}

func TestLifecycleIsolationConcurrentSnapshotsAndMixedTTL(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	s.APIKeyID = "key-one"
	block := func(text, ttl string) interface{} {
		return map[string]interface{}{"type": "text", "text": text, "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": ttl}}
	}
	req := &ClaudeRequest{Model: "claude-opus-5.5", System: []interface{}{block("long prefix", "1h"), block("short suffix", "5m")}, Messages: []ClaudeMessage{{Role: "user", Content: "tail"}}}
	p := buildLifecycleProfile(req)
	first := tracker.ComputeWithSettings("a", p, s)
	concurrent := tracker.ComputeWithSettings("a", p, s)
	if first.CacheCreation5mInputTokens <= 0 || first.CacheCreation1hInputTokens <= 0 || first.CacheCreation5mInputTokens+first.CacheCreation1hInputTokens != first.CacheCreationInputTokens {
		t.Fatalf("mixed TTL: %+v", first)
	}
	tracker.UpdateWithSettings("a", p, s, first)
	if concurrent.CacheReadInputTokens != 0 || scalePromptCacheUsage(concurrent, p, 1).CacheCreationInputTokens != first.CacheCreationInputTokens {
		t.Fatal("inflight cold snapshot changed")
	}
	other := s
	other.APIKeyID = "key-two"
	for name, usage := range map[string]promptCacheUsage{"key": tracker.ComputeWithSettings("a", p, other), "account": tracker.ComputeWithSettings("b", p, s)} {
		if usage.CacheReadInputTokens != 0 {
			t.Fatalf("%s leaked: %+v", name, usage)
		}
	}
	req.Model = "claude-sonnet-4.5"
	if u := tracker.ComputeWithSettings("a", buildLifecycleProfile(req), s); u.CacheReadInputTokens != 0 {
		t.Fatal("model scope leaked")
	}
	req.Model = "claude-opus-5-5"
	alias := buildLifecycleProfile(req)
	if u := tracker.ComputeWithSettings("a", alias, s); u.CacheReadInputTokens != first.CacheCreationInputTokens {
		t.Fatalf("alias failed to reuse: %+v", u)
	}
	tracker.mu.Lock()
	entries := tracker.entriesByAccount[lifecycleScope("a", p, s)]
	last := p.Breakpoints[len(p.Breakpoints)-1]
	e := entries[last.Fingerprint]
	e.ExpiresAt = time.Now().Add(-time.Second)
	entries[last.Fingerprint] = e
	tracker.mu.Unlock()
	u := tracker.ComputeWithSettings("a", alias, s)
	if u.CacheReadInputTokens != first.CacheCreation1hInputTokens || u.CacheCreationInputTokens != first.CacheCreation5mInputTokens || u.CacheCreation1hInputTokens != 0 {
		t.Fatalf("only short segment should expire: %+v", u)
	}
}

func TestLifecycleReadRenewsEarlierCheckpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: "unchanged system", Messages: []ClaudeMessage{{Role: "user", Content: "old question"}}}
	p := buildLifecycleProfile(req)
	tracker.UpdateWithSettings("a", p, s, tracker.ComputeWithSettings("a", p, s))
	ancestor := p.Breakpoints[0]
	tracker.mu.Lock()
	entries := tracker.entriesByAccount[lifecycleScope("a", p, s)]
	e := entries[ancestor.Fingerprint]
	e.ExpiresAt = time.Now().Add(time.Second)
	entries[ancestor.Fingerprint] = e
	tracker.mu.Unlock()
	u := tracker.ComputeWithSettings("a", p, s)
	if u.CacheReadInputTokens <= ancestor.CumulativeTokens {
		t.Fatalf("expected to read a longer checkpoint: %+v", u)
	}
	tracker.UpdateWithSettings("a", p, s, u)
	tracker.mu.Lock()
	renewed := entries[ancestor.Fingerprint].ExpiresAt
	tracker.mu.Unlock()
	if renewed.Before(time.Now().Add(4 * time.Minute)) {
		t.Fatal("reading the full prefix did not renew its earlier checkpoint")
	}
}

func TestLifecycleReadRefreshesDispatchSnapshotAfterExpiry(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	p := buildLifecycleProfile(&ClaudeRequest{Model: "claude-sonnet-4.5", System: "stable system", Messages: []ClaudeMessage{{Role: "user", Content: "question"}}})
	tracker.UpdateWithSettings("a", p, s, tracker.ComputeWithSettings("a", p, s))
	u := tracker.ComputeWithSettings("a", p, s)
	// Simulate the request completing after every checkpoint's wall-clock
	// expiry. The read succeeded from its immutable dispatch-time snapshot.
	tracker.mu.Lock()
	entries := tracker.entriesByAccount[lifecycleScope("a", p, s)]
	for hash, entry := range entries {
		entry.ExpiresAt = time.Now().Add(-time.Second)
		entries[hash] = entry
	}
	tracker.mu.Unlock()
	tracker.UpdateWithSettings("a", p, s, u)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, bp := range p.Breakpoints {
		entry := tracker.entriesByAccount[lifecycleScope("a", p, s)][bp.Fingerprint]
		if entry.ExpiresAt.Before(time.Now().Add(4 * time.Minute)) {
			t.Fatalf("successful dispatch read did not refresh checkpoint %d", bp.CumulativeTokens)
		}
	}
}

func TestLifecycleAppendCannotExtendShortLivedReadDependency(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	s := lifecyclePolicy()
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: []interface{}{
		map[string]interface{}{"type": "text", "text": "existing short-lived prefix", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "5m"}},
	}}
	p := buildLifecycleProfile(req)
	tracker.UpdateWithSettings("a", p, s, tracker.ComputeWithSettings("a", p, s))
	// Moving the marker must not silently upgrade the already-read 5m prefix
	// when only the new suffix is written at 1h.
	req.System = []interface{}{
		map[string]interface{}{"type": "text", "text": "existing short-lived prefix"},
		map[string]interface{}{"type": "text", "text": "new long-lived suffix", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}},
	}
	appended := buildLifecycleProfile(req)
	u := tracker.ComputeWithSettings("a", appended, s)
	if u.CacheReadInputTokens != p.Breakpoints[0].CumulativeTokens || u.CacheCreation1hInputTokens <= 0 {
		t.Fatalf("expected short-lived read and 1h suffix write: %+v", u)
	}
	tracker.UpdateWithSettings("a", appended, s, u)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	last := appended.Breakpoints[len(appended.Breakpoints)-1]
	entry := tracker.entriesByAccount[lifecycleScope("a", appended, s)][last.Fingerprint]
	if entry.TTL > 5*time.Minute || entry.ExpiresAt.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("new checkpoint outlived its read dependency: ttl=%v expiry=%v", entry.TTL, entry.ExpiresAt)
	}
}
