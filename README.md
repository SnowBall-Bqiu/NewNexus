# NewNexus

单管理员、自托管的 Cloudflare 优选、Cloudflare for SaaS 与多 CDN CNAME 线路调度平台。Go 控制面与 Linux 探测 Agent，React / TypeScript / Vite 界面，采用本地维护的 shadcn/ui Base UI 组件。项目许可证为 AGPL-3.0。

当前工作树为从零重写版本；首次启动没有示例资源和伪造运行数据。

## 功能

- Cloudflare 官方 IPv4/IPv6、手工 IP/CIDR、CSV/JSON、DNS 和 HTTPS 候选源；IPv6 稳定抽样；库存、榜单、活动目标三级探测。
- TCP/TLS/HTTP、业务 Host 与 SNI、请求模板、状态码与内容验证、p50/p95、下载速度、资格过滤和可配置评分。
- 项目与站点、节点组、线路健康视角；优先级组与固定权重；仲裁、迟滞、恢复、性能门槛、冷却和全故障保留 DNS。
- Cloudflare、DNSPod、阿里云 DNS 的真实适配器；区域、线路与套餐能力同步；逐条发布差异、不可变快照、API/权威 DNS 验证和失败补偿。
- SaaS 区域、回退源、额度、自定义主机名、TXT/HTTP 预验证、独立源站、批量任务、受管 DNS 自动化和迁移发布。
- 一次性节点注册、保留身份的重新部署、节点删除、TLS HTTP/2 双向流、凭据轮换与撤销、持久任务/结果队列、背压、断线重放和幂等确认。
- 唯一管理员、Argon2id、TOTP/恢复码、安全会话、API Key、CSRF、AEAD 凭据加密、SSRF 防护、HMAC Webhook 与 SMTP。
- 真实作业、告警、审计、SSE；浅色/深色/系统主题、移动抽屉、键盘操作、紧凑表格和候选虚拟渲染。

功能实现和验收证据分别记录在 [验收清单](docs/ACCEPTANCE.md)。真实云账户、生产 DNS 与目标规模压力测试必须以该文档中的实际结果为准。

SaaS 主机名所有权与证书验证必须分别完成；O2O 预验证受 Cloudflare 限制。操作顺序、接管现有 DNS 和回滚说明见 [SaaS 迁移](docs/SAAS_MIGRATION.md)。

## 本地开发

需要 Go 1.27.3、Node.js 22、npm。首次安装依赖：

```sh
go mod download
cd web
npm ci --registry=https://registry.npmjs.org
npm run build
```

在仓库根启动控制面（以下示例为 POSIX shell，PowerShell 使用 `$env:变量='值'`）：

```sh
NEWNEXUS_ALLOW_INSECURE_AGENT=1 NEWNEXUS_COOKIE_SECURE=0 go run ./cmd/server
```

打开 `http://127.0.0.1:8080` 创建唯一管理员。需要热更新时，在 `web` 运行 `npm run dev`，Vite 把 `/api` 代理到 8080。明文 gRPC 与非安全 Cookie 仅在显式启用且绑定回环的开发模式下可用。开发环境会在数据目录创建仅本机使用的随机主密钥。

## Docker 部署

1. 复制 `.env.example` 为 `.env`，生成独立的 32 字节 base64 主密钥，例如 `openssl rand -base64 32`。
2. 配置 TLS：把完整证书链及私钥放入外部目录，设置 `NEWNEXUS_TLS_DIR`；文件名为 `fullchain.pem` / `privkey.pem`。UID/GID 65532 必须可读，详情见 [TLS](deploy/tls/README.md)。
3. 二选一启动数据库 profile。Compose 配置解析要求主密钥与数据库密码均非空，即使使用 SQLite 也为 `MYSQL_PASSWORD` 设置随机值。此 Compose 示例直接在 URL 中引用密码，请使用 `openssl rand -hex 32` 生成的随机十六进制值。MySQL 管理员密码由镜像随机生成，控制面仅使用 `newnexus` 库内的应用账户；数据库端口不映射到宿主机。

