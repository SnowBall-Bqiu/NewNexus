import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import type { AgentTLS } from '@/lib/node-install';
import { AgentTLSDetails } from './agent-tls-details';

const tls: AgentTLS = { selfSignedEnabled: true, serverName: 'stable-agent.internal', caUrl: '/api/v1/agent-ca.pem', caSha256: 'a'.repeat(64), caFingerprintSha256: 'b'.repeat(64), notAfter: '2046-01-01T00:00:00Z' };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('shows and copies distinct certificate hashes, SNI, expiry and the independent API CA URL', async () => {
  const user = userEvent.setup();
  const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue();
  const fetchMock = vi.fn();
  vi.stubGlobal('fetch', fetchMock);
  render(<AgentTLSDetails tls={tls} origin="https://api.example.com:8443" />);
  expect(screen.getByRole('link', { name: '下载 CA 证书' })).toHaveAttribute('href', 'https://api.example.com:8443/api/v1/agent-ca.pem');
  expect(screen.getByText(tls.caSha256!)).toBeVisible();
  expect(screen.getByText(tls.caFingerprintSha256!)).toBeVisible();
  for (const [label, value] of [['复制 SNI', tls.serverName], ['复制 PEM SHA-256', tls.caSha256], ['复制证书指纹', tls.caFingerprintSha256], ['复制到期时间', tls.notAfter], ['复制 CA 下载地址', 'https://api.example.com:8443/api/v1/agent-ca.pem']]) {
    await user.click(screen.getByRole('button', { name: label }));
    expect(copy).toHaveBeenLastCalledWith(value);
  }
  expect(screen.getByText(/已安装节点请先/)).toHaveTextContent('保留注册码、节点凭据及数据目录');
  expect(screen.getByText(/已安装节点请先/)).toHaveTextContent('不要重跑新节点安装指令');
  expect(fetchMock).not.toHaveBeenCalled();
});

it('hides every certificate field and link when disabled, including stale malicious metadata', () => {
  const { rerender } = render(<AgentTLSDetails tls={tls} origin="https://api.example.com" />);
  expect(screen.getByRole('link', { name: '下载 CA 证书' })).toBeVisible();
  rerender(<AgentTLSDetails tls={{ ...tls, selfSignedEnabled: false, caUrl: 'https://attacker.example/ca' }} origin="https://api.example.com" />);
  expect(screen.queryByRole('link')).not.toBeInTheDocument();
  expect(screen.queryByText('已生效的 gRPC 自签证书')).not.toBeInTheDocument();
  expect(screen.queryByText(tls.caSha256!)).not.toBeInTheDocument();
  expect(screen.queryByText(tls.serverName!)).not.toBeInTheDocument();
});

it('shows an error without exposing a download link for a malformed active bundle', () => {
  render(<AgentTLSDetails tls={{ ...tls, caUrl: 'https://attacker.example/ca' }} origin="https://api.example.com" />);
  expect(screen.getByRole('alert')).toHaveTextContent('自签证书信息不完整');
  expect(screen.queryByRole('link')).not.toBeInTheDocument();
  expect(screen.queryByText(tls.caSha256!)).not.toBeInTheDocument();
});
