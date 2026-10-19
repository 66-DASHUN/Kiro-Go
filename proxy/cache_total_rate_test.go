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
	for _, ratio := range []float64{0, 1} {
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

func TestTotalCacheDiagnosticsExposeMatchLifecycle(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 10000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: [32]byte{9}, CumulativeTokens: 10000, TTL: time.Hour}},
	}
	first := tracker.ComputeWithSettings("account", profile, policy)
	tracker.UpdateWithSettings("account", profile, policy, first)
	second := tracker.ComputeWithSettings("account", profile, policy)
	tracker.UpdateWithSettings("account", profile, policy, second)
	if len(tracker.diagnostics) != 2 {
		t.Fatalf("diagnostics = %d, want 2", len(tracker.diagnostics))
	}
	latest := tracker.diagnostics[1]
	if latest.Matched != 10000 || latest.MatchedShare != first.CacheCreationInputTokens || latest.MatchedTTLSeconds != 3600 || latest.MatchedPrefixTokens != 10000 {
		t.Fatalf("match lifecycle not recorded: %+v", latest)
	}
}

func TestTotalCacheRatioDoesNotSplitTokensWithinAnAdmittedPrefix(t *testing.T) {
	const ratio = 0.9
	model := "claude-sonnet-4.5"
	var fingerprint [32]byte
	for i := 1; i < 256; i++ {
		fingerprint[0] = byte(i)
		if totalCacheAdmission(totalCacheAdmissionRatio(ratio, model, fingerprint), model, fingerprint) {
			break
		}
	}
	if !totalCacheAdmission(totalCacheAdmissionRatio(ratio, model, fingerprint), model, fingerprint) {
		t.Fatal("failed to find an admitted prefix fixture")
	}

	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(ratio)
	first := &promptCacheProfile{
		Model:            model,
		TotalInputTokens: 10000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 9800, TTL: 5 * time.Minute}},
	}
	cold := tracker.ComputeWithSettings("account", first, policy)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != 9800 || billedClaudeInputTokens(first.TotalInputTokens, cold) != 200 {
		t.Fatalf("an admitted cold prefix must be fully created and leave only its actual tail ordinary: %+v", cold)
	}
	tracker.UpdateWithSettings("account", first, policy, cold)
	warm := tracker.ComputeWithSettings("account", first, policy)
	if warm.CacheReadInputTokens != 9800 || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(first.TotalInputTokens, warm) != 200 {
		t.Fatalf("an admitted warm prefix must be fully read without an artificial ordinary share: %+v", warm)
	}

	appendedFingerprint := fingerprint
	appendedFingerprint[1] = 1
	appended := &promptCacheProfile{
		Model:            model,
		TotalInputTokens: 10500,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: fingerprint, CumulativeTokens: 9800, TTL: 5 * time.Minute},
			{Fingerprint: appendedFingerprint, CumulativeTokens: 10300, TTL: 5 * time.Minute},
		},
	}
	appendUsage := tracker.ComputeWithSettings("account", appended, policy)
	if appendUsage.CacheReadInputTokens != 9800 || appendUsage.CacheCreationInputTokens != 500 || billedClaudeInputTokens(appended.TotalInputTokens, appendUsage) != 200 {
		t.Fatalf("an appended request must read the matched prefix, write only the new stable tail, and keep the current tail ordinary: %+v", appendUsage)
	}
}

