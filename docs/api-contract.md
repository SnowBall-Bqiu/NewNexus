# NewNexus REST 与 Agent 契约

基础路径 `/api/v1`，JSON 使用 snake_case，时间使用 UTC RFC3339。完整机器可读定义见 [OpenAPI](../api/openapi.yaml)。业务资源使用 UUID；会话、API Key 和幂等索引使用不可逆哈希标识。

Cookie 登录返回 `csrf_token`；携带 Cookie 的写请求必须发送 `X-CSRF-Token`。API Key 使用 `Authorization: Bearer <key>`。错误格式为 `{error:{code,message}}`；原始厂商响应和凭据不会成为公共错误正文。

集合 GET 返回 `{items,total,next_cursor}`，支持 `limit`（默认 50、最大 500）、`cursor`、`q`、`project_id`、`status`，候选另支持 `source_id`、`tier` 和 `sort=score`。游标不再存在时返回 `invalid_cursor`。空安装始终返回空集合。

## 管理员与安全

| 方法与路径 | 输入及行为 |
| --- | --- |
| GET /bootstrap/status | 返回 `{initialized,setup_token_required}`；无需登录，不返回初始化凭据 |
| POST /bootstrap | `{username,password,setup_token?}`；唯一管理员，密码至少 12 字符；生产首次初始化要求运维持有的 setup_token，缺失/错误返回 403 setup_token_invalid |
| POST /auth/login | `{username,password,totp_code?}`；`totp_code` 同时接受一次性恢复码 |
| GET /auth/me | 返回 `{user:{username,totp_enabled},csrf_token}` |
| POST /auth/logout | 撤销当前会话并清除 Cookie |
| POST /auth/totp/setup | 返回 `{secret,otpauth_url}`；待确认密钥独立加密 |
| POST /auth/totp/enable | `{code}`；返回只显示一次的 `recovery_codes` |
| GET /auth/sessions | 当前管理员会话列表，不包含会话 token/CSRF |
| DELETE /auth/sessions/:id | 撤销指定会话 |
| GET /auth/api-keys | 只返回名称、ID、元数据 |
| POST /auth/api-keys | `{name}`；返回 `{id,key}`，完整 Key 仅本次显示 |
| DELETE /auth/api-keys/:id | 撤销 Key |

密码使用 Argon2id；TOTP 检查时间窗口并阻止同一时间步重复使用。登录具有 IP 频率限制。生产环境强制外部主密钥、TLS 与安全 Cookie；明文 gRPC、开发鉴权和不安全 Cookie 的例外仅限显式启用的非生产回环监听器。

生产首次安装从 `NEWNEXUS_SETUP_TOKEN` 读取 32–512 bytes、无空白的初始化凭据；未配置时在数据目录自动生成随机 `bootstrap.token`（0600，重启保留）。API 仅持有哈希；运维从环境或私有数据卷读取凭据后，在初始化表单填写。已有管理员后不再需要初始化凭据，普通登录不携带它。生产多副本建议配置相同的外部 setup token。

## 资源

除下列限制外，集合支持 GET/POST，详情支持 GET/PUT/PATCH/DELETE。创建返回资源（201）；异步外部操作返回 `{job_id}`（202）。服务端字段由白名单保护，不能通过通用 CRUD 伪造能力、凭据、健康、任务或发布状态。关系仍被引用时删除返回 409。

