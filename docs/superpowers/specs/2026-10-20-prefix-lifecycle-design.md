# Prefix lifecycle cache simulation

Approved scope: Kiro only. Add `prefix_lifecycle`; preserve the pinned `sub2api` mode and all existing settings/defaults. This is a local usage simulation, not evidence of provider caching or saved Kiro credits.

Use stable additive semantic block counts for both total input and prefix boundaries. Include tool definitions, system content, message framing, media and tool results. Count the pending assistant framing outside the cached prefix. Never normalize an old prefix to a later request's total. Output remains native usage when available, otherwise the existing generated-content estimate.

Cold requests create the full eligible prefix; successful repeats read it without creating it again. Appends read the unchanged prefix and create only the eligible suffix. Changed content invalidates that point and its descendants. Explicit cache markers limit eligibility; without markers, content boundaries are implicit 5-minute checkpoints. Short inputs participate. Only explicit 1-hour markers produce 1-hour writes. A longer-lived suffix cannot outlive its required shorter-lived prefix.

Cache scope is effective API key, upstream account and resolved model (dot/dash aliases share identity). A shared outbound key does not distinguish its downstream users. Capture usage at dispatch; commit and refresh only after success. Concurrent cold requests remain cold in their individual usage snapshots. Cache state is in memory and resets on restart.

Ratios are inactive in this mode. Disabling cache is the explicit off switch. Keep total = ordinary + read + create, and 5m create + 1h create = create. JSON/SSE and Messages/Chat/Responses share these rules. Admin UI identifies lifecycle mode and hides its inactive ratios; diagnostics contain counters only.

Acceptance: test cold/repeat/append/edit, explicit tails, TTL expiry, account/key/model isolation, disabled/failed requests, concurrent snapshots, no completion rescaling, protocol parity and legacy reference regressions. Do not claim CCTest passed without running it. No production settings are changed as part of adding this mode.
