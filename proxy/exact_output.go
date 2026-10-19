package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"

	refcompat "kiro-proxy/internal/sub2apiref/apicompat"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
)

// exactRequestMeter is request-local state; it is never sent to the upstream.
// Result is assigned only after both transport parsing and reference metering
// finish successfully. The enclosing request owns its cache commit policy.
type exactRequestMeter struct {
	Protocol       string
	Body           []byte
	Model          string
	Stream         bool
	HostedSearch   bool
	EstimatedInput int
	Thinking       bool
	MaxOutput      int
	StopSequences  []string
	CacheUsage     *refkiro.Usage
	Commit         func()
	Result         *refkiro.ParseResult
	ConvertedUsage *refcompat.ResponsesUsage
}

type exactOutputResult struct {
	result    *refkiro.ParseResult
	converted *refcompat.ResponsesUsage
	err       error
}

// parseExactEventStreamContext fans the actual upstream bytes into the pinned
// reference parser without buffering the entire stream. Existing callbacks keep
// their content/credit behavior; only completion usage comes from the reference.
func parseExactEventStreamContext(ctx context.Context, body io.ReadCloser, payload *KiroPayload, callback *KiroStreamCallback) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	meter := payload.exactMeter
	meter.Result = nil
	meter.ConvertedUsage = nil
	requestCtx := refkiro.KiroRequestContext{
		ToolNameMap:          payload.ToolNameMap,
		ThinkingEnabled:      meter.Thinking,
		CacheEmulationUsage:  meter.CacheUsage,
		MaxOutputTokens:      meter.MaxOutput,
		EstimatedInputTokens: meter.EstimatedInput,
		StopSequences:        meter.StopSequences,
	}
	stream := meter.Stream || meter.Protocol == "chat_completions" || meter.Protocol == "chat" || meter.Protocol == "responses" || meter.Protocol == "openai"
	if meter.HostedSearch {
		if stream {
			requestCtx = refkiro.KiroRequestContext{}
		} else {
			requestCtx = refkiro.KiroRequestContext{CacheEmulationUsage: meter.CacheUsage, EstimatedInputTokens: meter.EstimatedInput}
		}
	}
	reader, writer := io.Pipe()
	done := make(chan exactOutputResult, 1)
	go func() {
		var parsed exactOutputResult
		if stream {
			var usageWriter exactSSEUsageWriter
			result, err := refkiro.StreamEventStreamAsAnthropicWithContext(ctx, reader, &usageWriter, meter.Model, meter.EstimatedInput, requestCtx)
			parsed.err = err
			if result != nil {
				parsed.result = &refkiro.ParseResult{Usage: result.Usage, StopReason: result.StopReason}
				parsed.converted = usageWriter.convertedUsage()
			}
		} else {
			parsed.result, parsed.err = refkiro.ParseNonStreamingEventStreamWithContext(reader, meter.Model, requestCtx)
		}
		// A reference frame rejection must unblock a producer currently writing
		// the rest of that frame into the tee.
		_ = reader.CloseWithError(parsed.err)
		done <- parsed
	}()
	stopCancel := context.AfterFunc(ctx, func() {
		// Closing the HTTP body interrupts a blocked upstream Read. Closing both
		// pipe ends also releases a producer or consumer blocked in the tee.
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
		_ = body.Close()
	})
	defer stopCancel()
	defer reader.Close()
	defer writer.Close()

	var callbacks KiroStreamCallback
	if callback != nil {
		callbacks = *callback
	}
	callbacks.OnComplete = nil
	var flushStopFilter func()
	filterStops := stream && !meter.HostedSearch && len(meter.StopSequences) > 0
	if filterStops {
		flushStopFilter = filterExactStopCallbacks(&callbacks, meter)
		callbacks.OnStopReason = nil
	}
	tee := io.TeeReader(body, writer)
	emitted, localErr := parseEventStreamTrackedContext(ctx, tee, &callbacks)
	if localErr == nil {
		// A callback can stop presentation early. The meter still consumes the
		// complete upstream response, including native usage in trailing frames.
		_, localErr = io.Copy(io.Discard, tee)
	}
	_ = writer.CloseWithError(localErr)
	parsed := <-done
	if err := ctx.Err(); err != nil {
		return emitted, err
	}
	if parsed.err != nil {
		return emitted, errors.Join(localErr, fmt.Errorf("reference output meter: %w", parsed.err))
	}
	if localErr != nil {
		return emitted, localErr
	}
	if flushStopFilter != nil {
		flushStopFilter()
		if callback != nil && callback.OnStopReason != nil {
			callback.OnStopReason(parsed.result.StopReason)
		}
	}
	meter.Result = parsed.result
	meter.ConvertedUsage = parsed.converted
	if callback != nil && callback.OnComplete != nil {
		callback.OnComplete(parsed.result.Usage.InputTokens, parsed.result.Usage.OutputTokens)
	}
	return emitted, nil
}

func filterExactStopCallbacks(callbacks *KiroStreamCallback, meter *exactRequestMeter) func() {
	filter := &refkiro.StopSequenceFilter{Sequences: meter.StopSequences}
	var thinking refkiro.ThinkingTextSplitter
	onText := callbacks.OnText
	emit := func(text string, isThinking bool) {
		if !isThinking {
			text = filter.Push(text)
		}
		if text != "" && onText != nil {
			onText(text, isThinking)
		}
	}
	callbacks.OnText = func(text string, isThinking bool) {
		if !isThinking && meter.Thinking {
			thinking.Push(text, emit)
			return
		}
		emit(text, isThinking)
	}
	if original := callbacks.OnToolUse; original != nil {
		callbacks.OnToolUse = func(tool KiroToolUse) {
			thinking.Flush(emit)
			original(tool)
		}
	}
	return func() {
		thinking.Flush(emit)
		if text := filter.Flush(); text != "" && onText != nil {
			onText(text, false)
		}
	}
}
