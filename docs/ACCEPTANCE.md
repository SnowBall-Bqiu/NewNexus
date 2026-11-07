# NewNexus 首版验收记录

记录日期：2026-11-07（Asia/Hong_Kong）。这是实现与实测的交付记录；未完成项目不会被计作通过。真实云账户验收按用户确认延后提供测试区域配置。

## MySQL 切换验收（当前版本）

按用户要求，生产部署已切换为 MySQL 8.4 LTS，轻量模式保留 SQLite。Compose 使用 `mysql` profile，CI 的业务和浏览器矩阵为 SQLite/MySQL；PostgreSQL 内部兼容代码和原始测试报告保留为历史记录，不代表 MySQL 的验收结果。更换连接 URL 不会自动转换已有 PostgreSQL 数据，旧库未被删除或改写。

| 检查 | MySQL 实测结果 |
| --- | --- |
| 数据库 | 官方 MySQL 8.4.13，InnoDB、utf8mb4、UTC，任务专用实例 |
| 仓储与并发 | 多实例迁移、CAS 唯一胜者、租约竞争/续租/过期、批量事务回滚全部通过 |
| JSON 与指标 | null/布尔/数值类型、generation guard、每日/晚到/未来分区、保留清理及午夜精度通过 |
| 认证、DNS/SaaS 控制层 | 全套功能测试使用独立 MySQL 数据库通过 |
| 完整 REST 业务验收 | 真实 MySQL 下通过，含厂商协议 fixture、DNS 验证、发布/回滚、自动模式与通知 |
| Linux Go race | 最终冻结源码全包通过；MySQL API 123.766 秒、Hub 51.791 秒、Store 36.370 秒，无 DATA RACE |
| SQLite、静态与模块检查 | 最终全包 `go test -p 1 -count 1 ./...`、`go vet ./...`、`go mod verify` 全部通过 |
| 浏览器 | 最终冻结版本 MySQL 13/13（43.8 秒）、SQLite 13/13（41.1 秒）通过；两套 HTML 报告与各 20 张布局截图分别保存 |
| 前端 | 37 测试、706 文案、UI/主题/动效检查及生产构建通过 |
| 数据库 TLS | 私有 CA 与证书主机名实连成功且有 TLS cipher；错名、不可信 CA、强制 TLS 实例明文均拒绝 |
| MySQL 备份恢复 | 生命周期库与非空指标库均在停止写入、连接为 0 后 dump 到新空库；9 表及所有现有资源类别计数一致，9/9 CHECK TABLE OK，最终镜像 `--migrate` 退出 0 |
| 最终 Docker / TLS Agent | 新 MySQL 镜像、专用库账号、UID/GID 65532、只读、移除 capabilities；CA 验证 HTTPS、初始化保护、Agent 两次轮换、持久重启和撤销退出全部通过 |
| Linux 发布包 | amd64/arm64 控制面与 Agent 构建通过；ELF64 架构与 SHA-256 核对一致，UI/API/运维文件齐全，无实际证书、私钥、注册码或环境密钥 |
| 新增驱动依赖扫描 | govulncheck：No vulnerabilities found |

MySQL 完整规模验收于 13:20:35–13:21:40（UTC+8）通过。保持 100 条 Agent 连接、100000 库存、10000 活动候选、1000 条规则、两个逻辑副本、每批 64 条结果、每副本 32 个数据库连接及默认调度预算 32；MySQL 实际缓冲池为 512 MiB，提交持久性 `1/1`，诊断观察器关闭。10000/10000 活动候选在前 30 秒实际到达三个不同节点，1000/1000 规则完成评估、双目标采样并健康。68000 条任务全部收到、ACK、持久完成并写入 raw metrics，零未完成、未确认和结果流错误；60 秒确认 63747 条，吞吐 1062.44 条/秒。12516 次已提交结果重放零增长，1600 次双 Hub 首次提交只增加 800 个唯一结果，两项均零错误。

原始 [最终 JSON](performance/mysql-optimized9-100-100k-10k-1000.json)、[阶段日志](performance/mysql-optimized9-100-100k-10k-1000.log)、此前未通过轮次及复现命令见 [性能验收说明](performance.md)。首轮规则版本冲突被 CAS 拒绝后在后续评估收敛，原日志完整保留；没有后台 deadline，计时终点主动取消的退出日志不计作结果丢失。该测试使用同进程的两个逻辑实例及合成观察值，不等同 100 台公网节点或长时间生产容量认证。

