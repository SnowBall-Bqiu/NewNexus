import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Cable, CheckCircle2, Cloud, KeyRound, Plus, RefreshCw, ShieldCheck, TriangleAlert } from 'lucide-react';
import { toast } from 'sonner';
import { api, formatDate, jsonBody, list } from '../lib/api';
import type { Connection } from '../types';
import { Badge, Button, DialogShell, Field, PageHeader, Section, Segmented } from '../components/ui';
import { QueryState } from '../components/QueryState';

const providerNames = { cloudflare: 'Cloudflare', dnspod: 'DNSPod', alidns: '阿里云 DNS' };

export function ConnectionsPage() {
  const connections = useQuery({ queryKey: ['connections'], queryFn: () => list<Connection>('/connections') });
  const [open, setOpen] = useState(false);
  return <>
    <PageHeader title="厂商连接" description="连接使用最小权限凭据，按 DNS、SaaS 和线路调度用途标记。" actions={<Button variant="primary" onClick={() => setOpen(true)}><Plus size={16} /> 添加连接</Button>} />
    <Section title="已连接账户" description="能力由实际 API 探测决定；套餐不支持的功能会自动降级。"><QueryState loading={connections.isLoading} error={connections.error}><div className="connection-grid">{connections.data?.data.map((connection) => <ConnectionCard key={connection.id} connection={connection} />)}</div></QueryState></Section>
    <div className="security-band"><ShieldCheck size={19} /><div><strong>凭据在写入前加密</strong><p>API 只返回掩码；日志、作业和错误消息不会包含 Secret。</p></div><code>AES-256-GCM</code></div>
    <ConnectionDialog open={open} onOpenChange={setOpen} />
  </>;
}

function ConnectionCard({ connection }: { connection: Connection }) {
  const queryClient = useQueryClient();
  const validate = useMutation({ mutationFn: () => api<Connection>(`/connections/${connection.id}/validate`, { method: 'POST' }), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['connections'] }); toast.success('连接验证通过'); }, onError: (error) => toast.error(error.message) });
  const capabilities = parseCapabilities(connection.capabilities);
  return <article className="connection-card"><header><span className={`provider-logo provider-logo--${connection.provider}`}>{connection.provider === 'cloudflare' ? <Cloud /> : connection.provider === 'dnspod' ? <Cable /> : <KeyRound />}</span><div><strong>{connection.name}</strong><span>{providerNames[connection.provider]}</span></div><Badge status={connection.status} /></header><div className="connection-card__secret"><span>凭据</span><code>{connection.credentialMask}</code></div><div className="capability-list">{capabilities.map((item) => <span key={item}><CheckCircle2 size={13} />{capabilityLabel(item)}</span>)}</div>{connection.statusMessage && <div className="connection-warning"><TriangleAlert size={14} />{connection.statusMessage}</div>}<footer><span>验证于 {formatDate(connection.lastValidatedAt)}</span><Button size="sm" onClick={() => validate.mutate()} disabled={validate.isPending}><RefreshCw size={14} className={validate.isPending ? 'spin' : ''} /> 重新验证</Button></footer></article>;
}

function ConnectionDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const [provider, setProvider] = useState<'cloudflare' | 'dnspod' | 'alidns'>('cloudflare');
  const [name, setName] = useState(''); const [first, setFirst] = useState(''); const [second, setSecond] = useState('');
  const queryClient = useQueryClient();
  const create = useMutation({ mutationFn: () => { const credentials = provider === 'cloudflare' ? { apiToken: first } : provider === 'dnspod' ? { secretId: first, secretKey: second } : { accessKeyId: first, accessKeySecret: second }; return api('/connections', jsonBody({ name, provider, purposes: provider === 'cloudflare' ? 'dns,saas' : 'dns,routing', credentials })); }, onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['connections'] }); onOpenChange(false); toast.success('厂商连接已保存'); }, onError: (error) => toast.error(error.message) });
  function submit(event: FormEvent) { event.preventDefault(); create.mutate(); }
  const labels = provider === 'cloudflare' ? ['API Token', ''] : provider === 'dnspod' ? ['SecretId', 'SecretKey'] : ['AccessKey ID', 'AccessKey Secret'];
  return <DialogShell open={open} onOpenChange={onOpenChange} title="添加厂商连接" description="保存前会调用厂商只读接口验证凭据和可用能力。" footer={<><Button onClick={() => onOpenChange(false)}>取消</Button><Button variant="primary" type="submit" form="connection-form" disabled={!name || !first || (Boolean(labels[1]) && !second) || create.isPending}>{create.isPending ? '正在验证…' : '验证并保存'}</Button></>}><form id="connection-form" className="form-grid" onSubmit={submit}><Field label="厂商"><Segmented label="DNS 厂商" value={provider} onChange={(value) => { setProvider(value as typeof provider); setFirst(''); setSecond(''); }} options={[{ value: 'cloudflare', label: 'Cloudflare' }, { value: 'dnspod', label: 'DNSPod' }, { value: 'alidns', label: '阿里云' }]} /></Field><Field label="连接名称" required><input value={name} onChange={(e) => setName(e.target.value)} placeholder={`${providerNames[provider]} 生产账户`} /></Field><Field label={labels[0]} required hint="请只授予 NewNexus 所需区域和资源权限"><input type="password" value={first} onChange={(e) => setFirst(e.target.value)} autoComplete="off" /></Field>{labels[1] && <Field label={labels[1]} required><input type="password" value={second} onChange={(e) => setSecond(e.target.value)} autoComplete="off" /></Field>}</form></DialogShell>;
}

function parseCapabilities(raw: string) { try { const value = JSON.parse(raw); if (Array.isArray(value)) return value as string[]; return Object.entries(value).filter(([, enabled]) => enabled).map(([key]) => key); } catch { return []; } }
function capabilityLabel(value: string) { return ({ dns: 'DNS', saas: 'SaaS', lines: '线路解析', weights: '权重', default_line: '默认线路', batchChanges: '批量变更' } as Record<string, string>)[value] ?? value; }
