# Sub2API Cache Emulation Adaptation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adapt Sub2API-style Kiro prompt-cache emulation to Kiro-Go with API-key-level settings, no 90% cap, and consistent Claude/OpenAI/Responses usage reporting.

**Architecture:** Extend the existing `promptCacheTracker` rather than importing Sub2API types. Keep cache entries in memory and keyed by Kiro account ID; resolve cache settings from the authenticated API key with global defaults. Build protocol-specific profiles before translation, compute usage before upstream dispatch, and update cache state only after successful responses.

**Tech Stack:** Go 1.21, existing `config` JSON persistence, `proxy` handlers, standard-library SHA-256/JSON/time/synchronization, Go tests.

---

### Task 1: Add global and API-key cache settings

**Files:**
- Modify: `config/config.go` (`ApiKeyEntry`, `Config`)
- Modify: `config/apikeys.go` (API-key lookup/update validation)
- Test: `config/apikeys_test.go`

- [ ] **Step 1: Write failing tests** for default inheritance, API-key override, ratio validation, and backward-compatible JSON loading.

```go
func TestPromptCacheSettingsInheritGlobalDefaults(t *testing.T) {
    settings := EffectivePromptCacheSettings("missing")
    if settings.Ratio != 1 { t.Fatalf("ratio=%v", settings.Ratio) }
}
func TestPromptCacheSettingsUseAPIKeyOverride(t *testing.T) {
    settings := EffectivePromptCacheSettings("key-with-ratio")
    if settings.Ratio != 0.7 { t.Fatalf("ratio=%v", settings.Ratio) }
}
func TestPromptCacheSettingsRejectOutOfRangeRatio(t *testing.T) {
    if err := ValidatePromptCacheRatio(1.1); err == nil { t.Fatal("expected range error") }
}
```

- [ ] **Step 2: Run `go test ./config -run PromptCacheSettings -count=1` and verify the new tests fail because the settings types and resolver do not exist.**
- [ ] **Step 3: Implement `PromptCacheSettings` with enabled, uniform/independent mode, creation ratio, and read ratio fields; add global defaults to `Config`, optional overrides to `ApiKeyEntry`, and an effective-settings resolver keyed by API-key ID.** Ratios must be finite and within `[0,1]`; missing values inherit global settings; uniform mode uses the common ratio for both fields.
- [ ] **Step 4: Run `go test ./config -run PromptCacheSettings -count=1` and verify all new tests pass.**
- [ ] **Step 5: Commit with `git add config && git commit -m "feat: add API key prompt cache settings"`.**

### Task 2: Match Sub2API tracker semantics

**Files:**
- Modify: `proxy/cache_tracker.go`
- Test: `proxy/cache_tracker_test.go`

- [ ] **Step 1: Write failing tests** for automatic default breakpoints, normalized full-prefix tokens, ten-breakpoint lookback, 5-minute/1-hour TTL, expiry, and zero/one ratio scaling.

```go
func TestPromptCacheTrackerAddsDefaultBreakpointWithoutCacheControl(t *testing.T) {
    profile := tracker.BuildClaudeProfile(req, totalTokens)
    tracker.Update("acct", profile)
    if got := tracker.Compute("acct", profile, PromptCacheSettings{Enabled: true, Ratio: 1}); got.CacheReadInputTokens == 0 { t.Fatal("expected read") }
}
func TestPromptCacheTrackerNormalizesLastBreakpointToTotalInput(t *testing.T) {
    usage := tracker.Compute("acct", profile, PromptCacheSettings{Enabled: true, Ratio: 1})
    if usage.CacheReadInputTokens != profile.TotalInputTokens { t.Fatalf("read=%d", usage.CacheReadInputTokens) }
}
func TestPromptCacheTrackerLimitsLookbackToTenBreakpoints(t *testing.T) {
    if got := tracker.maxLookback; got != 10 { t.Fatalf("lookback=%d", got) }
}
func TestPromptCacheTrackerScalesCacheUsageByRatio(t *testing.T) {
    usage := tracker.Compute("acct", profile, PromptCacheSettings{Enabled: true, Ratio: 0.5})
    if usage.CacheReadInputTokens != profile.TotalInputTokens/2 { t.Fatalf("read=%d", usage.CacheReadInputTokens) }
}
```

