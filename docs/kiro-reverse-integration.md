# Kiro Proxy 2.1.0 custom integration

This fork merges tanu360/kiro-reverse-api at 9290b3565528677085ff75d174f996d120e81a6d with the deployed Kiro-Go ef60b42 history.

The new admin interface, SQLite, Responses endpoint, documents, backups, audit and stream completion fixes come from upstream. This fork retains account ownership by API key, global/per-key prompt cache policy, and independent creation/read ratios. In `prefix_total` mode, the ratio is the stable admission reference for cold prefixes; deterministic content variation can shift admission by up to five percentage points. An admitted cold prefix is written through its final cache breakpoint, a live hit reads the saved share, and an appended request writes only its new stable tail. The ratio is not a per-request token budget. A 100% setting admits every eligible cold prefix. Other cache modes retain their separate creation/read semantics. Output tokens remain independent.

## Existing deployments

Set DATA_DIR to the mounted data directory, for example /app/data. CONFIG_PATH=/app/data/config.json remains supported. Set both explicitly when using the upstream Dockerfile, because that image defaults DATA_DIR to /app/state.

On first startup with an empty SQLite configuration, config.json is imported transactionally into kiro.db. IDs, account states, bindings, credentials, key values and counters remain intact. Older model aliases are materialized if the JSON did not provide mappings. The original JSON is retained. Once the database contains configuration, it takes precedence over the JSON.

Back up the complete data directory before migration. Rollback requires the previous image and the final pre-cutover JSON copy; the older version cannot read SQLite changes made after deployment.

## Routing and cache

With API key authentication enabled, requests can select only accounts bound to their validated key. Session affinity, failover, web search and Responses follow the same ownership rule. Unbound accounts cannot serve authenticated requests. Turning authentication off uses the global pool.

Per-key cache fields inherit the corresponding global value when omitted. A key can restore all cache defaults with clearPromptCacheOverrides. Policy changes do not replace other omitted account or settings fields.

Cache matching uses cumulative content prefixes and is isolated by account, model, tool definitions and effective reasoning context. Claude tool-level cache_control survives request decoding. Five-minute and one-hour entries are supported; a shorter TTL cannot downgrade an existing live one-hour entry. Lookup does not renew cache state: only successful upstream completion stores or renews prefixes, so failed attempts cannot keep a prefix alive.

Chat Completions and Responses profiles include the current message as well as history. Complete repeated requests can reuse the full eligible input. When the current turn changes or history grows, only unchanged earlier prefixes are read and the new tail is counted as creation. Prefix weights are normalized into total input token space, then scaled at completion to native upstream input usage. Current user messages have the same semantic fingerprint shape when moved into history; routing/conversation identifiers are excluded, while tool results, images and documents participate in content matching.

The dispatch snapshot fixes the cache hit/miss decision and effective admission ratio for the entire request, including concurrent requests. In `prefix_total`, input beyond the final cache breakpoint and any cache-admission miss remain ordinary input. Claude ordinary input plus cache creation plus cache reads equals total input. OpenAI cache details are subsets of inclusive input/prompt totals. Output tokens do not participate in input cache policy. These are in-memory local usage estimates, not upstream cache telemetry; restarting the service clears the simulated cache.

## Production scope

The service keeps the kiro-go container name, existing Docker networks, aliases, localhost port and data mount. The release process tests a copied data directory on a separate loopback port before replacing only Kiro. It does not require changes to Sub2API, Nginx, DNS, firewall or other containers.

