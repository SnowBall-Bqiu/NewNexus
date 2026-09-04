# NewNexus 开发文档

## 1. 技术栈与目录

- Go 1.22 模块，建议使用 Go 1.24；控制端和 Agent 均可构建为 `CGO_ENABLED=0` 的 Linux 二进制。
- React 18、TypeScript 5.6、Vite 5。
- `@cloudflare/kumo@2.9.2` 提供 Sidebar、Dialog、Tooltip、Table、Select、Toasty、Meter 与 TimeseriesChart；Phosphor 提供图标，Virtuoso 提供大候选列表虚拟化，ECharts 6 提供历史指标渲染。
- SQLite 使用纯 Go 驱动；PostgreSQL 使用 GORM PostgreSQL 驱动。

```text
cmd/server/          控制面 HTTP、SSE、gRPC 入口
cmd/agent/           仅主动出站的探测 Agent
internal/api/        /api/v1 REST 与静态前端
internal/agenthub/   agent.v1 注册、双向流、任务与结果
internal/agentoutbox/ Agent bbolt 持久化结果队列
internal/jobs/       外部写作业、租约、重试、取消与进度
internal/netguard/   SSRF、端口白名单与固定解析地址
internal/dnsverify/  权威 DNS 回读确认
internal/optimizer/  候选同步、IPv6 抽样与评分
internal/routing/    线路仲裁、迟滞、发布和回滚
internal/provider/   Cloudflare、DNSPod、阿里云适配器
internal/security/   Argon2id、AEAD 与令牌工具
internal/store/      双数据库、迁移和演示数据
web/                 React 管理界面
api/openapi.yaml     REST 契约入口
proto/agent/v1/      正式 Agent 协议与 rc.2 兼容说明
deploy/              反向代理与 systemd 示例
```

## 2. 初始化

```powershell
git clone https://github.com/SnowBall-Bqiu/NewNexus.git
cd NewNexus
go mod download
cd web
npm ci
cd ..
```

本地演示模式：

```powershell
cd web
npm run build
cd ..
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
$env:NEWNEXUS_DATABASE_DRIVER='sqlite'
$env:NEWNEXUS_DATABASE_URL='dev-newnexus.db'
$env:NEWNEXUS_HTTP_ADDR='127.0.0.1:18080'
$env:NEWNEXUS_GRPC_ADDR='127.0.0.1:19090'
$env:NEWNEXUS_PUBLIC_URL='http://127.0.0.1:18080'
$env:NEWNEXUS_AGENT_PUBLIC_SERVER='127.0.0.1:19090'
go install github.com/air-verse/air@v1.61.7
air -c .air.toml
```

打开 `http://127.0.0.1:18080`。`DEV_AUTH` 会绕过首次管理员流程，仅限本机开发。

后端开发统一使用 Air 热重载：

```powershell
# 终端 1
$env:PATH="$env:PATH;$env:USERPROFILE\go\bin"
go install github.com/air-verse/air@v1.61.7
$env:NEWNEXUS_DEMO='true'
$env:NEWNEXUS_DEV_AUTH='true'
$env:NEWNEXUS_HTTP_ADDR='127.0.0.1:18080'
$env:NEWNEXUS_GRPC_ADDR='127.0.0.1:19090'
air -c .air.toml

# 终端 2
cd web
$env:VITE_API_PROXY_TARGET='http://127.0.0.1:18080'
npm run dev
```

Vite 将 `/api`、`/healthz` 和 `/readyz` 请求代理到 `VITE_API_PROXY_TARGET`，未设置时默认使用 `http://127.0.0.1:18080`。提交前仍需运行生产构建，因为控制端正式提供的是 `web/dist`。

`.air.toml` 监听 Go 与 API YAML 文件，构建到 `tmp/air`，忽略前端产物、依赖目录、测试文件和本地数据库。不要用 `go run ./cmd/server` 代替日常后端开发入口；这样可以保证热重载配置持续可用。

## 3. 数据库开发

SQLite 是默认值，启动时自动建表和迁移。SQLite 测试不得启动多个控制面或调度实例。

PostgreSQL 示例：

```powershell
docker compose --profile postgres up -d postgres
$env:NEWNEXUS_DATABASE_DRIVER='postgres'
$env:NEWNEXUS_DATABASE_URL='postgres://newnexus:change-me-before-production@127.0.0.1:5432/newnexus?sslmode=disable'
air -c .air.toml
```

新增实体时：

