export const statusLabels: Record<string, string> = {
  healthy: '健康', connected: '已连接', active: '已生效', online: '在线', applied: '已发布', synced: '已同步', succeeded: '已完成', resolved: '已恢复',
  warning: '注意', degraded: '退化', pending: '等待中', pending_validation: '待验证', queued: '已排队', running: '执行中', checking: '检查中',
  rotation_pending: '轮换待确认', re_enroll_required: '需要重新注册', revoked: '已撤销', offline: '离线', failed: '失败', error: '异常', open: '处理中',
  acknowledged: '已确认', preview: '仅预览', rollback_unverified: '回滚未确认', dns_automation_paused: '自动化已暂停', disabled: '已停用',
};
const success = new Set(['healthy', 'connected', 'active', 'online', 'applied', 'synced', 'succeeded', 'resolved']);
const warning = new Set(['warning', 'degraded', 'pending', 'pending_validation', 'queued', 'running', 'checking', 'rotation_pending', 're_enroll_required', 'preview']);
const danger = new Set(['revoked', 'offline', 'failed', 'error', 'open', 'rollback_unverified', 'dns_automation_paused']);
export function statusVariant(status: string): 'success' | 'warning' | 'error' | 'neutral' {
  return success.has(status) ? 'success' : warning.has(status) ? 'warning' : danger.has(status) ? 'error' : 'neutral';
}
