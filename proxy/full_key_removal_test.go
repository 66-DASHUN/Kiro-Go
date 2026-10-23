package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-proxy/config"
)

func TestRemovedFullKeyForwardingCannotRouteLegacyConfiguration(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	h.pool.Reload()
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"unexpected":"full-key route"}`)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	raw, _ := json.Marshal(map[string]any{"key": "legacy-full-key", "enabled": true, "upstreamForward": map[string]any{"enabled": true, "baseUrl": upstream.URL, "apiKey": "legacy-secret"}})
	var entry config.ApiKeyEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/messages", `{"model":"claude-sonnet-4.5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, 503},
		{"GET", "/v1/models", "", 200},
		{"POST", "/v1/messages", probeFixtureBody(t, tagProbeFixture), 503},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-Api-Key", entry.Key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if calls.Load() != 0 {
			t.Fatal("removed full-key settings still routed a request upstream")
		}
		if w.Code != tc.status {
			t.Fatalf("%s status=%d want=%d body=%s", tc.path, w.Code, tc.status, w.Body.String())
		}
	}
}

func TestRemovedFullKeyAdminFieldIsRejected(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	body := `{"key":"removed-setting","upstreamForward":{"enabled":true,"baseUrl":"https://upstream.example","apiKey":"legacy-secret"}}`
	w := httptest.NewRecorder()
	h.apiCreateApiKey(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "removed") {
		t.Fatalf("removed field silently accepted: %d %s", w.Code, w.Body.String())
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "existing", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.apiUpdateApiKey(w, httptest.NewRequest("PUT", "/", strings.NewReader(body)), entry.ID)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "removed") {
		t.Fatalf("removed patch accepted: %d", w.Code)
	}
	raw, _ := json.Marshal(toApiKeyDetailView(*config.GetApiKeyEntry(entry.ID)))
	if strings.Contains(string(raw), "upstreamForward") {
		t.Fatal("removed settings remain in admin view")
	}
}

func TestRemovedFullKeyDoesNotBypassCreditLimit(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	var entry config.ApiKeyEntry
	if err := json.Unmarshal([]byte(`{"key":"legacy-limit","enabled":true,"creditLimit":1,"creditsUsed":2,"upstreamForward":{"enabled":true,"baseUrl":"https://upstream.example","apiKey":"fixture"}}`), &entry); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Api-Key", entry.Key)
	if _, err := h.authenticate(r); err == nil {
		t.Fatal("removed full-key setting still bypasses credit limit")
	}
}
