# Kiro Prompt Cache 90% Hit Cap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Raise the simulated Claude prompt-cache read ceiling from 85% to 90%, verify the full token-accounting chain, and deploy the verified build to `kiro.dashun9527.com` with an image-and-source rollback path.

**Architecture:** Keep the existing in-memory, per-account prompt cache tracker and introduce one named ratio constant used by the existing cap calculation. Prove the new boundary with a focused red-green regression test, run the complete Go and Docker build checks, then replace only the Kiro-Go Compose service after preserving the current source file and image.

**Tech Stack:** Go 1.23, Go `testing`, Docker BuildKit, Docker Compose, PowerShell, SSH/Paramiko deployment helper.

---

### Task 1: Add the failing 90% cache-read regression test

**Files:**
- Modify: `proxy/cache_tracker_test.go`
- Test: `proxy/cache_tracker_test.go`

- [ ] **Step 1: Add the SHA-256 import and focused boundary test**

Update the imports and append this test:

```go
import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestPromptCacheReadIsCappedAtNinetyPercent(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	fingerprint := sha256.Sum256([]byte("stable-2000-token-prefix"))
	profile := &promptCacheProfile{
		Breakpoints: []promptCacheBreakpoint{
			{
				Fingerprint:      fingerprint,
				CumulativeTokens: 2000,
				TTL:              defaultPromptCacheTTL,
			},
		},
		TotalInputTokens: 2000,
		Model:            "claude-sonnet-4.5",
	}

	tracker.Update("acct-cache-90", profile)
	usage := tracker.Compute("acct-cache-90", profile)

	if usage.CacheReadInputTokens != 1800 {
		t.Fatalf("expected 1800 cache-read tokens, got %+v", usage)
	}
	if usage.CacheCreationInputTokens != 0 {
		t.Fatalf("expected no cache creation for a fully matched prefix, got %+v", usage)
	}
	reported := buildClaudeUsageMap(2000, 0, usage, true)
	if got := reported["input_tokens"]; got != 200 {
		t.Fatalf("expected 200 uncached input tokens, got %#v", got)
	}
}
```

- [ ] **Step 2: Format the test file**

Run:

```powershell
gofmt -w proxy/cache_tracker_test.go
```

Expected: the command exits with status 0 and changes only formatting in the edited test file.

- [ ] **Step 3: Run the focused test and verify RED**

Run:

```powershell
go test ./proxy -run '^TestPromptCacheReadIsCappedAtNinetyPercent$' -count=1 -v
```

Expected: FAIL with `expected 1800 cache-read tokens`; the current 85% implementation reports 1,700 cache-read tokens. A compile error or unrelated failure does not satisfy RED.

### Task 2: Implement the named 90% cache-read ceiling

**Files:**
- Modify: `proxy/cache_tracker.go:14-21`
- Modify: `proxy/cache_tracker.go:158-163`
- Test: `proxy/cache_tracker_test.go`

- [ ] **Step 1: Add the ratio constant**

Add the named constant next to the existing prompt-cache constants:

```go
const defaultPromptCacheTTL = 5 * time.Minute
const maxPromptCacheHitRatio = 0.90

// Anthropic requires cached prefixes to reach a minimum token count before
// caching takes effect. Breakpoints below this threshold are excluded from
// matching and storage to avoid reporting unrealistic 100% cache hits on
// short requests.
const defaultMinCacheableTokens = 1024
const opusMinCacheableTokens = 4096
```

- [ ] **Step 2: Use the constant in the cache cap calculation**

Replace the existing 85% block with:

```go
	// Cap cacheable tokens at 90% of total input to keep at least 10% of a
	// fully matching request uncached. The newest content in a request is
	// never fully served from cache on the current turn.
	maxCacheable := int(float64(profile.TotalInputTokens) * maxPromptCacheHitRatio)
	if lastTokens > maxCacheable {
		lastTokens = maxCacheable
	}
```

- [ ] **Step 3: Format production and test files**

Run:

```powershell
gofmt -w proxy/cache_tracker.go proxy/cache_tracker_test.go
```

Expected: exit status 0.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run:

```powershell
go test ./proxy -run '^TestPromptCacheReadIsCappedAtNinetyPercent$' -count=1 -v
```

Expected: PASS; the test observes 1,800 cache-read tokens, zero cache-creation tokens, and 200 uncached input tokens.

- [ ] **Step 5: Run all prompt-cache tests**

Run:

