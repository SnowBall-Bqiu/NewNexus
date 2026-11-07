import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { api, type Resource } from '@/lib/api';
import { t } from '@/lib/i18n';
import type { NodeInstallation } from '@/lib/node-install';
import { NodeEnrollmentDialog } from './node-enrollment-dialog';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), api: vi.fn() }));
const installation: NodeInstallation = { server: '127.0.0.1:9091', insecureLoopback: true, artifacts: [{ arch: 'amd64', url: '/api/v1/agent-downloads/linux-amd64', sha256: 'a'.repeat(64) }] };
beforeEach(() => { vi.mocked(api).mockReset(); vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? installation : { token: 'generated-once-token', expiresAt: '2026-11-07T15:00:00Z' } as never); });
afterEach(cleanup);

function show(props: { redeployNode?: Resource; onClose?: () => void; onCreated?: () => void } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return { ...render(<QueryClientProvider client={client}><NodeEnrollmentDialog open onClose={vi.fn()} onCreated={vi.fn()} {...props} /></QueryClientProvider>), client };
}

it('generates and copies a real installation script, and keeps other loopback targets on TLS', async () => {
  const user = userEvent.setup();
  const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue();
  show();
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue('127.0.0.1:9091'));
  await user.type(screen.getByLabelText('节点名称'), 'hk-edge-01');
  await user.click(screen.getByRole('button', { name: '生成安装指令' }));
  const script = await screen.findByLabelText('Linux 安装指令');
  expect(script).toHaveTextContent('generated-once-token');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_SERVER="127.0.0.1:9091"');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_INSECURE_LOOPBACK="true"');
  await user.click(screen.getByRole('button', { name: '复制安装指令' }));
  expect(copy).toHaveBeenCalledWith(script.textContent);
  await user.clear(screen.getByLabelText('控制面 gRPC 地址'));
  await user.type(screen.getByLabelText('控制面 gRPC 地址'), '127.0.0.1:9443');
  expect(screen.getByLabelText('Linux 安装指令')).not.toHaveTextContent('INSECURE_LOOPBACK');
  expect(screen.getByLabelText('节点上的私有 CA 路径（可选）')).toBeVisible();
});

it('does not issue a token when binaries are missing or the endpoint is invalid', async () => {
  const user = userEvent.setup();
  vi.mocked(api).mockResolvedValue({ ...installation, artifacts: [] });
  show();
  await user.type(screen.getByLabelText('节点名称'), 'hk-edge-01');
  expect(await screen.findByRole('alert')).toHaveTextContent('下载文件尚未就绪');
  expect(screen.getByRole('button', { name: '生成安装指令' })).toBeDisabled();
  await user.clear(screen.getByLabelText('控制面 gRPC 地址'));
  await user.type(screen.getByLabelText('控制面 gRPC 地址'), 'https://host:443');
  expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveAttribute('aria-invalid', 'true');
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
});

it('uses API and console metadata separately from the public Agent endpoint', async () => {
  const user = userEvent.setup();
  const metadata = {
    ...installation, server: 'agent.example.com:443', insecureLoopback: false,
    downloadOrigin: 'https://api.example.com', consoleOrigin: 'https://console.example.com',
  };
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? metadata : { token: 'generated-once-token', expiresAt: '2026-11-07T15:00:00Z' } as never);
  show();
  await waitFor(() => expect(screen.getByLabelText('安装包下载入口')).toHaveValue('https://api.example.com'));
  expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue('agent.example.com:443');
  expect(screen.getByRole('link', { name: t('settings.addresses.openConsole') })).toHaveAttribute('href', 'https://console.example.com/nodes');
  await user.type(screen.getByLabelText('节点名称'), 'remote-node');
  await user.click(screen.getByRole('button', { name: '生成安装指令' }));
  const script = await screen.findByLabelText('Linux 安装指令');
  expect(script).toHaveTextContent('https://api.example.com/api/v1/agent-downloads/linux-amd64');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_SERVER="agent.example.com:443"');
  expect(script).not.toHaveTextContent(window.location.origin);
  expect(script).not.toHaveTextContent('https://console.example.com/api');
});

