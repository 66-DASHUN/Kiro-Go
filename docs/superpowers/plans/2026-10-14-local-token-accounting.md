# Local Token Accounting Implementation Plan

> Execute inline in the existing isolated worktree, using test-driven-development and a read-only reviewer at completion.

**Goal:** Fix context-window occupancy being billed as input and use one offline BPE estimator for input, output and simulated cache.

**Architecture:** Keep positive native usage authoritative; use local request/output counts when missing. Embed o200k_base via tokenizer v0.7.0. Preserve key-specific cache policies and normalize cache profiles to the caller's input total.

**Tech Stack:** Go 1.25 module, pure Go tokenizer, existing httptest AWS event-stream fixtures and SQLite configuration tests.

## Task 1: Reproduce incorrect counts before changing production code

Files: proxy/token_accounting_regression_test.go, proxy/handler_test.go.

- [ ] Change the existing fallback test to expect local input 999 even when upstream context is 5000.
- [ ] Add BPE goldens using existing estimator API:
  ```go
  if got := estimateApproxTokens("hello"); got != 1 { t.Fatalf("got %d", got) }
  if got := estimateApproxTokens(strings.TrimSpace(strings.Repeat("hello ", 100))); got != 100 { t.Fatalf("got %d", got) }
  ```
- [ ] Add a Claude profile regression: BuildClaudeProfile(shortRequest, 1).TotalInputTokens must equal 1, despite metadata wrappers.
- [ ] Add real handler tests for /v1/messages, /v1/chat/completions and /v1/responses, stream true/false. Mock upstream with answer + contextUsageEvent + meteringEvent, no native usage. Compare fixed input under short/long answers and context 2.05/2.5; input must equal the existing request estimator and cache total must remain bounded.
- [ ] Run `go test ./proxy -run 'Test(LocalToken|LocalUsage|FinalizeUsageTokensFallsBack)' -count=1`; confirm numeric assertion failures rather than compile errors.

## Task 2: Implement embedded BPE counting

Files: go.mod, go.sum, proxy/token_estimator.go, proxy/local_tokenizer.go, proxy/local_tokenizer_test.go.

- [ ] Pin `go get github.com/tiktoken-go/tokenizer@v0.7.0`, retaining the module's Go version.
- [ ] Initialize `tokenizer.Get(tokenizer.O200kBase)` once. Use its `Count` through estimateApproxTokens; retain old character logic in a named fallback.
- [ ] Count bounded UTF-8 chunks, preferring whitespace after the midpoint; cover long ASCII, Chinese, code/JSON, emoji, special-token-looking user strings, and concurrent use. No runtime HTTP or filesystem vocabulary reads.
- [ ] Run the numeric goldens and tokenizer race tests; add a benchmark for long normal text and long continuous ASCII.

## Task 3: Remove context occupancy from usage fallback

Files: proxy/handler.go, proxy/responses_api.go, proxy/claude_websearch.go, proxy/kiro.go, proxy/handler_test.go.

- [ ] Replace the helper with:
  ```go
  func finalizeUsageTokens(inputTokens, outputTokens, estimatedInputTokens, estimatedOutputTokens int) (int, int) {
      if inputTokens <= 0 { inputTokens = estimatedInputTokens }
      if outputTokens <= 0 { outputTokens = estimatedOutputTokens }
      return inputTokens, outputTokens
  }
  ```
- [ ] Remove context-to-input callbacks, unused context token variables and the removed helper argument in every Claude, OpenAI, Responses and search call site. Update helper tests for the new signature.
- [ ] Keep raw context percentage available in the event parser's existing diagnostic/callback path; do not add it to response input usage.
- [ ] Rerun the protocol regressions and native-usage preference test.

## Task 4: Keep cache profile totals aligned and verify full behavior

Files: proxy/cache_tracker.go, proxy/token_accounting_regression_test.go, README.md, README.zh-CN.md if present.

- [ ] In BuildClaudeProfile, derive total from blocks only when the caller supplies no positive total. Otherwise normalize breakpoints to the supplied total without raising it to the metadata-wrapper count.
- [ ] Verify cold/warm caches on a long input with policies 80% and 95%, final sum ordinary + read + creation = input; verify short input bypass and /count_tokens agreement.
- [ ] Document offline BPE approximation, authoritative native usage, context occupancy exclusion, and preserved cache simulation policy.
- [ ] Run `gofmt`, `go mod tidy`, `go test ./... -count=1`, `go vet ./...`, targeted race checks, and a Linux CGO-free build into an ignored directory. Run benchmark once to inspect pathological-input cost.
- [ ] Have a read-only reviewer inspect the final diff and act on important findings. Re-run checks after any resulting code edits.
- [ ] Commit only scoped code, tests and design/plan documents. Report tested examples, approximation limits and that production remains unchanged.
