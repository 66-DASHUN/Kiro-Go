package proxy

import "strings"

const (
	effortUnset   = ""
	effortMinimal = "minimal"
	effortLow     = "low"
	effortMedium  = "medium"
	effortHigh    = "high"
	effortXHigh   = "xhigh"
	effortMax     = "max"
)

var effortOrder = map[string]int{
	effortLow:    1,
	effortMedium: 2,
	effortHigh:   3,
	effortXHigh:  4,
	effortMax:    5,
}

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none", "off", "disabled":
		return effortUnset
	case "minimal", "min":
		return effortMinimal
	case "low":
		return effortLow
	case "medium", "med":
		return effortMedium
	case "high":
		return effortHigh
	case "xhigh", "extra_high", "extra-high":
		return effortXHigh
	case "max", "maximum":
		return effortMax
	default:
		return effortUnset
	}
}

func resolveThinkingWithEffort(current bool, rawEffort string) bool {
	switch normalizeReasoningEffort(rawEffort) {
	case effortUnset:
		return current
	case effortMinimal:
		return false
	default:
		return true
	}
}

func (h *Handler) applyReasoningEffort(payload *KiroPayload, model, rawEffort string) {
	effort := normalizeReasoningEffort(rawEffort)
	if payload == nil || effort == effortUnset || effort == effortMinimal {
		return
	}
	if native, ok := resolveModelEffort(effort, h.effortLevelsForModel(model)); ok {
		if payload.AdditionalModelRequestFields == nil {
			payload.AdditionalModelRequestFields = map[string]interface{}{}
		}
		output, _ := payload.AdditionalModelRequestFields["output_config"].(map[string]interface{})
		if output == nil {
			output = map[string]interface{}{}
		}
		output["effort"] = native
		payload.AdditionalModelRequestFields["output_config"] = output
	}
}

func (h *Handler) effortLevelsForModel(model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return nil
	}
	h.modelsCacheMu.RLock()
	defer h.modelsCacheMu.RUnlock()
	for _, m := range h.cachedModels {
		if strings.EqualFold(strings.TrimSpace(m.ModelId), model) {
			return effortLevelsFromSchema(m.AdditionalModelRequestFieldsSchema)
		}
	}
	return nil
}

func effortLevelsFromSchema(schema map[string]interface{}) []string {
	if len(schema) == 0 {
		return nil
	}
	outputConfig, _ := nestedSchemaMap(schema, "properties", "output_config")
	effort, _ := nestedSchemaMap(outputConfig, "properties", "effort")
	rawEnum, ok := effort["enum"].([]interface{})
	if !ok {
		return nil
	}
	levels := make([]string, 0, len(rawEnum))
	for _, raw := range rawEnum {
		if s, ok := raw.(string); ok {
			//! Keep the schema's spelling: upstream validates against its own enum, so a
			//! model advertising "max" rejects the normalized "xhigh".
			if normalized := normalizeReasoningEffort(s); normalized != effortUnset && normalized != effortMinimal {
				levels = append(levels, strings.TrimSpace(s))
			}
		}
	}
	return levels
}

func nestedSchemaMap(root map[string]interface{}, keys ...string) (map[string]interface{}, bool) {
	current := root
	for _, key := range keys {
		if current == nil {
			return nil, false
		}
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func resolveModelEffort(rawEffort string, supportedLevels []string) (string, bool) {
	effort := normalizeReasoningEffort(rawEffort)
	if effort == effortUnset || effort == effortMinimal || len(supportedLevels) == 0 {
		return "", false
	}
	target := effortOrder[effort]
	// Older schemas spell their top tier "max" without an "xhigh" tier.
	// Preserve that alias only when xhigh itself is unavailable.
	if effort == effortXHigh {
		hasXHigh, hasMax := false, ""
		for _, level := range supportedLevels {
			if normalizeReasoningEffort(level) == effortXHigh {
				hasXHigh = true
			}
			if normalizeReasoningEffort(level) == effortMax {
				hasMax = level
			}
		}
		if !hasXHigh && hasMax != "" {
			return hasMax, true
		}
	}
	best := ""
	bestRank := -1
	for _, level := range supportedLevels {
		normalized := normalizeReasoningEffort(level)
		rank, ok := effortOrder[normalized]
		if !ok {
			continue
		}
		if rank <= target && rank > bestRank {
			best = level
			bestRank = rank
		}
	}
	if best != "" {
		return best, true
	}
	lowest := ""
	lowestRank := 1 << 30
	for _, level := range supportedLevels {
		normalized := normalizeReasoningEffort(level)
		rank, ok := effortOrder[normalized]
		if ok && rank < lowestRank {
			lowest = level
			lowestRank = rank
		}
	}
	return lowest, lowest != ""
}
