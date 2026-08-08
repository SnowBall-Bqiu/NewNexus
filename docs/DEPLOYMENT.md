# NewNexus 部署指南

本文覆盖预编译 Linux 包、Docker Compose、SQLite、PostgreSQL、TLS 反向代理和远程 Agent。生产环境不要启用 `NEWNEXUS_DEMO` 或 `NEWNEXUS_DEV_AUTH`。

## 1. 选择部署模式

| 模式 | 适用范围 | 约束 |
| --- | --- | --- |
| SQLite 单机 | 最多约 5 个 Agent、5000 个候选、100 条线路规则 | 只运行一个控制面和调度实例 |
| PostgreSQL | 生产环境、更高写入量、后续横向扩展 | 需要独立备份和数据库运维 |
| Docker Compose | 快速安装或容器平台 | 仍需为 HTTP 与 gRPC 配置 TLS 入口 |

无论使用哪种模式，Cloudflare、DNSPod 和阿里云仍是权威 DNS。NewNexus 不提供权威 DNS，也不使用 Cloudflare Load Balancing。

## 2. 使用预编译 Linux 包

发布目录包含以下文件：

```text
newnexus-v0.1.0-linux-amd64/
├── newnexus-server
├── newnexus-agent
├── web/dist/
├── api/openapi.yaml
├── deploy/
├── docs/
├── README.md
└── LICENSE
```

控制端运行时的工作目录必须是发布目录，因为前端静态文件和 OpenAPI 文件按 `web/dist`、`api/openapi.yaml` 读取。

校验并解压：

```bash
sha256sum -c SHA256SUMS
tar -xzf newnexus-v0.1.0-linux-amd64.tar.gz
cd newnexus-v0.1.0-linux-amd64
```

当前预编译发布包只支持 Linux amd64。

### 2.1 创建运行用户与目录

```bash
sudo useradd --system --home /var/lib/newnexus --shell /usr/sbin/nologin newnexus
sudo install -d -o newnexus -g newnexus /opt/newnexus /var/lib/newnexus /etc/newnexus
sudo install -m 755 newnexus-server newnexus-agent /opt/newnexus/
sudo cp -a web api docs deploy README.md LICENSE /opt/newnexus/
sudo chown -R root:root /opt/newnexus
```

### 2.2 配置 SQLite 控制端

生成并妥善备份主密钥：

```bash
openssl rand -base64 32
sudo install -m 600 -o root -g root deploy/newnexus.env.example /etc/newnexus/newnexus.env
sudo editor /etc/newnexus/newnexus.env
```

至少修改：

```dotenv
NEWNEXUS_MASTER_KEY=生成的32字节Base64密钥
NEWNEXUS_DATABASE_DRIVER=sqlite
NEWNEXUS_DATABASE_URL=/var/lib/newnexus/newnexus.db
NEWNEXUS_PUBLIC_URL=https://newnexus.example.com
NEWNEXUS_AGENT_PUBLIC_SERVER=grpc.newnexus.example.com:443
NEWNEXUS_AGENT_BINARY_PATH=/opt/newnexus/newnexus-agent
NEWNEXUS_SECURE_COOKIES=true
```

主密钥用于加密厂商凭据。丢失或更换后，现有 Cloudflare、DNSPod、阿里云和 SMTP 凭据无法解密。

安装 systemd 服务：

```bash
sudo install -m 644 deploy/systemd/newnexus.service /etc/systemd/system/newnexus.service
sudo systemctl daemon-reload
sudo systemctl enable --now newnexus
sudo systemctl status newnexus
curl http://127.0.0.1:8080/readyz
```

首次打开控制台后创建唯一管理员，并立即启用 TOTP。

### 2.3 配置 PostgreSQL 控制端

先创建数据库和最小权限用户，然后修改 `/etc/newnexus/newnexus.env`：

```dotenv
NEWNEXUS_DATABASE_DRIVER=postgres
NEWNEXUS_DATABASE_URL=postgres://newnexus:强密码@127.0.0.1:5432/newnexus?sslmode=require
```

如果 PostgreSQL 与控制端在同一受控主机且没有数据库 TLS，可按实际安全边界使用 `sslmode=disable`。控制端启动时自动执行 GORM 迁移；升级前仍必须先备份数据库。

## 3. TLS 与反向代理

HTTP 控制台默认监听 `:8080`，Agent gRPC 默认监听 `:9090`。生产环境建议只让反向代理访问这两个端口，并分别配置域名：

- `newnexus.example.com` 转发 HTTP、REST 和 SSE 到 `127.0.0.1:8080`。
- `grpc.newnexus.example.com` 使用 HTTP/2 长连接转发到明文 h2c `127.0.0.1:9090`。

Caddy 示例位于 `deploy/Caddyfile.example`。预编译包直接部署时，将示例中的 `control-plane` 改为 `127.0.0.1`：

```caddyfile
newnexus.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}

grpc.newnexus.example.com {
    reverse_proxy h2c://127.0.0.1:9090
}
```

SSE 不应启用响应缓冲；gRPC 入口必须允许 HTTP/2、长连接和至少 4 MiB 消息。

## 4. 安装远程探测 Agent

