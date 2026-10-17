package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ClaudeOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

// Kiro does not advertise native constrained decoding. This compatibility
// adapter sends the schema as instructions and validates the complete answer
// before emitting JSON or SSE. It never repairs values or retries paid calls.
type claudeStructuredOutput struct {
	schema      *jsonschema.Schema
	stream      bool
	instruction string
}

type noExternalSchemaLoader struct{}

func (noExternalSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("external schema references are not supported")
}

func prepareClaudeStructuredOutput(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	var format *ClaudeOutputFormat
	if req.OutputConfig != nil {
		format = req.OutputConfig.Format
	}
	if format == nil {
		format = req.OutputFormat
		if format != nil {
			// Normalize the legacy field so effort forwarding, cache identity and
			// all response paths observe the same structured-output request.
			req.OutputConfig = &ClaudeOutputConfig{Format: format}
		}
	}
	if format == nil {
		return ""
	}
	if format.Type != "json_schema" {
		return "output_config.format.type must be json_schema"
	}
	if len(format.Schema) == 0 || len(format.Schema) > 128*1024 {
		return "output_config.format.schema must contain a JSON Schema object of at most 128 KiB"
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(format.Schema)))
	if err != nil {
		return "output_config.format.schema is invalid"
	}
	if _, ok := doc.(map[string]interface{}); !ok {
		return "output_config.format.schema must be an object"
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noExternalSchemaLoader{})
	compiler.AssertFormat()
	const schemaURL = "https://kiro.invalid/structured-response.json"
	if err := compiler.AddResource(schemaURL, doc); err != nil {
		return "output_config.format.schema is invalid"
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return "output_config.format.schema is invalid or contains unsupported external references"
	}
	instruction := "The client requires structured output. When returning a final answer, return exactly one JSON value conforming to the following JSON Schema. Do not add Markdown fences or prose. Tool calls remain available when needed; the schema applies to the final answer text.\nJSON Schema:\n" + string(format.Schema)
	switch req.System.(type) {
	case nil, string, []interface{}, []string:
	default:
		return "system must be text or an array of content blocks"
	}
	req.structuredOutput = &claudeStructuredOutput{schema: schema, stream: req.Stream, instruction: instruction}
	return ""
}

func (format *claudeStructuredOutput) validate(text string, toolCount int, stopReason string) error {
	// Tools and deliberately incomplete answers can precede a final JSON answer.
	if format == nil || toolCount > 0 || stopReason == "max_tokens" || stopReason == "refusal" {
		return nil
	}
	text = normalizeClaudeStructuredJSONText(text)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err == nil {
		err = format.schema.Validate(doc)
	}
	if err != nil {
		return errors.New("upstream response did not conform to output_config.format.schema")
	}
	return nil
}

// Upstream models occasionally wrap an otherwise valid structured answer in a
// JSON Markdown fence. Anthropic structured output returns the JSON value
// itself, so remove only a complete json fence and leave all other text alone.
func normalizeClaudeStructuredJSONText(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return text
	}
	lineEnd := strings.IndexByte(trimmed, '\n')
	if lineEnd < 0 {
		return text
	}
	language := strings.TrimSpace(trimmed[3:lineEnd])
	if language != "" && !strings.EqualFold(language, "json") {
		return text
	}
	inner := strings.TrimSpace(trimmed[lineEnd+1 : len(trimmed)-3])
	if inner == "" {
		return text
	}
	return inner
}

func (h *Handler) writeClaudeBufferedResponse(w http.ResponseWriter, response *ClaudeResponse, stream bool) {
	if !stream {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(response)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	start := *response
	start.Content = []ClaudeContentBlock{}
	start.Usage.OutputTokens = 0
	start.StopReason = ""
	raw, _ := json.Marshal(start)
	var message map[string]interface{}
	json.Unmarshal(raw, &message)
	message["stop_reason"] = nil
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{"type": "message_start", "message": message})
	for index, block := range response.Content {
		raw, _ := json.Marshal(block)
		var content map[string]interface{}
		json.Unmarshal(raw, &content)
		h.sendWebSearchContentBlock(w, flusher, index, content)
	}
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": response.StopReason, "stop_sequence": response.StopSequence}, "usage": response.Usage,
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

func validateStructuredStreamingWriter(req *ClaudeRequest, w http.ResponseWriter) string {
	if req.structuredOutput != nil && req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			return "Streaming not supported"
		}
	}
	return ""
}