- `/projects`：`{name,description,status}`。
- `/sites`：`{project_id,name,url,probe_profile_id,status,healthy,last_probed_at,...}`。URL 与关联模板决定真实站点任务。
- `/connections`：`{name,provider:cloudflare|dnspod|alidns,purposes,credentials,...}`。凭据仅写入，分别 AEAD 加密；GET 返回掩码与真实发现的 `capabilities`、`zone_capabilities`、`capability_errors`、同步状态。能力未探明时保持未知；成功读 API 不保证写权限。
- `/sources`：`{project_id,name,type:manual|cloudflare|url|dns|csv|json,location,enabled,...}`。`url` 必须 HTTPS；保存与执行均拒绝不安全地址，执行时重新解析并钉住公共 IP。
- `/candidates`：`{project_id,source_id,address,family,tier,healthy,score,success_rate,tcp_ms,tls_ms,ttfb_ms,p50_ms,p95_ms,download_bps,sample_count,last_probed_at,aggregate_pending}`。健康、评分和测量字段只读；合法公共 IP 才能进入库存。
- `/nodes`：仅允许 GET、编辑元数据和删除；注册通过注册码。可编辑 `{name,group_name,region,carrier,mode:tcp|http|all}`；能力、协议、凭据、在线状态、最近心跳与实际样本失败率由服务端维护。
- `/node-groups`：`{name,description,node_ids}`。节点的组名称与显式 node_ids 均可建立同一组成员关系。
- `/probe-profiles`：`{project_id,name,mode,url,host,sni,path,method,headers,expected_statuses,expected_contains,timeout_ms,max_bytes,retries,samples,max_redirects,interval_seconds,user_agent}`。缺省/null 字段继承全局模板；URL 为空继承全局 URL。关联站点/线路的间隔可设 5–86400 秒；候选仍执行库存/榜单/活动三级频率。
- `/rules`（别名 `/routes`）：`{project_id,site_id?,name,hostname,dns_connection_id,zone_id,zone_name,ttl,publish_mode:manual|auto,switch_mode:availability|performance|ranking,probe_profile_id,lines}`。
- 每条线路 `{id,provider_line_code,alias,node_group,health_perspective,min_nodes?,quorum_percent?,targets}`。省略仲裁值表示继承当前全局设置；更新时传 null 清除旧自定义值。运行时有效值不会被写成新配置。
- 每个目标 `{id,name,cname,priority,weight,enabled,protocol?,port?,headers?}`。协议/端口/请求头可覆盖；连接目标 CNAME，Host/TLS SNI 保持业务 URL（除显式模板覆盖）。健康、连续轮数、评分属于只读运行状态。
- `/saas/zones`：`{project_id,connection_id,zone_id,zone_name,fallback_origin,cname_target,...}`。创建会排真实回退源写入作业；同步读取 observed，漂移只告警。
- `/saas/hostnames`：`{project_id,site_id?,saas_zone_id,hostname,validation_method:txt|http,automation_mode:manual|auto,custom_origin?,dns_connection_id?,dns_zone_id?,dns_zone_name?,...}`。主机名和 SSL 独立状态、验证操作清单及厂商 ID 只读。创建/删除走持久作业。
- `/optimized-hostnames`：`{project_id,hostname,dns_connection_id,zone_id,zone_name,family,top_n,ttl,...}`。通过预览/发布生成受管 A/AAAA；Cloudflare 必须 DNS-only。
- `/releases`、`/jobs`、`/alerts`、`/audit` 只能读取和执行指定动作，不能直接创建、改状态或删除。

连接 `zones` 元素为 `{id,name,grade?,account_id?}`；Cloudflare account_id 来自真实区域 API。同账户自定义源站验证使用该 ID，并确认显式、已代理的 A/AAAA/CNAME。自定义源站支持由已验证 SaaS 能力及官方全套餐能力提供，SNI rewrite 不在此能力中。

## 动作与发布

节点重新部署沿用原节点 ID、名称、分组、地区、运营商、管理模式与历史计数。生成注册码即撤销原凭据和待确认轮换凭据，节点标记为已撤销；重复签发立即使上次注册码失效，注册成功后节点先转为离线，再由新 Agent 心跳上线。注册码在数据库中仅保存哈希，不能从节点读取接口或审计日志找回。撤销节点同样会使未使用的重新部署注册码失效。

注册码消费、重新部署与删除通过数据库事务和节点行锁串行化；会话使用内部世代和有效网关租约共同校验心跳、结果、任务 ACK、凭据 ACK 及断开清理。即使旧网关尚未关闭连接，也不能更新重新部署后的节点；删除后的旧帧和旧注册码不能重建节点。内部世代与注册码哈希不在 REST 节点响应中公开。

