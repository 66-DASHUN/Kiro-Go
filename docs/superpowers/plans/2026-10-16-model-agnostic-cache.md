# 通用模型缓存模拟 Implementation Plan

> Task 1–2 的通用模型准入目标已完成。此计划中 `prefix-total-v4` 的行为描述属于历史记录；当前总缓存规则为 `prefix-total-v7`，以 [前缀准入设计](../specs/2026-10-18-cache-prefix-admission-design.md) 和 [本地验收](../reviews/2026-10-18-cache-prefix-admission-verification.md) 为准。

> **For agentic workers:** Execute inline with superpowers:executing-plans. Request an independent reviewer before merge; do not delegate implementation.

**Goal:** 使兼容现有接口的新模型无需新增缓存白名单即可按有效 Key 策略参与模拟。

**Architecture:** 保持现有动态模型发现与权限路由，移除 tracker 中模型家族准入。缓存只在成功生成后提交；Claude 已有别名保持归一，其他标识保留标点以防误共享。

**Tech Stack:** Go、httptest、SQLite、Docker、Python 运维脚本。

---

### Task 1: 先复现新家族及身份问题

- [x] 修改 `proxy/cache_model_family_test.go`：旧“拒绝所有未知名称”仅保留空标识拒绝，未知但成功生成的名称纳入新测试。
- [x] 新建 `proxy/cache_generic_models_test.go`：真实 profile 构建覆盖 `gpt-5.6-sol`、`qwen3-coder-next`、`deepseek-3.2`、`minimax-m2.5`、`glm-5`、`kimi-k3`、`future/new-family.v1`。短输入首次创建、重复读取、追加后旧份额不变；非 Claude 点横杠不能共享。
- [x] 新增三协议 JSON/SSE 端到端成功和失败测试；显式别名映射到未来模型，验证上游收到实际模型，Key 0% 覆盖全局，未知模型上游失败时没有条目。
- [x] 运行 `go test ./proxy -run 'TestGenericCache' -count=1`，确认因 `unsupported_model` 和标点指纹冲突失败，保存红灯日志。

### Task 2: 最小通用化及诊断

- [x] `proxy/cache_model.go` 的 `totalCacheMinTokens` 改为 `if strings.TrimSpace(model)=="" { return math.MaxInt }; return 1`。家族正则仅用于 Claude 的已知别名归一，不再控制资格；非 Claude 直接返回小写/trim 后原标识。
- [x] `proxy/cache_total_rate.go` 升级规则 `prefix-total-v4`，空标识用 `empty_model`，更新阻断分支；不改份额、TTL、成功提交路径。
- [x] 更新 `proxy/cache_total_rate_test.go` 中版本和空标识断言；`web/locales/{zh,en}.json` 添加空标识原因并解释通用模拟。
- [x] 如三协议测试发现缓存身份与实际 payload 不一致，只修复该身份来源，不改配置映射优先级。
- [x] 运行定向测试及 `go test ./... -json`、`go vet ./...`、`go build ./...`、`git diff --check` 和 locale JSON 解析。

### Task 3: 审查、验证与部署

- [x] 记录 v7 规则、边界、红绿回归和未完成的 CCTest 验证，见 2026-10-18 前缀准入设计与验收报告。
- [x] 完成独立审查；修复同一候选前缀被尾部变化影响和正比例时旧缓存读取被零浮动阈值挡住的问题，并复测。
- [x] 本地全量 Go 测试、vet、build、locale 键一致性与 diff 检查通过。
- [ ] 提交并 fetch 核对远端历史；仅快进推送 integration/main，不强推。
- [ ] 使用独立发布目录，执行 Linux 测试/vet/race/build、候选副本测试、短输入冷写热读和原生协议检查。
- [ ] 仅切换 Kiro；验证公网健康、SQLite/API Key/绑定/缓存配置保持，并比对其他容器基线；保存备份、回滚点和发布报告。无 CCTest 原始报告时不声称 CCTest 通过。
