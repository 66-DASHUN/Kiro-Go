# Kiro protocol integrity implementation plan

**Goal:** Preserve tool failures and valid tool parameter schemas, verify real caching and protocol behavior, and deploy only Kiro.

**Architecture:** Keep the existing Kiro transport, native signature passthrough and prefix-total-v10 cache simulator. Correct information loss in the Claude-to-Kiro translator without changing accounts, routing, pricing or cache ratios.

**Tech stack:** Go, Python HTTP probes, Docker and SQLite backup.

## 1. Reproduce information loss

- [ ] Add `proxy/claude_tool_integrity_test.go`: translate error/success tool results through active, historical and tool_choice=none turns; assert the error is still observable.
- [ ] Test that tools retain parameters named `encrypted`, `additionalProperties` and `required`, including nested objects, and preserve literal default objects.
- [ ] Run `go test ./proxy -run 'TestClaudeToolResultStatus|TestToolSchemaPreserves' -count=1`; observe failures before implementation.

## 2. Correct translation

- [ ] In `proxy/translator.go`, map boolean is_error=true to Kiro status=error; preserve failure status in narrated historical results.
- [ ] Traverse schema-valued keywords explicitly in cleanSchema; do not treat property-name maps or default/enum values as schemas. Keep removing Kiro-rejected keywords at actual schema nodes.
- [ ] Run focused regressions, `go test ./... -count=1` and `go vet ./...`; review the diff and commit the verified patch.

## 3. Candidate verification and deployment

- [ ] Archive the exact commit and record SHA256. Derive deployment scripts from the previous release with new names and explicit old-image assertions.
- [ ] Run Linux tests, vet, race tests and build in the resource-limited build container.
- [ ] Start a candidate on port 18334 with a consistent SQLite snapshot and disabled refresh tokens. Assert stream block lifecycle, nonstream response, tool JSON/results, WebSearch results, thinking/signatures, and cold/warm/append caching using real small requests.
- [ ] On success, wait for Kiro to be idle, stop only Kiro, back up its data, retain its old container, start the candidate image on the production port and update the compose image.
- [ ] Verify public requests, health, database integrity, key/account/cache policy equality and unchanged other containers. Roll back on a material production failure.

## Acceptance limits

Live probes prove protocol behavior, not CCTest scoring. The existing CCTest report does not disclose the exact five failing validators. Native signatures must pass through unchanged; no fabricated signatures, token inflation or benchmark-specific response branches are permitted. Record any remaining CCTest uncertainty in the release report.
