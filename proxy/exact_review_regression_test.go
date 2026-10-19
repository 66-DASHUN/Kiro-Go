package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	refcache "kiro-proxy/internal/sub2apiref/cache"
)

func TestExactReferenceStructuredStreamUsesPublicStreamingUsage(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w, exactHTTPCompleteFrames(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": `{"answer":42}`})))
	})
	key := exactHTTPKey(t, 1)
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(`{"model":"claude-sonnet-4.5","stream":true,"max_tokens":256,`+structuredFixture+`,"messages":[]}`), &body); err != nil {
		t.Fatal(err)
	}
	body["messages"] = []map[string]interface{}{{"role": "user", "content": strings.Repeat("a", 5000)}}
	rec := localUsageJSONRequest(t, h, "/v1/messages", body, &key)
	usage := runtimeProtocolUsage(t, rec, true)
	if usage["input_tokens"] != float64(0) || usage["cache_creation_input_tokens"] != float64(1261) || usage["output_tokens"] != float64(3) {
		t.Fatalf("buffered structured SSE must use stream meter: %v", usage)
	}
	for _, event := range parseResponsesSSE(t, rec.Body.String()) {
		if event.Name == "message_delta" {
			u := event.Data["usage"].(map[string]interface{})
			if _, ok := u["input_tokens"]; !ok {
				t.Fatal("final reference input usage missing")
			}
		}
	}
}

func TestExactReferenceResponsesContinuationCountsRestoredHistory(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w, exactHTTPCompleteFrames(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"})))
	})
	key := exactHTTPKey(t, 0)
	first := map[string]interface{}{"model": "claude-sonnet-4.5", "input": strings.Repeat("prior history ", 500), "store": true, "max_output_tokens": 128}
	rec := localUsageJSONRequest(t, h, "/v1/responses", first, &key)
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id := response["id"].(string)
	state, err := loadResponseState(id)
	if err != nil {
		t.Fatal(err)
	}
	next := map[string]interface{}{"model": "claude-sonnet-4.5", "input": "next", "previous_response_id": id, "store": false, "max_output_tokens": 128}
	raw, _ := json.Marshal(next)
	var req OpenAIResponsesRequest
	_ = json.Unmarshal(raw, &req)
	prepared, msg := prepareResponsesRequest(&req, state.Messages)
	if msg != "" {
		t.Fatal(msg)
	}
	actualBody, _ := json.Marshal(prepared.OpenAIRequest)
	want, err := refcache.InputTokensForModel(context.Background(), "chat_completions", actualBody, req.Model)
	if err != nil {
		t.Fatal(err)
	}
	usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/responses", next, &key), false)
	if want < 1000 || usage["input_tokens"] != float64(want) {
		t.Fatalf("restored history count got=%v want=%d", usage, want)
	}
}

func TestExactReferenceOriginalInputFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/sub2api-exact-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Requests []struct {
			Request json.RawMessage
			Tokens  int
		}
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for i, f := range fixtures.Requests {
		n, err := refcache.InputTokens(context.Background(), "anthropic", f.Request)
		if err != nil || n != f.Tokens {
			t.Fatalf("fixture %d input=%d want=%d error=%v", i, n, f.Tokens, err)
		}
	}
}
