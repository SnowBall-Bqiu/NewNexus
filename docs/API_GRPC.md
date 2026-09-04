# API 与 gRPC 约定

REST 固定使用 `/api/v1`，时间为 UTC RFC 3339，ID 为 UUID。项目、连接、SaaS、候选、线路、发布、作业、告警和审计列表统一接受 `limit` 与 `cursor`，返回 `data`、`meta.total` 和 `meta.nextCursor`。候选榜单游标由 `score + address + id` 组成，其他时间序列列表使用 `created_at + id`，因此同分或同时间记录不会跳项。错误使用 `error.code`、`error.message`。浏览器 Cookie 写请求必须携带与 Cookie 一致的 `X-CSRF-Token`，`Bearer nn_...` API Key 不使用 Cookie CSRF。

DNS、SaaS、候选源同步、发布、回滚和批量清理返回：

```json
{
  "data": {
    "jobId": "uuid",
    "job": { "status": "queued", "progress": 0 },
    "reused": false
  }
}
```

客户端可轮询 `/jobs/{id}` 或订阅 `/events/stream`。同一资源的重复外部写请求应携带稳定 `Idempotency-Key`。

线路发布、自动切换和回滚共享 `route:<route_id>` 执行锁。作业列表的 `waitingForLock`、`lockOwnerJobId` 和 `executionOrder` 用于展示锁占用与排队顺序。SaaS 自动 DNS 失败会以失败状态结束作业并把对象设为 `dns_automation_paused`，只有管理员调用恢复接口后才会重新排队。

Agent 新协议使用生成代码实现的 `agent.v1.AgentServiceV1`，包括 typed `Enroll`、`ClientEnvelope`、`ServerEnvelope`、任务 ACK、结果批次和凭据轮换 ACK。rc.2 的 Struct `AgentService` 仍同时注册，旧 Agent 会被标记为降级兼容模式。

完整 REST 契约见 [`../api/openapi.yaml`](../api/openapi.yaml)，Agent 契约见 [`../proto/agent/v1/agent.proto`](../proto/agent/v1/agent.proto)。
