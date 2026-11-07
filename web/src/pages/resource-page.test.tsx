import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, list } from '@/lib/api';
import { ResourcePage } from './resource-page';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), list: vi.fn(), api: vi.fn() }));
let capability: Record<string, unknown> = {};
beforeEach(() => {
  capability = {};
  vi.mocked(api).mockReset();
  vi.mocked(list).mockImplementation(async (path) => {
    const data = path.startsWith('/connections') ? [{ id: 'cf', name: 'Cloudflare', provider: 'cloudflare', zoneCapabilities: { zone: capability } }] : path.startsWith('/saas/zones') ? [{ id: 'saas-zone', connectionId: 'cf', zoneId: 'zone', zoneName: 'example.net' }] : path.startsWith('/saas/hostnames') ? [{ id: 'hostname', hostname: 'customer.example.com', saasZoneId: 'saas-zone', status: 'active', sslStatus: 'active' }] : [];
    return { data, total: data.length } as never;
  });
});
afterEach(cleanup);
function renderPage(resourceKey = 'hostnames') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return { ...render(<MemoryRouter><QueryClientProvider client={client}><ResourcePage resourceKey={resourceKey} /></QueryClientProvider></MemoryRouter>), client };
}

describe('SaaS capability gating', () => {
  it('disables unknown write operations with guidance while retaining status synchronization', async () => {
    const user = userEvent.setup(); renderPage();
    await user.click(await screen.findByRole('button', { name: 'customer.example.com 的操作' }));
    expect(await screen.findByRole('menuitem', { name: '检测验证与证书' })).not.toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: '激活并准备切流' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: '删除' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText(/尚未确认此接入区的 SaaS 能力/)).toBeInTheDocument();
    await user.click(await screen.findByRole('menuitem', { name: '编辑' }));
    expect(await screen.findByRole('button', { name: '保存配置' })).toBeDisabled();
    expect(screen.getByRole('textbox', { name: '独立源站主机名（可选）' })).toBeDisabled();
  });
  it('enables independent origins only after that zone reports the specific capability', async () => {
    capability = { saas: true, customOrigin: true };
    const user = userEvent.setup(); renderPage();
    await user.click(await screen.findByRole('button', { name: 'customer.example.com 的操作' }));
    await user.click(await screen.findByRole('menuitem', { name: '编辑' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存配置' })).toBeEnabled());
    expect(screen.getByRole('textbox', { name: '独立源站主机名（可选）' })).toBeEnabled();
  });
});

describe('node lifecycle actions', () => {
  const node = { id: 'd5802c51-58b3-4c19-9cae-f515d3dad14b', name: 'existing-edge', status: 'revoked' };
  beforeEach(() => {
    vi.mocked(list).mockResolvedValue({ data: [node], total: 1 } as never);
    vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? { server: '127.0.0.1:9091', artifacts: [{ arch: 'amd64', url: '/api/v1/agent-downloads/linux-amd64', sha256: 'a'.repeat(64) }], insecureLoopback: true } : {} as never);
  });

  it('allows deleting and redeploying a revoked node, and does not revoke it when the redeploy dialog opens or closes', async () => {
    const user = userEvent.setup();
    renderPage('nodes');
    const trigger = await screen.findByRole('button', { name: 'existing-edge 的操作' });
    await user.click(trigger);
    expect(await screen.findByRole('menuitem', { name: '重新部署此节点' })).not.toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: '删除节点' })).not.toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: '轮换节点凭据' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: '撤销节点' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('menuitem', { name: '重新部署此节点' }));
    await screen.findByRole('dialog', { name: '重新部署探测节点' });
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '重新部署探测节点' })).not.toBeInTheDocument());
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
  });

  it('requires the exact node name before deletion and invalidates node and group choices after success', async () => {
    const user = userEvent.setup();
    const { client } = renderPage('nodes');
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    await user.click(await screen.findByRole('button', { name: 'existing-edge 的操作' }));
    await user.click(await screen.findByRole('menuitem', { name: '删除节点' }));
    expect(await screen.findByRole('dialog', { name: '删除节点' })).toHaveTextContent('此操作不会卸载远程主机上的 Agent');
    const confirm = screen.getByRole('button', { name: '确认删除' });
    expect(confirm).toBeDisabled();
    await user.type(screen.getByRole('textbox', { name: '输入名称确认删除' }), 'another-node');
    expect(confirm).toBeDisabled();
    await user.clear(screen.getByRole('textbox', { name: '输入名称确认删除' }));
    await user.type(screen.getByRole('textbox', { name: '输入名称确认删除' }), node.name);
    await user.click(confirm);
    await waitFor(() => expect(vi.mocked(api)).toHaveBeenCalledWith(`/nodes/${node.id}`, { method: 'DELETE' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '删除节点' })).not.toBeInTheDocument());
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['nodes'] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['groups'] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['options'] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['node-options'] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['node-option'] });
  });
});
