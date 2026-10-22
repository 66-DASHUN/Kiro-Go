# Kiro signature and message envelope compatibility

Scope: Kiro only. References: htesd/kirorsfornewapi commit
6251966aace40790a29ee6c87f94c6fe8fffe7a2 and acking-you/static_flow PR 29.

The signature-only reasoning event is already forwarded. Preserve that behavior
and test it across ordinary, hosted-search and structured-output responses.
Buffered native reasoning must end at answer/tool boundaries: a later signature
must never authenticate a concatenation of previously separate reasoning blocks.
Keep signatures opaque; do not create, decode/rewrite or cryptographically
certify them. Preserve unsigned reasoning without inventing signatures.

Add the documented nullable stop_details field consistently to buffered messages,
message_start and message_delta. No additional stop detail is available from
Kiro, so the value is null. Do not invent usage metadata, change model identity,
select test-only upstreams or alter cache/billing rules.

Implementation and checks:

1. Add regression coverage for native reasoning separated by answer/tool events,
   and response envelope parity across ordinary/search/structured paths.
2. Confirm regressions fail, then add explicit buffered collector boundaries and
   nullable stop_details to each Anthropic response writer.
3. Run focused protocol regressions, full Go tests, build and diff checks.
4. Report local verification separately from deployment and fresh CCTest results.

Limitations: envelope compatibility cannot change an AWS signature into an
Anthropic-direct signature. It also does not implement code execution or produce
Anthropic encrypted search metadata. No CCTest pass is promised.
