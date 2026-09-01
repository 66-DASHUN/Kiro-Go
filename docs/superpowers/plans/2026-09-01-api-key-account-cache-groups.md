# API Key 账号分组与缓存策略 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让多个对外 API Key 分别拥有独立缓存策略和 Kiro 账号池，并在管理页面完成配置。

**Architecture:** 在 `Account` 上增加可选 `ApiKeyID` 归属字段，配置加载时将历史未归属账号迁移到第一个启用 Key。账号池提供按 Key 过滤的模型选择方法，所有协议入口使用鉴权上下文中的 Key ID；缓存设置沿用全局默认与 API Key 覆盖继承。管理页在现有 Key 卡片内编辑缓存策略和绑定账号，账号表单编辑归属。

**Tech Stack:** Go 1.23、现有 `config`/`pool` 包、net/http JSON API、原生 HTML/CSS/JavaScript 管理界面、Go testing、Docker Compose。

---

### Task 1: Account ownership model and migration

**Files:**
- Modify: `config/config.go`
- Test: `config/config_test.go`

- [ ] **Step 1: Write failing tests** for JSON persistence of `apiKeyId`, migration of empty ownership to the first enabled API Key, and preservation of explicitly assigned accounts.
- [ ] **Step 2: Run** `go test ./config -run 'ApiKey.*Account|Account.*ApiKey' -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement** `ApiKeyID string json:"apiKeyId,omitempty"` on `Account`; add a load-time migration that selects the first enabled `ApiKeyEntry`, assigns only empty ownership values, and saves once when changed. Add validation/helper lookup for an assigned Key.
- [ ] **Step 4: Run** the focused config tests and `go test ./config -count=1`.
- [ ] **Step 5: Commit** `feat: add api key ownership to accounts`.

### Task 2: Key-scoped account-pool routing

**Files:**
- Modify: `pool/account.go`
- Test: `pool/account_test.go`
- Modify: `proxy/handler.go`
- Modify: `proxy/responses_handler.go`
- Modify: `proxy/websearch.go`
- Modify: `proxy/websearch_loop.go`

- [ ] **Step 1: Write failing pool tests** for selecting only accounts whose `ApiKeyID` matches, excluding accounts on retry, and returning nil when a Key has no accounts.
- [ ] **Step 2: Run** `go test ./pool -run 'ApiKey|KeyScoped' -count=1` and confirm failure.
- [ ] **Step 3: Implement** `GetNextForModelForAPIKeyExcluding(apiKeyID, model string, excluded map[string]bool)` by applying ownership filtering before existing cooldown, quota, model, and weighted-round-robin checks. Keep the existing method as a compatibility wrapper for unauthenticated/internal paths.
- [ ] **Step 4: Replace protocol and web-search selection calls** with the Key-scoped method when `apiKeyID` is available; preserve legacy global selection only for an empty ID.
- [ ] **Step 5: Run** pool tests plus handler package tests and commit `feat: route requests through api key account groups`.

### Task 3: API DTOs and safe ownership updates

**Files:**
- Modify: `proxy/handler.go`
- Modify: `proxy/admin_apikeys.go`
- Test: `proxy/apikeys_test.go`
- Test: `proxy/handler_test.go`

- [ ] **Step 1: Write failing API tests** asserting account list/create/update exposes `apiKeyId`, rejects unknown Key IDs, and Key views include bound-account counts plus cache override fields.
- [ ] **Step 2: Run** the focused tests and confirm failure.
- [ ] **Step 3: Implement** `apiKeyId` in account JSON DTO handling, validate referenced Key existence on create/update, reload the pool after ownership changes, and add `boundAccountCount` to Key views. Keep update rollback semantics on save failure.
- [ ] **Step 4: Run** `go test ./proxy -run 'ApiKey|Account' -count=1`.
- [ ] **Step 5: Commit** `feat: expose api key account bindings in admin api`.

### Task 4: Admin page cache and account-group controls

**Files:**
- Modify: `web/index.html`
- Modify: `web/app.js`
- Modify: `web/locales/zh.json`
- Modify: `web/locales/en.json`
- Modify: `web/locales/vi.json`
- Modify: `web/styles.css`

- [ ] **Step 1: Add markup** for a dedicated “Prompt Cache” section with global defaults and per-Key controls: enable switch, mode selector, uniform ratio, independent creation/read ratios, inherit-default action, and a bound-account count.
- [ ] **Step 2: Extend Key rendering and modal state** to load API Key override fields, show whether each field inherits, validate ratios from 0 to 100%, and send explicit `clearPromptCacheOverrides` when requested.
- [ ] **Step 3: Add account ownership select** to the account detail/edit flow, populated from `apiKeysCache`, with an unassigned option and save payload `apiKeyId`.
- [ ] **Step 4: Add Chinese/English/Vietnamese labels and error/success feedback; keep controls keyboard accessible and responsive at narrow widths.
- [ ] **Step 5: Run** `node --check web/app.js` and manually exercise create/edit/inherit flows against a local server.
- [ ] **Step 6: Commit** `feat: add api key cache and account group controls`.

### Task 5: End-to-end regression and release

**Files:**
- Modify: `config/config_test.go`, `pool/account_test.go`, `proxy/apikeys_test.go`, and `proxy/handler_test.go` when extending regression coverage.
- Modify: `docs/superpowers/specs/2026-09-01-api-key-account-cache-groups-design.md` only for approved clarifications.

- [ ] **Step 1: Run** `gofmt -w` on changed Go files, then `go test ./... -count=1`.
- [ ] **Step 2: Run** `go build ./...` and `git diff --check`.
- [ ] **Step 3: Start the local service and verify** Key A and Key B receive only their assigned accounts, cache settings are reflected in usage, and an unassigned legacy account is assigned to `kiro` after reload.
- [ ] **Step 4: Build the deployment image on the server**, preserving the current image and Compose file for rollback; recreate only `kiro-go` and keep `/opt/kiro-go/data` intact.
- [ ] **Step 5: Verify** remote container health, `/health`, `/admin`, and recent logs; report the image tag, commit, and rollback tag.
