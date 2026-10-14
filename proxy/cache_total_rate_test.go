package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTotalCacheHostedSuccessUsesModeAwareStorage(t *testing.T) {
	for _, ratio := range []float64{0, .8} {
		t.Run(fmt.Sprint(ratio), func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 10000, "outputTokens": 137, "stopReason": "end_turn"}))
			})
			payload := testKiroPayload()
			payload.HostedSearchTools = map[string]bool{"web_search": true}
			payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("stable hosted content ", 1000)
			for round := 0; round < 2; round++ {
				got, err := callKiroWithHostedSearch(context.Background(), testAccount(t), payload, &KiroStreamCallback{}, h.promptCache, 10000, totalCachePolicy(ratio), false)
				if err != nil {
					t.Fatal(err)
				}
				if ratio == 0 && (got != nil || len(h.promptCache.entriesByAccount["only"]) > 0) {
					t.Fatal("hosted zero mode saved hidden cache")
				}
				if ratio > 0 && (got == nil || (round == 1 && got.CacheWriteTokens != 0)) {
					t.Fatalf("hosted repeated input should only read: %+v", got)
				}
			}
			if len(h.promptCache.diagnostics) != 2 {
				t.Fatal("hosted successful usage not diagnosed")
			}
		})
	}
}

func TestTotalCacheDiagnosticsAuthBoundsAndPrivacy(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	h.adminSessions = newAdminSessionStore()
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
	usage := h.promptCache.ComputeWithSettings("secret-account", profile, totalCachePolicy(.8))
	for i := 0; i < promptCacheDiagnosticCapacity+10; i++ {
		h.promptCache.UpdateWithSettings("secret-account", profile, totalCachePolicy(.8), usage)
	}
	url := "/admin/api/prompt-cache/diagnostics"
	unauthorized := httptest.NewRecorder()
	h.handleAdminAPI(unauthorized, httptest.NewRequest("GET", url, nil))
	if unauthorized.Code != 401 {
		t.Fatalf("diagnostics leaked without auth: %d", unauthorized.Code)
	}
	token, _, err := h.adminSessions.Issue()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", url, nil)
	r.Header.Set("X-Admin-Token", token)
	response := httptest.NewRecorder()
	h.handleAdminAPI(response, r)
	var data struct {
		Records []promptCacheDiagnostic `json:"records"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Records) != promptCacheDiagnosticCapacity {
		t.Fatalf("diagnostic bounds: %d", len(data.Records))
	}
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "secret-account") || strings.Contains(response.Body.String(), "sk-") {
		t.Fatal("diagnostic privacy violation")
	}
	for _, entry := range data.Records {
		if entry.Read+entry.Write+entry.Ordinary != entry.Total || entry.Write5m+entry.Write1h != entry.Write {
			t.Fatalf("diagnostic values invalid: %+v", entry)
		}
	}
	h.promptCache.mu.Lock()
	for i := range h.promptCache.diagnostics {
		h.promptCache.diagnostics[i].Time = time.Now().Add(-25 * time.Hour)
	}
	h.promptCache.mu.Unlock()
	response = httptest.NewRecorder()
	h.handleAdminAPI(response, r)
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Records) != 0 {
		t.Fatal("expired diagnostics were retained")
	}
}

func TestTotalCacheConcurrentSnapshotAndNativeMinimum(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-haiku-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{3}, CumulativeTokens: 10000, TTL: time.Hour}}}
	policy := totalCachePolicy(.95)
	cold := tracker.ComputeWithSettings("account", profile, policy)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 20; j++ {
				tracker.UpdateWithSettings("account", profile, policy, cold)
				got := scalePromptCacheUsage(cold, profile, 12000)
				if got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens < 10800 || got.CacheCreationInputTokens > 11400 {
					t.Errorf("dispatch changed: %+v", got)
					return
				}
			}
		}()
	}
	workers.Wait()
	if short := scalePromptCacheUsage(cold, profile, 4095); short.CacheReadInputTokens+short.CacheCreationInputTokens != 0 {
		t.Fatalf("native minimum ignored: %+v", short)
	}
	if equal := scalePromptCacheUsage(cold, profile, 4096); equal.CacheCreationInputTokens == 0 {
		t.Fatal("valid raw prefix suppressed because scaled budget < minimum")
	}
	warm := tracker.ComputeWithSettings("account", profile, policy)
	before := tracker.entriesByAccount["account"][[32]byte{3}]
	zero := tracker.ComputeWithSettings("account", profile, totalCachePolicy(0))
	tracker.UpdateWithSettings("account", profile, totalCachePolicy(0), zero)
	if tracker.entriesByAccount["account"][[32]byte{3}] != before {
		t.Fatal("zero policy renewed hidden entry")
	}
	if warm.CacheCreationInputTokens != 0 {
		t.Fatal("repeat renewed as writing")
	}
	tracker.entriesByAccount["account"][[32]byte{3}] = promptCacheEntry{ExpiresAt: time.Now().Add(-time.Second), TTL: time.Hour}
	if expired := tracker.ComputeWithSettings("account", profile, policy); expired.CacheReadInputTokens != 0 || expired.CacheCreationInputTokens == 0 {
		t.Fatalf("expired cache not rebuilt: %+v", expired)
	}
}

func TestTotalCacheNativeCalibrationRechecksMatchedMinimum(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-haiku-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 6000, TTL: time.Hour}, {Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: time.Hour}}}
	tracker.Update("account", &promptCacheProfile{Model: profile.Model, TotalInputTokens: 6000, Breakpoints: profile.Breakpoints[:1]})
	snapshot := tracker.ComputeWithSettings("account", profile, totalCachePolicy(.8))
	if snapshot.CacheReadInputTokens == 0 {
		t.Fatal("fixture did not match raw prefix")
	}
	usage := scalePromptCacheUsage(snapshot, profile, 6000)
	if usage.CacheReadInputTokens != 0 {
		t.Fatalf("calibrated matched prefix is 3600 below Haiku floor: %+v", usage)
	}
	before := tracker.entriesByAccount["account"][[32]byte{1}]
	tracker.UpdateWithSettings("account", cacheProfileWithInputTokens(profile, 6000), totalCachePolicy(.8), usage)
	if tracker.entriesByAccount["account"][[32]byte{1}] != before {
		t.Fatal("below-minimum native match renewed the old entry")
	}
	if zero := scalePromptCacheUsage(snapshot, profile, 0); zero.CacheReadInputTokens+zero.CacheCreationInputTokens != 0 {
		t.Fatalf("zero native input retained positive cache: %+v", zero)
	}
}

func TestTotalCacheBelowMinimumExplainedInDiagnostic(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload("short content")
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"
	snapshot := beginOpenAICacheUsage(tracker, "account", payload, 3000, totalCachePolicy(.8))
	if got := finishOpenAICacheUsage(tracker, "account", snapshot, 3000); got != nil {
		t.Fatalf("short input cached: %+v", got)
	}
	if len(tracker.diagnostics) != 1 || tracker.diagnostics[0].Limitation != "below_model_minimum" || tracker.diagnostics[0].Ordinary != 3000 {
		t.Fatalf("missing native short-input explanation: %+v", tracker.diagnostics)
	}
	if len(tracker.entriesByAccount["account"]) != 0 {
		t.Fatal("short input stored cache")
	}
}

func TestTotalCacheNativeUpwardCalibrationRetainsPotentialMatch(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("stable Haiku input ", 1000))
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"
	policy := totalCachePolicy(.8)
	first := beginOpenAICacheUsage(tracker, "account", payload, 3000, policy)
	cold := finishOpenAICacheUsage(tracker, "account", first, 6000)
	if cold == nil || cold.CacheWriteTokens == 0 {
		t.Fatal("native upward input did not write")
	}
	for fp, entry := range tracker.entriesByAccount["account"] {
		entry.ExpiresAt = time.Now().Add(time.Minute)
		tracker.entriesByAccount["account"][fp] = entry
	}
	second := beginOpenAICacheUsage(tracker, "account", payload, 3000, policy)
	warm := finishOpenAICacheUsage(tracker, "account", second, 6000)
	if warm == nil || warm.CachedTokens != cold.CacheWriteTokens || warm.CacheWriteTokens != 0 {
		t.Fatalf("identical upward calibrated input wrote again: cold=%+v warm=%+v", cold, warm)
	}
	if second.usage.matchedTTL == 0 {
		t.Fatal("potential-match TTL was erased before calibration")
	}
	for _, entry := range tracker.entriesByAccount["account"] {
		if !entry.ExpiresAt.After(time.Now().Add(4 * time.Minute)) {
			t.Fatal("upward native match was not renewed")
		}
	}
}

func TestTotalCacheFullyMatchedDoesNotCreateOrUpgradeBreakpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(.8)
	full := promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: 5 * time.Minute}
	first := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{full}}
	tracker.Update("account", first)
	full.TTL = time.Hour
	next := &promptCacheProfile{Model: first.Model, TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 6000, TTL: time.Hour}, full}}
	usage := tracker.ComputeWithSettings("account", next, policy)
	if usage.CacheCreationInputTokens != 0 {
		t.Fatal("fixture must be fully matched")
	}
	tracker.UpdateWithSettings("account", next, policy, usage)
	if _, exists := tracker.entriesByAccount["account"][[32]byte{1}]; exists {
		t.Fatal("zero writing created a new shorter breakpoint")
	}
	if tracker.entriesByAccount["account"][[32]byte{2}].TTL != 5*time.Minute {
		t.Fatal("zero writing upgraded a 5m cache to 1h")
	}
}

func TestTotalCacheTTLAnchoredToDispatch(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: 5 * time.Minute}}}
	policy := totalCachePolicy(.8)
	snapshot := tracker.ComputeWithSettings("account", profile, policy)
	if snapshot.startedAt.IsZero() {
		t.Error("dispatch snapshot has no start time")
	}
	snapshot.startedAt = time.Now().Add(-4 * time.Minute)
	tracker.UpdateWithSettings("account", profile, policy, snapshot)
	if stored := tracker.entriesByAccount["account"][[32]byte{1}]; !stored.ExpiresAt.Equal(snapshot.startedAt.Add(5 * time.Minute)) {
		t.Fatalf("request duration extended TTL: %+v", stored)
	}
	tracker = newPromptCacheTracker(time.Hour)
	snapshot.startedAt = time.Now().Add(-6 * time.Minute)
	tracker.UpdateWithSettings("account", profile, policy, snapshot)
	if len(tracker.entriesByAccount["account"]) != 0 {
		t.Fatal("already-expired dispatch cache resurrected on completion")
	}
}

func TestTotalCacheExactAllocationAndTTL(t *testing.T) {
	for _, test := range []struct{ total, eligible, matched, read, write, ordinary int }{
		{10000, 10000, 0, 0, 8000, 2000}, {10000, 10000, 10000, 8000, 0, 2000},
		{11000, 11000, 10000, 8000, 800, 2200}, {10000, 2000, 2000, 2000, 0, 8000},
	} {
		profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: test.total, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: test.eligible, TTL: time.Hour}}}
		usage := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8}, profile, test.eligible, test.matched)
		if usage.CacheReadInputTokens != test.read || usage.CacheCreationInputTokens != test.write || billedClaudeInputTokens(test.total, usage) != test.ordinary {
			t.Fatalf("%+v: %+v", test, usage)
		}
	}
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: 2000, TTL: time.Hour}, {CumulativeTokens: 10000, TTL: 5 * time.Minute}}}
	usage := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8}, profile, 10000, 0)
	if usage.CacheCreation1hInputTokens != 1600 || usage.CacheCreation5mInputTokens != 6400 {
		t.Fatalf("mixed TTL lost proportions: %+v", usage)
	}
	for write := 1; write < 50; write++ {
		short, long := allocateCacheWriteTTL(write, 31, 67)
		if short+long != write || short < 0 || long < 0 {
			t.Fatalf("rounding %d: %d+%d", write, short, long)
		}
	}
}

func TestTotalCacheLookbackWindowAndNoPhantomStorage(t *testing.T) {
	for _, appended := range []int{19, 20} {
		t.Run(fmt.Sprint(appended), func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			policy := totalCachePolicy(.8)
			first := map[string]interface{}{"type": "text", "text": strings.Repeat("first prefix ", 1000), "cache_control": map[string]interface{}{"type": "ephemeral"}}
			req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{first}}}}
			profile := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
			tracker.Update("account", profile)
			delete(first, "cache_control")
			content := req.Messages[0].Content.([]interface{})
			for i := 0; i < appended; i++ {
				content = append(content, map[string]interface{}{"type": "text", "text": fmt.Sprintf("new block %d", i)})
			}
			content[len(content)-1].(map[string]interface{})["cache_control"] = map[string]interface{}{"type": "ephemeral"}
			req.Messages[0].Content = content
			next := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
			usage := tracker.ComputeWithSettings("account", next, policy)
			if (usage.rawRead > 0) != (appended == 19) {
				t.Fatalf("20 inclusive position window: %+v", usage)
			}
			coldTracker := newPromptCacheTracker(time.Hour)
			cold := coldTracker.ComputeWithSettings("account", next, policy)
			coldTracker.UpdateWithSettings("account", next, policy, cold)
			if len(coldTracker.entriesByAccount["account"]) != 1 {
				t.Fatal("lookup candidates were stored as phantom breakpoints")
			}
		})
	}
}

func TestTotalCacheParallelToolRunsAndMatchedRenewal(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(.8)
	initial := map[string]interface{}{"type": "text", "text": strings.Repeat("initial prefix ", 1000), "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{initial}}}}
	first := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	tracker.Update("account", first)
	fingerprint := first.Breakpoints[0].Fingerprint
	tracker.entriesByAccount["account"][fingerprint] = promptCacheEntry{ExpiresAt: time.Now().Add(time.Minute), TTL: time.Hour}
	before := tracker.entriesByAccount["account"][fingerprint]
	delete(initial, "cache_control")
	uses, results := []interface{}{}, []interface{}{}
	for i := 0; i < 30; i++ {
		uses = append(uses, map[string]interface{}{"type": "tool_use", "id": fmt.Sprint(i), "name": "lookup", "input": map[string]interface{}{"query": "q"}})
		results = append(results, map[string]interface{}{"type": "tool_result", "tool_use_id": fmt.Sprint(i), "content": "ok"})
	}
	results[len(results)-1].(map[string]interface{})["cache_control"] = map[string]interface{}{"type": "ephemeral"}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: uses}, ClaudeMessage{Role: "user", Content: results})
	next := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	usage := tracker.ComputeWithSettings("account", next, policy)
	if usage.rawRead != first.Breakpoints[0].CumulativeTokens {
		t.Fatalf("parallel tool run exhausted lookback: %+v", usage)
	}
	if tracker.entriesByAccount["account"][fingerprint] != before {
		t.Fatal("lookup renewed before success")
	}
	tracker.UpdateWithSettings("account", next, policy, usage)
	if !tracker.entriesByAccount["account"][fingerprint].ExpiresAt.After(before.ExpiresAt) {
		t.Fatal("matched moved breakpoint not renewed after success")
	}
}

func TestTotalCacheProtocolJSONAndSSE(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 137}))
				})
				enabled, mode, ratio := true, "prefix_total", .8
				key, err := config.AddApiKey(config.ApiKeyEntry{ID: "total-mode", Key: "sk-total-mode-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
				if err != nil {
					t.Fatal(err)
				}
				h.pool.Reload()
				request := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
				messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("stable total input ", 1000)}}
				if path == "/v1/responses" {
					request["input"], request["store"] = messages, false
				} else {
					request["messages"] = messages
				}
				body, _ := json.Marshal(request)
				for round := 0; round < 3; round++ {
					if round == 2 {
						ratio = 0
						if err := config.UpdateApiKey(key.ID, config.ApiKeyEntry{Enabled: true, PromptCacheEmulationRatio: &ratio}); err != nil {
							t.Fatal(err)
						}
					}
					r := withApiKeyContext(httptest.NewRequest("POST", path, strings.NewReader(string(body))), &key)
					rec := httptest.NewRecorder()
					switch path {
					case "/v1/messages":
						h.handleClaudeMessages(rec, r)
					case "/v1/chat/completions":
						h.handleOpenAIChat(rec, r)
					default:
						h.handleOpenAIResponses(rec, r)
					}
					if rec.Code != 200 {
						t.Fatalf("request: %s", rec.Body.String())
					}
					usage := runtimeProtocolUsage(t, rec, stream)
					var read, write, total, output float64
					if path == "/v1/messages" {
						read, _ = usage["cache_read_input_tokens"].(float64)
						write, _ = usage["cache_creation_input_tokens"].(float64)
						total = usage["input_tokens"].(float64) + read + write
						output = usage["output_tokens"].(float64)
					} else {
						field, input, out := "prompt_tokens_details", "prompt_tokens", "completion_tokens"
						if path == "/v1/responses" {
							field, input, out = "input_tokens_details", "input_tokens", "output_tokens"
						}
						if details, ok := usage[field].(map[string]interface{}); ok {
							read, _ = details["cached_tokens"].(float64)
							write, _ = details["cache_write_tokens"].(float64)
						}
						total, output = usage[input].(float64), usage[out].(float64)
					}
					if total != 10000 || output != 137 || (round < 2 && (read+write < 7500 || read+write > 8500)) || (round == 0 && read != 0) || (round == 1 && write != 0) || (round == 2 && read+write != 0) {
						t.Fatalf("round %d: %v", round, usage)
					}
					t.Logf("round %d total=%.0f read=%.0f write=%.0f output=%.0f", round, total, read, write, output)
				}
			})
		}
	}
}

func totalCachePolicy(ratio float64) config.PromptCacheSettings {
	return config.PromptCacheSettings{Enabled: true, Mode: "prefix_total", Ratio: ratio}
}

func TestTotalCacheBudgetColdWarmPartialAndScaling(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, InputFingerprint: [32]byte{31}, Breakpoints: []promptCacheBreakpoint{
		{Fingerprint: [32]byte{1}, CumulativeTokens: 5000, TTL: 5 * time.Minute},
		{Fingerprint: [32]byte{2}, CumulativeTokens: 8000, TTL: time.Hour},
	}}
	policy := totalCachePolicy(.8)
	p := variedPrefixReadRatio(.8, profile)
	budget := minInt(8000, int(math.Floor(10000*p)))
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != budget {
		t.Fatalf("cold total cache budget: %+v, want %d", cold, budget)
	}
	tracker.Update("account", &promptCacheProfile{Model: profile.Model, TotalInputTokens: 5000, Breakpoints: profile.Breakpoints[:1]})
	partial := tracker.ComputeWithSettings("account", profile, policy)
	wantRead := budget * 5000 / 8000
	if partial.CacheReadInputTokens != wantRead || partial.CacheCreationInputTokens != budget-wantRead || partial.CacheCreation5mInputTokens != 0 || partial.CacheCreation1hInputTokens != budget-wantRead {
		t.Fatalf("partial must only write new interval: %+v", partial)
	}
	tracker.Update("account", profile)
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens != budget || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(10000, warm) != 10000-budget {
		t.Fatalf("repeated content must not write: %+v", warm)
	}
	for _, snapshot := range []promptCacheUsage{cold, partial, warm} {
		usage := scalePromptCacheUsage(snapshot, profile, 20000)
		wantBudget := minInt(16000, int(math.Floor(20000*p)))
		if usage.CacheReadInputTokens+usage.CacheCreationInputTokens != wantBudget || usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens {
			t.Fatalf("native scaling: %+v", usage)
		}
		if again := scalePromptCacheUsage(snapshot, profile, 20000); again != usage {
			t.Fatal("snapshot changed after other completions")
		}
	}
}

func TestTotalCacheZeroDoesNotCreateStorage(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("stable input ", 1000))
	snapshot := beginOpenAICacheUsage(tracker, "account", payload, 10000, totalCachePolicy(0))
	if got := finishOpenAICacheUsage(tracker, "account", snapshot, 10000); got != nil {
		t.Fatalf("zero rate cached: %+v", got)
	}
	if len(tracker.entriesByAccount["account"]) != 0 {
		t.Fatal("zero total cache rate stored invisible prefixes")
	}
}

func TestTotalCacheMovingBreakpointFindsOldPrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(.8)
	block := func(text string, marked bool) map[string]interface{} {
		b := map[string]interface{}{"type": "text", "text": text}
		if marked {
			b["cache_control"] = map[string]interface{}{"type": "ephemeral", "ttl": "1h"}
		}
		return b
	}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: []interface{}{block(strings.Repeat("stable system ", 1000), false)}, Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{block(strings.Repeat("stable user ", 1000), true)}}}}
	first := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	tracker.Update("account", first)
	req.Messages[0].Content.([]interface{})[0].(map[string]interface{})["cache_control"] = nil
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "ok"}, ClaudeMessage{Role: "user", Content: []interface{}{block(strings.Repeat("new tail ", 400), true)}})
	next := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	usage := tracker.ComputeWithSettings("account", next, policy)
	if usage.rawRead != first.Breakpoints[len(first.Breakpoints)-1].CumulativeTokens {
		t.Fatalf("moved marker lost old prefix: %+v", usage)
	}
}

func TestTotalCacheModelThresholds(t *testing.T) {
	for _, test := range []struct {
		model string
		floor int
	}{{"claude-sonnet-4.5", 1024}, {"claude-haiku-4-5-20251001-thinking", 4096}, {"claude-opus-4.6", 4096}} {
		for _, n := range []int{test.floor - 1, test.floor} {
			profile := &promptCacheProfile{Model: test.model, TotalInputTokens: n, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: n, TTL: time.Hour}}}
			got := newPromptCacheTracker(time.Hour).ComputeWithSettings("account", profile, totalCachePolicy(.8))
			if (got.CacheCreationInputTokens > 0) != (n == test.floor) {
				t.Fatalf("model %s prefix %d: %+v", test.model, n, got)
			}
		}
	}
	profile := &promptCacheProfile{Model: "unknown-model", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
	if got := newPromptCacheTracker(time.Hour).ComputeWithSettings("account", profile, totalCachePolicy(.8)); got.CacheCreationInputTokens != 0 {
		t.Fatalf("unknown model fabricated cache: %+v", got)
	}
}
