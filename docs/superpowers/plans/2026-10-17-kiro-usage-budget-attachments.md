# Kiro usage budget and attachment accounting implementation plan

> **For agentic workers:** Use superpowers:test-driven-development and superpowers:subagent-driven-development for implementation and independent review. Execute within the existing isolated integration worktree.

**Goal:** Enforce the requested output limit, count attachment content instead of transport encoding, and serialize Claude cache usage consistently.

**Architecture:** Preserve the existing local tokenizer and cache policies. A shared semantic output budget limits text/thinking and admits complete tool arguments atomically; a dedicated attachment estimator counts decoded documents and image dimensions. Keep native upstream usage distinct from local estimates and preserve search counters.

**Tech Stack:** Go, net/http, existing o200k_base tokenizer, image decoding, existing document translation, httptest upstream event streams.

**Scope:** Kiro only. No Sub2API code/configuration edits, production configuration changes, push, or deployment. Preserve mappings, keys, cache modes/ratios, TTL/isolation and account concurrency.

## Task 1: Real output budget

Files: new `proxy/output_budget.go`, `proxy/output_budget_test.go`; integrate with `proxy/kiro.go`, `proxy/handler.go`, `proxy/claude_websearch.go`, and `proxy/stream_integrity.go` as required.

- [ ] Add endpoint regressions: `max_tokens: 8` with 100 repeated words must return at most 8 locally counted semantic tokens and `stop_reason: max_tokens`, in stream and nonstream modes. Assert one upstream call and successful accounting rather than 499 or account failure.
- [ ] Run `go test ./proxy -run 'TestOutputBudget' -count=1 -v`; observe the overlong response failure before editing production code.
- [ ] Implement shared accumulated-content budget, excluding signatures/stream wrappers. Retain native limit forwarding. Never independently sum token counts of small network fragments.
- [ ] Ensure text/thinking share a limit, tool arguments are complete valid JSON or omitted, and truncation takes precedence over tool-use stop reasons. Preserve genuine signatures; never attach a signature to changed thinking.
- [ ] Apply the budget to structured output and all hosted-search rounds, stopping further paid rounds when exhausted. Preserve honest `max_tokens` on incomplete structured JSON.
- [ ] Keep local completion separate from user cancellation/upstream errors. Do not retry, cool down, or rotate accounts on an intentional limit. Preserve observed native usage and Credits; never invent missing metering.
- [ ] Test chunk boundaries, Unicode, tool boundaries, thinking/signatures, structured truncation, search rounds, cancellation, and native usage provenance.

## Task 2: Semantic attachment estimation

Files: new `proxy/attachment_tokens.go`, `proxy/attachment_tokens_test.go`; modify `proxy/token_estimator.go`, count endpoint normalization in `proxy/handler.go`, and cache identity code only if tests expose a difference.

- [ ] Add regressions: identical decoded text in `source.type=text` and base64 must count equally; two 128x128 PNGs with different entropy must count equally; an inline image and its fetched URL must have matching count endpoint/request totals.
- [ ] Run `go test ./proxy -run 'TestAttachment' -count=1 -v`; confirm encoding-driven mismatches before implementation.
- [ ] Decode supported textual document sources before BPE counting. Reuse existing document format parsing for inline file variants. Do not count a base64 envelope as natural language.
- [ ] Estimate images from decoded dimensions with documented resizing and pixel/token rules. Use a clearly documented nonzero fallback for unreadable or unsupported binary attachments; do not infer image complexity from compressed byte size.
- [ ] Estimate PDF visual-page cost from bounded page inspection, without pretending compressed streams are decoded text. Reuse standard-library ZIP/XML for DOCX/XLSX text if available, with explicit expansion bounds. Clearly document formats that retain an approximate fallback.
- [ ] Normalize remote images identically in count and inference endpoints, reusing existing public-host protections and download limits. Preserve content-sensitive cache fingerprints.
- [ ] Verify nested tool-result attachments, count/formal usage/cache conservation, and unchanged ordinary text counts.

## Task 3: Consistent Claude usage fields

Files: `proxy/translator.go`, `proxy/cache_tracker.go`, new `proxy/usage_serialization_test.go`; integration changes only where required.

- [ ] Add serialization parity regression for cache enabled/disabled, stream/nonstream, buffered structured, and search responses. Enabled zeros must include read/write and both 5m/1h children as integers; disabled cache fields must be handled consistently.
- [ ] Run `go test ./proxy -run 'TestUsageSerialization' -count=1 -v`; observe missing zero fields.
- [ ] Reuse `buildClaudeUsageMap` or equivalent serialization in `ClaudeUsage`, preserving required input/output and hosted search counters. Retain cache conservation: ordinary input + read + write equals total input.
- [ ] Confirm start snapshots and final cumulative usage are not added together; retain native usage behavior.

## Task 4: Review and validation

- [ ] Review specification compliance, fix gaps, then independently review code quality and fix findings.
- [ ] Run new targeted regressions, `go test ./... -count=1`, `go vet ./...`, and `go build ./...`. Run race tests when a working supported compiler is available.
- [ ] Inspect `git diff --check`, changed file list, and final diff for scope. Document the estimator approximation and native-versus-local usage limitation.
- [ ] Report actual test results and source locations. Leave the completed change local and production unchanged.
