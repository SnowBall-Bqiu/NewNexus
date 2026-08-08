# NewNexus

仓库地址：[github.com/SnowBall-Bqiu/NewNexus](https://github.com/SnowBall-Bqiu/NewNexus)

NewNexus 是一个 AGPL-3.0 的自托管网络调度控制台，统一管理 Cloudflare 优选、Cloudflare for SaaS、自定义 CNAME 线路、多地探测 Agent、DNS 发布、告警与审计。它不依赖 Cloudflare Load Balancing，也不充当权威 DNS；线路记录最终由 Cloudflare DNS、DNSPod 或阿里云 DNS 应答。

## 已实现能力

- Go 模块化控制面，REST `/api/v1`、SSE 实时状态及 `agent.v1` gRPC 双向流。
- SQLite 单实例模式与 PostgreSQL 生产模式，启动时自动迁移。
- Cloudflare、DNSPod、阿里云 DNS 连接校验、区域/线路同步、记录差异及发布适配器。
- Cloudflare for SaaS 区域、自定义主机名、TXT 预验证、证书状态和额度视图。
- Cloudflare 官方 IPv4/IPv6 网段同步，IPv6 有界抽样，候选分层与综合评分。
- 线路目标的优先级组和固定权重，健康目标摘除、整组故障后切到下一优先级；全故障保持原 DNS。
- DNS 发布预览、CNAME 环路和根域检查、发布快照、审计与显式回滚。
- 仅主动出站的 Linux/Docker Agent，注册码换取凭据、心跳、断线重连、任务背压和结果幂等。
- 中文 React 运维控制台，Base UI、Sonner、Virtuoso、Recharts 及受约束的 reduced-motion 动效。

## 本地开发

需要 Go 1.24+ 与 Node.js 22+。

```powershell
cd web
npm install
npm run build
cd ..
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
go install github.com/air-verse/air@v1.61.7
air -c .air.toml
```

打开 `http://127.0.0.1:8080`。开发认证模式会直接进入带演示数据的工作台。关闭 `NEWNEXUS_DEV_AUTH` 后，首次访问会要求创建唯一管理员。

开发时也可以分别运行后端和 Vite：

```powershell
# terminal 1
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
air -c .air.toml

# terminal 2
cd web
npm run dev
```

完整的架构、数据库、API、Agent 协议、适配器和测试说明见 [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md)。

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

生产环境应将 REST 与 gRPC 都置于 TLS 入口后。gRPC 入口必须支持 HTTP/2 长连接；Agent 只需出站访问该入口，不开放公网端口。示例见 `deploy/Caddyfile.example`。

预编译 Linux 包、systemd、备份恢复和升级步骤见 [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md)。

## Agent

先在“探测节点”中生成一次性注册码，然后在远端 Linux 或容器运行：

```bash
newnexus-agent \
  --server grpc.newnexus.example.com:443 \
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
| `NEWNEXUS_PUBLIC_URL` | `http://localhost:8080` | 控制台外部地址 |
| `NEWNEXUS_AGENT_PUBLIC_SERVER` | `localhost:9090` | 一键安装命令使用的公网 gRPC 地址 |
| `NEWNEXUS_AGENT_BINARY_PATH` | `newnexus-agent-linux-{arch}` | 主控提供下载的 Agent 路径模板；`{arch}` 会替换为 `amd64` 或 `arm64` |
| `NEWNEXUS_AGENT_INSTALL_INSECURE` | `false` | 仅本地开发的一键安装明文 gRPC 开关 |
| `NEWNEXUS_SECURE_COOKIES` | `false` | TLS 生产环境设为 `true` |
| `NEWNEXUS_PROBE_INTERVAL` | `30s` | 调度器评估周期 |
| `NEWNEXUS_SCHEDULER_ENABLED` | `true` | 是否在当前副本运行调度器 |
| `NEWNEXUS_VERSION` | `dev` | 运行版本标识 |

## 构建 Linux 发布包

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1 -Version 0.1.0
```

产物输出到 `dist/`，本地脚本生成 Linux amd64 控制端压缩包与 `SHA256SUMS`，包内同时携带 amd64/arm64 Agent。主控的“探测节点”页面可选择目标架构，并为每个一次性注册码生成可复制的一键安装命令。

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
npm run build
```

OpenAPI 文档由运行中的控制面提供：`GET /api/v1/openapi.yaml`。健康检查为 `/healthz`，数据库就绪检查为 `/readyz`。

## 安全边界

- 厂商凭据用主密钥派生的 AES-GCM 独立加密，API 仅返回掩码。
- 管理员密码使用 Argon2id；会话使用 HttpOnly Cookie、CSRF Token，可启用 TOTP。
- Agent 探测拒绝回环、私网、链路本地与元数据地址，并限制响应体与重定向次数。
- Cloudflare DNS 仅允许默认线路；运营商线路和权重取决于 DNSPod/阿里云账户能力。
- 所有目标不健康时不会清空记录。控制面停止自动变更并保留最后已知可用 DNS 状态。

## 许可

[AGPL-3.0](LICENSE)
