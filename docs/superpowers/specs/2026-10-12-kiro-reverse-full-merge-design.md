# Kiro reverse API full integration

Approved scope: the user selected the complete integration of tanu360/kiro-reverse-api, retaining existing accounts, API keys, account ownership, and cache policies; publish to 66-DASHUN/Kiro-Go and deploy only the Kiro service.

## Source versions

- Existing production source: ef60b42cb09b3fefa83fbc2d56467d31d918e848, Kiro-Go 1.1.5 with 30 unpublished customization commits.
- Incoming project: 9290b3565528677085ff75d174f996d120e81a6d, Kiro Proxy 2.1.0.
- Common ancestor: bef941842ab49b06fc15e990edb5070a98460c72.

## Integration

Use the incoming project as the application baseline and retain both histories in a merge commit. Keep its new admin UI, SQLite storage, protocol fixes, response chaining, documents, request audit, snapshots, Microsoft SSO and session affinity. Adapt the existing account API-key ownership and all prompt-cache policy fields into the new config, API, scheduler and UI. Preserve explicit account bindings through all import, OAuth, update, backup, restore and migration paths. Requests authenticated by one key must never choose another key's account, including affinity and retry paths.

Preserve global and per-key cache settings, independent creation/read percentages and the 90 percent cache-read cap. Keep upstream native token accounting where supplied; use the custom emulation policy consistently in Claude, OpenAI and Responses paths.

## Data migration

Read the existing config.json from an isolated copy of the Kiro data directory. Import settings and credentials transactionally into a previously empty kiro.db. Keep IDs, secret values, account states, token expiry, counters, bindings, model mappings and cache settings. Existing populated SQLite state takes priority and must never be overwritten by a stale JSON file. Invalid input fails closed without exposing credentials. Retain config.json and a full pre-deployment backup.

The old global API key and old JSON field spellings are normalized only when necessary; already explicit per-account ownership is authoritative. Verify migration by comparing IDs, counts and hashes rather than printing secrets.

## Response completion

Adopt the incoming stream protocol logic: answer plus a metering trailer can prove completion when stopReason is absent; metadata/envelope finish fields are understood. Reject empty output, broken frames, incomplete tools and reasoning-only responses. Keep lenientStreamIntegrity disabled unless real isolated validation proves the deployed account omits both stop and metering signals; any decision to enable it must be recorded with its limitation.

## Deployment boundary

Only /opt/kiro-go and Docker objects named for Kiro may change. Preserve the existing localhost binding 127.0.0.1:8321, hostname, Kiro Docker network, admin credential, API key values and data mount. Do not change global Docker/network/firewall/DNS/NGINX configuration or restart other services. Build and test before cutover. Limit build resources to protect production load.

Take an all-container ID/start-time/restart-count snapshot, capture other service health, back up the exact Compose configuration, environment and Kiro data, and retain the old image/container for rollback. Validate the candidate on a different loopback port with copied data; candidate state must not share the live SQLite database. Immediately before cutover, stop only old Kiro and migrate a fresh final copy so credential refreshes are retained.

Switch only Kiro. Verify the admin UI, correct/incorrect API-key authentication, account ownership, normal and thinking Claude SSE, OpenAI/Responses completion and the Sub2API route. If acceptance fails, restore the old Kiro container/config immediately. Compare all non-Kiro containers and health against the baseline.

## Verification and publishing

Run existing baseline tests, regression tests for migration/binding/cache/completion, full Go tests, race tests where practical, vet, Linux build and admin UI validation. Windows upstream baseline currently fails SQLite temporary-directory cleanup because tests leave open handles; distinguish and correct lifecycle cleanup or verify the complete suite on Linux before deployment.

Push the merge/integration branch and the previously unpublished deployed history to the user's existing repository without force. Promote only with a fast-forward after checking the remote branch state. Do not publish data, .env, tokens, admin passwords, database files or deployment backups. Report deployed image, commit, upstream provenance, validation and rollback location.
