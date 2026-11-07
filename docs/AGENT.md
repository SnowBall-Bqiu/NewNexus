# NewNexus 探测节点

探测节点只主动连接控制面，不监听任何 TCP/UDP 公网端口。每个节点使用独立的数据目录和一次性注册码。数据目录包含长期节点凭据、待执行任务、未确认探测结果和幂等收据，必须持久保存并限制访问权限。

## 注册和运行

在控制台「探测节点」生成一次性注册码后，使用注册结果中的 Linux 安装命令。命令包含本次真实注册码，自动识别 `amd64` / `arm64`，从填写的「安装包下载入口」下载 Agent，并验证控制面提供的 SHA-256 后安装。需要目标主机具备 systemd、curl、sha256sum，并以 root 或 sudo 执行。注册弹窗关闭前复制命令；注册码不会再次展示。不要把命令发送到公共聊天或日志。

安装命令创建权限为 `0600` 的 `/etc/newnexus/agent.env` 和 systemd 单元，使用 `DynamicUser` 运行 Agent，持久化状态目录为 `/var/lib/newnexus-agent`。安装后使用 `systemctl status newnexus-agent`、`journalctl -u newnexus-agent` 检查运行情况。注册码只在首次注册时使用；成功后可以从环境文件中删除。控制面 gRPC 入口必须有 TLS，反向代理须支持 HTTP/2 和双向流，禁用响应缓冲，空闲超时建议至少 5 分钟。

安装表单中的「控制面 gRPC 地址」填写节点实际可达的 `host:port`，例如 `agents.example.com:443`，不要填 HTTP URL 或控制台页面端口。在控制面设置 `NEWNEXUS_AGENT_PUBLIC_ADDR=agents.example.com:443` 可预填该值。`NEWNEXUS_GRPC_ADDR=0.0.0.0:9090`、`[::]:9090` 只是监听地址，控制台不会据此推断公开入口；未配置时需在表单填写。

「安装包下载入口」与「控制面 gRPC 地址」分别配置。可在控制面设置 `NEWNEXUS_AGENT_DOWNLOAD_ORIGIN=https://console.example.com`（或 `-agent-download-origin`），让安装表单预填节点可达的 HTTPS 下载入口；仅允许 HTTP(S) origin，可带端口，不能包含账号密码、路径、查询参数或片段。末尾 `/` 会自动去除。未配置时表单使用当前控制台来源，也可在表单中单独修改。服务端不会根据请求的 Host、Origin 或 Forwarded 头推断公开入口。

管理员也可在后台「系统设置」保存默认 API / 安装包地址、Agent gRPC 地址和控制台访问地址，对应 `addresses.api_origin`、`addresses.agent_endpoint`、`addresses.console_origin`。数据库中的非空值优先于环境配置，保存后新生成的安装信息立即生效，控制面重启后仍保留。留空保存表示恢复环境或运行时默认值。控制台地址的环境默认值为 `NEWNEXUS_CONSOLE_ORIGIN`（或 `-console-origin`）；与下载入口一样仅接受 HTTP(S) origin。三种公开地址独立配置，不会更改监听器、反向代理、DNS 或已有 Agent 的目标地址。

页面位置为「系统设置 → 访问地址」（`/settings?tab=addresses`）。页面同时展示已生效地址及其来源；空输入框中的环境默认值仅作提示，不会被误写成数据库覆盖值。控制台访问地址用于生成“打开控制台”链接；API 地址用于安装包下载，浏览器自身仍通过当前控制台的同源 `/api` 代理请求服务。

使用 Vite 开发服务作为 HTTP 隧道源站时，在未提交的 `web/.env.local` 中配置精确域名列表 `VITE_DEV_ALLOWED_HOSTS=console.example.com,api.example.com`，由 `web/vite.config.ts` 加载。不要设置 `allowedHosts: true`。它只影响开发服务的 Host 校验，不能为隧道新增 gRPC 支持；正式环境应由入口反向代理配置允许域名。

