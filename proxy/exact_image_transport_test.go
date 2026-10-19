package proxy

import (
	"context"
	"fmt"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExactReferenceImageCountBlocksLoopback(t *testing.T) {
	server, hits := newRemoteImageServer(t, false)
	h := newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) { t.Fatal("count endpoint contacted Kiro") })
	key := exactHTTPKey(t, 1)
	for _, toolInput := range []bool{false, true} {
		image := map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": fmt.Sprintf("%s/img.png?tool=%t", server.URL, toolInput)}}
		var content interface{} = []interface{}{image}
		role := "user"
		if toolInput {
			role = "assistant"
			content = []interface{}{map[string]interface{}{"type": "tool_use", "id": "id1", "name": "lookup", "input": image}}
		}
		body := map[string]interface{}{"model": "claude-sonnet-4.5", "messages": []interface{}{map[string]interface{}{"role": role, "content": content}}}
		localUsageJSONRequest(t, h, "/v1/messages/count_tokens", body, &key)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("reference counting bypassed the image network guard: %d requests", got)
	}
}

func TestExactReferenceImageTransportPreservesDimensions(t *testing.T) {
	server, hits := newRemoteImageServer(t, true)
	want := refkiro.EstimateImageTokens(context.Background(), "image/png", testPNGBase64)
	got := refkiro.EstimateImageTokens(context.Background(), "", server.URL+"/img.png")
	if got != want || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("image tokens=%d want=%d requests=%d", got, want, atomic.LoadInt32(hits))
	}
}

func TestExactReferenceImageTransportRejectsNonPublicDial(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]", "169.254.169.254", "10.0.0.1", "100.100.100.200", "localhost"} {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+"/image.png", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := (exactImageTransport{}).RoundTrip(request)
		if response != nil {
			response.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "non-public address") {
			t.Errorf("host=%s expected rejection before dial, got %v", host, err)
		}
	}
}

func TestExactReferenceImageTransportRejectsRedirectCredentialsAndPrivateDestination(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.Write([]byte("not an image"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destination := strings.Replace(target.URL, "127.0.0.1", "127.0.0.2", 1)
		if r.URL.Path == "/credentials" {
			destination = strings.Replace(target.URL, "http://", "http://secret:password@", 1)
		}
		http.Redirect(w, r, destination, http.StatusFound)
	}))
	defer source.Close()
	old := remoteImageAddrAllowed
	remoteImageAddrAllowed = func(addr netip.Addr) bool { return addr == netip.MustParseAddr("127.0.0.1") }
	defer func() { remoteImageAddrAllowed = old }()
	for _, path := range []string{"/private", "/credentials"} {
		refkiro.EstimateImageTokens(context.Background(), "", source.URL+path)
	}
	refkiro.EstimateImageTokens(context.Background(), "", strings.Replace(target.URL, "http://", "http://secret:password@", 1)+"/direct")
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("credentials reached image destination: %d requests", got)
	}
}
