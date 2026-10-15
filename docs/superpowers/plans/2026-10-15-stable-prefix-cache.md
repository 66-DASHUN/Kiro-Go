# Stable prefix cache implementation plan

**Goal:** Preserve committed cache shares across appended input and apply the user's floating reference only to new cacheable content.

**Architecture:** Extend the existing in-memory prefix entry and dispatch snapshot with immutable accounting metadata; replace only `prefix_total` allocation/storage. Retain all protocol update sites, native input calibration, TTL and legacy mode behavior.

**Tech stack:** Go, existing protocol fixtures, localized plain JavaScript administration interface.

Approved design: `../specs/2026-10-15-stable-prefix-cache-design.md`. Baseline: `90374f8`; existing linked worktree on `integration/kiro-reverse-2.1.0`. Baseline `go test ./proxy ./config -count=1` passes. Execute in this session, with an independent reviewer before the final commit. No deployment or Sub2API edits.

## 1. Reproduce before implementation

- [ ] Add `proxy/cache_stable_prefix_test.go` using real tracker Compute/UpdateWithSettings calls. Cold T=C=10000 then append T=C=11000 with the first fingerprint at M=10000: assert the new read equals the cold write for different content seeds and new reference settings. Assert new write is `floor(1000*p)`.
- [ ] Cover a legacy Update entry: it must not invent a committed read in total mode. Add partial expiry and native calibration regressions with known stored shares.
- [ ] Run `go test ./proxy -run 'TestStablePrefix' -count=1`; confirm failure is the observed read drift, not a compilation error.

## 2. Implement immutable shares

- [ ] In `proxy/cache_tracker.go`, extend the comparable `promptCacheEntry` with committed raw-prefix/cached counts, and `promptCacheUsage` with the matching entry's unscaled snapshot. Leave legacy allocation and legacy Update unchanged.
- [ ] In `proxy/cache_total_rate.go`, only match certified total-mode entries. Allocate reads from the saved share and writes from the newly eligible interval; retain minimum/zero/disabled checks and native calibration snapshot behavior.
- [ ] Store cumulative shares for newly written breakpoints, preserve existing live committed shares, and renew only the actual matched entry with its snapshotted lifetime and share. Keep the first committed accounting basis under concurrent completions.
- [ ] Update only total-mode fixtures/assertions that relied on unmeasured legacy entries or the old request-wide budget. Verify intermediate TTL buckets still sum to W and unchanged repeats never write.
- [ ] Run `go test ./proxy -run 'StablePrefix|TotalCache' -count=1`, then the full proxy/config suites.

## 3. Verify the protocol boundary and explain the reference

- [ ] Add a protocol fixture for Messages, Chat Completions and Responses, each JSON/SSE: mock native input 10000 on cold/repeat and 11000 on append. Parse real wire usage and assert append reads the previous cumulative write/read share without losing total-input conservation.
- [ ] Increment the diagnostic rule version and show the saved matched share. Update `web/app.js` diagnostic label, `web/locales/zh.json`, `web/locales/en.json`, and README to explain that existing shares remain fixed while new content uses the configured floating reference.
- [ ] Run `go test ./... -count=1`, `go vet ./...`, `node --check web/app.js`, locale JSON/key parity and `git diff --check`. Run race checks with an available compiler or document the concrete limitation and verify on the existing isolated Linux build environment if available.

## 4. Review and finish

- [ ] Request an independent read-only code review against `90374f8` and this approved design. Fix important issues and rerun relevant tests.
- [ ] Save exact evidence and compatibility limitations in `docs/superpowers/reviews/2026-10-15-stable-prefix-cache-verification.md`.
- [ ] Commit the reviewed local change on the existing integration branch. Report implementation status and state clearly that production has not been deployed.