| 路径（默认 POST） | 说明 |
| --- | --- |
| /connections/:id/validate、/sync | 真实凭据、能力、区域、线路发现作业 |
| GET /connections/:id/zones | 缓存的实际区域；缺少时同步 |
| GET /connections/:id/lines?zone_id=... | 实际厂商线路代码；中文名称不会替代 API 代码 |
| /sources/:id/sync | 实际导入并去重；IPv6 稳定抽样 |
| /candidates/probe | `{ids?:[]}`；排实际探测任务作业 |
| /nodes/enrollment-tokens | `{name?,expires_in_seconds?}` → `{token,expires_at}` |
| GET /nodes/installation | 登录后返回 `{server,download_origin,console_origin,insecure_loopback,tls,artifacts:[{arch,url,sha256}]}`；三种公开地址独立，`tls` 始终存在，仅列出实际存在的 Linux amd64/arm64 安装包 |
| GET/HEAD /agent-downloads/linux-amd64、/linux-arm64 | 公开下载固定白名单二进制，支持 Range；不提供目录或凭据下载；未部署的架构返回 404 |
| GET/HEAD /agent-ca.pem | 仅托管 Agent 自签名 TLS 开启时匿名下载 CA PEM，关闭时 404；不返回服务器证书或任何私钥，始终 no-store |
| /nodes/:id/revoke、/rotate | 撤销或持久凭据轮换；等待匹配的轮换 ACK 后移除旧凭据 |
| /nodes/:id/redeploy | `{confirmed:true,expires_in_seconds?:900}` → 201 `{token,expires_at,node_id,name}`；需要登录和 CSRF，必须显式确认；有效期可设 60–900 秒，省略时 15 分钟；只显示一次，no-store |
| DELETE /nodes/:id | 撤销凭据与在线会话，删除节点、分组中的显式成员 ID、待执行任务与重新部署注册码；保留指标、已完成任务和审计记录 |
| /rules/:id/evaluate | 使用真实新鲜节点指标仲裁，不修改权重 |
| /rules/:id/preview | 返回不可变发布与逐条 DNS 差异 |
| /optimized-hostnames/:id/preview | 用有资格的真实候选生成 A/AAAA 发布 |
| /saas/zones/:id/sync | 只读 observed/额度/回退源漂移检查 |
| /saas/hostnames/:id/sync、/activate | 自动切流前重新确认 hostname 与 SSL 均 active |
| /saas/hostnames/:id/preview | 双 active 后预览旧 DNS 到 SaaS 的迁移，保留回滚快照 |
| /saas/hostnames/:id/resume-automation | 明确恢复因漂移暂停的 DNS 自动化后同步 |
| /releases/:id/apply | `{confirmed:true}` 且必须 `Idempotency-Key`；重新检查配置/远端快照/能力/SaaS |
| /releases/:id/rollback | 按已完成变更 journal 精确恢复，拒绝覆盖后续手工漂移 |
| /jobs/:id/retry | 仅失败作业可重试；指数退避与租约跨副本生效 |
| /alerts/:id/acknowledge、/resolve | 告警状态操作 |

发布在 API 回读和直接权威 DNS 验证都通过后才报告成功。失败按实际已完成记录补偿，保留明确失败或回滚失败状态；不伪造成功。全故障 fail-stale 不删除或猜测替代 DNS。

## SaaS 批量

`POST /saas/hostnames/bulk` 接收：

```json
{"saas_zone_id":"uuid","hostnames":["a.example.com","b.example.com"],"validation_method":"txt","automation_mode":"manual"}
```

可追加单项创建相同的 DNS、origin、project/site 字段。`POST /saas/hostnames/bulk-delete` 接收 `{ids:[...],confirmed:true}`。每批 1–500 项，必须 `Idempotency-Key`。202 返回 `{items,job_ids,errors:[{index,hostname|id,code,message}],accepted,total}`。逐项持久化、独立作业与逐项错误；部分接受不等于全部成功。重放返回已保存结果。

## 指标、排名、实时和运维