func TestTotalCacheConcurrentSnapshotAndNativeShortInput(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-haiku-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{3}, CumulativeTokens: 10000, TTL: time.Hour}}}
	policy := totalCachePolicy(1)
	cold := tracker.ComputeWithSettings("account", profile, policy)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 20; j++ {
				tracker.UpdateWithSettings("account", profile, policy, cold)
				got := scalePromptCacheUsage(cold, profile, 12000)
				if got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens != 12000 {
					t.Errorf("dispatch changed: %+v", got)
					return
				}
			}
		}()
	}
	workers.Wait()
	if short := scalePromptCacheUsage(cold, profile, 1); short.CacheCreationInputTokens != 1 || short.CacheReadInputTokens != 0 {
		t.Fatalf("native short input excluded: %+v", short)
	}
	if equal := scalePromptCacheUsage(cold, profile, 4096); equal.CacheCreationInputTokens == 0 {
		t.Fatal("native input unexpectedly excluded")
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

func TestTotalCacheNativeCalibrationClampsMatchedShare(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-haiku-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 6000, TTL: time.Hour}, {Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: time.Hour}}}
	commitTotalCacheFixture(tracker, &promptCacheProfile{Model: profile.Model, TotalInputTokens: 6000, Breakpoints: profile.Breakpoints[:1]}, totalCachePolicy(1))
	snapshot := tracker.ComputeWithSettings("account", profile, totalCachePolicy(1))
	if snapshot.CacheReadInputTokens == 0 {
		t.Fatal("fixture did not match raw prefix")
	}
	usage := scalePromptCacheUsage(snapshot, profile, 6000)
	if usage.CacheReadInputTokens != 3600 || usage.CacheCreationInputTokens != 2400 {
		t.Fatalf("native count must clamp the saved read and write only new content: %+v", usage)
	}
	before := tracker.entriesByAccount["account"][[32]byte{1}]
	tracker.UpdateWithSettings("account", cacheProfileWithInputTokens(profile, 6000), totalCachePolicy(1), usage)
	after := tracker.entriesByAccount["account"][[32]byte{1}]
	if after.PrefixTokens != before.PrefixTokens || after.CachedTokens != before.CachedTokens || after.ExpiresAt.Before(before.ExpiresAt) {
		t.Fatal("native short match changed the committed accounting basis")
	}
	if zero := scalePromptCacheUsage(snapshot, profile, 0); zero.CacheReadInputTokens+zero.CacheCreationInputTokens != 0 {
		t.Fatalf("zero native input retained positive cache: %+v", zero)
	}
}

func TestTotalCacheShortInputExplainedInDiagnostic(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload("short content")
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"
	snapshot := beginOpenAICacheUsage(tracker, "account", payload, 3, totalCachePolicy(1))
	got := finishOpenAICacheUsage(tracker, "account", snapshot, 3)
	if got == nil || got.CacheWriteTokens <= 0 || got.CachedTokens != 0 {
		t.Fatalf("short native input did not write: %+v", got)
	}
	if len(tracker.diagnostics) != 1 {
		t.Fatalf("missing short input diagnostic: %+v", tracker.diagnostics)
	}
	d := tracker.diagnostics[0]
	if d.Rule != totalCacheRuleVersion || d.Limitation != "" || d.Ordinary+d.Read+d.Write != 3 || d.Write != got.CacheWriteTokens {
		t.Fatalf("incorrect short input diagnostic: %+v", d)
	}
	if len(tracker.entriesByAccount["account"]) == 0 {
		t.Fatal("successful short input did not store cache")
	}
}

func TestTotalCacheNativeUpwardCalibrationRetainsPotentialMatch(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := estimatedOpenAIPayload(strings.Repeat("stable Haiku input ", 1000))
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"
	policy := totalCachePolicy(1)
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
	entry := tracker.entriesByAccount["account"][second.usage.matchedFingerprint]
	if !entry.ExpiresAt.After(time.Now().Add(4 * time.Minute)) {
		t.Fatal("upward native match was not renewed")
	}
}

