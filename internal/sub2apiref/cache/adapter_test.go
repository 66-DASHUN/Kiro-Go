package cache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixtureProfile(t *testing.T, protocol string, body []byte, total int) *Profile {
	t.Helper()
	p, ok := BuildProfile(context.Background(), protocol, body, "claude-sonnet-4-6", total)
	if !ok {
		t.Fatal("reference fixture must be cacheable")
	}
	return p
}

func TestReferencePrepareCommitAndAccountIsolation(t *testing.T) {
	tracker := NewTracker()
	profile := fixtureProfile(t, "anthropic", kiroCacheRequestBody("deferred commit", false), 2000)
	policy := Policy{Enabled: true, Mode: "uniform", Ratio: 1}
	for range 2 {
		got := tracker.Prepare("55", profile, policy).Usage()
		if got == nil || got.CacheCreationInputTokens != 2000 || got.CacheReadInputTokens != 0 {
			t.Fatalf("cold prepare: %+v", got)
		}
	}
	tracker.Prepare("55", profile, policy).Commit()
	got := tracker.Prepare("55", profile, policy).Usage()
	if got == nil || got.CacheReadInputTokens != 2000 || got.CacheCreationInputTokens != 0 || got.InputTokens != 0 {
		t.Fatalf("committed prefix: %+v", got)
	}
	got = tracker.Prepare("other-account", profile, policy).Usage()
	if got == nil || got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens != 2000 {
		t.Fatalf("account isolation: %+v", got)
	}
}

func TestReferenceScalingAndTTL(t *testing.T) {
	for _, oneHour := range []bool{false, true} {
		p := fixtureProfile(t, "anthropic", kiroCacheRequestBody("independent ratios", oneHour), 2000)
		tracker := NewTracker()
		policy := Policy{Enabled: true, Mode: "independent", CreationRatio: 0.75, ReadRatio: 0.25}
		plan := tracker.Prepare("79", p, policy)
		got := plan.Usage()
		if got == nil || got.CacheCreationInputTokens != 1500 || got.InputTokens != 500 {
			t.Fatalf("cold: %+v", got)
		}
		if oneHour && (got.CacheCreation1hInputTokens != 1500 || got.CacheCreation5mInputTokens != 0) {
			t.Fatalf("1h: %+v", got)
		}
		if !oneHour && (got.CacheCreation5mInputTokens != 1500 || got.CacheCreation1hInputTokens != 0) {
			t.Fatalf("5m: %+v", got)
		}
		plan.Commit()
		got = tracker.Prepare("79", p, policy).Usage()
		if got == nil || got.CacheReadInputTokens != 500 || got.CacheCreationInputTokens != 0 || got.InputTokens != 1500 {
			t.Fatalf("warm: %+v", got)
		}
	}
	if a, b := scaleKiroCacheCreationTTLTokens(1001, 999, 1000, 0.5); a != 501 || b != 499 {
		t.Fatalf("reference rounded split: %d,%d", a, b)
	}
}

func TestReferencePrefixGrowthAndRenewal(t *testing.T) {
	tracker := NewTracker()
	policy := Policy{Enabled: true, Mode: "uniform", Ratio: 1}
	first := fixtureProfile(t, "anthropic", kiroCacheMultiMessageBody("cached prefix", "tail one"), 6000)
	second := fixtureProfile(t, "anthropic", kiroCacheMultiMessageBody("cached prefix", "tail two"), 6000)
	tracker.Prepare("6", first, policy).Commit()
	got := tracker.Prepare("6", second, policy).Usage()
	if got == nil || got.CacheReadInputTokens <= 0 || got.CacheReadInputTokens >= 6000 || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("partial hit: %+v", got)
	}
	nearExpiry := time.Now().Add(2 * time.Second)
	for fp, e := range tracker.entries["6"] {
		e.expiresAt = nearExpiry
		tracker.entries["6"][fp] = e
	}
	plan := tracker.Prepare("6", first, policy)
	// Reference compute renews one matching entry even before commit.
	renewed := 0
	for _, e := range tracker.entries["6"] {
		if e.expiresAt.After(nearExpiry.Add(time.Minute)) {
			renewed++
		}
	}
	if renewed != 1 {
		t.Fatalf("prepare should renew precisely the matched prefix: %d", renewed)
	}
	plan.Commit()
	for _, e := range tracker.entries["6"] {
		if !e.expiresAt.After(nearExpiry.Add(time.Minute)) {
			t.Fatal("commit must renew every current breakpoint")
		}
	}
	for fp, e := range tracker.entries["6"] {
		e.expiresAt = time.Now().Add(-time.Second)
		tracker.entries["6"][fp] = e
	}
	got = tracker.Prepare("6", first, policy).Usage()
	if got == nil || got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens != 6000 {
		t.Fatalf("expired: %+v", got)
	}
}

