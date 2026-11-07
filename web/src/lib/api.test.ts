import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, jsonBody, list, setCsrfToken } from './api';

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(''); });
describe('REST boundary', () => {
  it('converts resource fields while preserving user supplied header and credential keys', () => {
    const request = jsonBody({ dnsConnectionId: 'dns-1', lines: [{ providerLineCode: 'default' }], headers: { 'X-Request-ID': 'a' }, credentials: { api_token: 'secret' } });
    expect(JSON.parse(String(request.body))).toEqual({ dns_connection_id: 'dns-1', lines: [{ provider_line_code: 'default' }], headers: { 'X-Request-ID': 'a' }, credentials: { api_token: 'secret' } });
  });
  it('keeps cookies and adds CSRF only on writes', async () => {
    const fetch = vi.fn().mockImplementation(async () => new Response(JSON.stringify({ ssl_status: 'active' }), { status: 200 })); vi.stubGlobal('fetch', fetch); setCsrfToken('csrf-test');
    expect(await api('/saas/hostnames/a', jsonBody({ customOrigin: 'origin.example.com' }, 'PUT'))).toEqual({ sslStatus: 'active' });
    const [, options] = fetch.mock.calls[0]; expect(options.credentials).toBe('same-origin'); expect(options.headers.get('X-CSRF-Token')).toBe('csrf-test');
    await api('/overview'); expect(fetch.mock.calls[1][1].headers.has('X-CSRF-Token')).toBe(false);
  });
  it('reads cursor pagination and structured errors without losing API codes', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: 'a', last_probed_at: 'now' }], total: 501, next_cursor: 'next' }))).mockResolvedValueOnce(new Response(JSON.stringify({ error: { code: 'csrf_invalid', message: '会话已失效' } }), { status: 403 })));
    expect(await list('/candidates')).toEqual({ data: [{ id: 'a', lastProbedAt: 'now' }], total: 501, nextCursor: 'next' });
    await expect(api('/projects', jsonBody({}))).rejects.toMatchObject({ name: 'ApiError', status: 403, code: 'csrf_invalid', message: '会话已失效' });
  });
  it('preserves zone IDs and settings header names while normalizing capability fields', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ zone_capabilities: { zone_with_underscore: { saas: true, custom_origin: true } }, capability_errors: { zone_with_underscore: 'Permission denied' } }))).mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ key: 'probe.headers', value: { 'X_Custom_Header': 'value', 'x-user-agent': 'custom' } }] }))));
    expect(await api('/connections/cf')).toEqual({ zoneCapabilities: { zone_with_underscore: { saas: true, customOrigin: true } }, capabilityErrors: { zone_with_underscore: 'Permission denied' } });
    expect((await list<{ key: string; value: unknown }>('/settings')).data[0].value).toEqual({ 'X_Custom_Header': 'value', 'x-user-agent': 'custom' });
  });
});
