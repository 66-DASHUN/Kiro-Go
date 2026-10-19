# Sub2API input/output metering migration

**Goal:** Adopt the default Kiro metering from nianzs/sub2api at 1cd5141b8b978ef2f5121b9805885706991e5000 in Kiro only, as explicitly requested. Push and deploy only Kiro after verification.

**Architecture:** Embed the same Anthropic reference BPE vocabulary and normalization. Count canonical message JSON plus four tokens per message and 150 per tool, with system text counted separately and image data replaced by a marker plus dimension-based visual tokens. Use the same engine for cache-prefix weights and output fallback. Prefer positive upstream output usage; concatenate observed reasoning/text/tool argument JSON for fallback without adding tool names. Keep successful-request cache commits, Key isolation, ratios, short-input simulation and valid zero ordinary input handling. OpenAI-compatible endpoints normalize input to Anthropic messages before applying the same metering. Preserve native usage separately for diagnostics.

This is a local reference tokenizer, not an exact tokenizer for current Claude models. The reference's JSON/nonstream fallback includes internal thinking tags and can replace valid zero input; do not reproduce those serialization defects. Binary attachments remain bounded and are not tokenized as base64. Output budgets remain independent from the billable counting migration so changing estimates does not shorten responses.

**Tech stack:** Go, embedded BPE vocabulary, pkoukk/tiktoken-go, existing Go protocol tests and Kiro-only candidate deployment.

- [x] Generate reference fixtures using the pinned reference source; test old implementation fails on BPE, serialized request and concatenated output.
- [x] Embed tokenizer with upstream notices/license; implement input and output adapters including OpenAI conversion, image handling and native usage preference.
- [ ] Verify count_tokens and JSON/SSE usage agree, cold/warm/append conservation, native output preference, tool arguments, reasoning, Unicode, long input and attachment handling.
- [ ] Run full tests/vet/race and review scoped diff; document remaining deliberate compatibility differences.
- [ ] Push branch, build Linux artifact, probe isolated candidate, switch only Kiro, verify public API and unchanged other-container state.

## Verified discoveries and compatibility decisions

The pinned reference cannot initialize its embedded vocabulary because it decodes the compressed `! 5` rank header as base64. The migration repairs that parser; fixtures from the repaired reference agree with @anthropic-ai/tokenizer 0.0.4. It does not copy the silent chars/4 failure. A 4 KiB chunk bound prevents quadratic work on uninterrupted text, with a documented possible boundary difference.

Observed reasoning is counted independently of display across Chat and Responses. Hosted search maintains a semantic message history, counts each round before cache allocation, and preserves summed usage. Direct-search summaries retain the reference byte-length/4 fallback. URL images and bounded document extraction remain supported, although the reference OpenAI bridge drops remote image URLs. Output budget truncation still uses the pre-existing separate codec.

Independent review found and prompted regression coverage for hosted aggregate overwrite, hidden/inline reasoning parity, and uninterrupted-text runtime cost. Golden text fixtures, request fixtures, image normalization, and both stream/nonstream hosted rounds cover the resulting implementation.