func TestTotalCacheFullyMatchedDoesNotCreateOrUpgradeBreakpoints(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(1)
	full := promptCacheBreakpoint{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: 5 * time.Minute}
	first := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{full}}
	commitTotalCacheFixture(tracker, first, policy)
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
	policy := totalCachePolicy(1)
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
	baseProfile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: 10000, TTL: time.Hour}}}
	cold := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8}, baseProfile, 10000, 0)
	if cold.CacheCreationInputTokens != 10000 || billedClaudeInputTokens(10000, cold) != 0 {
		t.Fatalf("an admitted cold prefix should be created in full: %+v", cold)
	}
	warm := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8, matchedCachedTokens: 10000}, baseProfile, 10000, 10000)
	if warm.CacheReadInputTokens != 10000 || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(10000, warm) != 0 {
		t.Fatalf("a warm cache must read the saved prefix in full: %+v", warm)
	}
	appendedProfile := &promptCacheProfile{Model: baseProfile.Model, TotalInputTokens: 11000, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: 10000, TTL: time.Hour}, {CumulativeTokens: 11000, TTL: time.Hour}}}
	appended := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8, matchedCachedTokens: 10000}, appendedProfile, 11000, 10000)
	if appended.CacheReadInputTokens != 10000 || appended.CacheCreationInputTokens != 1000 || billedClaudeInputTokens(11000, appended) != 0 {
		t.Fatalf("an appended request must read its prior prefix and write only the new stable tail: %+v", appended)
	}

	for _, test := range []struct{ total, eligible, matched, saved, read, write, ordinary int }{
		{10000, 10000, 0, 0, 0, 10000, 0}, {10000, 10000, 10000, 10000, 10000, 0, 0},
		{11000, 11000, 10000, 10000, 10000, 1000, 0}, {10000, 2000, 2000, 1600, 1600, 0, 8400},
	} {
		profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: test.total, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: test.eligible, TTL: time.Hour}}}
		usage := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8, matchedCachedTokens: test.saved}, profile, test.eligible, test.matched)
		if usage.CacheReadInputTokens != test.read || usage.CacheCreationInputTokens != test.write || billedClaudeInputTokens(test.total, usage) != test.ordinary {
			t.Fatalf("%+v: %+v", test, usage)
		}
	}
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{CumulativeTokens: 2000, TTL: time.Hour}, {CumulativeTokens: 10000, TTL: 5 * time.Minute}}}
	usage := allocateTotalCacheUsage(promptCacheUsage{totalCacheRate: true, totalRatio: .8}, profile, 10000, 0)
	if usage.CacheCreation1hInputTokens != 2000 || usage.CacheCreation5mInputTokens != 8000 {
		t.Fatalf("mixed TTL lost proportions: %+v", usage)
	}
	for write := 1; write < 50; write++ {
		short, long := allocateCacheWriteTTL(write, 31, 67)
		if short+long != write || short < 0 || long < 0 {
			t.Fatalf("rounding %d: %d+%d", write, short, long)
		}
	}
}

func TestTotalCacheConfiguredRatioLeavesOrdinaryInputColdAndWarm(t *testing.T) {
	const model = "claude-sonnet-4.5"
	policy := totalCachePolicy(.95)
	for _, total := range []int{13, 10000} {
		t.Run(fmt.Sprintf("tokens=%d", total), func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			var profile *promptCacheProfile
			for seed := 1; seed < 256; seed++ {
				fingerprint := [32]byte{byte(seed), 23}
				candidate := &promptCacheProfile{
					Model:            model,
					TotalInputTokens: total,
					InputFingerprint: fingerprint,
					Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: total, TTL: 5 * time.Minute}},
				}
				admissionRatio := totalCacheAdmissionRatio(policy.Ratio, model, fingerprint)
				if totalCacheAdmission(admissionRatio, model, fingerprint) {
					profile = candidate
					break
				}
			}
			if profile == nil {
				t.Fatal("failed to find an admitted prefix fixture")
			}

			cold := tracker.ComputeWithSettings("account", profile, policy)
			coldOrdinary := billedClaudeInputTokens(total, cold)
			if cold.CacheCreationInputTokens != total || cold.CacheReadInputTokens != 0 || coldOrdinary != 0 || coldOrdinary+cold.CacheCreationInputTokens != total {
				t.Fatalf("admitted cold prefix should be created in full: %+v ordinary=%d", cold, coldOrdinary)
			}
			if cold.CacheCreation5mInputTokens+cold.CacheCreation1hInputTokens != cold.CacheCreationInputTokens {
				t.Fatalf("cold cache TTL buckets do not sum to write: %+v", cold)
			}
			tracker.UpdateWithSettings("account", profile, policy, cold)

			warm := tracker.ComputeWithSettings("account", profile, policy)
			warmOrdinary := billedClaudeInputTokens(total, warm)
			if warm.CacheReadInputTokens != cold.CacheCreationInputTokens || warm.CacheCreationInputTokens != 0 || warmOrdinary != 0 || warmOrdinary+warm.CacheReadInputTokens != total {
				t.Fatalf("warm request should read the saved prefix in full: %+v ordinary=%d", warm, warmOrdinary)
			}
		})
	}
}

