package proxy

import (
	"encoding/json"
	"kiro-proxy/internal/anthropictokenizer"
	"math"
	"strings"
)

// Legacy cache boundary weights. Public request metering now uses the
// reference serialized-message accounting in sub2api_metering.go.
const claudeMessageFramingTokens = 2
const claudeGenerationPrefixTokens = 2

func estimateApproxTokens(text string) int {
	return anthropictokenizer.CountTokens(text)
}

func estimateCharacterTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	return referenceRequestInputTokens(req)
}

func estimateClaudeSystemTokens(system interface{}) int {
	switch value := system.(type) {
	case string:
		if isAnthropicBillingHeaderBlock(map[string]interface{}{"type": "text", "text": value}) {
			return 0
		}
		return estimateApproxTokens(value)
	case []string:
		total := 0
		for _, block := range value {
			if isAnthropicBillingHeaderBlock(map[string]interface{}{"type": "text", "text": block}) {
				continue
			}
			total += estimateApproxTokens(block)
		}
		return total
	case []interface{}:
		total := 0
		for _, block := range value {
			if isAnthropicBillingHeaderBlock(block) {
				continue
			}
			total += estimateClaudeValueTokens(block)
		}
		return total
	default:
		if isAnthropicBillingHeaderBlock(system) {
			return 0
		}
		return estimateClaudeValueTokens(system)
	}
}

func estimateClaudeToolTokens(tool ClaudeTool) int {
	return estimateApproxTokens(tool.Name) + estimateApproxTokens(tool.Description) + estimateJSONTokens(tool.InputSchema)
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	// Sub2API estimates a complete output buffer, not each transport fragment.
	// Tool names and transport/signature fields are not argument content.
	var output strings.Builder
	output.WriteString(thinkingContent)
	output.WriteString(content)
	for _, tu := range toolUses {
		if raw, err := json.Marshal(tu.Input); err == nil {
			output.Write(raw)
		}
	}
	return estimateApproxTokens(output.String())
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case []string:
		total := 0
		for _, part := range value {
			total += estimateApproxTokens(part)
		}
		return total
	case map[string]interface{}:
		if tokens, ok := estimateAttachmentTokens(value); ok {
			return tokens
		}
		typeName, _ := value["type"].(string)
		switch typeName {
		case "text":
			if text, ok := value["text"].(string); ok {
				return estimateApproxTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return estimateApproxTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokens(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += estimateApproxTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += estimateApproxTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	return referenceRequestInputTokens(openAIUsageRequest(req))
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	default:
		if blocks, ok := value.([]interface{}); ok {
			total := 0
			text := ""
			for _, block := range blocks {
				if object, ok := block.(map[string]interface{}); ok {
					if tokens, recognized := estimateAttachmentTokens(object); recognized {
						total += tokens
						continue
					}
				}
				text += extractOpenAIMessageText(block)
			}
			return total + estimateApproxTokens(text)
		}
		if block, ok := value.(map[string]interface{}); ok {
			if tokens, recognized := estimateAttachmentTokens(block); recognized {
				return tokens
			}
		}
		text := extractOpenAIMessageText(value)
		if text != "" {
			return estimateApproxTokens(text)
		}
		return estimateJSONTokens(value)
	}
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}

// Hosted-search follow-ups have no public request object. Count their semantic
// content, excluding transport identifiers/wrappers, with the same local engine.
func estimateKiroPayloadInputTokens(payload *KiroPayload) int {
	if payload == nil {
		return 0
	}
	total := 0
	for _, history := range payload.ConversationState.History {
		total += estimateKiroUserInputTokens(history.UserInputMessage, true)
		if msg := history.AssistantResponseMessage; msg != nil {
			total += estimateClaudeOutputTokens(msg.Content, "", msg.ToolUses)
		}
	}
	total += estimateKiroUserInputTokens(&payload.ConversationState.CurrentMessage.UserInputMessage, true)
	return total
}

func estimateKiroToolTokens(tool KiroToolWrapper) int {
	return estimateApproxTokens(tool.ToolSpecification.Name) + estimateApproxTokens(tool.ToolSpecification.Description) + estimateJSONTokens(tool.ToolSpecification.InputSchema.JSON)
}

func estimateKiroUserInputTokens(msg *KiroUserInputMessage, includeTools bool) int {
	if msg == nil {
		return 0
	}
	total := estimateApproxTokens(msg.Content)
	for i := range msg.Images {
		total += estimateImageInputTokens(&msg.Images[i])
	}
	for i := range msg.Documents {
		total += estimateDocumentInputTokens(&msg.Documents[i])
	}
	if msg.UserInputMessageContext == nil {
		return total
	}
	if includeTools {
		for _, tool := range msg.UserInputMessageContext.Tools {
			total += estimateKiroToolTokens(tool)
		}
	}
	for _, result := range msg.UserInputMessageContext.ToolResults {
		for _, content := range result.Content {
			total += estimateApproxTokens(content.Text)
		}
	}
	return total
}
