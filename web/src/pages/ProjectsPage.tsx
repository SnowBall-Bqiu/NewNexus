import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Boxes, Globe2, MoreHorizontal, Plus, Server } from 'lucide-react';
import { api, jsonBody, list } from '../lib/api';
import type { Project } from '../types';
import { Badge, Button, DialogShell, Field, IconButton, PageHeader, Section } from '../components/ui';
import { QueryState } from '../components/QueryState';
import { toast } from 'sonner';

export function ProjectsPage() {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => list<Project>('/projects') });
  const create = useMutation({ mutationFn: () => api<Project>('/projects', jsonBody({ name, description })), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['projects'] }); setOpen(false); setName(''); setDescription(''); toast.success('项目已创建'); } });
  function submit(event: FormEvent) { event.preventDefault(); create.mutate(); }
  return <>
    <PageHeader title="项目与站点" description="按业务边界组织 SaaS、优选候选、探测配置和线路规则。" actions={<Button variant="primary" onClick={() => setOpen(true)}><Plus size={16} /> 新建项目</Button>} />
    <QueryState loading={projects.isLoading} error={projects.error}><div className="project-grid">{projects.data?.data.map((project, index) => <article className="project-card" key={project.id}><div className="project-card__top"><span className="project-avatar" style={{ background: project.color }}>{project.name.slice(0, 1)}</span><Badge status={project.status} /><IconButton label="项目菜单"><MoreHorizontal size={17} /></IconButton></div><div className="project-card__body"><h2>{project.name}</h2><p>{project.description || '尚未添加项目说明'}</p></div><div className="project-card__stats"><span><Globe2 size={15} /><strong>{index === 0 ? 2 : 0}</strong> 站点</span><span><Server size={15} /><strong>{index === 0 ? 3 : 0}</strong> 节点组</span><span><Boxes size={15} /><strong>{index === 0 ? '8.2K' : 0}</strong> 候选</span></div><button className="project-card__action">进入项目 <span>→</span></button></article>)}</div></QueryState>
    <Section title="配置边界" description="项目之间共享厂商连接与节点，但探测模板、候选、线路和发布版本彼此隔离。"><div className="info-band"><Boxes size={19} /><div><strong>单管理员不等于单项目</strong><p>用项目隔离不同业务域名，避免一次线路发布影响无关服务。</p></div></div></Section>
    <DialogShell open={open} onOpenChange={setOpen} title="新建项目" description="创建后可以继续添加站点、探测配置和线路规则。" footer={<><Button onClick={() => setOpen(false)}>取消</Button><Button variant="primary" form="project-form" type="submit" disabled={!name || create.isPending}>{create.isPending ? '正在创建…' : '创建项目'}</Button></>}><form id="project-form" onSubmit={submit} className="form-grid"><Field label="项目名称" required><input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：生产主站" /></Field><Field label="说明"><textarea value={description} onChange={(e) => setDescription(e.target.value)} placeholder="这个项目承载哪些业务？" rows={3} /></Field></form></DialogShell>
  </>;
}
