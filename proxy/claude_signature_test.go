package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestClaudePreservesUpstreamThinkingSignature(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"normal", "search", "structured"} {
			for _, display := range []string{"summarized", "omitted"} {
				t.Run(fmt.Sprintf("stream=%v/%s/%s", stream, mode, display), func(t *testing.T) {
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						answer := "answer"
						if mode == "structured" {
							answer = `{"answer":42}`
						}
						writeFrames(w,
							awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "native "}),
							awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
							awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "opaque-upstream-signature"}),
							awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": answer}), meteringFrame(t))
					})
					h.cachedModels = []ModelInfo{nativeOptionsModel()}
					extra := ""
					if mode == "search" {
						extra = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
					}
					if mode == "structured" {
						extra = `,` + structuredFixture
					}
					body := fmt.Sprintf(`{"model":"claude-opus-5.5","max_tokens":2048,"stream":%v,"thinking":{"type":"adaptive","display":%q}%s,"messages":[{"role":"user","content":"hello"}]}`, stream, display, extra)
					r := postClaudeMessages(t, h, body)
					if r.Code != 200 {
						t.Fatalf("request failed: %d %s", r.Code, r.Body.String())
					}
					wantText := "native thinking"
					if display == "omitted" {
						wantText = ""
					}
					if !stream {
						var response struct {
							Content []ClaudeContentBlock `json:"content"`
						}
						if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						if len(response.Content) == 0 || response.Content[0].Type != "thinking" || response.Content[0].Thinking != wantText || response.Content[0].Signature != "opaque-upstream-signature" {
							t.Fatalf("thinking signature lost or changed: %s", r.Body.String())
						}
						return
					}
					var text, signature string
					thinkingIndex := float64(-1)
					signatureBeforeStop := false
					for _, frame := range parseResponsesSSE(t, r.Body.String()) {
						switch frame.Name {
						case "content_block_start":
							block := frame.Data["content_block"].(map[string]interface{})
							if block["type"] == "thinking" {
								thinkingIndex = frame.Data["index"].(float64)
							}
						case "content_block_delta":
							delta := frame.Data["delta"].(map[string]interface{})
							if frame.Data["index"] != thinkingIndex {
								continue
							}
							if delta["type"] == "thinking_delta" {
								text += delta["thinking"].(string)
							}
							if delta["type"] == "signature_delta" {
								signature += delta["signature"].(string)
							}
						case "content_block_stop":
							if frame.Data["index"] == thinkingIndex && signature != "" {
								signatureBeforeStop = true
							}
						}
					}
					if text != wantText || signature != "opaque-upstream-signature" || !signatureBeforeStop {
						t.Fatalf("signature must precede thinking block stop, unchanged: %s", r.Body.String())
					}
				})
			}
		}
	}
}

func TestClaudeDoesNotInventThinkingSignature(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFrames(w, awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "unsigned reasoning"}), awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}), meteringFrame(t))
			})
			r := postClaudeMessages(t, h, fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":2048,"stream":%v,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hello"}]}`, stream))
			if r.Code != 200 || strings.Contains(r.Body.String(), `"signature"`) || strings.Contains(r.Body.String(), `signature_delta`) {
				t.Fatalf("signature was invented: %s", r.Body.String())
			}
		})
	}
}

func TestClaudeStreamMessageDeltaUsageMatchesReference(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeKiroTextResponse(t, w, "ok")
	})
	r := postClaudeMessages(t, h, `{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if r.Code != http.StatusOK {
		t.Fatalf("request failed: %d %s", r.Code, r.Body.String())
	}
	for _, frame := range parseResponsesSSE(t, r.Body.String()) {
		if frame.Name != "message_delta" {
			continue
		}
		usage, ok := frame.Data["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("message_delta usage missing: %#v", frame.Data)
		}
		if len(usage) != 2 || usage["output_tokens"] == nil {
			t.Fatalf("message_delta must contain reference input_tokens and output_tokens: %#v", usage)
		}
		return
	}
	t.Fatalf("message_delta event missing: %s", r.Body.String())
}

