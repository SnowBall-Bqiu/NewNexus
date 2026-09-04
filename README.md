# NewNexus

仓库地址：[github.com/SnowBall-Bqiu/NewNexus](https://github.com/SnowBall-Bqiu/NewNexus)

NewNexus 是一个 AGPL-3.0 的自托管网络调度控制台，统一管理 Cloudflare 优选、Cloudflare for SaaS、自定义 CNAME 线路、多地探测 Agent、DNS 发布、告警与审计。它不依赖 Cloudflare Load Balancing，也不充当权威 DNS；线路记录最终由 Cloudflare DNS、DNSPod 或阿里云 DNS 应答。

## 已实现能力

- Go 模块化控制面，REST `/api/v1`、SSE 实时状态及 `agent.v1` gRPC 双向流。
- SQLite 单实例租约与 PostgreSQL 多工作器模式，版本化迁移兼容 rc.2 数据。
- 带租约、重试、幂等键、取消与进度事件的异步作业中心；DNS 与 SaaS 外部写操作返回 `202 + job_id`。
- Cloudflare、DNSPod、阿里云 DNS 连接校验、区域/线路同步、记录差异及发布适配器。
- Cloudflare for SaaS 区域、回退源、配额、TXT/HTTP 预验证、证书状态、批量清理和零停机切流门槛。
- Cloudflare 官方 IPv4/IPv6 网段同步，IPv6 有界抽样，候选分层与综合评分。
- 线路目标的优先级组和固定权重，健康目标摘除、整组故障后切到下一优先级；全故障保持原 DNS。
- DNS 发布预览、CNAME 环路和根域检查、发布快照、审计与显式回滚。
- 仅主动出站的 Linux/Docker Agent，15 分钟一次性注册码、凭据轮换/撤销、任务 ACK/租约、bbolt 断线 outbox 和结果幂等。
- Argon2id、可选 TOTP、一次性恢复码、会话撤销、API Key、CSRF、CSP/HSTS 与加密通知密钥。
- 中文 React 运维控制台，采用紧凑 Kumo 风格，支持项目名称/简称/识别色自定义，以及浅色、深色、黑白高对比和跟随系统四种主题模式。
- 控制台实际接入 `@cloudflare/kumo@2.9.2`，使用 Kumo Sidebar、Dialog、Table、Select、Toasty、Meter 与 TimeseriesChart；大候选列表保留 Virtuoso 虚拟化，并为所有必要动效提供 reduced-motion 分支。

## 本地开发

需要 Go 1.24+ 与 Node.js 22+。

```powershell
cd web
npm install
npm run build
cd ..
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
$env:NEWNEXUS_HTTP_ADDR='127.0.0.1:18080'
$env:NEWNEXUS_GRPC_ADDR='127.0.0.1:19090'
$env:NEWNEXUS_PUBLIC_URL='http://127.0.0.1:18080'
$env:NEWNEXUS_AGENT_PUBLIC_SERVER='127.0.0.1:19090'
go install github.com/air-verse/air@v1.61.7
air -c .air.toml
```

打开 `http://127.0.0.1:18080`。开发认证模式会直接进入带演示数据的工作台。关闭 `NEWNEXUS_DEV_AUTH` 后，首次访问会要求创建唯一管理员。

开发时也可以分别运行后端和 Vite：

```powershell
# terminal 1
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
$env:NEWNEXUS_HTTP_ADDR='127.0.0.1:18080'
$env:NEWNEXUS_GRPC_ADDR='127.0.0.1:19090'
air -c .air.toml

# terminal 2
cd web
$env:VITE_API_PROXY_TARGET='http://127.0.0.1:18080'
npm run dev
```

完整开发说明见 [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md)，接口契约见 [`docs/API_GRPC.md`](docs/API_GRPC.md)，Agent 自动部署见 [`docs/AGENT.md`](docs/AGENT.md)。

## Docker Compose

从 `.env.example` 创建 `.env`，必须设置不可丢失的 `NEWNEXUS_MASTER_KEY`。更换该密钥后，已有厂商凭据将无法解密。

仓库根目录提供两个 Compose 文件：`compose.yaml` 用于从源码构建和本地开发；`docker-compose.yaml` 使用已发布的 GHCR 多架构镜像，并为每个配置行提供中文注释。直接部署镜像时执行：

```bash
cp .env.example .env
# 编辑 .env，至少设置 NEWNEXUS_MASTER_KEY 和 POSTGRES_PASSWORD
docker compose -f docker-compose.yaml pull
docker compose -f docker-compose.yaml up -d postgres control-plane
```

```bash
# SQLite 单机
docker compose --profile sqlite up -d --build

# PostgreSQL
docker compose --profile postgres up -d --build

# SQLite 加本机 Agent
docker compose --profile sqlite --profile agent up -d --build
```

生产环境的 gRPC 默认由控制端直接加载证书并提供 TLS HTTP/2；REST 可继续放在 Caddy 等反向代理后。只有明文监听明确绑定到回环或私网 IP 时，才允许显式开启 h2c。Agent 只需出站访问 gRPC 入口，不开放公网端口。

预编译 Linux 包与 systemd 见 [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md)，备份恢复见 [`docs/BACKUP_RESTORE.md`](docs/BACKUP_RESTORE.md)，升级回退见 [`docs/UPGRADE.md`](docs/UPGRADE.md)，故障排查见 [`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md)。

## Agent

先在“探测节点”中生成一次性注册码，然后在远端 Linux 或容器运行：

```bash
newnexus-agent \
  --server grpc.newnexus.example.com:9090 \
  --enroll-code ONE_TIME_CODE \
  --name hk-edge-01 \
  --state /var/lib/newnexus-agent/state.json
```

本地无 TLS 的开发控制面需要额外传 `--insecure`。生产环境不要使用该参数。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `NEWNEXUS_DATABASE_DRIVER` | `sqlite` | `sqlite` 或 `postgres` |
| `NEWNEXUS_DATABASE_URL` | `newnexus.db` | SQLite 路径或 PostgreSQL DSN |
| `NEWNEXUS_MASTER_KEY` | 开发派生值 | 生产必须设置的 32 字节 Base64 密钥 |
| `NEWNEXUS_HTTP_ADDR` | `:8080` | REST、SSE 与前端监听地址 |
| `NEWNEXUS_GRPC_ADDR` | `:9090` | Agent gRPC 监听地址 |
| `NEWNEXUS_GRPC_TLS_CERT_FILE` | 空 | 生产 gRPC 叶证书链文件；必须与私钥同时设置 |
| `NEWNEXUS_GRPC_TLS_KEY_FILE` | 空 | 生产 gRPC 私钥文件；必须与证书同时设置 |
| `NEWNEXUS_GRPC_ALLOW_H2C` | 开发模式 `true` | 仅回环或字面量私网地址可显式开启的明文 HTTP/2 开关 |
| `NEWNEXUS_PUBLIC_URL` | `http://localhost:8080` | 控制台外部地址 |
| `NEWNEXUS_AGENT_PUBLIC_SERVER` | `localhost:9090` | 一键安装命令使用的公网 gRPC 地址 |
| `NEWNEXUS_AGENT_BINARY_PATH` | `newnexus-agent-linux-{arch}` | 主控提供下载的 Agent 路径模板；`{arch}` 会替换为 `amd64` 或 `arm64` |
| `NEWNEXUS_AGENT_INSTALL_INSECURE` | `false` | 仅本地开发的一键安装明文 gRPC 开关 |
| `NEWNEXUS_AGENT_TLS_SHA256` | 自动计算 | 可选覆盖公网 gRPC 叶证书 SHA-256 指纹；控制端默认从证书计算 |
| `NEWNEXUS_PROVIDER_MAX_RETRIES` | `3` | 厂商 API 临时错误最大退避重试次数 |
| `NEWNEXUS_PROVIDER_MIN_INTERVAL` | `0` | 每个厂商适配器的最小请求间隔，例如 `100ms` |
| `NEWNEXUS_SECURE_COOKIES` | `false` | TLS 生产环境设为 `true` |
| `NEWNEXUS_ENABLE_HSTS` | 按 Public URL | 全站 HTTPS 后启用 HSTS |
| `NEWNEXUS_TRUSTED_PROXY_CIDRS` | 空 | 允许解析转发 IP 头的可信代理 CIDR |
| `NEWNEXUS_PROBE_INTERVAL` | `30s` | 调度器评估周期 |
| `NEWNEXUS_INVENTORY_INTERVAL` | `6h` | 库存发现探测周期 |
| `NEWNEXUS_RANKING_INTERVAL` | `5m` | 榜单复测周期 |
| `NEWNEXUS_ACTIVE_INTERVAL` | `30s` | 活动目标健康检查周期 |
| `NEWNEXUS_JOB_WORKERS` | SQLite 1 / PostgreSQL 4 | 异步作业工作器数量 |
| `NEWNEXUS_SCHEDULER_ENABLED` | `true` | 是否在当前副本运行调度器 |
| `NEWNEXUS_VERSION` | `dev` | 运行版本标识 |

## 构建 Linux 发布包

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1 -Version 0.1.0
```

产物输出到 `dist/`，本地脚本生成 Linux amd64 控制端、amd64 Agent 压缩包与 `SHA256SUMS`。主控的“探测节点”页面会为一次性注册码生成可复制的一键安装命令；GitHub Release CI 另外生成 arm64 对应包和镜像。

## 自动发布

在 GitHub 创建并发布 Release 后，`.github/workflows/release.yml` 会从该 Release 的标签构建并上传：

- `newnexus-v<version>-linux-amd64.tar.gz`
- `newnexus-v<version>-linux-arm64.tar.gz`
- `SHA256SUMS`

同一工作流还会把 `linux/amd64`、`linux/arm64` 多架构的控制端与 Agent 镜像推送到 GitHub Container Registry：

```bash
docker pull ghcr.io/snowball-bqiu/newnexus:v0.1.0
docker pull ghcr.io/snowball-bqiu/newnexus-agent:v0.1.0
```

正式 Release 会更新 `latest`；标记为 prerelease 的版本只发布版本标签，不覆盖 `latest`。

## 验证

```powershell
go test ./cmd/... ./internal/...
cd web
npm run lint
npm run test
npm run build
```

OpenAPI 文档由运行中的控制面提供：`GET /api/v1/openapi.yaml`。健康检查为 `/healthz`，数据库就绪检查为 `/readyz`。

## 安全边界

- 厂商凭据用主密钥派生的 AES-GCM 独立加密，API 仅返回掩码。
- 管理员密码使用 Argon2id；浏览器会话使用 HttpOnly Cookie 与 CSRF，API Key 独立认证；可启用 TOTP 与一次性恢复码。
- Agent 探测拒绝回环、私网、链路本地与元数据地址，并限制响应体与重定向次数。
- Cloudflare DNS 仅允许默认线路；运营商线路和权重取决于 DNSPod/阿里云账户能力。
- 所有目标不健康时不会清空记录。控制面停止自动变更并保留最后已知可用 DNS 状态。

## 许可

[AGPL-3.0](LICENSE)