```powershell
go test ./proxy -run 'PromptCache|BuildClaudeUsageMapIncludesCacheFields' -count=1 -v
```

Expected: all selected tests PASS with no panic or race-like errors.

- [ ] **Step 6: Commit the tested behavior change**

Run:

```powershell
git add proxy/cache_tracker.go proxy/cache_tracker_test.go
git commit -m "feat: raise prompt cache hit cap to 90 percent"
```

Expected: one commit containing only the production calculation and its regression test.

### Task 3: Run complete local verification

**Files:**
- Verify: `proxy/cache_tracker.go`
- Verify: `proxy/cache_tracker_test.go`
- Verify: `Dockerfile`

- [ ] **Step 1: Run the complete Go test suite**

Run:

```powershell
go test ./... -count=1
```

Expected: exit status 0 and every package passes.

- [ ] **Step 2: Compile every Go package**

Run:

```powershell
go build ./...
```

Expected: exit status 0 with no compiler errors.

- [ ] **Step 3: Build the deployment image locally**

Run:

```powershell
docker build --pull=false --tag kiro-go:cache90-verify .
```

Expected: exit status 0 and a final image named `kiro-go:cache90-verify`.

- [ ] **Step 4: Inspect the image without starting the service**

Run:

```powershell
docker image inspect kiro-go:cache90-verify --format '{{.Id}} {{.Config.Cmd}}'
```

Expected: one image ID and the command `[./kiro-go]`.

- [ ] **Step 5: Review the exact implementation diff**

Run:

```powershell
git diff HEAD^ -- proxy/cache_tracker.go proxy/cache_tracker_test.go
git status --short --branch
```

Expected: the diff contains the 90% constant, the single cap calculation change, and the focused test. The worktree has no uncommitted source changes.

### Task 4: Perform a mutation-free production preflight

**Files:**
- Read: `/opt/kiro-go/docker-compose.yml` on the server
- Read: `/opt/kiro-go/proxy/cache_tracker.go` on the server
- Read: current Kiro-Go container and image metadata

- [ ] **Step 1: Select an authorized SSH task record**

Set the known authorized task record and target used by the existing deployment helper:

```powershell
$RemoteSession = 'C:\Users\dashun\.codex\sessions\2026\08\30\rollout-2026-08-30T21-06-49-01a052c7-9f64-7402-b6c0-434a3ba48eab.jsonl'
$TargetHost = '142.214.159.77'
```

Expected: the helper can resolve the authorized SSH username, port, and password without printing the password. If the SSH banner or authentication cannot be completed, stop before all mutation steps and restore access first.

- [ ] **Step 2: Run the read-only server preflight**

Run:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
docker compose config --quiet
container_id=$(docker compose ps -q kiro-go)
test -n "$container_id"
printf 'container=%s\n' "$container_id"
docker inspect --format 'image_id={{.Image}} image_name={{.Config.Image}} status={{.State.Status}}' "$container_id"
grep -n 'maxCacheable :=.*0.85' proxy/cache_tracker.go
sha256sum proxy/cache_tracker.go
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 60
```

Expected: Compose configuration is valid, the service container is running, and the deployed source still contains the 85% calculation. No file, image, or container is changed.

### Task 5: Preserve rollback state and stage the verified source

**Files:**
- Upload: `proxy/cache_tracker.go`
- Create: `/opt/kiro-go/proxy/cache_tracker.go.codex-cache90-backup` on the server
- Create: `/opt/kiro-go/.codex-cache90-compose-image` on the server
- Create: image tag `kiro-go:rollback-cache90-20260831` on the server

- [ ] **Step 1: Preserve the current source and running image**

Run:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
test ! -e proxy/cache_tracker.go.codex-cache90-backup
container_id=$(docker compose ps -q kiro-go)
test -n "$container_id"
image_id=$(docker inspect --format '{{.Image}}' "$container_id")
image_name=$(docker inspect --format '{{.Config.Image}}' "$container_id")
docker image tag "$image_id" kiro-go:rollback-cache90-20260831
cp -p proxy/cache_tracker.go proxy/cache_tracker.go.codex-cache90-backup
printf '%s\n' "$image_name" > .codex-cache90-compose-image
printf 'rollback_image=%s source_backup=%s\n' 'kiro-go:rollback-cache90-20260831' 'proxy/cache_tracker.go.codex-cache90-backup'
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 60
```

Expected: the old image has a rollback tag, the exact old source file is backed up, and the Compose image name is recorded before replacement.

