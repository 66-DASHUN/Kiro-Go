# Prompt cache prefix and lifecycle implementation plan

**Goal:** Improve input cache matching and lifecycle accuracy while preserving existing reference-ratio behavior.

**Architecture:** Existing content tracker with dispatch snapshots and successful-completion updates. OpenAI profiles cover the entire effective input using stable message wrappers and normalized prefix weights. Claude decoding retains tool markers and profile context.

**Tech stack:** Go, SHA-256 fingerprints, existing mutex-protected in-memory tracker and HTTP fixtures.

## Task 1: Regression evidence

- [x] Add proxy/cache_prefix_lifecycle_test.go to exercise tool-marker decoding, reasoning isolation, mixed TTL preservation, read-only lookup, OpenAI complete repeats, tail changes, history growth, images/tool-result semantics, native short-input bypass and failed protocol requests.
- [x] Run `go test ./proxy -run 'TestCacheOptimization' -count=1` and confirm expected behavioral failures before production changes. Initial repeat reported only 2036/10000 reads; missing tool TTL, lookup renewal and downgrade regressions also failed as expected.

## Task 2: Claude profile context

- [x] In proxy/translator.go retain ClaudeTool.CacheControl (`json:"cache_control,omitempty"`). The tracker reads it for TTL and canonical fingerprints omit it.
- [x] In proxy/cache_tracker.go add thinking/output context to buildCachePreludeBlock, skip nil requests, and preserve all tool-definition semantics in its wrapper.
- [x] Run tool/context focused regressions.

## Task 3: Complete OpenAI profiles

- [x] Replace BuildOpenAIProfile's history-only calculation with semantic user/assistant message blocks, including the final current user message. Model/tool/inference/reasoning context belongs in the prelude; tools never appear twice in user message blocks.
- [x] Normalize all cumulative message breakpoints by total input divided by the estimated complete-input weight. Only eligible breakpoints participate in matching/storage.
- [x] Update the old history-only read assertion to require reads for the prior prefix and creation for a changed current tail. Verify full repeats and history growth.
- [x] Reproduce and correct nil tool-definition context invalidating earlier history when a new tool result arrives.

## Task 4: Success-only lifecycle

- [x] Remove expiry renewal from ComputeWithSettings. Keep renewal in Update only. Preserve the maximum TTL and expiry of existing live entries.
- [x] Honor supported maximum TTL in the constructor/updates; production uses the one-hour maximum while automatic breakpoints default to five minutes.
- [x] Confirm final native short-input profiles produce no cache entry and failed attempts leave cache state unchanged.

## Task 5: Integration and verification

- [x] Document the final matching/lifecycle behavior in docs/kiro-reverse-integration.md.
- [x] Run `gofmt` on changed Go files, `go test ./... -count=1`, `go vet ./...`, `go build ./...`, and `git diff --check`: all passed. Attempted focused race tests; this Windows environment has CGO_ENABLED=0 and no gcc/clang compiler available, so `-race` could not execute. Existing concurrent dispatch regression passed for all three protocols.
- [x] Inspect the complete diff for changes outside cache behavior. Changes are limited to tracker logic, tool cache metadata, tracker initialization, regression tests and documentation. No remote push or deployment performed.

## Final local fixture evidence

All three protocols preserved upstream total input 10000 and output 137. With an 80% reference ratio, identical repeated multi-turn requests reported reads 8194 for Claude Messages, 8447 for Chat Completions and 7932 for Responses; JSON and SSE results matched within each protocol. Cold requests reported creation using the same effective ratio and zero reads. Partial-prefix reuse, native short-input bypass, model/tool/reasoning/account isolation, failed-request lifecycle and concurrent cold-request snapshots passed.
