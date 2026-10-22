package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestClaudeStopDetailsAcrossResponsePaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"normal", "search", "structured"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					answer := "answer"
					if mode == "structured" {
						answer = `{"answer":42}`
					}
					writeKiroTextResponse(t, w, answer)
				})
				h.cachedModels = []ModelInfo{nativeOptionsModel()}
				extra := ""
				if mode == "search" {
					extra = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
				} else if mode == "structured" {
					extra = `,` + structuredFixture
				}
				r := postClaudeMessages(t, h, fmt.Sprintf(`{"model":"claude-opus-5.5","max_tokens":2048,"stream":%v%s,"messages":[{"role":"user","content":"hello"}]}`, stream, extra))
				if r.Code != 200 {
					t.Fatalf("request: %d %s", r.Code, r.Body.String())
				}
				check := func(label string, message map[string]interface{}) {
					t.Helper()
					value, present := message["stop_details"]
					if !present || value != nil {
						t.Errorf("%s stop_details must be explicitly null: %#v", label, message)
					}
				}
				if !stream {
					var response map[string]interface{}
					if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					check("message", response)
					return
				}
				start, delta := false, false
				for _, frame := range parseResponsesSSE(t, r.Body.String()) {
					if frame.Name == "message_start" {
						start = true
						check("message_start", frame.Data["message"].(map[string]interface{}))
					}
					if frame.Name == "message_delta" {
						delta = true
						check("message_delta", frame.Data["delta"].(map[string]interface{}))
					}
				}
				if !start || !delta {
					t.Fatal("message_start/message_delta missing")
				}
			})
		}
	}
}
