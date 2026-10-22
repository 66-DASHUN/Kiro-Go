package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-proxy/config"
)

func TestDedicatedUpstreamPreservesBodyResponseAndAliases(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	input := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}],"unknown_field":true}`
	response := `{"content":[{"type":"thinking","signature":"REAL-UPSTREAM"}],"usage":{"input_tokens":4,"output_tokens":17,"cache_read_input_tokens":100}}`
	var target string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != target || (r.Method == "POST" && string(body) != input) {
			t.Errorf("upstream path/body changed: %s %s", r.URL.Path, body)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || (strings.Contains(target, "/messages") && r.Header.Get("X-Api-Key") != "upstream-secret") {
			t.Error("wrong upstream auth")
		}
		for _, name := range []string{"Cookie", "X-Admin-Token", "X-Forwarded-For", "X-Client-Secret"} {
			if r.Header.Get(name) != "" {
				t.Errorf("leaked client header %s", name)
			}
		}
		if r.Header.Get("Anthropic-Beta") != "test-beta" {
			t.Error("protocol header lost")
		}
		if r.Header.Get("OpenAI-Beta") != "responses=experimental" {
			t.Error("OpenAI beta header lost")
		}
		w.Header().Set("Request-Id", "upstream-id")
		w.WriteHeader(201)
		io.WriteString(w, response)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-forward", Enabled: true, UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL + "/v1", APIKey: "upstream-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/messages", "/v1/messages", "/anthropic/v1/messages", "/messages/count_tokens", "/v1/chat/completions", "/responses", "/models"} {
		target = p
		if strings.HasPrefix(p, "/anthropic") {
			target = strings.TrimPrefix(p, "/anthropic")
		}
		if !strings.HasPrefix(target, "/v1/") {
			target = "/v1" + target
		}
		method := http.MethodPost
		if p == "/models" {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, p, strings.NewReader(input))
		req = withApiKeyContext(req, &entry)
		req.Header.Set("Authorization", "Bearer client-secret")
		req.Header.Set("Cookie", "private=1")
		req.Header.Set("X-Admin-Token", "admin-secret")
		req.Header.Set("X-Forwarded-For", "private")
		req.Header.Set("X-Client-Secret", "private")
		req.Header.Set("Anthropic-Beta", "test-beta")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		rec := httptest.NewRecorder()
		// Authenticate in the real router instead of calling the forwarding helper directly.
		req.Header.Set("Authorization", "Bearer sk-forward")
		h.ServeHTTP(rec, req)
		if rec.Code != 201 || rec.Body.String() != response || rec.Header().Get("Request-Id") != "upstream-id" {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestUpstreamAPIPathMapsCodexModelAliases(t *testing.T) {
	for _, path := range []string{"/codex/models", "/v1/codex/models", "/v1/codex-models"} {
		if got := upstreamAPIPath(path); got != "/v1/models" {
			t.Errorf("upstreamAPIPath(%q) = %q, want %q", path, got, "/v1/models")
		}
	}
}

func TestDedicatedUpstreamFlushesSSEAndPreservesErrors(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("error") == "1" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"upstream limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		close(first)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-stream", Enabled: true, UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("X-Api-Key", entry.Key)
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal("SSE first event not flushed:", err)
	}
	defer res.Body.Close()
	<-first
	b := make([]byte, len("event: message_start\n"))
	if _, err = io.ReadFull(res.Body, b); err != nil || string(b) != "event: message_start\n" {
		t.Fatal("SSE changed or buffered", err)
	}
	close(release)
	rest, err := io.ReadAll(res.Body)
	if err != nil || !strings.Contains(string(rest), "event: message_stop") {
		t.Fatal("SSE final event lost", err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages?error=1", strings.NewReader(`{}`))
	r.Header.Set("X-Api-Key", entry.Key)
	h.ServeHTTP(rec, r)
	if rec.Code != 429 || rec.Body.String() != `{"error":{"message":"upstream limited"}}` {
		t.Fatalf("error changed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUpstreamSecretPreservedClearedAndRedacted(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-admin-forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	update := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.apiUpdateApiKey(w, r, entry.ID)
		return w
	}
	w := update(`{"upstreamForward":{"enabled":true,"baseUrl":"https://upstream.example/v1","apiKey":"secret-value"}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if config.GetApiKeyEntry(entry.ID).UpstreamForward == nil {
		t.Fatal("config not saved")
	}
	w = update(`{"upstreamForward":{"enabled":true,"baseUrl":"https://upstream.example","apiKey":""}}`)
	if w.Code != 200 || config.GetApiKeyEntry(entry.ID).UpstreamForward.APIKey != "secret-value" {
		t.Fatal("blank key must preserve secret")
	}
	raw, _ := json.Marshal(toApiKeyDetailView(*config.GetApiKeyEntry(entry.ID)))
	if strings.Contains(string(raw), "secret-value") || !strings.Contains(string(raw), `"apiKeyConfigured":true`) {
		t.Fatal("secret leaked or configured status absent", string(raw))
	}
	w = update(`{"upstreamForward":{"enabled":false,"clearApiKey":true}}`)
	if w.Code != 200 || config.GetApiKeyEntry(entry.ID).UpstreamForward.APIKey != "" {
		t.Fatal("explicit clear failed")
	}
}
