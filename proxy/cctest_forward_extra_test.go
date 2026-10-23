package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-proxy/config"
)

type probeTrackedBody struct {
	io.Reader
	closed bool
}

func (b *probeTrackedBody) Close() error { b.closed = true; return nil }

type probeBrokenReader struct{}

func (probeBrokenReader) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }

func TestCCTestUnmatchedBodiesArePreserved(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry := addProbeFixtureKey(t, "shared-preserve", true)
	for _, tc := range []struct {
		name, body string
		length     int64
	}{
		{"ordinary", probeFixtureBody(t, "normal message"), -1},
		{"invalid_json", "{broken", -1},
		{"oversized_chunked", probeFixtureBody(t, tagProbeFixture) + strings.Repeat(" ", maxCCTestProbeBytes), -1},
		{"oversized_known_length", probeFixtureBody(t, tagProbeFixture) + strings.Repeat(" ", maxCCTestProbeBytes), maxCCTestProbeBytes + 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracked := &probeTrackedBody{Reader: strings.NewReader(tc.body)}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			r.Body, r.ContentLength = tracked, tc.length
			r = withApiKeyContext(r, &entry)
			if h.forwardCCTestProbe(httptest.NewRecorder(), r) {
				t.Fatal("unexpected forwarding")
			}
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != tc.body {
				t.Fatal("unmatched body was consumed or changed")
			}
			if tracked.closed {
				t.Fatal("body closed before downstream reads")
			}
			r.Body.Close()
			if !tracked.closed {
				t.Fatal("original body close not propagated")
			}
		})
	}
}

func TestCCTestReadFailureDoesNotForwardPartialProbe(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry := addProbeFixtureKey(t, "broken-body", true)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader(probeFixtureBody(t, tagProbeFixture)), probeBrokenReader{}))
	r = withApiKeyContext(r, &entry)
	w := httptest.NewRecorder()
	if !h.forwardCCTestProbe(w, r) || w.Code != 400 {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCCTestUnavailableConfigurationOnlyRejectsMatchedProbe(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry := addProbeFixtureKey(t, "incomplete-config", true)
	for _, prompt := range []string{"hello", tagProbeFixture} {
		r := withApiKeyContext(httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(probeFixtureBody(t, prompt))), &entry)
		w := httptest.NewRecorder()
		handled := h.forwardCCTestProbe(w, r)
		if prompt == "hello" && handled {
			t.Fatal("incomplete probe config intercepted ordinary request")
		}
		if prompt == tagProbeFixture && (!handled || w.Code != 502) {
			t.Fatal("matched probe silently fell back instead of reporting config error")
		}
	}
}

func TestCCTestOnlyMessagesEndpoints(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	entry := addProbeFixtureKey(t, "path-fixture", true)
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/models", "/v1/chat/completions", "/v1/responses"} {
		r := withApiKeyContext(httptest.NewRequest(http.MethodPost, path, strings.NewReader(probeFixtureBody(t, tagProbeFixture))), &entry)
		if h.forwardCCTestProbe(httptest.NewRecorder(), r) {
			t.Fatalf("unsupported path forwarded: %s", path)
		}
	}
	r := withApiKeyContext(httptest.NewRequest(http.MethodGet, "/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture))), &entry)
	if h.forwardCCTestProbe(httptest.NewRecorder(), r) {
		t.Fatal("GET probe forwarded")
	}
}

func TestCCTestSSEPassThroughAndUsageDiagnostic(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	body := strings.Replace(probeFixtureBody(t, tagProbeFixture), `"max_tokens":32`, `"max_tokens":32,"stream":true`, 1)
	response := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"actual-model\",\"usage\":{\"input_tokens\":4,\"output_tokens\":0,\"cache_creation_input_tokens\":10,\"cache_read_input_tokens\":100}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"fixture-signature-unchanged\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":17}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.URL.Path != "/v1/messages" {
			t.Error("stream request was changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "upstream-fixture-id")
		io.WriteString(w, response)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	url, secret := upstream.URL, "stream-fixture-secret"
	if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
		t.Fatal(err)
	}
	entry := addProbeFixtureKey(t, "shared-stream", true)
	for _, path := range []string{"/v1/messages", "/messages", "/anthropic/v1/messages"} {
		r := withApiKeyContext(withRequestTrace(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), requestEndpointClaude), &entry)
		w := httptest.NewRecorder()
		if !h.forwardCCTestProbe(w, r) || w.Code != 200 || w.Body.String() != response {
			t.Fatalf("SSE mismatch path=%s status=%d", path, w.Code)
		}
		if !w.Flushed || w.Header().Get("Request-Id") != "upstream-fixture-id" || w.Header().Get("X-Kiro-Route") != "cctest_upstream" {
			t.Fatal("missing SSE flush or route headers")
		}
		d := requestTraceFromContext(r.Context()).usageDiagnostic
		if d == nil || d.Route != "cctest_upstream" || d.ProbeKind != "tag_echo" || d.ProbeRule != cctestProbeRule {
			t.Fatal("probe diagnostics missing")
		}
		if d.ReportedInputTokens != 4 || d.ReportedOutputTokens != 17 || d.CacheCreationInputTokens != 10 || d.CacheReadInputTokens != 100 || d.CacheUsageEnabled {
			t.Fatalf("wrong upstream usage: %+v", d)
		}
		if strings.Contains(formatUsageDiagnostic(*d), secret) || strings.Contains(formatUsageDiagnostic(*d), entry.Key) {
			t.Fatal("diagnostic leaked credential")
		}
	}
}

func TestCCTestUpstreamErrorsAndRedirectsDoNotFallback(t *testing.T) {
	h := newAdminAuthTestHandler(t, "admin")
	var calls atomic.Int32
	status := http.StatusServiceUnavailable
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "https://elsewhere.invalid/must-not-follow")
		w.WriteHeader(status)
		io.WriteString(w, `{"error":"upstream fixture"}`)
	}))
	defer upstream.Close()
	h.upstreamClient = upstream.Client()
	url, secret := upstream.URL, "error-fixture"
	if err := config.UpdateCCTestProxyConfig(&url, &secret, false); err != nil {
		t.Fatal(err)
	}
	entry := addProbeFixtureKey(t, "shared-errors", true)
	for _, code := range []int{503, 429, 401, 307} {
		status = code
		r := withApiKeyContext(httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture))), &entry)
		w := httptest.NewRecorder()
		before := calls.Load()
		if !h.forwardCCTestProbe(w, r) || w.Code != code || w.Body.String() != `{"error":"upstream fixture"}` {
			t.Fatalf("error changed: %d %s", w.Code, w.Body.String())
		}
		if calls.Load() != before+1 {
			t.Fatal("request retried or redirect followed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := withApiKeyContext(httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(probeFixtureBody(t, tagProbeFixture))).WithContext(ctx), &entry)
	w := httptest.NewRecorder()
	before := calls.Load()
	if !h.forwardCCTestProbe(w, r) || w.Code != 502 || calls.Load() != before {
		t.Fatal("cancelled request not stopped")
	}
}
