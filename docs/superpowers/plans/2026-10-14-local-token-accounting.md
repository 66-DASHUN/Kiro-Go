# Local Token Accounting Implementation Plan

> Execute inline in the existing isolated worktree, using test-driven-development and a read-only reviewer at completion.

**Goal:** Fix context-window occupancy being billed as input and use one offline BPE estimator for input, output and simulated cache.

**Architecture:** Keep positive native usage authoritative; use local request/output counts when missing. Embed o200k_base via tokenizer v0.7.0. Preserve key-specific cache policies and normalize cache profiles to the caller's input total.

**Tech Stack:** Go 1.25 module, pure Go tokenizer, existing httptest AWS event-stream fixtures and SQLite configuration tests.

## Task 1: Reproduce incorrect counts before changing production code

Files: proxy/token_accounting_regression_test.go, proxy/handler_test.go.

- [x] Change the existing fallback test to expect local input 999 even when upstream context is 5000.
- [x] Add BPE goldens using existing estimator API:
  ```go
  if got := estimateApproxTokens("hello"); got != 1 { t.Fatalf("got %d", got) }
  if got := estimateApproxTokens(strings.TrimSpace(strings.Repeat("hello ", 100))); got != 100 { t.Fatalf("got %d", got) }
  ```
- [x] Add a Claude profile regression: BuildClaudeProfile(shortRequest, 1).TotalInputTokens must equal 1, despite metadata wrappers.
- [x] Add real handler tests for /v1/messages, /v1/chat/completions and /v1/responses, stream true/false. Mock upstream with answer + contextUsageEvent + meteringEvent, no native usage. Compare fixed input under short/long answers and context 2.05/2.5; input must equal the existing request estimator and cache total must remain bounded.
- [x] Run `go test ./proxy -run 'Test(LocalToken|LocalUsage|FinalizeUsageTokensFallsBack)' -count=1`; confirm numeric assertion failures rather than compile errors.

## Task 2: Implement embedded BPE counting

Files: go.mod, go.sum, proxy/token_estimator.go, proxy/local_tokenizer.go, proxy/local_tokenizer_test.go.

- [x] Pin `go get github.com/tiktoken-go/tokenizer@v0.7.0`, retaining the module's Go version.
- [x] Initialize `tokenizer.Get(tokenizer.O200kBase)` once. Use its `Count` through estimateApproxTokens; retain old character logic in a named fallback.
- [x] Count bounded UTF-8 chunks, preferring whitespace after the midpoint; cover long ASCII, Chinese, code/JSON, emoji, special-token-looking user strings, and concurrent use. No runtime HTTP or filesystem vocabulary reads.
- [x] Run the numeric goldens and tokenizer race tests; add a benchmark for long normal text and long continuous ASCII.

## Task 3: Remove context occupancy from usage fallback

Files: proxy/handler.go, proxy/responses_api.go, proxy/claude_websearch.go, proxy/kiro.go, proxy/handler_test.go.

- [x] Replace the helper with:
  ```go
  func finalizeUsageTokens(inputTokens, outputTokens, estimatedInputTokens, estimatedOutputTokens int) (int, int) {
      if inputTokens <= 0 { inputTokens = estimatedInputTokens }
      if outputTokens <= 0 { outputTokens = estimatedOutputTokens }
      return inputTokens, outputTokens
  }
  ```
- [x] Remove context-to-input callbacks, unused context token variables and the removed helper argument in every Claude, OpenAI, Responses and search call site. Update helper tests for the new signature.
- [x] Keep raw context percentage available in the event parser's existing diagnostic/callback path; do not add it to response input usage.
- [x] Rerun the protocol regressions and native-usage preference test.

## Task 4: Keep cache profile totals aligned and verify full behavior

Files: proxy/cache_tracker.go, proxy/token_accounting_regression_test.go, README.md, README_CN.md.

- [x] In BuildClaudeProfile, derive total from blocks only when the caller supplies no positive total. Otherwise normalize breakpoints to the supplied total without raising it to the metadata-wrapper count.
- [x] Verify cold/warm caches on a long input with policies 80% and 95%, final sum ordinary + read + creation = input; verify short input bypass and /count_tokens agreement.
- [x] Document offline BPE approximation, authoritative native usage, context occupancy exclusion, and preserved cache simulation policy.
- [x] Run `gofmt`, `go mod tidy`, `go test ./... -count=1`, `go vet ./...`, targeted race checks, and a Linux CGO-free build into an ignored directory. Run benchmark once to inspect pathological-input cost.
- [x] Have a read-only reviewer inspect the final diff and act on important findings. Re-run checks after any resulting code edits.
- [x] Commit only scoped code, tests and design/plan documents. Report tested examples, approximation limits and that production remains unchanged.

## Independent-review corrections

- [x] Reproduce and fix Claude/Chat SSE tool output being counted in both the raw text buffer and tool list. Tool-only output was 16 vs expected 8; all 3 protocols, JSON/SSE, tool-only/text+tool now agree.
- [x] Reproduce and fix Hosted Search missing local usage in follow-up rounds, including mixed native/local rounds. Initial input 1200 omitted follow-up input; hidden search output also disappeared.
- [x] Establish a cache snapshot for each hosted round and sum its finalized read/write subsets, rather than scaling the initial request's hit over all rounds. Changed MCP results must create new cache entries. Hold all updates until the hosted request succeeds; regression tests cover MCP cancellation and failed follow-up.
- [x] Count proxy-generated visible search result text in the local output fallback when hosted and client tools coexist; preserve positive native output. Observed RED 14 vs 122; streaming and JSON tests cover both native/missing usage.
- [x] Reproduce and fix missing injected thinking-prompt input in Chat/Responses (1 vs expected 24).

The prior cache fixture supplied total=120 for a long cache prefix and relied on profile inflation to pass the cache threshold. It now supplies the request's local count; preserving the caller's small positive total is independently tested.

## Final verification

- Final `go test ./... -count=1`: passed; proxy 30.812s.
- Final `go vet ./...`, `git diff --check`: passed.
- Linux amd64, CGO=0, trimpath build: passed; SHA256 E7B0C0E77F6A21EBB5785BBF79CDDF6527DE733A7C614394102DEB45AEE6EE1E.
- Final isolated Linux `go test -race ./proxy -run "Test(LocalToken|LocalUsage|FinalizeUsageTokens|PromptCache|RuntimeEveryProtocol|Cache)" -count=1 -p 1`: passed, 25.359s; no production cutover or service changes.
- One-shot Windows benchmark: normal 92k-byte sample 116ms, continuous 100k-byte ASCII 462ms (includes one-shot initialization/noise; bounded chunks avoid merging the entire uninterrupted input quadratically).
- Independent read-only review: all important findings fixed; final targeted combination passed 3.123s, no outstanding important findings.
- Implementation committed locally; no push or deployment performed. Historical database counters left as-is.
