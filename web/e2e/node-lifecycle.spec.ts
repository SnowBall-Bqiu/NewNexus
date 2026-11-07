import { test, expect, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdir, mkdtemp, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
const agentBinary = resolve(root, `bin/newnexus-agent${process.platform === 'win32' ? '.exe' : ''}`);

async function csrf(page: Page) {
  const response = await page.request.get('/api/v1/auth/me');
  expect(response.ok()).toBe(true);
  return (await response.json()).csrf_token as string;
}

async function stopAgent(child?: ChildProcess) {
  if (!child?.pid || child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((done) => {
    const timer = setTimeout(() => { child.kill('SIGKILL'); }, 5000);
    child.once('exit', () => { clearTimeout(timer); done(); });
    child.kill();
  });
}

test('已撤销节点重新部署保留身份和分组，删除在线节点清理引用且保留审计', async ({ page }, testInfo) => {
  test.skip(Boolean(process.env.NEWNEXUS_E2E_BASE_URL), 'requires the isolated local control plane and Agent port');
  test.setTimeout(90_000);
  const original = (await (await page.request.get('/api/v1/settings')).json()).items as { key: string; value: unknown }[];
  const originalEnabled = original.find((item) => item.key === 'agent_tls.self_signed_enabled')?.value ?? false;
  const originalEndpoint = original.find((item) => item.key === 'addresses.agent_endpoint')?.value ?? '';
  let child: ChildProcess | undefined;
  let groupId = '';
  let nodeId = '';
  let spawnError: Error | undefined;
  const name = `e2e-node-lifecycle-${Date.now()}`;
  const node = async () => {
    if (spawnError) throw spawnError;
    const response = await page.request.get(`/api/v1/nodes/${nodeId}`);
    return response.ok() ? await response.json() as { id: string; name: string; status: string } : undefined;
  };
  try {
    const settings = await page.request.put('/api/v1/settings', { headers: { 'X-CSRF-Token': await csrf(page) }, data: {
      'agent_tls.self_signed_enabled': true, 'addresses.agent_endpoint': '127.0.0.1:19090',
    } });
    expect(settings.ok()).toBe(true);
    const metadata = await (await page.request.get('/api/v1/nodes/installation')).json();
    const privateRoot = resolve(root, '.cache/e2e-node-lifecycle');
    await mkdir(privateRoot, { recursive: true });
    const directory = await mkdtemp(resolve(privateRoot, 'case-'));
    const caPath = resolve(directory, 'ca.pem');
    await writeFile(caPath, await (await page.request.get(metadata.tls.ca_url)).body(), { mode: 0o600 });
    const startAgent = (token: string, suffix: string) => {
      spawnError = undefined;
      const process = spawn(agentBinary, [], { cwd: root, windowsHide: true, stdio: 'ignore', env: {
        ...globalThis.process.env, NEWNEXUS_AGENT_NAME: name, NEWNEXUS_AGENT_SERVER: metadata.server,
        NEWNEXUS_AGENT_CA_FILE: caPath, NEWNEXUS_AGENT_TLS_SERVER_NAME: metadata.tls.server_name,
        NEWNEXUS_AGENT_INSECURE_LOOPBACK: 'false', NEWNEXUS_AGENT_DATA_DIR: resolve(directory, suffix),
        NEWNEXUS_AGENT_ENROLLMENT_TOKEN: token,
      } });
      process.on('error', (error) => { spawnError = error; });
      return process;
    };
    const enrollment = await page.request.post('/api/v1/nodes/enrollment-tokens', { headers: { 'X-CSRF-Token': await csrf(page) }, data: { name, expires_in_seconds: 900 } });
    expect(enrollment.ok()).toBe(true);
    child = startAgent((await enrollment.json()).token, 'original');
    await expect.poll(async () => {
      if (spawnError) throw spawnError;
      const items = (await (await page.request.get(`/api/v1/nodes?q=${encodeURIComponent(name)}`)).json()).items as { id: string; name: string; status: string }[];
      const found = items.find((item) => item.name === name && item.status === 'online');
      nodeId = found?.id ?? '';
      return Boolean(found);
    }, { timeout: 15_000 }).toBe(true);
    const group = await page.request.post('/api/v1/node-groups', { headers: { 'X-CSRF-Token': await csrf(page) }, data: { name: `${name}-group`, node_ids: [nodeId] } });
    expect(group.ok()).toBe(true);
    groupId = (await group.json()).id;

    await expect.poll(async () => {
      const item = await (await page.request.get(`/api/v1/nodes/${nodeId}`)).json();
      return typeof item.agent_concurrency === 'number';
    }, { timeout: 15_000 }).toBe(true);
    await page.goto('/nodes');
    await page.getByRole('radio', { name: '跟随系统', exact: true }).click();
    await page.locator('.resource-name').filter({ hasText: name }).click();
    let details = page.getByRole('dialog');
    await expect(details.locator('dt').filter({ hasText: /^Agent 并发数$/ })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^CPU 架构$/ })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^操作系统$/ })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^支持的探测协议$/ })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^(status|agentConcurrency|agentMode|architecture|backpressure|os|outboxBytes|outboxSize|pendingTasks|protocolVersion|protocols)$/ })).toHaveCount(0);
    await expect(details.getByText('TCP 连接', { exact: true }).first()).toBeVisible();
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 950 });
      for (const colorScheme of ['light', 'dark'] as const) {
        await page.emulateMedia({ colorScheme });
        expect(await details.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);
        await page.screenshot({ path: testInfo.outputPath(`node-details-${width}-${colorScheme}.png`) });
      }
    }
    await page.keyboard.press('Escape');
    await expect(details).toHaveCount(0);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto('/nodes?tab=groups');
    await page.locator('.resource-name').filter({ hasText: `${name}-group` }).click();
    details = page.getByRole('dialog');
    const members = details.getByRole('list', { name: '成员节点列表', exact: true });
    await expect(members.getByText(name, { exact: true })).toBeVisible();
    await expect(members.getByText(nodeId, { exact: true })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^状态$/ })).toBeVisible();
    await expect(details.locator('dt').filter({ hasText: /^status$/ })).toHaveCount(0);
    await expect(members.locator('pre')).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath('node-group-resolved-members.png') });
    await page.keyboard.press('Escape');
    await expect(details).toHaveCount(0);
    await page.goto('/nodes');
    const trigger = page.getByRole('button', { name: `${name} 的操作`, exact: true });
    await trigger.click();
    await expect(page.getByRole('menuitem', { name: '删除节点', exact: true })).toBeVisible();
    await page.getByRole('menuitem', { name: '重新部署此节点', exact: true }).click();
    let dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    expect((await node())?.status).toBe('online');
    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);
    await expect(trigger).toBeFocused();
    expect((await node())?.status).toBe('online');

    const revoked = await page.request.post(`/api/v1/nodes/${nodeId}/revoke`, { headers: { 'X-CSRF-Token': await csrf(page) }, data: {} });
    expect(revoked.ok()).toBe(true);
    await expect.poll(async () => (await node())?.status).toBe('revoked');
    await stopAgent(child); child = undefined;
    await page.reload();
    await trigger.click();
    await page.getByRole('menuitem', { name: '重新部署此节点', exact: true }).click();
    dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('textbox', { name: '节点名称', exact: true })).toHaveValue(name);
    await expect(dialog.getByRole('textbox', { name: '节点名称', exact: true })).toBeDisabled();
    const generate = dialog.getByRole('button', { name: '生成重新部署指令', exact: true });
    await expect(generate).toBeDisabled();
    const confirm = dialog.getByRole('checkbox');
    await confirm.focus();
    await confirm.press('Space');
    const issued = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/nodes/${nodeId}/redeploy` && response.request().method() === 'POST');
    await generate.click();
    const issuedResponse = await issued;
    expect(issuedResponse.ok()).toBe(true);
    expect(issuedResponse.request().postDataJSON().confirmed).toBe(true);
    const redeployment = await issuedResponse.json() as { token: string; node_id: string };
    expect(redeployment.node_id).toBe(nodeId);
    const script = dialog.locator('pre');
    await expect(script).toContainText('/var/backups/newnexus-agent');
    await expect(script).toContainText('NEWNEXUS_AGENT_CA_FILE');
    // Hide the one-time token in screenshots while retaining the reviewable UI.
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 950 });
      for (const theme of ['浅色主题', '深色主题']) {
        // The background theme controls remain available once the dialog closes;
        // system preference is sufficient here because earlier tests select system.
        await page.emulateMedia({ colorScheme: theme === '浅色主题' ? 'light' : 'dark' });
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
        const bounds = await dialog.evaluate((element) => ({ width: element.clientWidth, content: element.scrollWidth }));
        expect(bounds.content).toBeLessThanOrEqual(bounds.width + 1);
        await page.screenshot({ path: testInfo.outputPath(`node-redeploy-${width}-${theme === '浅色主题' ? 'light' : 'dark'}.png`), mask: [script, dialog.locator('code[aria-label]')] });
      }
    }
    await dialog.locator('[data-slot="dialog-footer"]').getByRole('button', { name: '关闭', exact: true }).click();
    child = startAgent(redeployment.token, 'redeployed');
    await expect.poll(async () => (await node())?.status, { timeout: 15_000 }).toBe('online');
    expect((await node())?.name).toBe(name);
    const list = await (await page.request.get(`/api/v1/nodes?q=${encodeURIComponent(name)}`)).json();
    expect(list.items).toHaveLength(1);
    expect(list.items[0].id).toBe(nodeId);
    expect((await (await page.request.get(`/api/v1/node-groups/${groupId}`)).json()).node_ids).toEqual([nodeId]);

    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.reload();
    await trigger.click();
    await page.getByRole('menuitem', { name: '删除节点', exact: true }).click();
    dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('button', { name: '确认删除', exact: true })).toBeDisabled();
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    expect((await node())?.status).toBe('online');
    await expect(trigger).toBeFocused();
    await trigger.click();
    await page.getByRole('menuitem', { name: '删除节点', exact: true }).click();
    dialog = page.getByRole('dialog');
    await dialog.getByRole('textbox', { name: '输入名称确认删除', exact: true }).fill(name);
    const deleted = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/nodes/${nodeId}` && response.request().method() === 'DELETE');
    await dialog.getByRole('button', { name: '确认删除', exact: true }).click();
    expect((await deleted).ok()).toBe(true);
    await expect(trigger).toHaveCount(0);
    expect((await page.request.get(`/api/v1/nodes/${nodeId}`)).status()).toBe(404);
    expect((await (await page.request.get(`/api/v1/node-groups/${groupId}`)).json()).node_ids).toEqual([]);
    const audit = await (await page.request.get(`/api/v1/audit?q=${encodeURIComponent(nodeId)}`)).json();
    expect(audit.items.some((item: { action: string }) => item.action === 'redeploy')).toBe(true);
    expect(audit.items.some((item: { action: string }) => item.action === 'delete')).toBe(true);
  } finally {
    await stopAgent(child);
    if (nodeId) await page.request.delete(`/api/v1/nodes/${nodeId}`, { headers: { 'X-CSRF-Token': await csrf(page) } });
    if (groupId) await page.request.delete(`/api/v1/node-groups/${groupId}`, { headers: { 'X-CSRF-Token': await csrf(page) } });
    const restored = await page.request.put('/api/v1/settings', { headers: { 'X-CSRF-Token': await csrf(page) }, data: {
      'agent_tls.self_signed_enabled': originalEnabled, 'addresses.agent_endpoint': originalEndpoint,
    } });
    expect(restored.ok()).toBe(true);
  }
});
