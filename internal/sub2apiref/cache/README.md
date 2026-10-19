# Pinned Sub2API cache and input metering

Reference: `https://github.com/nianzs/sub2api`, revision
`1cd5141b8b978ef2f5121b9805885706991e5000`.

`kiro_cache_emulation.go` retains the reference's pure profile, fingerprint,
token-counting, scaling, TTL, compute, update and pruning implementations.
`reference/kiro_cache_emulation.go.txt` preserves the complete original service
source. `PROVENANCE.json` records upstream paths and original SHA-256 hashes.
The reference license is at `../LICENSE`.

The only adaptations in that source are its package/import paths, removal of
GatewayService/Group/Account wrappers, and use of a stable string account ID
instead of the source's hashed numeric account ID. The account ID remains the
sole tracker namespace. Every tracker instance owns its map instead of sharing
the reference's package-global tracker. `adapter.go` exposes the removed service
wrapper behavior through explicit `Policy`, `Profile`, and `Plan` types.
Policies are already-effective settings; their defaulting belongs to the caller.

The adapter keeps the reference's 1024/4096 raw-token minimums before scaling,
ten-breakpoint search window, final-breakpoint TTL classification, rounding,
matched-entry renewal during prepare, and full-chain renewal during commit.
A zero-ratio plan still commits prefixes. Commit is not made idempotent.
The original nonpositive input argument distinction is retained: only the Chat
Completions wrapper replaces that argument with the computed profile total.

Input metering uses the official algorithm without repairing its tokenizer
initialization fallback. Anthropic messages retain their serialized JSON shape.
Chat Completions passes through the exact Chat Completions → Responses →
Anthropic converters. Responses uses reference legacy-ingress normalization,
additional-tool lifting, client-tool lowering, and Responses → Anthropic
conversion. The OpenAI paths use the reference cache-control prefilter and force
Anthropic streaming. `InputTokensForModel` applies an account model mapping at
the reference converter boundary. These helpers only estimate input; they do
not submit model calls or inject identity prompts.

The compatibility package is copied in full with import path substitutions.
The Claude effort catalog is copied unchanged; its model-ID map and the OpenAI
model classifiers are exact extractions from their original constants files.
The cache-control prefilter omits only a log call. Legacy-ingress normalization
uses the reference default max-output-token floor in place of service config.

Tests reuse original cache fixtures and original token-counter assertions,
and cover cache lifecycle, account scope, TTL, scaling, raw-token eligibility,
prefix lookback and the public protocol counting paths.
