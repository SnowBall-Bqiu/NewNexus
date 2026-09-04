# 故障排查

## 首次管理员创建返回 400/422

- 用户名至少 3 个字符，密码至少 12 个字符。
- 确认请求 `Content-Type: application/json`，字段仅为 `username`、`password`。
- 若数据库已存在管理员，使用登录页，不要重复调用 `/auth/setup`。
- 反向代理必须原样转发 Cookie 和 `X-CSRF-Token`；登录后的浏览器写请求缺少 CSRF 会返回 403。

## Agent 离线

- 检查公网 gRPC 域名支持 TLS、HTTP/2 长连接和至少 4 MiB 消息。
- 注册码只有 15 分钟且只能使用一次；架构必须匹配。
- 查看 `journalctl -u newnexus-agent -n 200` 和 `/var/lib/newnexus-agent/state.json` 权限。
- rc.2 Agent 显示“降级”属于滚动升级状态；“不兼容”需要更新 Agent。

## DNS 发布失败

- 先查看作业详情和发布差异，不要手工删除现有记录。
- 检查厂商权限、真实线路代码、权重能力、根域 flattening/ALIAS 能力和同名非受管记录。
- 厂商 API 或权威 DNS 任一确认失败会恢复发布前快照；回滚失败会生成严重告警。
- 全目标故障采用 fail-stale，保持当前记录并停止自动变更。

## SaaS 长期待验证

- 所有权状态与证书状态是两个独立门槛，二者都为 `active` 才切业务 CNAME。
- 待验证主机名占用配额；在 SaaS 页面批量清理无效条目。
- 自定义源站必须是连接账户中已有、已代理的 A/AAAA/CNAME 主机名，不能是 IP 或通配符。