```sh
# 轻量模式：唯一控制面实例
docker compose --profile sqlite up -d --build
# 或 MySQL 8.4 LTS 模式
docker compose --profile mysql up -d --build
```

HTTPS 默认映射到 8443，Agent gRPC TLS 映射到 9443。REST 与 gRPC 都使用证书；反向代理必须支持 HTTP/2 gRPC 长连接和 SSE，参考 [Caddy 配置](deploy/Caddyfile.example)。不要同时启用 sqlite 与 mysql profile。迁移会在服务启动时执行，也可以先执行 `newnexus-server --migrate`。

`NEWNEXUS_TRUSTED_PROXIES` 未设置、留空或仅含空白时，默认信任回环地址 `127.0.0.1/32`、`::1/128`，以及 Docker 常用的六段 172 地址池：`172.17.0.0/16`、`172.18.0.0/16`、`172.19.0.0/16`、`172.20.0.0/14`、`172.24.0.0/14`、`172.28.0.0/14`，合计覆盖 `172.17.0.0` 至 `172.31.255.255`。地址池说明见 [Docker 官方网络文档](https://docs.docker.com/engine/network/#automatic-subnet-allocation)。该默认会信任这些范围内的所有来源，后端端口应只供代理访问。

显式设置为 `none` 可禁用代理信任，忽略 `X-Forwarded-For` 并直接使用连接来源；`none` 不区分大小写，前后空白会被忽略。非空的逗号分隔 IP/CIDR 列表会完全覆盖默认值，需要保留的默认地址也须列入。使用其他 Docker 子网（包括 `192.168.0.0/16`、`10.0.0.0/8` 或 IPv6）时，应显式配置后端实际看到的代理来源。仅可信来源的转发头会用于区分登录限流客户端；应用从右向左跳过可信代理，采用第一个不可信地址。每个客户端五分钟内最多接受十次认证尝试，被拒绝的请求不会延长窗口。

数据库默认使用 InnoDB、`utf8mb4` 和 UTC。已有独立 MySQL 时，由管理员预建专用数据库并授予应用账户该库内 `SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, DROP` 权限，以便启动迁移与指标分区维护；不需要全局管理或创建数据库权限。MySQL 账户、自动备份及 binlog/PITR 由数据库运维独立管理。

Compose 的 InnoDB 缓冲池默认 512 MiB，可通过 `MYSQL_BUFFER_POOL_SIZE` 调整；连接、排序和其他运行开销另需内存。提交持久性保持 `innodb_flush_log_at_trx_commit=1`、`sync_binlog=1`。独立 MySQL 同样应按数据与索引工作集配置缓存，容量验收使用的实际参数记录在 [性能说明](docs/performance.md)。

生产环境首次创建管理员需要初始化验证码，防止公开入口被抢先注册。可预设独立随机的 `NEWNEXUS_SETUP_TOKEN`（32–512 字符且不含空白），或在启动后本机读取私有数据目录中的 `bootstrap.token`。Docker SQLite 例子为 `docker compose --profile sqlite exec server-sqlite cat /data/bootstrap.token`，MySQL profile 对应 `server-mysql`。只把验证码输入首次安装表单，不公开或放入聊天；已有管理员时不再使用该验证码。

生产 MySQL 跨主机连接使用 `mysql://newnexus:URL_ENCODED_PASSWORD@db.example.com:3306/newnexus?tls=true`；密码需 URL 编码，`tls=true` 验证证书和主机名（省略时也默认开启）。私有 CA 追加 `&tls_ca=/etc/newnexus/db-ca.pem`，需要显式证书域名时追加 `&tls_server_name=db.example.com`。禁止跳过证书验证或失败后降级。Compose 中的 `tls=false` 只针对同一受控 Docker 网络。对外发布前配置防火墙、证书续期、备份和故障通知。

已有 PostgreSQL 数据不会因更换 URL 自动转换。`--migrate` 仅负责目标数据库的表结构升级；跨引擎迁移须保留原库和主密钥，停写后单独转换并核对资源、任务、指标及发布记录，再切换连接。不要将旧 PostgreSQL 数据目录挂载给 MySQL。此前 PostgreSQL 压测保留为历史记录，MySQL 的结论以本轮独立验收为准。

## 接入探测 Agent

在「探测节点」生成一次性注册码后，可直接复制本次注册的 Linux 安装命令到目标主机执行。命令自动识别 amd64/arm64、下载并校验 SHA-256，创建权限为 `0600` 的环境文件和使用 `DynamicUser` 的 systemd 服务。安装命令含一次性注册码，只在注册结果中展示，请妥善保管。

为控制面配置 `NEWNEXUS_AGENT_PUBLIC_ADDR=agents.example.com:443`，或在安装表单填写节点可达的 TLS gRPC `host:port`。通配监听地址无法推断公开地址。安装包下载入口单独通过 `NEWNEXUS_AGENT_DOWNLOAD_ORIGIN=https://console.example.com` 预填，或在表单中修改；未配置时使用当前控制台来源。本地 `127.0.0.1` 控制台也可填写远程节点可达的 HTTPS 下载入口，但下载域名不代表同时支持 gRPC。后台「系统设置」可持久保存默认 API、Agent 和控制台地址，保存即作用于新安装信息；非空数据库值覆盖环境默认值，留空恢复继承。控制台访问地址可由 `NEWNEXUS_CONSOLE_ORIGIN` 提供环境默认值。私有 CA 文件需提前安装到节点，详见 [Agent 文档](docs/AGENT.md)。

后台还提供默认关闭的 Agent 自签名 TLS 开关。开启后控制面托管加密保存的 CA 与服务器证书，新的 gRPC 连接立即使用该证书，控制台 HTTP(S) 配置保持独立。FRP 与 Agent 前置代理须使用 TCP 透传，移除外部 NGINX 对 Agent 入口的 TLS 终止。已有节点先手动下载并校验 CA，补充现有环境文件中的 CA 路径和服务器名称后重启服务；保留原节点凭据和数据目录，不要重新注册。具体步骤见 [自签名 TLS](docs/AGENT.md#控制面托管的-agent-自签名-tls)。

也可手动安装 Linux Agent，或构建 `Dockerfile.agent`。推荐通过环境传入注册码，避免出现在进程参数中：

```sh
NEWNEXUS_AGENT_SERVER=agents.example.com:443 \
NEWNEXUS_AGENT_ENROLLMENT_TOKEN='<一次性注册码>' \
NEWNEXUS_AGENT_DATA_DIR=/var/lib/newnexus-agent \
newnexus-agent --name hk-edge-01 --region HK --carrier telecom
```

注册码换取凭据后不再需要保留；持久化 Agent 数据目录。默认只探测公开地址的 80/443 端口，其他业务端口需在 Agent 的 `--allowed-ports` 中显式增加。私有 CA 使用 `--ca-file`；不得跳过证书验证。详细配置见 [Agent 文档](docs/AGENT.md)。

本机 Agent 为可选 profile：配置 `NEWNEXUS_AGENT_ENROLLMENT_TOKEN` 和证书中的 `NEWNEXUS_TLS_SERVER_NAME` 后运行 `docker compose --profile sqlite --profile agent up -d --build`；MySQL 对应替换 profile。注册码消费后可清空环境值，节点凭据保存在独立卷。

## 运维与规模

SQLite 只支持一个服务/调度实例；建议不超过 5 个节点、5000 库存、1000 活动候选及 100 条规则。MySQL 8.4 使用 InnoDB 事务、跨副本租约和分区指标，支持多个控制面副本；共享数据库、主密钥和证书，通过支持 HTTP/2 的入口分流。首次初始化前，为所有副本设置相同的外部 `NEWNEXUS_SETUP_TOKEN`，避免各副本自动生成不同验证码后被负载均衡到另一实例。Compose 示例用于单控制面部署，多副本时使用独立服务编排与入口，避免争用相同发布端口。

Agent 默认限速为每节点 10 样本/秒。按 10000 活动候选、3 节点、每任务 3 样本、30 秒周期计算，仅候选探测就需要每节点平均 30 样本/秒，另需计入线路探测、重试和其他层级。部署该规模时先核算带宽和节点容量，再显式提高 `NEWNEXUS_AGENT_RATE`；例如 50 需要通过实际节点资源验收，不能沿用默认值推断容量。

原始样本/5 分钟聚合/小时聚合默认保留 7/30/180 天。管理设置可覆盖。数据库与密钥的备份、恢复和发布后复核见 [备份恢复](docs/BACKUP_RESTORE.md)。生产容量结论及可复现压力测试见 [验收清单](docs/ACCEPTANCE.md)。

## 测试与构建

```sh
go test ./...
go vet ./...
# 真实 MySQL 8.4 业务、仓储、租约与分区验证；仅专用测试实例允许创建/删除测试库
NEWNEXUS_TEST_MYSQL='mysql://test_user:URL_ENCODED_PASSWORD@127.0.0.1:3306/newnexus_test?tls=false' \
  NEWNEXUS_TEST_BACKEND=mysql go test ./...
cd web
npm test
npm run build
cd ..
go build -o bin/newnexus-server ./cmd/server
cd web
npm run test:e2e
```

首次运行 Playwright 需 `npx playwright install chromium`；Linux CI 使用 `--with-deps`。端到端脚本在独立临时数据目录启动实际控制面，自动创建测试管理员；不连接真实云账户，不把测试凭据部署到生产。API 契约见 [OpenAPI](api/openapi.yaml) 与 [REST 说明](docs/api-contract.md)，节点协议见 [agent.v1](proto/agent/v1/agent.proto)。

PowerShell 可运行 `./scripts/build-release.ps1`，输出 Linux amd64/arm64 控制面、Agent、前端静态文件、配置、文档和 SHA-256。每个架构发布目录的 `web/agent` 及控制面 Docker 镜像都含两种架构的 Agent，供安装命令下载。独立部署控制面时，将发布目录的完整 `web` 放到 `/opt/newnexus/web`，设置 `NEWNEXUS_WEB_DIR=/opt/newnexus/web`，并使用 `deploy/systemd/newnexus.service` 与 `deploy/newnexus.env.example` 配置。单个控制面二进制不内嵌前端；Agent 二进制独立运行。下载目录可由 `NEWNEXUS_AGENT_DOWNLOAD_DIR` 覆盖。

源码开发时，在 `npm run build` 后运行 `./scripts/build-agent-downloads.ps1` 生成 `web/dist/agent` 下的下载资源；再次构建前端会清空该目录，需重新生成。该脚本也支持 `-OutputDirectory` 指向独立下载目录。

在新 Linux 主机上，从对应架构的发布目录安装控制面（需要 root 或 sudo）：

```sh
getent group newnexus >/dev/null || groupadd --system newnexus
id newnexus >/dev/null 2>&1 || useradd --system --gid newnexus --home-dir /var/lib/newnexus --no-create-home --shell /usr/sbin/nologin newnexus
install -m 0755 newnexus-server /usr/local/bin/newnexus-server
install -d -m 0755 /opt/newnexus
cp -R web /opt/newnexus/
install -d -m 0700 /etc/newnexus
install -d -m 0750 -o root -g newnexus /etc/newnexus/tls
install -m 0600 deploy/newnexus.env.example /etc/newnexus/server.env
install -m 0644 deploy/systemd/newnexus.service /etc/systemd/system/newnexus.service
```

编辑 `/etc/newnexus/server.env` 中的主密钥、TLS 路径和可选数据库 URL；将真实 `fullchain.pem`、`privkey.pem` 安装为 `root:newnexus`、权限 `0640`。完成后执行 `systemctl daemon-reload && systemctl enable --now newnexus`，用 `journalctl -u newnexus` 检查启动。systemd 自动创建权限 `0700` 的 `/var/lib/newnexus`。环境文件与 TLS 私钥独立维护，不放进发布包。
