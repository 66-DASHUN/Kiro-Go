# Kiro Proxy 2.1.0 custom integration

This fork merges tanu360/kiro-reverse-api at 9290b3565528677085ff75d174f996d120e81a6d with the deployed Kiro-Go ef60b42 history.

The new admin interface, SQLite, Responses endpoint, documents, backups, audit and stream completion fixes come from upstream. This fork retains account ownership by API key, global/per-key prompt cache policy, and independent creation/read ratios. Cache policy values are references for local usage emulation: eligible creation/read portions vary deterministically with request input by up to five percentage points, without a fixed 90 percent cap. Endpoint settings of 0 and 100 percent remain exact. Partial-prefix hits and cold requests can have an overall read rate below that reference. Output tokens remain independent.

## Existing deployments

Set DATA_DIR to the mounted data directory, for example /app/data. CONFIG_PATH=/app/data/config.json remains supported. Set both explicitly when using the upstream Dockerfile, because that image defaults DATA_DIR to /app/state.

On first startup with an empty SQLite configuration, config.json is imported transactionally into kiro.db. IDs, account states, bindings, credentials, key values and counters remain intact. Older model aliases are materialized if the JSON did not provide mappings. The original JSON is retained. Once the database contains configuration, it takes precedence over the JSON.

Back up the complete data directory before migration. Rollback requires the previous image and the final pre-cutover JSON copy; the older version cannot read SQLite changes made after deployment.

## Routing and cache

With API key authentication enabled, requests can select only accounts bound to their validated key. Session affinity, failover, web search and Responses follow the same ownership rule. Unbound accounts cannot serve authenticated requests. Turning authentication off uses the global pool.

Per-key cache fields inherit the corresponding global value when omitted. A key can restore all cache defaults with clearPromptCacheOverrides. Policy changes do not replace other omitted account or settings fields.

## Production scope

The service keeps the kiro-go container name, existing Docker networks, aliases, localhost port and data mount. The release process tests a copied data directory on a separate loopback port before replacing only Kiro. It does not require changes to Sub2API, Nginx, DNS, firewall or other containers.