func TestClaudeKeepsSeparatelySignedThinkingBlocks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFrames(w,
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "first"}),
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "upstream-first"}),
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "second"}),
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "upstream-second"}),
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}), meteringFrame(t))
			})
			r := postClaudeMessages(t, h, fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":2048,"stream":%v,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hello"}]}`, stream))
			if r.Code != 200 {
				t.Fatalf("request: %s", r.Body.String())
			}
			if !stream {
				var response struct {
					Content []ClaudeContentBlock `json:"content"`
				}
				if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Content) != 3 || response.Content[0].Thinking != "first" || response.Content[0].Signature != "upstream-first" || response.Content[1].Thinking != "second" || response.Content[1].Signature != "upstream-second" {
					t.Fatalf("separately signed blocks were merged: %s", r.Body.String())
				}
				return
			}
			signatures := map[float64]string{}
			for _, frame := range parseResponsesSSE(t, r.Body.String()) {
				if frame.Name != "content_block_delta" {
					continue
				}
				delta := frame.Data["delta"].(map[string]interface{})
				if delta["type"] == "signature_delta" {
					signatures[frame.Data["index"].(float64)] = delta["signature"].(string)
				}
			}
			if len(signatures) != 2 || signatures[0] != "upstream-first" || signatures[1] != "upstream-second" {
				t.Fatalf("signature block association lost: %v", signatures)
			}
		})
	}
}

// An answer between native reasoning events ends the first reasoning block,
// even when that first block has no signature. Signing the merged text changes
// the text authenticated by the later upstream signature.
func TestClaudeSignatureDoesNotCoverEarlierUnsignedReasoning(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"normal", "search", "structured", "tool"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					answer := "answer"
					if mode == "structured" {
						answer = `{"answer":42}`
					}
					boundary := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": answer})
					if mode == "tool" {
						boundary = awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "read-id", "name": "read", "input": `{}`, "stop": true})
					}
					writeFrames(w,
						awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "earlier unsigned"}),
						boundary,
						awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "later signed"}),
						awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "opaque+/signature=="}),
						meteringFrame(t))
				})
				h.cachedModels = []ModelInfo{nativeOptionsModel()}
				extra := ""
				if mode == "search" {
					extra = `,"tools":[{"type":"web_search_20250305","name":"web_search"}]`
				} else if mode == "structured" {
					extra = `,` + structuredFixture
				} else if mode == "tool" {
					extra = `,"tools":[{"name":"read","input_schema":{"type":"object"}}]`
				}
				r := postClaudeMessages(t, h, fmt.Sprintf(`{"model":"claude-opus-5.5","max_tokens":2048,"stream":%v,"thinking":{"type":"adaptive"}%s,"messages":[{"role":"user","content":"hello"}]}`, stream, extra))
				if r.Code != 200 {
					t.Fatalf("request failed: %d %s", r.Code, r.Body.String())
				}
				var blocks []ClaudeContentBlock
				if !stream {
					var response ClaudeResponse
					if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					for _, block := range response.Content {
						if block.Type == "thinking" {
							blocks = append(blocks, block)
						}
					}
				} else {
					for _, frame := range parseResponsesSSE(t, r.Body.String()) {
						if frame.Name == "content_block_start" {
							block := frame.Data["content_block"].(map[string]interface{})
							if block["type"] == "thinking" {
								blocks = append(blocks, ClaudeContentBlock{Type: "thinking"})
							}
						} else if frame.Name == "content_block_delta" && len(blocks) > 0 {
							delta := frame.Data["delta"].(map[string]interface{})
							if delta["type"] == "thinking_delta" {
								blocks[len(blocks)-1].Thinking += delta["thinking"].(string)
							}
							if delta["type"] == "signature_delta" {
								blocks[len(blocks)-1].Signature += delta["signature"].(string)
							}
						}
					}
				}
				if len(blocks) != 2 || blocks[0].Thinking != "earlier unsigned" || blocks[0].Signature != "" || blocks[1].Thinking != "later signed" || blocks[1].Signature != "opaque+/signature==" {
					t.Fatalf("signature attached to different reasoning: %#v", blocks)
				}
			})
		}
	}
}
