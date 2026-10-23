package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/config"
)

func TestCCTestTransportKeepsProtocolHeadersAndStripsCredentials(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Cookie", "X-Admin-Token", "X-Forwarded-For", "X-Client-Secret"} {
			if r.Header.Get(name) != "" {
				t.Errorf("leaked header %s", name)
			}
		}
		if r.Header.Get("Anthropic-Beta") != "fixture-beta" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("protocol headers missing")
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "upstream-secret" {
			t.Error("wrong upstream credential")
		}
		w.Header().Set("Set-Cookie", "upstream-session=private")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"actual","usage":{"input_tokens":4,"output_tokens":17,"cache_creation_input_tokens":10,"cache_read_input_tokens":100}}`)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	url, secret := upstream.URL, "upstream-secret"
	if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
		t.Fatal(err)
	}
	entry := addProbeFixtureKey(t, "shared-headers", true)
	r := withApiKeyContext(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture))), &entry)
	for _, name := range []string{"Cookie", "X-Admin-Token", "X-Forwarded-For", "X-Client-Secret", "X-Api-Key", "Authorization"} {
		r.Header.Set(name, "client-private")
	}
	r.Header.Set("Anthropic-Beta", "fixture-beta")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	if !h.forwardCCTestProbe(w, r) || w.Code != 200 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("forwarding failed status=%d", w.Code)
	}
	got := config.GetApiKeyEntry(entry.ID)
	if got.TokensUsed != 131 || got.CreditsUsed != 0 || got.RequestsCount != 1 {
		t.Fatalf("upstream usage changed: tokens=%d credits=%v requests=%d", got.TokensUsed, got.CreditsUsed, got.RequestsCount)
	}
}

func TestCCTestLoopAndTransportErrorsUseClaudeEnvelope(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry := addProbeFixtureKey(t, "shared-loop", true)
	for _, loop := range []bool{true, false} {
		url, secret := "https://kiro.example", "fixture"
		if !loop {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			url = server.URL
			server.Close()
		}
		if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
			t.Fatal(err)
		}
		r := withApiKeyContext(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture))), &entry)
		r.Host = "kiro.example"
		w := httptest.NewRecorder()
		if !h.forwardCCTestProbe(w, r) || w.Code != 502 || !strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatalf("loop=%v status=%d body=%s", loop, w.Code, w.Body.String())
		}
	}
}

func TestCCTestTransportFlushesBeforeUpstreamCompletes(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	url, secret := upstream.URL, "stream-fixture"
	if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
		t.Fatal(err)
	}
	entry := addProbeFixtureKey(t, "shared-live-stream", true)
	server := httptest.NewServer(h)
	defer server.Close()
	defer once.Do(func() { close(release) })
	request, err := http.NewRequest("POST", server.URL+"/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Api-Key", entry.Key)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("first SSE chunk buffered:", err)
	}
	defer response.Body.Close()
	first := make([]byte, len("event: message_start\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "event: message_start\n" {
		t.Fatal("first event unavailable before completion", err)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(rest), "event: message_stop") {
		t.Fatal("last event lost", err)
	}
}

func TestOpenAIBetaCORSRemainsAvailable(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	r := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	r.Header.Set("Origin", "https://client.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "authorization,content-type,openai-beta")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), "openai-beta") {
		t.Fatal("unrelated CORS handling changed")
	}
}
