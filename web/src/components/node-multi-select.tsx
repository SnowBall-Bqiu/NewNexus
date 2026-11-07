import { useState } from 'react';
import { useInfiniteQuery, useQueries } from '@tanstack/react-query';
import { Search, X } from 'lucide-react';
import { ErrorState, StatusBadge } from '@/components/common';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { api, ApiError, list, type Resource } from '@/lib/api';
import { t } from '@/lib/i18n';

type NodeMultiSelectProps = {
  id: string;
  label: string;
  value: unknown;
  onChange: (ids: string[]) => void;
  disabled?: boolean;
};

export function NodeMultiSelect({ id, label, value, onChange, disabled = false }: NodeMultiSelectProps) {
  const [search, setSearch] = useState('');
  const selected = [...new Set(Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string' && Boolean(item)) : [])];
  const query = useInfiniteQuery({
    queryKey: ['node-options', search.trim()], initialPageParam: '', staleTime: 15_000, refetchOnMount: 'always',
    queryFn: ({ pageParam }) => list<Resource>(`/nodes?limit=100${search.trim() ? `&q=${encodeURIComponent(search.trim())}` : ''}${pageParam ? `&cursor=${encodeURIComponent(pageParam)}` : ''}`),
    getNextPageParam: (page) => page.nextCursor || undefined,
  });
  const nodes = [...new Map((query.data?.pages.flatMap((page) => page.data) ?? []).map((node) => [node.id, node])).values()];
  // Resolve selected members separately so filtering or paging never drops their
  // labels, or removes a reference merely because its node is no longer listed.
  const members = useQueries({ queries: selected.map((nodeId) => ({
    queryKey: ['node-option', nodeId], queryFn: () => api<Resource>(`/nodes/${encodeURIComponent(nodeId)}`),
    initialData: () => nodes.find((node) => node.id === nodeId), staleTime: 30_000, refetchOnMount: 'always' as const, retry: false,
  })) });
  const setSelected = (nodeId: string, checked: boolean) => onChange(checked ? [...new Set([...selected, nodeId])] : selected.filter((item) => item !== nodeId));

  return <fieldset className="field min-w-0" disabled={disabled} aria-describedby={`${id}-hint`}>
    <legend className="mb-2 text-sm font-medium">{label}</legend>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <span className="text-xs text-muted-foreground" role="status">{t('nodes.picker.selected', { count: selected.length })}</span>
      <Button type="button" variant="ghost" size="sm" disabled={disabled || !selected.length} onClick={() => onChange([])}>{t('nodes.picker.clear')}</Button>
    </div>
    {selected.length > 0 && <ul className="flex flex-wrap gap-2" aria-label={t('nodes.picker.selectedLabel')}>
      {selected.map((nodeId, index) => {
        const member = members[index];
        const missing = member.error instanceof ApiError && member.error.status === 404;
        const node = missing ? undefined : member.data ?? nodes.find((item) => item.id === nodeId);
        const name = String(node?.name || nodeId);
        return <li key={nodeId} className="inline-flex min-w-0 max-w-full items-center gap-1 rounded-md border bg-muted/40 py-0.5 pl-2 text-xs" title={nodeId}>
          <span className="min-w-0 break-all">{node ? name : member.isPending ? t('nodes.picker.unloaded', { id: nodeId }) : <>{nodeId} · {t(missing ? 'nodes.details.memberMissing' : 'nodes.details.memberUnavailable')}</>}</span>
          <Button type="button" variant="ghost" size="icon-sm" className="size-6 shrink-0" aria-label={t('nodes.picker.remove', { name })} disabled={disabled} onClick={() => setSelected(nodeId, false)}><X size={12} /></Button>
        </li>;
      })}
    </ul>}
    <div className="relative">
      <Search size={14} className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
      <Input id={id} className="pl-9" aria-label={t('nodes.picker.search')} value={search} disabled={disabled} autoComplete="off" placeholder={t('nodes.picker.searchHint')} onChange={(event) => setSearch(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') event.preventDefault(); }} />
    </div>
    {query.error && <ErrorState error={query.error} retry={() => { void query.refetch(); }} />}
    <div className="max-h-64 overflow-y-auto overscroll-contain rounded-md border divide-y" aria-busy={query.isFetching}>
      {nodes.map((node) => {
        const name = String(node.name || node.id);
        const metadata = [node.region, node.carrier].filter(Boolean).map(String).join(' · ');
        const optionId = `${id}-${node.id}`;
        return <Label key={node.id} htmlFor={optionId} className="flex cursor-pointer items-start gap-3 px-3 py-3 hover:bg-muted/50 has-focus-visible:bg-muted/50">
          <Checkbox id={optionId} className="mt-0.5" aria-label={name} checked={selected.includes(node.id)} disabled={disabled} onCheckedChange={(checked) => setSelected(node.id, checked === true)} />
          <span className="min-w-0 flex-1 space-y-1"><span className="block break-all text-sm font-medium">{name}</span><span className="block break-all text-xs font-normal text-muted-foreground">{metadata || node.id}</span></span>
          <StatusBadge value={node.status} />
        </Label>;
      })}
      {query.isPending && <p role="status" className="px-3 py-5 text-center text-sm text-muted-foreground">{t('common.loading')}</p>}
      {!query.isPending && !query.error && !nodes.length && <p className="px-3 py-5 text-center text-sm text-muted-foreground">{search.trim() ? t('nodes.picker.noMatches') : t('nodes.picker.empty')}</p>}
    </div>
    {query.hasNextPage && <Button type="button" size="sm" variant="outline" disabled={disabled || query.isFetching} onClick={() => { void query.fetchNextPage(); }}>{query.isFetchingNextPage ? t('common.loading') : t('nodes.picker.loadMore')}</Button>}
    <small id={`${id}-hint`}>{t('nodes.picker.hint')}</small>
  </fieldset>;
}
