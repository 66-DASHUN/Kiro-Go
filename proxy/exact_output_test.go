package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	refkiro "kiro-proxy/internal/sub2apiref/kiro"
)

func TestExactReferenceOutputHookModesAndSingleCompletion(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		stream   bool
		want     int
	}{{"anthropic", false, 8}, {"anthropic", true, 2}, {"chat", false, 2}, {"chat_completions", false, 2}, {"responses", false, 2}} {
		t.Run(tc.protocol+map[bool]string{true: "_stream", false: "_json"}[tc.stream], func(t *testing.T) {
			raw := exactOutputFrames(t)
			meter := &exactRequestMeter{Protocol: tc.protocol, Stream: tc.stream, Model: "claude-sonnet-4-5", EstimatedInput: 47, Thinking: true}
			payload := &KiroPayload{exactMeter: meter}
			completed, gotIn, gotOut := 0, 0, 0
			var text strings.Builder
			emitted, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), payload, &KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }, OnComplete: func(i, o int) { completed++; gotIn = i; gotOut = o }})
			if err != nil || !emitted || completed != 1 || gotIn != 47 || gotOut != tc.want || text.String() != "plananswer" || meter.Result == nil {
				t.Fatalf("emitted=%v err=%v completed=%d usage=%d/%d text=%q result=%+v", emitted, err, completed, gotIn, gotOut, text.String(), meter.Result)
			}
		})
	}
}

func TestExactReferenceOutputHookFullCachePreservesZero(t *testing.T) {
	meter := &exactRequestMeter{Protocol: "anthropic", Stream: true, EstimatedInput: 47, CacheUsage: &refkiro.Usage{CacheReadInputTokens: 47}}
	input := -1
	_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(exactOutputFrames(t))), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{OnComplete: func(i, _ int) { input = i }})
	if err != nil || input != 0 {
		t.Fatalf("input=%d err=%v", input, err)
	}
}

func TestExactReferenceOutputHookConvertedUsageRetainsPositiveStart(t *testing.T) {
	for _, protocol := range []string{"chat_completions", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, input := range []int{0, -4} {
				raw := exactOutputFrames(t)
				raw = append(raw, awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": input, "outputTokens": 3}})...)
				meter := &exactRequestMeter{Protocol: protocol, Stream: stream, EstimatedInput: 47, Thinking: true}
				_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{})
				if err != nil || meter.ConvertedUsage == nil {
					t.Fatalf("err=%v converted=%+v", err, meter.ConvertedUsage)
				}
				if meter.Result.Usage.InputTokens != input || meter.ConvertedUsage.InputTokens != 47 || meter.ConvertedUsage.OutputTokens != 3 || meter.ConvertedUsage.TotalTokens != 50 {
					t.Fatalf("protocol=%s stream=%v raw=%+v converted=%+v", protocol, stream, meter.Result.Usage, meter.ConvertedUsage)
				}
			}
		}
	}
}

func TestExactReferenceOutputHookConvertedUsageCacheZeroAndNegative(t *testing.T) {
	for _, tc := range []struct {
		cache              *refkiro.Usage
		input, read, write int
	}{
		{&refkiro.Usage{CacheReadInputTokens: 47}, 47, 47, 0},
		{&refkiro.Usage{InputTokens: -7, CacheReadInputTokens: 47, CacheCreationInputTokens: -9}, 47, 47, 0},
		{&refkiro.Usage{InputTokens: 20, CacheReadInputTokens: -7, CacheCreationInputTokens: 27}, 47, 0, 27},
	} {
		meter := &exactRequestMeter{Protocol: "responses", EstimatedInput: 47, Thinking: true, CacheUsage: tc.cache}
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(exactOutputFrames(t))), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{})
		if err != nil || meter.ConvertedUsage == nil {
			t.Fatalf("err=%v converted=%+v", err, meter.ConvertedUsage)
		}
		u := meter.ConvertedUsage
		read := 0
		if u.InputTokensDetails != nil {
			read = u.InputTokensDetails.CachedTokens
		}
		if u.InputTokens != tc.input || read != tc.read || u.CacheCreationInputTokens != tc.write {
			t.Fatalf("cache=%+v converted=%+v read=%d", tc.cache, u, read)
		}
	}
}