- [ ] **Step 2: Run `go test ./proxy -run 'PromptCacheTracker(AddsDefault|Normalizes|Limits|Scales)' -count=1` and verify expected failures.**
- [ ] **Step 3: Implement the tracker changes:** add default breakpoints only when no explicit breakpoint exists, normalize cumulative breakpoint tokens to `TotalInputTokens`, scan at most ten recent breakpoints, preserve 5-minute/1-hour TTLs, and apply a supplied effective ratio without any 0.90 cap. Keep account isolation and update-after-success behavior unchanged.
- [ ] **Step 4: Run the focused tracker tests and the existing `go test ./proxy -run PromptCache -count=1`; verify all pass.**
- [ ] **Step 5: Commit with `git add proxy/cache_tracker.go proxy/cache_tracker_test.go && git commit -m "feat: align prompt cache tracker with Sub2API"`.**

### Task 3: Resolve API-key settings in request context

**Files:**
- Modify: `proxy/auth.go`
- Modify: `proxy/handler.go`
- Test: `proxy/apikeys_test.go` or `proxy/handler_test.go`

- [ ] **Step 1: Write a failing test** that authenticates a request with an API key and verifies the handler resolves that key's effective cache settings, while unauthenticated/legacy mode uses global defaults.
- [ ] **Step 2: Run the focused test and verify it fails because only the API-key ID is currently exposed.**
- [ ] **Step 3: Add a context accessor returning the matched `config.ApiKeyEntry` (or its ID plus resolver) and a single handler helper that returns effective `PromptCacheSettings` for the current request.** Do not expose raw key material in logs.
- [ ] **Step 4: Run the focused auth/handler test and verify it passes.**
- [ ] **Step 5: Commit with `git add proxy/auth.go proxy/handler.go proxy/*test.go && git commit -m "feat: resolve prompt cache settings per API key"`.**

### Task 4: Wire cache emulation into all protocol handlers

**Files:**
- Modify: `proxy/handler.go`
- Modify: `proxy/responses_handler.go`
- Modify: `proxy/cache_tracker.go` (protocol profile builders if needed)
- Test: `proxy/handler_test.go`, `proxy/responses_handler_test.go`

- [ ] **Step 1: Write failing integration tests** asserting cache fields for Claude, Chat Completions, and Responses cold/repeated requests, including API-key ratio override.
- [ ] **Step 2: Run the focused tests and verify OpenAI/Responses fail because those handlers currently do not build or compute cache profiles.**
- [ ] **Step 3: Build profiles before translation for each protocol, compute usage after account selection, apply effective API-key settings, and call `Update` only after successful upstream completion.** Preserve existing response formats: Claude uses `cache_creation_input_tokens`/`cache_read_input_tokens`; OpenAI uses `prompt_tokens_details.cached_tokens` and `cache_read_input_tokens`; Responses uses its existing usage structure extended with cache fields.
- [ ] **Step 4: Run `go test ./proxy -run 'Cache|Responses|OpenAI' -count=1` and verify all focused tests pass.**
- [ ] **Step 5: Commit with `git add proxy/handler.go proxy/responses_handler.go proxy/cache_tracker.go proxy/*test.go && git commit -m "feat: wire prompt cache emulation across protocols"`.**

### Task 5: Add cache usage statistics

**Files:**
- Modify: `config/config.go`, `config/apikeys.go` (persisted API-key counters)
- Modify: `proxy/handler.go`, `proxy/responses_handler.go` (record cache counters)
- Modify: `proxy/admin_apikeys.go` (admin payload fields)
- Test: `config/apikeys_test.go`, `proxy/apikeys_test.go`

- [ ] **Step 1: Write failing tests** for recording cache-read/cache-creation counters and returning them in the API-key admin response.
- [ ] **Step 2: Run the focused tests and verify the counters are absent.**
- [ ] **Step 3: Add cumulative `CacheReadTokens` and `CacheCreationTokens` to API-key persisted usage, increment them on successful requests, and serialize them in the existing masked API-key admin response.** Do not change existing total-token or credit semantics.
- [ ] **Step 4: Run focused tests and verify persistence across config reload.**
- [ ] **Step 5: Commit with `git add config proxy && git commit -m "feat: persist prompt cache usage statistics"`.**

### Task 6: Full verification and review

**Files:**
- Modify only files required by prior tasks.

- [ ] **Step 1: Run `gofmt -w` on changed Go files.**
- [ ] **Step 2: Run `go test ./... -count=1` and record the complete result.**
- [ ] **Step 3: Run `go build ./...` and verify exit code 0.**
- [ ] **Step 4: Inspect the complete diff from the implementation base commit for accidental 90% constants, raw API-key logging, TTL changes, or unrelated edits.**
- [ ] **Step 5: Run `git status --short` and report the exact verification evidence before any deployment decision.**
