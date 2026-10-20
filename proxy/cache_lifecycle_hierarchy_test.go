package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestLifecycleToolChoiceOnlyInvalidatesMessagePrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	settings := lifecyclePolicy()
	req := &ClaudeRequest{
		Model:    "claude-opus-5.5",
		Tools:    []ClaudeTool{{Name: "lookup", Description: strings.Repeat("Stable tool definition. ", 30), InputSchema: map[string]interface{}{"type": "object"}}},
		System:   strings.Repeat("Stable system instructions. ", 30),
		Messages: []ClaudeMessage{{Role: "user", Content: "Look up the version."}},
	}
	first := buildLifecycleProfile(req)
	cold := tracker.ComputeWithSettings("account", first, settings)
	tracker.UpdateWithSettings("account", first, settings, cold)

	for _, choice := range []interface{}{map[string]interface{}{"type": "auto"}, map[string]interface{}{"type": "tool", "name": "lookup"}, map[string]interface{}{"type": "none"}} {
		req.ToolChoice = choice
		next := buildLifecycleProfile(req)
		usage := tracker.ComputeWithSettings("account", next, settings)
		wantRead := first.LifecycleToolTokens + first.LifecycleSystemTokens
		if usage.CacheReadInputTokens != wantRead || usage.CacheCreationInputTokens != first.LifecycleMessageTokens {
			t.Fatalf("tool_choice=%v must preserve tool/system prefix and rewrite messages: got read=%d create=%d want read=%d create=%d", choice, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, wantRead, first.LifecycleMessageTokens)
		}
		if next.TotalInputTokens != first.TotalInputTokens || billedClaudeInputTokens(next.TotalInputTokens, usage) != claudeGenerationPrefixTokens {
			t.Fatal("cache identity change must not change input metering")
		}
	}
}

func TestLifecycleToolChoiceWithoutStablePrefixDoesNotCreateRead(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	settings := lifecyclePolicy()
	req := &ClaudeRequest{Model: "claude-opus-5.5", Messages: []ClaudeMessage{{Role: "user", Content: "question"}}}
	profile := buildLifecycleProfile(req)
	tracker.UpdateWithSettings("account", profile, settings, tracker.ComputeWithSettings("account", profile, settings))
	req.ToolChoice = map[string]interface{}{"type": "none"}
	changed := buildLifecycleProfile(req)
	if usage := tracker.ComputeWithSettings("account", changed, settings); usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != changed.TotalInputTokens-claudeGenerationPrefixTokens {
		t.Fatalf("changed message settings cannot reuse old messages: %+v", usage)
	}
}
