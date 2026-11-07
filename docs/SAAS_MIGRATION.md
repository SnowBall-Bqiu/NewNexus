# SaaS 主机名迁移与验证

NewNexus 将主机名所有权验证和 TLS 证书验证分别展示。`status=active` 与 `ssl_status=active` 都满足后，才允许生成切流发布。创建主机名、自动写入验证记录、激活和 DNS 发布是可追踪的独立作业；创建成功不表示已经切换客户流量。

## 迁移流程

1. 同步 SaaS 区域与账户能力，确认回退源、额度、独立源站及 DNS 线路能力。不可用或未经确认的能力会阻止相应配置。
2. 单个或批量创建主机名。自动模式只在已配置并授权的 DNS 区域写入受管验证记录；手动模式按详情中的 TXT 或 HTTP 清单操作。
3. 分别完成所有权与证书验证，刷新观察状态，等待两者均为 active。HTTP 验证需要在现有业务源站提供厂商返回的路径与准确内容。NewNexus 不会通过修改现有业务源站来代替这个步骤。
4. 保持旧 CDN 仍然服务，在详情中生成迁移预览。选择接管现有 A/AAAA/CNAME 时，预览记录当前 DNS 快照，逐条显示将删除和新增的记录。
5. 审查不可变发布差异并确认应用。发布前重新检查双 active 状态、记录所有权、冲突、CNAME 循环及区域能力；发布后通过厂商 API 和权威 DNS 验证。
6. 观察新流量与证书。失败可用该发布的快照回滚；补偿作业和失败原因留在作业、告警及审计中。旧 CDN 的账户或源站清理需要由管理员在确认观察期结束后操作。

回滚恢复原有外部记录时，系统保留它们原来的所有权，不会把恢复的外部记录标为 NewNexus 受管记录。日常创建或激活不会默默覆盖现有业务记录。

## Cloudflare 限制

静态独立源站在 Cloudflare for SaaS 各套餐提供；SNI Rewrite 是单独的 Enterprise 能力，本版不推断或自动设置它。独立源站必须是同一 Cloudflare 账户中有明确已代理 A/AAAA/CNAME 记录的主机名，不能使用 IP、URL 或仅 wildcard 记录。连接凭据需要读取相关区域才能验证；无法验证时拒绝创建并显示原因。参见 [套餐能力](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/plans/) 和 [独立源站要求](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/custom-origin/)。

Cloudflare 已明确：客户主机名本身位于 Cloudflare 区域时，Orange-to-Orange（O2O）不支持 TXT/HTTP token 预验证。因此不能承诺该场景具有普通外部 CDN 迁移的同等预验证流程；以账户实际能力、Cloudflare 返回的验证步骤与状态为准。参见 [Cloudflare 预验证限制](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/domain-support/hostname-validation/pre-validation/)。

普通迁移也需要预先签发证书。所有权 token 与 `ssl.validation_records` 是两套字段，完成前者不会自动完成后者。NewNexus 的双 active 门槛遵循 [Cloudflare 零停机迁移流程](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/domain-support/hostname-validation/zero-downtime-migration/)。DNS 缓存、旧 CDN 的兼容性与客户源站响应仍需在专用测试域名上实际验证。

## 删除与清理

批量删除使用逐项作业和幂等键，返回每项成功或错误。清理前核对受管记录及影响范围，大范围操作需持续按住确认。系统只自动清理能证明归属于当前资源的受管记录；无法确定所有权的 DNS 保留并给出错误供管理员处理。
