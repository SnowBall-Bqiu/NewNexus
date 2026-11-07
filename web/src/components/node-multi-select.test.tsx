import { useState } from 'react';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, list } from '@/lib/api';
import { NodeMultiSelect } from './node-multi-select';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), api: vi.fn(), list: vi.fn() }));

const online = { id: 'node-online', name: '香港在线节点', status: 'online', region: 'HK' };
const offline = { id: 'node-offline', name: '东京离线节点', status: 'offline', region: 'JP' };
const saved = { id: 'node-saved', name: '已保存的新加坡节点', status: 'offline', region: 'SG' };
const nodes = [online, offline, saved];

beforeEach(() => {
  vi.mocked(list).mockReset();
  vi.mocked(api).mockReset();
  vi.mocked(list).mockImplementation(async (path) => {
    const query = new URL(path, 'https://console.example.com').searchParams.get('q') ?? '';
    const data = [online, offline].filter((node) => node.name.includes(query));
    return { data, total: data.length } as never;
  });
  vi.mocked(api).mockImplementation(async (path) => {
    const node = nodes.find((item) => path === `/nodes/${item.id}`);
    if (!node) throw new ApiError('节点已不存在', 404, 'not_found');
    return node as never;
  });
});
afterEach(cleanup);

function Selector({ initial = [], onChange, disabled = false }: { initial?: unknown; onChange: (value: string[]) => void; disabled?: boolean }) {
  const [value, setValue] = useState<unknown>(initial);
  return <NodeMultiSelect id="members" label="成员节点" value={value} disabled={disabled} onChange={(next) => { setValue(next); onChange(next); }} />;
}

function show(initial: unknown = [], disabled = false, client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })) {
  const changed = vi.fn();
  render(<QueryClientProvider client={client}><Selector initial={initial} onChange={changed} disabled={disabled} /></QueryClientProvider>);
  return { changed, client };
}