- GET `/metrics/history?resource_type=&id=&group_id=&line_id=&resolution=hour|five_minute&hours=24`：读取实际持久聚合，支持 1–4320 小时及资源/节点组/线路交集；返回 `{items,total,scope,resolution,hours}`。`items` 包含 `{time,latency,success,samples,tcp_ms,tls_ms,ttfb_ms,download_bps}`；success 为 0–100 百分比。
- GET `/candidates/rankings?group_id=&line_id=&project_id=&family=&limit=&cursor=`：以实际新鲜样本及当前权重重新计算全局/组/线路资格与评分，不把全局缓存冒充组榜单。
- GET `/overview`：`{counts,healthy_rules,online_nodes,latency_series,recent_alerts,recent_releases,database,version,updated_at}`。历史曲线为实际最近 24 小时小时桶。
- GET `/settings`：`{items:[{key,value,updated_at}],total}`；SMTP 密码/Webhook secret 只回掩码。PUT 接收 `{key:value,...}`，拒绝未知键、错误类型和越界值；这两个密钥的空值或原掩码表示保留已有加密值。评分/资格设置改变会标记待重算并返回 `{ok:true,job_id}`。
- 三个公开地址设置为 `addresses.api_origin`、`addresses.agent_endpoint`、`addresses.console_origin`。前后两项接受 HTTP(S) origin，中间项接受非通配 `host:port`；三项必须是字符串，允许空串表示继承。非空值规范化后持久保存且立即作用于安装元数据，分别覆盖 `NEWNEXUS_AGENT_DOWNLOAD_ORIGIN`、`NEWNEXUS_AGENT_PUBLIC_ADDR`（或实际监听地址）、`NEWNEXUS_CONSOLE_ORIGIN`。GET 对这三行额外返回 `effective_value` 和 `source:database|runtime|unset`，`value` 始终保留原始数据库配置（继承时为空），避免普通设置表单把环境默认值误保存成覆盖项。清空后 PUT 恢复环境默认值；环境也未配置时，origin 保持空供控制台回退当前来源。默认地址不改变监听端口、代理或已有节点连接。
- `agent_tls.self_signed_enabled` 是默认 false 的持久布尔设置，只接受 JSON boolean。PUT 为 true 时先准备可用的加密证书材料，失败返回 `503 agent_tls_unavailable` 且不保存启用状态；正常设置鉴权、CSRF 和审计继续适用。GET settings 仅返回开关布尔值，不包含证书材料。新 gRPC 连接读取最新开关，已有连接在重连前保持原有传输。此开关不修改 HTTP(S) 控制台配置。
- 全局模板键为 `probe.default_url/default_user_agent/timeout_ms/max_bytes/retries/host/sni/path/method/headers/expected_statuses/expected_contains/samples/max_redirects`；完整范围以 OpenAPI SettingsUpdate 为准。
- GET `/system/status`：真实 `database,db_bytes,disk_free_bytes,disk_total_bytes,disk_free_percent,last_cleanup_at,last_cleanup_error,cleanup_duration_ms,partitions,raw_samples,aggregates,db_growth_bytes,db_growth_window_seconds,observed_at`。raw_samples 为报告行数，aggregates 为桶行数。远端 MySQL 磁盘未知时返回 null 和说明，绝不伪造 0。磁盘 warning/critical 阈值由 settings 生效。
- GET `/events`：SSE `event:resource.updated`，`data:{kind,id}`。跨 MySQL 副本以持久资源更新时间发现变更；id 为空表示重读该集合，支持 Last-Event-ID 重连。
- GET `/healthz` 或 `/api/v1/healthz`：无需登录，实际 DB ping；独立 server `--healthcheck` 验证本机 TLS 证书。

告警通知按 `incident_id` 固定任务身份和事件快照；同一告警解决后再次打开会生成新 incident。恢复未完成的通知排队会复用该 incident 的原任务。Webhook 使用 HTTPS，`X-NewNexus-Signature` 为原始请求体的 HMAC-SHA256（`sha256=<hex>`），`X-NewNexus-Event-ID` 与正文顶层 `id` 为 incident ID，正文 `data.id` 为控制台告警资源 ID；无 incident 的旧记录回退资源 ID。SMTP 正文使用相同事件 ID。通知失败会按作业策略重试，接收方应按事件 ID 幂等处理；已发送但响应丢失时仍可能重投。

## Agent v1

