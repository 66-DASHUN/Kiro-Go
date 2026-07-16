# Kiro-Go 独立 HTTPS 与 Sub2API 网络集成部署设计

日期：2026-07-16

## 1. 目标

将 `gwenliu1025/Kiro-Go` 当前 `main` 提交
`0773d57ae919ee5715905a33084d77e0adc1d257` 部署到
`47.253.211.154`，通过 `https://kiro.dashun9527.com` 提供服务，同时不重建、
不重启、不改写现有 Sub2API、PostgreSQL、Redis、Nginx 其他站点和其他容器。

Kiro-Go 需要加入现有外部 Docker 网络 `sub2api_sub2api-network`，使 Sub2API
未来可通过 Docker DNS 地址 `http://kiro-go-pr131:8321` 访问它。本次只建立
网络可达性，不修改 Sub2API 的上游地址或数据库配置。

## 2. 已确认环境

- 系统：Alibaba Cloud Linux 3，`x86_64`。
- Docker：`26.1.3`；Docker Compose：`v2.27.0`。
- 主机资源：2 vCPU、1.8 GiB 内存、1 GiB Swap、根分区剩余约 21 GiB。
- 当前可用内存约 544 MiB，不适合在生产主机执行 Go 多阶段镜像构建。
- `127.0.0.1:8321` 未占用。
- `sub2api_sub2api-network` 已存在，当前连接 Sub2API、PostgreSQL、Redis 和
  `gpt-image-playground`。
- Nginx 由宝塔管理，监听公网 `80/443`；现有站点均使用独立配置和独立证书。
- `kiro.dashun9527.com` 已启用 Cloudflare 代理，但源站尚无该域名的独立站点
  和证书。
- GHCR 已发布可匿名拉取的 `linux/amd64` 镜像，目标摘要为
  `sha256:66f5258ea7891e0a03b949ee6f47a6ec21a6655a21fb17c30ce2ee66cd999def`。

## 3. 部署架构

```text
公网客户端
  -> Cloudflare HTTPS
  -> 源站 Nginx HTTPS（kiro.dashun9527.com）
  -> 127.0.0.1:8321
  -> Kiro-Go 容器:8321

Sub2API 容器
  -> sub2api_sub2api-network
  -> kiro-go-pr131:8321
  -> Kiro-Go 容器:8321
```

Kiro-Go Compose 项目使用自己的默认网络，并同时加入外部
`sub2api_sub2api-network`。外部网络上的固定别名为 `kiro-go-pr131`。容器不
发布 `0.0.0.0:8321`，宿主机只绑定 `127.0.0.1:8321`。

## 4. 文件与运行边界

所有新增运行资产放在 `/opt/kiro-go`：

- `/opt/kiro-go/compose.yml`：仅定义 Kiro-Go 服务和网络。
- `/opt/kiro-go/.env`：管理密码等运行变量，权限 `0600`。
- `/opt/kiro-go/data/`：持久化 `/app/data`，目录权限限制为 root 可写。
- `/opt/kiro-go/acme-webroot/`：ACME HTTP-01 验证目录。
- `/opt/kiro-go/letsencrypt/`：该域名的证书与续期状态。

容器名为 `kiro-go`，重启策略为 `unless-stopped`，配置健康检查。镜像使用摘要
固定，不使用可漂移的 `latest` 作为最终运行引用，也不在生产主机执行源码构建。

## 5. 认证与密钥

Kiro-Go 默认管理密码为 `changeme`，且默认 API Key 开关关闭，不能直接暴露
公网。部署流程必须按以下顺序执行：

1. 使用密码学安全随机值生成管理密码，写入权限为 `0600` 的 `.env`。
2. 仅启动回环端口，不创建公网 Nginx 站点。
3. 通过 `127.0.0.1:8321` 的管理 API 创建一条随机 API Key。
4. 通过管理 API 启用 `requireApiKey=true`。
5. 验证无 Key 请求返回 `401`，正确 Key 请求能通过认证边界。
6. 只有上述检查通过后才启用公网 Nginx 站点。

