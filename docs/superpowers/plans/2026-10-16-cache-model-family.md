# Cache Model Family Implementation Plan

**Goal:** Prevent total-cache simulation from silently dropping valid Claude versions missing from a fixed version list.

**Architecture:** Restrict recognition to Claude Sonnet/Opus/Haiku with numeric versions. Use local family defaults, canonicalize equivalent model names only inside total-mode cache profiles, and preserve routing and legacy-mode behavior.

**Tech Stack:** Go, existing in-memory prefix tracker, httptest, embedded locale JSON.

## Task 1: Regression evidence

- [x] Add `proxy/cache_model_family_test.go` covering all current aliases, numeric future versions, minimum boundaries and cold/warm/append behavior. Assert unknown/malformed models cannot store cache, equivalent names share profiles, and distinct models remain isolated.
- [x] Run `go test ./proxy -run 'TestTotalCacheFamily|TestTotalCacheAlias' -count=1`; observe missing-rule and alias-fingerprint failures before changing production code.

## Task 2: Minimal correction

- [x] Add `proxy/cache_model.go`: normalize cache model identity and return simulation defaults for supported family syntax.
- [x] Replace `totalCacheMinTokens`'s four-version table with this family policy. Normalize only total-mode Claude/OpenAI profile identity. Do not modify upstream request objects or legacy profiles.
- [x] Update diagnostic labels to say simulation minimum and unrecognized family, avoiding claims about official upstream support.
- [x] Rerun the regression command and confirm success.

## Task 3: Integration and verification

- [x] Extend existing `TestTotalCacheProtocolJSONAndSSE` to exercise Sonnet 4.6, Opus 5.5 and Haiku with source aliases mapped to real test targets. Validate cold/warm/append/zero cases in all three protocols and both response modes.
- [x] Run `go test ./...`, `go vet ./...`, `go build ./...` and `git diff --check`.
- [x] Review the diff for credential exposure, unintended routing/settings changes and cache identity collisions; record results in `docs/superpowers/reviews/2026-10-16-cache-model-family-verification.md`.
- [x] Report local completion and explicitly identify that production remains on its existing version.
