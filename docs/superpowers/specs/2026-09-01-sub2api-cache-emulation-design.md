# Sub2API Cache Emulation Adaptation Design

## Goal

将 Sub2API 的 Kiro Prompt Cache 模拟规则适配到 Kiro-Go，移除现有固定 90% 上限，并支持 API Key 级缓存模拟配置。

## Scope

- 适用于 Claude `/v1/messages`、OpenAI Chat Completions 和 OpenAI Responses 三个入口。
- 缓存按 Kiro 账号 ID 隔离，缓存条目只保存在进程内存中，服务重启后清空。
- 默认缓存 TTL 为 5 分钟，显式 `ttl: "1h"` 时使用 1 小时，最大 TTL 为 1 小时。
- 普通模型最低缓存门槛为 1024 tokens，模型名包含 `opus` 时为 4096 tokens。
- 没有显式 `cache_control` 的请求使用默认缓存断点；有显式断点时不额外补断点。
- 缓存前缀使用规范化 JSON 和 SHA-256 链式指纹，忽略缓存控制字段及结构位置字段。
- 命中从最近断点向前最多回看 10 个断点；命中条目续期。
- 断点累计 Token 归一化到请求总输入 Token 空间，避免工具和 JSON 结构估算口径不一致。

## Configuration

Global configuration supplies defaults:

- `promptCacheEmulationEnabled` defaults to `true`.
- `promptCacheEmulationRatio` defaults to `1.0`.
- `promptCacheEmulationMode` defaults to `uniform`.
- `promptCacheCreationEmulationRatio` and `promptCacheReadEmulationRatio` default to `1.0`.

Each API key can override the same fields. Missing API-key values inherit the global defaults. Ratios are validated and clamped to `[0, 1]`; there is no 90% cap. In `uniform` mode the common ratio scales both cache reads and cache creation. In `independent` mode the two ratios are applied separately.

The existing JSON configuration remains backward compatible. Existing API keys receive inherited defaults without requiring a migration command.

## Data Flow

1. Parse the client request and estimate total input tokens before translation.
2. Build a protocol-specific cache profile containing canonical prefix fingerprints, cumulative token counts, breakpoints, and TTLs.
3. Resolve effective cache settings from the API key and global defaults.
4. Compute the longest recent matching prefix for the selected account.
5. Scale cache-read and cache-creation token counts using the effective ratios, then derive uncached input tokens as the non-negative remainder.
6. Emit protocol-specific usage fields, including cache creation TTL breakdown.
7. After a successful upstream response, commit all cacheable breakpoints to the in-memory tracker.
8. Add cache token counters to persisted runtime statistics and expose them through existing admin statistics payloads where supported.

## Compatibility and Non-goals

- Do not copy Sub2API source files or its Group/database types; implement equivalent behavior using Kiro-Go types.
- Do not persist prompt-cache entries to disk or Redis in this change.
- Do not alter upstream Kiro request authentication, account scheduling, or response translation semantics.
- Do not change the existing approximate tokenizer globally; only cache-profile normalization may use total-input scaling.
- Automatic breakpoints are limited to Kiro cache emulation and must not affect non-Kiro request paths.

## Testing

Add tests for cold creation, repeated prefix reads, account isolation, 5-minute and 1-hour TTL behavior, expiry, minimum token thresholds, automatic breakpoints, ten-breakpoint lookback, ratio and independent-ratio scaling, API-key override/inheritance, and usage mapping for all three protocol paths. Run the focused proxy tests, the full Go test suite, and `go build ./...` before completion.
