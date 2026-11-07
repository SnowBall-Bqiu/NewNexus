import { test, expect, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createHash, X509Certificate } from 'node:crypto';
import { mkdir, mkdtemp, writeFile } from 'node:fs/promises';
import { connect as connectHTTP2 } from 'node:http2';
import { connect as connectTLS } from 'node:tls';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
const agentBinary = resolve(root, `bin/newnexus-agent${process.platform === 'win32' ? '.exe' : ''}`);
const settingKey = 'agent_tls.self_signed_enabled';
type Metadata = { server: string; insecure_loopback: boolean; tls: { self_signed_enabled: boolean; ca_url?: string; ca_sha256?: string; ca_fingerprint_sha256?: string; server_name?: string; not_after?: string } };

async function csrf(page: Page) {
  const response = await page.request.get('/api/v1/auth/me');
  expect(response.ok()).toBe(true);
  return (await response.json()).csrf_token as string;
}

async function stopAgent(child?: ChildProcess) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((done) => { child.once('exit', () => done()); child.kill(); });
}

async function grpcHealth(server: string, ca: string, servername: string) {
  return new Promise<{ status: unknown; grpcStatus: unknown }>((done, reject) => {
    const connection = connectHTTP2(`https://${server}`, { ca, servername });
    const timer = setTimeout(() => { connection.destroy(); reject(new Error('TLS gRPC health timed out')); }, 8000);
    connection.on('error', (error) => { clearTimeout(timer); reject(error); });
    const stream = connection.request({ ':method': 'POST', ':path': '/grpc.health.v1.Health/Check', 'content-type': 'application/grpc', te: 'trailers' });
    let headers: Record<string, unknown> = {}; let trailers: Record<string, unknown> = {};
    stream.on('response', (value) => { headers = value; });
    stream.on('trailers', (value) => { trailers = value; });
    stream.on('data', () => {});
    stream.on('error', (error) => { clearTimeout(timer); connection.destroy(); reject(error); });
    stream.on('end', () => { clearTimeout(timer); connection.close(); done({ status: headers[':status'], grpcStatus: headers['grpc-status'] ?? trailers['grpc-status'] }); });
    stream.end(Buffer.alloc(5));
  });
}

