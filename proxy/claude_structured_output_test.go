package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStructuredOutputConstraintSurvivesPromptFilters(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var payload KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(payload)
		if strings.Count(string(raw), "JSON Schema:") != 1 {
			t.Errorf("generated schema instruction was filtered or duplicated: %s", raw)
		}
		writeKiroTextResponse(t, w, `{"answer":42}`)
	})
	if err := config.UpdatePromptFilterConfig(false, false, false, []config.PromptFilterRule{{Enabled: true, Type: "regex", Match: "(?s).*", Replace: "filtered-system"}}); err != nil {
		t.Fatal(err)
	}
	r := postClaudeMessages(t, h, `{"model":"claude-sonnet-4.5","max_tokens":2048,"system":"original-system",`+structuredFixture+`,"messages":[{"role":"user","content":"answer"}]}`)
	if r.Code != 200 {
		t.Fatalf("request failed: %d %s", r.Code, r.Body.String())
	}
}

func TestStructuredOutputFailureAccountsOnceAndDoesNotCommitCache(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "not json"}), awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 100, "outputTokens": 3, "stopReason": "end_turn"}), awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": .25}))
	})
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "structured-key", Key: "sk-structured-local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	body := `{"model":"claude-sonnet-4.5","max_tokens":2048,` + structuredFixture + `,"messages":[{"role":"user","content":"answer"}]}`
	r := httptest.NewRecorder()
	h.handleClaudeMessages(r, withApiKeyContext(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), &key))
	if r.Code != 502 {
		t.Fatalf("bad schema answer accepted: %d %s", r.Code, r.Body.String())
	}
	used := config.GetApiKeyEntry(key.ID)
	if used.CreditsUsed != .25 || used.TokensUsed != 103 || used.RequestsCount != 1 {
		t.Fatalf("actual usage not finalized exactly once: %+v", used)
	}
	if len(h.promptCache.entriesByAccount["only"]) != 0 {
		t.Fatal("failed validation certified a cache entry")
	}
	page := getObserveStore().RequestPage(requestQuery{Page: 1, PageSize: 10, Status: "failed"})
	if page.Total != 1 || len(page.Requests) != 1 || page.Requests[0].Status != 502 || page.Requests[0].Credits != .25 {
		t.Fatalf("failed request lost actual usage: %+v", page)
	}
}

const structuredFixture = `"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}}}`

func TestClaudeStructuredOutputLegacyOutputFormatAlias(t *testing.T) {
	req := &ClaudeRequest{}
	body := []byte(`{"model":"claude-sonnet-4.5","max_tokens":128,"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}},"messages":[{"role":"user","content":"answer"}]}`)
	if err := json.Unmarshal(body, req); err != nil {
		t.Fatal(err)
	}
	if msg := prepareClaudeStructuredOutput(req); msg != "" {
		t.Fatalf("legacy output_format rejected: %s", msg)
	}
	if req.OutputConfig == nil || req.OutputConfig.Format == nil {
		t.Fatal("legacy output_format was not normalized")
	}
	if req.structuredOutput == nil || req.structuredOutput.schema == nil {
		t.Fatal("legacy output_format did not compile schema")
	}
}

func TestClaudeStructuredOutputJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, search := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/search=%v", stream, search), func(t *testing.T) {
				var calls atomic.Int32
				h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var payload KiroPayload
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					raw, _ := json.Marshal(payload)
					if !strings.Contains(string(raw), `JSON Schema`) || !strings.Contains(string(raw), `additionalProperties`) || !strings.Contains(string(raw), `original-system`) {
						t.Errorf("schema or system instruction lost: %s", raw)
					}
					writeKiroTextResponse(t, w, `{"answer":42}`)
				})
				h.cachedModels = []ModelInfo{nativeOptionsModel()}
				tools := ""
				if search {
					tools = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
				}
				body := fmt.Sprintf(`{"model":"claude-opus-5.5","max_tokens":2048,"stream":%v,"system":"original-system",%s%s,"messages":[{"role":"user","content":"Return the answer"}]}`, stream, structuredFixture, tools)
				r := postClaudeMessages(t, h, body)
				if r.Code != 200 || calls.Load() != 1 {
					t.Fatalf("request: calls=%d status=%d body=%s", calls.Load(), r.Code, r.Body.String())
				}
				var text string
				if stream {
					events := parseResponsesSSE(t, r.Body.String())
					if len(events) == 0 || events[0].Name != "message_start" || events[len(events)-1].Name != "message_stop" {
						t.Fatal("invalid SSE envelope")
					}
					for _, event := range events {
						if event.Name == "content_block_delta" {
							delta := event.Data["delta"].(map[string]interface{})
							if delta["type"] == "text_delta" {
								text += delta["text"].(string)
							}
						}
					}
				} else {
					var response struct {
						Content []ClaudeContentBlock `json:"content"`
					}
					if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					for _, block := range response.Content {
						if block.Type == "text" {
							text += block.Text
						}
					}
				}
				if text != `{"answer":42}` {
					t.Fatalf("structured output changed: %q", text)
				}
			})
		}
	}
}

func TestClaudeStructuredOutputNormalizesJSONCodeFence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				writeKiroTextResponse(t, w, "```json\n{\"answer\":42}\n```")
			})
			body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":128,"stream":%v,%s,"messages":[{"role":"user","content":"answer"}]}`, stream, structuredFixture)
			r := postClaudeMessages(t, h, body)
			if r.Code != http.StatusOK {
				t.Fatalf("code-fenced JSON rejected: %d %s", r.Code, r.Body.String())
			}
			if stream {
				var text string
				for _, event := range parseResponsesSSE(t, r.Body.String()) {
					if event.Name != "content_block_delta" {
						continue
					}
					delta := event.Data["delta"].(map[string]interface{})
					if delta["type"] == "text_delta" {
						text += delta["text"].(string)
					}
				}
				if text != `{"answer":42}` {
					t.Fatalf("stream response retained code fence: %q", text)
				}
				return
			}
			var response struct {
				Content []ClaudeContentBlock `json:"content"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Content) != 1 || response.Content[0].Text != `{"answer":42}` {
				t.Fatalf("buffered response retained code fence: %+v", response.Content)
			}
		})
	}
}

func TestClaudeStructuredOutputRejectsInvalidResponsesWithoutRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, answer := range []string{`not json`, `{"answer":"42"}`, `{"answer":42,"extra":true}`, `{}`, `{"answer":42} {}`} {
			t.Run(fmt.Sprintf("%v/%s", stream, answer), func(t *testing.T) {
				var calls atomic.Int32
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeKiroTextResponse(t, w, answer) })
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":2048,"stream":%v,%s,"messages":[{"role":"user","content":"answer"}]}`, stream, structuredFixture)
				r := postClaudeMessages(t, h, body)
				if r.Code != http.StatusBadGateway || calls.Load() != 1 || strings.Contains(r.Body.String(), "message_start") {
					t.Fatalf("invalid output must not look successful or be retried: calls=%d status=%d body=%s", calls.Load(), r.Code, r.Body.String())
				}
			})
		}
	}
}

func TestClaudeStructuredOutputRejectsInvalidRequestBeforeUpstream(t *testing.T) {
	for _, format := range []string{
		`{"type":"xml","schema":{}}`, `{"type":"json_schema"}`, `{"type":"json_schema","schema":{"type":"invalid"}}`,
		`{"type":"json_schema","schema":{"$ref":"http://127.0.0.1/private"}}`,
		`{"type":"json_schema","schema":{"$ref":"file:///etc/passwd"}}`,
	} {
		t.Run(format, func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("invalid schema reached upstream")
				writeKiroTextResponse(t, w, "ok")
			})
			r := postClaudeMessages(t, h, `{"model":"claude-sonnet-4.5","max_tokens":2048,"output_config":{"format":`+format+`},"messages":[{"role":"user","content":"answer"}]}`)
			if r.Code != 400 {
				t.Fatalf("invalid format not rejected: %d %s", r.Code, r.Body.String())
			}
		})
	}
}