最终浏览器报告分别为 `output/e2e-mysql-final/index.html`、`output/e2e-sqlite-mysql-final/index.html`，各自的 `screenshots/` 保存 20 张主题/尺寸截图。最终 MySQL Linux 并发回归原始日志见 [mysql-final-linux-race.txt](performance/mysql-final-linux-race.txt)；SQLite 全包、vet 和模块校验日志保存于 `output/`。通知恢复回归验证同一 incident 复用任务、重新打开后独立事件与旧任务兼容处理，实际 HTTPS 投递使用不可变事件快照。

最终控制面与 Agent Docker 镜像使用本轮冻结源码重新构建。控制面缺失或错误首次初始化 proof 均拒绝，正确 proof 成功创建唯一管理员；Agent 没有入站端口，凭据连续两次实际改变，重启后节点 ID 与第二轮凭据一致，撤销后退出码 1 且删除活动凭据文件。独立 MySQL 新空库备份恢复、计数核对、表检查与迁移均通过，原始证据保存为 `output/mysql-final-*`。

生命周期恢复库保留 49 条资源、7 个资源类别，真实 HTTPS 启动后原管理员会话、初始化状态和已撤销节点均保留。另用最终负载客户端生成极小合成观测样本，单独补验非空指标分区恢复：原始指标和收据各 74 条，五分钟和小时桶各 50 条，242 条资源、12 个现有类别在新空库逐项一致；9 表检查和最终镜像迁移均通过。小样本只用于备份数据覆盖，不作为容量结论，原始证据为 `output/mysql-final-populated-backup*`。

最终 MySQL 发布包位于 `output/release-20261107-mysql/linux-amd64` 与 `linux-arm64`，每个架构包含控制面、独立 Agent、生产 UI、API 契约、文档、配置模板、systemd unit、SQLite 备份脚本及 `SHA256SUMS`。原始构建日志与核验结果为 `output/mysql-final-release-build.log`、`output/mysql-final-release-verification.json`。下方 PostgreSQL 统计仅是换库前的历史证据。

## 功能与证据索引

| 范围 | 实现入口 | 本地证据 |
| --- | --- | --- |
| 首次唯一管理员、密码、TOTP、恢复码、会话、API Key、CSRF | `internal/auth`、`internal/api`，登录与安全设置 | Go 单元/HTTP 测试、真实浏览器首次安装与 TOTP/恢复码登录 |
| 加密凭据、日志脱敏、SSRF、端口/超时/大小/速率限制 | `internal/security`、`internal/agentclient` | 解析/重定向/地址与 TLS 测试；生产启动守卫测试 |
| 项目、站点、节点组、HTTP 模板与管理设置 | REST v1、资源页面与设置 | CRUD/校验测试、真实浏览器保存与筛选 |
| 官方 IPv4/IPv6、手工/CIDR、CSV/JSON、DNS、HTTPS 源 | `internal/optimizer`、候选源页面 | 源解析、稳定抽样、资格/评分/聚合测试；手工源实际同步 |
| 库存/榜单/活动三级探测与全局/组/线路排名 | `internal/api/workers.go`、`internal/optimizer` | 调度、组筛选、加权评分与真实原始指标排名测试 |
| TCP/TLS/HTTP、Host/SNI、模板、失败重试、p50/p95/吞吐 | `internal/agentclient` | 本地 TCP/HTTP/TLS 服务的真实测量与策略边界测试 |
| 优先级/固定权重、健康仲裁、迟滞/冷却/恢复、全故障保留 | `internal/routing`、`internal/api` | 路由单元测试、实际 HTTP/gRPC 验收中的线路独立故障与保留 DNS |
| Cloudflare、DNSPod、阿里云 DNS | `internal/provider` | 生产签名与分页适配器的 HTTP 契约测试；真实账户待补 |
| Top N A/AAAA、CNAME 差异、能力/循环/冲突校验 | `internal/control` | 不可变发布预览、权限/额度/权重降级、发布前校验测试 |
| API+权威 DNS 验证、快照、补偿、回滚、漂移告警 | `internal/control`、发布/作业/告警页面 | HTTP 厂商模拟服务与本地权威 DNS UDP 服务，执行生产代码路径 |
| SaaS 区域、回退源、额度、独立源站、批量主机名 | `internal/provider`、`internal/control`、`internal/api/bulk.go` | 单项/批量幂等、逐项错误、异步作业、所有权和证书独立状态测试 |
| TXT/HTTP 手动和自动验证、双 active 迁移及清理 | SaaS 主机名详情、迁移发布 | 双 active 门槛、接管外部记录与快照回滚测试；见 `SAAS_MIGRATION.md` |
| Agent 注册、双向 gRPC、持久队列、背压、重放、轮换/撤销 | `cmd/agent`、`internal/agentclient`、`internal/agenthub` | 本地真实 TLS HTTP/2 和持久化测试，独立 Docker Agent 生命周期实测 |
| 作业租约、结果幂等、双控制面、SSE | `internal/store`、`internal/api`、`internal/agenthub` | 实际 MySQL 随机隔离数据库、多 Store/Hub 竞争与重放测试；SSE API 回归 |
| Webhook HMAC/重试、SMTP、告警/审计 | `internal/api/notifications.go`、`internal/control` | 本地 HTTPS Webhook 验签，SMTP STARTTLS/隐式 TLS/认证/收件及失败拒绝，告警/审计测试；公网实际收件待补 |
| 原始/5 分钟/小时指标、清理、容量和磁盘 | `internal/store`、`/system/status`、系统设置 | SQLite/MySQL 聚合、分区、保留/清理测试；指标展示读取真实 API |
| shadcn Base UI、主题、表格、虚拟渲染、键盘、移动布局 | `web/src`，源码内置维护 | Vitest、UI/i18n 静态检查与 Playwright；历史曲线无补间动画 |
| Docker、TLS、非 root、Linux amd64/arm64、备份恢复 | Dockerfile/Compose、`deploy`、`scripts` | 下列部署实测；CI 工作流已编写，未声称远程执行 |