安装元数据的 `tls` 关闭时严格为 `{self_signed_enabled:false}`，无 CA 地址或指纹。开启时为 `{self_signed_enabled:true,server_name,ca_url,ca_sha256,ca_fingerprint_sha256,not_after}`，并强制 `insecure_loopback:false`。`ca_sha256` 是可下载 PEM 原始字节的 SHA-256，`ca_fingerprint_sha256` 是 CA 证书 DER 的 SHA-256，`not_after` 使用 RFC3339。内部 `agent-tls` 资源不在 REST 白名单中，私钥只以 Vault 加密材料存储。公开 CA 下载不提供私钥，且禁用缓存。已有节点必须手动下载并核对 CA，配置可信 CA 路径及服务器名称，再重启现有服务；保留长期凭据与持久数据目录。FRP 和前置代理需为 Agent 入口提供 TCP 透传，不能在外部 NGINX 上终止 TLS；控制台 HTTPS 不受影响，操作步骤见 [Agent 文档](AGENT.md)。

`download_origin` 优先使用 `addresses.api_origin`，回退到可选配置 `NEWNEXUS_AGENT_DOWNLOAD_ORIGIN`（或 `-agent-download-origin`）。`console_origin` 优先使用 `addresses.console_origin`，回退 `NEWNEXUS_CONSOLE_ORIGIN`（或 `-console-origin`）。两者接受无凭据、路径（可有末尾 `/`）、查询或片段的 HTTP(S) origin，返回时规范化为无末尾 `/`。均未配置时返回空字符串，控制台可回退到当前页面来源并允许管理员填写节点可达的下载入口；服务端不从请求头推断。下载 origin 仅供 HTTP(S) 安装包下载，不能据此推断 TLS gRPC 入口或明文模式，`server` 继续独立配置。`server` 优先采用 `addresses.agent_endpoint`；数据库覆盖为不同于运行时已确认的明文目标时，`insecure_loopback` 一律为 false，即使新目标仍是另一个回环 IP 或端口。

安装向导从 `NEWNEXUS_AGENT_PUBLIC_ADDR`（或 `-agent-public-addr`）读取向节点公开的 gRPC `host:port`，未配置时采用实际非通配监听地址；通配监听器不推断公网域名，控制台要求填写实际可达地址。`insecure_loopback` 只在服务端已经允许的非生产回环明文配置下、公开目标与实际明文监听器为同一字面回环 IP 和端口时为 true。公开地址覆盖为不同端口、不同 IP 或主机名（包括 `localhost`）时保持 TLS 默认，避免误判本机 TLS 代理。`NEWNEXUS_AGENT_DOWNLOAD_DIR`（或 `-agent-download-dir`）默认 `<web-dir>/agent`，只读取 `newnexus-agent-linux-amd64`、`newnexus-agent-linux-arm64` 两个常规文件。元数据每次按实际文件重新计算 SHA-256，缺少安装包时返回空数组，不生成无法下载的指令。注册码继续仅由独立的鉴权 POST 生成，公开二进制下载不携带或返回注册码。

契约见 [agent.proto](../proto/agent/v1/agent.proto)，标准 gRPC/protobuf `google.protobuf.Struct`，服务 `agent.v1.AgentService`，协议版本 1。Enroll 一次性注册码换凭据，Connect 为主动出站的 TLS HTTP/2 双向流。

任务包含 id/candidate/address 和完整模板字段；结果 id 必须为 `sha256(node_id + ":" + task_id)` 的小写十六进制编码，不能通过换 ID 让同一任务重复计数。结果包含真实观测 UTC、sample_count/success_rate（0–1）、延迟、吞吐、HTTP 状态与错误。批结果上限 256；结果 ID/raw/rollup/任务完成/节点计数/待评分标记原子提交后才 ACK。离线旧报告在可配置 raw 保留期内接受，过期仍幂等完成并 ACK 以排空 outbox。

任务 ACK 仅表示 Agent 已持久接收；结果 ACK 后才删除持久 outbox。服务端有界队列与 Agent 本地并发、速率、端口白名单构成背压，未接收任务继续持久重放。乱序不覆盖较新观测；独立聚合 worker 从完整近期历史重算候选/站点评分，generation CAS 保留重算期间新到和乱序报告。撤销、轮换及协议不兼容均有显式帧/错误。

