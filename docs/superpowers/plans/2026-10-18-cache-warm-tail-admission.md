# Warm Cache Tail Admission Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Admit a newly appended cacheable tail when a live prefix matched, while retaining configured-rate admission for cold prefixes.

**Architecture:** Keep cache lookup, accounting allocation, TTL handling, account isolation, and successful-response storage in the current tracker. Change the admission decision after lookup: a live match admits all remaining cacheable tokens; without a live match, the stable prefix-specific rate check controls admission. Increment the diagnostic rule identifier to `prefix-total-v8`.

**Tech Stack:** Go, existing `proxy` package tests, `go test`, `go vet`, `go build`.

---

### Task 1: Add a failing warm-tail regression

**Files:**
- Modify: `proxy/cache_total_rate_test.go`

- [x] **Step 1: Update the current warm-tail regression**

In `TestTotalCacheMatchedPrefixAdmitsNewTail`, keep the existing 10,000-token cold prefix setup and deterministic 1,000-token tail fingerprint that fails the cold threshold. Change the expected result to:

```go
if usage.CacheReadInputTokens != 10000 || usage.CacheCreationInputTokens != 1000 ||
    usage.limitation != "" || billedClaudeInputTokens(11000, usage) != 0 {
    t.Fatalf("matched prefix must admit its entire new tail: %+v", usage)
}
tracker.UpdateWithSettings("account", next, policy, usage)
if warm := tracker.ComputeWithSettings("account", next, policy); warm.CacheReadInputTokens != 11000 || warm.CacheCreationInputTokens != 0 {
    t.Fatalf("successful tail write was not reusable: %+v", warm)
}
```

- [x] **Step 2: Run the test and confirm the failure**

Run: `go test ./proxy -run '^TestTotalCacheMatchedPrefixAdmitsNewTail$' -count=1`

Expected: FAIL because current behavior rejects the tail and bills those 1,000 tokens as ordinary input.

### Task 2: Change the admission branch

**Files:**
- Modify: `proxy/cache_total_rate.go`
- Modify: `proxy/cache_total_rate_test.go`

- [x] **Step 1: Admit a tail only after a live prefix hit**

In `computeTotalCacheUsage`, initialize `cacheWriteAdmitted` to true if no tail exists or `rawRead > 0`. Run the deterministic tail threshold only when `rawCacheable > rawRead` and `rawRead == 0`. Keep the early exits for disabled and zero-rate settings, and keep `cache_admission_miss` for rejected cold prefixes.

- [x] **Step 2: Version diagnostics**

Set `totalCacheRuleVersion` to `prefix-total-v8` and update the exact version assertion in `TestTotalCacheShortInputExplainedInDiagnostic`.

- [x] **Step 3: Run focused cache tests**

Run: `go test ./proxy -run 'TestTotalCache(MatchedPrefixAdmitsNewTail|RejectedColdPrefixStaysOrdinaryAndUnstored|ShortInputExplainedInDiagnostic)|TestStablePrefixAppendAlwaysAdmitsAfterMatch' -count=1`

Expected: PASS, including cold rejection, warm extension, repeated reads, and the diagnostic version.

### Task 3: Verify repository behavior

**Files:**
- Verify: `proxy/cache_total_rate.go`
- Verify: `proxy/cache_total_rate_test.go`
- Verify: `docs/superpowers/specs/2026-10-18-cache-prefix-admission-design.md`

- [x] **Step 1: Run the proxy package suite**

Run: `go test ./proxy -count=1`

Expected: PASS with input conservation, TTL allocation, native calibration, isolation, and JSON/SSE coverage intact.

- [x] **Step 2: Run the full checks**

Run each command: `go test ./... -count=1`, `go vet ./...`, and `go build ./...`.

Expected: all commands exit successfully.

- [x] **Step 3: Check the final diff**

Run: `git diff --check` and `git status --short`.

Expected: only Kiro cache logic, focused tests, and its rule/plan documents are changed; no Sub2API source is changed.
