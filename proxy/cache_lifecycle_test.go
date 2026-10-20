package proxy

import (
	"encoding/json"
	"kiro-proxy/config"
	"strings"
	"testing"
	"time"
)

func TestLifecycleToolCacheBlockCountsSerializedStructure(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-5.5",
		Tools: []ClaudeTool{{
			Name:        "read_file",
			Description: "Read a file from the workspace and return its contents.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Workspace-relative path"},
				},
				"required": []interface{}{"path"},
			},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: "Use the tool."}},
	}

	wireTool := map[string]interface{}{
		"name":         req.Tools[0].Name,
		"description":  req.Tools[0].Description,
		"input_schema": req.Tools[0].InputSchema,
	}
	want := estimateApproxTokens(canonicalizeCacheValue(wireTool))
	profile := buildLifecycleProfile(req)
	if profile.LifecycleToolTokens != want || profile.LifecycleSystemTokens != 0 || profile.LifecycleMessageTokens <= 0 {
		t.Fatalf("lifecycle token breakdown is inaccurate: tool=%d system=%d message=%d", profile.LifecycleToolTokens, profile.LifecycleSystemTokens, profile.LifecycleMessageTokens)
	}
	if profile.LifecycleToolTokens+profile.LifecycleSystemTokens+profile.LifecycleMessageTokens+claudeGenerationPrefixTokens != profile.TotalInputTokens {
		t.Fatalf("lifecycle token breakdown does not conserve total: %+v", profile)
	}
	if len(profile.Breakpoints) < 2 {
		t.Fatal("tool and message cache breakpoints were not created")
	}
	got := profile.Breakpoints[0].CumulativeTokens
	if got != want {
		t.Fatalf("tool cache block must count serialized structure: got %d want %d", got, want)
	}
	semanticOnly := estimateClaudeToolTokens(req.Tools[0])
	if got <= semanticOnly {
		t.Fatalf("serialized tool count did not include structural overhead: serialized=%d semantic=%d", got, semanticOnly)
	}
	tracker := newPromptCacheTracker(time.Hour)
	policy := lifecyclePolicy()
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if billedClaudeInputTokens(profile.TotalInputTokens, cold) != 2 || cold.CacheReadInputTokens != 0 {
		t.Fatalf("serialized counting changed cold framing or created a phantom read: %+v", cold)
	}
	tracker.UpdateWithSettings("account", profile, policy, cold)
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(profile.TotalInputTokens, warm) != 2 {
		t.Fatalf("serialized counting changed the write/read lifecycle: cold=%+v warm=%+v", cold, warm)
	}
	t.Logf("tool tokens: semantic=%d serialized=%d; cold input=2 create=%d; warm input=2 read=%d", semanticOnly, got, cold.CacheCreationInputTokens, warm.CacheReadInputTokens)
}

func TestLifecycleMessageCacheBlockCountsSerializedStructure(t *testing.T) {
	message := map[string]interface{}{"type": "text", "text": "stable prefix", "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "5m"}}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{message}}}}
	profile := buildLifecycleProfile(req)
	serialized := estimateApproxTokens(canonicalizeCacheValue(map[string]interface{}{"type": "text", "text": "stable prefix"})) + claudeMessageFramingTokens
	semantic := estimateClaudeValueTokens(message) + claudeMessageFramingTokens
	if profile.LifecycleMessageTokens != serialized || profile.LifecycleMessageTokens <= semantic {
		t.Fatalf("message structure was not counted: got=%d serialized=%d semantic=%d", profile.LifecycleMessageTokens, serialized, semantic)
	}
	if profile.TotalInputTokens != profile.LifecycleMessageTokens+claudeGenerationPrefixTokens {
		t.Fatalf("message token breakdown does not conserve total: %+v", profile)
	}
}

func TestLifecycleToolCountExcludesCacheMarkersAndIncludesServerToolOptions(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-opus-5.5", Tools: []ClaudeTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 3}}}
	plain := buildLifecycleProfile(req)
	raw, err := json.Marshal(req.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plain.Breakpoints[0].CumulativeTokens, estimateJSONTokens(req.Tools[0]); got != want {
		t.Fatalf("server tool options missing: got=%d want=%d wire=%s", got, want, raw)
	}
	req.Tools[0].CacheControl = map[string]interface{}{"type": "ephemeral", "ttl": "1h"}
	marked := buildLifecycleProfile(req)
	if marked.TotalInputTokens != plain.TotalInputTokens || marked.InputFingerprint != plain.InputFingerprint || marked.Breakpoints[0].TTL != time.Hour {
		t.Fatal("cache markers must change TTL without changing token content or prefix identity")
	}
}

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
	miss := tracker.ComputeWithSettings("account", next, settings)
	if miss.CacheReadInputTokens != 0 || miss.CacheCreationInputTokens != next.TotalInputTokens-claudeGenerationPrefixTokens || billedClaudeInputTokens(next.TotalInputTokens, miss) != claudeGenerationPrefixTokens {
		t.Fatalf("0%% read target must rewrite the full prefix without inflating input: %+v", miss)
	}
	tracker.UpdateWithSettings("account", next, settings, appended)
	if got := tracker.ComputeWithSettings("account", next, settings); got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens != next.TotalInputTokens-claudeGenerationPrefixTokens {
		t.Fatalf("0%% read target should continue to re-create the prefix: %+v", got)
	}
	settings.Ratio = 1
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
	wantPrefixTokens := estimateApproxTokens(canonicalizeCacheValue(map[string]interface{}{"type": "text", "text": "short"}))
	if u.CacheCreationInputTokens != wantPrefixTokens || u.CacheCreation1hInputTokens != u.CacheCreationInputTokens || billedClaudeInputTokens(p.TotalInputTokens, u) <= 2 {
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
