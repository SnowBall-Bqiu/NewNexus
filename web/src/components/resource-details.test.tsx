import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, type Resource } from '@/lib/api';
import { resources } from '@/lib/resources';
import { formatBytes, ResourceDetails } from './resource-details';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), api: vi.fn() }));
beforeEach(() => { vi.mocked(api).mockReset(); });
afterEach(cleanup);
function show(resourceKey: string, item: Resource, client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })) {
  return { ...render(<QueryClientProvider client={client}><ResourceDetails spec={resources[resourceKey]} item={item} /></QueryClientProvider>), client };
}
function valueFor(label: string) {
  const row = screen.getByText(label, { selector: 'dt' }).parentElement!;
  return within(row.querySelector('dd')!);
}

describe('resource detail presentation', () => {
  it('presents node telemetry with Chinese field names, modes, capability lists and readable units while retaining unknown values', () => {
    show('nodes', {
      id: 'node-online', name: '香港节点', status: 'online', agentConcurrency: 16, agentMode: 'tcp', architecture: 'amd64',
      backpressure: false, os: 'linux', outboxBytes: 2048, outboxSize: 0, pendingTasks: 3, probeSamplesFailed: 2,
      probeSamplesTotal: 50, protocolVersion: 1, protocols: 'tcp,http', failureRate: 0.04,
      capabilities: ['tcp', 'tls'], detectedCapabilities: ['http', 'download', 'future-capability'], unknownFutureField: '保留原始信息',
      credential: 'must-not-be-displayed',
    });
    for (const label of ['Agent 并发数', 'Agent 实际探测模式', 'CPU 架构', '任务接收状态', '操作系统', '待上报结果占用', '待上报结果数', '待处理任务数', '失败探测样本数', '探测样本总数', '通信协议版本', '支持的探测协议']) expect(screen.getByText(label, { selector: 'dt' })).toBeInTheDocument();
    expect(valueFor('状态').getByText('在线')).toBeInTheDocument();
    expect(valueFor('Agent 实际探测模式').getByText('仅 TCP')).toBeInTheDocument();
    expect(valueFor('任务接收状态').getByText('正常接收任务')).toBeInTheDocument();
    expect(valueFor('CPU 架构').getByText('x86_64 / amd64')).toBeInTheDocument();
    expect(valueFor('操作系统').getByText('Linux')).toBeInTheDocument();
    expect(valueFor('待上报结果占用').getByText('2 KiB')).toHaveAttribute('title', '2,048 B');
    expect(valueFor('待上报结果数').getByText('0')).toBeInTheDocument();
    expect(valueFor('探测失败率').getByText('4%')).toBeInTheDocument();
    expect(valueFor('已检测能力').getAllByRole('listitem').map((item) => item.textContent)).toEqual(['HTTP 请求', '下载测速', 'future-capability']);
    expect(valueFor('unknownFutureField').getByText('保留原始信息')).toBeInTheDocument();
    expect(screen.queryByText('must-not-be-displayed')).not.toBeInTheDocument();
    expect(screen.queryByText('agentConcurrency', { selector: 'dt' })).not.toBeInTheDocument();
  });

  it('resolves group members by name using the selector cache, and distinguishes loading, deleted and temporarily unavailable nodes', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    client.setQueryData(['node-options', ''], { pages: [{ data: [{ id: 'cached', name: '香港在线节点', status: 'online' }], total: 1 }], pageParams: [''] });
    let resolveNode!: (value: Resource) => void;
    const pending = new Promise<Resource>((resolve) => { resolveNode = resolve; });
    vi.mocked(api).mockImplementation(async (path) => {
      if (path === '/nodes/cached') return { id: 'cached', name: '香港在线节点', status: 'online' } as never;
      if (path === '/nodes/missing') throw new ApiError('not found', 404, 'not_found');
      if (path === '/nodes/unavailable') throw new ApiError('unavailable', 503, 'unavailable');
      if (path === '/nodes/pending') return pending as never;
      throw new Error(`Unexpected path ${path}`);
    });
    show('groups', { id: 'group-id', name: '测试节点组', status: 'active', nodeIds: ['cached', 'missing', 'unavailable', 'pending', 'cached'] }, client);
    const list = screen.getByRole('list', { name: '成员节点列表' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(4);
    expect(within(list).getByText('香港在线节点')).toBeInTheDocument();
    expect(within(list).getByText('cached')).toBeInTheDocument();
    expect(await within(list).findByText('节点已删除或不可用')).toBeInTheDocument();
    expect(await within(list).findByText('暂时无法读取节点')).toBeInTheDocument();
    expect(within(list).getAllByText('正在读取节点…')).toHaveLength(1);
    await act(async () => { resolveNode({ id: 'pending', name: '东京离线节点', status: 'offline' }); });
    await waitFor(() => expect(within(list).queryByText('正在读取节点…')).not.toBeInTheDocument());
    expect(within(list).getByText('东京离线节点')).toBeInTheDocument();
    expect(within(list).getByText('离线')).toBeInTheDocument();
    expect(valueFor('状态').getByText('已激活')).toBeInTheDocument();
    expect(vi.mocked(api).mock.calls.some(([path]) => path === '/nodes/cached')).toBe(true);
  });

  it('revalidates an expired and invalidated selector cache before treating an old member as still available', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    client.setQueryData(['node-options', ''], { pages: [{ data: [{ id: 'deleted-member', name: '已删除的旧节点名', status: 'online' }], total: 1 }], pageParams: [''] }, { updatedAt: Date.now() - 120_000 });
    await client.invalidateQueries({ queryKey: ['node-options'] });
    vi.mocked(api).mockRejectedValue(new ApiError('not found', 404, 'not_found'));
    show('groups', { id: 'group-id', name: '测试节点组', nodeIds: ['deleted-member'] }, client);
    const members = screen.getByRole('list', { name: '成员节点列表' });
    expect(await within(members).findByText('节点已删除或不可用')).toBeInTheDocument();
    expect(within(members).getByText('deleted-member')).toBeInTheDocument();
    expect(within(members).queryByText('已删除的旧节点名')).not.toBeInTheDocument();
    expect(within(members).queryByText('在线')).not.toBeInTheDocument();
    expect(api).toHaveBeenCalledWith('/nodes/deleted-member');
  });

  it('replaces an empty group UUID array with an explicit empty state', () => {
    show('groups', { id: 'empty-group', name: '空节点组', nodeIds: [] });
    expect(valueFor('成员节点').getByText('暂无成员节点')).toBeInTheDocument();
    expect(screen.queryByText('[]')).not.toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });
});

it('formats zero, fractional and large byte values without invalid units', () => {
  expect(formatBytes(0)).toBe('0 B');
  expect(formatBytes(0.5)).toBe('0.5 B');
  expect(formatBytes(1536)).toBe('1.5 KiB');
  expect(formatBytes(1024 ** 3)).toBe('1 GiB');
  expect(formatBytes(-1)).toBe('—');
  expect(formatBytes(Number.NaN)).toBe('—');
});
