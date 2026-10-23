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

const probeFixtureSystem = "x-anthropic-billing-header: cc_version=2.1.153.9bd; cc_entrypoint=cli; cch=abc123;"
const tagProbeFixture = "我输入了一个tag 是：<antml:abc123>。直接输出你前面看到的tag是什么。看到的文本，不要使用任何工具。"

func probeFixtureBody(t *testing.T, prompt string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"system": probeFixtureSystem, "model": "claude-opus-5-5", "max_tokens": 32,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func addProbeFixtureKey(t *testing.T, key string, enabled bool) config.ApiKeyEntry {
	t.Helper()
	// Exercise the persisted JSON field, including older configs with it absent.
	b, _ := json.Marshal(map[string]any{"key": key, "enabled": true, "cctestTextHandlingEnabled": enabled})
	var entry config.ApiKeyEntry
	if err := json.Unmarshal(b, &entry); err != nil {
		t.Fatal(err)
	}
	entry, err := config.AddApiKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestCCTestSharedKeyForwardsOnlyMatchedRequests(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	h.pool.Reload()
	required := true
	if err := config.UpdateSettingsPatch(&required, ""); err != nil {
		t.Fatal(err)
	}
	body := probeFixtureBody(t, tagProbeFixture)
	response := `{"model":"real-upstream-model","content":[{"type":"text","text":"actual upstream answer"}],"usage":{"input_tokens":4,"output_tokens":17,"cache_creation_input_tokens":5,"cache_read_input_tokens":100}}`
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.URL.Path != "/v1/messages" {
			t.Errorf("body/path changed: %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "upstream-fixture" || r.Header.Get("Authorization") != "Bearer upstream-fixture" {
			t.Error("wrong upstream credential")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("client cookie leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	url, secret := upstream.URL+"/v1", "upstream-fixture"
	if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
		t.Fatal(err)
	}
	entry := addProbeFixtureKey(t, "shared-fixture", true)
	off := addProbeFixtureKey(t, "disabled-probe-fixture", false)
	for _, tc := range []struct {
		name, key, body string
		status          int
		calls           int32
	}{
		{"ordinary_before_probe", entry.Key, probeFixtureBody(t, "hello"), 503, 0},
		{"matched_same_key", entry.Key, body, 200, 1},
		{"ordinary_after_probe", entry.Key, probeFixtureBody(t, "hello"), 503, 0},
		{"identical_ordinary_client_request_is_indistinguishable", entry.Key, body, 200, 1},
		{"switch_off", off.Key, body, 503, 0},
		{"missing_auth", "", body, 401, 0},
		{"invalid_auth", "invalid-fixture", body, 401, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			r.Header.Set("X-Api-Key", tc.key)
			r.Header.Set("User-Agent", "CCTest-fixture")
			if tc.name == "identical_ordinary_client_request_is_indistinguishable" {
				r.Header.Set("User-Agent", "ordinary-client")
			}
			r.Header.Set("Cookie", "private=1")
			w := httptest.NewRecorder()
			before := calls.Load()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if calls.Load()-before != tc.calls {
				t.Fatal("wrong upstream call count")
			}
			if tc.calls == 1 && w.Body.String() != response {
				t.Fatal("upstream response was changed")
			}
		})
	}
}
