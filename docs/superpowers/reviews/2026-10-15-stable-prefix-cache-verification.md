# Stable prefix cache verification — 2026-10-15

## Scope and implementation

Approved design: `../specs/2026-10-15-stable-prefix-cache-design.md`.
Implementation plan: `../plans/2026-10-15-stable-prefix-cache.md`.
Baseline: `90374f8`; plan commit: `ada6f6d`.

Only Kiro's `prefix_total` accounting changes. A successful cache write saves a cumulative simulated share at each real breakpoint. A later request reads the matched entry's committed share, clamped to the calibrated raw matched length, and applies the floating reference only to newly cacheable content:

```
R = min(saved matched share, M)
W = floor((C - M) * p)
ordinary input = T - R - W
```

Dispatch snapshots retain the original committed counts through native calibration and expiry/pruning during the response. Concurrent successful completions preserve the first live certified accounting basis. A zero-write repeat neither creates shorter breakpoints nor upgrades their TTL. Only the genuine matched entry is renewed, using dispatch time and its original TTL/counts. Legacy entries without share metadata are cold in total mode; legacy mode formulas and storage remain unchanged.

Diagnostics use rule `prefix-total-v2`, expose `matchedShare`, and retain `budget` as the read + write sum for API compatibility. The administration interface and README explain the difference between a reference for new content and the resulting whole-request cache rate.

## Regression evidence

The baseline proxy/config suites passed before implementation. Five new `TestStablePrefix` tests were then run against the old implementation and failed on the intended behavior: appending changed the old read amount; native calibration rescaled that amount; intermediate prefix shares drifted; legacy entries invented an unmeasured share.

After implementation, the original five and two additional regression tests passed. They cover successive appends, references 95%/80%/20%, intermediate expiry, native correction/clamping, restoration of original snapshot metadata after pruning, and first-commit accounting under overlapping completions.

Protocol fixtures exercise Messages, Chat Completions and Responses, each JSON and SSE, with upstream usage mocked locally. They cover cold input, repeat, append, repeated append and 0%. All preserve total input and output counts; appended reads equal the previous cumulative cache amount. Messages JSON/SSE example:

| Scenario | Full input | Ordinary | Read | Write | Output |
| --- | ---: | ---: | ---: | ---: | ---: |
| Cold | 10000 | 1809 | 0 | 8191 | 137 |
| Repeat | 10000 | 1809 | 8191 | 0 | 137 |
| Append | 11000 | 2270 | 8191 | 539 | 137 |
| Repeat append | 11000 | 2270 | 8730 | 0 | 137 |
| Zero reference | 11000 | 11000 | 0 | 0 | 137 |

The append write uses the current request's calibrated new interval, which need not equal the difference between consecutive full-input totals.

## Verification commands and results

- `go test ./proxy -run 'StablePrefix|TotalCache' -count=1` — passed.
- `go test ./proxy -run 'StablePrefix|TotalCacheProtocol' -count=1 -v` — passed, including all six protocol/stream variants.
- `go test ./... -count=1` — passed; proxy 39.622 s, config 5.324 s, pool 1.198 s.
- `go vet ./...` — passed.
- `node --check web/app.js` — passed.
- Python JSON parsing and Chinese/English locale key-set comparison — passed, 690 keys in each locale.
- `git diff --check` — passed; Git's Windows line-ending conversion notices are informational.

Local Windows `go test -race` could not start because CGO is disabled and no C compiler is installed. The same implementation and tests were therefore archived and checked in the existing isolated Linux test environment, without replacing any production container:

- Archive SHA-256: `1ac80138469d263fc84a105e24fad1c7af2ec2db908f348eb9d441b3755ea68c`.
- Go: `go1.26.8 linux/amd64`.
- `go test -race ./config ./pool ./proxy -count=1 -p 1` — passed; config 7.225 s, pool 1.346 s, proxy 72.293 s.
- Isolated container: `kiro-stable-prefix-check-1ac8013846`, exit 0; success marker checked, container removed afterward.
- Local evidence: `D:/AI/阿里服务器/ops/kiro-reverse-release/stable-prefix-check-20261015/race-verified.log` and `source-check.json`.

## Independent review

The independent read-only `stable_cache_final_audit` review compared the implementation against the design and baseline. It found no blocking issues in stable prefix shares, native count calibration, TTL, concurrent commits or legacy-mode isolation. Its targeted tests, JavaScript syntax check and diff check passed. No source edits were required after this review.

## Compatibility and operational limits

- This is local cache usage simulation, not proof of upstream cache hits or a reduction in Kiro Credit consumption. No upstream request/model payload changes are made.
- Existing live shares remain fixed when a nonzero reference changes. Whole-request cache rate can therefore fall outside the current new-content reference range. Zero/disabled still suppress all reported cache use and renewal.
- Entries remain in memory and cold after restart. Native input calibration can clamp a read when the calibrated matched prefix is smaller than its saved share.
- No schema migration, Sub2API changes, routing changes, account/binding/concurrency changes, or historical bill rewrites.
- No production deployment, Git push or new paid inference requests were performed during this optimization.
