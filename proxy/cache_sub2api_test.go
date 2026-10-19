package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	refcache "kiro-proxy/internal/sub2apiref/cache"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sub2apiPolicy(r float64) config.PromptCacheSettings {
	return config.PromptCacheSettings{Enabled: true, Mode: "sub2api", Ratio: r}
}

// The former tests assumed caching short inputs, per-interval mixed TTLs, and
// recalibration to native counts. None are the pinned reference's behavior.
func TestSub2APICacheMatchesOriginalProfileAndState(t *testing.T) {
	for _, ratio := range []float64{0, .9, 1} {
		t.Run(fmt.Sprint(ratio), func(t *testing.T) {
			tracker, oracle := newPromptCacheTracker(time.Hour), refcache.NewTracker()
			policy := sub2apiPolicy(ratio)
			req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("stable reference input ", 1000)}}}
			for _, phase := range []string{"cold", "warm", "append", "other-account", "short"} {
				account := "account"
				if phase == "append" {
					req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "answer"}, ClaudeMessage{Role: "user", Content: strings.Repeat("new tail ", 300)})
				}
				if phase == "other-account" {
					account = "other-account"
				}
				if phase == "short" {
					req.Messages = []ClaudeMessage{{Role: "user", Content: "hello"}}
				}
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				input, err := refcache.InputTokens(context.Background(), "anthropic", body)
				if err != nil {
					t.Fatal(err)
				}
				wantProfile, ok := refcache.BuildProfile(context.Background(), "anthropic", body, req.Model, input)
				profile := tracker.BuildClaudeProfileWithSettings(req, input, policy)
				if (profile != nil) != ok {
					t.Fatalf("%s profile acceptance differs", phase)
				}
				if profile != nil && !reflect.DeepEqual(profile.exactProfile.Breakpoints(), wantProfile.Breakpoints()) {
					t.Fatalf("%s reference breakpoints differ", phase)
				}
				plan := oracle.Prepare(account, wantProfile, refcache.Policy{Enabled: true, Mode: "uniform", Ratio: ratio})
				got := tracker.ComputeWithSettings(account, profile, policy)
				want := refcache.Usage{}
				if u := plan.Usage(); u != nil {
					want = *u
				}
				if got.CacheCreationInputTokens != want.CacheCreationInputTokens || got.CacheReadInputTokens != want.CacheReadInputTokens || got.CacheCreation5mInputTokens != want.CacheCreation5mInputTokens || got.CacheCreation1hInputTokens != want.CacheCreation1hInputTokens {
					t.Fatalf("%s got %+v want %+v", phase, got, want)
				}
				// The HTTP-success callback commits the plan separately from lookup.
				if got.exactPlan != nil {
					got.exactPlan.Commit()
				}
				plan.Commit()
			}
		})
	}
}

func TestSub2APICacheOriginalMinimumAndFinalTTL(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := sub2apiPolicy(.9)
	short := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	if got := tracker.BuildClaudeProfileWithSettings(short, 100000, policy); got != nil {
		t.Fatalf("native total must not admit a short raw prefix: %+v", got)
	}
	block := func(ttl string) []interface{} {
		return []interface{}{map[string]interface{}{"type": "text", "text": strings.Repeat("reference prefix ", 1000), "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": ttl}}}
	}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: block("1h")}, {Role: "assistant", Content: block("5m")}}}
	profile := tracker.BuildClaudeProfileWithSettings(req, 10001, policy)
	got := tracker.ComputeWithSettings("account", profile, policy)
	if got.CacheCreationInputTokens != 9001 || got.CacheCreation5mInputTokens != 9001 || got.CacheCreation1hInputTokens != 0 || got.CacheReadInputTokens != 0 {
		t.Fatalf("all creation uses final TTL and reference rounding: %+v", got)
	}
}
