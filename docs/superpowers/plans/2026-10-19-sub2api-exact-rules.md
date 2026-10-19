# Sub2API exact usage rules implementation plan

User direction: input, output and caching must follow the reference project's rules, superseding previous short-input caching and metering fixes. Reference is pinned to nianzs/sub2api `1cd5141b8b978ef2f5121b9805885706991e5000`. Publish and deploy only Kiro; preserve service topology and user account/Key configuration.

Architecture: vendor reference tokenizer, cache profile/tracker and protocol conversion/counting primitives under `internal/sub2apiref`. Keep the source algorithms unchanged; expose narrow adapters for the existing Kiro HTTP/pool/storage code. Feed the reference output meter the actual upstream event stream, with matching stream/JSON context. Current `sub2api` mode will use this reference path. Existing alternative cache modes remain explicitly separate; deployed Keys already select `sub2api`.

Observable reference behavior includes default tokenizer initialization fallback, 1024/4096 cache minimums before normalization, last-breakpoint TTL classification, ten-prefix lookback, exact rounding and expiry renewal, account scope, and stream/JSON usage differences. Do not repair or reinterpret these in the reference path. Preserve ordinary non-metering service concerns (authentication, model routing, storage, concurrency, existing transport).

- [x] Pin failing regression cases against current implementation: tokenizer fallback, short prefixes, mixed TTL, native-vs-fallback output, JSON reasoning tags and 100% cache zero handling.
- [x] Copy tokenizer source and vocabulary unchanged in a separate reference namespace; keep source/license/hash provenance.
- [x] Vendor the reference cache builder/tracker/input counters and protocol normalization dependencies. Expose request input count, profile, compute, commit and usage types; account identifiers are stable Kiro IDs.
- [x] Vendor the reference upstream stream/JSON usage parser and image estimator without calling its request builder or identity prompt injection. Expose a metering-only adapter; response parsing must not submit extra model calls.
- [x] Connect active Sub2API requests to raw public request profiles and reference upstream metering, preserving streaming operation and bounded storage. Use reference rounding, minimum, TTL and zero fallback at response formatting boundaries.
- [ ] Run isolated reference-vs-adapter fixtures plus all local tests/vet and focused Linux race checks. Review source correspondence and failure behavior.
- [ ] Push, build a resource-limited Kiro candidate, test cold/warm/append/short/mixed-TTL and protocol behavior, then replace only Kiro and verify public endpoints plus unchanged other containers.

Validation commands: `go test ./internal/sub2apiref/... -count=1`, `go test ./proxy -run TestExactReference -count=1`, `go test ./... -count=1`, `go vet ./...`, and Linux `go test -race ./internal/sub2apiref/... ./proxy -run 'TestExactReference|TestSub2API' -count=1`.
