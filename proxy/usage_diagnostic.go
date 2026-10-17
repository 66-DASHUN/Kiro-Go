package proxy

import (
	"context"
	"encoding/json"

	"kiro-proxy/logger"
)

type usageDiagnostic struct {
	RequestID                      string `json:"request_id,omitempty"`
	Endpoint                       string `json:"endpoint,omitempty"`
	Route                          string `json:"route,omitempty"`
	KiroRounds                     int    `json:"kiro_rounds"`
	Model                          string `json:"model,omitempty"`
	RequestInputEstimate           int    `json:"request_input_estimate"`
	TranslatedInputEstimate        int    `json:"translated_input_estimate"`
	NativeInputTokens              int    `json:"native_input_tokens"`
	NativeOutputTokens             int    `json:"native_output_tokens"`
	ReportedInputTokens            int    `json:"reported_input_tokens"`
	ReportedOutputTokens           int    `json:"reported_output_tokens"`
	ResponseStatus                 int    `json:"response_status"`
	FailureClass                   string `json:"failure_class,omitempty"`
	InputTokenSource               string `json:"input_token_source"`
	OutputTokenSource              string `json:"output_token_source"`
	CacheReadInputTokens           int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens       int    `json:"cache_creation_input_tokens"`
	CacheUsageSource               string `json:"cache_usage_source"`
	CacheUsageEnabled              bool   `json:"cache_usage_enabled"`
	EstimatedReturnedOutputTokens  int    `json:"estimated_returned_output_tokens"`
	ReturnedOutputBytes            int    `json:"returned_output_bytes"`
	RequestedMaxOutputTokens       int    `json:"requested_max_output_tokens"`
	ForwardedMaxOutputTokens       int    `json:"forwarded_max_output_tokens"`
	ForwardedNativeMaxOutputTokens int    `json:"forwarded_native_max_output_tokens"`
	ObservedKiroStopReason         string `json:"observed_kiro_stop_reason,omitempty"`
	ResponseStopReason             string `json:"response_stop_reason,omitempty"`
	LocalOutputBudgetLimit         int    `json:"local_output_budget_limit"`
	LocalOutputBudgetUsed          int    `json:"local_output_budget_used"`
	LocalOutputBudgetStopped       bool   `json:"local_output_budget_stopped"`
	LocalOutputBudgetTruncated     bool   `json:"local_output_budget_truncated"`
	SystemPromptBytesBeforeFilter  int    `json:"system_prompt_bytes_before_filter"`
	SystemPromptBytesAfterFilter   int    `json:"system_prompt_bytes_after_filter"`
	PromptFilterChangedSystem      bool   `json:"prompt_filter_changed_system"`
}

type streamDiagnostic struct {
	ObservedKiroStopReason string
	OutputBudgetLimit      int
	OutputBudgetUsed       int
	OutputBudgetStopped    bool
	OutputBudgetTruncated  bool
}

func setRequestUsageDiagnostic(ctx context.Context, diagnostic usageDiagnostic) {
	trace := requestTraceFromContext(ctx)
	if trace == nil {
		return
	}
	diagnostic.RequestID = trace.requestID
	diagnostic.Endpoint = trace.endpoint
	trace.usageDiagnostic = &diagnostic
}

func updateRequestUsageDiagnostic(ctx context.Context, update func(*usageDiagnostic)) {
	trace := requestTraceFromContext(ctx)
	if trace == nil || trace.usageDiagnostic == nil || update == nil {
		return
	}
	update(trace.usageDiagnostic)
}

func emitRequestUsageDiagnostic(ctx context.Context) {
	trace := requestTraceFromContext(ctx)
	if trace == nil || trace.usageDiagnostic == nil {
		return
	}
	logger.Infof("[UsageDiag] %s", formatUsageDiagnostic(*trace.usageDiagnostic))
}

func formatUsageDiagnostic(diagnostic usageDiagnostic) string {
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func usageTokenSource(nativeTokens int) string {
	if nativeTokens > 0 {
		return "kiro_native"
	}
	return "local_estimate"
}

func systemPromptFilterDiagnostics(system interface{}) (beforeBytes, afterBytes int, changed bool) {
	before := extractSystemPrompt(system)
	after := applyPromptFilters(before)
	return len(before), len(after), before != after
}

func claudeOutputByteLength(content, thinkingContent string, toolUses []KiroToolUse) int {
	length := len(content) + len(thinkingContent)
	for _, toolUse := range toolUses {
		length += len(toolUse.Name)
		if toolInput, err := json.Marshal(toolUse.Input); err == nil {
			length += len(toolInput)
		}
	}
	return length
}

func claudeBlocksByteLength(blocks []map[string]interface{}) int {
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return 0
	}
	return len(encoded)
}
