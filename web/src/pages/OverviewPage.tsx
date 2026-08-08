import { useQuery } from '@tanstack/react-query';
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { ArrowRight, CheckCircle2, Clock3, GitBranch, RadioTower, ShieldCheck, TriangleAlert } from 'lucide-react';
import { Link } from 'react-router-dom';
import { list, api, formatDate, formatNumber } from '../lib/api';
import type { Alert, Release, Route, Summary } from '../types';
import { Badge, PageHeader, Section, Stat } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function OverviewPage() {
  const summary = useQuery({ queryKey: ['summary'], queryFn: () => api<Summary>('/summary'), refetchInterval: 30_000 });
  const routes = useQuery({ queryKey: ['routes'], queryFn: () => list<Route>('/routes') });
  const alerts = useQuery({ queryKey: ['alerts', 'open'], queryFn: () => list<Alert>('/alerts?status=open') });
  const releases = useQuery({ queryKey: ['releases'], queryFn: () => list<Release>('/releases') });
  const data = summary.data;
  return <>
    <PageHeader title="网络运行总览" description="聚合查看探测、线路、Cloudflare SaaS 和发布状态。" actions={<span className="last-updated"><Clock3 size={14} /> {data ? formatDate(data.updatedAt) : '正在同步'}</span>} />
    <QueryState loading={summary.isLoading} error={summary.error}>
      {data && <div className="stats-band">
        <Stat label="健康线路" value={`${data.healthyRoutes}/${data.counts.routes ?? 0}`} detail={<><CheckCircle2 size={14} /> 全局策略</>} tone="green" />
        <Stat label="在线探测节点" value={`${data.onlineNodes}/${data.counts.nodes ?? 0}`} detail={<><RadioTower size={14} /> gRPC 双向流</>} tone="blue" />
        <Stat label="优选候选库存" value={formatNumber(data.counts.candidates ?? 0)} detail={<><GitBranch size={14} /> 三级自适应探测</>} tone="cyan" />
        <Stat label="待处理告警" value={data.counts.alerts ?? 0} detail={<><TriangleAlert size={14} /> 需关注</>} tone={(data.counts.alerts ?? 0) > 0 ? 'amber' : 'green'} />
      </div>}
    </QueryState>
    <div className="overview-grid">
      <Section title="边缘质量趋势" description="过去 24 小时活动候选的综合探测结果。" className="overview-chart">
        <div className="chart-legend"><span><i className="legend-swatch legend-swatch--green" />中位延迟</span><strong>{data?.latencySeries.at(-1)?.latency ?? '-'} ms</strong></div>
        <div className="chart-wrap">{data && <ResponsiveContainer width="100%" height="100%"><AreaChart data={data.latencySeries} margin={{ top: 10, right: 12, left: -20, bottom: 0 }}><defs><linearGradient id="latencyFill" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor="#229a62" stopOpacity={0.22} /><stop offset="100%" stopColor="#229a62" stopOpacity={0} /></linearGradient></defs><CartesianGrid vertical={false} stroke="#e7e9ed" /><XAxis dataKey="time" tickLine={false} axisLine={false} tick={{ fontSize: 11, fill: '#77808f' }} interval={5} /><YAxis tickLine={false} axisLine={false} tick={{ fontSize: 11, fill: '#77808f' }} /><Tooltip contentStyle={{ borderRadius: 6, border: '1px solid #dfe2e7', boxShadow: '0 8px 24px rgba(20,27,40,.1)', fontSize: 13 }} animationDuration={0} /><Area type="monotone" dataKey="latency" stroke="#16794b" strokeWidth={2} fill="url(#latencyFill)" isAnimationActive={false} /></AreaChart></ResponsiveContainer>}</div>
      </Section>
      <Section title="当前告警" description="未确认且仍需处理的事件。" actions={<Link className="text-link" to="/alerts">查看全部 <ArrowRight size={14} /></Link>} className="alerts-panel">
        <QueryState loading={alerts.isLoading} error={alerts.error}>{alerts.data?.data.slice(0, 3).map((alert) => <div className="alert-row" key={alert.id}><span className={`alert-icon alert-icon--${alert.severity}`}>{alert.severity === 'warning' ? <TriangleAlert size={16} /> : <ShieldCheck size={16} />}</span><div><strong>{alert.title}</strong><p>{alert.message}</p><time>{formatDate(alert.createdAt)}</time></div></div>)}</QueryState>
      </Section>
    </div>
    <Section title="线路运行矩阵" description="业务域名的发布模式、健康状态与最近变更。" actions={<Link className="text-link" to="/routes">管理线路 <ArrowRight size={14} /></Link>}>
      <div className="data-table-wrap"><table className="data-table"><thead><tr><th>规则</th><th>业务域名</th><th>切换模式</th><th>发布方式</th><th>TTL</th><th>状态</th></tr></thead><tbody>{routes.data?.data.map((route) => <tr key={route.id}><td><strong>{route.name}</strong></td><td><code>{route.hostname}</code></td><td>{switchModeLabel(route.switchMode)}</td><td>{route.publishMode === 'automatic' ? '自动发布' : '手动确认'}</td><td>{route.ttl}s</td><td><Badge status={route.status} /></td></tr>)}</tbody></table></div>
    </Section>
    <Section title="最近发布" description="所有线路变更都有版本、触发来源和回滚快照。">
      <div className="timeline-list">{releases.data?.data.slice(0, 4).map((release) => <div className="timeline-item" key={release.id}><span className={`timeline-dot timeline-dot--${release.status}`} /><div><strong>发布 #{release.version}</strong><span>{release.trigger === 'health' ? '健康仲裁触发' : release.trigger === 'manual' ? '管理员手动发布' : '回滚'}</span></div><Badge status={release.status} /><time>{formatDate(release.appliedAt ?? release.createdAt)}</time></div>)}</div>
    </Section>
  </>;
}

function switchModeLabel(mode: string) { return ({ availability: '仅可用性', availability_performance: '可用性 + 性能', score: '持续综合排名' } as Record<string, string>)[mode] ?? mode; }