Cloudflare Tunnel 的公开主机名目前不支持原生 gRPC；官方支持的是私网路由配合 Cloudflare One/WARP，见 [Cloudflare gRPC 限制](https://developers.cloudflare.com/network/grpc-connections/)及 [Tunnel gRPC 配置](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/grpc/)。本项目 Agent 当前只使用原生 gRPC，不能仅把公开 HTTP Tunnel 域名填成 `:443` 就完成连接。可使用可达的 TLS gRPC 入口，或先搭建私网路由；安装包下载仍可走公开 HTTP(S) Tunnel。

若控制台在 `127.0.0.1` / `localhost` 打开，远程节点无法访问该下载入口；可继续使用本地控制台，在表单的下载入口中填写节点可达的 HTTPS 控制台域名。该入口必须实际转发 `/api/v1/agent-downloads/`；普通 HTTPS 控制台隧道可供下载二进制，并不代表同时支持 Agent 的 TLS gRPC 双向流。gRPC 目标须独立填写已验证可达的地址，不能直接从下载域名推断。回环地址 gRPC 的明文模式仅用于在控制面同一主机调试，不能直接用于远程节点。

也可自行安装二进制并设置以下环境变量启动：

```sh
export NEWNEXUS_AGENT_SERVER=control.example.com:443
export NEWNEXUS_AGENT_ENROLLMENT_TOKEN='从控制台复制的一次性注册码'
export NEWNEXUS_AGENT_NAME=hong-kong-01
export NEWNEXUS_AGENT_REGION=香港
export NEWNEXUS_AGENT_CARRIER=电信
export NEWNEXUS_AGENT_DATA_DIR=/var/lib/newnexus-agent
./newnexus-agent
```

系统信任库必须包含控制面证书的 CA。自备 CA 使用 `NEWNEXUS_AGENT_CA_FILE=/etc/newnexus/ca.pem`；该路径必须是节点主机上已安装且 Agent 可读的 PEM 文件，控制台不会自动传输用户本地的证书文件。控制面托管 CA 的入口见下一节。可用 `NEWNEXUS_AGENT_TLS_SERVER_NAME` 指定证书主机名。要求 TLS 客户端证书的代理可以同时设置 `NEWNEXUS_AGENT_CLIENT_CERT`、`NEWNEXUS_AGENT_CLIENT_KEY`。不提供跳过证书验证开关。下载入口若使用私有 CA，同样需先加入节点系统信任库；gRPC 的 CA 配置不会自动改变 curl 的下载证书验证。

## 重新部署和删除节点

节点列表每行的操作菜单提供「重新部署此节点」和「删除节点」。重新部署用于保留同一节点身份、名称、分组、地区、管理模式与历史统计，同时重新安装 Agent 或迁移到另一台主机。确认生成后原凭据立即撤销，在线会话也会被隔离；新的 15 分钟一次性注册码绑定原节点 ID。重新生成会使上一份未使用注册码失效，关闭弹窗后无法找回明文注册码。

在目标 Linux 主机以 root 执行本次生成的重新部署指令。标准 systemd 安装会先下载并校验新二进制及启用中的托管 CA，全部成功后才停止原服务；旧环境文件、二进制、单元、证书和状态数据备份至权限受限的 `/var/backups/newnexus-agent/redeploy.*`。脚本使用新的状态目录注册原节点 ID，旧凭据与本机撤销标记不会带入新部署。原始状态会移入备份，不递归删除。脚本不会自动接管自定义 systemd 单元、drop-in 或非标准数据目录。

同一份指令不要重复执行；执行记录会拒绝旧指令覆盖较新的部署。如果安装失败，脚本尽量恢复文件和状态，保留备份位置并停止服务。后台已经撤销的旧凭据不会恢复有效，须重新生成注册码后再尝试。备份含敏感凭据，应保持 root 专用权限。

删除节点会撤销其凭据，移除节点记录、分组中的显式成员引用、待执行任务和绑定的重新部署注册码；历史指标、已完成任务与审计保留。删除只影响控制面，不会通过网络卸载远程主机上的服务；需要时在原节点手动执行 `systemctl disable --now newnexus-agent`。删除后的旧 Agent、旧凭据和旧注册码都不能恢复这条节点记录。若要保留原 ID，请选择重新部署。

## 控制面托管的 Agent 自签名 TLS

后台「系统设置」的 Agent 自签名 TLS 开关默认为关闭，对应布尔设置 `agent_tls.self_signed_enabled`。开启时控制面生成并加密保存专用于 Agent gRPC 的 CA 和服务器证书，准备成功后才启用。开关立即作用于新建立的 gRPC 连接，不改变控制台 HTTP(S) 配置；已有连接继续使用已协商的传输，直到重连。关闭后新连接恢复原有 gRPC TLS 配置，或原本显式允许的本地开发回环明文模式，不能据此对远程节点使用明文。

明文回退要求控制面同时满足 `NEWNEXUS_ENV=development`、实际 gRPC 监听器绑定字面回环地址，以及显式配置 `NEWNEXUS_ALLOW_INSECURE_AGENT=1`，并且连接来源也是回环地址。关闭开关本身不会授予明文权限：未满足这些条件且没有原全局 TLS 证书时，新连接会被拒绝；生产环境和公网监听器不会因此自动降级。同机 NGINX 的本地 h2c 开发模式也需要上述显式许可；FRP 连接显示为本机来源不能替代此许可。

安装元数据的 `tls` 会提供服务器名称、CA 下载地址、PEM 文件 SHA-256、证书 DER SHA-256 指纹和有效期。开启时公开 `GET/HEAD /api/v1/agent-ca.pem` 仅返回 CA 证书，不包含 CA 私钥、服务器私钥或注册码；关闭时返回 404，响应使用 `Cache-Control: no-store`。私钥以主密钥加密保存在数据库的内部资源中，不作为 REST 资源公开。关闭、重开和正常控制面重启均保留原 CA；备份恢复必须同时保留数据库和原主密钥。

FRP 必须使用 TCP 透传，将远程节点连接原样转发至控制面的 gRPC 监听器。使用此开关时，Agent 入口的外部 NGINX 或其他代理应移除 TLS 终止配置，改为纯 TCP 透传；控制台自己的 HTTPS 入口继续正常提供下载和管理页面。只有 HTTP(S) 转发的隧道不能替代 gRPC TCP 入口，也不能把控制台下载域名直接当作 Agent 地址。

已有节点启用该模式前，先通过可信 HTTPS 控制台入口手动下载 CA，核对控制台显示的 SHA-256，将 CA 放到节点上 Agent 可读取的固定路径，再编辑现有的环境文件，增加 `NEWNEXUS_AGENT_CA_FILE` 与控制台给出的 `NEWNEXUS_AGENT_TLS_SERVER_NAME`。CA 是公开证书，可使用 `0644` 权限，其父目录必须允许 Agent 用户读取。保留现有节点凭据与 `/var/lib/newnexus-agent` 数据目录；不要重新注册或覆盖整份环境文件。配置完成后重启节点服务，使其建立新的 TLS 连接。若同时更改 FRP 公网入口，再单独更新现有 `NEWNEXUS_AGENT_SERVER`。

```sh
# 使用节点实际可达、证书可信的控制台入口；先下载到临时文件并核对控制台提供的哈希。
curl --fail --show-error --location https://console.example.com/api/v1/agent-ca.pem -o /tmp/newnexus-agent-ca.pem
sha256sum /tmp/newnexus-agent-ca.pem
install -m 0644 /tmp/newnexus-agent-ca.pem /etc/newnexus/agent-ca.pem
# 编辑已有 /etc/newnexus/agent.env，保留其中的其他配置与持久凭据：
# NEWNEXUS_AGENT_CA_FILE=/etc/newnexus/agent-ca.pem
# NEWNEXUS_AGENT_TLS_SERVER_NAME=<控制台安装信息中的服务器名称>
systemctl restart newnexus-agent
```

本地开发仅可显式使用 `--insecure-loopback --server 127.0.0.1:9090` 或 `[::1]:9090`。远程私网 IP、容器服务名、`localhost` 和公网地址都不能启用明文模式；容器之间也应使用 TLS。

退出发送 SIGTERM/SIGINT 即可。未完成任务保留在磁盘，重启后继续执行。退出期间被取消的探测不会写入误报故障结果。

## Linux 构建与安装

正式发布包中，每个控制面架构目录的 `web/agent/` 都包含两种 Linux Agent：`newnexus-agent-linux-amd64`、`newnexus-agent-linux-arm64`。控制面 Docker 镜像也内置这两个文件，节点架构不必与控制面相同。控制面通过公开的 `/api/v1/agent-downloads/linux-amd64` 和 `/api/v1/agent-downloads/linux-arm64` 提供文件；二进制下载不需要管理员会话，文件本身不包含注册码或节点凭据。注册和安装配置信息仍需管理员权限。

默认下载目录为 `NEWNEXUS_WEB_DIR/agent`，也可用 `NEWNEXUS_AGENT_DOWNLOAD_DIR` 指定独立目录。仅安装控制面二进制、遗漏 `web/agent` 时，安装界面会提示下载资源不可用；先部署文件再复制安装命令。使用自定义下载目录的容器部署时，需将含两种二进制的目录挂载到容器内，并将配置指向该容器路径。

源码开发或单独更新下载文件时，在仓库根目录运行 PowerShell：

```powershell
# npm run build 会清空 web/dist，应先完成前端构建，再生成下载文件。
./scripts/build-agent-downloads.ps1
# 或指定 NEWNEXUS_AGENT_DOWNLOAD_DIR 对应的绝对路径：
./scripts/build-agent-downloads.ps1 -OutputDirectory /path/to/agent-downloads
```

脚本使用当前 Go 工具链，默认输出到 `web/dist/agent`，不把二进制提交到 Git。运行 `scripts/build-release.ps1` 时会在前端构建后自动调用该脚本，并在各架构发布目录的 `SHA256SUMS` 中列出两个下载文件的哈希。

使用仓库 `go.mod` 要求的 Go 工具链，从仓库根目录运行：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/newnexus-agent-linux-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/newnexus-agent-linux-arm64 ./cmd/agent
```

二进制不依赖 libc；宿主机仍需正常 DNS、CA 信任库和准确时间。可安装到 `/usr/local/bin/newnexus-agent`，复制 `cmd/agent/newnexus-agent.service` 到 `/etc/systemd/system/`，将上述环境变量写入权限为 `0600` 的 `/etc/newnexus/agent.env`，再执行 `systemctl daemon-reload && systemctl enable --now newnexus-agent`。单元使用 `DynamicUser`、私有临时目录和持久 `StateDirectory`；不要给节点 root 权限。

```sh
docker build -f cmd/agent/Dockerfile -t newnexus-agent .
docker volume create newnexus-agent-data
docker run -d --name newnexus-agent --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --mount source=newnexus-agent-data,target=/data \
  --env-file /etc/newnexus/agent.env newnexus-agent
```

镜像使用 UID/GID `65532:65532`，默认数据目录 `/data`。绑定宿主目录时预先将所有权设置为该 UID/GID，权限 `0700`。不同节点不能共享同一数据卷；同一目录会有独占进程锁，避免多个进程破坏持久队列。

## 本地策略和健康检查

环境变量都有对应 CLI 参数，`newnexus-agent --help` 可查看完整选项。

| 环境变量 | 默认值 | 含义 |
| --- | --- | --- |
| `NEWNEXUS_AGENT_CONCURRENCY` | `4` | 同时执行任务数，最多 128 |
| `NEWNEXUS_AGENT_MODE` | `all` | 节点能力：`all`、`tcp` 或 `http`；控制面结合能力调度 |
| `NEWNEXUS_AGENT_RATE` | `10` | 所有 worker 合计每秒探测样本数 |
| `NEWNEXUS_AGENT_ALLOWED_PORTS` | `80,443` | 目标端口白名单，HTTP 重定向也验证 |
| `NEWNEXUS_AGENT_MAX_TIMEOUT` | `30s` | 单个样本最高超时，最多 `60s` |
| `NEWNEXUS_AGENT_MAX_SAMPLES` | `10` | 单任务最高样本数 |
| `NEWNEXUS_AGENT_MAX_RESPONSE_BYTES` | `16777216` | 单样本下载上限，最多 128 MiB |
| `NEWNEXUS_AGENT_MAX_PENDING` | `10000` | 持久待执行任务和未确认结果的合计上限 |
| `NEWNEXUS_AGENT_MAX_OUTBOX_BYTES` | `134217728` | 新任务入队的磁盘字节水位 |
| `NEWNEXUS_AGENT_USER_AGENT` | Firefox UA + NewNexus 版本 | 默认请求 User-Agent |

任务不能放宽节点本地限制。默认任务采样 3 次、每次超时 5 秒、下载 1 MiB 前缀，默认允许最多 3 次重定向且硬上限为 5 次。不使用系统 `HTTP_PROXY`/`HTTPS_PROXY`，所有探测直接连接已校验并固定的候选 IP。URL、候选 CNAME 解析结果和重定向目标均拒绝私网、回环、链路本地、元数据、IPv4 映射、文档及其他保留地址。混合返回公网和私网地址的域名整条拒绝，防止 DNS 重绑定。

对公开业务使用非 80/443 端口时，在每个承担该业务探测的节点设置相应白名单，例如 `NEWNEXUS_AGENT_ALLOWED_PORTS=80,443,8443,2053`。端口支持范围为 1..65535；控制面配置不会自动扩大节点白名单，拒绝原因会随测量结果上报。

部署容量按样本数计算：每秒最低样本数约为「活跃候选数 × 代表节点数 × 每任务样本数 ÷ 周期」，再加站点、规则、重试和发现任务。10,000 个活跃候选、3 个代表节点、3 次样本、30 秒周期，需要至少 3,000 样本/秒；再加 1,000 条规则、每规则 2 个目标和 2 个节点，需要约 400 样本/秒。均分到 100 个节点约 34 样本/秒，默认每节点 10 样本/秒不能支撑这个负载。应结合实际 RTT、下载大小和带宽，为节点设置充足速率、并发及容量余量，并通过真实节点压测验证。控制面合成结果压测不测量这些网络成本，详见 [性能验证](performance.md)。

HTTP 的实际 TCP 连接固定到候选 IP/CNAME 和任务目标端口，业务 URL 的 Host 与 TLS SNI 保持不变；模板可显式覆盖 Host/SNI。HTTPS 重定向不能降级 HTTP。下载上限表示测量前缀的大小，超大文件只测前缀；`expected_contains` 也在此前缀内匹配。TLS 始终验证证书。

控制面可为每个样本配置 `retries: 0..3`，失败后才重试，重试也计入节点总速率。`sample_count` 和 `attempted_samples` 是实际尝试数，`requested_samples` 是要求成功的样本数；`success_rate` 按所有尝试计算，保留重试中的失败信息用于评分。`success` 表示每个要求样本最终成功，用于可用性仲裁。HTTP 自动跳转 HTTPS 时继续使用原候选 IP，并在端口白名单内从 80 切换到 443；任务显式配置候选端口时保持该覆盖值。

```sh
newnexus-agent --healthcheck --data-dir /var/lib/newnexus-agent
```

健康检查读取 `health.json` 并校验连接状态和有效期，不开放额外监听端口。Docker 内置该检查。断开连接即标记不健康；重连不丢弃队列。日志采用 JSON，不输出注册码、长期凭据、HTTP 模板请求头或请求正文。

## 结果持久化与凭据生命周期

- 服务端任务先原子落盘、`fsync`，再确认接收。完成后结果先原子落盘，再删除任务。进程在这两步之间退出可自动恢复。
- 结果 ID 固定为 `hex(SHA-256(UTF-8(node_id + ":" + task_id)))`，使用小写十六进制。Agent 与服务端共用该函数，服务端拒绝改换结果 ID 或将旧结果 ID 绑定到另一任务，断线重连或任务重复派发不改变 ID。结果每批最多 256 个，只在收到匹配 `batch_id` 的服务端 ACK 后删除。
- 同时最多一个未确认批次，15 秒未 ACK 重发；断线后重建批次并使用相同结果 ID 重放。已 ACK 任务的本地幂等收据保留 7 天，最多 50000 条。
- 队列达到条目或字节水位时发送背压帧且不确认新任务。正在执行的任务允许先写入结果，可能短暂超出入队水位；这样不会为了水位丢弃测量结果。磁盘写入失败直接停止并报错，修复磁盘后重启恢复。
- 凭据轮换先保存新凭据，再回复 `credential_ack`。撤销收到后清除凭据、持久写入撤销标记、终止进程；被撤销的数据目录不能自动重新注册。更换节点时使用新注册码和新数据目录。
- 轮换确认到达前，新旧凭据均可用于恢复连接；匹配的持久确认到达后才撤销旧凭据。已有轮换等待确认时不能覆盖它，已撤销节点也不能因旧连接尚未退出而重新轮换。轮换只原子更新凭据字段，保留同时提交的探测计数和管理员配置。
- 控制面的连接主循环每五秒检查一次待确认轮换，并统一投递凭据；轮换操作成功后可能需要等待一个检查周期。凭据帧与持久确认在同一主循环中按序处理，避免较早轮换的延迟帧覆盖较新的本地凭据。
- 网关每秒从持久队列拉取本节点待投递任务，使其他控制面副本生成的任务及时送达；该周期独立于五秒的凭据、撤销和连接租约检查。已接受任务保留原重试等待，背压时任务继续保存在数据库中。
- 服务端结果事务与撤销通过节点行锁串行化：撤销等待已经开始提交的批次结束，撤销操作成功返回后不再接纳新测量。撤销原子清除现有及待确认凭据；心跳、旧连接退出和已延迟到达的确认不能恢复撤销节点。
- 协议版本固定为 `agent.v1`/版本 `1`，版本不兼容直接报错。连接失败按 1 至 30 秒指数退避并加随机抖动。

## 测试

```sh
go test ./internal/agentclient ./cmd/agent
```

安全与探测测试使用注入解析器/拨号器和本地 HTTP/TLS 服务，校验真实 TCP/TLS/HTTP 测量、业务 Host/SNI、固定候选路由、大小/超时/端口/速率/重定向限制。gRPC 使用 bufconn 测试注册、协议协商、任务持久确认、结果 ACK、断线重放、重复任务、轮换和撤销。没有默认访问云厂商账户或公共测量站点。

2026-11-06/07 的本地 Docker 验证实际运行了独立 Agent 镜像：CA 验证的 TLS 注册、心跳、健康检查，两次连续凭据轮换（后一次成功验证了前一次 ACK），使用原数据卷重启后恢复凭据，撤销后以状态码 1 退出。容器使用非 root UID 65532、只读根文件系统且没有端口映射。另有真实 TLS/HTTP2 gRPC 服务和生产 Hub 的自动化互通测试；性能规模与尚未覆盖的网络拓扑单列在性能文档中。
