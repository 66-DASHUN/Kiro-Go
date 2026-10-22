package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-proxy/config"
)

func TestDedicatedUpstreamKeyIgnoresHistoricalCreditLimit(t *testing.T) {
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "sk-forward-credit-limit", Enabled: true, CreditLimit: 1,
		UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: "https://upstream.example", APIKey: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.RecordApiKeyUsage(entry.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer "+entry.Key)
	if _, err := (&Handler{}).authenticate(req); err != nil {
		t.Fatalf("dedicated upstream key should not be blocked by historical credit limit: %v", err)
	}
}

func TestDedicatedUpstreamCorsAllowsOpenAIBeta(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,openai-beta")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")), "openai-beta") {
		t.Fatalf("OpenAI-Beta CORS preflight not allowed: status=%d allow=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestDedicatedUpstreamLoopGuardUsesOpenAIErrorEnvelope(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "sk-forward-loop", Enabled: true,
		UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: "https://kiro.example", APIKey: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Host = "kiro.example"
	req.Header.Set("Authorization", "Bearer "+entry.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("loop guard returned invalid JSON: %v", err)
	}
	if rec.Code != http.StatusBadGateway || envelope.Type != "" || envelope.Error.Message == "" {
		t.Fatalf("loop guard returned non-OpenAI error envelope: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDedicatedUpstreamLoopGuardUsesClaudeErrorEnvelope(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "sk-forward-loop-claude", Enabled: true,
		UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: "https://kiro.example", APIKey: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Host = "kiro.example"
	req.Header.Set("X-Api-Key", entry.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("loop guard returned non-Claude error envelope: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDedicatedUpstreamIsolatesKeysAndDoesNotFollowRedirects(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	enabled := true
	config.UpdateSettingsPatch(&enabled, "")
	calls := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "https://elsewhere.example/private")
		w.WriteHeader(307)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	forward, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-forward", Enabled: true, UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	normal, _ := config.AddApiKey(config.ApiKeyEntry{Key: "sk-normal", Enabled: true})
	disabled, _ := config.AddApiKey(config.ApiKeyEntry{Key: "sk-disabled", Enabled: false, UpstreamForward: forward.UpstreamForward})
	for _, tc := range []struct {
		key    string
		status int
	}{{normal.Key, 200}, {disabled.Key, 401}, {"sk-invalid", 401}, {forward.Key, 307}} {
		req := httptest.NewRequest("GET", "/models", nil)
		req.Header.Set("Authorization", "Bearer "+tc.key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: %d", tc.key, rec.Code)
		}
	}
	if calls != 1 {
		t.Fatal("normal/disabled requests leaked or redirect followed", calls)
	}
}

func TestDedicatedUpstreamRecordsActualUsageWithoutKiroCredits(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := newAdminAuthTestHandler(t, "admin")
		required := true
		config.UpdateSettingsPatch(&required, "")
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"actual-model\",\"usage\":{\"input_tokens\":4,\"output_tokens\":0,\"cache_creation_input_tokens\":10,\"cache_read_input_tokens\":100}}}\n\n")
				io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":17}}\n\n")
			} else {
				io.WriteString(w, `{"model":"actual-model","usage":{"input_tokens":4,"output_tokens":17,"cache_creation_input_tokens":10,"cache_read_input_tokens":100}}`)
			}
		}))
		h.upstreamClient = upstream.Client()
		entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-usage", Enabled: true, UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "secret"}})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"requested-model"}`))
		req.Header.Set("X-Api-Key", entry.Key)
		h.ServeHTTP(rec, req)
		got := config.GetApiKeyEntry(entry.ID)
		if got.RequestsCount != 1 || got.TokensUsed != 131 || got.CreditsUsed != 0 {
			t.Fatalf("stream=%v: usage=%+v", stream, got)
		}
		upstream.Close()
	}
}

func TestDedicatedUpstreamTransportFailureUsesRouteErrorShape(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want string
	}{{"/v1/messages", `"type":"error"`}, {"/v1/chat/completions", `"error":{"message"`}} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-error-" + strings.ReplaceAll(tc.path, "/", "-"), Enabled: true, UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: server.URL, APIKey: "secret"}})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		server.Close()
		method := http.MethodPost
		req := httptest.NewRequest(method, tc.path, strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Api-Key", entry.Key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s returned %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestDedicatedUpstreamTransportFailureUsesIncomingProtocolPath(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "sk-error-openai-messages-upstream-path", Enabled: true,
		UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: server.URL + "/messages", APIKey: "secret"},
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Api-Key", entry.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("transport failure returned invalid JSON: %v", err)
	}
	if rec.Code != http.StatusBadGateway || envelope.Type != "" || envelope.Error.Message == "" {
		t.Fatalf("OpenAI transport failure used wrong error envelope: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
