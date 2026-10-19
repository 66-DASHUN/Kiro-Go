# Prefix lifecycle implementation plan

1. [x] Record baseline and reproduce the absent lifecycle behavior in configuration and lifecycle tests.
2. [x] Add a separate stable profile, scoped cache computation and successful-completion commit path. Keep all existing modes unchanged.
3. [x] Wire coherent counting through count_tokens, Messages, Chat, Responses and hosted rounds. Preserve native/fallback output accounting.
4. [x] Add the selectable mode and clear ratio-independent UI/diagnostics in both locales.
5. [x] Verify lifecycle unit and HTTP scenarios, failed/concurrent/expiry cases, then run the full Go suite and vet. Review changes and report measured examples and deployment status.

Validation and operating limits: [2026-10-20-prefix-lifecycle-validation.md](../specs/2026-10-20-prefix-lifecycle-validation.md).