func TestTotalCacheAdmissionTracksWholePrefixesByConfiguredRatio(t *testing.T) {
	const samples = 1000
	accepted := 0
	for i := 0; i < samples; i++ {
		var fingerprint [32]byte
		fingerprint[0], fingerprint[1] = byte(i>>8), byte(i)
		ratio := totalCacheAdmissionRatio(.8, "claude-sonnet-4.5", fingerprint)
		first := totalCacheAdmission(ratio, "claude-sonnet-4.5", fingerprint)
		if first != totalCacheAdmission(totalCacheAdmissionRatio(.8, "claude-sonnet-4.5", fingerprint), "claude-sonnet-4.5", fingerprint) {
			t.Fatalf("admission changed for stable fingerprint %d", i)
		}
		if first {
			accepted++
		}
	}
	if accepted < 750 || accepted > 850 {
		t.Fatalf("80%% content admission selected %d/%d prefixes", accepted, samples)
	}
	var fingerprint [32]byte
	fingerprint[0] = 1
	if totalCacheAdmission(0, "claude-sonnet-4.5", fingerprint) || !totalCacheAdmission(1, "claude-sonnet-4.5", fingerprint) {
		t.Fatal("zero and full admission boundaries are incorrect")
	}
}

func TestTotalCacheAdmissionRatioHonorsConfiguredUpperCap(t *testing.T) {
	for _, test := range []struct {
		ratio float64
		low   float64
		high  float64
	}{
		{ratio: 0, low: 0, high: 0},
		{ratio: .01, low: 0, high: .06},
		{ratio: .8, low: .75, high: .85},
		{ratio: .95, low: .9, high: .95},
		{ratio: .99, low: .9, high: .95},
		{ratio: 1, low: 1, high: 1},
	} {
		for i := 0; i < 1000; i++ {
			var fingerprint [32]byte
			fingerprint[0], fingerprint[1] = byte(i>>8), byte(i)
			got := totalCacheAdmissionRatio(test.ratio, "claude-sonnet-4.5", fingerprint)
			if got < test.low || got > test.high {
				t.Fatalf("ratio %.2f produced threshold %.5f outside [%.2f, %.2f]", test.ratio, got, test.low, test.high)
			}
			if again := totalCacheAdmissionRatio(test.ratio, "claude-sonnet-4.5", fingerprint); again != got {
				t.Fatalf("ratio threshold changed for stable fingerprint: %.5f != %.5f", got, again)
			}
		}
	}
}