it('preserves manual addresses when installation metadata arrives or refreshes', async () => {
  const user = userEvent.setup();
  let resolveMetadata!: (value: NodeInstallation) => void;
  const metadata = new Promise<NodeInstallation>((resolve) => { resolveMetadata = resolve; });
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? metadata : { token: 'generated-once-token' } as never);
  const { client } = show();
  // Wait for the dialog's initial focus before editing another field.
  await waitFor(() => expect(screen.getByLabelText('节点名称')).toHaveFocus());
  const download = screen.getByLabelText('安装包下载入口');
  const server = screen.getByLabelText('控制面 gRPC 地址');
  await user.clear(download);
  await user.type(download, 'https://manual-api.example.com');
  await user.type(server, 'manual-agent.example.com:443');
  await act(async () => resolveMetadata({
    ...installation, server: 'default-agent.example.com:443', downloadOrigin: 'https://default-api.example.com', consoleOrigin: 'https://default-console.example.com',
  }));
  await screen.findByRole('link', { name: t('settings.addresses.openConsole') });
  expect(download).toHaveValue('https://manual-api.example.com');
  expect(server).toHaveValue('manual-agent.example.com:443');
  await act(async () => {
    client.setQueryData(['nodes', 'installation'], {
      ...installation, server: 'refreshed-agent.example.com:443', downloadOrigin: 'https://refreshed-api.example.com', consoleOrigin: 'https://refreshed-console.example.com',
    });
  });
  await waitFor(() => expect(screen.getByRole('link', { name: t('settings.addresses.openConsole') })).toHaveAttribute('href', 'https://refreshed-console.example.com/nodes'));
  expect(download).toHaveValue('https://manual-api.example.com');
  expect(server).toHaveValue('manual-agent.example.com:443');
});

it('does not request an enrollment token for an invalid download origin', async () => {
  const user = userEvent.setup();
  show();
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue('127.0.0.1:9091'));
  await user.type(screen.getByLabelText('节点名称'), 'remote-node');
  const download = screen.getByLabelText('安装包下载入口');
  for (const invalidOrigin of ['https://api.example.com/api', 'https://api.example.com?token=secret', 'https://user:password@api.example.com', 'https://api.example.com:0']) {
    await user.clear(download);
    await user.type(download, invalidOrigin);
    expect(download).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('button', { name: '生成安装指令' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '生成安装指令' }));
  }
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
});

const selfSignedInstallation: NodeInstallation = {
  ...installation, server: '198.51.100.12:19443', downloadOrigin: 'https://api.example.com', insecureLoopback: false,
  tls: { selfSignedEnabled: true, serverName: 'stable-agent.internal', caUrl: '/api/v1/agent-ca.pem', caSha256: 'c'.repeat(64), caFingerprintSha256: 'd'.repeat(64), notAfter: '2046-01-01T00:00:00Z' },
};

it('automatically installs the verified managed CA and stable SNI for an IP endpoint without changing enrollment credentials', async () => {
  const user = userEvent.setup();
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? selfSignedInstallation : { token: 'generated-once-token', expiresAt: '2026-11-07T15:00:00Z' } as never);
  const { client } = show();
  const ca = screen.getByLabelText('节点上的私有 CA 路径（可选）');
  const serverName = screen.getByLabelText(t('nodes.enrollment.tlsServerName'));
  await waitFor(() => expect(ca).toBeDisabled());
  expect(ca).toHaveValue('/etc/newnexus/agent-ca.pem');
  expect(serverName).toBeDisabled();
  expect(serverName).toHaveValue('stable-agent.internal');
  expect(screen.getByRole('link', { name: '下载 CA 证书' })).toHaveAttribute('href', 'https://api.example.com/api/v1/agent-ca.pem');
  await user.type(screen.getByLabelText('节点名称'), 'direct-ip-node');
  await user.click(screen.getByRole('button', { name: '生成安装指令' }));
  const script = await screen.findByLabelText('Linux 安装指令');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_SERVER="198.51.100.12:19443"');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_TLS_SERVER_NAME="stable-agent.internal"');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_CA_FILE="/etc/newnexus/agent-ca.pem"');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_ENROLLMENT_TOKEN="generated-once-token"');
  expect(script).toHaveTextContent('https://api.example.com/api/v1/agent-ca.pem');
  expect(script).not.toHaveTextContent('INSECURE_LOOPBACK');
  expect(script).not.toHaveTextContent('--insecure');
  await user.clear(screen.getByLabelText('安装包下载入口'));
  await user.type(screen.getByLabelText('安装包下载入口'), 'https://other-api.example.com');
  expect(screen.getByRole('link', { name: '下载 CA 证书' })).toHaveAttribute('href', 'https://other-api.example.com/api/v1/agent-ca.pem');
  expect(screen.getByLabelText('Linux 安装指令')).toHaveTextContent('https://other-api.example.com/api/v1/agent-ca.pem');

  await act(async () => { client.setQueryData(['nodes', 'installation'], { ...selfSignedInstallation, tls: { ...selfSignedInstallation.tls, selfSignedEnabled: false } }); });
  await waitFor(() => expect(ca).toBeEnabled());
  expect(serverName).toBeEnabled();
  expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument();
  expect(screen.getByLabelText('Linux 安装指令')).not.toHaveTextContent('/api/v1/agent-ca.pem');
  expect(screen.getByLabelText('Linux 安装指令')).toHaveTextContent('NEWNEXUS_AGENT_ENROLLMENT_TOKEN="generated-once-token"');
  expect(vi.mocked(api).mock.calls.filter(([path]) => path === '/nodes/enrollment-tokens')).toHaveLength(1);
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation' || path === '/nodes/enrollment-tokens')).toBe(true);
});

