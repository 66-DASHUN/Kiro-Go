package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeStructuredSchemaCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, schema, valid, invalid string }{
		{"lookahead", `{"type":"object","properties":{"code":{"type":"string","pattern":"^(?=.*[A-Z])(?=.*[0-9])[A-Za-z0-9]{4,12}$"}},"required":["code"],"additionalProperties":false}`, `{"code":"Ab12"}`, `{"code":"abcd"}`},
		{"backreference", `{"type":"object","properties":{"code":{"type":"string","pattern":"^([A-Z]{2})-\\1$"}},"required":["code"]}`, `{"code":"AB-AB"}`, `{"code":"AB-CD"}`},
		{"draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"n":{"type":"integer","minimum":1}},"required":["n"]}`, `{"n":2}`, `{"n":0}`},
		{"draft2020-localref", `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://example.invalid/result","type":"object","properties":{"n":{"$ref":"#/$defs/count"}},"$defs":{"count":{"type":"integer","minimum":1}},"required":["n"]}`, `{"n":2}`, `{"n":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(tc.schema)}}}
			if msg := prepareClaudeStructuredOutput(req); msg != "" {
				t.Fatal(msg)
			}
			if err := req.structuredOutput.validate(tc.valid, 0, "end_turn"); err != nil {
				t.Fatalf("valid response rejected: %v", err)
			}
			if err := req.structuredOutput.validate(tc.invalid, 0, "end_turn"); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestClaudeStructuredPatternTimeoutFailsClosedUnderNot(t *testing.T) {
	req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"string","not":{"pattern":"^(a+)+$"}}`)}}}
	if msg := prepareClaudeStructuredOutput(req); msg != "" {
		t.Fatal(msg)
	}
	text, _ := json.Marshal(strings.Repeat("a", 10000) + "!")
	if err := req.structuredOutput.validate(string(text), 0, "end_turn"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("pattern timeout beneath not must reject the whole response: %v", err)
	}
}

func TestClaudeLegacyStructuredFormatPreservesEffort(t *testing.T) {
	req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Effort: "high"}, OutputFormat: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`)}}
	if msg := prepareClaudeStructuredOutput(req); msg != "" {
		t.Fatal(msg)
	}
	if req.OutputConfig.Effort != "high" {
		t.Fatalf("effort lost: %+v", req.OutputConfig)
	}
}
