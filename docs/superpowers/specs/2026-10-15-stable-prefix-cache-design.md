# Stable simulated prefix accounting

The user approved this change after six live mapped-model cache checks. The accepted scope is Kiro's `prefix_total` mode: preserve the previously reported cached share of a live, unchanged prefix and allocate only newly appended content using the configured reference and deterministic content variation. Do not change Sub2API, model routing, account bindings, concurrency, historical bills, or production deployment.

## Evidence and selected approach

The live sample wrote 2,390 of 2,517 input tokens. An identical repeat read 2,390, but appending 34 input tokens reduced the old read to 2,378 because the entire prefix was multiplied by a new request ratio. The CCMAX reference sequence retained its previous cached prefix while writing the increment.

Keep the existing mode and configuration fields, but store a committed simulated token share alongside each real write breakpoint. Alternatives were keeping request-wide rescaling (retains the defect) or removing user-configured ratios (contradicts the requested reference behavior). The selected approach keeps the reference as a tendency for new cacheable input, not a compulsory per-request target.

## Allocation

Let T be full input, C the last eligible breakpoint, M the live matched prefix in the current calibrated token space, A the share committed for that matched entry, and p the current content-dependent reference. A request snapshots A and its original prefix count at dispatch.

`R = min(A, M)`

`W = floor((C - M) * p)`

`I = T - R - W`

Cold requests have M=A=0. Exact repeats have M=C and W=0. A changed tail or reference setting does not recalculate A. Disabled/zero references still report no cache and do not store or renew. Unsupported or below-minimum prefixes remain ineligible. If native correction makes M fall below the model minimum, discard the match and rebuild only if C remains eligible. Clamp reads when authoritative input counts no longer fit the stored share; never create negative ordinary input.

The reference controls new eligible content. The displayed total rate remains (R+W)/T and can naturally change with prefix coverage, old references, and authoritative count corrections. It is not promised to remain inside a hard per-request band after a setting change. Existing mode names and saved values are preserved; update help text to explain this distinction.

## Entry lifecycle and compatibility

Store the original calibrated prefix count and cumulative simulated cached share in each committed entry. For a newly written breakpoint N after M, its cumulative share is R plus the portion of this request's W attributable to M..N. The final breakpoint stores exactly R+W. A breakpoint receiving no newly written share is not silently created. Keep the first valid committed share stable under concurrent completions; subsequent hits renew lifetime without rewriting that share.

Retain account/model fingerprint isolation, the existing 20-position lookup window, explicit write breakpoints, 5m/1h classification, success-only updates, dispatch-anchored TTL and dispatch snapshots. A longer-lived ancestor can remain reusable after a shorter descendant expires. Legacy entries lack a known committed share and cannot be treated as measured `prefix_total` hits. Other modes retain their existing formulas.

Input count calibration rescales the current request's raw C/M positions, but never multiplies the saved cached share by the new whole-request estimate or ratio. Preserve potential matches and their saved share when the dispatch estimate is below minimum but the final count may qualify. No new service or persistence is introduced; a process restart remains a cold cache.

## Verification and observability

Add regression tests before implementation for appended-tail stability, reference changes, successive increments, intermediate breakpoints/partial expiry, legacy entry isolation, authoritative correction, and JSON/SSE on all three protocols. Retain existing threshold, zero/disabled, privacy, hosted-search, concurrency and legacy tests, updating only assertions that describe the superseded whole-request formula.

Advance the diagnostic rule version and expose the committed matched share without prompt, key or account data. Explain that p describes the new-content allocation and actual total percentages use full input. Update Chinese and English help text and README.

Run targeted regression tests red then green, `go test ./...`, `go vet ./...`, race tests where supported, JavaScript syntax and locale parity checks. Obtain independent code review and resolve important findings. Produce a local verification report; do not deploy or issue new paid inference requests for this implementation task.
