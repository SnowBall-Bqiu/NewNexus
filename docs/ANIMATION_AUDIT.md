# 动效机会扫描与实现复核

按本地 `.agent/skills/find-animation-opportunities/SKILL.md` 进行只读扫描，候选交回 `animate` 规则实现。这里只记录与运维操作有关的反馈和空间关系。

| 位置 | 当前行为 | 目的 | 频率 | 保留的动效 |
| --- | --- | --- | --- | --- |
| `web/src/components/ui/dialog.tsx` | 发布及破坏操作确认 | 空间关系、消除突变 | 偶发 | opacity + scale .96，250ms `--ease-out`；原语维护退出和焦点；reduced-motion 仅125ms透明度 |
| `web/src/components/ui/sheet.tsx` | 资源详情与编辑抽屉 | 空间关系 | 偶发 | 从对应边缘平移，200ms `--ease-drawer`；reduced-motion移除平移 |
| `web/src/components/ui/dropdown-menu.tsx`、`select.tsx` | 与触发器关联的浮层 | 空间关系 | 每日数十次 | 触发点origin，opacity + scale .95，200ms `--ease-out`；reduced-motion仅透明度 |
| `web/src/index.css` 普通按钮 | 确认收到按压 | 反馈 | 每日数十次 | pointer按压scale .97，160ms `--ease-out`；键盘focus-visible与reduced-motion移除缩放 |
| `web/src/pages/resource-page.tsx` HoldButton | 批量SaaS清理 | 反馈、防止误触 | 低频高风险 | 2秒线性transform进度，松开/Escape/失焦取消；reduced-motion以静态状态文字代替进度移动 |
| Sonner | 偶发操作完成或失败 | 空间关系、状态反馈 | 偶发 | 同一边缘进入/退出，250ms opacity/transform；reduced-motion仅125ms透明度 |

拒绝的候选：

- 主导航与分页：高频操作，不做页面切换或stagger。
- 候选排序、筛选和新结果到达：功能数据需要即时可读，不做重排或数字弹跳。
- Recharts历史曲线：用于读取指标，所有数据补间关闭。
- 节点健康/告警颜色变化：需要即时反映状态，不插入渐变。
- 首次安装庆祝、背景漂移或粒子：对配置和监控没有功能收益。

结论：界面所需动效集中在原语与低频危险确认，无需引入 Motion。静态检查和 Playwright 覆盖规范违规、正常/减弱动效、菜单位置与退出焦点。逐帧视觉观察不能由静态规则替代；浏览器测试截图与trace保存在测试产物中。

补充浏览器检查使用 Chrome DevTools Protocol `Animation.setPlaybackRate=0.2`，将弹窗动画减慢到五倍，采集四个画面及实际 scale/opacity/transition 值；菜单来源点以未缩放的元素尺寸核对，避免把过渡期间的 boundingClientRect 当作布局尺寸。该检查另在 reduced-motion 下确认只保留透明度。最终执行结果见验收记录，不将自动采样表述为人工逐帧检查。