推荐在“探测节点”点击“注册节点”。主控会生成只显示一次的一键安装命令，形式如下：

```bash
curl -fsSL 'https://newnexus.example.com/install-agent.sh' | sudo sh -s -- --server 'grpc.newnexus.example.com:443' --code 'ONE_TIME_CODE' --name 'hk-edge-01' --protocols 'tcp,http' --download-url 'https://newnexus.example.com/downloads/newnexus-agent-linux-amd64' --sha256 '...'
```

将整条命令复制到目标 Linux amd64 主机执行。脚本会校验架构和 SHA-256，安装低权限 systemd 服务并立即注册。命令含一次性注册码，不要粘贴到公开日志、聊天或工单。

手动安装方式如下。

在控制台“探测节点”创建一次性注册码。远端主机执行：

```bash
sudo useradd --system --home /var/lib/newnexus-agent --shell /usr/sbin/nologin newnexus-agent
sudo install -d -o newnexus-agent -g newnexus-agent /var/lib/newnexus-agent /etc/newnexus
sudo install -m 755 newnexus-agent /usr/local/bin/newnexus-agent
sudo install -m 600 deploy/agent.env.example /etc/newnexus/agent.env
sudo editor /etc/newnexus/agent.env
sudo install -m 644 deploy/systemd/newnexus-agent.service /etc/systemd/system/newnexus-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now newnexus-agent
journalctl -u newnexus-agent -f
```

`/etc/newnexus/agent.env` 示例：

```dotenv
NEWNEXUS_AGENT_SERVER=grpc.newnexus.example.com:443
NEWNEXUS_AGENT_ENROLL_CODE=一次性注册码
NEWNEXUS_AGENT_NAME=hk-edge-01
NEWNEXUS_AGENT_PROTOCOLS=tcp,http
NEWNEXUS_AGENT_STATE=/var/lib/newnexus-agent/state.json
```

首次注册成功后，节点凭据写入状态文件。随后可删除环境文件中的一次性注册码并重启 Agent。Agent 只主动出站，不监听公网端口。仅本地明文开发环境使用 `NEWNEXUS_AGENT_INSECURE=true`。

## 5. Docker Compose

```bash
cp .env.example .env
openssl rand -base64 32
# 将结果写入 .env 的 NEWNEXUS_MASTER_KEY

# SQLite
docker compose --profile sqlite up -d --build

# PostgreSQL
docker compose --profile postgres up -d --build

# SQLite 加本机 Agent
docker compose --profile sqlite --profile agent up -d --build
```

启动本机 Agent 前，先在控制台生成注册码并填写 `.env`。生产环境同时修改 PostgreSQL 密码、外部 URL 和安全 Cookie 设置。

## 6. 备份、恢复与升级

SQLite 一致性备份：

```bash
sudo systemctl stop newnexus
sudo cp /var/lib/newnexus/newnexus.db /srv/backup/newnexus-$(date +%F-%H%M).db
sudo systemctl start newnexus
```

PostgreSQL 备份：

```bash
pg_dump --format=custom --file=newnexus-$(date +%F-%H%M).dump newnexus
```

备份必须同时包含 `/etc/newnexus/newnexus.env` 中的主密钥。恢复时停止控制端、恢复数据库与相同主密钥，再启动服务。

升级流程：

1. 校验新压缩包 SHA-256，备份数据库和主密钥。
2. 停止 `newnexus`，将新版本解压到新的 `/opt/newnexus-<version>` 目录。
3. 将 `/opt/newnexus` 符号链接切到新目录，确认工作目录仍能找到 `web/dist` 与 `api`。
4. 启动服务并检查 `/healthz`、`/readyz`、登录、Agent 在线和一条 DNS 预览。
5. 若失败，停止服务、恢复数据库快照并切回旧版本目录。

## 7. 上线检查

- `NEWNEXUS_DEMO=false`、`NEWNEXUS_DEV_AUTH=false`、`NEWNEXUS_SECURE_COOKIES=true`。
- 主密钥和数据库备份已验证可恢复。
- HTTP 与 gRPC 均使用有效 TLS；9090 不直接暴露公网。
- Cloudflare Token、DNSPod 和阿里云凭据遵循最小权限。
- Cloudflare 受管优选 A/AAAA 记录保持 DNS-only。
- SaaS 自动 DNS 只有在主机名和证书状态均为 `active` 后才写业务 CNAME。
- 告警出口、磁盘水位、Agent 离线和凭据失效通知经过演练。

## 8. 故障排查

```bash
journalctl -u newnexus -n 200 --no-pager
curl -v http://127.0.0.1:8080/healthz
curl -v http://127.0.0.1:8080/readyz
systemctl status newnexus newnexus-agent
```

- 页面返回 404：检查服务工作目录中是否存在 `web/dist/index.html`。
- OpenAPI 返回 404：检查 `api/openapi.yaml` 是否随包部署。
- Agent 不在线：检查 443 到 gRPC 域名、HTTP/2 反代、TLS SNI 和注册码是否已使用。
- 厂商发布失败：先查看发布预览和连接权限，不要手工删除当前 DNS；全故障策略会保留旧记录。
- 凭据无法解密：确认恢复的是原始 `NEWNEXUS_MASTER_KEY`。
