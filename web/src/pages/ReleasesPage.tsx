import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, GitCommitHorizontal, History, RotateCcw } from 'lucide-react';
import { toast } from 'sonner';
import { api, formatDate, list } from '../lib/api';
import type { Release } from '../types';
import { Badge, Button, DialogShell, InlineCode, PageHeader, Section } from '../components/ui';
import { QueryState } from '../components/QueryState';

export function ReleasesPage() {
  const releases = useQuery({ queryKey: ['releases'], queryFn: () => list<Release>('/releases') });
  const [selected, setSelected] = useState<Release>();
  const queryClient = useQueryClient();
  const rollback = useMutation({ mutationFn: (id: string) => api<Release>(`/releases/${id}/rollback`, { method: 'POST' }), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ['releases'] }); setSelected(undefined); toast.success('已回滚到发布前快照'); }, onError: (error) => toast.error(error.message) });
  return <>
    <PageHeader title="发布记录" description="每次受管 DNS 变更都保存版本、触发方式、差异和回滚快照。" />
    <Section title="版本历史" description="回滚本身也会产生一个新版本，不覆盖已有审计历史。"><QueryState loading={releases.isLoading} error={releases.error}><div className="release-list">{releases.data?.data.map((release) => <article className="release-item" key={release.id}><span className={`release-item__icon release-item__icon--${release.status}`}><GitCommitHorizontal size={17} /></span><div className="release-item__main"><div><strong>发布 #{release.version}</strong><Badge status={release.status} /></div><p>{triggerLabel(release.trigger)}</p><InlineCode>{compactDiff(release.diffJson)}</InlineCode></div><div className="release-item__meta"><span>{formatDate(release.appliedAt ?? release.createdAt)}</span><span>{release.snapshotJson ? '含已验证快照' : '无快照'}</span></div><Button size="sm" onClick={() => setSelected(release)} disabled={!release.snapshotJson || release.status !== 'applied'}><RotateCcw size={14} /> 回滚</Button></article>)}</div></QueryState></Section>
    <div className="policy-foot"><History size={17} /><div><strong>回滚不会删除当前记录</strong><p>系统读取发布前快照并生成反向变更；无法验证快照时会拒绝操作。</p></div></div>
    <DialogShell open={Boolean(selected)} onOpenChange={(open) => !open && setSelected(undefined)} title={`回滚发布 #${selected?.version ?? ''}`} description="这会把该次发布涉及的线路记录恢复到发布前状态。" footer={<><Button onClick={() => setSelected(undefined)}>取消</Button><Button variant="danger" onClick={() => selected && rollback.mutate(selected.id)} disabled={rollback.isPending}><RotateCcw size={14} /> {rollback.isPending ? '正在回滚…' : '确认回滚'}</Button></>}><div className="rollback-flow"><span>当前记录</span><ArrowRight size={16} /><span>发布前快照</span></div><p className="dialog-copy">回滚前会再次读取厂商 observed 状态；发生漂移时将保存新的差异记录。</p></DialogShell>
  </>;
}

function triggerLabel(trigger: string) { return ({ health: '健康仲裁自动触发', manual: '管理员手动发布', rollback: '管理员执行回滚', performance: '性能退化触发' } as Record<string, string>)[trigger] ?? trigger; }
function compactDiff(raw: string) { try { const value = JSON.parse(raw); return Array.isArray(value) ? `${value.length} 条记录变更` : Object.entries(value).map(([key, item]) => `${key}: ${item}`).join(' · '); } catch { return raw || '无差异数据'; } }