1. 在 `internal/domain/models.go` 定义字段、索引和 JSON 名称。
2. 将实体加入 `internal/store/store.go` 的 `AutoMigrate`。
3. 同时跑 SQLite 与 PostgreSQL 迁移测试。
4. 外部厂商对象保留 desired/observed、厂商 ID、同步时间和错误状态。

## 4. API 约定

- 管理 API 固定前缀 `/api/v1`，时间使用 UTC RFC 3339，资源 ID 使用 UUID。
- 列表响应为 `data` 与 `meta.total`；错误响应包含稳定 `error.code` 和面向管理员的消息。
- 浏览器写请求受会话认证和 CSRF 保护；API Key 不使用 Cookie CSRF。外部厂商写操作必须返回 `202`、`job_id`，支持 `Idempotency-Key` 并记录审计。
- 实时状态使用 `/api/v1/events/stream` SSE，不为高频状态刷新增加布局动画。
- 运行中的 OpenAPI：`GET /api/v1/openapi.yaml`。

增加接口时同时更新：

1. `internal/api/server.go` 路由与处理器。
2. `api/openapi.yaml`。
3. `web/src/types.ts` 与调用页面。
4. 成功、权限不足、厂商超时和重复请求测试。

## 5. Agent gRPC 协议

服务名为 `agent.v1.AgentService`：

- `Enroll` 使用一次性注册码换取节点 ID 和长期凭据。
- `Connect` 是 TLS HTTP/2 双向流，服务端推送任务，Agent 上报心跳和批量结果。
- Agent 携带 `Authorization: Bearer <token>` 和 `x-agent-version` 元数据。
- `proto/agent/v1/agent.proto` 定义版本、能力、任务 ACK、结果批次和恢复游标；rc.2 Struct Agent 以降级模式滚动兼容。
- 结果通过任务/结果 ID 幂等；任务有租约，流量受 4 MiB 消息限制与并发信号量背压。
- Agent 断线指数退避，凭据保存在 `--state`，结果保存在相邻的 bbolt outbox，收到服务端 ACK 后删除。

本地运行 Agent：

```powershell
go run ./cmd/agent -- `
  --server 127.0.0.1:9090 `
  --enroll-code YOUR_CODE `
  --name local-agent `
  --state .tmp/agent-state.json `
  --insecure
