import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Check, Cloud, Copy, KeyRound, Plus, RefreshCw, ShieldCheck } from 'lucide-react';
import { toast } from 'sonner';
import { api, formatDate, jsonBody, list } from '../lib/api';
import type { Connection, CustomHostname, SaaSZone } from '../types';
import { Badge, Button, DialogShell, Field, IconButton, InlineCode, PageHeader, Section, Segmented } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function SaaSPage() {
  const zones = useQuery({ queryKey: ['saas-zones'], queryFn: () => list<SaaSZone>('/saas/zones') });
  const hostnames = useQuery({ queryKey: ['saas-hostnames'], queryFn: () => list<CustomHostname>('/saas/hostnames') });
  const connections = useQuery({ queryKey: ['connections'], queryFn: () => list<Connection>('/connections') });
  const [open, setOpen] = useState(false);
  const [hostname, setHostname] = useState('');
  const [automationMode, setAutomationMode] = useState('automatic');
  const [dnsConnectionId, setDNSConnectionId] = useState('');
  const [dnsZoneId, setDNSZoneId] = useState('');
  const [dnsZoneName, setDNSZoneName] = useState('');
  const queryClient = useQueryClient();
  const currentZone = zones.data?.data[0];
  const dnsConnections = connections.data?.data.filter((connection) => connection.provider === 'cloudflare' || connection.provider === 'dnspod' || connection.provider === 'alidns') ?? [];
  const selectedDNSConnection = dnsConnectionId || dnsConnections[0]?.id || '';
  const automaticReady = automationMode === 'manual' || Boolean(selectedDNSConnection && dnsZoneId && dnsZoneName);
  const create = useMutation({
    mutationFn: () => api<CustomHostname>('/saas/hostnames', jsonBody({ saasZoneId: currentZone?.id, hostname, validationMethod: 'txt', automationMode, dnsConnectionId: automationMode === 'automatic' ? selectedDNSConnection : '', dnsZoneId: automationMode === 'automatic' ? dnsZoneId : '', dnsZoneName: automationMode === 'automatic' ? dnsZoneName : '' })),
    onSuccess: (item) => { queryClient.invalidateQueries({ queryKey: ['saas-hostnames'] }); setOpen(false); setHostname(''); toast.success(item.errorMessage ? '主机名已创建，DNS 自动化等待处理' : '自定义主机名已提交'); },
    onError: (error) => toast.error(error instanceof Error ? error.message : '创建失败'),
  });
  const sync = useMutation({
    mutationFn: (id: string) => api<CustomHostname>(`/saas/hostnames/${id}/sync`, jsonBody({})),
    onSuccess: (item) => { queryClient.invalidateQueries({ queryKey: ['saas-hostnames'] }); item.errorMessage ? toast.warning('状态已同步，DNS 自动化仍需处理') : toast.success('Cloudflare 与 DNS 状态已同步'); },
    onError: (error) => toast.error(error instanceof Error ? error.message : '同步失败'),
  });
  function submit(event: FormEvent) { event.preventDefault(); create.mutate(); }
  const quotaPercent = currentZone ? Math.min(100, currentZone.quotaUsed / Math.max(1, currentZone.quotaAllocated) * 100) : 0;
  return <>
    <PageHeader title="Cloudflare SaaS" description="管理回退源、自定义主机名、域名验证和边缘证书状态。" actions={<Button variant="primary" onClick={() => setOpen(true)} disabled={!currentZone}><Plus size={16} /> 添加主机名</Button>} />
    <QueryState loading={zones.isLoading} error={zones.error}>{currentZone && <div className="saas-zone-band"><div className="saas-zone-band__identity"><span className="provider-logo provider-logo--cloudflare"><Cloud size={20} /></span><div><span>当前 SaaS 区域</span><h2>{currentZone.zoneName}</h2><Badge status={currentZone.status} /></div></div><div className="saas-zone-band__origin"><span>回退源</span><InlineCode>{currentZone.fallbackOrigin}</InlineCode><span>CNAME 目标</span><InlineCode>{currentZone.cnameTarget}</InlineCode></div><div className="quota"><div><span>主机名额度</span><strong>{currentZone.quotaUsed} / {currentZone.quotaAllocated}</strong></div><div className="quota__track"><span style={{ width: `${quotaPercent}%` }} /></div><small>硬上限 {currentZone.quotaHardCap}</small></div></div>}</QueryState>
    <Section title="自定义主机名" description="只有主机名状态和证书状态同时生效后，才允许自动切流。">
      <QueryState loading={hostnames.isLoading} error={hostnames.error}><div className="data-table-wrap"><table className="data-table"><thead><tr><th>主机名</th><th>接入方式</th><th>主机名状态</th><th>证书状态</th><th>验证方式</th><th>最近检查</th><th aria-label="操作" /></tr></thead><tbody>{hostnames.data?.data.map((host) => <tr key={host.id}><td><div className="hostname-cell"><strong className="domain-cell"><ShieldCheck size={15} />{host.hostname}</strong>{host.errorMessage && <small title={host.errorMessage}>{host.errorMessage}</small>}</div></td><td>{host.automationMode === 'automatic' ? '自动 DNS' : '手动 DNS'}</td><td><Badge status={host.status} /></td><td><Badge status={host.sslStatus} /></td><td><span className="method-label"><KeyRound size={14} /> {host.validationMethod.toUpperCase()}</span></td><td>{formatDate(host.lastCheckedAt)}</td><td><IconButton label="同步 Cloudflare 与 DNS 状态" onClick={() => sync.mutate(host.id)} disabled={sync.isPending && sync.variables === host.id}><RefreshCw size={15} className={sync.isPending && sync.variables === host.id ? 'spin' : undefined} /></IconButton></td></tr>)}</tbody></table></div></QueryState>
    </Section>
    {hostnames.data?.data.some((host) => host.validationName) && <Section title="待完成验证" description="外部 DNS 模式下，将下面的 TXT 记录添加到权威 DNS。"><div className="validation-list">{hostnames.data.data.filter((host) => host.validationName).map((host) => <div className="validation-item" key={host.id}><div><Badge status="pending_validation" /><strong>{host.hostname}</strong></div><div><span>记录名</span><InlineCode>{host.validationName}</InlineCode><IconButton label="复制记录名" onClick={() => navigator.clipboard.writeText(host.validationName)}><Copy size={15} /></IconButton></div><div><span>记录值</span><InlineCode>{host.validationValue}</InlineCode><IconButton label="复制记录值" onClick={() => navigator.clipboard.writeText(host.validationValue)}><Copy size={15} /></IconButton></div></div>)}</div></Section>}
    <DialogShell open={open} onOpenChange={setOpen} title="添加自定义主机名" description={`将主机名添加到 ${currentZone?.zoneName ?? 'SaaS 区域'} 并预申请边缘证书。`} size={automationMode === 'automatic' ? 'lg' : undefined} footer={<><Button onClick={() => setOpen(false)}>取消</Button><Button variant="primary" form="hostname-form" type="submit" disabled={!hostname || !automaticReady || create.isPending}>{create.isPending ? '正在提交…' : '添加主机名'}</Button></>}><form id="hostname-form" onSubmit={submit} className={`form-grid ${automationMode === 'automatic' ? 'form-grid--two' : ''}`}><Field label="业务主机名" hint="填写最终由访客访问的完整域名" required><input autoFocus value={hostname} onChange={(e) => setHostname(e.target.value.toLowerCase())} placeholder="www.customer.com" /></Field><Field label="DNS 验证与指向"><Segmented label="DNS 自动化方式" value={automationMode} onChange={setAutomationMode} options={[{ value: 'automatic', label: '自动写入' }, { value: 'manual', label: '生成操作清单' }]} /></Field>{automationMode === 'automatic' && <><Field label="权威 DNS 连接" required><select value={selectedDNSConnection} onChange={(e) => setDNSConnectionId(e.target.value)}><option value="" disabled>选择连接</option>{dnsConnections.map((connection) => <option key={connection.id} value={connection.id}>{connection.name} · {connection.provider}</option>)}</select></Field><Field label="DNS 区域" hint="例如 customer.com" required><input value={dnsZoneName} onChange={(e) => setDNSZoneName(e.target.value.toLowerCase())} placeholder="customer.com" /></Field><Field label="厂商区域 ID" hint="Cloudflare Zone ID；DNSPod/阿里云可填写区域名" required><input value={dnsZoneId} onChange={(e) => setDNSZoneId(e.target.value)} placeholder="Zone ID 或区域名" /></Field></>}<div className={`form-notice ${automationMode === 'automatic' ? 'form-notice--wide' : ''}`}><Check size={16} /><span>默认先写 TXT 预验证；只有主机名和证书均为 active 后才发布业务 CNAME。</span></div></form></DialogShell>
  </>;
}
