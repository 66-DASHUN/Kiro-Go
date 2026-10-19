package proxy

import (
	"encoding/json"
	"strings"
)

// Match countKiroInputTokensFromPayload in the pinned Sub2API reference:
// system text + canonical message JSON + 4/message + 150/tool. Go's map JSON
// encoder sorts keys and escapes strings the same way as its canonical writer.
func referenceRequestInputTokens(req *ClaudeRequest) int {
	total := 0
	switch s := req.System.(type) {
	case string:
		total += estimateApproxTokens(s)
	case []interface{}:
		for _, b := range s {
			total += referenceSystemTokens(b)
		}
	case []string:
		for _, b := range s {
			total += estimateApproxTokens(b)
		}
	default:
		total += referenceSystemTokens(s)
	}
	if len(req.Messages) > 0 {
		raw, _ := json.Marshal(req.Messages)
		var messages interface{}
		if json.Unmarshal(raw, &messages) == nil {
			sanitized, media := referenceSanitizeMedia(messages)
			total += estimateJSONTokens(sanitized) + media
		}
		total += 4 * len(req.Messages)
	}
	return max(1, total+150*len(req.Tools))
}

func referenceSystemTokens(v interface{}) int {
	if s, ok := v.(string); ok {
		return estimateApproxTokens(s)
	}
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["text"].(string); ok {
			return estimateApproxTokens(s)
		}
	}
	return 0
}

// Keep encoded binary data out of textual token counts. Image wrappers are
// retained, as in Sub2API; documents retain Kiro's bounded extraction support.
func referenceSanitizeMedia(v interface{}) (interface{}, int) {
	switch x := v.(type) {
	case []interface{}:
		out := make([]interface{}, len(x))
		total := 0
		for i, c := range x {
			sanitized, n := referenceSanitizeMedia(c)
			out[i] = sanitized
			total += n
		}
		return out, total
	case map[string]interface{}:
		kind, _ := x["type"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "image" || kind == "image_url" || kind == "input_image" {
			img := extractImageFromClaudeBlock(x)
			if kind != "image" {
				img = extractImageFromOpenAIPart(x)
			}
			return referenceMediaMarker(x, "[image]"), estimateImageInputTokens(img)
		}
		if kind == "document" {
			n, _ := estimateAttachmentTokens(x)
			return referenceMediaMarker(x, "[document]"), n
		}
		out := make(map[string]interface{}, len(x))
		total := 0
		for k, c := range x {
			sanitized, n := referenceSanitizeMedia(c)
			out[k] = sanitized
			total += n
		}
		return out, total
	default:
		return v, 0
	}
}

func referenceMediaMarker(v interface{}, marker string) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, c := range x {
			if _, ok := c.(string); ok && (k == "data" || k == "url" || k == "image_url") {
				out[k] = marker
			} else {
				out[k] = referenceMediaMarker(c, marker)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, c := range x {
			out[i] = referenceMediaMarker(c, marker)
		}
		return out
	default:
		return v
	}
}

// OpenAI-compatible requests are metered after normalization to Anthropic,
// just as the reference gateway does before entering its Kiro runtime.
func openAIUsageRequest(req *OpenAIRequest) *ClaudeRequest {
	out := &ClaudeRequest{}
	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, extractOpenAIMessageText(m.Content))
			continue
		}
		if m.Role == "tool" {
			out.Messages = append(out.Messages, ClaudeMessage{Role: "user", Content: []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": referenceToolID(m.ToolCallID), "content": m.Content}}})
			continue
		}
		var blocks []interface{}
		if text, ok := m.Content.(string); ok && text != "" {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
		} else if values, ok := m.Content.([]interface{}); ok {
			for _, v := range values {
				if block := referenceOpenAIBlock(v); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		for _, tc := range m.ToolCalls {
			var input interface{}
			if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
				input = map[string]interface{}{}
			}
			blocks = append(blocks, map[string]interface{}{"type": "tool_use", "id": referenceToolID(tc.ID), "name": tc.Function.Name, "input": input})
		}
		var content interface{} = blocks
		if text, ok := m.Content.(string); ok && len(m.ToolCalls) == 0 && m.Role != "assistant" {
			content = text
		}
		out.Messages = append(out.Messages, ClaudeMessage{Role: m.Role, Content: content})
	}
	out.System = strings.Join(system, "\n\n")
	out.Messages = mergeUsageMessages(out.Messages)
	out.Tools = make([]ClaudeTool, len(req.Tools))
	return out
}

func mergeUsageMessages(messages []ClaudeMessage) []ClaudeMessage {
	var out []ClaudeMessage
	blocks := func(v interface{}) []interface{} {
		if a, ok := v.([]interface{}); ok {
			return a
		}
		if s, ok := v.(string); ok {
			return []interface{}{map[string]interface{}{"type": "text", "text": s}}
		}
		return nil
	}
	for _, m := range messages {
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			last := &out[len(out)-1]
			last.Content = append(blocks(last.Content), blocks(m.Content)...)
		} else {
			out = append(out, m)
		}
	}
	return out
}

func appendHostedUsageRound(base *ClaudeRequest, tools []KiroToolUse, results []KiroToolResult) *ClaudeRequest {
	next := *base
	next.Messages = append([]ClaudeMessage(nil), base.Messages...)
	var assistant, user []interface{}
	for _, t := range tools {
		assistant = append(assistant, map[string]interface{}{"type": "tool_use", "id": referenceToolID(t.ToolUseID), "name": t.Name, "input": t.Input})
	}
	for _, r := range results {
		var content []interface{}
		for _, c := range r.Content {
			content = append(content, map[string]interface{}{"type": "text", "text": c.Text})
		}
		user = append(user, map[string]interface{}{"type": "tool_result", "tool_use_id": referenceToolID(r.ToolUseID), "content": content})
	}
	next.Messages = append(next.Messages, ClaudeMessage{Role: "assistant", Content: assistant}, ClaudeMessage{Role: "user", Content: user})
	return &next
}

func referenceToolID(id string) string {
	if after, ok := strings.CutPrefix(id, "fc_"); ok && (strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_")) {
		return after
	}
	if !strings.HasPrefix(id, "toolu_") && !strings.HasPrefix(id, "call_") {
		return "toolu_" + id
	}
	return id
}

func referenceOpenAIBlock(v interface{}) interface{} {
	b, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	kind, _ := b["type"].(string)
	switch kind {
	case "text", "input_text", "output_text":
		text, _ := b["text"].(string)
		if text == "" {
			return nil
		}
		return map[string]interface{}{"type": "text", "text": text}
	case "image_url", "input_image":
		raw, _ := b["image_url"].(string)
		if nested, ok := b["image_url"].(map[string]interface{}); ok {
			raw, _ = nested["url"].(string)
		}
		if strings.HasPrefix(raw, "data:") {
			media, data, ok := strings.Cut(strings.TrimPrefix(raw, "data:"), ";base64,")
			if ok {
				return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": media, "data": data}}
			}
		}
		// Keep existing URL image support (reference bridge drops remote URLs).
		return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": raw}}
	default:
		return v
	}
}