```

生产 Agent 禁止使用 `--insecure`。

## 6. DNS 与 SaaS 适配器

`internal/provider.Adapter` 固定包含：能力检测、凭据验证、区域/线路同步、记录读取和变更应用。Cloudflare for SaaS API 保持为 Cloudflare 独立能力，不塞进通用 DNS 适配器。

适配器开发规则：

- Cloudflare DNS 只提供默认线路，并强制受管记录 `proxied=false`。
- DNSPod 和阿里云线路代码来自厂商 API，不在业务层猜测代码。
- 厂商不支持权重时返回能力限制，不用重复记录模拟权重。
- 变更先预览、后应用、再从厂商 API 回读；失败保留发布前快照。
- SaaS 自动 DNS 先调和 TXT，只有 hostname 与 SSL 同时 `active` 后才调和业务 CNAME。
- CNAME 与 A/AAAA 冲突时停止，不自动删除未知记录。

真实账号测试必须显式启用，默认测试套件不得产生外部 DNS 写入。

## 7. 调度与安全不变量

- 默认至少 2 个节点、67% 仲裁、连续 3 轮异常才切换。
- 性能切换要求备用优 20% 且连续 5 轮；恢复连续 5 轮健康；切换后冷却 10 分钟。
- 权重固定，不随评分自动改变；评分只影响优先级组选择。
- 样本不足时暂停自动切换并告警。
- 所有目标故障时 fail-stale：保留当前 DNS，不删除记录。
- 探测和订阅必须通过 SSRF 地址校验、端口白名单、超时、响应体和重定向限制。

任何修改这些不变量的代码都需要对应的仲裁、迟滞、冷却和全故障测试。

## 8. 前端与动效

界面中文首发，文案经 `web/src/lib/i18n.ts` 管理。运维页面采用 Kumo 风格的紧凑表格、明确状态和差异视图，不创建营销页。主题状态由 `web/src/lib/theme.ts` 管理，提供浅色、深色、黑白高对比和跟随系统四种模式；项目名称、最多两个字符的简称和识别色由项目接口持久化。

### Kumo 组件约定

- 固定使用 `@cloudflare/kumo@2.9.2`。入口必须先加载 `@cloudflare/kumo/styles/standalone`，再加载仅负责产品布局与主题桥接的 `src/tool-theme.css`；项目不引入 Tailwind，也不维护第二套组件样式。
- `src/components/ui.tsx` 是业务薄层，只负责状态中文映射、资源页头、指标带、异步状态和统一确认流程。不要复制 Kumo 内部样式或从 `@base-ui/react` 直接构建第二套组件。
- React Router 通过 Kumo `LinkProvider` 接入；Toast 统一经 `src/lib/toast.ts` 的 Kumo manager 分发。
- 浅色、深色和跟随系统主题同步写入 `data-theme`、`data-theme-mode`、Kumo `data-mode` 与 `color-scheme`。黑白模式通过覆盖 Kumo semantic token 实现，不为单个组件写颜色补丁。
- 核心资源删除使用业务薄层 `AppDeleteResource` 输入资源名称。该组件完全由 Kumo `Dialog`、`Field`、`Input` 和 `Button` 组合，因为 Kumo 2.9.2 的 `DeleteResource` 除删除按钮外未开放中文文案；叶子资源使用危险确认 Dialog，大范围外部删除仍使用 2 秒按住确认。
- 可比较数据使用 Kumo `Table`，状态丰富对象使用单层 Resource List，总览才使用独立概览卡。禁止嵌套 `LayerCard`。

升级 Kumo 时：

1. 将目标版本精确写入 `web/package.json`，运行 `npm install --save-exact @cloudflare/kumo@<version>`。
2. 检查 release notes 和本地 `dist/index.d.ts`，确认 Sidebar、Dialog、Toasty、Select、Table 与图表接口兼容。
3. 运行 `npm run lint`、`npm test`、`npm run build` 和 `npm run test:e2e`。
4. 在浅色、深色、黑白、跟随系统以及 reduced-motion 下检查 1440、1116、820、390 四档视口。
5. 不允许为了兼容升级重新加入 Lucide、Sonner、Recharts、旧 Base UI 包或应用层颜色 transition；`check:kumo` 会阻止这些回归。

动效规则：

- 只使用 `--ease-out`、`--ease-in-out`、`--ease-drawer` 三个 token。
- 常规动画只修改 `transform` 和 `opacity`，Accordion 是唯一高度例外。
- 禁止 `transition: all`、UI `ease-in`、`scale(0)` 和无理由超过 300ms 的动画。
- 实时指标、状态颜色、表格筛选和导航即时更新，不做 Tween 或 Stagger。
- 所有动效必须有 `prefers-reduced-motion` 分支，触摸设备不触发伪 Hover 位移。

静态策略检查已集成到 `npm run build`。

## 9. 测试与构建

```powershell
go test ./cmd/... ./internal/...
go vet ./cmd/... ./internal/...

cd web
npm test
npm run build
cd ..

powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1 -Version 0.1.0
```

GitHub Release 发布时，`.github/workflows/release.yml` 会分别交叉编译 Linux amd64/arm64 控制端部署包；每个包都包含两种 Agent。工作流汇总 `SHA256SUMS` 并上传到对应 Release，同时通过 Buildx 将多架构控制端与 Agent 镜像推送到 `ghcr.io/snowball-bqiu/newnexus`、`ghcr.io/snowball-bqiu/newnexus-agent`。工作流只使用仓库自带的 `GITHUB_TOKEN`，需要保留仓库 Actions 的 `contents: write` 与 `packages: write` 权限。

浏览器验收至少覆盖 1440px 桌面和 390px 移动端，并检查：首次安装、TOTP、凭据、SaaS、线路发布预览、回滚、键盘焦点、reduced-motion 和横向溢出。

真实厂商账户测试使用单独环境变量并显式启用。PostgreSQL 集成测试使用 `NEWNEXUS_TEST_POSTGRES_URL`，为每个测试创建隔离 schema，覆盖迁移重复执行、日分区、Agent 结果幂等和多工作器租约；GitHub CI 会启动临时 PostgreSQL 运行这些测试。

## 10. 当前实现边界

- 首版为模块化单体；`ProviderAdapterV1` 只冻结迁移方向，不加载第三方二进制。
- SQLite 不支持多副本；启动租约会拒绝第二个控制面。PostgreSQL 作业使用 `FOR UPDATE SKIP LOCKED`，原始样本按 UTC 日分区并批量写入，生产容量基线仍需在目标硬件验收。
- SMTP 与 HMAC Webhook 使用异步通知作业；生产启用前仍应执行真实投递、退避和收件端去重演练。
- Docker 镜像需要可用的 Docker daemon 才能完成实际构建验收。
- Release 部署包与 Agent 支持 Linux amd64/arm64；其他操作系统和 CPU 架构不在首版发布范围内。
