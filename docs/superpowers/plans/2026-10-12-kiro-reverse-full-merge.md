# Kiro reverse API integration implementation plan

> For agentic workers: use superpowers:subagent-driven-development for separated implementation and review tasks. The user has authorized push and production deployment scoped exclusively to Kiro.

**Goal:** Merge Kiro Proxy 2.1.0 into the existing customized Kiro repository and deploy a tested candidate without affecting other services.

**Architecture:** Incoming 9290b356 supplies the application baseline. Preserve both Git histories and port custom ownership/cache behavior to its SQLite config and redesigned UI. Migrate a copied legacy state and switch only the existing Kiro container after acceptance.

**Tech Stack:** Go 1.25+, SQLite modernc, vanilla JavaScript admin UI, Docker Compose, Python/Paramiko deployment orchestration.

## Task 1: integration baseline

- [x] Compare source, baseline tests, license and deployment configuration.
- [x] Isolate work at .worktrees/kiro-reverse-full-merge on integration/kiro-reverse-2.1.0.
- [ ] Commit the design and plan; preserve incoming source in a merge with 9290b356 and deployed ef60b42 ancestry.
- [ ] Use incoming source for shared application files; retain local docs and records of custom behavior.

## Task 2: configuration and migration

Files: config/config.go, config/apikeys.go, config/credentials.go, config/legacy_migration.go, config/custom_cache.go, main.go; corresponding *_test.go files.

- [ ] First add behavior tests for JSON-to-SQLite import, secret/ID preservation, idempotency, invalid input, existing DB precedence, per-key cache inheritance and config API ownership validation.
- [ ] Run the new tests and observe failure against the incoming baseline: `go test ./config -run 'Legacy|Binding|PromptCache' -count=1`.
- [ ] Add Account.ApiKeyID, all existing cache fields and validation/helpers using ef60b42 definitions with kiro-proxy imports. Normalize/migrate old JSON into SQLite in one transaction. Invoke import before normal Load and never overwrite populated SQLite settings.
- [ ] Verify tests and Windows SQLite handle lifecycle; preserve original data files and avoid logging secrets.

## Task 3: ownership and cache runtime

Files: pool/account.go and new ownership tests; proxy/handler.go, proxy/cache_tracker.go, proxy/responses*.go, proxy/claude_websearch.go, proxy/admin_apikeys.go and binding/import/OAuth paths.

- [ ] Add tests proving key-a can never select key-b accounts through round-robin, affinity, failover, API imports or OAuth. Port the existing cache regression cases under the new module name.
- [ ] Observe targeted failure, then adapt key filtering at every selection boundary. Use the request's validated API key ID, preserving a global pool only when authentication is explicitly disabled.
- [ ] Apply EffectivePromptCacheSettings to Claude, OpenAI and Responses paths, preserving native token counters and failure semantics.
- [ ] Expose fields in safe admin DTOs and settings patches without replacing omitted values.
- [ ] Run `go test ./pool ./proxy -count=1` and inspect all GetNext/ExecuteRetry selection call sites.

## Task 4: admin UI parity

Files: web/app.js, web/index.html, web/styles.css, web/locales/en.json, web/locales/zh.json.

- [ ] Add account key binding selection/visibility to add/import/edit/OAuth workflows in the incoming UI.
- [ ] Add global and per-key cache policy editing matching preserved backend field names and percent ranges; display inheritance explicitly.
- [ ] Validate JavaScript syntax and inspect rendered desktop/mobile UI with an isolated data fixture. Verify changing a binding/override persists and omitted settings remain unchanged.

## Task 5: review and build

- [ ] Independently review spec coverage before code quality; fix important findings.
- [ ] Run `go test ./... -count=1`, `go vet ./...`, targeted race tests and Linux amd64 build. Require full Linux suite if Windows cleanup prevents reliable full testing.
- [ ] Confirm strict completion accepts answer+metering without stopReason and rejects broken frames/reasoning-only output.
- [ ] Commit source with both ancestry parents, record exact SHA and push without force to 66-DASHUN/Kiro-Go. Verify remote ref equals local commit.

## Task 6: isolated candidate and cutover

- [ ] Capture exact Kiro configuration and non-Kiro container/health baseline to /opt/kiro-go/deployment/20261012-reverse-merge.
- [ ] Back up Compose, environment and live data; retain old image and rollback command.
- [ ] Build a candidate with CPU/memory limits; run it on a free localhost port with a copied data directory and identical credentials. Verify migration hashes and API ownership.
- [ ] Send minimal hi requests for haiku/sonnet normal/thinking and require valid terminal events; verify OpenAI and Responses, including one Sub2API-shaped Claude request.
- [ ] Stop only old Kiro, preserve its container for rollback, copy its final live state and start the new Kiro with the original localhost binding and network.
- [ ] Verify public admin, SSE and Sub2API route. On failure restore old Kiro immediately.
- [ ] Compare every non-Kiro ID/start/restart value and service health against baseline. Record final image, Git SHA, source commit, migration counts, acceptance and rollback location.
