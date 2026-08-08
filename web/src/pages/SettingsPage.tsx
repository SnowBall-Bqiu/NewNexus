import { useEffect, useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Bell, Database, KeyRound, Save, ShieldCheck, TimerReset } from 'lucide-react';
import { toast } from 'sonner';
import { api, list } from '../lib/api';
import type { Setting } from '../types';
import { Button, Field, PageHeader, Section } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function SettingsPage() {
  const settings = useQuery({ queryKey: ['settings'], queryFn: () => list<Setting>('/settings') });
  const [values, setValues] = useState<Record<string, string>>({});
  const queryClient = useQueryClient();
  useEffect(() => { if (settings.data) setValues(Object.fromEntries(settings.data.data.map((item) => [item.key, item.value]))); }, [settings.data]);
  const save = useMutation({ mutationFn: () => api('/settings', { method: 'PUT', body: JSON.stringify(values) }), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['settings'] }); toast.success('设置已保存'); } });
  function update(key: string, value: string) { setValues((current) => ({ ...current, [key]: value })); }
  function submit(event: FormEvent) { event.preventDefault(); save.mutate(); }
  return <>
    <PageHeader title="系统设置" description="配置探测节奏、指标保留、通知渠道和实例安全。" actions={<Button variant="primary" form="settings-form" type="submit" disabled={save.isPending}><Save size={16} /> {save.isPending ? '正在保存' : '保存设置'}</Button>} />
    <QueryState loading={settings.isLoading} error={settings.error}><form id="settings-form" onSubmit={submit} className="settings-layout">
      <Section title="探测调度" description="全量、榜单和活动目标使用不同频率，避免候选数与节点数相乘。"><div className="settings-section-icon"><TimerReset /></div><div className="settings-fields"><Field label="全量发现频率"><input value={values['probe.discovery_interval'] ?? ''} onChange={(e) => update('probe.discovery_interval', e.target.value)} /></Field><Field label="榜单复测频率"><input value={values['probe.ranking_interval'] ?? ''} onChange={(e) => update('probe.ranking_interval', e.target.value)} /></Field><Field label="活动目标频率"><input value={values['probe.active_interval'] ?? ''} onChange={(e) => update('probe.active_interval', e.target.value)} /></Field></div></Section>
      <Section title="指标保留" description="到期原始样本自动清理，聚合数据继续用于趋势与容量分析。"><div className="settings-section-icon"><Database /></div><div className="settings-fields"><Field label="原始样本（天）"><input type="number" min="1" value={values['retention.raw_days'] ?? ''} onChange={(e) => update('retention.raw_days', e.target.value)} /></Field><Field label="5 分钟聚合（天）"><input type="number" min="1" value={values['retention.five_minute_days'] ?? ''} onChange={(e) => update('retention.five_minute_days', e.target.value)} /></Field><Field label="小时聚合（天）"><input type="number" min="1" value={values['retention.hourly_days'] ?? ''} onChange={(e) => update('retention.hourly_days', e.target.value)} /></Field></div></Section>
      <Section title="通知渠道" description="Webhook 请求使用 HMAC 签名，邮件通过实例自己的 SMTP 发送。"><div className="settings-section-icon"><Bell /></div><div className="settings-fields settings-fields--wide"><Field label="Webhook URL"><input value={values['alerts.webhook_url'] ?? ''} onChange={(e) => update('alerts.webhook_url', e.target.value)} placeholder="https://hooks.example.com/newnexus" /></Field><Field label="SMTP 主机"><input value={values['alerts.smtp_host'] ?? ''} onChange={(e) => update('alerts.smtp_host', e.target.value)} placeholder="smtp.example.com:587" /></Field></div></Section>
      <Section title="管理员安全" description="生产实例应启用 TOTP，并妥善备份主密钥和恢复码。"><div className="settings-section-icon"><ShieldCheck /></div><div className="security-settings"><div><KeyRound size={16} /><span><strong>双因素认证</strong><small>当前会话已启用 TOTP 保护</small></span></div><Button>管理 TOTP</Button></div></Section>
    </form></QueryState>
  </>;
}