func TestTotalCacheAdmissionForPrefixIgnoresUncachedSuffix(t *testing.T) {
	const model = "claude-sonnet-4.5"
	const configuredRatio = .8
	tracker := newPromptCacheTracker(time.Hour)
	policy := totalCachePolicy(configuredRatio)
	var first, second *promptCacheProfile
	for prefixSeed := 1; prefixSeed < 256 && first == nil; prefixSeed++ {
		fingerprint := [32]byte{42, 17, byte(prefixSeed)}
		for i := 1; i < 256 && first == nil; i++ {
			for j := i + 1; j < 256; j++ {
				candidateA := &promptCacheProfile{
					Model:            model,
					TotalInputTokens: 11000,
					InputFingerprint: [32]byte{byte(i), 1},
					Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 10000, TTL: time.Hour}},
				}
				candidateB := &promptCacheProfile{
					Model:            model,
					TotalInputTokens: 11000,
					InputFingerprint: [32]byte{byte(j), 2},
					Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 10000, TTL: time.Hour}},
				}
				admittedA := totalCacheAdmission(variedPrefixReadRatio(configuredRatio, candidateA), model, fingerprint)
				admittedB := totalCacheAdmission(variedPrefixReadRatio(configuredRatio, candidateB), model, fingerprint)
				if admittedA != admittedB {
					first, second = candidateA, candidateB
					break
				}
			}
		}
	}
	if first == nil {
		t.Fatal("failed to find uncached suffixes that cross the old admission threshold")
	}

	firstUsage := tracker.ComputeWithSettings("account", first, policy)
	secondUsage := tracker.ComputeWithSettings("account", second, policy)
	if firstUsage.CacheCreationInputTokens != secondUsage.CacheCreationInputTokens || firstUsage.limitation != secondUsage.limitation {
		t.Fatalf("same cacheable prefix changed admission with its uncached suffix: first=%+v second=%+v", firstUsage, secondUsage)
	}
}

func TestTotalCacheRatioDoesNotTruncateAnExistingCacheHit(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 10000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: [32]byte{71, 12}, CumulativeTokens: 10000, TTL: time.Hour}},
	}
	fullPolicy := totalCachePolicy(1)
	cold := tracker.ComputeWithSettings("account", profile, fullPolicy)
	tracker.UpdateWithSettings("account", profile, fullPolicy, cold)

	lowPolicy := totalCachePolicy(.01)
	usage := tracker.ComputeWithSettings("account", profile, lowPolicy)
	if usage.CacheReadInputTokens != cold.CacheCreationInputTokens || usage.CacheCreationInputTokens != 0 || billedClaudeInputTokens(profile.TotalInputTokens, usage) != 0 {
		t.Fatalf("configured admission ratio must not truncate an already matching cache entry: cold=%+v usage=%+v", cold, usage)
	}
}

func TestTotalCacheRejectedColdPrefixStaysOrdinaryAndUnstored(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	const model = "claude-sonnet-4.5"
	policy := totalCachePolicy(.8)
	var profile *promptCacheProfile
	for i := 1; i < 256; i++ {
		fingerprint := [32]byte{byte(i)}
		candidate := &promptCacheProfile{
			Model:            model,
			TotalInputTokens: 10000,
			InputFingerprint: [32]byte{9, 8, byte(i)},
			Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 10000, TTL: time.Hour}},
		}
		if !totalCacheAdmission(totalCacheAdmissionRatio(policy.Ratio, model, fingerprint), model, fingerprint) {
			profile = candidate
			break
		}
	}
	if profile == nil {
		t.Fatal("failed to find a deterministic rejected cold prefix fixture")
	}
	usage := tracker.ComputeWithSettings("account", profile, policy)
	if usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 || billedClaudeInputTokens(10000, usage) != 10000 || usage.limitation != "cache_admission_miss" {
		t.Fatalf("rejected cold prefix should remain ordinary input: %+v", usage)
	}
	tracker.UpdateWithSettings("account", profile, policy, usage)
	if len(tracker.entriesByAccount["account"]) != 0 {
		t.Fatal("rejected cold prefix created a hidden cache entry")
	}
	if repeated := tracker.ComputeWithSettings("account", profile, policy); repeated.CacheReadInputTokens+repeated.CacheCreationInputTokens != 0 {
		t.Fatalf("rejected content changed its admission on repeat: %+v", repeated)
	}
}

