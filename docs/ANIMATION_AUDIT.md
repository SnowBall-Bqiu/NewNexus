# 动效机会扫描记录

本轮在主界面完成后执行只读扫描。保留的高价值动效为：Kumo 按钮按压反馈、Dialog 进入/退出与焦点恢复、Tooltip 来源点反馈、移动 Sidebar 与遮罩、Kumo Toasty 可中断 Toast。应用自定义项目只改变 `transform`/`opacity`，并在 `prefers-reduced-motion` 下去除位移和缩放。

明确拒绝的候选：主导航页面切换、表格筛选/排序/分页、候选虚拟列表 stagger、健康状态刷新、SSE 作业进度、数字跳动、ECharts 数据 tween、告警级别颜色过渡。它们频率高或承载功能数据，动画会降低扫描效率或制造状态延迟。

首版没有出现需要弹簧、共享布局或手势跟随的交互，因此没有引入 Motion。静态脚本阻止 `transition: all`、独立 `ease-in`、`scale(0)` 和缺少 reduced-motion 分支。
