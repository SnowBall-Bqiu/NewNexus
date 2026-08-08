import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { BellRing, Check, CircleAlert, Info, TriangleAlert } from 'lucide-react';
import { api, formatDate, list } from '../lib/api';
import type { Alert } from '../types';
import { Badge, Button, PageHeader, Section } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function AlertsPage() {
  const alerts = useQuery({ queryKey: ['alerts'], queryFn: () => list<Alert>('/alerts') });
  const queryClient = useQueryClient();
  const acknowledge = useMutation({ mutationFn: (id: string) => api(`/alerts/${id}/acknowledge`, { method: 'POST' }), onSuccess: () => queryClient.invalidateQueries({ queryKey: ['alerts'] }) });
  return <>
    <PageHeader title="告警" description="聚合线路、SaaS、Agent、厂商凭据和发布失败事件。" />
    <div className="alert-summary"><div><BellRing size={18} /><span>未确认</span><strong>{alerts.data?.data.filter((item) => item.status === 'open').length ?? 0}</strong></div><div><TriangleAlert size={18} /><span>警告</span><strong>{alerts.data?.data.filter((item) => item.severity === 'warning').length ?? 0}</strong></div><div><CircleAlert size={18} /><span>严重</span><strong>{alerts.data?.data.filter((item) => item.severity === 'critical').length ?? 0}</strong></div></div>
    <Section title="事件清单" description="确认只表示管理员已经看到，事件状态仍由对应资源决定。"><QueryState loading={alerts.isLoading} error={alerts.error}><div className="alerts-list">{alerts.data?.data.map((alert) => <article className={`alert-entry alert-entry--${alert.severity}`} key={alert.id}><span>{alert.severity === 'critical' ? <CircleAlert /> : alert.severity === 'warning' ? <TriangleAlert /> : <Info />}</span><div><div><strong>{alert.title}</strong><Badge status={alert.status} /></div><p>{alert.message}</p><small>{alert.category} · {formatDate(alert.createdAt)}</small></div>{alert.status === 'open' && <Button size="sm" onClick={() => acknowledge.mutate(alert.id)}><Check size={14} /> 确认</Button>}</article>)}</div></QueryState></Section>
  </>;
}