- [ ] **Step 2: Upload the verified production file to a staging path**

Run:

```powershell
$VerifiedSource = (Resolve-Path -LiteralPath 'proxy\cache_tracker.go').Path
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost put $VerifiedSource '/tmp/cache_tracker.go.cache90' --mode 0644
```

Expected: the helper reports the uploaded byte count and SHA-256 digest without modifying the live source path.

- [ ] **Step 3: Verify the staged digest and install the source file**

Run:

```powershell
$SourceSha = (Get-FileHash -Algorithm SHA256 -LiteralPath $VerifiedSource).Hash.ToLowerInvariant()
$RemoteCommand = @"
set -eu
cd /opt/kiro-go
printf '%s  %s\n' '$SourceSha' '/tmp/cache_tracker.go.cache90' | sha256sum -c -
grep -n 'maxPromptCacheHitRatio = 0.90' /tmp/cache_tracker.go.cache90
install -m 0644 /tmp/cache_tracker.go.cache90 proxy/cache_tracker.go
sha256sum proxy/cache_tracker.go
"@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 60
```

Expected: the staged digest matches the locally verified file, the 90% constant is present, and only `proxy/cache_tracker.go` is replaced.

### Task 6: Build and deploy the Kiro-Go service

**Files:**
- Build context: `/opt/kiro-go`
- Preserve: `/opt/kiro-go/data`

- [ ] **Step 1: Build the production service image**

Run:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
docker compose build kiro-go
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 900
```

Expected: Docker BuildKit exits successfully and produces the Compose service image from the staged 90% source.

- [ ] **Step 2: Recreate only the Kiro-Go service**

Run:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
docker compose up -d --no-deps --force-recreate kiro-go
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 180
```

Expected: only Kiro-Go is recreated; the persistent `./data:/app/data` volume remains attached.

### Task 7: Verify production and roll back on any failure

**Files:**
- Verify: `/opt/kiro-go/proxy/cache_tracker.go`
- Verify: current Kiro-Go container, logs, and HTTP endpoint

- [ ] **Step 1: Run server-side post-deployment checks**

Run:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
container_id=$(docker compose ps -q kiro-go)
test -n "$container_id"
test "$(docker inspect --format '{{.State.Status}}' "$container_id")" = 'running'
grep -n 'maxPromptCacheHitRatio = 0.90' proxy/cache_tracker.go
curl -fsS http://127.0.0.1:8080/admin | grep -q 'Kiro-Go'
docker compose logs --tail=100 kiro-go
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 120
```

Expected: the container is running, the deployed source contains the 90% constant, the local admin endpoint responds successfully, and recent logs contain no startup panic or fatal loop.

- [ ] **Step 2: Verify the public endpoint**

Run:

```powershell
curl.exe -fsS --max-time 20 'https://kiro.dashun9527.com/admin' | Select-String -SimpleMatch 'Kiro-Go'
```

Expected: the public HTTPS endpoint returns the Kiro-Go admin application.

- [ ] **Step 3: If any deployment check fails, restore source and image**

Run this rollback only when Task 6 or Task 7 fails:

```powershell
$RemoteCommand = @'
set -eu
cd /opt/kiro-go
test -f proxy/cache_tracker.go.codex-cache90-backup
test -f .codex-cache90-compose-image
cp -p proxy/cache_tracker.go.codex-cache90-backup proxy/cache_tracker.go
image_name=$(cat .codex-cache90-compose-image)
docker image tag kiro-go:rollback-cache90-20260831 "$image_name"
docker compose up -d --no-build --no-deps --force-recreate kiro-go
container_id=$(docker compose ps -q kiro-go)
test -n "$container_id"
test "$(docker inspect --format '{{.State.Status}}' "$container_id")" = 'running'
curl -fsS http://127.0.0.1:8080/admin | grep -q 'Kiro-Go'
'@
$EncodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($RemoteCommand))
py -3.13 'D:\AI\阿里服务器\.codex-remote-ops.py' --session $RemoteSession --known-hosts 'D:\AI\阿里服务器\.codex-known-hosts' --target-host $TargetHost exec $EncodedCommand --timeout 180
```

Expected: the original source and image are restored and the admin endpoint responds again.

- [ ] **Step 4: Record final evidence**

Run:

```powershell
git log -3 --oneline
git status --short --branch
```

Expected: the design, plan, and implementation commits are visible; no source changes remain uncommitted. Report the focused test, complete test suite, build, image, container status, and public endpoint evidence in the final handoff.
