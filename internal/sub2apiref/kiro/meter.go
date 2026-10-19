package kiro

import (
	"context"
	"io"
)

// MeterEventStream consumes the actual AWS event stream using the pinned
// reference parser. It performs no upstream requests and invokes no request
// builder. The stream flag must match the public request's response mode because
// the reference intentionally counts JSON and streaming output differently.
//
// The returned Usage is the parser's accounting result. For JSON, presentation
// truncation can change ResponseBody usage without changing ParseResult.Usage;
// callers needing that presentation should use the original exported parser.
func MeterEventStream(ctx context.Context, body io.Reader, model string, stream bool, requestCtx KiroRequestContext) (Usage, error) {
	if stream {
		result, err := StreamEventStreamAsAnthropicWithContext(ctx, body, io.Discard, model, requestCtx.EstimatedInputTokens, requestCtx)
		if err != nil {
			return Usage{}, err
		}
		return result.Usage, nil
	}
	result, err := ParseNonStreamingEventStreamWithContext(body, model, requestCtx)
	if err != nil {
		return Usage{}, err
	}
	return result.Usage, nil
}
