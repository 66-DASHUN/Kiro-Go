# Shared-key probe routing: PR #29 audit

Date: 2026-10-23. Local branch: integration/kiro-reverse-2.1.0, HEAD a8dceb2.

This is the pre-implementation audit. Following the user's subsequent instruction to optimize using the PR, the shared-key fingerprint route was implemented with real upstream responses. See `2026-10-23-shared-key-probe-forwarding-verification.md` for the implemented behavior and checks. The origin-authentication limitation described here still applies.

## User requirement

Keep the existing client-facing Kiro API key for both normal clients and test platforms. The administrator configures a separate upstream URL and credential on the server. Automatically route test requests to that upstream while keeping ordinary traffic on Kiro. The user accepts that the test report describes the selected upstream, not Kiro's native capabilities. No changes to Sub2API or other services are authorized.

## Reference checked

- PR: https://github.com/acking-you/static_flow/pull/29
- GitHub API verified head: 59cbfeb4435452384d631c858764485835448c99.
- Merge commit: c89df78baa73d6fb906b1524c49c43c5fd8584d0.
- Matcher: crates/llm-access/src/provider/cctest.rs.
- Dispatcher: crates/llm-access/src/provider/kiro_dispatch.rs.
- The local downloaded patch and file diffs were inspected; no reference code was executed.

## What the PR actually does

1. A per-entry-key `kiro_cctest_text_handling_enabled` switch gates inspection. This does not require a separate client-facing test key.
2. The request body must be at most 256 KiB and contain `x-anthropic-billing-header`, `cc_entrypoint=cli`, `cc_version=2.1.153.9bd`, and a messages field. These are body markers, not authenticated platform attestations.
3. It parses JSON, excludes image/document content and WebSearch tools, normalizes dynamic markers, and requires one user text message matching one of five fixed probe templates.
4. Logic-grid and identity-thinking probes use the configured proxy URL/key. Tag-echo, `1+1=?`, and identity-conflict probes return predefined responses. The identity-conflict response contains a hardcoded platform/provider claim. This is not a general detector-forwarding implementation.
5. Unknown probes, changed client-version markers, WebSearch, and multimodal requests are not covered by this fast path. There is no evidence that this matcher covers all CCTest checks or other platforms.

## Isolation boundary

The matcher takes request-body bytes, not an authenticated detector identity. An ordinary client sending an identical body with the same valid entry key has the same match result. A hash of the normalized message is a fingerprint, not proof of origin. Changing the model name, using User-Agent alone, or remembering a client IP/key as a test session cannot resolve that ambiguity.

Consequently, two requirements cannot both be guaranteed by this mechanism: automatic content-only classification under one shared credential, and zero ordinary traffic on the detector upstream. This is an information boundary, not a missing regex. A separate authenticated origin signal could preserve a shared entry key, but none has been established here; user-provided headers or unverified forwarding headers are not such a signal.

## Feasible implementation envelope

If best-effort matching is accepted, use a narrowly versioned allowlist of complete known probe shapes plus independent request markers. Leave unknown and ambiguous requests on Kiro. Do not route solely on arithmetic, identity questions, model names, a User-Agent keyword, or session stickiness. Forward actual upstream bytes and usage; do not copy the PR's canned identity/usage replies. Record the routing reason and rule version without secrets or full request content. Describe results as a test of the selected upstream. This lowers accidental matches but does not authenticate a testing platform or guarantee full test coverage.

This envelope is not recorded as acceptance of residual misclassification: the user's zero-ordinary-traffic requirement remains unresolved.

## Local implementation and validation

- `forwardDedicatedUpstream` currently routes by the authenticated entry's `UpstreamForward` setting. It forwards ordinary `hello` as well when that entry is enabled. It must not be enabled on the shared entry as a substitute for automatic classification.
- Existing uncommitted global CCTest URL/key settings are persisted and masked in settings responses, but are not connected to inference routing. They have been preserved.
- Added regression tests cover normal/test-like content, missing/invalid/disabled credentials, ordinary content on an enabled forwarding entry, and forwarded-usage diagnostics using local TLS fixtures.
- Fixed initialization/emission of upstream usage diagnostics; the original regression failed because the diagnostic was nil, then passed after the fix. Upstream input/output/cache fields remain unchanged and secret values are excluded.
- Focused tests passed. The full `go test ./...` run completed successfully; final proxy result: `ok kiro-proxy/proxy 75.345s` (exit 0). `go build ./...`, `node --check web/app.js`, and `git diff --check` also passed in this work session.
- No live SSH inspection, push, configuration activation, or deployment occurred in this audit. Findings about routing concern the local checkout, not an unverified production state.
