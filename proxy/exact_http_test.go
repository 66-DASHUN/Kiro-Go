package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	refcompat "kiro-proxy/internal/sub2apiref/apicompat"
	refcache "kiro-proxy/internal/sub2apiref/cache"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
	accountpool "kiro-proxy/pool"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func exactHTTPKey(t *testing.T, ratio float64) config.ApiKeyEntry {
	t.Helper()
	enabled, mode := true, "sub2api"
	if err := config.UpdateSettingsPatch(&enabled, ""); err != nil {
		t.Fatal(err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "exact-reference", Key: "sk-exact-reference-fixture", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	accountpool.GetPool().Reload()
	return key
}

func exactHTTPProtocol(path string) string {
	switch path {
	case "/v1/messages":
		return "anthropic"
	case "/v1/chat/completions":
		return "chat_completions"
	default:
		return "responses"
	}
}

func exactHTTPCompleteFrames(t *testing.T, frames []byte) []byte {
	t.Helper()
	return append(frames, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"})...)
}

func exactHTTPRequest(path string, stream bool, messages []map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{"model": "claude-sonnet-4.5", "stream": stream, "max_tokens": 256}
	if path == "/v1/responses" {
		body["input"], body["store"] = messages, false
	} else {
		body["messages"] = messages
	}
	return body
}

func exactJSONMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// This oracle owns an independent original-reference state tracker. It never
// calls the proxy's estimators, cache adapters, usage finalizers or converters.
func exactHTTPOracle(t *testing.T, tracker *refcache.Tracker, path string, body map[string]interface{}, frames []byte, ratio float64) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	protocol, model := exactHTTPProtocol(path), body["model"].(string)
	input, err := refcache.InputTokensForModel(context.Background(), protocol, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := refcache.BuildProfile(context.Background(), protocol, raw, model, input)
	plan := tracker.Prepare("independent-account", profile, refcache.Policy{Enabled: true, Mode: "uniform", Ratio: ratio})
	requestCtx := refkiro.KiroRequestContext{EstimatedInputTokens: input, MaxOutputTokens: 256}
	if u := plan.Usage(); u != nil {
		requestCtx.CacheEmulationUsage = &refkiro.Usage{InputTokens: u.InputTokens, CacheReadInputTokens: u.CacheReadInputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheCreation5mInputTokens: u.CacheCreation5mInputTokens, CacheCreation1hInputTokens: u.CacheCreation1hInputTokens}
	}
	plan.Commit()
	rec := httptest.NewRecorder()
	// gateway_forward_as_responses.go always requests Anthropic streaming,
	// including when the OpenAI client requests a buffered JSON response.
	stream := body["stream"].(bool) || path != "/v1/messages"
	if stream {
		_, err = refkiro.StreamEventStreamAsAnthropicWithContext(context.Background(), bytes.NewReader(frames), rec, model, input, requestCtx)
	} else {
		var result *refkiro.ParseResult
		result, err = refkiro.ParseNonStreamingEventStreamWithContext(bytes.NewReader(frames), model, requestCtx)
		if err == nil {
			_, _ = rec.Write(result.ResponseBody)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	usage := runtimeProtocolUsage(t, rec, stream)
	if path == "/v1/messages" {
		return usage
	}
	// Replay the actual SSE through the original stateful bridge: it keeps
	// positive values from message_start when message_delta contains zero.
	state := refcompat.NewAnthropicEventToResponsesState()
	var response *refcompat.ResponsesResponse
	collect := func(events []refcompat.ResponsesStreamEvent) {
		for _, event := range events {
			if event.Response != nil && event.Response.Usage != nil {
				response = event.Response
			}
		}
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event refcompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		collect(refcompat.AnthropicEventToResponsesEvents(&event, state))
	}
	collect(refcompat.FinalizeAnthropicResponsesStream(state))
	if response == nil {
		t.Fatal("reference bridge omitted terminal response usage")
	}
	if path == "/v1/responses" {
		return exactJSONMap(t, response.Usage)
	}
	return exactJSONMap(t, refcompat.ResponsesToChatCompletions(response, model).Usage)
}

func testExactReferenceHTTPMatrix(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, ratio := range []float64{0, .9, 1} {
				for _, native := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v/ratio=%v/native=%v", path, stream, ratio, native), func(t *testing.T) {
						frames := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "antidisestablishmentarianism"})
						if native {
							frames = append(frames, awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": 19763, "outputTokens": 137}})...)
						}
						frames = exactHTTPCompleteFrames(t, frames)
						h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeFrames(w, frames) })
						key := exactHTTPKey(t, ratio)
						oracle := refcache.NewTracker()
						messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("stable reference prefix ", 1000)}}
						for _, phase := range []string{"cold", "warm", "append", "append-warm", "short"} {
							if phase == "append" {
								messages = append(messages, map[string]interface{}{"role": "assistant", "content": "previous answer"}, map[string]interface{}{"role": "user", "content": strings.Repeat("new suffix ", 300)})
							}
							if phase == "short" {
								messages = []map[string]interface{}{{"role": "user", "content": "hello world"}}
							}
							body := exactHTTPRequest(path, stream, messages)
							want := exactHTTPOracle(t, oracle, path, body, frames, ratio)
							rec := localUsageJSONRequest(t, h, path, body, &key)
							got := runtimeProtocolUsage(t, rec, stream)
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("%s public usage differs from pinned 1cd5141: got=%s want=%s", phase, mustExactJSON(t, got), mustExactJSON(t, want))
							}
						}
					})
				}
			}
		}
	}
}