describe('node membership selector', () => {
  it('selects and removes both online and offline nodes as string IDs', async () => {
    const user = userEvent.setup();
    const { changed } = show();
    const onlineCheckbox = await screen.findByRole('checkbox', { name: online.name });
    const offlineCheckbox = await screen.findByRole('checkbox', { name: offline.name });
    expect(offlineCheckbox).toBeEnabled();
    await user.click(onlineCheckbox);
    await user.click(offlineCheckbox);
    expect(changed.mock.lastCall?.[0]).toEqual([online.id, offline.id]);
    expect(onlineCheckbox).toBeChecked();
    expect(offlineCheckbox).toBeChecked();
    await user.click(screen.getByRole('button', { name: `移除 ${online.name}` }));
    expect(changed.mock.lastCall?.[0]).toEqual([offline.id]);
    await user.click(screen.getByRole('button', { name: '清空选择' }));
    expect(changed.mock.lastCall?.[0]).toEqual([]);
    expect(offlineCheckbox).not.toBeChecked();
  });

  it('searches the server while preserving nodes already selected outside the result', async () => {
    const user = userEvent.setup();
    const { changed } = show();
    await user.click(await screen.findByRole('checkbox', { name: online.name }));
    await user.type(screen.getByRole('textbox', { name: '搜索节点' }), '东京');
    await waitFor(() => expect(vi.mocked(list).mock.calls.some(([path]) => new URL(path, 'https://console.example.com').searchParams.get('q') === '东京')).toBe(true));
    expect(await screen.findByRole('button', { name: `移除 ${online.name}` })).toBeVisible();
    await user.click(await screen.findByRole('checkbox', { name: offline.name }));
    expect(changed.mock.lastCall?.[0]).toEqual([online.id, offline.id]);
    expect(vi.mocked(list).mock.calls.every(([path]) => new URL(path, 'https://console.example.com').searchParams.get('limit') === '100')).toBe(true);
  });

  it('loads later pages with the returned cursor without losing the first-page selection', async () => {
    const user = userEvent.setup();
    const cursor = 'next/page+2';
    vi.mocked(list).mockImplementation(async (path) => {
      const page = new URL(path, 'https://console.example.com').searchParams.get('cursor');
      return page === cursor ? { data: [offline], total: 2 } as never : { data: [online], total: 2, nextCursor: cursor } as never;
    });
    const { changed } = show();
    await user.click(await screen.findByRole('checkbox', { name: online.name }));
    await user.click(screen.getByRole('button', { name: '加载更多节点' }));
    await user.click(await screen.findByRole('checkbox', { name: offline.name }));
    expect(changed.mock.lastCall?.[0]).toEqual([online.id, offline.id]);
    expect(screen.getByRole('checkbox', { name: online.name })).toBeChecked();
    expect(vi.mocked(list).mock.calls.some(([path]) => new URL(path, 'https://console.example.com').searchParams.get('cursor') === cursor)).toBe(true);
    expect(screen.queryByRole('button', { name: '加载更多节点' })).not.toBeInTheDocument();
  });

  it('resolves saved nodes outside the option page and retains unknown IDs until explicitly removed', async () => {
    const user = userEvent.setup();
    const { changed } = show([saved.id, 'unknown-node']);
    expect(await screen.findByRole('button', { name: `移除 ${saved.name}` })).toBeVisible();
    const removeUnknown = await screen.findByRole('button', { name: '移除 unknown-node' });
    await waitFor(() => expect(vi.mocked(api)).toHaveBeenCalledWith('/nodes/unknown-node'));
    expect(changed).not.toHaveBeenCalled();
    await user.click(removeUnknown);
    expect(changed.mock.lastCall?.[0]).toEqual([saved.id]);
    await user.click(screen.getByRole('button', { name: `移除 ${saved.name}` }));
    expect(changed.mock.lastCall?.[0]).toEqual([]);
  });

  it('explains when no nodes have been registered', async () => {
    vi.mocked(list).mockResolvedValue({ data: [], total: 0 } as never);
    show();
    expect(await screen.findByText('暂无可选节点，请先注册探测节点')).toBeVisible();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
  });

  it('rechecks an invalidated cached member and stops showing its deleted name after a 404', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    client.setQueryData(['node-options', ''], { pages: [{ data: [online], total: 1 }], pageParams: [''] }, { updatedAt: Date.now() - 120_000 });
    await client.invalidateQueries({ queryKey: ['node-options'] });
    vi.mocked(list).mockResolvedValue({ data: [], total: 0 } as never);
    vi.mocked(api).mockRejectedValue(new ApiError('not found', 404, 'not_found'));
    const { changed } = show([online.id], false, client);
    expect(await screen.findByText(`${online.id} · 节点已删除或不可用`)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: `移除 ${online.id}` })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: `移除 ${online.name}` })).not.toBeInTheDocument();
    expect(api).toHaveBeenCalledWith(`/nodes/${online.id}`);
    expect(changed).not.toHaveBeenCalled();
  });

  it('distinguishes an empty search from an empty node inventory', async () => {
    const user = userEvent.setup();
    show();
    await screen.findByRole('checkbox', { name: online.name });
    await user.type(screen.getByRole('textbox', { name: '搜索节点' }), '不存在的节点');
    expect(await screen.findByText('没有匹配的节点')).toBeVisible();
    expect(screen.queryByText('暂无可选节点，请先注册探测节点')).not.toBeInTheDocument();
  });

  it('prevents changes when the field is disabled', async () => {
    const user = userEvent.setup();
    const { changed } = show([saved.id], true);
    expect(screen.getByRole('textbox', { name: '搜索节点' })).toBeDisabled();
    const checkbox = await screen.findByRole('checkbox', { name: online.name });
    expect(checkbox).toHaveAttribute('aria-disabled', 'true');
    await user.click(checkbox);
    expect(await screen.findByRole('button', { name: `移除 ${saved.name}` })).toBeDisabled();
    expect(screen.getByRole('button', { name: '清空选择' })).toBeDisabled();
    expect(changed).not.toHaveBeenCalled();
  });
});
