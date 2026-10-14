# Total cache rate implementation plan

> **For agentic workers:** Use superpowers:subagent-driven-development to implement this coherent cache subsystem task, then spec review and quality review. No production deployment, push, external paid calls, or Sub2API edits in this request.

**Goal:** Make each Key's reference ratio control (read + write) / complete input, with content variation and sound virtual prefix lifecycle.

**Architecture:** Add prefix_total rather than silently redefine prefix. Keep prefix/uniform/independent saved modes. Separate content-block lookup candidates from stored write breakpoints. Snapshot once at dispatch; scale that snapshot at completion. Build a bounded, secret-free diagnostic view to explain usage.

**Tech Stack:** Go, existing SQLite configuration, plain JS/HTML localized administration interface.

Baseline: bdc07ba, integration/kiro-reverse-2.1.0 in the existing isolated worktree. go test ./... passed on 2026-10-15. Authoritative design: docs/superpowers/specs/2026-10-15-total-cache-rate-design.md.

## Task 1 — Implement the cache subsystem with red/green verification

Files: config/config.go, config/prompt_cache.go, new config total-cache tests; proxy/cache_tracker.go (prefer extracted proxy/cache_total_rate.go for focused allocation/diagnostics), new proxy total-cache tests; proxy/openai_usage.go; the three Claude successful completion update sites in handler.go/claude_websearch.go; web/index.html, web/app.js, web/locales/{zh,en}.json. Add a focused authenticated diagnostic route only if needed by the admin view, following current routing conventions.

- [x] Write failing config tests before production edits: prefix_total accepted; new/missing mode defaults to prefix_total at .95; explicit prefix/uniform/independent persist unchanged after reload; per-Key overrides/inheritance persist including 0% and disabled.
- [x] Run: go test ./config -run 'TotalCache' -count=1. Expected failures due to missing mode/default.
- [x] Add PromptCacheModePrefixTotal = "prefix_total", validation and default handling, retaining explicit legacy semantics. For the new mode creation/read ratios are legacy inactive fields: the single Ratio controls the sum, explain this in UI and docs.
- [x] Write failing tracker tests exercising these exact calculations with effective p=.8 and no variation in the allocation unit test:

```text
T=10000,C=10000,M=0     => R=0,W=8000,I=2000
T=10000,C=10000,M=10000 => R=8000,W=0,I=2000
T=11000,C=11000,M=10000 => R=8000,W=800,I=2200
T=10000,C=2000,M=2000   => R=2000,W=0,I=8000
p=0 or disabled        => R=W=0 and no store/renew
```

- [x] Run: go test ./proxy -run 'TotalCache' -count=1. Ensure observed failures reflect missing behavior rather than syntax problems.
- [x] Implement B=min(C,floor(T*p)), R=floor(B*M/C), W=B-R, I=T-B. Clamp invalid inputs, handle C=0. Use existing deterministic variation range (80 -> 75..85,95 -> 90..95), retain p from dispatch during native count calibration. Do not requery cache at completion. Fully matched content always W=0.
- [x] Add red tests for marker migration with a stable 6802 prefix, 20-position inclusive window, no phantom candidate storage, consecutive tool_use/tool_result grouping, expired/mutated/model/account isolation, native threshold recalibration, zero-ratio no update.
- [x] Implement content-block candidate lookback only for the new mode; explicit write breakpoints remain separate. For requests with no explicit markers preserve the existing local automatic behavior and document it. A run of same consecutive tool type counts as one position; prelude metadata counts zero. Renew an actually matched live entry only after success, even if it is no longer an explicit write breakpoint. Do not renew every lookup candidate.
- [x] Normalize supported model aliases/thinking suffixes and test thresholds: Sonnet4.5 1024; Haiku4.5,Opus4.5/4.6 4096. Unmaintained models in prefix_total report no cache. Avoid unrelated changes to legacy model support.
- [x] Split W5m/W1h from the raw NEW interval M..C using proportional allocation and largest remainder; do not classify the old discount as new writing. Keep 5m default and explicit client TTL. Test mixed TTL and renewal W=0.
- [x] Apply mode-aware success update in all Claude/OpenAI/hosted-search paths. Test zero and disabled neither create nor renew. Requests that fail do not update. Concurrent snapshots remain unchanged when another completion updates the tracker.
- [x] Add bounded secret-free completion diagnostics (T,C,M,p,B,R,W,I,TTL split, rule version, limitation reason and an independent diagnostic ID). Show total cache/read/write percentages separately in the admin cache panel. Authenticate any new endpoint; no request content or keys in diagnostics. Use an in-memory ring with a stated capacity and time retention, no new infrastructure.
- [x] Add the new mode/default in JS normalization, forms, summaries and both locales. Old modes must remain labeled as old semantics; 0% in prefix_total means no reads/writes. Do not globally describe prefix legacy as new behavior.
- [x] Protocol regression fixtures: /v1/messages, /v1/chat/completions, /v1/responses, JSON and SSE, cold/warm/zero, output unchanged, native total conserved. Reuse existing upstream test server, never call live Kiro.
- [x] Run targeted go test ./config ./proxy -run 'TotalCache' -count=1, then go test ./..., go vet ./..., node --check web/app.js, JSON locale parsing and locale key parity. Attempt go test -race ./proxy ./config if the installed toolchain supports it; record any environment limitation accurately.
- [x] Self-review diff and report DONE or concrete remaining issues. Do not commit until verification passes. Keep changes reviewable in the existing branch; no push or deployment.

## Task 2 — Independent spec compliance review

- [x] Read final diff against baseline, compare all design requirements; explicitly examine TTL scaling, moving markers, duplicate writing, zero policy and unsupported models. Return file/line evidence for gaps, do not modify.
- [x] Implementer fixes valid gaps, reruns targeted checks; reviewer rechecks.

## Task 3 — Independent quality review and final verification

- [x] Review algorithm, bounds, concurrency, auth, diagnostic privacy, legacy persistence and test effectiveness. Fix critical/important findings and recheck.
- [x] Run fresh final checks and write docs/superpowers/reviews/2026-10-15-total-cache-rate-verification.md with exact results and limitations.
- [x] Verify scope excludes Sub2API/production/secret data. Report local implementation completion and online unchanged. Do not imply deployment.

## Completion notes (2026-10-15)

All implementation and local verification tasks are complete. The moving-marker fixture uses a generated stable prefix rather than hard-coding the audit sample's 6802 tokens. Diagnostics use independent IDs, not historical billing IDs; top-level cache_control parsing is outside this change. Race detection is blocked by CGO_ENABLED=0 and the absence of a C compiler. The exact checks, review fixes and scope are recorded in ../reviews/2026-10-15-total-cache-rate-verification.md. No push, production deployment, configuration migration or Sub2API change was performed.
