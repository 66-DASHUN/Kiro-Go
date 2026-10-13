# Account Concurrency Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan inline, task by task. No parallel delegation is needed for this shared scheduling change.

**Goal:** Add an editable per-account upstream concurrency limit with default 5 and cancellable waiting.

**Architecture:** Persist the setting on `config.Account` and gate complete upstream invocations through one process-wide account registry. Read fresh configuration while holding the registry mutex; admin updates notify it after persistence. Keep active requests when lowering a limit and wake FIFO waiters when raising it.

**Tech Stack:** Go, SQLite JSON settings, native JavaScript account detail UI.

---

### Task 1: Configuration and admin API

Files: `config/config.go`, `config/account_compat.go`, new `config/account_concurrency.go`, new `config/account_concurrency_test.go`, `proxy/handler.go`, `proxy/account_binding.go`, new `proxy/account_concurrency_test.go`.

- [ ] Write tests using account JSON to require default 5 on new accounts, effective 5 on legacy accounts, admin patch persistence across restart, positive integer validation, and preservation after a stale status update in both credential stores.

```go
var account config.Account
json.Unmarshal([]byte(`{"id":"only","maxConcurrency":2}`), &account)
// Admin PUT {"maxConcurrency":2} must return 200 and GET must return 2.
// PUT values 0, -1, 1.5, "5", null must return 400 without mutation.
```

- [ ] Run `go test ./config ./proxy -run 'TestAccountConcurrency|TestAdminAccountConcurrency' -count=1`; observe missing default/ignored patch failures.
- [ ] Add `Account.MaxConcurrency int`, `AccountPatch.MaxConcurrency *int`, `DefaultAccountMaxConcurrency = 5`, an effective limit method, and latest-config lookup. Normalize omitted values only when creating an account; reject negative additions and nonpositive patches. Preserve current concurrency in `preserveAccountCredentials` for status updates.
- [ ] Return effective concurrency in both admin account serializers; accept only integer JSON numbers in `[1,2147483647]`; map invalid concurrency to 400.
- [ ] Rerun the targeted command and confirm it passes.

### Task 2: Shared upstream limiter

Files: new `proxy/account_concurrency.go`, `proxy/kiro.go`, `proxy/mcp_websearch.go`, `proxy/handler.go`, `proxy/account_concurrency_test.go`.

- [ ] Write an integration test holding five upstream streams open, then mix Messages, Chat and Responses requests (including SSE). The sixth must wait until one stream finishes. Write tests for independent accounts, cancellation, hot limit raise/lower and error release.
- [ ] Run `go test ./proxy -run 'TestAccountConcurrency' -count=1`; observe the sixth invocation reaching upstream.
- [ ] Implement the registry with this API:

```go
type accountConcurrencyLimiter struct {
    mu sync.Mutex
    accounts map[string]*accountConcurrencyState
    waitTimeout time.Duration
}
func (l *accountConcurrencyLimiter) acquire(ctx context.Context, account *config.Account) (func(), error)
func (l *accountConcurrencyLimiter) notify(account *config.Account)
```

States hold active count and FIFO waiter pointers. Acquisition drains capacity using the latest persisted limit; a ready channel grants a slot. Cancellation removes a pending waiter, or returns a granted slot if cancellation won the ready race. A `sync.Once` release decrements active, drains waiters, and deletes an idle state. Waiting uses a 60-second timer and typed local 429.

- [ ] Gate `CallKiroAPIContext` and `performKiroWebSearch` with acquire/defer-release. Notify after successful admin concurrency patch. All endpoint retries stay within the acquired slot.
- [ ] Add focused timeout/no-cooldown and double-release checks, then run `go test ./config ./pool ./proxy -count=1`.

### Task 3: Admin UI

Files: `web/app.js`, `web/locales/zh.json`, `web/locales/en.json`.

- [ ] Insert a number field after account weight with default 5, minimum 1, step 1 and a save button. Use translated labels explaining shared account counting and waiting.

```javascript
async function saveMaxConcurrency(id) {
  const maxConcurrency = Number($('maxConcurrencyInput').value);
  if (!Number.isInteger(maxConcurrency) || maxConcurrency < 1 || maxConcurrency > 2147483647) {
    toast(t('detail.maxConcurrencyError'), 'warning'); return;
  }
  await putAccount(id, { maxConcurrency }, t('detail.saved'));
}
```

- [ ] Bind `saveMaxConcurrency` in the existing detail event delegation. Add matching Chinese and English translation keys.
- [ ] Check JS syntax, JSON parsing and actual rendered field/save behavior with existing browser tooling or a local isolated admin fixture.

### Task 4: Verification and release evidence

- [ ] Run `go test ./... -count=1` and `go vet ./...`, inspect all results.
- [ ] Run Linux race tests for config/pool/proxy and Linux build using the existing release infrastructure; do not change any other service.
- [ ] Review `git diff --check` and the complete change against the design, then commit the verified source. Record tests and actual release status in the final response.

Self-review: default, persistence, stale snapshots, all generation protocols, MCP boundary, waiting, errors, dynamic limits, frontend and single-process deployment boundary are covered by Tasks 1–4.
