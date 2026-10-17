package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func nativeOptionsModel() ModelInfo {
	m := effortSchemaModel("claude-opus-5.5", "low", "medium", "high", "xhigh", "max")
	props := m.AdditionalModelRequestFieldsSchema["properties"].(map[string]interface{})
	props["thinking"] = map[string]interface{}{"properties": map[string]interface{}{
		"type":    map[string]interface{}{"enum": []interface{}{"adaptive", "disabled"}},
		"display": map[string]interface{}{"enum": []interface{}{"summarized", "omitted"}},
	}}
	props["max_tokens"] = map[string]interface{}{"type": "integer", "minimum": float64(1024), "maximum": float64(128000)}
	return m
}

func TestClaudeNativeThinkingOptionsReachWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"adaptive", "enabled", "disabled"} {
			t.Run(fmt.Sprintf("%s/stream=%v", kind, stream), func(t *testing.T) {
				calls := make(chan map[string]interface{}, 1)
				h := newStreamTestHandler(t, captureAdditionalFields(t, calls))
				h.cachedModels = []ModelInfo{nativeOptionsModel()}
				budget := ""
				if kind == "enabled" {
					budget = `,"budget_tokens":2048`
				}
				display := `,"display":"omitted"`
				if kind == "disabled" {
					display = ""
				}
				body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":8192,"stream":%v,"thinking":{"type":%q%s%s},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"solve this"}]}`, stream, kind, display, budget)
				if r := postClaudeMessages(t, h, body); r.Code != 200 {
					t.Fatalf("request: %s", r.Body.String())
				}
				fields := <-calls
				thought, _ := fields["thinking"].(map[string]interface{})
				want := kind
				if want == "enabled" {
					want = "adaptive"
				}
				if thought["type"] != want || (kind != "disabled" && thought["display"] != "omitted") {
					t.Fatalf("native thinking config lost: %#v", fields)
				}
				output, _ := fields["output_config"].(map[string]interface{})
				if output["effort"] != "max" || fields["max_tokens"] != float64(8192) {
					t.Fatalf("effort and output budget lost: %#v", fields)
				}
			})
		}
	}
}

func TestClaudeLowEnabledBudgetUsesPromptFallback(t *testing.T) {
	m := nativeOptionsModel()
	props := m.AdditionalModelRequestFieldsSchema["properties"].(map[string]interface{})
	props["thinking"] = map[string]interface{}{"properties": map[string]interface{}{
		"type":          map[string]interface{}{"enum": []interface{}{"adaptive", "enabled", "disabled"}},
		"budget_tokens": map[string]interface{}{"type": "integer", "minimum": float64(1024)},
	}}
	h := &Handler{cachedModels: []ModelInfo{m}}
	req := &ClaudeRequest{Model: "claude-opus-5-5", Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 99}}
	if got := h.nativeClaudeThinking(req, true); got != nil {
		t.Fatalf("small native thinking budget must use prompt fallback, got %#v", got)
	}
}

func TestClaudeNativeOptionsSearchRoundAndLegacyFallback(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			calls := make(chan map[string]interface{}, 1)
			h := newStreamTestHandler(t, captureAdditionalFields(t, calls))
			if native {
				h.cachedModels = []ModelInfo{nativeOptionsModel()}
			}
			r := postClaudeMessages(t, h, `{"model":"claude-opus-5.5","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"latest Go version"}]}`)
			if r.Code != 200 {
				t.Fatalf("request: %s", r.Body.String())
			}
			fields := <-calls
			if native {
				if _, ok := fields["thinking"]; !ok {
					t.Fatal("search round lost native thinking")
				}
				if _, ok := fields["max_tokens"]; ok {
					t.Fatal("small budget must not be raised to native minimum")
				}
			} else if len(fields) != 0 {
				t.Fatalf("unsupported fields sent: %#v", fields)
			}
		})
	}
}

func TestClaudeStreamEndIncludesStopSequence(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) { writeKiroTextResponse(t, w, "ok") })
	r := postClaudeMessages(t, h, `{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	for _, frame := range parseResponsesSSE(t, r.Body.String()) {
		event := frame.Data
		if event["type"] == "message_delta" {
			delta := event["delta"].(map[string]interface{})
			if value, ok := delta["stop_sequence"]; !ok || value != nil {
				t.Fatalf("stop_sequence must be explicitly null: %#v", delta)
			}
			return
		}
	}
	var body map[string]interface{}
	_ = json.Unmarshal(r.Body.Bytes(), &body)
	t.Fatalf("message_delta missing: %v", body)
}