## 已完成的独立部署实测

- Docker Desktop Linux 引擎构建控制面和独立 Agent 镜像成功；控制面以 UID/GID 65532、只读根文件系统、移除 capabilities 运行，TLS 健康检查通过。
- 使用临时测试 CA，客户端验证证书链和主机名后访问 HTTPS 健康端点及 SPA 页面成功；未跳过 TLS 校验。
- 独立 Agent 容器通过 TLS gRPC 完成一次性注册、在线心跳与健康检查；连续两次轮换凭据成功，重启后使用持久凭据恢复；撤销后进程停止。Agent 没有发布入站端口。
- Python SQLite 在线备份、完整性校验、恢复到新路径成功，备份与恢复文件 SHA-256 相同。
- PostgreSQL 17 的 `pg_dump -Fc`、恢复到新建空数据库和逐类资源计数核对成功，覆盖管理员、会话、设置、连接、候选、作业、审计等实际验收数据。没有覆盖已有数据库。
- 本地 SMTP 服务实际验证 STARTTLS 和隐式 TLS 两条生产协议路径、CA/主机名校验、认证、显示名称信封、中文 MIME 正文与事件 ID；无 STARTTLS、证书主机名不匹配或认证失败时均不投递。

这些容器和数据均为本任务专用测试资源，没有连接用户生产云账户。

## 切换前 PostgreSQL 版本的历史回归

2026-11-07 最终冻结源码复跑结果：

| 检查 | 结果 |
| --- | --- |
| Go 全包 `go test -p 1 ./... -count=1`，SQLite + 真实 PostgreSQL | 全部通过 |
| Linux Go 1.27.3 `go test -race -p 2 -count=1 ./...`，真实 PostgreSQL | 全部通过，无并发竞争报告 |
| `go mod verify`、`go vet ./...` | 通过 |
| 前端 Vitest | 12 文件、37 测试通过，包含生产初始化验证码表单 |
| Base UI / 主题 / 动效 / i18n / TypeScript / Vite build | 通过，706 条中文文案 |
| Chrome Playwright + SQLite 实际控制面 | 13/13 通过（33.7 秒） |
| Chrome Playwright + PostgreSQL 实际控制面 | 13/13 通过（30.7 秒） |
| npm production dependency audit | 0 vulnerabilities |
| govulncheck v1.9.0 全源码扫描 | No vulnerabilities found |
| Linux amd64 / arm64 控制面与 Agent 发布包 | 全部构建成功；ELF 架构、SHA256、UI 和运维文件核验通过 |
| 最终控制面 / Agent Docker 镜像 | 构建成功；非 root、只读运行与 TLS 健康检查通过 |
| 生产首次管理员保护 | CA 验证的 HTTPS 下，缺失/错误验证码均 403，正确码初始化成功，私有文件 0600 |

