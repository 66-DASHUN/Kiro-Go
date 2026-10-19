# Sub2API prefix cache alignment

Approved scope: Kiro only. Reference: nianzs/sub2api commit
1cd5141b8b978ef2f5121b9805885706991e5000.

Add a distinct `sub2api` mode and make it the default for new configurations.
Retain legacy modes. During this release migrate existing `prefix_total` policies
to `sub2api`, preserving all ratios, keys, bindings, limits and accounts.

Chain content fingerprints; use explicit cache boundaries and propagate their
TTL to later message ends. Without explicit boundaries use message ends. Scale
the final eligible boundary to total input, as the reference does. Search the
last ten boundaries for a live match. Multiply matched read and unmatched write
by the configured ratio using rounding, without jitter or admission lottery.
Ordinary input is the remainder. Commit prefixes only after successful calls.
Keep short-input simulation, Key isolation, accurate mixed-TTL subdivision and
existing actual output/reasoning counting. Do not copy the reference's zero-input
fallback or claim this is native upstream cache telemetry.

1. Regression tests: cold, warm, append, ratios 0/90/100%, expiry, scope/model
   isolation, explicit TTL, short prompts, completion calibration and protocol usage.
2. Implement engine, config defaults, UI and mode-aware diagnostics.
3. Run focused/full tests, vet and Linux race/build checks; commit and push.
4. Back up Kiro; build isolated candidate; migrate candidate policies and test.
5. Switch only kiro-go during idle, retaining rollback container and DB backup.
6. Verify production usage, SQLite, preserved runtime/settings and unchanged other containers.
