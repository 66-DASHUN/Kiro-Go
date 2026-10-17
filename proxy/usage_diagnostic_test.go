package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-proxy/logger"
)

func TestFormatUsageDiagnosticIncludesCountersWithoutPromptContent(t *testing.T) {
	diagnostic := usageDiagnostic{
		RequestID:                     "request-123",
		Endpoint:                      requestEndpointClaude,
		Model:                         "claude-opus-5-5",
		RequestInputEstimate:          24000,
		TranslatedInputEstimate:       15000,
		NativeInputTokens:             14945,
		NativeOutputTokens:            51,
		ReportedInputTokens:           14945,
		ReportedOutputTokens:          51,
		InputTokenSource:              "kiro_native",
		OutputTokenSource:             "kiro_native",
		CacheReadInputTokens:          0,
		CacheCreationInputTokens:      12759,
		CacheUsageEnabled:             true,
		EstimatedReturnedOutputTokens: 52,
		ReturnedOutputBytes:           231,
		RequestedMaxOutputTokens:      4096,
		ForwardedMaxOutputTokens:      4096,
		ObservedKiroStopReason:        "end_turn",
		ResponseStopReason:            "end_turn",
		CacheUsageSource:              "local_simulation",
		PromptFilterChangedSystem:     false,
		SystemPromptBytesBeforeFilter: 18000,
		SystemPromptBytesAfterFilter:  18000,
	}

	got := formatUsageDiagnostic(diagnostic)
	for _, want := range []string{
		`"request_id":"request-123"`,
		`"request_input_estimate":24000`,
		`"translated_input_estimate":15000`,
		`"native_input_tokens":14945`,
		`"native_output_tokens":51`,
		`"response_status":0`,
		`"cache_usage_source":"local_simulation"`,
		`"observed_kiro_stop_reason":"end_turn"`,
		`"response_stop_reason":"end_turn"`,
		`"local_output_budget_stopped":false`,
		`"system_prompt_bytes_before_filter":18000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic JSON missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "private prompt") || strings.Contains(got, "api-key-secret") {
		t.Fatalf("diagnostic must not contain prompt or credential data: %s", got)
	}
}

func TestUsageTokenSourceDistinguishesNativeUsageFromFallback(t *testing.T) {
	if got := usageTokenSource(18); got != "kiro_native" {
		t.Fatalf("positive upstream usage source = %q, want kiro_native", got)
	}
	if got := usageTokenSource(0); got != "local_estimate" {
		t.Fatalf("missing upstream usage source = %q, want local_estimate", got)
	}
}

func TestClaudeStreamDiagnosticSeparatesLocalOutputBudgetFromUpstreamStop(t *testing.T) {
	budget := newOutputBudget(4)
	var got streamDiagnostic
	var responseStopReason string
	callback := wrapOutputBudget(&KiroStreamCallback{
		onStreamDiagnostic: func(diagnostic streamDiagnostic) { got = diagnostic },
		OnStopReason:       func(reason string) { responseStopReason = reason },
	}, budget)
	body := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("answer ", 80)}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
		meteringFrame(t),
	}, nil))

	if err := parseEventStream(body, callback); err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	if got.ObservedKiroStopReason != "" || !got.OutputBudgetStopped || !got.OutputBudgetTruncated || got.OutputBudgetLimit != 4 || got.OutputBudgetUsed != 4 {
		t.Fatalf("stream diagnostic = %+v, want local truncation before Kiro stop metadata", got)
	}
	if responseStopReason != "max_tokens" {
		t.Fatalf("response stop reason = %q, want max_tokens", responseStopReason)
	}
}

func TestClaudeDirectWebSearchEmitsUsageDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	previousLevel := logger.GetLevel()
	logger.SetOutput(&logs)
	logger.SetLevel(logger.LevelInfo)
	t.Cleanup(func() {
		logger.ResetOutput()
		logger.SetLevel(previousLevel)
	})

	h := newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Error("direct web search must not call the model")
	})
	stubMCPWebSearch(t, http.StatusOK, mcpOneResult)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":1024,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: go release"}]}`))
	request.Header.Set("x-request-id", "cctest-websearch")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	decodeClaudeMessage(t, recorder)

	lineStart := strings.Index(logs.String(), "[UsageDiag] ")
	if lineStart < 0 {
		t.Fatalf("direct web search diagnostic missing: %s", logs.String())
	}
	line := strings.TrimSpace(strings.SplitN(logs.String()[lineStart+len("[UsageDiag] "):], "\n", 2)[0])
	var got usageDiagnostic
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode usage diagnostic: %v; line=%s", err, line)
	}
	if got.RequestID != "cctest-websearch" || got.Route != "websearch_direct" || got.RequestInputEstimate <= 0 || got.ReportedOutputTokens <= 0 || got.NativeInputTokens != 0 {
		t.Fatalf("unexpected direct web search diagnostic: %+v", got)
	}
}

func TestClaudeStreamEmitsUsageDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	previousLevel := logger.GetLevel()
	logger.SetOutput(&logs)
	logger.SetLevel(logger.LevelInfo)
	t.Cleanup(func() {
		logger.ResetOutput()
		logger.SetLevel(previousLevel)
	})

	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w,
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
			awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"inputTokens": 7, "outputTokens": 2, "stopReason": "end_turn"}),
			meteringFrame(t),
		)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-request-id", "cctest-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	lineStart := strings.Index(logs.String(), "[UsageDiag] ")
	if lineStart < 0 {
		t.Fatalf("usage diagnostic log missing: %s", logs.String())
	}
	line := logs.String()[lineStart+len("[UsageDiag] "):]
	line = strings.TrimSpace(strings.SplitN(line, "\n", 2)[0])
	var got usageDiagnostic
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode usage diagnostic: %v; line=%s", err, line)
	}
	if got.RequestID != "cctest-123" || got.NativeInputTokens != 7 || got.NativeOutputTokens != 2 || got.ReportedOutputTokens != 2 || got.ObservedKiroStopReason != "end_turn" || got.ForwardedMaxOutputTokens != 64 || got.LocalOutputBudgetStopped {
		t.Fatalf("unexpected usage diagnostic: %+v", got)
	}
}

func TestClaudeUpstreamFailureEmitsSanitizedUsageDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	previousLevel := logger.GetLevel()
	logger.SetOutput(&logs)
	logger.SetLevel(logger.LevelInfo)
	t.Cleanup(func() {
		logger.ResetOutput()
		logger.SetLevel(previousLevel)
	})

	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "credential-bearing upstream detail", http.StatusBadRequest)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-request-id", "cctest-failure")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("upstream failure status = %d, want an error", rec.Code)
	}

	lineStart := strings.Index(logs.String(), "[UsageDiag] ")
	if lineStart < 0 {
		t.Fatalf("failure diagnostic missing: %s", logs.String())
	}
	line := strings.TrimSpace(strings.SplitN(logs.String()[lineStart+len("[UsageDiag] "):], "\n", 2)[0])
	var got usageDiagnostic
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode failure diagnostic: %v; line=%s", err, line)
	}
	if got.RequestID != "cctest-failure" || got.ResponseStatus != rec.Code || got.FailureClass == "" {
		t.Fatalf("unexpected failure diagnostic: %+v; response=%d", got, rec.Code)
	}
	if strings.Contains(line, "credential-bearing upstream detail") {
		t.Fatalf("diagnostic leaked raw upstream error: %s", line)
	}
}
