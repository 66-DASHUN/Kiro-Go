package proxy

// Each upstream signature belongs to the preceding native thinking block.
// Keep block boundaries: concatenating separately signed blocks invalidates
// their association. Signatures remain opaque and are never generated here.
type claudeThinkingCollector struct {
	blocks []ClaudeContentBlock
	closed bool
}

func (c *claudeThinkingCollector) text(text string) {
	if text == "" {
		return
	}
	if c.closed || len(c.blocks) == 0 || c.blocks[len(c.blocks)-1].Signature != "" {
		c.blocks = append(c.blocks, ClaudeContentBlock{Type: "thinking"})
	}
	c.closed = false
	c.blocks[len(c.blocks)-1].Thinking += text
}

// Ordinary output and tool calls end native thinking just as they do in the
// live SSE writer. Never merge later reasoning into an earlier unsigned block.
func (c *claudeThinkingCollector) endBlock() { c.closed = true }

func (c *claudeThinkingCollector) signature(signature string) {
	if signature == "" {
		return
	}
	if c.closed || len(c.blocks) == 0 {
		c.blocks = append(c.blocks, ClaudeContentBlock{Type: "thinking"})
	}
	c.closed = false
	c.blocks[len(c.blocks)-1].Signature = signature
}

func (c *claudeThinkingCollector) responseBlocks(omit bool) []ClaudeContentBlock {
	blocks := append([]ClaudeContentBlock(nil), c.blocks...)
	if omit {
		for i := range blocks {
			blocks[i].Thinking = ""
		}
	}
	return blocks
}

func replaceClaudeThinkingBlocks(content, thinking []ClaudeContentBlock) []ClaudeContentBlock {
	if len(thinking) == 0 {
		return content
	}
	result := append([]ClaudeContentBlock(nil), thinking...)
	for _, block := range content {
		if block.Type != "thinking" {
			result = append(result, block)
		}
	}
	return result
}

func claudeThinkingBlockMap(block ClaudeContentBlock) map[string]interface{} {
	result := map[string]interface{}{"type": "thinking", "thinking": block.Thinking}
	if block.Signature != "" {
		result["signature"] = block.Signature
	}
	return result
}