管理密码和 API Key 只在最终交付中向用户显示一次，不写入日志、设计文档、
Nginx 配置或命令历史。Kiro 上游账号不在本次部署范围内；没有导入账号时，
模型请求可能返回无可用账号，这不代表部署失败。

## 6. HTTPS 与 Nginx

为 `kiro.dashun9527.com` 新建独立宝塔风格 Nginx 配置，不修改其他域名文件。
配置分两阶段启用：

1. HTTP 阶段仅提供 `/.well-known/acme-challenge/`，用于申请证书。
2. 证书成功后启用 `80 -> 443` 跳转及 HTTPS 反向代理到
   `http://127.0.0.1:8321`。

代理配置保留流式响应语义：禁用代理缓冲和请求缓冲，设置足够的读写超时，
转发 `Host`、`X-Forwarded-*`、真实客户端地址以及连接升级头。每次写入后必须
先执行 `nginx -t`；只有语法检查成功才平滑 reload，不执行 Nginx restart。

源站证书使用独立 Certbot 容器按 webroot 模式申请，证书和续期数据只挂载到
`/opt/kiro-go`。新增独立 systemd oneshot/timer 定期执行续期，并仅在成功续期
后平滑 reload Nginx。该流程不安装全局 Python 包，不占用新的常驻端口。

## 7. 资源与隔离控制

- 不在服务器构建镜像，避免低内存环境影响现有服务。
- 只拉取固定摘要镜像并启动一个 Kiro-Go 容器。
- 不执行 `docker compose up` 于 Sub2API 项目目录。
- 不修改 Sub2API Compose、数据库、Redis、Nginx 其他站点或宝塔面板配置。
- 不开放新的安全组端口；公网继续只使用既有 `80/443`。
- 加入共享网络只提供网络可达性，不授予数据库凭据，也不自动更改业务流量。

## 8. 验证标准

部署前记录全部现有容器的镜像、状态和 `StartedAt`。部署后必须验证：

- `kiro-go` 为 `running/healthy`。
- `http://127.0.0.1:8321/health` 返回 `200`。
- 从 `sub2api` 容器解析 `kiro-go-pr131` 成功，并访问其 `/health` 返回 `200`。
- `ss` 显示 `8321` 只绑定 `127.0.0.1`，没有绑定 `0.0.0.0` 或 `::`。
- 未带 API Key 的模型接口返回 `401`。
- `https://kiro.dashun9527.com/health` 和管理页面通过 Cloudflare 返回预期响应。
- 源站证书域名匹配、有效期正常，`nginx -t` 通过。
- 部署前已存在容器的 `StartedAt` 全部不变，状态仍正常。
- Nginx 主进程只发生 reload，不发生服务中断式 restart。

## 9. 失败处理与回滚

任何阶段失败时停止在当前阶段，不继续扩大暴露面，并说明失败位置：

- 镜像拉取或健康检查失败：不创建公网 Nginx 配置，删除 Kiro-Go 容器即可。
- API Key 强制认证失败：保持仅回环访问，不启用公网域名。
- 证书申请失败：保留 HTTP 验证站点或恢复到部署前无专属站点状态，不启用
  错误证书。
- `nginx -t` 失败：不 reload，恢复新增配置的备份或删除新配置。
- 公网验收失败：先禁用新域名配置，不操作其他站点。

完整回滚只涉及本次新增资产：停止并删除 `kiro-go` 容器、删除其 Compose 网络
连接、删除 `kiro.dashun9527.com` 的新增 Nginx 配置、证书续期 timer 和
`/opt/kiro-go`。回滚前保存 Kiro-Go 数据目录归档；不删除外部
`sub2api_sub2api-network`，不重启 Sub2API 或其他容器。

## 10. 非目标

- 不导入或迁移 Kiro/AWS 账号凭据。
- 不修改 Sub2API 上游地址、账户记录或数据库。
- 不开放公网 TCP `8321`。
- 不更改 Cloudflare DNS、WAF 或 SSL 模式。
- 不升级 Docker、Nginx、宝塔、Sub2API 或其他现有服务。
