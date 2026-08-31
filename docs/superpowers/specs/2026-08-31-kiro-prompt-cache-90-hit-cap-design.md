# Kiro Prompt Cache 90% Hit Cap Design

## Goal

Raise Kiro-Go's simulated prompt-cache read ceiling from 85% to 90% for Claude API requests while keeping the existing cache eligibility and lifetime semantics intact.

## Scope

- Change the maximum simulated cache-read share of total input tokens from `0.85` to `0.90`.
- Introduce a named constant for the ratio so the calculation is explicit and independently testable.
- Keep the default ephemeral cache TTL at 5 minutes.
- Keep explicit long-lived cache TTL normalization at 1 hour.
- Keep the minimum cacheable prefix at 1,024 tokens for non-Opus models and 4,096 tokens for Opus models.
- Keep cache tracking isolated per account and in process memory.
- Do not add an admin setting, persistence, or new API fields.

## Calculation Rules

For a request with total input tokens `T`:

1. The maximum cacheable/readable amount is `floor(T × 0.90)`.
2. At least `T - floor(T × 0.90)` tokens remain outside the simulated cache-read amount.
3. A matching cached prefix contributes to `cache_read_input_tokens`, capped by the maximum above.
4. `cache_creation_input_tokens` remains `max(cacheable_tokens - cache_read_input_tokens, 0)`.
5. Reported uncached `input_tokens` remains `max(T - cache_creation_input_tokens - cache_read_input_tokens, 0)`.
6. The 5-minute and 1-hour cache-creation breakdown continues to derive from the cache breakpoints after accounting for matched tokens.

The first request still creates cache usage rather than reporting a cache read. Repeated matching requests can reach the 90% ceiling only when the matched prefix, account, token threshold, and TTL conditions permit it.

## Implementation

Modify `proxy/cache_tracker.go`:

- Add a package-level `maxPromptCacheHitRatio` constant with value `0.90` near the existing cache constants.
- Replace the hard-coded `0.85` multiplier with the named constant.
- Update the nearby comment from 85% to 90% and clarify that at least 10% remains uncached on a fully matching request.

Modify `proxy/cache_tracker_test.go`:

- Add a focused test that creates a cacheable 2,000-token profile, stores it for an account, computes a repeated request, and asserts exactly 1,800 cache-read tokens, zero cache-creation tokens, and 200 reported uncached input tokens when the final breakpoint covers all 2,000 tokens.
- Retain the existing cache tracker tests unchanged to guard TTL, threshold, fingerprint, and usage-field behavior.

## Verification

- Run the focused cache tracker test once before the production change and confirm it fails because the current result is capped at 1,700 tokens.
- Apply the production change and rerun the focused test; it must report 1,800 cache-read tokens and 200 uncached input tokens.
- Run all prompt-cache tests.
- Run the full Go test suite.
- Build the Kiro-Go binary and Docker image successfully.

## Deployment and Rollback

Deploy to the existing `kiro.dashun9527.com` Kiro-Go service using its current Docker Compose project and persistent `data` volume. Before replacement, record the running container and image identity and preserve a rollback image tag. Rebuild the service from the verified source, recreate only the Kiro-Go container, and verify container health, local HTTP responsiveness, version reporting, and recent logs.

If startup or health verification fails, recreate the service from the preserved image without changing the persistent configuration volume. The deployment may cause a brief interruption while the single Kiro-Go container restarts.

## Non-Goals

- No change to actual upstream Kiro caching.
- No change to request routing, account selection, credit accounting, API authentication, or model-cache behavior.
- No attempt to manufacture a fixed 90% hit rate when the prompt prefix does not match or is ineligible.
