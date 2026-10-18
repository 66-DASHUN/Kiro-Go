# Structured output and cache boundaries

Scope: Kiro only; Sub2API reference group 41 is read-only. User authorized implementation and deployment.

Evidence: production 9875d45 logs reject structured schemas before upstream with generic 400; tests reproduce legal ECMAScript lookahead/backreference rejection and legacy-format effort loss. Simple live structured probes succeed. Exact CCTest schema remains unavailable (report endpoint 403).

Reference: 5711 cached group-41 requests over seven days average ordinary 86, output 1352, creation 11898, read 192192. These are workload observations, not target quotas. The old CCTest baseline starts warm, so its cold/warm state cannot be copied.

Implementation:
1. Use bounded ECMAScript regex validation, retain offline-only schema refs, preserve effort, return actionable compile errors. Never repair invalid answers or add paid retries.
2. Add an explicit local estimate of message role/boundary (2 tokens/message) and assistant generation prefix (2 tokens/request). Count these once in input totals; cache message framing with its content, leave the generation prefix outside cache. These are estimates, not exact Anthropic tokenizer telemetry.
3. Cold admission walks back to the longest earlier admitted eligible boundary if the last is rejected. Never invent reads; successful writes certify stored entries; matched prefixes read and only appended cacheable content writes. Preserve per-key ratios, zero policy, isolation, TTL and snapshot/native calibration.
4. Run red/green regression, all Go tests/vet, Linux race and real candidate schema/cache/protocol probes. Commit/push exact revision, back up data and cut over only Kiro; compare every other container's identity/start/restart state.

Acceptance: input + read + write equals total; full-content cached Claude requests retain estimated assistant start; explicit uncached tails remain ordinary; cold requests have no reads; ECMAScript patterns accept valid and reject invalid outputs; schema validation failure never commits cache or retries. Official CCTest score is unverified until its real run is available.