func TestTotalCacheMatchedPrefixAdmitsNewTail(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	model := "claude-sonnet-4.5"
	policy := totalCachePolicy(.8)
	var first *promptCacheProfile
	var next *promptCacheProfile
	for oldSeed := 1; oldSeed < 256 && next == nil; oldSeed++ {
		oldFingerprint := [32]byte{byte(oldSeed), 1}
		admission := totalCacheAdmissionRatio(policy.Ratio, model, oldFingerprint)
		if !totalCacheAdmission(admission, model, oldFingerprint) {
			continue
		}
		candidateFirst := &promptCacheProfile{Model: model, TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: oldFingerprint, CumulativeTokens: 10000, TTL: time.Hour}}}
		for newSeed := 1; newSeed < 256; newSeed++ {
			newFingerprint := [32]byte{byte(newSeed), 2}
			if newFingerprint == oldFingerprint {
				continue
			}
			first = candidateFirst
			next = &promptCacheProfile{
				Model:            model,
				TotalInputTokens: 11000,
				Breakpoints: []promptCacheBreakpoint{
					candidateFirst.Breakpoints[0],
					{Fingerprint: newFingerprint, CumulativeTokens: 11000, TTL: 5 * time.Minute},
				},
			}
			break
		}
	}
	if first == nil || next == nil {
		t.Fatal("failed to find an admitted prefix and distinct appended tail")
	}
	cold := tracker.ComputeWithSettings("account", first, policy)
	if cold.CacheCreationInputTokens != 10000 || billedClaudeInputTokens(10000, cold) != 0 {
		t.Fatalf("admitted cold prefix should be created in full: %+v", cold)
	}
	tracker.UpdateWithSettings("account", first, policy, cold)
	usage := tracker.ComputeWithSettings("account", next, policy)
	if usage.CacheReadInputTokens != 10000 || usage.CacheCreationInputTokens != 1000 || usage.limitation != "" || billedClaudeInputTokens(11000, usage) != 0 || usage.CacheReadInputTokens+usage.CacheCreationInputTokens+billedClaudeInputTokens(11000, usage) != 11000 {
		t.Fatalf("matched prefix must read the stored prefix and write only the new stable tail: %+v", usage)
	}
	tracker.UpdateWithSettings("account", next, policy, usage)
	warm := tracker.ComputeWithSettings("account", next, policy)
	if warm.CacheReadInputTokens != 11000 || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(11000, warm) != 0 {
		t.Fatalf("successful tail write was not fully reusable: %+v", warm)
	}
}

func TestTotalCacheLookbackWindowAndNoPhantomStorage(t *testing.T) {
	for _, appended := range []int{19, 20} {
		t.Run(fmt.Sprint(appended), func(t *testing.T) {
			tracker := newPromptCacheTracker(time.Hour)
			policy := totalCachePolicy(1)
			first := map[string]interface{}{"type": "text", "text": strings.Repeat("first prefix ", 1000), "cache_control": map[string]interface{}{"type": "ephemeral"}}
			req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{first}}}}
			profile := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
			commitTotalCacheFixture(tracker, profile, policy)
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
	policy := totalCachePolicy(1)
	initial := map[string]interface{}{"type": "text", "text": strings.Repeat("initial prefix ", 1000), "cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"}}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{initial}}}}
	first := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	commitTotalCacheFixture(tracker, first, policy)
	fingerprint := first.Breakpoints[0].Fingerprint
	entry := tracker.entriesByAccount["account"][fingerprint]
	entry.ExpiresAt = time.Now().Add(time.Minute)
	tracker.entriesByAccount["account"][fingerprint] = entry
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
	testCacheProtocolJSONAndSSE(t, "prefix_total", 1)
}

func TestSub2APICacheProtocolJSONAndSSE(t *testing.T) {
	for _, ratio := range []float64{.9, 1} {
		t.Run(fmt.Sprint(ratio), func(t *testing.T) { testCacheProtocolJSONAndSSE(t, "sub2api", ratio) })
	}
}

