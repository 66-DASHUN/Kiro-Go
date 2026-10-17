package proxy

import "strings"

// modelRequestSchema comes from Kiro's ListAvailableModels metadata. Never send
// guessed extension fields to models that do not advertise them.
func (h *Handler) modelRequestSchema(model string) map[string]interface{} {
	h.modelsCacheMu.RLock()
	defer h.modelsCacheMu.RUnlock()
	for _, m := range h.cachedModels {
		if totalCacheModelIdentity(m.ModelId) == totalCacheModelIdentity(model) {
			return m.AdditionalModelRequestFieldsSchema
		}
	}
	return nil
}

func enumHas(schema map[string]interface{}, value string) bool {
	values, _ := schema["enum"].([]interface{})
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (h *Handler) nativeClaudeThinking(req *ClaudeRequest, thinking bool) map[string]interface{} {
	if req == nil {
		return nil
	}
	schema, ok := nestedSchemaMap(h.modelRequestSchema(req.Model), "properties", "thinking")
	if !ok {
		return nil
	}
	typeSchema, _ := nestedSchemaMap(schema, "properties", "type")
	kind := ""
	if thinking {
		kind = "adaptive"
		if req.Thinking != nil && enumHas(typeSchema, strings.ToLower(strings.TrimSpace(req.Thinking.Type))) {
			kind = strings.ToLower(strings.TrimSpace(req.Thinking.Type))
		}
	} else if req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "disabled") {
		kind = "disabled"
	}
	if kind == "" || !enumHas(typeSchema, kind) {
		return nil
	}
	result := map[string]interface{}{"type": kind}
	if req.Thinking != nil && kind != "disabled" {
		displaySchema, _ := nestedSchemaMap(schema, "properties", "display")
		if display := strings.ToLower(strings.TrimSpace(req.Thinking.Display)); enumHas(displaySchema, display) {
			result["display"] = display
		}
		budgetSchema, ok := nestedSchemaMap(schema, "properties", "budget_tokens")
		if kind == "enabled" && ok {
			if !schemaAcceptsTokenBudget(budgetSchema, req.Thinking.BudgetTokens) {
				// Kiro's native thinking schema rejects small budgets. Returning nil
				// selects the existing prompt-based compatibility path instead of
				// sending a request that the upstream will reject with HTTP 400.
				return nil
			}
			result["budget_tokens"] = req.Thinking.BudgetTokens
		}
	}
	return result
}

func (h *Handler) claudeNeedsThinkingPrompt(req *ClaudeRequest, thinking bool) bool {
	return thinking && h.nativeClaudeThinking(req, thinking) == nil
}

func schemaAcceptsTokenBudget(schema map[string]interface{}, budget int) bool {
	if budget <= 0 {
		return false
	}
	if minimum, ok := schema["minimum"].(float64); ok && float64(budget) < minimum {
		return false
	}
	if maximum, ok := schema["maximum"].(float64); ok && float64(budget) > maximum {
		return false
	}
	return true
}

func (h *Handler) applyClaudeInferenceOptions(payload *KiroPayload, req *ClaudeRequest, thinking bool) {
	if payload == nil || req == nil {
		return
	}
	h.applyReasoningEffort(payload, req.Model, claudeOutputEffort(req))
	mergeField := func(key string, value interface{}) {
		if payload.AdditionalModelRequestFields == nil {
			payload.AdditionalModelRequestFields = map[string]interface{}{}
		}
		payload.AdditionalModelRequestFields[key] = value
	}
	if native := h.nativeClaudeThinking(req, thinking); native != nil {
		mergeField("thinking", native)
	}
	if schema, ok := nestedSchemaMap(h.modelRequestSchema(req.Model), "properties", "max_tokens"); ok && schemaAcceptsTokenBudget(schema, req.MaxTokens) {
		mergeField("max_tokens", req.MaxTokens)
	}
}
