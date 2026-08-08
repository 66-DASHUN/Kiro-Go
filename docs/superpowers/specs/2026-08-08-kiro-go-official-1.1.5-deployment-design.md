# Kiro-Go Official 1.1.5 Deployment Design

Date: 2026-08-08

## 1. Goal

Deploy the complete official Kiro-Go 1.1.5 codebase to the existing
`kiro.dashun9527.com` service, add the user's Kiro API Key as an EU account,
and verify a real model request succeeds.

The deployed application source must match official commit
`f8f6071c9298a4266ad3e0c7e483d4a2510cbcaf` from
`Quorinex/Kiro-Go`. Local high-cache, custom IAM cross-region, and legacy
request-pipeline changes are intentionally excluded from the deployed branch.

## 2. Source And Branch Strategy

- Preserve `upgrade/kiro-1.1.5` and
  `backup/pre-kiro-1.1.5-upgrade` as historical recovery points.
- Create `official/1.1.5-deploy` directly from official commit `f8f6071`.
- Do not cherry-pick or merge local application-source commits into the
  official deployment branch.
- Keep deployment-only files outside the official source tree where possible.
- Push the official deployment branch to `66-DASHUN/Kiro-Go` without replacing
  the existing upgrade branch.

This structure makes future upstream comparison straightforward: the deployed
application commit is byte-for-byte official, while local experiments remain
available on separate branches.

## 3. Deployment Boundary

Only the Kiro-Go service may be rebuilt or replaced. The deployment must not:

- rebuild, restart, or edit Sub2API, PostgreSQL, Redis, Nginx, or other
  containers;
- modify Cloudflare DNS, WAF, or SSL settings;
- expose port 8321 publicly;
- overwrite `/opt/kiro-go/data`;
- remove the currently running image before the replacement is healthy.

Runtime settings such as host port, admin password, data path, Docker networks,
and restart policy belong to `/opt/kiro-go/compose.yml` and
`/opt/kiro-go/.env`, not to source changes.

## 4. Backup And Rollback

Before replacement, create timestamped backups of:

- `/opt/kiro-go/compose.yml`;
- `/opt/kiro-go/.env` without printing its contents;
- `/opt/kiro-go/data`;
- the current container inspection output;
- the current image ID and immutable image reference;
- the start times of all other running containers.

The old image and container configuration remain available until final
verification succeeds. Rollback recreates only `kiro-go` with the saved image
and compose configuration, then verifies loopback and public health endpoints.

## 5. Build And Release

The release artifact must be built from official commit `f8f6071`. The build
records:

- source commit;
- binary or image SHA-256;
- image ID;
- build timestamp;
- target architecture.

The production Compose file uses an immutable image tag or digest. Deployment
uses `docker compose up -d --no-deps kiro-go` so no sibling service is
recreated.

## 6. Account Provisioning

After the official service is healthy:

1. Open `https://kiro.dashun9527.com/admin`.
2. Choose the official Kiro API Key account method.
3. Submit the user-provided key with region `eu-central-1`.
4. Confirm the stored account is enabled and does not have OAuth refresh
   credentials or a Profile ARN.
5. Confirm the request path targets `runtime.eu-central-1.kiro.dev` with the
   API Key protocol.

The Kiro API Key is never written to Git, build logs, deployment summaries,
shell output, or documentation. It is stored only in the existing protected
Kiro-Go data configuration through the authenticated admin workflow.

## 7. Verification

Source verification:

- `git rev-parse HEAD` equals official commit `f8f6071...` on the source-only
  deployment branch before deployment metadata is applied externally;
- `go test ./...` passes;
- `go build ./...` passes;
- official Web Search, Microsoft SSO, stream cancellation, retry, and stream
  integrity tests are present and pass.

Deployment verification:

- the `kiro-go` container is running and healthy;
- `http://127.0.0.1:8321/health` returns HTTP 200;
- `https://kiro.dashun9527.com/health` returns HTTP 200;
- `https://kiro.dashun9527.com/admin` loads successfully;
- the UI reports version 1.1.5;
- port 8321 remains bound to loopback only;
- all pre-existing non-Kiro containers retain their original start times;
- the EU API Key account appears enabled;
- an authenticated real model request completes successfully;
- logs do not contain the full Kiro API Key.

## 8. Failure Handling

- Build or test failure stops before any production mutation.
- Backup failure stops before container replacement.
- Health-check failure triggers Kiro-Go-only rollback.
- Admin import failure leaves the official service running but does not count
  as task completion.
- A model request failure requires inspecting the redacted response, account
  region, runtime host, and container logs before retrying.
- Any unrelated container restart or configuration change is a deployment
  failure and triggers an immediate stop for review.

## 9. Completion Criteria

The task is complete only when all of the following are true:

- the running source is official Kiro-Go 1.1.5;
- the previous local branches and production data remain recoverable;
- only Kiro-Go was replaced;
- the EU API Key account was added without exposing the key;
- a real model request succeeded through the public service;
- the official deployment branch is available in `66-DASHUN/Kiro-Go`.