test('自签开关控制CA下载，真实Agent验证CA后注册并使用持久凭据重连', async ({ page, playwright }, testInfo) => {
  test.skip(Boolean(process.env.NEWNEXUS_E2E_BASE_URL), 'requires the isolated local control plane and Agent port');
  test.setTimeout(90_000);
  const baseURL = testInfo.project.use.baseURL!;
  const originalResponse = await page.request.get('/api/v1/settings');
  const original = (await originalResponse.json()).items as { key: string; value: unknown }[];
  const originalEnabled = original.find((item) => item.key === settingKey)?.value ?? false;
  const originalEndpoint = original.find((item) => item.key === 'addresses.agent_endpoint')?.value ?? '';
  const anonymous = await playwright.request.newContext({ baseURL });
  let agent: ChildProcess | undefined;
  let agentError: Error | undefined;
  try {
    await page.goto('/settings?tab=addresses');
    const toggle = page.getByRole('checkbox', { name: 'gRPC 自签证书直连', exact: true });
    await toggle.check();
    await page.getByRole('textbox', { name: '默认 Agent 连接地址', exact: true }).fill('127.0.0.1:19090');
    const saveResponse = page.waitForResponse((response) => response.url().endsWith('/api/v1/settings') && response.request().method() === 'PUT');
    await page.getByRole('button', { name: '保存设置', exact: true }).click();
    expect((await saveResponse).ok()).toBe(true);
    const metadata = await (await page.request.get('/api/v1/nodes/installation')).json() as Metadata;
    expect(metadata.tls.self_signed_enabled).toBe(true);
    expect(metadata.insecure_loopback).toBe(false);
    expect(metadata.tls.ca_url).toBe('/api/v1/agent-ca.pem');
    expect(metadata.tls.server_name).toBeTruthy();
    const certificateResponse = await anonymous.get(metadata.tls.ca_url!);
    expect(certificateResponse.ok()).toBe(true);
    expect(certificateResponse.headers()['cache-control']).toContain('no-store');
    const ca = await certificateResponse.text();
    expect(ca).not.toContain('PRIVATE KEY');
    const certificate = new X509Certificate(ca);
    expect(certificate.ca).toBe(true);
    expect(createHash('sha256').update(ca).digest('hex')).toBe(metadata.tls.ca_sha256);
    expect(createHash('sha256').update(certificate.raw).digest('hex')).toBe(metadata.tls.ca_fingerprint_sha256);
    expect(await grpcHealth(metadata.server, ca, metadata.tls.server_name!)).toEqual({ status: 200, grpcStatus: '12' });
    const trustedWithoutCA = await new Promise<boolean>((done) => {
      const socket = connectTLS({ host: '127.0.0.1', port: 19090, servername: metadata.tls.server_name, rejectUnauthorized: true });
      socket.setTimeout(5000, () => { socket.destroy(); done(false); });
      socket.on('error', () => done(false));
      socket.on('secureConnect', () => { socket.destroy(); done(true); });
    });
    expect(trustedWithoutCA).toBe(false);
    await expect(page.getByRole('link', { name: '下载 CA 证书', exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('agent-self-signed-enabled.png'), fullPage: true });
    for (const colorScheme of ['light', 'dark'] as const) {
      await page.setViewportSize({ width: 390, height: 844 });
      await page.emulateMedia({ colorScheme });
      await expect(page.getByRole('link', { name: '下载 CA 证书', exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true);
      await page.screenshot({ path: testInfo.outputPath(`agent-self-signed-mobile-${colorScheme}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: 'light' });

    const privateRoot = resolve(root, '.cache/e2e-agent-tls');
    await mkdir(privateRoot, { recursive: true });
    const dataDir = await mkdtemp(resolve(privateRoot, 'node-'));
    const caPath = resolve(dataDir, 'ca.pem');
    await writeFile(caPath, ca, { mode: 0o600 });
    const nodeName = `e2e-self-signed-${Date.now()}`;
    const tokenResponse = await page.request.post('/api/v1/nodes/enrollment-tokens', { headers: { 'X-CSRF-Token': await csrf(page) }, data: { name: nodeName, expires_in_seconds: 900 } });
    expect(tokenResponse.ok()).toBe(true);
    const token = (await tokenResponse.json()).token as string;
    const startAgent = (enrollmentToken: string) => {
      agentError = undefined;
      const child = spawn(agentBinary, [], { cwd: root, windowsHide: true, stdio: 'ignore', env: {
        ...process.env, NEWNEXUS_AGENT_SERVER: metadata.server, NEWNEXUS_AGENT_CA_FILE: caPath,
        NEWNEXUS_AGENT_TLS_SERVER_NAME: metadata.tls.server_name, NEWNEXUS_AGENT_DATA_DIR: dataDir,
        NEWNEXUS_AGENT_NAME: nodeName, NEWNEXUS_AGENT_ENROLLMENT_TOKEN: enrollmentToken, NEWNEXUS_AGENT_INSECURE_LOOPBACK: 'false',
      } });
      child.on('error', (error) => { agentError = error; });
      return child;
    };
    agent = startAgent(token);
    const nodeIsOnline = async () => {
      if (agentError) throw agentError;
      const response = await page.request.get(`/api/v1/nodes?q=${encodeURIComponent(nodeName)}`);
      const { items } = await response.json();
      return items.some((node: { name: string; status: string }) => node.name === nodeName && node.status === 'online');
    };
    await expect.poll(nodeIsOnline, { timeout: 15_000 }).toBe(true);
    await stopAgent(agent); agent = undefined;
    await expect.poll(nodeIsOnline, { timeout: 10_000 }).toBe(false);
    // No enrollment token is supplied on restart: only persisted credentials.
    agent = startAgent('');
    await expect.poll(nodeIsOnline, { timeout: 15_000 }).toBe(true);
    await stopAgent(agent); agent = undefined;

    await toggle.focus();
    await toggle.press('Space');
    await expect(toggle).not.toBeChecked();
    const disabledSave = page.waitForResponse((response) => response.url().endsWith('/api/v1/settings') && response.request().method() === 'PUT');
    await page.getByRole('button', { name: '保存设置', exact: true }).click();
    expect((await disabledSave).ok()).toBe(true);
    expect((await anonymous.get('/api/v1/agent-ca.pem')).status()).toBe(404);
    const disabledMetadata = await (await page.request.get('/api/v1/nodes/installation')).json() as Metadata;
    expect(disabledMetadata.tls).toEqual({ self_signed_enabled: false });
    await expect(page.getByRole('link', { name: '下载 CA 证书', exact: true })).toHaveCount(0);
    const reenable = await page.request.put('/api/v1/settings', { headers: { 'X-CSRF-Token': await csrf(page) }, data: { [settingKey]: true } });
    expect(reenable.ok()).toBe(true);
    expect(await (await anonymous.get('/api/v1/agent-ca.pem')).text()).toBe(ca);
  } finally {
    await stopAgent(agent);
    const response = await page.request.put('/api/v1/settings', { headers: { 'X-CSRF-Token': await csrf(page) }, data: { [settingKey]: originalEnabled, 'addresses.agent_endpoint': originalEndpoint } });
    expect(response.ok()).toBe(true);
    await anonymous.dispose();
  }
});