it('keeps manual private CA and SNI available when self-signed management is off', async () => {
  const user = userEvent.setup();
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? { ...selfSignedInstallation, tls: { ...selfSignedInstallation.tls, selfSignedEnabled: false, caUrl: 'https://attacker.example/ca' } } : { token: 'generated-once-token', expiresAt: '2026-11-07T15:00:00Z' } as never);
  show();
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue(selfSignedInstallation.server));
  expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument();
  await user.type(screen.getByLabelText('节点名称'), 'private-proxy-node');
  await user.type(screen.getByLabelText('节点上的私有 CA 路径（可选）'), '/etc/proxy/private-ca.pem');
  await user.type(screen.getByLabelText(t('nodes.enrollment.tlsServerName')), 'private-proxy.example.com');
  await user.click(screen.getByRole('button', { name: '生成安装指令' }));
  const script = await screen.findByLabelText('Linux 安装指令');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_CA_FILE="/etc/proxy/private-ca.pem"');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_TLS_SERVER_NAME="private-proxy.example.com"');
  expect(script).not.toHaveTextContent('$tmp/agent-ca.pem');
  expect(script).not.toHaveTextContent('attacker.example');
});

it.each([
  { caUrl: 'https://attacker.example/ca' },
  { caSha256: 'invalid' },
  { serverName: 'https://wrong.example.com' },
])('does not create enrollment credentials for an unsafe managed certificate bundle: %j', async (invalid) => {
  const user = userEvent.setup();
  vi.mocked(api).mockResolvedValue({ ...selfSignedInstallation, tls: { ...selfSignedInstallation.tls, ...invalid } });
  show();
  await user.type(screen.getByLabelText('节点名称'), 'blocked-node');
  expect(await screen.findByRole('alert')).toHaveTextContent('自签证书信息不完整');
  expect(screen.getByRole('button', { name: '生成安装指令' })).toBeDisabled();
  expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument();
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
});

