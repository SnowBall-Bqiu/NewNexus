# Agent 自动部署与协议

## 控制台生成命令

在“探测节点”创建节点后，主控生成 15 分钟有效的一次性注册码、目标架构下载地址、二进制 SHA-256 和可复制命令。Agent 只建立出站 TLS HTTP/2 连接，不监听公网端口。

```bash
curl -fsSL 'https://newnexus.example.com/install-agent.sh' | sudo sh -s -- \
  --server 'grpc.newnexus.example.com:9090' \
  --code 'ONE_TIME_CODE' \
  --name 'hk-edge-01' \
  --protocols 'tcp,http' \
  --architecture 'linux-amd64' \
  --download-url 'https://newnexus.example.com/downloads/newnexus-agent-linux-amd64' \
  --sha256 'BINARY_SHA256' \
  --tls-sha256 'CERTIFICATE_SHA256'
```

安装器校验架构、下载协议、二进制哈希和证书指纹，创建低权限用户、systemd 服务与状态目录。指纹写入 `/etc/newnexus/agent.env`，Agent 每次握手都在正常 CA 校验之后再次校验服务端叶证书。重复安装会备份环境文件；`--upgrade` 保留节点凭据，`--uninstall` 默认保留状态，`--uninstall --purge` 才删除状态和 outbox。

## 可靠传输

- `Enroll` 消费一次性注册码并换取 90 天节点凭据。
- `Connect` 首先发送 `hello` 和 feature negotiation；rc.2 Agent 未声明增强功能时标记为降级模式。
- 服务端任务持久化后发送，Agent 先 ACK；租约超时会重新投递。
- 探测结果先写入本地 bbolt outbox，再批量上报；只有收到 `result_ack` 才删除。
- 服务端以 `job_id` 幂等保存结果，并拒绝超过 24 小时或未来超过 5 分钟的乱序时间戳。
- 凭据轮换带有 `rotation_id`：服务端保留旧 Token，Agent 原子持久化新 Token 并 ACK 后才提交；超时或断线会回退旧 Token。
- 节点长期凭据过期后，只在收到明确的 `Unauthenticated/expired` 时消费一次性恢复码重新 `Enroll`；恢复码可撤销、会过期且只可使用一次。
- 被撤销节点不会使用恢复码自动重新注册，控制台保持 `revoked` 状态并生成告警。

节点撤销后可以在“探测节点”页面执行删除。删除前控制面会检查未完成任务以及使用该节点组的线路仲裁容量；存在依赖时返回明确原因，不会强制删除。删除控制面记录不会卸载远端 Agent，如需彻底移除仍应在目标主机执行卸载命令。

正式契约见 [`../proto/agent/v1/agent.proto`](../proto/agent/v1/agent.proto)。
