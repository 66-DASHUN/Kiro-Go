# Cache Read Hit Ratio Correction Plan

> **For agentic workers:** Execute inline using test-driven development and verify each step before release.

**Goal:** Keep lifecycle-mode ordinary input low by always creating a cold eligible prefix and interpreting the configured ratio as a warm cache-read hit target.

**Architecture:** Cold requests write the complete eligible prefix after successful upstream completion. Existing cached prefixes use one deterministic, scope/content/attempt sample with the configured target and ±5 percentage point variation. A simulated warm miss rewrites the complete eligible prefix; a warm hit reads the saved prefix and writes only a new eligible suffix. Disabled cache remains fully disabled. Other modes retain their existing semantics.

**Tech Stack:** Go, existing lifecycle tracker, admin UI locale strings, Go test suite.

---

### Task 1: Prove cold writes and warm-hit semantics

**Files:**
- Modify: `proxy/cache_lifecycle_admission_test.go`
- Modify: `proxy/cache_lifecycle_http_test.go`

- [x] Replace cold-admission expectations with regression cases for 0%, 90%, and 100%: enabled cold cacheable input always writes the eligible prefix; ordinary input is only the uncovered tail and assistant framing.
- [x] Seed a live prefix, then issue enough successful warm dispatch snapshots to verify a 90% ±5pp read-hit rate. Every miss must rewrite the entire eligible prefix and preserve low ordinary input; every hit must read the matched prefix and write only an appended suffix.
- [x] Run the focused read-hit regression before implementation, observe the old 100% warm reads and zero-write 0% cold behavior, then rerun after implementation.

### Task 2: Implement and document the corrected lifecycle policy

**Files:**
- Modify: `proxy/cache_lifecycle.go`
- Modify: `proxy/cache_tracker.go`
- Modify: `proxy/cache_total_rate.go`
- Modify: `proxy/cache_lifecycle_admission_test.go`
- Modify: `proxy/cache_lifecycle_http_test.go`
- Modify: `web/app.js`
- Modify: `web/locales/zh.json`
- Modify: `web/locales/en.json`
- Modify: `docs/superpowers/specs/2026-10-20-prefix-lifecycle-design.md`

- [x] Move the varied ratio decision from cold-prefix creation to matched warm-prefix reads. Keep the cold path write-complete and preserve snapshot/commit-after-success behavior.
- [x] Make diagnostics state whether a read lookup was checked and whether it hit; update the rule version and UI/help text so the setting is not described as cold admission.
- [x] Preserve cache accounting conservation, TTL boundaries, scope isolation, zero/disabled behavior, concurrency snapshots, and all non-lifecycle modes.
- [x] Run the focused lifecycle suite, then `go test ./...`, `go vet ./...`, `go build ./...`, `node --check web/app.js`, locale parity, and `git diff --check`.

### Task 3: Review and release only Kiro

- [x] Inspect the final diff against the lifecycle design and protocol regressions; do not alter Sub2API or other services. The focused independent review found no actionable cache-lifecycle issues.
- [x] Run the Kiro-only release preflight and verify the public health endpoint, container/data paths, and current global/per-Key cache settings. Deployment verification remains pending; preserve account/config data and `.codex-release/`.
- [ ] Report CCTest protocol checks as unverified unless a new real CCTest run provides evidence; do not claim that aggregate-score changes prove protocol compatibility.
