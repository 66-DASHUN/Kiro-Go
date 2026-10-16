# Local usage estimates

When Kiro supplies positive native token counters, those counters take priority.
Otherwise the proxy estimates semantic text with its embedded `o200k_base`
vocabulary. This is not Claude's proprietary tokenizer. Cache emulation splits
the same input total into ordinary input, cache creation and cache read; these
three quantities sum to total input. Cache rates, TTL and key isolation are
independent of the attachment changes described here.

## Attachments

The count endpoint, request accounting and cache profiles share the estimator.
URL images are fetched through the existing public-host and size checks before
both counting and inference. Inline and URL versions of the same bytes therefore
have the same count. Cache fingerprints still depend on attachment content.

- Text documents: decode base64 and UTF-8/BOM-marked UTF-16 before tokenization.
- Images: read PNG/JPEG/GIF/WebP dimensions without decoding pixels. Scale down
  to at most 1568 pixels per edge and 1.15 megapixels, then estimate one token
  per 750 pixels, rounded up. Do not upscale small images. Compressed byte size
  and base64 length do not determine image tokens. Unreadable images use a
  documented fallback of 1536 tokens.
- DOCX: read textual runs from the main document, headers, footers and notes.
- XLSX: read worksheet cell values, resolve referenced shared strings and inline
  rich text, and include cached formula results. Formatting and charts are not
  fully rendered. Unused shared strings are excluded.
- PDF: estimate 2300 tokens per page, using readable page dictionaries or page
  tree counts. This is a midpoint approximation for combined text/visual page
  processing, not PDF text extraction. Compressed/encrypted page trees without
  readable counts fall back to one estimated page, and may undercount large
  documents.
- Legacy binary DOC/XLS, invalid/unsupported documents and archives exceeding
  inspection limits use 1024 tokens per document. These formats remain coarse
  approximations and do not use base64 length as a language token count.

Attachment inspection limits decoded/expanded data and extracted spreadsheet
text to 32 MiB, ZIP entries to 4096, and PDF page estimates to 10000. These are
counting limits; they do not replace the existing request and upstream limits.

## Output limits and billing

The native requested maximum is still forwarded. A local shared semantic budget
also limits returned text, thinking and complete tool names/arguments when the
upstream ignores it. Signatures, thinking delimiters and transport envelopes are
excluded. Stream fragments share accumulated token state. Tool arguments are
admitted whole or omitted, and intentionally incomplete responses report
`max_tokens`. Hosted search rounds share one budget.

Stopping locally closes the upstream response after the limit is detected.
Only native token counters and Credits already observed before that stop can be
retained. Native usage may exceed the returned semantic content, because it
measures upstream generation. Missing metering is not proof of zero upstream
consumption; the proxy cannot determine or guarantee the final Kiro Credits
charged after early closure. In the absence of native token counters, reported
tokens describe the locally counted returned content.

When cache emulation is enabled, JSON and SSE usage include integer read/write
zeros and both 5-minute/1-hour creation fields. When disabled they omit cache
fields consistently. The SSE start snapshot and final cumulative usage are
snapshots of one request and must not be added together.