func testCacheProtocolJSONAndSSE(t *testing.T, policyMode string, initialRatio float64) {
	for _, model := range []string{"claude-sonnet-4.5", "claude-sonnet-4.6", "claude-opus-5.5", "claude-haiku-4.5"} {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, path, stream), func(t *testing.T) {
					nativeInput := 10000
					h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": nativeInput, "outputTokens": 137}))
					})
					if err := config.UpdateModelMappings([]config.ModelMappingRule{{Key: "client-model", Value: model}}); err != nil {
						t.Fatal(err)
					}
					enabled, mode, ratio := true, policyMode, initialRatio
					key, err := config.AddApiKey(config.ApiKeyEntry{ID: "total-mode", Key: "sk-total-mode-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
					if err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					request := map[string]interface{}{"model": "client-model", "stream": stream, "max_tokens": 256}
					messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("stable total input ", 1000)}}
					if path == "/v1/responses" {
						request["input"], request["store"] = messages, false
					} else {
						request["messages"] = messages
					}
					semanticInput := estimateApproxTokens(strings.Repeat("stable total input ", 1000))
					previousCache := float64(0)
					for round := 0; round < 5; round++ {
						if round == 1 {
							request["model"] = strings.ReplaceAll(model, ".", "-")
						}
						if round == 2 {
							nativeInput = 11000
							messages = append(messages, map[string]interface{}{"role": "assistant", "content": "ok"}, map[string]interface{}{"role": "user", "content": strings.Repeat("appended input ", 100)})
							semanticInput += estimateApproxTokens("ok") + estimateApproxTokens(strings.Repeat("appended input ", 100))
							if path == "/v1/responses" {
								request["input"] = messages
							} else {
								request["messages"] = messages
							}
						}
						if round == 4 {
							ratio = 0
							if err := config.UpdateApiKey(key.ID, config.ApiKeyEntry{Enabled: true, PromptCacheEmulationRatio: &ratio}); err != nil {
								t.Fatal(err)
							}
						}
						body, _ := json.Marshal(request)
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
						expectedTotal := float64(nativeInput)
						expectedCacheable := expectedTotal
						if path == "/v1/messages" {
							var counted ClaudeRequest
							_ = json.Unmarshal(body, &counted)
							expectedTotal = float64(estimateClaudeRequestInputTokens(&counted))
							expectedCacheable = float64(semanticInput + 2*len(messages))
						}
						if policyMode == "sub2api" {
							if path != "/v1/messages" {
								var counted OpenAIRequest
								encoded, _ := json.Marshal(messages)
								_ = json.Unmarshal(encoded, &counted.Messages)
								expectedTotal = float64(estimateOpenAIRequestInputTokens(&counted))
							}
							expectedCacheable = math.Round(expectedTotal * initialRatio)
						}
						if total != expectedTotal || output != 137 || read+write > total || (round == 0 && (read != 0 || write != expectedCacheable)) || (round > 0 && round < 4 && read < previousCache-1) || ((round == 1 || round == 3) && write != 0) || (round == 2 && write <= 0) || (round == 4 && read+write != 0) {
							t.Fatalf("round %d: %v", round, usage)
						}
						previousCache = read + write
						t.Logf("round %d total=%.0f read=%.0f write=%.0f output=%.0f", round, total, read, write, output)
					}
				})
			}
		}
	}
}

func totalCachePolicy(ratio float64) config.PromptCacheSettings {
	return config.PromptCacheSettings{Enabled: true, Mode: "prefix_total", Ratio: ratio}
}

func commitTotalCacheFixture(tracker *promptCacheTracker, profile *promptCacheProfile, policy config.PromptCacheSettings) promptCacheUsage {
	usage := tracker.ComputeWithSettings("account", profile, policy)
	tracker.UpdateWithSettings("account", profile, policy, usage)
	return usage
}

