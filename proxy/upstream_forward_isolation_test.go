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

func TestDedicatedUpstreamMessagesIsolateCredentialsNotPromptContent(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	h.pool.Reload()
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"upstream-model","content":[{"type":"text","text":"upstream"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	forwarding := &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "upstream-fixture-secret"}
	entries := []config.ApiKeyEntry{
		{Key: "normal-fixture-key", Enabled: true},
		{Key: "forward-fixture-key", Enabled: true, UpstreamForward: forwarding},
		{Key: "disabled-fixture-key", Enabled: false, UpstreamForward: forwarding},
	}
	for _, entry := range entries {
		if _, err := config.AddApiKey(entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, key, prompt string
		wantStatus        int
		wantCalls         int32
	}{
		{"ordinary_request_stays_local", entries[0].Key, "hello", http.StatusServiceUnavailable, 0},
		{"test_like_content_does_not_change_route", entries[0].Key, "1+1=?", http.StatusServiceUnavailable, 0},
		{"missing_key_is_rejected", "", "hello", http.StatusUnauthorized, 0},
		{"invalid_key_is_rejected", "invalid-fixture-key", "hello", http.StatusUnauthorized, 0},
		{"disabled_key_is_rejected", entries[2].Key, "hello", http.StatusUnauthorized, 0},
		{"ordinary_content_on_forward_key_is_forwarded", entries[1].Key, "hello", http.StatusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]interface{}{
				"model": "claude-sonnet-4.5", "max_tokens": 16,
				"messages": []map[string]string{{"role": "user", "content": tc.prompt}},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
			if tc.key != "" {
				req.Header.Set("X-Api-Key", tc.key)
			}
			req.Header.Set("User-Agent", "CCTest-fixture")
			rec := httptest.NewRecorder()
			before := calls.Load()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := calls.Load() - before; got != tc.wantCalls {
				t.Fatalf("upstream calls=%d want=%d", got, tc.wantCalls)
			}
		})
	}
}

func TestDedicatedUpstreamInitializesUsageDiagnostic(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"actual-upstream-model","usage":{"input_tokens":4,"output_tokens":17,"cache_creation_input_tokens":10,"cache_read_input_tokens":100}}`)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "client-diagnostic-fixture", Enabled: true,
		UpstreamForward: &config.UpstreamForwardConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "upstream-diagnostic-fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"requested-model","messages":[{"role":"user","content":"hello"}]}`))
	req = withApiKeyContext(withRequestTrace(req, requestEndpointClaude), &entry)
	rec := httptest.NewRecorder()
	if !h.forwardDedicatedUpstream(rec, req) || rec.Code != http.StatusOK {
		t.Fatalf("forward failed: %d %s", rec.Code, rec.Body.String())
	}
	d := requestTraceFromContext(req.Context()).usageDiagnostic
	if d == nil {
		t.Fatal("forwarded request has no usage diagnostic; route and upstream usage cannot be audited")
	}
	if d.Route != "dedicated_upstream" || d.Model != "actual-upstream-model" || d.ResponseStatus != http.StatusOK {
		t.Fatalf("unexpected route diagnostic: %+v", d)
	}
	if d.ReportedInputTokens != 4 || d.ReportedOutputTokens != 17 || d.CacheCreationInputTokens != 10 || d.CacheReadInputTokens != 100 {
		t.Fatalf("diagnostic did not retain upstream usage: %+v", d)
	}
	if d.CacheUsageEnabled || d.CacheUsageSource != "upstream" || d.InputTokenSource != "upstream" || d.OutputTokenSource != "upstream" {
		t.Fatalf("unexpected usage sources: %+v", d)
	}
	raw := formatUsageDiagnostic(*d)
	if strings.Contains(raw, entry.Key) || strings.Contains(raw, entry.UpstreamForward.APIKey) {
		t.Fatal("diagnostic leaked an API key")
	}
}
