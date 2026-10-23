# Remove full-key upstream forwarding

Date: 2026-10-23 (Asia/Shanghai).

The user requested removal of full-key forwarding while retaining known-probe forwarding.

## Changes

- Removed the per-key full-forwarding field, destination/secret editor, view fields, translations, configuration patch/clone support, and all seven dispatch hooks.
- Removed the historical Credits-limit exemption attached to full-key forwarding.
- Legacy stored `upstreamForward` fields are ignored during decoding and no longer emitted on save. They cannot activate a route. Existing client keys and probe switches remain intact.
- Admin create/update requests that submit `upstreamForward` return HTTP 400 with a removal explanation. This guard does not store or activate the removed setting.
- Retained only the global CCTest destination/secret and per-key probe switch. Matched probes use the byte-preserving transport in `proxy/cctest_upstream.go`; unmatched traffic uses Kiro.
- Retired tests specific to the removed mode. Preserved transport coverage under probe tests, including protocol headers, secret filtering, actual upstream usage, immediate SSE flushing, upstream failures and loop prevention. Unrelated OpenAI-Beta CORS coverage remains.

## Verification scope

Regression tests first reproduced four failures: a legacy configuration still forwarded ordinary traffic, admin APIs accepted the removed field, the legacy setting bypassed Credits limits, and it conflicted with a preserved probe switch. All four pass after removal. Existing probe tests pass, including shared-key selection and unchanged responses/usage.

`go test ./...` passed, with the proxy package completing in 81.976 seconds. `go vet ./...`, `go build ./...`, `node --check web/app.js`, `git diff --check`, and targeted DOM/locale checks passed. No production configuration, Sub2API instance or other service was modified. This is a local code change; deployment is a separate action.

The probe fingerprint's existing limitation is unchanged: identical ordinary requests can match the known test templates. Removing full-key forwarding does not turn fingerprint matching into platform authentication.
