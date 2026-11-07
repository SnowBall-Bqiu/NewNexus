import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, list } from '@/lib/api';
import { t } from '@/lib/i18n';
import { ResourcePage } from './resource-page';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), api: vi.fn(), list: vi.fn() }));

const online = { id: 'node-online', name: '香港在线节点', status: 'online' };
const offline = { id: 'node-offline', name: '东京离线节点', status: 'offline' };
const saved = { id: 'node-saved', name: '已保存的新加坡节点', status: 'offline' };
const group = { id: 'group-existing', name: '现有节点组', description: '用于验收', nodeIds: [saved.id, 'unknown-node'] };

beforeEach(() => {
  vi.mocked(list).mockReset();
  vi.mocked(api).mockReset();
  vi.mocked(list).mockImplementation(async (path) => {
    const data = path.startsWith('/node-groups?') ? [group] : path.startsWith('/nodes?') ? [online, offline] : [];
    return { data, total: data.length } as never;
  });
  vi.mocked(api).mockImplementation(async (path, options) => {
    if (options?.method === 'POST' || options?.method === 'PUT') return { id: group.id } as never;
    const node = [online, offline, saved].find((item) => path === `/nodes/${item.id}`);
    if (node) return node as never;
    throw new ApiError('节点已不存在', 404, 'not_found');
  });
});
afterEach(cleanup);

function show() {
  render(<MemoryRouter><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}><ResourcePage resourceKey="groups" /></QueryClientProvider></MemoryRouter>);
}

function expectNodePicker() {
  expect(screen.getByRole('textbox', { name: '搜索节点' })).toBeVisible();
  expect(screen.queryByRole('textbox', { name: t('resources.groups.fields.nodeIds.label') })).not.toBeInTheDocument();
}

function membershipWrite(method: string) {
  return vi.mocked(api).mock.calls.find(([path, options]) => path.startsWith('/node-groups') && options?.method === method);
}

describe('node group membership editing', () => {
  it('creates a group with selected node_ids as an array rather than JSON text', async () => {
    const user = userEvent.setup();
    show();
    await user.click(screen.getByRole('button', { name: '新建节点组' }));
    expectNodePicker();
    await user.type(screen.getByRole('textbox', { name: '节点组名称' }), '跨区域节点组');
    await user.click(await screen.findByRole('checkbox', { name: online.name }));
    await user.click(screen.getByRole('checkbox', { name: offline.name }));
    await user.click(screen.getByRole('button', { name: '保存配置' }));
    await waitFor(() => expect(membershipWrite('POST')).toBeDefined());
    const [path, options] = membershipWrite('POST')!;
    expect(path).toBe('/node-groups');
    const payload = JSON.parse(options!.body as string);
    expect(payload).toMatchObject({ name: '跨区域节点组', node_ids: [online.id, offline.id] });
    expect(payload).not.toHaveProperty('nodeIds');
    expect(payload.node_ids.every((id: unknown) => typeof id === 'string')).toBe(true);
  });

  it('allows creating an empty group without manually entering an empty JSON array', async () => {
    const user = userEvent.setup();
    show();
    await user.click(screen.getByRole('button', { name: '新建节点组' }));
    expectNodePicker();
    await user.type(screen.getByRole('textbox', { name: '节点组名称' }), '暂不分配节点');
    await user.click(screen.getByRole('button', { name: '保存配置' }));
    await waitFor(() => expect(membershipWrite('POST')).toBeDefined());
    expect(JSON.parse(membershipWrite('POST')![1]!.body as string).node_ids).toEqual([]);
  });

  it('shows saved labels and preserves missing or off-page IDs in an unchanged PUT', async () => {
    const user = userEvent.setup();
    show();
    await user.click(await screen.findByRole('button', { name: '现有节点组 的操作' }));
    await user.click(await screen.findByRole('menuitem', { name: '编辑' }));
    expectNodePicker();
    expect(await screen.findByRole('button', { name: `移除 ${saved.name}` })).toBeVisible();
    expect(await screen.findByRole('button', { name: '移除 unknown-node' })).toBeVisible();
    await user.click(screen.getByRole('button', { name: '保存配置' }));
    await waitFor(() => expect(membershipWrite('PUT')).toBeDefined());
    const [path, options] = membershipWrite('PUT')!;
    expect(path).toBe(`/node-groups/${group.id}`);
    expect(JSON.parse(options!.body as string).node_ids).toEqual([saved.id, 'unknown-node']);
  });

  it('removes unknown IDs explicitly and saves a cleared membership as node_ids: []', async () => {
    const user = userEvent.setup();
    show();
    await user.click(await screen.findByRole('button', { name: '现有节点组 的操作' }));
    await user.click(await screen.findByRole('menuitem', { name: '编辑' }));
    expectNodePicker();
    await user.click(await screen.findByRole('button', { name: '移除 unknown-node' }));
    expect(screen.queryByRole('button', { name: '移除 unknown-node' })).not.toBeInTheDocument();
    expect(await screen.findByRole('button', { name: `移除 ${saved.name}` })).toBeVisible();
    await user.click(screen.getByRole('button', { name: '清空选择' }));
    await user.click(screen.getByRole('button', { name: '保存配置' }));
    await waitFor(() => expect(membershipWrite('PUT')).toBeDefined());
    expect(JSON.parse(membershipWrite('PUT')![1]!.body as string).node_ids).toEqual([]);
  });
});
