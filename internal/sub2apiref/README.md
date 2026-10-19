# Pinned Sub2API usage rules

Source: nianzs/sub2api `1cd5141b8b978ef2f5121b9805885706991e5000`.

The `sub2api` policy uses the original tokenizer, input protocol converters, cache tracker and raw AWS response parser here. Existing alternative policy modes continue to use their legacy implementations. Source manifests, original excerpts and LGPL-3.0 license are included. These packages make no model calls; Kiro-Go retains authentication, account scheduling, upstream transport and protocol delivery.

The original official tokenizer initialization fallback is intentional. Admission requires 4096 semantic tokens for Opus and 1024 otherwise, before cache normalization. Prefix storage is account-scoped and occurs after upstream HTTP acceptance. Mixed TTL creation follows the last eligible breakpoint. Input/output/cache values, rounding and stream/JSON discrepancies follow the pinned implementation, including JSON full-cache input fallback. No extra ratio jitter or local streaming output cap is applied.

OpenAI requests use original protocol conversion and streaming accounting even when the public response is buffered. Anthropic JSON presentation uses the original cap separately from its pre-cap accounting result. Dedicated hosted search retains the initial request/cache plan. Native Anthropic JSON uses final-round usage. For exactly one recognized search tool after protocol conversion, Anthropic SSE and both OpenAI presentation modes reproduce the original stream filter: every inner message usage event is suppressed, leaving only outer initial input/cache usage, zero output tokens and zero credits, without nested cache TTL counters. Mixed-tool orchestration retains the proxy extension's behavior. Generic direct search counts its reference-format summary using UTF-8 bytes / 4.

The dedicated hosted stream intentionally keeps valid closing SSE frames. The pinned source also suppresses its final terminal frame, which can cause an Anthropic missing-terminal error; Kiro-Go preserves the exposed counters while providing successful protocol closure. This is a transport compatibility difference, not a change to the reference usage numbers.

Kiro-Go extensions still provide transport/model routing, remote-image fetching, stored Responses history and mixed client/hosted tool orchestration. Stored history is included in the reference input converter; these extensions do not imply identical upstream response content or identical capabilities to a different service. The source's reference algorithms are preserved, not evidence of native provider cache hits or exact current Claude tokenization.

Validation includes original-source hash correspondence, isolated parser goldens, full HTTP comparisons against an independent original tracker/parser/converter state, structured streaming and restored-history regressions, and stream cancellation/error tests.

The remote image HTTP client uses Kiro-Go's public-address-only, DNS-pinned transport. This transport adaptation also covers image-shaped values inside nested message tool data, rejects credential URLs/private destinations and prevents environment proxies from bypassing the guard. The original image sizing, URL cache and fallback token rules remain unchanged.
