package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/auth"
	"kiro-proxy/config"
	"kiro-proxy/db"
	"kiro-proxy/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func initRuntimeParityConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
}

func TestRuntimeCachePolicyRatiosDisableAndConfiguredReference(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{TotalInputTokens: 10000, Model: "claude-sonnet-4.5", Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 10000, TTL: time.Hour}}}
	policy := config.PromptCacheSettings{Enabled: true, Mode: config.PromptCacheModeIndependent, CreationRatio: 0.25, ReadRatio: 1}
	first := tracker.ComputeWithSettings("a", profile, policy)
	if first.CacheCreationInputTokens < 2000 || first.CacheCreationInputTokens > 3000 || first.CacheCreation1hInputTokens != first.CacheCreationInputTokens {
		t.Fatalf("creation=%+v", first)
	}
	tracker.Update("a", profile)
	second := tracker.ComputeWithSettings("a", profile, policy)
	if second.CacheReadInputTokens != 10000 || second.CacheCreationInputTokens != 0 {
		t.Fatalf("read=%+v", second)
	}
	policy.ReadRatio = 0.5
	if got := tracker.ComputeWithSettings("a", profile, policy); got.CacheReadInputTokens < 4500 || got.CacheReadInputTokens > 5500 {
		t.Fatalf("ratio=%+v", got)
	}
	policy.Enabled = false
	if got := tracker.ComputeWithSettings("a", profile, policy); got != (promptCacheUsage{}) {
		t.Fatalf("disabled=%+v", got)
	}
}

func TestRuntimeClaudeDefaultCacheProfileWithoutMarkers(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: strings.Repeat("stable prefix ", 1000)}}}
	profile := tracker.BuildClaudeProfile(req, 10000)
	if profile == nil {
		t.Fatal("default profile missing")
	}
	if profile.Breakpoints[len(profile.Breakpoints)-1].CumulativeTokens != 10000 {
		t.Fatalf("profile=%+v", profile)
	}
}