it('requires explicit confirmation before revoking an existing node and creates a bound redeployment command once', async () => {
  const user = userEvent.setup();
  const existingNode = { id: 'd5802c51-58b3-4c19-9cae-f515d3dad14b', name: 'existing-edge', status: 'revoked' };
  const onCreated = vi.fn();
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? selfSignedInstallation : { token: 'bound-redeployment-token', expiresAt: '2026-11-07T15:00:00Z', nodeId: existingNode.id, name: existingNode.name } as never);
  show({ redeployNode: existingNode, onCreated });
  const generate = screen.getByRole('button', { name: '生成重新部署指令' });
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue(selfSignedInstallation.server));
  expect(screen.getByRole('textbox', { name: '节点名称' })).toBeDisabled();
  expect(screen.getByRole('textbox', { name: '节点名称' })).toHaveValue(existingNode.name);
  expect(screen.getByText(existingNode.id)).toBeInTheDocument();
  expect(generate).toBeDisabled();
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
  await user.click(screen.getByRole('checkbox', { name: /我已知晓旧凭据/ }));
  expect(generate).toBeEnabled();
  await user.click(generate);
  const script = await screen.findByLabelText('Linux 重新部署指令');
  expect(script).toHaveTextContent('bound-redeployment-token');
  expect(script).toHaveTextContent('NEWNEXUS_AGENT_NAME="existing-edge"');
  expect(script).toHaveTextContent('/etc/newnexus/agent-ca.pem');
  expect(vi.mocked(api)).toHaveBeenCalledWith(`/nodes/${existingNode.id}/redeploy`, expect.objectContaining({ method: 'POST', body: JSON.stringify({ confirmed: true, expires_in_seconds: 900 }) }));
  expect(vi.mocked(api).mock.calls.filter(([path]) => path.endsWith('/redeploy'))).toHaveLength(1);
  expect(vi.mocked(api).mock.calls.some(([path]) => path === '/nodes/enrollment-tokens')).toBe(false);
  expect(onCreated).toHaveBeenCalledOnce();
  expect(screen.queryByRole('button', { name: '生成重新部署指令' })).not.toBeInTheDocument();
});

it('discards redeployment credentials and confirmation when the dialog is closed', async () => {
  const user = userEvent.setup();
  const existingNode = { id: 'd5802c51-58b3-4c19-9cae-f515d3dad14b', name: 'existing-edge' };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const renderDialog = (open: boolean) => <QueryClientProvider client={client}><NodeEnrollmentDialog open={open} redeployNode={existingNode} onClose={vi.fn()} onCreated={vi.fn()} /></QueryClientProvider>;
  const view = render(renderDialog(true));
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue(installation.server));
  await user.click(screen.getByRole('checkbox', { name: /我已知晓旧凭据/ }));
  await user.click(screen.getByRole('button', { name: '生成重新部署指令' }));
  await screen.findByLabelText('Linux 重新部署指令');
  view.rerender(renderDialog(false));
  view.rerender(renderDialog(true));
  await screen.findByRole('heading', { name: '重新部署探测节点' });
  expect(screen.queryByLabelText('Linux 重新部署指令')).not.toBeInTheDocument();
  expect(screen.getByRole('checkbox', { name: /我已知晓旧凭据/ })).not.toBeChecked();
  expect(screen.getByRole('button', { name: '生成重新部署指令' })).toBeDisabled();
  expect(vi.mocked(api).mock.calls.filter(([path]) => path.endsWith('/redeploy'))).toHaveLength(1);
});

it('does not revoke credentials while a manual CA is inside the state directory that redeployment resets', async () => {
  const user = userEvent.setup();
  vi.mocked(api).mockImplementation(async (path) => path === '/nodes/installation' ? { ...selfSignedInstallation, tls: { selfSignedEnabled: false } } : { token: 'should-not-be-issued' } as never);
  show({ redeployNode: { id: 'd5802c51-58b3-4c19-9cae-f515d3dad14b', name: 'existing-edge' } });
  const ca = screen.getByLabelText('节点上的私有 CA 路径（可选）');
  await waitFor(() => expect(screen.getByLabelText('控制面 gRPC 地址')).toHaveValue(selfSignedInstallation.server));
  await user.click(screen.getByRole('checkbox', { name: /我已知晓旧凭据/ }));
  for (const value of ['/var/lib/newnexus-agent/private-ca.pem', '/var/lib/private/newnexus-agent/private-ca.pem', '/var/lib/./newnexus-agent/../newnexus-agent/private-ca.pem']) {
    await user.clear(ca);
    await user.type(ca, value);
    expect(ca).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('alert')).toHaveTextContent('请先将证书迁移到 /etc/newnexus');
    expect(screen.getByRole('button', { name: '生成重新部署指令' })).toBeDisabled();
  }
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
  await user.clear(ca);
  await user.type(ca, '/etc/newnexus/private-ca.pem');
  expect(ca).toHaveAttribute('aria-invalid', 'false');
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: '生成重新部署指令' })).toBeEnabled();
  expect(vi.mocked(api).mock.calls.every(([path]) => path === '/nodes/installation')).toBe(true);
});