浏览器覆盖 23 个功能视图直达刷新、项目 CRUD/筛选/键盘焦点、连接与 CSRF、真实手工源同步、TOTP/恢复码、节点注册入口、全局 HTTP 模板保存、五个主要页面 × 明暗主题 × 桌面/移动端及减弱动效。虚拟行卸载、大批 DNS 删除确认、SaaS 验证说明与未激活禁切流三个前端交互使用隔离接口 fixture；对应后端业务由真实 HTTP/gRPC/仓储和厂商协议 fixture 独立验收，不声称浏览器已操作真实云账户。

五倍慢速动效使用 Chrome CDP 自动采样验证；不是人工慢速录像审查。最终独立容器 Agent 通过 CA 验证的 TLS gRPC 完成注册、两次凭据轮换、持久卷重启恢复和撤销退出（状态码 1）；无入站端口。两个数据库的浏览器 HTML 报告与 20 张主题/尺寸截图在本地 `output/` 中；该目录不纳入 Git。

发布包只复制明确的部署模板，包含 SQLite 备份脚本和 Agent systemd unit；不复制部署目录中的实际证书、私钥或环境密钥。CI 同步相同内容，远程 CI 未在本会话执行。

## PostgreSQL 版本的历史压力验收

目标保持 100 条 Agent 流、100000 库存、10000 活动候选、1000 条规则、两个共享 PostgreSQL 的控制面实例。使用实际仓储、调度、路由和 gRPC 结果摄入；探测观察值由负载客户端生成，不产生外部 CDN 带宽或 DNS 发布请求。

最终 optimized4 同规模基线通过：10000/10000 活跃候选在 30 秒内完成三个节点的覆盖，1000/1000 规则完成评估、双目标采样和健康判定。共 68000 个任务全部 ACK、完成并写入原始指标，0 pending、0 error；60 秒内 ACK 66022，吞吐 1100.30 结果/秒。12282 次重放未增加指标且无错误，两个重新连接的 Hub 在 1580 次提交后只有 790 个唯一指标。

第一轮至 optimized3 的失败或中间报告均保留，含死锁、排空与周期覆盖问题及修复过程。`ResultLoss` 表示测量和排空窗口结束时仍未 ACK 的结果，不能直接解释为永久丢失。最终 JSON、阶段日志和复现命令见 [性能验收说明](performance.md)。此测试为同一进程中的两个逻辑控制面实例，使用真实 PostgreSQL 和 gRPC；不等同 100 台物理节点、公网 TLS 带宽或跨机长稳验收。

可复现命令、机器环境、后续报告与限制以 `performance/` 最终说明为准。真实生产吞吐还取决于节点网络、TLS、磁盘、数据库硬件、探测样本数及云厂商限额。

## 延后验收：真实账户与生产网络

用户已选择先完成本地、双数据库和压力测试，稍后补充测试账户配置。下面项目仍需专用测试区域和通知目标：

1. 三家 DNS 实际账户的权限、套餐、线路、额度、加权能力与限流响应。
2. 专用 DNS 记录的创建、更新、API/公网权威验证、失败补偿与恢复，确认无专用区域以外的变更。
3. Cloudflare for SaaS 的真实额度、回退源、自定义源站、TXT/HTTP 证书验证、续期、迁移与清理；O2O 场景按厂商实际限制验收。
4. 实际 SMTP 收件、Webhook 重试/验签、节点离线与发布失败告警。
5. 独立 Linux 节点的公网 TCP/TLS/业务 URL 测量、真实 CDN 带宽、跨机双副本故障恢复及长期保留清理。

密钥只写本机受限环境文件或部署密钥系统，不粘贴到聊天或纳入仓库。开始时记录专用域名与原始 DNS 快照，结束后用发布回滚恢复并核对。真实账户测试的结果确认前，本交付不声明已完成生产环境验收。
