package kiro

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// These expectations preserve the pinned reference's default-tokenizer fallback,
// JSON/stream differences, native usage precedence, and cache merge behavior.
func TestExactReferenceMeterGoldens(t *testing.T) {
	type event struct{ kind, body string }
	cases := []struct {
		name                 string
		events               []event
		cache                *Usage
		jsonWant, streamWant Usage
	}{
		{"plain_fallback", []event{{"assistantResponseEvent", `{"content":"hello world"}`}}, nil,
			Usage{InputTokens: 47, OutputTokens: 2, TotalTokens: 2}, Usage{InputTokens: 47, OutputTokens: 2, TotalTokens: 49}},
		{"reasoning_tags_only_in_json_count", []event{{"reasoningContentEvent", `{"text":"plan"}`}, {"assistantResponseEvent", `{"content":"answer"}`}}, nil,
			Usage{InputTokens: 47, OutputTokens: 8, TotalTokens: 8}, Usage{InputTokens: 47, OutputTokens: 2, TotalTokens: 49}},
		{"normalized_tool_arguments", []event{{"toolUseEvent", `{"toolUseEvent":{"toolUseId":"id1","name":"bash","input":"{\"command\": \"echo hello world\"}","stop":true}}`}}, nil,
			Usage{InputTokens: 47, OutputTokens: 7, TotalTokens: 7}, Usage{InputTokens: 47, OutputTokens: 7, TotalTokens: 54}},
		{"native_metadata_and_credits", []event{{"assistantResponseEvent", `{"content":"hi"}`}, {"messageMetadataEvent", `{"messageMetadataEvent":{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":42,"totalTokens":52,"cacheReadInputTokens":999}}}`}, {"meteringEvent", `{"usage":0.125}`}}, nil,
			Usage{InputTokens: 10, OutputTokens: 42, TotalTokens: 52, KiroCredits: 0.125}, Usage{InputTokens: 10, OutputTokens: 42, TotalTokens: 52, KiroCredits: 0.125}},
		{"direct_native_fields", []event{{"usageEvent", `{"inputTokens":11,"outputTokens":43,"totalTokens":54}`}}, nil,
			Usage{InputTokens: 11, OutputTokens: 43, TotalTokens: 54}, Usage{InputTokens: 11, OutputTokens: 43, TotalTokens: 54}},
		{"zero_native_output_uses_fallback", []event{{"assistantResponseEvent", `{"content":"hello world"}`}, {"messageMetadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":0}}`}}, nil,
			Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}, Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
		{"full_cache_json_zero_input_fallback", []event{{"assistantResponseEvent", `{"content":"hello world"}`}}, &Usage{CacheReadInputTokens: 47},
			Usage{InputTokens: 47, OutputTokens: 2, TotalTokens: 49, CacheReadInputTokens: 47}, Usage{OutputTokens: 2, TotalTokens: 49, CacheReadInputTokens: 47}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			for _, e := range tc.events {
				raw.Write(meterFrame(e.kind, e.body))
			}
			for _, stream := range []bool{false, true} {
				got, err := MeterEventStream(context.Background(), bytes.NewReader(raw.Bytes()), "claude-sonnet-4-5", stream, KiroRequestContext{EstimatedInputTokens: 47, ThinkingEnabled: true, CacheEmulationUsage: tc.cache})
				if err != nil {
					t.Fatal(err)
				}
				want := tc.jsonWant
				if stream {
					want = tc.streamWant
				}
				if got != want {
					t.Fatalf("stream=%v: got %+v; want %+v", stream, got, want)
				}
			}
		})
	}
}

func TestExactReferenceJSONPresentationCapDoesNotChangeMeteredUsage(t *testing.T) {
	raw := meterFrame("assistantResponseEvent", `{"content":"abcdefghijklmnopqrst"}`)
	ctx := KiroRequestContext{EstimatedInputTokens: 47, MaxOutputTokens: 2}
	result, err := ParseNonStreamingEventStreamWithContext(bytes.NewReader(raw), "claude-sonnet-4-5", ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Usage struct {
			Output int `json:"output_tokens"`
		} `json:"usage"`
		Stop    string `json:"stop_reason"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result.ResponseBody, &body); err != nil {
		t.Fatal(err)
	}
	if result.Usage.OutputTokens != 5 || result.StopReason != "end_turn" || body.Usage.Output != 2 || body.Stop != "max_tokens" || body.Content[0].Text != "abcdefghijk" {
		t.Fatalf("result=%+v body=%+v", result, body)
	}
	got, err := MeterEventStream(context.Background(), bytes.NewReader(raw), "claude-sonnet-4-5", true, ctx)
	if err != nil || got.OutputTokens != 5 {
		t.Fatalf("stream cap: %+v %v", got, err)
	}
}

func TestExactReferenceMeterPropagatesTruncatedFrame(t *testing.T) {
	for _, stream := range []bool{false, true} {
		_, err := MeterEventStream(context.Background(), bytes.NewReader([]byte{0, 0, 0}), "claude-sonnet-4-5", stream, KiroRequestContext{})
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("stream=%v: got %v", stream, err)
		}
	}
}

func meterFrame(kind, payload string) []byte {
	headers := append([]byte{11}, []byte(":event-type")...)
	headers = append(headers, 7, byte(len(kind)>>8), byte(len(kind)))
	headers = append(headers, []byte(kind)...)
	frame := make([]byte, 12+len(headers)+len(payload)+4)
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(frame)))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	return frame
}
