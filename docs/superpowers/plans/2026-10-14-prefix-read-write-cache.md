# 前缀读写缓存 Implementation Plan

> **For agentic workers:** 使用 superpowers:executing-plans 在当前已隔离 worktree 中顺序执行，完成后使用 requesting-code-review 进行独立只读审查。

**Goal:** 按用户批准的 linapi 参考拆分新增 prefix 默认模式，保持真实内容计数及多 Key 控制，不修改 Sub2API。

**Architecture:** tracker 根据实际匹配前缀与 Key 参考比例划分读取，剩余可缓存前缀作为写入；完成时缩放发出时快照，保证总输入与 TTL 守恒。config 保留旧模式并新增默认模式，后台同步展示新语义。

**Tech Stack:** Go、离线 o200k_base、原生 HTML/JavaScript、中英文 JSON locales。

## 任务与检查

- [x] 只读参考样本及现有配置，用户批准 prefix 设计。
- [x] 检查现有 linked worktree、干净 Git 状态，执行完整基线测试。
- [ ] 新增 `proxy/cache_prefix_mode_test.go`：冷/热/部分/显式断点/比例 0 与关闭/TTL/原生缩放/三种协议 JSON+SSE；新增 `config/prefix_cache_test.go`：新默认与旧策略保留、每 Key 保存重载。
- [ ] 执行 `go test ./config ./proxy -run 'TestPrefix' -count=1`，确认因为模式未支持及语义断点计数不一致而失败。
- [ ] 修改 `config/config.go`、`config/prompt_cache.go`、`config/apikeys.go` 支持 prefix 默认与验证，每 Key 的 Ratio 用作 prefix 读取参考。
- [ ] 修改 `proxy/cache_tracker.go`，增加 prefix snapshot 字段与派生读写函数，计算 C−R 与 TTL 拆分；旧模式保持原公式。
- [ ] 修改 `proxy/token_estimator.go`、profile 内容计数，复用语义工具/消息计数器，将指纹 JSON 与 Token 分离。
- [ ] 执行上述 targeted tests，修正实现直到通过；完整旧模式回归保持。
- [ ] 修改 `web/index.html`、`web/app.js`、`web/locales/zh.json`、`web/locales/en.json`、README 中英文，更新新模式、禁用字段与摘要。
- [ ] 在独立本地 Kiro 实例检查后台默认模式、Key 独立比例保存、旧模式可选及新模式重载，测试仅使用合成数据。
- [ ] 执行 `go test ./... -count=1`、`go vet ./...`、`git diff --check`、Linux CGO=0 构建与新增 tracker 并发 race。
- [ ] 独立代码审查，处理重要问题，记录最终验证并提交，报告本地完成与线上状态。

## 回归场景

固定 profile T=10,200/C=10,000、缓存 TTL 为 1h、参考 80%：冷输入普通 200/写入 10,000/读取 0；热读取 7,500–8,500/写入剩余/普通仍 200。原生 T=20,400 时普通为 400、缓存前缀 20,000，三项之和 20,400。参考 95% 的读取区间 90%–95%，不能升到 99%；0% 仍写可缓存前缀，disabled=true 才全部普通。

协议测试使用固定合成原生 input=10,000/output=137，三接口×流/非流×冷/热；profile 无显式断点时覆盖全部语义输入，冷热均 read+write=10,000，warm read≤9,500、write>0。

## 发布边界

本轮不隐式覆盖生产的明确旧模式。后续 Kiro 发布使用既有候选实例、完整备份、独立镜像及回滚流程；候选配置应将全局和当前 Key 的 mode 切为 prefix、保持旧读取参考值。未获本次发布指令之前不操作线上配置或重启容器。Sub2API 始终只读。
