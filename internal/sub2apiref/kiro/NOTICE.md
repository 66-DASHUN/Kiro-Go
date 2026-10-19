# Pinned Kiro reference parser

Source: https://github.com/nianzs/sub2api, commit
`1cd5141b8b978ef2f5121b9805885706991e5000`, `backend/internal/pkg/kiro`.
Original source is distributed under LGPL version 3; its repository license is
preserved in `LICENSE`. `SOURCE.json` records original and vendored SHA-256 hashes.

The five copied Go source files are unchanged except for the tokenizer import
path in `translator.go`. `websearch.go` supplies the original web-search
description helpers and embedded-tool-call ID helper required by the translator.
Its request functions and the translator request builder are retained solely to
preserve complete original source. The integration uses only counting/parsing
entry points, never the identity prompt injection or request builder.

`meter.go` is a local metering-only facade. It consumes AWS event-stream bytes,
passes the caller's response mode and context to the original parser, and sends
the reference streaming presentation to `io.Discard`.

## Verified reference behavior

`meter_test.go` was first run against an isolated module containing the original,
unmodified five Kiro files and original tokenizer files/vocabulary from this
commit, and then against the vendored module. The oracle module used the original
`github.com/Wei-Shaw/sub2api` module path and pinned reference gjson/image/sync
versions. All seven usage fixture pairs and cap/error fixtures passed.

Preserved quirks include:

- The default tokenizer can fall back to integer rune-count / 4.
- JSON fallback output counts injected `<thinking>` tags and two newlines;
  streaming fallback counts emitted reasoning without those tags.
- Tool input counts use serialized normalized argument JSON, not tool names.
- Positive native output usage wins over estimated output. Upstream cache fields
  are ignored in favor of local cache emulation.
- JSON falls back to the input estimate after a zero-input cache merge without
  recomputing total tokens. Streaming preserves the merged zero input.
- JSON applies `max_tokens` only to eligible presented text blocks. This changes
  `ResponseBody` usage/stop reason, but leaves `ParseResult.Usage` and
  `ParseResult.StopReason` unchanged. Streaming has no local max-token cap.
- Stream cancellation is checked between frames; callers must close a blocked
  reader/pipe to interrupt a pending frame read. JSON does not accept a context.

These are compatibility requirements, not local corrections to the reference.
