# 前缀读写缓存验证记录

2026-10-14，仅 Kiro。实现基于 626f387，用户批准设计见对应 spec。未修改 Sub2API、线上数据库、账号或生产容器；本地后台使用无账号的独立合成数据库。

## 结果

- `go test ./... -count=1`：通过，config 5.811s / proxy 41.487s。
- `go vet ./...`、`node --check web/app.js`、`git -c core.safecrlf=false diff --check`：通过。
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath`：通过。
- Linux Go 1.26.8 下 `go test -race ./config ./proxy -run "Test(Prefix|PromptCache|Cache|ClaudeWebSearch|WebSearch)" -count=1 -p 1`：通过，config 1.224s / proxy 13.273s。测试在仅挂载本次源码、限制 1 CPU / 2 GiB 的临时容器执行，生产数据未挂载。
- 独立只读审查：未发现重要缺陷；审查者独立运行 config/proxy 全量测试及 diff 检查。

## 关键行为

固定合成输入总量 10,200、可缓存前缀 10,000、参考读取 80%：

| 请求 | 普通输入 | 缓存读取 | 缓存写入 | 合计 |
|---|---:|---:|---:|---:|
| 冷请求 | 200 | 0 | 10,000 | 10,200 |
| 重复请求 | 200 | 8,307 | 1,693 | 10,200 |

40 个内容种子的读取比例为 75.13%–84.75%；同一输入稳定。95% / 100% 参考限定为 90%–95%，0% 只写，关闭模拟不读不写。部分命中、过期、账号隔离、5m/1h 分配、显式断点后的普通输入及原生输入缩放均通过。8 个 goroutine 同时计算、更新和缩放快照，合计与 TTL 明细守恒；后续更新不改变已发出的快照。

Claude、Chat Completions、Responses 均覆盖流式与非流式的冷/热请求；合成原生 input=10,000/output=137。输出不参与输入缓存拆分。

## 后台

本地后台初始为 prefix / 95%；全局改为 80% 保存并刷新后保留。合成 Key 独立保存 prefix / 70%，刷新后列表摘要及编辑器均为 70%，全局仍为 80%。中英文选项、独立字段禁用状态与旧模式选择正常。验证后关闭后台浏览器和本地测试进程。

## 上线边界

新配置或缺省 mode 默认 prefix。明确保存的 uniform / independent 保留，防止静默改变已存策略。用户线上全局和 Key 仍使用原模式；后续发布需明确切换为 prefix，同时保留已有参考比例、禁用状态和绑定。历史用量不重算，线上仍运行 626f387。

参考的是 linapi 样本的读写拆分表现，未获得其内部算法；此功能仍为本地模拟，长前缀依然可能产生几万 Token 的缓存量。