func TestReferenceMinimumBeforeScalingAndLastTTL(t *testing.T) {
	short := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"short"}]}`)
	if p, ok := BuildProfile(context.Background(), "anthropic", short, "claude-sonnet-4-6", 100000); ok || p != nil {
		t.Fatal("large supplied total must not make a short raw prefix cacheable")
	}
	if MinimumCacheableTokens("claude-opus-4-5-20251101-thinking") != 4096 || MinimumCacheableTokens("gpt-5.6-sol") != 1024 {
		t.Fatal("reference minimum changed")
	}
	var body map[string]any
	if err := json.Unmarshal(kiroCacheMultiMessageBody("mixed ttl", "tail"), &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	first["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
	last := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	p := fixtureProfile(t, "anthropic", raw, 6000)
	got := NewTracker().Prepare("mixed", p, Policy{Enabled: true, Mode: "uniform", Ratio: 1}).Usage()
	if got == nil || got.CacheCreation5mInputTokens != 6000 || got.CacheCreation1hInputTokens != 0 {
		t.Fatalf("reference classifies all creation by final TTL: %+v", got)
	}
}

func TestReferenceInputUsesSerializedMessages(t *testing.T) {
	ctx := context.Background()
	// The reference retains raw Anthropic arrays (66 JSON runes / 4 + 4 = 20),
	// while these OpenAI string inputs become Anthropic string content
	// (41 JSON runes / 4 + 4 = 14). The official fallback counts JSON structure.
	for protocol, body := range map[string]string{
		"anthropic":        `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`,
		"chat_completions": `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello world"}]}`,
		"responses":        `{"model":"claude-sonnet-4-6","input":"hello world"}`,
	} {
		got, err := InputTokens(ctx, protocol, []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
		want := 14
		if protocol == "anthropic" {
			want = 20
		}
		if got != want {
			t.Fatalf("%s serialized reference count = %d, want %d", protocol, got, want)
		}
	}
	got, err := InputTokens(ctx, "anthropic", []byte(`{"messages":[{"role":"user","content":"hello world"}]}`))
	if err != nil || got != 14 {
		t.Fatalf("raw Anthropic string must retain its JSON shape: %d, %v", got, err)
	}
}

func TestReferenceOpenAIStableHistoryFixtures(t *testing.T) {
	prefix := strings.Repeat("cached history prefix ", 600)
	for _, protocol := range []string{"chat_completions", "responses"} {
		body := kiroChatCompletionsConversationBody([]string{prefix, "tail one"})
		changed := kiroChatCompletionsConversationBody([]string{prefix, "tail two"})
		if protocol == "responses" {
			body = kiroResponsesConversationRequestBody("workspace", []string{prefix, "tail one"})
			changed = kiroResponsesConversationRequestBody("workspace", []string{prefix, "tail two"})
		}
		tracker := NewTracker()
		policy := Policy{Enabled: true, Mode: "uniform", Ratio: 1}
		tracker.Prepare("account", fixtureProfile(t, protocol, body, 6000), policy).Commit()
		got := tracker.Prepare("account", fixtureProfile(t, protocol, changed, 6000), policy).Usage()
		if got == nil || got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens <= 0 {
			t.Fatalf("%s stable prefix: %+v", protocol, got)
		}
	}
}

func TestReferenceTenBreakpointLookback(t *testing.T) {
	body := func(n int) []byte {
		messages := make([]any, n)
		for i := range messages {
			messages[i] = map[string]any{"role": "user", "content": strings.Repeat("x", 4096)}
		}
		out, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "messages": messages})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tracker := NewTracker()
	policy := Policy{Enabled: true, Mode: "uniform", Ratio: 1}
	tracker.Prepare("lookback", fixtureProfile(t, "anthropic", body(1), 1024), policy).Commit()
	within := tracker.Prepare("lookback", fixtureProfile(t, "anthropic", body(10), 10240), policy).Usage()
	beyond := tracker.Prepare("lookback", fixtureProfile(t, "anthropic", body(11), 11264), policy).Usage()
	if within == nil || within.CacheReadInputTokens != 1024 {
		t.Fatalf("tenth candidate: %+v", within)
	}
	if beyond == nil || beyond.CacheReadInputTokens != 0 {
		t.Fatalf("eleventh candidate must not be searched: %+v", beyond)
	}
}

func TestReferenceResponsesLegacyAndAdditionalTools(t *testing.T) {
	ctx := context.Background()
	for _, body := range []string{
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello world"}]}`,
		`{"model":"claude-sonnet-4-6","prompt":"hello world"}`,
	} {
		got, err := InputTokens(ctx, "responses", []byte(body))
		if err != nil || got != 14 {
			t.Fatalf("legacy Responses count: %d, %v", got, err)
		}
	}
	body := []byte(`{"model":"claude-sonnet-4-6","input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},{"type":"message","role":"user","content":"hello world"}]}`)
	got, err := InputTokens(ctx, "responses", body)
	if err != nil || got != 164 {
		t.Fatalf("lifted additional tool: %d, %v", got, err)
	}
}
