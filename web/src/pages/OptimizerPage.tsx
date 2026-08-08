import { useMemo, useState, type CSSProperties, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Virtuoso } from 'react-virtuoso';
import { Activity, Filter, Gauge, Globe2, Play, Plus, RefreshCw, Search, Send, SlidersHorizontal, Wifi } from 'lucide-react';
import { toast } from 'sonner';
import { api, formatBytesPerSecond, formatDate, formatNumber, jsonBody, list } from '../lib/api';
import type { Candidate, CandidateSource, Connection, OptimizedHostname, OptimizedHostnamePreview, ProbeProfile, Project } from '../types';
import { Badge, Button, DialogShell, Field, InlineCode, PageHeader, Section, Segmented } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function OptimizerPage() {
  const [tier, setTier] = useState('all');
  const [search, setSearch] = useState('');
  const [createHostnameOpen, setCreateHostnameOpen] = useState(false);
  const [previewHostname, setPreviewHostname] = useState<OptimizedHostname>();
  const queryClient = useQueryClient();
  const candidates = useQuery({ queryKey: ['candidates'], queryFn: () => list<Candidate>('/optimizer/candidates?limit=500') });
  const sources = useQuery({ queryKey: ['sources'], queryFn: () => list<CandidateSource>('/optimizer/sources') });
  const profiles = useQuery({ queryKey: ['probe-profiles'], queryFn: () => list<ProbeProfile>('/probe/profiles') });
  const hostnames = useQuery({ queryKey: ['optimized-hostnames'], queryFn: () => list<OptimizedHostname>('/optimizer/hostnames') });
  const sync = useMutation({ mutationFn: (id: string) => api<{ candidateCount: number }>(`/optimizer/sources/${id}/sync?samplePerPrefix=32`, { method: 'POST' }), onSuccess: (result) => { queryClient.invalidateQueries({ queryKey: ['candidates'] }); queryClient.invalidateQueries({ queryKey: ['sources'] }); toast.success(`已同步 ${result.candidateCount} 个候选地址`); }, onError: (error) => toast.error(error.message) });
  const filtered = useMemo(() => (candidates.data?.data ?? []).filter((item) => (tier === 'all' || item.tier === tier) && (!search || item.address.includes(search) || item.tags.includes(search))), [candidates.data, tier, search]);
  const profile = profiles.data?.data[0];
  return <>
    <PageHeader title="Cloudflare 优选" description="从官方网段和导入源发现候选，以线路视角持续探测并生成稳定榜单。" actions={<><Button onClick={() => setCreateHostnameOpen(true)}><Plus size={16} /> 新建优选域名</Button><Button variant="primary" onClick={() => sources.data?.data[0] && sync.mutate(sources.data.data[0].id)} disabled={sync.isPending}><RefreshCw size={16} className={sync.isPending ? 'spin' : ''} /> {sync.isPending ? '正在同步' : '同步官方网段'}</Button></>} />
    <div className="optimizer-overview">
      <div className="optimizer-score"><div><span>最佳综合评分</span><strong>{candidates.data?.data[0]?.score.toFixed(1) ?? '-'}</strong><small>/ 100</small></div><div className="score-ring" style={{ '--score': `${candidates.data?.data[0]?.score ?? 0}%` } as CSSProperties}><Gauge size={23} /></div></div>
      <div className="optimizer-kpi"><Activity size={17} /><div><span>活动候选</span><strong>{formatNumber((candidates.data?.data ?? []).filter((item) => item.tier === 'active').length)}</strong></div></div>
      <div className="optimizer-kpi"><Wifi size={17} /><div><span>平均成功率</span><strong>{average(candidates.data?.data.map((item) => item.successRate) ?? []).toFixed(2)}%</strong></div></div>
      <div className="optimizer-kpi"><Play size={17} /><div><span>活动探测周期</span><strong>{profile?.intervalSeconds ?? 30}s</strong></div></div>
    </div>
    <Section title="全局 HTTPS 探测模板" description="CF 优选候选连接到目标 IP，但保留测试 URL 的 Host 与 TLS SNI。" actions={<Button size="sm"><SlidersHorizontal size={15} /> 编辑模板</Button>}>
      <div className="probe-profile-row"><div><span>测试 URL</span><code>{profile?.url ?? '尚未配置'}</code></div><div><span>协议</span><strong>{profile?.mode === 'all' ? 'TCP + HTTP' : profile?.mode?.toUpperCase()}</strong></div><div><span>超时</span><strong>{profile?.timeoutMs ?? 5000} ms</strong></div><div><span>最大读取</span><strong>{formatNumber(profile?.maxBytes ?? 0)} B</strong></div><div><span>User-Agent</span><strong>Firefox</strong></div></div>
    </Section>
    <Section title="受管优选域名" description="将榜单前 N 个健康地址物化为 A/AAAA；Cloudflare DNS 始终以 DNS-only 发布。"><div className="optimized-hostname-list">{hostnames.data?.data.map((item) => <div className="optimized-hostname-row" key={item.id}><span className="optimized-hostname-icon"><Globe2 size={16} /></span><div><strong>{item.hostname}</strong><span>{item.zoneName} · {item.family === 6 ? 'IPv6' : 'IPv4'}</span></div><span><b>前 {item.topN}</b><small>{item.family === 6 ? 'AAAA' : 'A'} · TTL {item.ttl}s</small></span><Badge status={item.status} /><Button size="sm" onClick={() => setPreviewHostname(item)}><Send size={14} /> 预览发布</Button></div>)}</div></Section>
    <Section title="候选排行榜" description={`当前展示 ${filtered.length} 条，生产模式使用虚拟列表承载十万级库存。`} actions={<div className="table-tools"><label className="search-box"><Search size={15} /><input value={search} onChange={(e) => setSearch(e.target.value)} placeholder="搜索 IP 或标签" /></label><Button size="icon" title="筛选"><Filter size={16} /></Button></div>}>
      <Segmented label="候选层级" value={tier} onChange={setTier} options={[{ value: 'all', label: '全部' }, { value: 'active', label: '活动' }, { value: 'ranking', label: '榜单' }, { value: 'inventory', label: '库存' }]} />
      <QueryState loading={candidates.isLoading} error={candidates.error}><div className="candidate-table"><div className="candidate-row candidate-row--header"><span>排名 / 地址</span><span>层级</span><span>综合评分</span><span>成功率</span><span>TCP</span><span>TLS</span><span>TTFB</span><span>吞吐</span><span>最近探测</span></div><Virtuoso style={{ height: Math.min(480, Math.max(240, filtered.length * 52)) }} data={filtered} itemContent={(index, item) => <div className="candidate-row"><span><b>{String(index + 1).padStart(2, '0')}</b><code>{item.address}</code><small>{item.tags}</small></span><span><Badge status={item.tier === 'active' ? 'active' : item.tier === 'ranking' ? 'connected' : 'unknown'}>{item.tier === 'active' ? '活动' : item.tier === 'ranking' ? '榜单' : '库存'}</Badge></span><span><strong className="score-value">{item.score.toFixed(1)}</strong></span><span>{item.successRate.toFixed(2)}%</span><span>{item.tcpMs.toFixed(1)} ms</span><span>{item.tlsMs.toFixed(1)} ms</span><span>{item.ttfbMs.toFixed(1)} ms</span><span>{formatBytesPerSecond(item.downloadBps)}</span><span>{formatDate(item.lastProbedAt)}</span></div>} /></div></QueryState>
    </Section>
    <Section title="候选来源" description="来源只负责发现；进入候选池后统一经过安全过滤与三级探测。"><div className="source-list">{sources.data?.data.map((source) => <div className="source-row" key={source.id}><span className="source-icon"><RefreshCw size={16} /></span><div><strong>{source.name}</strong><span>{source.type === 'cloudflare_official' ? 'Cloudflare API' : source.location}</span></div><Badge status={source.status} /><span>{formatNumber(source.candidateCount)} 个候选</span><time>{formatDate(source.lastSyncedAt)}</time></div>)}</div></Section>
    <CreateOptimizedHostnameDialog open={createHostnameOpen} onOpenChange={setCreateHostnameOpen} />
    {previewHostname && <OptimizedHostnamePreviewDialog hostname={previewHostname} open={Boolean(previewHostname)} onOpenChange={(open) => !open && setPreviewHostname(undefined)} />}
  </>;
}

function CreateOptimizedHostnameDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => list<Project>('/projects') });
  const connections = useQuery({ queryKey: ['connections'], queryFn: () => list<Connection>('/connections') });
  const [projectId, setProjectId] = useState(''); const [connectionId, setConnectionId] = useState(''); const [zoneName, setZoneName] = useState(''); const [zoneId, setZoneId] = useState(''); const [hostname, setHostname] = useState(''); const [family, setFamily] = useState('4'); const [topN, setTopN] = useState(2);
  const selectedProject = projectId || projects.data?.data[0]?.id || '';
  const dnsConnections = connections.data?.data.filter((item) => item.purposes.includes('dns')) ?? [];
  const selectedConnection = connectionId || dnsConnections[0]?.id || '';
  const create = useMutation({ mutationFn: () => api('/optimizer/hostnames', jsonBody({ projectId: selectedProject, dnsConnectionId: selectedConnection, zoneId, zoneName, hostname, family: Number(family), topN, ttl: 60 })), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['optimized-hostnames'] }); onOpenChange(false); setHostname(''); toast.success('优选域名 desired 状态已创建'); }, onError: (error) => toast.error(error.message) });
  function submit(event: FormEvent) { event.preventDefault(); create.mutate(); }
  return <DialogShell open={open} onOpenChange={onOpenChange} title="新建受管优选域名" description="选择榜单前 N 个健康地址生成 A 或 AAAA 记录，发布前仍会展示逐条差异。" size="lg" footer={<><Button onClick={() => onOpenChange(false)}>取消</Button><Button variant="primary" type="submit" form="optimized-hostname-form" disabled={!selectedProject || !selectedConnection || !zoneName || !zoneId || !hostname || create.isPending}>{create.isPending ? '正在创建…' : '创建 desired 状态'}</Button></>}><form id="optimized-hostname-form" className="form-grid form-grid--two" onSubmit={submit}><Field label="项目" required><select value={selectedProject} onChange={(e) => setProjectId(e.target.value)}>{projects.data?.data.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field><Field label="DNS 连接" required><select value={selectedConnection} onChange={(e) => setConnectionId(e.target.value)}>{dnsConnections.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field><Field label="DNS 区域" required><input value={zoneName} onChange={(e) => setZoneName(e.target.value.toLowerCase())} placeholder="example.com" /></Field><Field label="厂商区域 ID" required><input value={zoneId} onChange={(e) => setZoneId(e.target.value)} placeholder="Cloudflare Zone ID" /></Field><Field label="优选子域名" required><input value={hostname} onChange={(e) => setHostname(e.target.value.toLowerCase())} placeholder="edge.example.com" /></Field><Field label="地址族"><Segmented label="优选地址族" value={family} onChange={setFamily} options={[{ value: '4', label: 'IPv4 / A' }, { value: '6', label: 'IPv6 / AAAA' }]} /></Field><Field label="榜单地址数量"><input type="number" min={1} max={20} value={topN} onChange={(e) => setTopN(Number(e.target.value))} /></Field><div className="form-notice"><Globe2 size={15} /><span>Cloudflare 适配器强制写入 <b>proxied=false</b>；没有健康候选时保持现有记录。</span></div></form></DialogShell>;
}

function OptimizedHostnamePreviewDialog({ hostname, open, onOpenChange }: { hostname: OptimizedHostname; open: boolean; onOpenChange: (open: boolean) => void }) {
  const queryClient = useQueryClient();
  const preview = useQuery({ queryKey: ['optimized-hostname-preview', hostname.id], queryFn: () => api<OptimizedHostnamePreview>(`/optimizer/hostnames/${hostname.id}/preview`, { method: 'POST' }), enabled: open, retry: false });
  const publish = useMutation({ mutationFn: () => api(`/optimizer/hostnames/${hostname.id}/publish`, { method: 'POST' }), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['optimized-hostnames'] }); onOpenChange(false); toast.success('优选 A/AAAA 已发布并回读确认'); }, onError: (error) => toast.error(error.message) });
  return <DialogShell open={open} onOpenChange={onOpenChange} title={`发布 ${hostname.hostname}`} description="地址来自当前健康榜单；变更后会从厂商 API 回读确认。" footer={<><Button onClick={() => onOpenChange(false)}>取消</Button><Button variant="primary" onClick={() => publish.mutate()} disabled={!preview.data?.canApply || publish.isPending}><Send size={14} /> {publish.isPending ? '正在发布…' : '确认发布'}</Button></>}><QueryState loading={preview.isLoading} error={preview.error}>{preview.data && <div className="hostname-preview"><div className="preview-summary"><span className={`preview-summary__icon ${preview.data.canApply ? 'is-ready' : ''}`}><Globe2 /></span><div><strong>{preview.data.recordType} · 前 {preview.data.addresses.length}</strong><p>{preview.data.hostname}</p></div><Badge status={preview.data.canApply ? 'healthy' : 'preview'} /></div>{preview.data.addresses.map((address) => <InlineCode key={address}>{address}</InlineCode>)}{preview.data.warnings.map((warning) => <div className="warning-callout" key={warning}>{warning}</div>)}</div>}</QueryState></DialogShell>;
}

function average(values: number[]) { return values.length ? values.reduce((total, value) => total + value, 0) / values.length : 0; }
