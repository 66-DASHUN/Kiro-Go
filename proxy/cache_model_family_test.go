package proxy

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTotalCacheFamilyLifecycle(t *testing.T) {
	models := map[string]int{
		"claude-sonnet-4": 1024, "claude-sonnet-4.5": 1024,
		"claude-sonnet-4.6": 1024, "claude-sonnet-5": 1024, "claude-sonnet-5.5": 1024,
		"claude-opus-4.5": 4096, "claude-opus-4.6": 4096, "claude-opus-4.7": 4096,
		"claude-opus-4.8": 4096, "claude-opus-5": 4096, "claude-opus-5.5": 4096,
		"claude-haiku-4.5": 4096, "claude-sonnet-6.1": 1024, "claude-haiku-6.2": 4096,
		"claude-opus-6.3": 4096, "claude-opus-5.5-20261001": 4096,
		"claude-sonnet-4.6-latest-thinking": 1024,
	}
	for model, floor := range models {
		for _, alias := range []string{model, strings.ReplaceAll(model, ".", "-"), " ANTHROPIC." + strings.ToUpper(model) + " "} {
			t.Run(alias, func(t *testing.T) {
				tracker := newPromptCacheTracker(time.Hour)
				policy := totalCachePolicy(.9)
				profile := &promptCacheProfile{Model: alias, TotalInputTokens: floor - 1, InputFingerprint: [32]byte{33}, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: floor - 1, TTL: 5 * time.Minute}}}
				short := tracker.ComputeWithSettings("account", profile, policy)
				if short.CacheReadInputTokens+short.CacheCreationInputTokens != 0 || short.limitation != "below_model_minimum" {
					t.Fatalf("short prefix should fail only the simulation minimum: %+v", short)
				}
				tracker.UpdateWithSettings("account", profile, policy, short)
				if len(tracker.entriesByAccount["account"]) != 0 {
					t.Fatal("short input created hidden cache")
				}
				profile.TotalInputTokens, profile.Breakpoints[0].CumulativeTokens = floor, floor
				cold := tracker.ComputeWithSettings("account", profile, policy)
				if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens <= 0 || cold.CacheCreationInputTokens > floor || cold.limitation != "" {
					t.Fatalf("eligible family lost its first write: %+v", cold)
				}
				tracker.UpdateWithSettings("account", profile, policy, cold)
				warm := tracker.ComputeWithSettings("account", profile, policy)
				if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 {
					t.Fatalf("repeat must read the saved share: %+v", warm)
				}
				profile.TotalInputTokens += 1000
				profile.Breakpoints = append(profile.Breakpoints, promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: profile.TotalInputTokens, TTL: time.Hour})
				appended := tracker.ComputeWithSettings("account", profile, policy)
				wantWrite := int(math.Floor(1000 * variedPrefixReadRatio(.9, profile)))
				if appended.CacheReadInputTokens != cold.CacheCreationInputTokens || appended.CacheCreationInputTokens != wantWrite || appended.CacheCreation1hInputTokens != wantWrite || appended.CacheCreation5mInputTokens != 0 {
					t.Fatalf("append should only write its new interval: %+v", appended)
				}
				if billedClaudeInputTokens(profile.TotalInputTokens, appended)+appended.CacheReadInputTokens+appended.CacheCreationInputTokens != profile.TotalInputTokens {
					t.Fatal("input accounting not conserved")
				}
			})
		}
	}
}

func TestTotalCacheFamilyRejectsUnknownNames(t *testing.T) {
	for _, model := range []string{"unknown-model", "gpt-5.5", "claude-fable-5", "claude-opus", "claude-opus-no-version", "claude-sonnet-4x", "prefix-claude-opus-5.5", "claude-opus-5.5-evil", "claude-haiku--5"} {
		t.Run(model, func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			profile := &promptCacheProfile{Model: model, TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
			got := tracker.ComputeWithSettings("account", profile, totalCachePolicy(.9))
			tracker.UpdateWithSettings("account", profile, totalCachePolicy(.9), got)
			if got.CacheReadInputTokens+got.CacheCreationInputTokens != 0 || got.limitation != "unsupported_model" || len(tracker.entriesByAccount["account"]) != 0 {
				t.Fatalf("unrecognized name must not invent cache: %+v", got)
			}
		})
	}
}

func TestTotalCacheAliasProfiles(t *testing.T) {
	for _, protocol := range []string{"claude", "openai"} {
		t.Run(protocol, func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			policy := totalCachePolicy(.9)
			build := func(model string) *promptCacheProfile {
				if protocol == "claude" {
					req := &ClaudeRequest{Model: model, Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("stable cache input ", 1000)}}}
					profile := tracker.BuildClaudeProfileWithSettings(req, 10000, policy)
					if req.Model != model {
						t.Fatal("cache model normalization mutated the upstream request")
					}
					return profile
				}
				payload := testKiroPayload()
				payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
				payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("stable cache input ", 1000)
				profile := tracker.BuildOpenAIProfileWithSettings(payload, 10000, policy)
				if payload.ConversationState.CurrentMessage.UserInputMessage.ModelID != model {
					t.Fatal("cache model normalization mutated the upstream payload")
				}
				return profile
			}
			first := build("claude-opus-5.5-20261001")
			cold := commitTotalCacheFixture(tracker, first, policy)
			for _, model := range []string{"claude-opus-5-5-20261001", " ANTHROPIC.CLAUDE-OPUS-5.5-20261001 "} {
				alias := build(model)
				if !reflect.DeepEqual(alias, first) {
					t.Fatalf("equivalent alias changed cache identity: %s", model)
				}
				warm := tracker.ComputeWithSettings("account", alias, policy)
				if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheReadInputTokens == 0 || warm.CacheCreationInputTokens != 0 {
					t.Fatalf("equivalent alias lost the saved share: %+v", warm)
				}
			}
			for _, model := range []string{"claude-opus-5.5-20261002", "claude-opus-5.6-20261001", "claude-sonnet-5.5-20261001", "claude-opus-5.5-20261001-thinking"} {
				other := build(model)
				if other.InputFingerprint == first.InputFingerprint {
					t.Fatalf("different model shared cache identity: %s", model)
				}
				if usage := tracker.ComputeWithSettings("account", other, policy); usage.CacheReadInputTokens != 0 {
					t.Fatalf("different model read saved prefix: %s %+v", model, usage)
				}
			}
			t.Logf("%s: cold write=%d; equivalent aliases read the same share", protocol, cold.CacheCreationInputTokens)
		})
	}
}

func Example_totalCacheFamilySimulation() {
	profile := &promptCacheProfile{Model: "claude-opus-5.5", TotalInputTokens: 21000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 20000, TTL: 5 * time.Minute}}}
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(.9)
	cold := commitTotalCacheFixture(tracker, profile, policy)
	warm := tracker.ComputeWithSettings("account", profile, policy)
	fmt.Println(cold.CacheCreationInputTokens > 0, cold.CacheReadInputTokens == 0)
	fmt.Println(warm.CacheCreationInputTokens == 0, warm.CacheReadInputTokens == cold.CacheCreationInputTokens)
	// Output:
	// true true
	// true true
}
