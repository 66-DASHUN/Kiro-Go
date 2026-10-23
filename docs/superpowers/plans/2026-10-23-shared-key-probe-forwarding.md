# Shared-key probe forwarding implementation plan

**Goal:** Keep the client's existing key and selectively forward PR #29's known text probes to the administrator's configured upstream.

**Architecture:** After authentication and before local model mapping/cache/account use, inspect a bounded copy of a messages request. An opt-in per-key switch gates the exact PR #29 fingerprint rules. Matched probes use the existing reverse proxy with real response bytes and usage; unknown requests retain their original body and stay on Kiro. No canned responses, fake identities, session-wide switching, or upstream fallback for ordinary requests.

**Constraints:** This is best-effort fingerprint matching, not platform authentication. Identical ordinary requests cannot be distinguished. WebSearch, multimodal content, other protocols, unknown prompts, and bodies over 256 KiB are excluded. The UI must explain those limits. Existing full-key forwarding and probe-only forwarding must not be enabled together.

**Execution:** Inline in the existing integration worktree, using test-driven development and verification-before-completion. The user's latest instruction approves implementation following the preceding audit; do not repeat design approval. No subagents. No production activation in this change.

## Steps

- [x] Add failing end-to-end tests in `proxy/cctest_forward_test.go`: shared key, known probe to a local TLS fixture, ordinary request stays local, no-session carryover, original request/response retained, no secret exposure.
- [x] Add a per-key nullable `cctestTextHandlingEnabled` flag to `config/config.go`, snapshot/patch support and mutual-exclusion validation in `config/apikeys.go`, and a locked global upstream getter in `config/cctest_proxy_config.go`. Test persistence, omitted/false patch semantics, and conflicting routes.
- [x] Add `proxy/cctest_probe.go` for the PR's five exact probe templates, bounded JSON inspection and dynamic UUID/tag/cch/probe normalization. Add negative shape/marker/tool/size tests.
- [x] Add `proxy/cctest_forward.go` to preserve unread bodies and route matched messages only. Extract reusable transport/accounting from `proxy/upstream_forward.go`; keep existing full-key behavior. Record route, probe kind and rule version without body or credentials.
- [x] Wire `proxy/handler.go`, `proxy/admin_apikeys.go`, the existing key editor in `web/index.html` / `web/app.js`, and both locale files. Default switches remain off. Preserve masked global URL/key configuration.
- [x] Test config errors, forbidden credentials, oversized/chunked bodies, redirects, failed upstream requests, path aliases and SSE passthrough. Run `go test ./...`, `go vet ./...`, `go build ./...`, `node --check web/app.js`, locale/UI checks, and `git diff --check`.
- [x] Record exact verification and the remaining false-positive/full-coverage limitation. Do not label local tests as CCTest success or a production deployment.
