# Lifecycle Admission Implementation Plan

> Superseded ratio semantics: this plan's cold-prefix admission behavior was the source of first-request ordinary-input spikes. The current lifecycle read-target rule is documented in `docs/superpowers/specs/2026-10-20-prefix-lifecycle-design.md` and implemented as `prefix-lifecycle-v5`.

> **For agentic workers:** Execute inline with superpowers:executing-plans; request a focused code review before release.

**Goal:** Preserve prefix read/write lifecycles while allowing global and per-Key cold-prefix admission targets, including 90% with ±5 percentage points, and deploy only Kiro.

**Architecture:** One admission decision per wholly cold dispatch, for the final eligible prefix. A scope/content/attempt hash supplies independent uniform samples for the threshold and decision. Retries get a new attempt so a rejected prefix is not permanently excluded. Warm reads and their eligible appended tails bypass admission and retain complete token counts. No per-token ratio scaling. Successful requests alone store/refresh checkpoints.

**Tech Stack:** Go, existing SQLite configuration, vanilla JavaScript, isolated Docker candidate and Kiro-only release tooling.

### Task 1: Define and test admission

- [x] Add `proxy/cache_lifecycle_admission_test.go`: verify exact 0/100 endpoints, 90% threshold range 85–95%, both accepted and rejected attempts, deterministic aggregate distribution, whole-prefix writes, no warming on rejection, retry admission, full warm reads and suffix writes even after target changes.
- [x] Run `go test ./proxy -run 'TestLifecycleAdmission' -count=1`; confirm the existing code fails because 0% cold requests still write.
- [x] Add an attempt counter under the tracker mutex and a domain-separated SHA-256 admission helper in `proxy/cache_lifecycle.go`. Set `cacheAdmissionChecked`, `cacheWriteAdmitted`, effective `totalRatio`, and `cache_admission_miss`. Gate only wholly cold writes; use no fallback lottery over earlier boundaries.
- [x] Change lifecycle-only deterministic fixtures to Ratio=1. Keep explicit target tests at .9. Run all lifecycle tests, including six HTTP paths.
- [x] Extend `config/lifecycle_cache_test.go` to test global .9 inheritance and a per-Key override through database reload.

### Task 2: Expose honest settings and diagnostics

- [x] In `web/app.js`, show/enable the existing ratio field for lifecycle globally and per Key, use a lifecycle admission label, show the ratio in summaries and diagnostics. Preserve independent creation/read controls for other modes.
- [x] Update both locale files: 90% means cold-prefix admission in 85–95%; 0% prevents cold admission, 100% admits all; warm prefixes and suffixes keep full token counts. Actual read rate depends on content reuse/TTL/routing, rather than this target.
- [x] Version diagnostics as `prefix-lifecycle-v2`; expose whether cold admission was checked and whether writes were admitted. Do not log content or credentials.
- [x] Run `go test ./...`, `go vet ./...`, `go build ./...`, Node syntax and locale key parity checks, and `git diff --check`. Request focused review, fix findings and rerun affected checks. Commit and push.

### Task 3: Release only Kiro

- [x] Read-only SSH preflight: validate the user-verified host key using existing authorized login tooling; snapshot Kiro revision, settings and all other container IDs/states/start times/restarts.
- [ ] Package committed source in a dedicated lifecycle release. Verify Linux build/test/vet and targeted race checks without rebuilding other services.
- [ ] Back up Kiro data. Start an isolated candidate with copied data and refresh tokens removed. Set only cache mode to `prefix_lifecycle` and target to .9 globally and for existing cache overrides; preserve all unrelated settings/accounts/keys/bindings/limits.
- [ ] Exercise cold/repeat/append/short/explicit TTL and public protocol paths on the candidate. Confirm normal input + read + write equals total; confirm generated output remains authoritative.
- [ ] During an idle Kiro window, stop/rename only `kiro-go`, copy final data, apply the same cache-only policy, and start the tested image with preserved runtime/network/health settings. Keep rollback container/data.
- [ ] Verify domain health, revision, selected cache policy, a real cold/repeat/append sequence, SQLite integrity, preserved policy shape and unchanged other containers. Record evidence and deployment report; rollback Kiro if acceptance fails.

## Validation before packaging

- Full Windows Go suite, vet, build, JavaScript syntax, locale parity (701 keys) and whitespace checks passed.
- 10,000 deterministic cold prefixes admitted 8,981 (89.81%); effective thresholds ranged from 85.0007% to 94.9975%.
- Six HTTP paths (Messages, Chat Completions, Responses; JSON and SSE), endpoints, retries, full reads and appended writes passed.
- Focused reviewer reported no actionable findings; deployment verification remains pending.