func TestExactReferenceUsageWriterPreservesPositiveBuckets(t *testing.T) {
	var writer exactSSEUsageWriter
	for _, event := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":5}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":-3,\"cache_read_input_tokens\":-4,\"cache_creation_input_tokens\":0}}\n\n",
	} {
		if _, err := writer.Write([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	before := writer.usage
	// Contents are not parsed or retained; even invalid huge content JSON is
	// irrelevant to this usage-only observer.
	if _, err := writer.Write([]byte("event: content_block_delta\ndata: " + strings.Repeat("x", 2<<20))); err != nil {
		t.Fatal(err)
	}
	if writer.usage != before {
		t.Fatal("content event changed usage")
	}
	u := writer.convertedUsage()
	if u.InputTokens != 35 || u.OutputTokens != 7 || u.TotalTokens != 42 || u.CacheCreationInputTokens != 5 || u.InputTokensDetails.CachedTokens != 10 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestExactReferenceOutputHookStopsAcrossChunksAndPreservesThinking(t *testing.T) {
	for _, inline := range []bool{false, true} {
		var raw bytes.Buffer
		if inline {
			raw.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>STOP inside reasoning</thinking>\n\nalpha ST"}))
		} else {
			raw.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "STOP inside reasoning", "signature": "signed-reasoning"}))
			raw.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "alpha ST"}))
		}
		raw.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "OP discarded", "stopReason": "end_turn"}))
		meter := &exactRequestMeter{Stream: true, Thinking: true, StopSequences: []string{"STOP"}}
		var text, thinking, signature, stop string
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw.Bytes())), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{
			OnText: func(s string, reason bool) {
				if reason {
					thinking += s
				} else {
					text += s
				}
			},
			OnThinkingSignature: func(s string) { signature = s }, OnStopReason: func(s string) { stop = s },
		})
		if err != nil || text != "alpha " || thinking != "STOP inside reasoning" || stop != "stop_sequence" || (!inline && signature != "signed-reasoning") {
			t.Fatalf("inline=%v err=%v text=%q thinking=%q signature=%q stop=%q", inline, err, text, thinking, signature, stop)
		}
	}
}

func TestExactReferenceOutputHookStopPrefixAtEOFIsEmitted(t *testing.T) {
	raw := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "alpha ST", "stopReason": "end_turn"})
	var text string
	_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: &exactRequestMeter{Stream: true, StopSequences: []string{"STOP"}}}, &KiroStreamCallback{OnText: func(s string, _ bool) { text += s }})
	if err != nil || text != "alpha ST" {
		t.Fatalf("err=%v text=%q", err, text)
	}
}

func TestExactReferenceOutputHookStopDoesNotFilterToolArguments(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "alpha STOP ignored"}))
	raw.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t1", "name": "bash", "input": `{"command":"echo STOP"}`, "stop": true, "stopReason": "tool_use"}))
	meter := &exactRequestMeter{Stream: true, StopSequences: []string{"STOP"}}
	var text, command string
	_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw.Bytes())), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{OnText: func(s string, _ bool) { text += s }, OnToolUse: func(tool KiroToolUse) { command, _ = tool.Input["command"].(string) }})
	if err != nil || text != "alpha " || command != "echo STOP" {
		t.Fatalf("err=%v text=%q command=%q", err, text, command)
	}
}

func TestExactReferenceOutputHookHostedContextOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		meter := &exactRequestMeter{Stream: stream, HostedSearch: true, Thinking: true, StopSequences: []string{"answer"}, MaxOutput: 1, EstimatedInput: 47, CacheUsage: &refkiro.Usage{CacheReadInputTokens: 47}}
		var text string
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(exactOutputFrames(t))), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{OnText: func(s string, _ bool) { text += s }})
		if err != nil || meter.Result == nil || text != "plananswer" {
			t.Fatalf("stream=%v err=%v text=%q result=%+v", stream, err, text, meter.Result)
		}
		wantOutput := 8
		wantCached := 47
		if stream {
			wantOutput = 1
			wantCached = 0
		}
		if meter.Result.Usage.InputTokens != 47 || meter.Result.Usage.OutputTokens != wantOutput || meter.Result.Usage.CacheReadInputTokens != wantCached {
			t.Fatalf("stream=%v result=%+v", stream, meter.Result)
		}
	}
}

