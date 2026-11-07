import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, expect, it, vi } from 'vitest';
import { AuthPage } from './auth';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('requires the operator setup proof and sends it only to first-admin bootstrap', async () => {
  const user = userEvent.setup();
  const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ user: { username: 'operator', totp_enabled: false }, csrf_token: 'test-csrf' }), { status: 200 }));
  vi.stubGlobal('fetch', fetchMock);
  render(<QueryClientProvider client={new QueryClient()}><AuthPage initialized={false} setupTokenRequired /></QueryClientProvider>);
  await user.type(screen.getByLabelText('管理员用户名'), 'operator');
  await user.type(screen.getByLabelText('密码', { exact: true }), 'NewNexus-Test-Only!26');
  await user.type(screen.getByLabelText('确认密码'), 'NewNexus-Test-Only!26');
  const proof = screen.getByLabelText('初始化验证码');
  expect(proof).toBeRequired();
  expect(proof).toHaveAttribute('type', 'password');
  await user.click(screen.getByRole('button', { name: '创建管理员并进入控制台' }));
  expect(fetchMock).not.toHaveBeenCalled();
  await user.type(proof, 'local-test-setup-proof-32-characters');
  await user.click(screen.getByRole('button', { name: '创建管理员并进入控制台' }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
  expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/bootstrap');
  expect(JSON.parse(fetchMock.mock.calls[0][1].body).setup_token).toBe('local-test-setup-proof-32-characters');
});

it('does not ask an existing administrator for the installation proof', () => {
  render(<QueryClientProvider client={new QueryClient()}><AuthPage initialized setupTokenRequired /></QueryClientProvider>);
  expect(screen.queryByLabelText('初始化验证码')).not.toBeInTheDocument();
  expect(screen.getByLabelText('验证码或恢复码')).toBeInTheDocument();
});
