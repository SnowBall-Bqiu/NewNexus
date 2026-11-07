import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { SettingsPage } from './settings';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
describe('SettingsPage', () => {
  it('activates managed certificate details only after saving and disables them without rewriting configured secrets', async () => {
    const user = userEvent.setup();
    let enabled = false;
    const rows = [
      { key: 'addresses.api_origin', value: 'https://api.example.com', effective_value: 'https://api.example.com', source: 'database' },
      { key: 'addresses.agent_endpoint', value: '198.51.100.2:19443', effective_value: '198.51.100.2:19443', source: 'database' },
      { key: 'addresses.console_origin', value: 'https://console.example.com', effective_value: 'https://console.example.com', source: 'database' },
      { key: 'alerts.smtp_password', value: '\u2022'.repeat(8) },
    ];
    const fetchMock = vi.fn().mockImplementation((url: string, options: RequestInit) => {
      if (url.endsWith('/nodes/installation')) return Promise.resolve(new Response(JSON.stringify({ server: '198.51.100.2:19443', insecure_loopback: false, artifacts: [], tls: { self_signed_enabled: enabled, server_name: 'stable-agent.internal', ca_url: '/api/v1/agent-ca.pem', ca_sha256: 'a'.repeat(64), ca_fingerprint_sha256: 'b'.repeat(64), not_after: '2046-01-01T00:00:00Z' } }), { status: 200 }));
      if (options.method === 'PUT') enabled = JSON.parse(String(options.body))['agent_tls.self_signed_enabled'];
      return Promise.resolve(new Response(JSON.stringify(options.method === 'PUT' ? {} : { items: [...rows, { key: 'agent_tls.self_signed_enabled', value: enabled }] }), { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<MemoryRouter initialEntries={['/settings?tab=addresses']}><QueryClientProvider client={client}><SettingsPage user={{ username: 'admin', totpEnabled: false }} /></QueryClientProvider></MemoryRouter>);
    const checkbox = await screen.findByRole('checkbox', { name: 'gRPC 自签证书直连' });
    expect(checkbox).not.toBeChecked();
    expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument();
    await user.click(checkbox);
    expect(checkbox).toBeChecked();
    expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    expect(await screen.findByRole('link', { name: '下载 CA 证书' })).toHaveAttribute('href', 'https://api.example.com/api/v1/agent-ca.pem');
    const firstWrite = JSON.parse(fetchMock.mock.calls.find(([, options]) => options.method === 'PUT')![1].body);
    expect(firstWrite).toEqual({ 'addresses.api_origin': 'https://api.example.com', 'addresses.agent_endpoint': '198.51.100.2:19443', 'addresses.console_origin': 'https://console.example.com', 'agent_tls.self_signed_enabled': true });
    await user.click(checkbox);
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(screen.queryByRole('link', { name: '下载 CA 证书' })).not.toBeInTheDocument());
    const writes = fetchMock.mock.calls.filter(([, options]) => options.method === 'PUT');
    expect(writes).toHaveLength(2);
    expect(JSON.parse(writes[1][1].body)).toEqual({ ...firstWrite, 'agent_tls.self_signed_enabled': false });
    expect(fetchMock.mock.calls.every(([url]) => url === '/api/v1/settings' || url === '/api/v1/nodes/installation')).toBe(true);
  });

  it('preserves inherited defaults and validates independent public addresses before saving', async () => {
    const user = userEvent.setup();
    let rows = [
      { key: 'addresses.api_origin', value: '', effective_value: 'https://api.example.com', source: 'runtime' },
      { key: 'addresses.agent_endpoint', value: '', effective_value: 'agent.example.com:443', source: 'runtime' },
      { key: 'addresses.console_origin', value: '', effective_value: '', source: 'unset' },
    ];
    const fetchMock = vi.fn().mockImplementation((_url: string, options: RequestInit) => {
      if (options.method === 'PUT') {
        const body = JSON.parse(String(options.body));
        rows = rows.map((row) => ({ ...row, value: body[row.key], effective_value: body[row.key] || row.effective_value }));
      }
      return Promise.resolve(new Response(JSON.stringify(options.method === 'PUT' ? {} : { items: rows }), { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    render(<MemoryRouter initialEntries={['/settings?tab=addresses']}><QueryClientProvider client={client}><SettingsPage user={{ username: 'admin', totpEnabled: false }} /></QueryClientProvider></MemoryRouter>);
    const apiAddress = await screen.findByRole('textbox', { name: '默认 API 地址' });
    await waitFor(() => expect(apiAddress).toHaveAttribute('placeholder', 'https://api.example.com'));
    expect(apiAddress).toHaveValue('');
    const agentAddress = screen.getByRole('textbox', { name: '默认 Agent 连接地址' });
    const consoleAddress = screen.getByRole('textbox', { name: '控制台访问地址' });
    await user.type(consoleAddress, 'https://console.example.com/path');
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    expect(consoleAddress).toHaveAttribute('aria-invalid', 'true');
    expect(fetchMock.mock.calls.every(([, options]) => options.method !== 'PUT')).toBe(true);
    await user.clear(consoleAddress); await user.type(consoleAddress, 'https://console.example.com');
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, options]) => options.method === 'PUT')).toBe(true));
    expect(JSON.parse(fetchMock.mock.calls.find(([, options]) => options.method === 'PUT')![1].body)).toEqual({
      'addresses.api_origin': '', 'addresses.agent_endpoint': '', 'addresses.console_origin': 'https://console.example.com',
    });
    await waitFor(() => expect(screen.getByRole('link', { name: '打开控制台' })).toHaveAttribute('href', 'https://console.example.com'));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['nodes', 'installation'] });
    await user.type(apiAddress, 'https://download.example.com');
    await user.type(agentAddress, 'edge.example.com:8443');
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(fetchMock.mock.calls.filter(([, options]) => options.method === 'PUT')).toHaveLength(2));
    expect(JSON.parse(fetchMock.mock.calls.filter(([, options]) => options.method === 'PUT')[1][1].body)).toEqual({
      'addresses.api_origin': 'https://download.example.com', 'addresses.agent_endpoint': 'edge.example.com:8443', 'addresses.console_origin': 'https://console.example.com',
    });
  });
  it('shows field validation and blocks invalid requests before serialization', async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockImplementation((_url: string, options: RequestInit) => Promise.resolve(new Response(JSON.stringify(options.method === 'PUT' ? {} : { items: [{ key: 'probe.retries', value: 2 }] }), { status: 200 })));
    vi.stubGlobal('fetch', fetchMock);
    render(<MemoryRouter><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><SettingsPage user={{ username: 'admin', totpEnabled: false }} /></QueryClientProvider></MemoryRouter>);
    const retries = await screen.findByRole('spinbutton', { name: '失败重试次数' });
    await waitFor(() => expect(retries).toHaveValue(2));
    await user.clear(retries);
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('请先修正下列设置，再保存。');
    expect(retries).toHaveAttribute('aria-invalid', 'true');
    expect(fetchMock.mock.calls.every(([, options]) => options.method !== 'PUT')).toBe(true);
    await user.type(retries, '0');
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, options]) => options.method === 'PUT' && options.body === '{"probe.retries":0}')).toBe(true));
  });
  it('submits edited HTTP template JSON as typed data and blocks invalid header values', async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockImplementation((_url: string, options: RequestInit) => Promise.resolve(new Response(JSON.stringify(options.method === 'PUT' ? {} : { items: [{ key: 'probe.method', value: 'GET' }, { key: 'probe.headers', value: {} }, { key: 'probe.expected_statuses', value: [200] }] }), { status: 200 })));
    vi.stubGlobal('fetch', fetchMock);
    render(<MemoryRouter><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><SettingsPage user={{ username: 'admin', totpEnabled: false }} /></QueryClientProvider></MemoryRouter>);
    const headers = await screen.findByRole('textbox', { name: '默认请求头（JSON）' });
    await waitFor(() => expect(headers).toHaveValue('{}'));
    fireEvent.change(headers, { target: { value: '{"X_Test":3}' } });
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    expect(headers).toHaveAttribute('aria-invalid', 'true');
    expect(fetchMock.mock.calls.every(([, options]) => options.method !== 'PUT')).toBe(true);
    fireEvent.change(headers, { target: { value: '{"X_Test":"ok"}' } });
    fireEvent.change(screen.getByRole('textbox', { name: '默认预期 HTTP 状态码（JSON）' }), { target: { value: '[200,204]' } });
    await user.click(screen.getByRole('combobox', { name: '请求方法' }));
    await user.click(await screen.findByRole('option', { name: 'POST' }));
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, options]) => options.method === 'PUT' && JSON.parse(options.body)['probe.method'] === 'POST' && JSON.parse(options.body)['probe.headers'].X_Test === 'ok' && JSON.parse(options.body)['probe.expected_statuses'].length === 2)).toBe(true));
  });
  it('keeps masked credentials configured and omits them when saving other settings', async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockImplementation((_url: string, options: RequestInit) => Promise.resolve(new Response(JSON.stringify(options.method === 'PUT' ? {} : { items: [{ key: 'probe.retries', value: 2 }, { key: 'alerts.smtp_password', value: '\u2022'.repeat(8) }, { key: 'alerts.webhook_secret', value: '\u2022'.repeat(8) }] }), { status: 200 })));
    vi.stubGlobal('fetch', fetchMock);
    render(<MemoryRouter><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><SettingsPage user={{ username: 'admin', totpEnabled: false }} /></QueryClientProvider></MemoryRouter>);
    const retries = await screen.findByRole('spinbutton', { name: '失败重试次数' });
    await waitFor(() => expect(retries).toHaveValue(2));
    await user.click(screen.getByRole('tab', { name: '告警通知' }));
    const configured = screen.getAllByPlaceholderText('已配置，留空保留现有值');
    expect(configured).toHaveLength(2);
    configured.forEach((input) => expect(input).toHaveValue(''));
    await user.click(screen.getByRole('tab', { name: '探测与调度' }));
    await user.clear(screen.getByRole('spinbutton', { name: '失败重试次数' }));
    await user.type(screen.getByRole('spinbutton', { name: '失败重试次数' }), '3');
    await user.click(screen.getByRole('button', { name: '保存设置' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, options]) => options.method === 'PUT')).toBe(true));
    const body = JSON.parse(fetchMock.mock.calls.find(([, options]) => options.method === 'PUT')?.[1].body);
    expect(body).toEqual({ 'probe.retries': 3 });
  });
});
