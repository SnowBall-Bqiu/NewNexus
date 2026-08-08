import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Check, Copy, Cpu, Plus, RadioTower, Terminal, WifiOff } from 'lucide-react';
import { toast } from 'sonner';
import { api, formatDate, formatNumber, jsonBody, list } from '../lib/api';
import type { ProbeNode } from '../types';
import { Badge, Button, DialogShell, Field, InlineCode, PageHeader, Section, Segmented } from '../components/ui';
import { QueryState } from '../components/QueryState';

interface EnrollmentResult { node: ProbeNode; enrollmentCode: string; command: string; installCommand: string; binarySha256: string; architecture: string }

export function NodesPage() {
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: () => list<ProbeNode>('/nodes'), refetchInterval: 15_000 });
  const [open, setOpen] = useState(false);
  const [result, setResult] = useState<EnrollmentResult>();
  return <>
    <PageHeader title="探测节点" description="Agent 通过 gRPC 双向流领取任务，只主动出站，不暴露入站端口。" actions={<Button variant="primary" onClick={() => { setResult(undefined); setOpen(true); }}><Plus size={16} /> 注册节点</Button>} />
    <div className="node-summary"><div><RadioTower size={20} /><span>在线节点</span><strong>{nodes.data?.data.filter((item) => item.status === 'online').length ?? 0}</strong></div><div><Cpu size={20} /><span>累计任务</span><strong>{formatNumber(nodes.data?.data.reduce((sum, item) => sum + item.tasksCompleted, 0) ?? 0)}</strong></div><div><WifiOff size={20} /><span>平均失败率</span><strong>{((nodes.data?.data.reduce((sum, item) => sum + item.failureRate, 0) ?? 0) / Math.max(1, nodes.data?.data.length ?? 1) * 100).toFixed(2)}%</strong></div></div>
    <Section title="节点清单" description="线路健康默认使用匹配运营商的节点组进行仲裁。"><QueryState loading={nodes.isLoading} error={nodes.error}><div className="node-grid">{nodes.data?.data.map((node) => <article className="node-card" key={node.id}><header><span className={`node-card__icon node-card__icon--${node.status}`}><RadioTower size={18} /></span><div><strong>{node.name}</strong><span>{node.version || '等待注册'}</span></div><Badge status={node.status} /></header><dl><div><dt>节点组</dt><dd>{node.groupName}</dd></div><div><dt>位置 / 运营商</dt><dd>{node.region} · {node.carrier}</dd></div><div><dt>探测能力</dt><dd>{node.protocols.toUpperCase()}</dd></div><div><dt>完成任务</dt><dd>{formatNumber(node.tasksCompleted)}</dd></div><div><dt>失败率</dt><dd>{(node.failureRate * 100).toFixed(2)}%</dd></div><div><dt>最近心跳</dt><dd>{formatDate(node.lastSeenAt)}</dd></div></dl></article>)}</div></QueryState></Section>
    <Section title="gRPC 连接约束" description="生产环境应在反向代理中开启 HTTP/2 长连接和 TLS。"><div className="protocol-flow"><span>Agent</span><i>出站 TLS / HTTP/2</i><span>agent.v1.Connect</span><i>任务与批量结果</i><span>控制面</span></div></Section>
    <EnrollmentDialog open={open} onOpenChange={setOpen} result={result} onResult={setResult} />
  </>;
}

function EnrollmentDialog({ open, onOpenChange, result, onResult }: { open: boolean; onOpenChange: (open: boolean) => void; result?: EnrollmentResult; onResult: (result: EnrollmentResult) => void }) {
  const [name, setName] = useState(''); const [groupName, setGroupName] = useState(''); const [region, setRegion] = useState(''); const [carrier, setCarrier] = useState(''); const [protocols, setProtocols] = useState('tcp,http'); const [architecture, setArchitecture] = useState('amd64');
  const queryClient = useQueryClient();
  const create = useMutation({ mutationFn: () => api<EnrollmentResult>('/nodes/enroll', jsonBody({ name, groupName, region, carrier, protocols, architecture })), onSuccess: (value) => { onResult(value); queryClient.invalidateQueries({ queryKey: ['nodes'] }); } });
  function submit(event: FormEvent) { event.preventDefault(); create.mutate(); }
  return <DialogShell open={open} onOpenChange={onOpenChange} title={result ? '节点注册码已创建' : '注册探测节点'} description={result ? '注册码只显示一次，Agent 首次连接后会换取独立节点凭据。' : '为节点设置位置、运营商和允许执行的协议。'} size="lg" footer={result ? <Button variant="primary" onClick={() => onOpenChange(false)}><Check size={15} /> 完成</Button> : <><Button onClick={() => onOpenChange(false)}>取消</Button><Button variant="primary" form="node-form" type="submit" disabled={!name || !groupName || create.isPending}>{create.isPending ? '正在创建…' : '生成注册码'}</Button></>}>
    {result ? <div className="enrollment-result"><div className="secret-row"><span>一次性注册码</span><InlineCode>{result.enrollmentCode}</InlineCode><Button size="sm" onClick={() => { navigator.clipboard.writeText(result.enrollmentCode); toast.success('注册码已复制'); }}><Copy size={14} /> 复制</Button></div><div className="command-block"><div><Terminal size={15} /> {result.architecture} 一键安装</div><code>{result.installCommand}</code><Button size="sm" onClick={() => { navigator.clipboard.writeText(result.installCommand); toast.success('安装命令已复制'); }}><Copy size={14} /> 复制安装命令</Button></div><div className="command-block command-block--secondary"><div><Terminal size={15} /> 已安装 Agent 时手动注册</div><code>{result.command}</code><Button size="sm" onClick={() => navigator.clipboard.writeText(result.command)}><Copy size={14} /> 复制注册命令</Button></div><div className="warning-callout">把一键命令粘贴到目标 Linux 主机执行。命令包含只显示一次的注册码，请勿发送到公开日志或工单。</div></div> : <form id="node-form" onSubmit={submit} className="form-grid form-grid--two"><Field label="节点名称" required><input value={name} onChange={(e) => setName(e.target.value)} placeholder="hk-edge-02" /></Field><Field label="节点组" required><input value={groupName} onChange={(e) => setGroupName(e.target.value)} placeholder="香港骨干" /></Field><Field label="位置"><input value={region} onChange={(e) => setRegion(e.target.value)} placeholder="香港" /></Field><Field label="运营商"><input value={carrier} onChange={(e) => setCarrier(e.target.value)} placeholder="移动" /></Field><Field label="系统架构"><Segmented label="Agent 系统架构" value={architecture} onChange={setArchitecture} options={[{ value: 'amd64', label: 'amd64' }, { value: 'arm64', label: 'arm64' }]} /></Field><Field label="探测协议"><Segmented label="节点探测协议" value={protocols} onChange={setProtocols} options={[{ value: 'tcp', label: 'TCP' }, { value: 'http', label: 'HTTP' }, { value: 'tcp,http', label: '全部' }]} /></Field></form>}
  </DialogShell>;
}
