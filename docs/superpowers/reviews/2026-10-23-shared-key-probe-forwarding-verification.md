# Shared-key probe forwarding verification

Date: 2026-10-23 (Asia/Shanghai). Integration branch: `integration/kiro-reverse-2.1.0`.

## Delivered behavior

The existing client API key can opt in through `cctestTextHandlingEnabled`; no new client key is required. The administrator saves the global CCTest upstream URL and secret in Settings. The secret is not returned by settings/key-view APIs. Omitted flag updates preserve the value, explicit false disables it, and legacy keys default to off.

The messages route authenticates before inspecting the request and before local model mapping, cache simulation or account use. It matches the reviewed PR #29's fixed body markers and five normalized text probes. Unknown, oversized, multimodal, WebSearch, non-messages and multi-message requests remain on the original Kiro path. No IP, key-wide or session-wide detection state is stored.

All matched probes are forwarded to the real configured upstream. This deliberately differs from the PR's three canned-response branches: there are no predefined answers, forged identity claims, fabricated signatures or synthesized upstream usage. Request bytes, upstream response bytes, SSE fragments and upstream usage are preserved. The response identifies the selected route through `X-Kiro-Route` and the fingerprint rule version through `X-Kiro-Probe-Rule`. Diagnostics record the rule and probe kind without the original prompt or API credentials.

Probe-only and full-key forwarding cannot both be enabled. A contradictory restored config fails closed instead of forwarding ordinary requests. Missing probe configuration rejects only matched probes; ordinary requests continue locally. Upstream errors are returned without local fallback or redirect-following.

## Tests and checks

- Before implementation, the shared-key integration test failed with HTTP 503 instead of the expected fixture-upstream HTTP 200. After wiring the route it passed.
- Before admin wiring, the admin round-trip test failed because the switch was not saved. It passed after create/update/view support was added.
- Shared-key tests verify ordinary traffic before and after a matched probe does not call the fixture upstream, invalid/missing credentials are rejected, the disabled switch stays local, and identical requests from an ordinary User-Agent are still indistinguishable.
- Matcher fixtures cover all five templates; negatives cover missing markers, a different CLI version, unknown/sub-string prompts, trailing text, invalid JSON, excessive body size, multiple/assistant messages, WebSearch, image/document/tool-result blocks and null content.
- Transport tests cover byte-exact request/response passthrough; three messages path aliases; SSE flushing; unchanged signature fragments; actual input, output, cache-write and cache-read counters; omitted/explicit-false updates; persistence and snapshot isolation; unread tail/Close preservation for chunked bodies; partial read errors; incomplete configuration; HTTP 401/429/503; redirects; cancellation; and exclusion of unrelated endpoint paths.
- `go test ./config ./proxy -run 'Test(CCTest|DedicatedUpstream|UpdateCCTest)' -count=1`: passed.
- `go test ./...`: passed; proxy package completed in 79.786 seconds.
- `go vet ./...`: passed.
- `go build ./...`: passed.
- `node --check web/app.js`: passed.
- Settings DOM IDs, both locale files and the key-save payload check: passed.
- `git diff --check`: passed. Git emitted only the repository's existing Windows line-ending notices.

## Use and limitations

1. Save URL/key in Settings → CCTest upstream configuration.
2. Edit the existing client key and enable known-probe forwarding. Keep full-key forwarding disabled.
3. A match records `route=cctest_upstream`, the probe kind, and rule `static-flow-pr29-text-v1`.

This is fingerprint routing, not platform-origin authentication. A normal client sending identical content and markers also matches. A changed test-platform version or unknown probe does not match. No claim is made that every CCTest/VeriDrop item is covered or passed. The result for a forwarded request measures the configured upstream, not native Kiro capability.

Verification used only local test fixtures with synthetic credentials. No production configuration was changed, no remote inference request was billed, and no deployment or other-service change is represented by these results.
