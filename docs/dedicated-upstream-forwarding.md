# Dedicated upstream forwarding

## Scope and configuration

In Admin → Settings → API keys, create a dedicated key, enable **Forward this
key to an upstream**, and enter an HTTPS base URL and upstream API key. Use the
generated **Kiro key** with the Kiro URL in clients. Keep the upstream key on the
server. An empty password field preserves a saved key; explicit clearing requires
disabling forwarding or supplying a replacement. Existing keys default to local
Kiro behavior. API key authentication must be enabled.

This is routing by an authenticated API key. All API requests for that key follow
the same route. There is no detector recognition, selective test response, identity
rewrite, or generated signature. A test on this key evaluates the configured
upstream, not native Kiro capability.

## Protocol and accounting

- Messages, count_tokens, models, Chat Completions and Responses use the upstream
  before local decoding, model mapping, cache emulation, and Kiro account selection.
- Base URLs may include `/v1`. Local aliases are canonicalized without duplicating
  `/v1`. Request JSON, including unknown fields and model names, is not modified.
- Status, response body, SSE frames, signatures and upstream usage are retained.
  Streams flush as data arrives. Transport errors return 502; no local fallback.
- Redirects are returned without following them. Client cookies, local credentials,
  admin headers and forwarding headers are not sent upstream. Upstream secrets are
  never included in API key list/detail/save responses. Server-side backups still
  contain configuration credentials and must retain their existing protection.
- Local key counters observe reported usage without changing response bytes.
  Anthropic totals include ordinary input, cache creation, cache reads and output;
  OpenAI totals do not count cached input twice. No Kiro Credits are invented.
- JSON observation is bounded at 8 MiB and SSE lines at 1 MiB. Missing/unparseable
  usage is marked unreported rather than estimated; this can undercount local key
  counters for unusual upstream responses. The upstream bill remains authoritative.
- Existing token limits are checked before routing using persisted counters;
  in-flight forwarded requests can exceed a limit before final usage is available.

## Verification and rollout

Tests cover persistence/deep copy, invalid configuration, secret redaction and
preservation, aliases, unchanged payload/model, header isolation, SSE flush,
upstream errors, redirects, normal/disabled key isolation and usage accounting.
Run `go test ./...`, `go vet ./...`, JavaScript syntax checks and a browser create /
edit / clear-key flow. Deploy only the Kiro container after candidate checks and
backups. Preserve data mounts and existing policy; verify other container IDs,
start times and restart counts before and after cutover.
