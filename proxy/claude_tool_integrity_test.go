package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeToolResultStatusSurvivesTranslation(t *testing.T) {
	for _, mode := range []string{"active", "historical", "none"} {
		for _, status := range []struct {
			name string
			flag interface{}
			want string
		}{
			{"error", true, "error"},
			{"success", false, "success"},
			{"omitted", nil, "success"},
		} {
			t.Run(mode+"/"+status.name, func(t *testing.T) {
				result := map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_probe", "content": "RESULT-MARKER"}
				if status.flag != nil {
					result["is_error"] = status.flag
				}
				req := &ClaudeRequest{
					Model: "claude-opus-5.5",
					Tools: []ClaudeTool{{Name: "run_probe", InputSchema: map[string]interface{}{"type": "object"}}},
					Messages: []ClaudeMessage{
						{Role: "user", Content: "run it"},
						{Role: "assistant", Content: []interface{}{map[string]interface{}{"type": "tool_use", "id": "toolu_probe", "name": "run_probe", "input": map[string]interface{}{}}}},
						{Role: "user", Content: []interface{}{result}},
					},
				}
				if mode == "none" {
					req.ToolChoice = map[string]interface{}{"type": "none"}
				}
				if mode == "historical" {
					req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: "Acknowledged."}, ClaudeMessage{Role: "user", Content: "continue"})
				}
				payload := ClaudeToKiro(req, false)
				current := payload.ConversationState.CurrentMessage.UserInputMessage
				if mode == "active" {
					ctx := current.UserInputMessageContext
					if ctx == nil || len(ctx.ToolResults) != 1 {
						t.Fatalf("missing active tool result: %+v", ctx)
					}
					if got := ctx.ToolResults[0].Status; got != status.want {
						t.Fatalf("tool result status = %q, want %q", got, status.want)
					}
					body := ctx.ToolResults[0].Content[0].Text
					if !strings.Contains(body, "RESULT-MARKER") || strings.Contains(body, "[tool_error]") != (status.want == "error") {
						t.Fatalf("model-visible tool result does not preserve status %q: %q", status.want, body)
					}
					return
				}
				text := current.Content
				if mode == "historical" {
					for _, h := range payload.ConversationState.History {
						if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "RESULT-MARKER") {
							text = h.UserInputMessage.Content
						}
					}
				}
				if !strings.Contains(text, "RESULT-MARKER") {
					t.Fatalf("narration lost the tool result: %q", text)
				}
				if got := strings.Contains(text, "[tool_error]"); got != (status.want == "error") {
					t.Fatalf("narration error marker = %t, want status %q: %q", got, status.want, text)
				}
				if status.want == "error" && strings.Count(text, "[tool_error]") != 1 {
					t.Fatalf("error marker duplicated: %q", text)
				}
			})
		}
	}
}

func TestToolSchemaPreservesKeywordNamedParametersAndLiteralValues(t *testing.T) {
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(`{
	  "type":"object", "additionalProperties":false,
	  "required":["encrypted","additionalProperties","required"],
	  "properties":{
	    "encrypted":{"type":"string"},
	    "additionalProperties":{"type":"boolean"},
	    "required":{"type":"array","items":{"type":"string"}},
	    "nested":{"type":"object","additionalProperties":false,"properties":{
	      "encrypted":{"type":"string","encrypted":true}
	    }},
	    "config":{"type":"object","default":{"encrypted":true,"required":[],"additionalProperties":"literal"}}
	  }
	}`), &schema); err != nil {
		t.Fatal(err)
	}
	tools, _ := convertClaudeTools([]ClaudeTool{{Name: "probe", InputSchema: schema}})
	got := tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	props := got["properties"].(map[string]interface{})
	for _, name := range []string{"encrypted", "additionalProperties", "required"} {
		if _, ok := props[name]; !ok {
			t.Errorf("tool parameter %q was deleted", name)
		}
	}
	if _, ok := got["additionalProperties"]; ok {
		t.Error("Kiro-rejected schema keyword was retained")
	}
	nested := props["nested"].(map[string]interface{})
	if _, ok := nested["additionalProperties"]; ok {
		t.Error("nested Kiro-rejected schema keyword was retained")
	}
	child, ok := nested["properties"].(map[string]interface{})["encrypted"].(map[string]interface{})
	if !ok {
		t.Error("nested keyword-named parameter was deleted")
	} else if _, ok := child["encrypted"]; ok {
		t.Error("Kiro-rejected annotation was retained at the schema node")
	}
	literal := props["config"].(map[string]interface{})["default"].(map[string]interface{})
	if len(literal) != 3 || literal["encrypted"] != true || literal["additionalProperties"] != "literal" {
		t.Errorf("default literal was modified: %+v", literal)
	}
	if len(schema["properties"].(map[string]interface{})) != 5 {
		t.Error("caller schema mutated")
	}
}
