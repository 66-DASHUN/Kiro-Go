# Prompt Cache Ratio Fluctuation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Use the configured prompt-cache ratio as a deterministic reference value with a small input-dependent fluctuation while preserving token accounting.

**Architecture:** Keep the existing in-memory per-account tracker and policy resolver. Derive one effective ratio from the configured ratio and a SHA-256 digest of the request profile; apply it to the raw matched or newly cacheable portion, then reconcile ordinary input as the remainder. Use the same calculation when scaling the dispatch snapshot to native completion input tokens.

**Tech Stack:** Go 1.21, standard-library SHA-256, existing proxy cache tracker and tests.

---

### Task 1: Add the failing ratio-fluctuation regression

**Files:**
- Create: `proxy/cache_ratio_regression_test.go`

- [x] **Step 1: Add regression tests** for 48 complete inputs around an 80% reference, exact 0% / 100% endpoints, automatic conversation prefix reuse, and stream-start / completion accounting. Require input conservation and preservation of native output tokens.
- [x] **Step 2: Run the new tests before implementation.**

```powershell
go test ./proxy -run 'TestPromptCache(ConfiguredRatio|RatioEndpoints|AutomaticConversation|StreamStart)' -count=1
```

Observed failures: warm inputs all return 80%; a 100% policy is capped at 90%; automatic follow-up reports no prefix reads; stream start mixes input-token bases.

### Task 2: Implement deterministic effective ratios

**Files:**
- Modify: `proxy/cache_tracker.go`
- Modify: `proxy/handler.go`
- Modify: `proxy/runtime_parity_test.go`

- [x] **Step 1: Add `promptCacheRatioVariation = 0.05` and derive a deterministic variation from the full input fingerprint, model and dispatch total.** Keep endpoint settings exact and shrink the symmetric spread near zero and one.

```go
func variedPromptCacheRatio(ratio float64, profile *promptCacheProfile) float64 {
    if ratio <= 0 || ratio >= 1 || profile == nil {
        return ratio
    }
    fingerprint := profile.InputFingerprint
    if fingerprint == ([32]byte{}) && len(profile.Breakpoints) > 0 {
        fingerprint = profile.Breakpoints[len(profile.Breakpoints)-1].Fingerprint
    }
    hasher := sha256.New()
    hasher.Write(fingerprint[:])
    writeHashChunk(hasher, profile.Model)
    writeHashChunk(hasher, strconv.Itoa(profile.TotalInputTokens))
    digest := hasher.Sum(nil)
    unit := float64(binary.BigEndian.Uint16(digest[:2]))/65535*2 - 1
    spread := math.Min(promptCacheRatioVariation, math.Min(ratio, 1-ratio))
    return math.Max(0, math.Min(1, ratio+unit*spread))
}
```

Add `encoding/binary`, and `InputFingerprint [32]byte` to `promptCacheProfile`. Claude uses its complete cumulative input hash. OpenAI hashes stable configuration, history and the dynamic current message for variation only; stable cache matching still excludes the dynamic turn.

- [x] **Step 2: Apply variation to both effective policy ratios, save them in the dispatch snapshot, and remove the fixed 90% read cap.**

```go
createRatio = variedPromptCacheRatio(createRatio, profile)
readRatio = variedPromptCacheRatio(readRatio, profile)
read := minInt(int(float64(matched)*readRatio), profile.TotalInputTokens)
```

At completion preserve `usage.readRatio` and `usage.creationRatio`. Cap read tokens at `inputTokens` and creation tokens at `inputTokens - usage.CacheReadInputTokens`; preserve the existing TTL-bucket sum.

- [x] **Step 3: Retain default block boundaries when Claude has no explicit marker.** Pre-scan blocks for any positive TTL; with no explicit TTL, create five-minute breakpoints for blocks. Otherwise retain the existing explicit-marker and subsequent message-end rules. Normalize cumulative estimates against the whole input rather than just the last cacheable boundary.
- [x] **Step 4: Use one total-input basis for streaming start.**

```go
messageStartUsage = scalePromptCacheUsage(cacheUsage, cacheProfile, startInputTokens)
```

- [x] **Step 5: Update fixed-ratio regression expectations to bounded reference values.** For independent 25% creation / 50% read, require creation within 20–30% and read within 45–55%, with zero opposite-phase tokens. For a 100% read policy, require the full eligible input. Keep disabled-cache, native totals, concurrency and binding assertions.
- [x] **Step 6: Run the cache and protocol tests.**

```powershell
go test ./proxy -run 'Cache|EveryProtocolPreservesNative' -count=1
```

Expected and observed: PASS.

### Task 3: Update policy and UI wording

**Files:**
- Modify: `web/locales/zh.json`
- Modify: `web/locales/en.json`
- Modify: `docs/kiro-reverse-integration.md`

- [x] **Step 1: Explain the reference value, maximum five-percentage-point variation, exact endpoints, cold writes, and partial-prefix hits in both global and API Key hints.** Identify this as local usage emulation.
- [x] **Step 2: Update the integration document to describe the final behavior.** Preserve historical design records.
- [x] **Step 3: Parse both locale files as JSON and inspect the diff to ensure only hint strings changed.**

### Task 4: Full verification

**Files:**
- No additional files.

- [x] **Step 1: Format changed Go files.**

```powershell
gofmt -w proxy/cache_tracker.go proxy/cache_ratio_regression_test.go proxy/runtime_parity_test.go
```

- [x] **Step 2: Run `go test ./... -count=1`.** All tested packages pass; exit code 0.
- [x] **Step 3: Run `go build ./...`.** Exit code 0.
- [x] **Step 4: Run `git diff --check` and inspect `git status --short`.** No whitespace errors; all changes remain local and uncommitted. No push or deployment in this request.

## Local verification evidence

- 48 complete repeated inputs with an 80% reference: 75.29%–84.89% cache reads; identical input remains stable.
- Claude Messages, OpenAI Chat Completions and Responses, streaming and non-streaming: preserve native input 10000 / output 10 in protocol regressions.
- Claude stream-start regression: start and completion share the effective ratio; completion preserves native input 10000 / output 137.
- Cold concurrency, account/key bindings, expiry and short-input bypass continue to pass.