func TestTotalCachePrefixAdmissionColdWarmPartialAndScaling(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 10000, InputFingerprint: [32]byte{31}, Breakpoints: []promptCacheBreakpoint{
		{Fingerprint: [32]byte{1}, CumulativeTokens: 5000, TTL: 5 * time.Minute},
		{Fingerprint: [32]byte{2}, CumulativeTokens: 8000, TTL: time.Hour},
	}}
	policy := totalCachePolicy(1)
	cold := tracker.ComputeWithSettings("account", profile, policy)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != 8000 {
		t.Fatalf("cold admitted prefix: %+v", cold)
	}
	first := &promptCacheProfile{Model: profile.Model, TotalInputTokens: 5000, Breakpoints: profile.Breakpoints[:1]}
	firstUsage := commitTotalCacheFixture(tracker, first, policy)
	partial := tracker.ComputeWithSettings("account", profile, policy)
	wantRead := firstUsage.CacheCreationInputTokens
	wantWrite := 3000
	if partial.CacheReadInputTokens != wantRead || partial.CacheCreationInputTokens != wantWrite || partial.CacheCreation5mInputTokens != 0 || partial.CacheCreation1hInputTokens != wantWrite {
		t.Fatalf("partial must only write new interval: %+v", partial)
	}
	tracker.UpdateWithSettings("account", profile, policy, partial)
	warm := tracker.ComputeWithSettings("account", profile, policy)
	if warm.CacheReadInputTokens != wantRead+wantWrite || warm.CacheCreationInputTokens != 0 || billedClaudeInputTokens(10000, warm) != 10000-wantRead-wantWrite {
		t.Fatalf("repeated content must not write: %+v", warm)
	}
	for _, snapshot := range []promptCacheUsage{cold, partial, warm} {
		usage := scalePromptCacheUsage(snapshot, profile, 20000)
		wantRead := minInt(snapshot.matchedCachedTokens, int(math.Round(float64(snapshot.rawRead)*2)))
		wantWrite := maxInt(0, int(math.Round(float64(snapshot.rawCacheable)*2))-int(math.Round(float64(snapshot.rawRead)*2)))
		if usage.CacheReadInputTokens != wantRead || usage.CacheCreationInputTokens != wantWrite || usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens {
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
	policy := totalCachePolicy(1)
	block := func(text string, marked bool) map[string]interface{} {
		b := map[string]interface{}{"type": "text", "text": text}
		if marked {
			b["cache_control"] = map[string]interface{}{"type": "ephemeral", "ttl": "1h"}
		}
		return b
	}
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", System: []interface{}{block(strings.Repeat("stable system ", 1000), false)}, Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{block(strings.Repeat("stable user ", 1000), true)}}}}
	first := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	commitTotalCacheFixture(tracker, first, policy)
	req.Messages[0].Content.([]interface{})[0].(map[string]interface{})["cache_control"] = nil
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "ok"}, ClaudeMessage{Role: "user", Content: []interface{}{block(strings.Repeat("new tail ", 400), true)}})
	next := tracker.BuildClaudeProfileWithSettings(req, estimateClaudeRequestInputTokens(req), policy)
	usage := tracker.ComputeWithSettings("account", next, policy)
	if usage.rawRead != first.Breakpoints[len(first.Breakpoints)-1].CumulativeTokens {
		t.Fatalf("moved marker lost old prefix: %+v", usage)
	}
}

func TestTotalCacheUniformNonemptyThreshold(t *testing.T) {
	for _, test := range []struct {
		model string
		floor int
	}{{"claude-sonnet-4.5", 1}, {"claude-haiku-4-5-20251001-thinking", 1}, {"claude-opus-4.6", 1}} {
		for _, n := range []int{test.floor - 1, test.floor} {
			profile := &promptCacheProfile{Model: test.model, TotalInputTokens: n, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: n, TTL: time.Hour}}}
			got := newPromptCacheTracker(time.Hour).ComputeWithSettings("account", profile, totalCachePolicy(1))
			if (got.CacheCreationInputTokens > 0) != (n == test.floor) {
				t.Fatalf("model %s prefix %d: %+v", test.model, n, got)
			}
		}
	}
	profile := &promptCacheProfile{Model: "", TotalInputTokens: 10000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
	if got := newPromptCacheTracker(time.Hour).ComputeWithSettings("account", profile, totalCachePolicy(1)); got.CacheCreationInputTokens != 0 {
		t.Fatalf("empty model fabricated cache: %+v", got)
	}
}