func TestExactReferenceOutputHookNativeUsageAndCredits(t *testing.T) {
	for _, stream := range []bool{false, true} {
		raw := exactOutputFrames(t)
		raw = append(raw, awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": 123, "outputTokens": 456, "totalTokens": 579}})...)
		raw = append(raw, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 0.125})...)
		meter := &exactRequestMeter{Stream: stream, Thinking: true, EstimatedInput: 47}
		var in, out, completed int
		var credits float64
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{
			OnComplete: func(i, o int) { in, out = i, o; completed++ },
			OnCredits:  func(c float64) { credits = c },
		})
		if err != nil || in != 123 || out != 456 || completed != 1 || credits != 0.125 || meter.Result.Usage.TotalTokens != 579 {
			t.Fatalf("stream=%v err=%v usage=%d/%d completed=%d credits=%g result=%+v", stream, err, in, out, completed, credits, meter.Result)
		}
	}
}

func TestExactReferenceOutputHookRetainsJSONPresentation(t *testing.T) {
	raw := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "abcdefghijklmnopqrst", "stopReason": "end_turn"})
	meter := &exactRequestMeter{MaxOutput: 2, EstimatedInput: 47}
	_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{})
	if err != nil || meter.Result == nil {
		t.Fatalf("err=%v result=%+v", err, meter.Result)
	}
	if meter.Result.Usage.OutputTokens != 5 || !bytes.Contains(meter.Result.ResponseBody, []byte(`"output_tokens":2`)) || !bytes.Contains(meter.Result.ResponseBody, []byte(`"text":"abcdefghijk"`)) {
		t.Fatalf("accounting/presentation mismatch: %+v body=%s", meter.Result, meter.Result.ResponseBody)
	}
}

func TestExactReferenceOutputHookErrorsDoNotComplete(t *testing.T) {
	for _, raw := range [][]byte{{0, 0, 0}, make([]byte, 12)} {
		meter := &exactRequestMeter{Stream: true}
		complete := false
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{OnComplete: func(int, int) { complete = true }})
		if err == nil || complete || meter.Result != nil {
			t.Fatalf("err=%v completed=%v result=%+v", err, complete, meter.Result)
		}
	}
}

func TestExactReferenceOutputHookReferenceRejectsOversizeFrameWithoutDeadlock(t *testing.T) {
	// The callback parser accepts up to 16 MiB, the reference accepts 10 MiB.
	raw := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("a", 11<<20)})
	meter := &exactRequestMeter{Stream: true}
	done := make(chan error, 1)
	go func() {
		_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(raw)), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid kiro eventstream frame length") {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reference rejection deadlocked tee")
	}
}

func TestExactReferenceOutputHookCancellationClosesBlockedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := parseExactEventStreamContext(ctx, reader, &KiroPayload{exactMeter: &exactRequestMeter{Stream: true}}, &KiroStreamCallback{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled parser did not exit")
	}
}

func TestExactReferenceOutputHookEarlyCallbackStopDrainsMeter(t *testing.T) {
	stopped := false
	completed := false
	meter := &exactRequestMeter{Stream: true, Thinking: true}
	_, err := parseExactEventStreamContext(context.Background(), io.NopCloser(bytes.NewReader(exactOutputFrames(t))), &KiroPayload{exactMeter: meter}, &KiroStreamCallback{outputStopped: func() bool { return stopped }, OnText: func(string, bool) { stopped = true }, OnComplete: func(_, o int) { completed = o == 2 }})
	if err != nil || !completed {
		t.Fatalf("err=%v completed=%v", err, completed)
	}
}

func TestExactReferenceOutputHookHTTPCommitBeforeReadAndBypassBudget(t *testing.T) {
	newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected server transport")
	})
	committed := false
	raw := exactOutputFrames(t)
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &exactReadObserver{Reader: bytes.NewReader(raw), beforeRead: func() {
			if !committed {
				t.Error("body read before cache commit")
			}
		}}}, nil
	})})
	t.Cleanup(func() { InitKiroHttpClient("") })
	meter := &exactRequestMeter{Stream: true, Thinking: true, EstimatedInput: 47, Commit: func() { committed = true }}
	payload := &KiroPayload{exactMeter: meter, InferenceConfig: &InferenceConfig{MaxTokens: 1}}
	var text strings.Builder
	err := CallKiroAPIContext(context.Background(), testAccount(t), payload, &KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }})
	if err != nil || !committed || text.String() != "plananswer" {
		t.Fatalf("err=%v committed=%v text=%q", err, committed, text.String())
	}
}

type exactReadObserver struct {
	io.Reader
	beforeRead func()
}

func (r *exactReadObserver) Read(p []byte) (int, error) { r.beforeRead(); return r.Reader.Read(p) }
func (*exactReadObserver) Close() error                 { return nil }

func exactOutputFrames(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	raw.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "plan"}))
	raw.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer", "stopReason": "end_turn"}))
	return raw.Bytes()
}