func mustExactJSON(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Fixed goldens prevent a mutually changed oracle and adapter from silently
// replacing the reference's broken-tokenizer fallback or JSON full-cache quirk.
func TestExactReferenceHTTPGoldenShortAndNative(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, native := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/native=%v", path, stream, native), func(t *testing.T) {
					frames := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello world"})
					if native {
						frames = append(frames, awsEventStreamFrame(t, "usageEvent", map[string]interface{}{"inputTokens": 11, "outputTokens": 43, "totalTokens": 54})...)
					}
					frames = exactHTTPCompleteFrames(t, frames)
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeFrames(w, frames) })
					key := exactHTTPKey(t, 1)
					body := exactHTTPRequest(path, stream, []map[string]interface{}{{"role": "user", "content": "hello world"}})
					got := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
					input, output := 14, 2
					if native {
						input, output = 11, 43
					}
					var want map[string]interface{}
					switch path {
					case "/v1/messages":
						want = map[string]interface{}{"input_tokens": input, "output_tokens": output}
					case "/v1/chat/completions":
						want = map[string]interface{}{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
					default:
						want = map[string]interface{}{"input_tokens": input, "output_tokens": output, "total_tokens": input + output}
					}
					if !reflect.DeepEqual(got, exactJSONMap(t, want)) {
						t.Fatalf("fixed reference golden got=%s want=%s", mustExactJSON(t, got), mustExactJSON(t, want))
					}
				})
			}
		}
	}
}

func TestExactReferenceHTTPCountTokensGolden(t *testing.T) {
	h := newStreamTestHandler(t, func(http.ResponseWriter, *http.Request) { t.Error("count endpoint must not call upstream") })
	key := exactHTTPKey(t, .9)
	for _, tc := range []struct {
		content interface{}
		want    int
	}{
		{"hello world", 14},
		{[]map[string]interface{}{{"type": "text", "text": "hello world"}}, 20},
	} {
		body := map[string]interface{}{"model": "claude-sonnet-4.5", "messages": []map[string]interface{}{{"role": "user", "content": tc.content}}}
		rec := localUsageJSONRequest(t, h, "/v1/messages/count_tokens", body, &key)
		var got map[string]int
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["input_tokens"] != tc.want {
			t.Fatalf("count_tokens got=%v want=%d", got, tc.want)
		}
	}
}

func TestExactReferenceHTTPLateNonpositiveNativeInput(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, nativeInput := range []int{0, -4} {
				t.Run(fmt.Sprintf("%s/stream=%v/native=%d", path, stream, nativeInput), func(t *testing.T) {
					frames := append(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello world"}), awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": nativeInput, "outputTokens": 43}})...)
					frames = exactHTTPCompleteFrames(t, frames)
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeFrames(w, frames) })
					key := exactHTTPKey(t, 1)
					body := exactHTTPRequest(path, stream, []map[string]interface{}{{"role": "user", "content": "hello world"}})
					want := exactHTTPOracle(t, refcache.NewTracker(), path, body, frames, 1)
					got := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("late native input differs: got=%s want=%s", mustExactJSON(t, got), mustExactJSON(t, want))
					}
				})
			}
		}
	}
}

func TestExactReferenceHTTPOutputFallbackVariants(t *testing.T) {
	fixtures := []struct {
		name   string
		frames func(*testing.T) []byte
	}{
		{"native_zero_output", func(t *testing.T) []byte {
			return append(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "antidisestablishmentarianism"}), awsEventStreamFrame(t, "messageMetadataEvent", map[string]interface{}{"tokenUsage": map[string]interface{}{"uncachedInputTokens": 10, "outputTokens": 0}})...)
		}},
		{"reasoning_and_text", func(t *testing.T) []byte {
			return append(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "plan"}), awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"})...)
		}},
		{"normalized_tool_arguments", func(t *testing.T) []byte {
			return awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "id1", "name": "bash", "input": `{"command": "echo hello world"}`, "stop": true})
		}},
	}
	for _, fixture := range fixtures {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", fixture.name, path, stream), func(t *testing.T) {
					frames := exactHTTPCompleteFrames(t, fixture.frames(t))
					h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeFrames(w, frames) })
					key := exactHTTPKey(t, 1)
					body := exactHTTPRequest(path, stream, []map[string]interface{}{{"role": "user", "content": "hello world"}})
					want := exactHTTPOracle(t, refcache.NewTracker(), path, body, frames, 1)
					got := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, path, body, &key), stream)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("reference output fallback differs: got=%s want=%s", mustExactJSON(t, got), mustExactJSON(t, want))
					}
				})
			}
		}
	}
}

func TestExactReferenceHTTPFullCacheJSONQuirk(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFrames(w, exactHTTPCompleteFrames(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello world"})))
			})
			key := exactHTTPKey(t, 1)
			// 5000 ASCII x plus the original serialized-message wrapper = 1261.
			body := exactHTTPRequest("/v1/messages", stream, []map[string]interface{}{{"role": "user", "content": strings.Repeat("x", 5000)}})
			for round := 0; round < 2; round++ {
				got := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/messages", body, &key), stream)
				input := float64(1261)
				if stream {
					input = 0
				}
				if got["input_tokens"] != input || got["output_tokens"] != float64(2) {
					t.Fatalf("round %d reference full-cache JSON fallback changed: %v", round, got)
				}
				field := "cache_creation_input_tokens"
				if round == 1 {
					field = "cache_read_input_tokens"
				}
				if got[field] != float64(1261) {
					t.Fatalf("round %d cache golden: %v", round, got)
				}
			}
		})
	}
}