func TestRuntimeAccountAffinityNeverEscapesKeyBinding(t *testing.T) {
	initRuntimeParityConfig(t)
	for _, id := range []string{"a", "b"} {
		if _, err := config.AddApiKey(config.ApiKeyEntry{ID: id, Key: "sk-" + id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := config.AddAccount(config.Account{ID: "acc-" + id, ApiKeyID: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	p := pool.GetPool()
	p.Reload()
	h := &Handler{pool: p, affinity: newSessionAffinity()}
	payload := payloadFor("same-conversation")
	h.affinity.Bind(affinityKey(payload), "acc-b")
	ctx := context.WithValue(context.Background(), apiKeyContextKey{}, apiKeyContextValue{id: "a"})
	if got := h.pickAccountForContext(ctx, payload, "model", nil); got == nil || got.ID != "acc-a" {
		t.Fatalf("affinity selected=%v", got)
	}
	if got := h.pickAccountForContext(ctx, payload, "model", map[string]bool{"acc-a": true}); got != nil {
		t.Fatalf("retry escaped group: %v", got)
	}
}

func TestRuntimeAdminCacheOverridesAndSafeBindingFields(t *testing.T) {
	initRuntimeParityConfig(t)
	h := &Handler{pool: pool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiCreateApiKey(rec, httptest.NewRequest("POST", "/admin/api/api-keys", strings.NewReader(`{"name":"Group A","key":"sk-safe-binding-secret","promptCacheEmulationEnabled":false,"promptCacheEmulationMode":"independent","promptCacheCreationEmulationRatio":0.25,"promptCacheReadEmulationRatio":0.75}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	key := config.GetApiKeyEntry(created.ID)
	if key == nil || key.PromptCacheEmulationEnabled == nil || *key.PromptCacheEmulationEnabled {
		t.Fatal("disabled override lost")
	}
	if err := config.AddAccount(config.Account{ID: "bound", ApiKeyID: created.ID}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.apiGetAccounts(rec, httptest.NewRequest("GET", "/admin/api/accounts", nil))
	var accounts []map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &accounts)
	if len(accounts) != 1 || accounts[0]["apiKeyId"] != created.ID || accounts[0]["apiKeyName"] != "Group A" {
		t.Fatalf("accounts=%v", accounts)
	}
	if strings.Contains(rec.Body.String(), key.Key) {
		t.Fatal("account listing exposed API key secret")
	}
	rec = httptest.NewRecorder()
	h.apiListApiKeys(rec, httptest.NewRequest("GET", "/admin/api/api-keys", nil))
	var listed struct {
		Keys []map[string]interface{} `json:"apiKeys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Keys) != 1 || listed.Keys[0]["accountCount"] != float64(1) || listed.Keys[0]["promptCacheEmulationEnabled"] != false {
		t.Fatalf("keys=%v", listed.Keys)
	}
	if strings.Contains(rec.Body.String(), key.Key) {
		t.Fatal("key listing exposed secret")
	}
	rec = httptest.NewRecorder()
	h.apiUpdateApiKey(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"clearPromptCacheOverrides":true}`)), created.ID)
	if rec.Code != 200 || config.GetApiKeyEntry(created.ID).PromptCacheEmulationEnabled != nil {
		t.Fatal("clear overrides failed")
	}
}

func TestRuntimeAccountAndAuthRejectInvalidKeyBinding(t *testing.T) {
	initRuntimeParityConfig(t)
	h := &Handler{pool: pool.GetPool()}
	cases := []struct {
		name, body string
		handler    func(http.ResponseWriter, *http.Request)
	}{
		{"add", `{"id":"invalid","apiKeyId":"missing"}`, h.apiAddAccount},
		{"import", `{"kiroApiKey":"ksk_test","apiKeyId":"missing"}`, h.apiImportCredentials},
		{"iam-start", `{"startUrl":"https://example.com","apiKeyId":"missing"}`, h.apiStartIamSso},
		{"iam-complete", `{"sessionId":"none","apiKeyId":"missing"}`, h.apiCompleteIamSso},
		{"builder-start", `{"apiKeyId":"missing"}`, h.apiStartBuilderIdLogin},
		{"builder-poll", `{"sessionId":"none","apiKeyId":"missing"}`, h.apiPollBuilderIdAuth},
		{"sso-import", `{"bearerToken":"token","apiKeyId":"missing"}`, h.apiImportSsoToken},
		{"microsoft-start", `{"apiKeyId":"missing"}`, h.apiStartMicrosoftSSO},
		{"microsoft-complete", `{"sessionId":"none","callbackUrl":"https://example.com","apiKeyId":"missing"}`, h.apiCompleteMicrosoftSSO},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(tc.body)))
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "API key") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRuntimeSettingsCachePointerPatchPreservesOmittedValues(t *testing.T) {
	initRuntimeParityConfig(t)
	h := &Handler{}
	if err := config.UpdatePromptCacheSettings(true, "independent", 1, 0.25, 0.75); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.apiUpdateSettings(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"promptCacheEmulationEnabled":false,"promptCacheReadEmulationRatio":0}`)))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := config.EffectivePromptCacheSettings("")
	if got.Enabled || got.CreationRatio != 0.25 || got.ReadRatio != 0 {
		t.Fatalf("settings=%+v", got)
	}
	rec = httptest.NewRecorder()
	h.apiUpdateSettings(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"promptCacheReadEmulationRatio":2}`)))
	if rec.Code != 400 {
		t.Fatalf("invalid ratio status=%d", rec.Code)
	}
}

func TestRuntimeHistoricalCountersCombineWithoutInventingRows(t *testing.T) {
	initRuntimeParityConfig(t)
	resetObservePersistenceForTest(t)
	if err := config.UpdateStats(10, 8, 2, 900, 1.5); err != nil {
		t.Fatal(err)
	}
	if err := persistRequestRecord(requestRecord{Success: true, InTokens: 40, OutTokens: 20, TotalTokens: 60, Credits: 0.1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := queryPersistedRequestStats()
		if err != nil {
			t.Fatal(err)
		}
		if got.TotalRequests != 11 || got.SuccessRequests != 9 || got.FailedRequests != 2 || got.TotalTokens != 960 || got.TotalCredits != 1.6 {
			t.Fatalf("iteration %d stats=%+v", i, got)
		}
		if got.SuccessInTokens != 40 || got.SuccessOutTokens != 20 {
			t.Fatalf("invented historical split: %+v", got)
		}
	}
	d, _ := db.Get()
	var rows int
	_ = d.QueryRow("SELECT COUNT(*) FROM requests").Scan(&rows)
	if rows != 1 {
		t.Fatalf("invented rows=%d", rows)
	}
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiResetStats(rec, httptest.NewRequest("POST", "/", nil))
	got, err := queryPersistedRequestStats()
	if err != nil {
		t.Fatal(err)
	}
	if got != (persistedRequestStats{}) {
		t.Fatalf("reset stats=%+v", got)
	}
}

func TestRuntimeAdminSessionsEqualExpiryEvictsInIssueOrder(t *testing.T) {
	store := newAdminSessionStore()
	tokens := make([]string, 0, maxAdminSessions)
	for i := 0; i < maxAdminSessions; i++ {
		token, _, err := store.Issue()
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	tied := time.Now().Add(time.Hour)
	store.mu.Lock()
	for _, token := range tokens {
		store.sessions[token] = tied
	}
	store.mu.Unlock()
	for i := 0; i < 10; i++ {
		if _, _, err := store.Issue(); err != nil {
			t.Fatal(err)
		}
		if store.Valid(tokens[i]) {
			t.Fatalf("issued session %d survived when expiries tied", i)
		}
	}
}

func TestRuntimeOpenAIDefaultCacheForRepeatedSingleTurn(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := testKiroPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("a stable prompt ", 1000)
	profile := tracker.BuildOpenAIProfile(payload, 10000)
	if profile == nil {
		t.Fatal("single turn default profile missing")
	}
	tracker.Update("account", profile)
	got := tracker.Compute("account", profile)
	if got.CacheReadInputTokens != 10000 {
		t.Fatalf("read=%+v", got)
	}
}

func runtimeProtocolRequest(t *testing.T, h *Handler, path string, stream bool, key *config.ApiKeyEntry) *httptest.ResponseRecorder {
	t.Helper()
	messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("stable prefix ", 1500)}}
	request := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 64}
	if path == "/v1/responses" {
		request["input"] = messages
		request["store"] = false
	} else {
		request["messages"] = messages
	}
	body, _ := json.Marshal(request)
	r := withApiKeyContext(httptest.NewRequest("POST", path, strings.NewReader(string(body))), key)
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/messages":
		h.handleClaudeMessages(rec, r)
	case "/v1/chat/completions":
		h.handleOpenAIChat(rec, r)
	default:
		h.handleOpenAIResponses(rec, r)
	}
	return rec
}

func runtimeProtocolUsage(t *testing.T, rec *httptest.ResponseRecorder, stream bool) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	if !stream {
		if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		return obj["usage"].(map[string]interface{})
	}
	var result map[string]interface{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj) != nil {
			continue
		}
		if usage, ok := obj["usage"].(map[string]interface{}); ok {
			result = usage
		}
		if nested, ok := obj["response"].(map[string]interface{}); ok {
			if usage, ok := nested["usage"].(map[string]interface{}); ok {
				result = usage
			}
		}
	}
	if result == nil {
		t.Fatal("final usage missing")
	}
	return result
}

func TestRuntimeEveryProtocolPreservesNativeCountersAndEffectiveCachePolicy(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				t.Run(path+fmt.Sprint(stream, enabled), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 10}))
					})
					create, read, mode := 0.25, 0.5, "independent"
					key, err := config.AddApiKey(config.ApiKeyEntry{ID: "key-a", Key: "sk-runtime-a", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheCreationEmulationRatio: &create, PromptCacheReadEmulationRatio: &read})
					if err != nil {
						t.Fatal(err)
					}
					h.pool.Reload()
					for i := 0; i < 2; i++ {
						rec := runtimeProtocolRequest(t, h, path, stream, &key)
						if rec.Code != 200 {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
						}
						usage := runtimeProtocolUsage(t, rec, stream)
						if path == "/v1/messages" {
							creation, _ := usage["cache_creation_input_tokens"].(float64)
							cached, _ := usage["cache_read_input_tokens"].(float64)
							if usage["input_tokens"].(float64)+creation+cached != 10000 || usage["output_tokens"] != float64(10) {
								t.Fatalf("native counters changed: %v", usage)
							}
							if !enabled && (creation != 0 || cached != 0) || enabled && (i == 0 && (creation < 2000 || creation > 3000 || cached != 0) || i == 1 && (cached < 4500 || cached > 5500 || creation != 0)) {
								t.Fatalf("policy iteration %d usage=%v", i, usage)
							}
						} else {
							inField, outField, detailField := "prompt_tokens", "completion_tokens", "prompt_tokens_details"
							if path == "/v1/responses" {
								inField, outField, detailField = "input_tokens", "output_tokens", "input_tokens_details"
							}
							if usage[inField] != float64(10000) || usage[outField] != float64(10) {
								t.Fatalf("native counters changed: %v", usage)
							}
							details, _ := usage[detailField].(map[string]interface{})
							if !enabled && details != nil {
								t.Fatalf("disabled policy reported cache: %v", usage)
							}
							if enabled {
								if details == nil {
									t.Fatalf("cache details missing: %v", usage)
								}
								creation := details["cache_write_tokens"].(float64)
								cached := details["cached_tokens"].(float64)
								if i == 0 && (creation < 2000 || creation > 3000 || cached != 0) {
									t.Fatalf("creation ratio wrong: %v", details)
								}
								if i == 1 && (cached < 4500 || cached > 5500 || creation != 0) {
									t.Fatalf("read ratio wrong: %v", details)
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestRuntimeEveryProtocolRejectsAccountsBoundToAnotherKey(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+fmt.Sprint(stream), func(t *testing.T) {
				upstreamCalls := 0
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls++
					writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), meteringFrame(t))
				})
				other, err := config.AddApiKey(config.ApiKeyEntry{ID: "other-key", Key: "sk-other", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				requested, err := config.AddApiKey(config.ApiKeyEntry{ID: "requested", Key: "sk-requested", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				account := config.GetAccounts()[0]
				account.ApiKeyID = other.ID
				if err := config.UpdateAccountApiKeyID(account.ID, other.ID); err != nil {
					t.Fatal(err)
				}
				h.pool.Reload()
				rec := runtimeProtocolRequest(t, h, path, stream, &requested)
				if rec.Code != 503 || !strings.Contains(rec.Body.String(), "No available accounts") || upstreamCalls != 0 {
					t.Fatalf("status=%d upstream=%d body=%s", rec.Code, upstreamCalls, rec.Body.String())
				}
			})
		}
	}
}

func TestRuntimeAccountBindingPatchRebindUnbindPreservesCredentials(t *testing.T) {
	initRuntimeParityConfig(t)
	for _, id := range []string{"a", "b"} {
		if _, err := config.AddApiKey(config.ApiKeyEntry{ID: id, Key: "sk-" + id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.AddAccount(config.Account{ID: "bound", ApiKeyID: "a", AccessToken: "fresh-token", RefreshToken: "fresh-refresh"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{pool: pool.GetPool()}
	for _, binding := range []string{"b", ""} {
		body, _ := json.Marshal(map[string]interface{}{"apiKeyId": binding, "nickname": "updated"})
		rec := httptest.NewRecorder()
		h.apiUpdateAccount(rec, httptest.NewRequest("PUT", "/", strings.NewReader(string(body))), "bound")
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		got := config.GetAccounts()[0]
		if got.ApiKeyID != binding || got.Nickname != "updated" || got.AccessToken != "fresh-token" || got.RefreshToken != "fresh-refresh" {
			t.Fatalf("binding=%q account=%+v", binding, got)
		}
	}
	rec := httptest.NewRecorder()
	h.apiUpdateAccount(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"nickname":"next"}`)), "bound")
	if config.GetAccounts()[0].ApiKeyID != "" {
		t.Fatal("omitted binding changed explicit unbind")
	}
}

func TestRuntimeImportAndExportPreserveKiroAPIKeyAndBinding(t *testing.T) {
	initRuntimeParityConfig(t)
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "group", Name: "Group", Key: "sk-group", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	oldClient := kiroRestHttpStore.Load()
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("network disabled") })})
	t.Cleanup(func() { kiroRestHttpStore.Store(oldClient) })
	h := &Handler{pool: pool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"kiroApiKey":"ksk_import_binding","apiKeyId":"group"}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].ApiKeyID != key.ID {
		t.Fatalf("accounts=%+v", accounts)
	}
	rec = httptest.NewRecorder()
	h.apiGetAccountFull(rec, httptest.NewRequest("GET", "/", nil), accounts[0].ID)
	var full map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &full)
	if full["apiKeyId"] != key.ID {
		t.Fatalf("full=%v", full)
	}
	rec = httptest.NewRecorder()
	h.apiExportAccounts(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
	var payload struct {
		Accounts []struct {
			ApiKeyID    string                 `json:"apiKeyId"`
			Credentials map[string]interface{} `json:"credentials"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if len(payload.Accounts) != 1 {
		t.Fatal("export account missing")
	}
	exported := payload.Accounts[0]
	if exported.ApiKeyID != key.ID || exported.Credentials["apiKeyId"] != key.ID || exported.Credentials["accessToken"] != "ksk_import_binding" || exported.Credentials["authMethod"] != "api_key" {
		t.Fatalf("export=%v", exported)
	}
}

func TestRuntimeSettingsCachePatchPreservesDormantIndependentRatios(t *testing.T) {
	initRuntimeParityConfig(t)
	if err := config.UpdatePromptCacheSettings(true, "uniform", 0.5, 0.2, 0.7); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	for _, body := range []string{`{"promptCacheEmulationEnabled":false}`, `{"promptCacheEmulationMode":"independent"}`} {
		rec := httptest.NewRecorder()
		h.apiUpdateSettings(rec, httptest.NewRequest("PUT", "/", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		got := config.Get()
		if got.PromptCacheCreationEmulationRatio != 0.2 || got.PromptCacheReadEmulationRatio != 0.7 {
			t.Fatalf("omitted fields changed: %+v", config.EffectivePromptCacheSettings(""))
		}
	}
}

func TestRuntimeMicrosoftProfileSelectionPreservesSnapshotAndExplicitUnbind(t *testing.T) {
	for _, binding := range []string{"a", ""} {
		t.Run(binding, func(t *testing.T) {
			h := newMicrosoftHandlerForTest(t)
			for _, id := range []string{"a", "b"} {
				if _, err := config.AddApiKey(config.ApiKeyEntry{ID: id, Key: "sk-" + id, Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
			flows := microsoftSSOFlows
			flows.mu.Lock()
			selected, err := flows.storeSelectionLocked("s", config.Account{ID: "ms", ApiKeyID: "a", AuthMethod: "external_idp", RefreshToken: "token", Enabled: true}, []KiroProfile{{ARN: "offered"}}, time.Now())
			flows.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			requested := "b"
			if binding == "" {
				requested = ""
			}
			body, _ := json.Marshal(map[string]string{"selectionId": selected, "profileArn": "offered", "apiKeyId": requested})
			rec := httptest.NewRecorder()
			h.apiSelectMicrosoftSSOProfile(rec, httptest.NewRequest("POST", "/", strings.NewReader(string(body))))
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if got := config.GetAccounts()[0].ApiKeyID; got != binding {
				t.Fatalf("binding=%q expected=%q", got, binding)
			}
		})
	}
}

func TestRuntimeConcurrentColdRequestsKeepDispatchCacheMiss(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			firstEntered := make(chan struct{})
			releaseFirst := make(chan struct{})
			var calls atomic.Int32
			h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(firstEntered)
					<-releaseFirst
				}
				writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 10}))
			})
			create, read, mode, enabled := 0.25, 0.5, "independent", true
			key, err := config.AddApiKey(config.ApiKeyEntry{ID: "concurrent", Key: "sk-concurrent", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheCreationEmulationRatio: &create, PromptCacheReadEmulationRatio: &read})
			if err != nil {
				t.Fatal(err)
			}
			h.pool.Reload()
			firstResult := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstResult <- runtimeProtocolRequest(t, h, path, false, &key) }()
			<-firstEntered
			second := runtimeProtocolRequest(t, h, path, false, &key)
			close(releaseFirst)
			first := <-firstResult
			for i, rec := range []*httptest.ResponseRecorder{first, second} {
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				usage := runtimeProtocolUsage(t, rec, false)

				creation, _ := usage["cache_creation_input_tokens"].(float64)
				cached, _ := usage["cache_read_input_tokens"].(float64)
				if path != "/v1/messages" {
					field := "prompt_tokens_details"
					if path == "/v1/responses" {
						field = "input_tokens_details"
					}
					details, _ := usage[field].(map[string]interface{})
					creation, _ = details["cache_write_tokens"].(float64)
					cached, _ = details["cached_tokens"].(float64)
				}
				if creation < 2000 || creation > 3000 || cached != float64(0) {
					t.Fatalf("request %d changed dispatch miss after concurrent completion: %v", i, usage)
				}
			}
		})
	}
}

func TestRuntimeNativeWebSearchLoopUsesEffectiveCachePolicy(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn", "inputTokens": 10000, "outputTokens": 10}))
	})
	create, read, mode := 0.25, 0.5, "independent"
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "search-key", Key: "sk-search", Enabled: true, PromptCacheEmulationMode: &mode, PromptCacheCreationEmulationRatio: &create, PromptCacheReadEmulationRatio: &read})
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 64, "messages": []map[string]string{{"role": "user", "content": strings.Repeat("stable prompt ", 1500)}}, "tools": []map[string]string{{"name": "web_search", "type": "web_search_20250305"}}})
		rec := httptest.NewRecorder()
		r := withApiKeyContext(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))), &key)
		h.handleClaudeMessages(rec, r)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		usage := runtimeProtocolUsage(t, rec, false)
		creation, _ := usage["cache_creation_input_tokens"].(float64)
		cached, _ := usage["cache_read_input_tokens"].(float64)
		if (i == 0 && (creation < 2000 || creation > 3000 || cached != 0)) || (i == 1 && (cached < 4500 || cached > 5500 || creation != 0)) || usage["input_tokens"].(float64)+creation+cached != 10000 {
			t.Fatalf("round %d usage=%v", i, usage)
		}
	}
}

func TestRuntimeRepeatedCredentialImportRejectsOriginalAfterRotation(t *testing.T) {
	initRuntimeParityConfig(t)
	t.Cleanup(installCleanAuthClient(t))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"accessToken":"new-access","refreshToken":"rotated-%d","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:profile/test"}`, calls)
	}))
	t.Cleanup(server.Close)
	oldOIDC := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return server.URL })
	t.Cleanup(func() { auth.SetOIDCTokenURLForTest(oldOIDC) })
	h := &Handler{pool: pool.GetPool()}
	body := `{"refreshToken":"original-refresh","clientId":"c","clientSecret":"s","authMethod":"idc"}`
	first := importCredentials(h, body)
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	second := importCredentials(h, body)
	if second.Code != 409 || len(config.GetAccounts()) != 1 || calls != 1 {
		t.Fatalf("status=%d accounts=%d refresh calls=%d", second.Code, len(config.GetAccounts()), calls)
	}
}

func TestRuntimeNativeInputBelowThresholdDoesNotReportCache(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{Model: "claude-sonnet-4.5", TotalInputTokens: 2000, Breakpoints: []promptCacheBreakpoint{{Fingerprint: [32]byte{1}, CumulativeTokens: 2000, TTL: time.Minute}}}
	usage := tracker.Compute("account", profile)
	scaled := scalePromptCacheUsage(usage, profile, 500)
	if scaled.CacheCreationInputTokens != 0 || scaled.CacheReadInputTokens != 0 {
		t.Fatalf("below native threshold=%+v", scaled)
	}
}
