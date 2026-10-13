# Prompt cache prefix and lifecycle optimization

## Goal and authorization

Implement the user's request to optimize Kiro-Go using nianzs/sub2api's cache simulation as a reference. Retain the already agreed global/per-key reference ratios and deterministic input-dependent variation of up to five percentage points. Work in the existing isolated integration worktree; validate locally.

Reference inspected: nianzs/sub2api main at 370c3eb512b1eed3f83585f9e5acb12a6b6421a9, particularly kiro_cache_emulation.go. Use its design principles rather than copying its service dependencies or assuming its accounting is native Kiro telemetry.

## Approach and alternatives

Adopt cumulative content prefixes, complete request accounting and success-only cache lifecycle updates in the existing tracker. This preserves current configuration and keeps all protocols on the same usage snapshot path. Replacing the service with sub2api would add unrelated routing/storage dependencies. Removing configurable variation would contradict the user's earlier selected reference-ratio behavior.

## Request profiles

- Claude retains explicit breakpoints and automatic five-minute fallback breakpoints. Preserve tool-level cache_control during JSON decoding. Include thinking/output effort context in the prefix prelude to avoid hits across different effective prompt modes.
- OpenAI Chat Completions and Responses build a profile from their effective Kiro input: stable model/tool/inference context, each historic message, and the current message. Give historic and current user messages the same semantic wrapper; exclude transport identifiers and duplicated tool definitions from message fingerprints. Include images, documents and tool results as semantic input.
- Store cumulative breakpoints at message boundaries, normalize their estimated weights into the total input token space and include the final current-message breakpoint. Identical complete requests can read the full eligible input. Appending or editing a later message reuses only an unchanged earlier prefix and creates the new tail. Changed model, tool definitions or reasoning context causes a miss.
- Search the latest ten eligible breakpoints. Minimum prefixes stay at 1024 tokens by default and 4096 for Opus. The complete input fingerprint continues to seed deterministic ratio variation.

## Lifecycle

Cache lookup never renews a prefix. Compute dispatch usage once, preserve its cold/warm decision and effective ratios until completion, and update entries only after a successful upstream response. Failed, interrupted and concurrent cold attempts must not create or renew entries.

Support five-minute and one-hour entries. Update preserves an existing longer TTL and expiry when the same content is later requested with a shorter TTL. Normalize TTLs to supported buckets and honor the tracker's maximum; production supports one hour. Expired prefixes miss and successful reads renew via the completion update.

## Accounting

Upstream positive input/output token counts remain authoritative, with existing estimation fallback. Completion scales the dispatch snapshot into upstream input space. Claude ordinary input + creation + reads must equal total input; five-minute + one-hour creation must equal total creation. OpenAI prompt/input tokens stay inclusive and cache details are subsets. Output tokens remain independent of input cache policy. Prefixes below the threshold at final native input size are neither reported nor stored.

## Verification

Test tools-only markers and TTL parsing; reasoning/model/tool/account isolation; complete repeated multi-turn hits and single-turn-to-growing-history reuse; tail edits and tool results; lookup without renewal; expiry and mixed TTL updates; native short-prefix bypass; failed requests and dispatch snapshots. Retain existing all-protocol streaming/non-streaming and ratio regressions. Run the full Go suite, vet, build, focused race tests when supported, and whitespace checks. No database or deployment configuration changes are required.
