package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/logger"
	"net/http"
	"strings"
	"testing"
)

func TestClaudeHostedSearchReasoningDiagnosticRespectsDisplay(t *testing.T) {
	for _, display := range []string{"omitted", "summarized"} {
		t.Run(display, func(t *testing.T) {
			var logs bytes.Buffer
			level := logger.GetLevel()
			logger.SetOutput(&logs)
			logger.SetLevel(logger.LevelInfo)
			t.Cleanup(func() { logger.ResetOutput(); logger.SetLevel(level) })
			const reasoning = "Observed native reasoning before the answer."
			h := newLegacyMeteringTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFrames(w, awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": reasoning}), awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "sig"}), awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}), meteringFrame(t))
			})
			body := map[string]interface{}{"model": "claude-opus-5.5", "max_tokens": 1024, "thinking": map[string]interface{}{"type": "adaptive", "display": display}, "messages": []map[string]interface{}{{"role": "user", "content": "Reply hello."}}, "tools": []ClaudeTool{nativeWebSearchTool()}}
			localUsageJSONRequest(t, h, "/v1/messages", body, nil)
			line := strings.SplitN(strings.SplitN(logs.String(), "[UsageDiag] ", 2)[1], "\n", 2)[0]
			var d usageDiagnostic
			if err := json.Unmarshal([]byte(line), &d); err != nil {
				t.Fatal(err)
			}
			if d.Route != "websearch_loop" {
				t.Fatalf("wrong route: %s", d.Route)
			}
			wantHidden, wantShown := 0, estimateClaudeOutputTokens("hello", reasoning, nil)
			if display == "omitted" {
				wantHidden = estimateApproxTokens(reasoning)
				wantShown = estimateApproxTokens("hello")
			}
			if d.SuppressedReasoningTokens != wantHidden || d.EstimatedReturnedOutputTokens != wantShown {
				t.Fatalf("display=%s hidden=%d/%d returned=%d/%d", display, d.SuppressedReasoningTokens, wantHidden, d.EstimatedReturnedOutputTokens, wantShown)
			}
		})
	}
}

func TestClaudeUnrequestedNativeReasoningIsCountedButNotDisplayed(t *testing.T) {
	const reasoning = "Native reasoning observed before the tool call."
	const signature = "signature-must-not-be-billed-or-displayed"
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"omitted", "disabled", "adaptive-omitted"} {
			for _, nativeUsage := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/mode=%s/native=%t", stream, mode, nativeUsage), func(t *testing.T) {
					h := newLegacyMeteringTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
						frames := [][]byte{
							awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": reasoning}),
							awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": signature}),
							awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
						}
						if nativeUsage {
							frames = append(frames, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"outputTokens": 123}))
						}
						writeFrames(w, append(frames, meteringFrame(t))...)
					})
					body := map[string]interface{}{"model": "claude-opus-5.5", "stream": stream, "max_tokens": 1024, "messages": []map[string]interface{}{{"role": "user", "content": "Reply hello."}}}
					if mode == "disabled" {
						body["thinking"] = map[string]interface{}{"type": "disabled"}
					} else if mode == "adaptive-omitted" {
						body["thinking"] = map[string]interface{}{"type": "adaptive", "display": "omitted"}
					}
					rec := localUsageJSONRequest(t, h, "/v1/messages", body, nil)
					usage := runtimeProtocolUsage(t, rec, stream)
					_, output, _, _ := localUsageTotals("/v1/messages", usage)
					want := estimateClaudeOutputTokens("hello", reasoning, nil)
					if nativeUsage {
						want = 123
					}
					if output != want {
						t.Errorf("output=%d want=%d; actual native reasoning must survive display suppression", output, want)
					}
					if strings.Contains(rec.Body.String(), reasoning) {
						t.Fatal("suppressed reasoning was displayed")
					}
					if mode != "adaptive-omitted" && strings.Contains(rec.Body.String(), signature) {
						t.Fatal("unrequested signature was displayed")
					}
				})
			}
		}
	}
}

func TestClaudeHostedRoundCountsUnrequestedNativeReasoning(t *testing.T) {
	const reasoning = "Observed native reasoning before a search."
	h := newLegacyMeteringTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFrames(w,
			awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": reasoning}),
			awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "signature"}),
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
			meteringFrame(t),
		)
	})
	round, err := h.callClaudeRound(context.Background(), testAccount(t), testKiroPayload(), "claude-opus-5.5", false, 20)
	if err != nil {
		t.Fatal(err)
	}
	if want := estimateClaudeOutputTokens("hello", reasoning, nil); round.outputTokens != want {
		t.Fatalf("round output=%d want=%d", round.outputTokens, want)
	}
	if round.thinking != "" {
		t.Fatal("suppressed reasoning became visible")
	}
}

func TestClaudeOutputDiagnosticSeparatesObservedAndDisplayedReasoning(t *testing.T) {
	for _, show := range []bool{false, true} {
		d := usageDiagnostic{}
		d.setClaudeOutputEstimates("hello", "Observed native reasoning.", show, nil)
		if d.EstimatedGeneratedOutputTokens != estimateClaudeOutputTokens("hello", "Observed native reasoning.", nil) || d.ObservedReasoningTokens != estimateApproxTokens("Observed native reasoning.") {
			t.Fatalf("generated counters lost: %+v", d)
		}
		if show {
			if d.SuppressedReasoningTokens != 0 || d.EstimatedReturnedOutputTokens != d.EstimatedGeneratedOutputTokens {
				t.Fatalf("visible counters: %+v", d)
			}
		} else if d.EstimatedReturnedOutputTokens != 1 || d.ReturnedOutputBytes != 5 || d.SuppressedReasoningTokens != d.ObservedReasoningTokens {
			t.Fatalf("hidden counters: %+v", d)
		}
	}
}
