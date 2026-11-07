import { test, expect } from '@playwright/test';

test('真实节点注册入口显示一次性注册码和可执行命令', async ({ page }) => {
  const metadataResponse = await page.request.get('/api/v1/nodes/installation'); expect(metadataResponse.ok()).toBe(true);
  const metadata = await metadataResponse.json() as { server: string; insecure_loopback: boolean; artifacts: { arch: string; url: string; sha256: string }[] };
  expect(metadata.server).toBe('127.0.0.1:19090'); expect(metadata.insecure_loopback).toBe(true);
  expect(metadata.artifacts.map((artifact) => artifact.arch).sort()).toEqual(['amd64', 'arm64']);
  await page.goto('/nodes'); await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(page.url()).origin });
  const trigger = page.getByRole('button', { name: '注册节点', exact: true }); await trigger.click();
  const dialog = page.getByRole('dialog'); const server = dialog.getByRole('textbox', { name: '控制面 gRPC 地址', exact: true });
  await expect(server).toHaveValue(metadata.server);
  await dialog.getByRole('textbox', { name: '节点名称', exact: true }).fill('e2e-edge-01');
  await server.fill('https://agents.example.com:443'); await expect(server).toHaveAttribute('aria-invalid', 'true');
  await expect(dialog.getByRole('button', { name: '生成安装指令', exact: true })).toBeDisabled(); await server.fill(metadata.server);
  const enrollmentResponse = page.waitForResponse((response) => response.url().endsWith('/api/v1/nodes/enrollment-tokens') && response.request().method() === 'POST');
  await dialog.getByRole('button', { name: '生成安装指令', exact: true }).click();
  const enrollment = await enrollmentResponse; expect(enrollment.ok()).toBe(true); const { token } = await enrollment.json() as { token: string };
  expect(token.length).toBeGreaterThan(20); await expect(dialog.locator('code[aria-label="一次性注册码"]')).toHaveText(token);
  await expect(page.getByText('注册码只显示这一次，请妥善保存。', { exact: false })).toBeVisible();
  const script = dialog.locator('pre[aria-label="Linux 安装指令"]'); await expect(script).toBeVisible(); const command = (await script.textContent())!;
  expect(command).toContain(`NEWNEXUS_AGENT_ENROLLMENT_TOKEN="${token}"`); expect(command).toContain(`NEWNEXUS_AGENT_SERVER="${metadata.server}"`);
  expect(command).toContain('NEWNEXUS_AGENT_INSECURE_LOOPBACK="true"'); expect(command).not.toMatch(/<[^>]*注册码[^>]*>|从控制台复制|ENROLLMENT_TOKEN_PLACEHOLDER/);
  expect(command).toContain('case "$(uname -m)" in'); expect(command).toContain('curl --fail'); expect(command).toContain('sha256sum -c -');
  expect(command).toContain('install -m 0600'); expect(command).toContain('DynamicUser=yes'); expect(command).toContain('systemctl enable --now newnexus-agent');
  for (const artifact of metadata.artifacts) { expect(command).toContain(new URL(artifact.url, page.url()).href); expect(command).toContain(artifact.sha256); expect(artifact.sha256).toMatch(/^[a-f0-9]{64}$/); }
  await dialog.getByRole('button', { name: '复制安装指令', exact: true }).click(); await expect.poll(() => page.evaluate(async () => (await navigator.clipboard.readText()).replace(/\r\n/g, '\n'))).toBe(command);
  await server.fill('[::1]:19090'); await expect(script).toContainText('NEWNEXUS_AGENT_SERVER="[::1]:19090"'); await expect(script).not.toContainText('NEWNEXUS_AGENT_INSECURE_LOOPBACK');
  await expect(dialog.getByRole('textbox', { name: '节点上的私有 CA 路径（可选）', exact: true })).toBeVisible();
  await dialog.getByRole('button', { name: '关闭', exact: true }).first().click(); await expect(dialog).toHaveCount(0); await expect(trigger).toBeFocused();
});

test('虚拟列表前端合约：滚动卸载行时详情保持稳定', async ({ page }) => {
  const items = Array.from({ length: 200 }, (_, index) => ({ id: `00000000-0000-4000-8000-${String(index + 1).padStart(12, '0')}`, address: `1.1.${Math.floor(index / 250)}.${index % 250 + 1}`, tier: 'ranked', score: 90 - index / 100, success_rate: 1, sample_count: 20 }));
  await page.route('**/api/v1/candidates/rankings?**', (route) => route.fulfill({ json: { items, total: items.length } }));
  const errors: string[] = []; page.on('pageerror', (error) => errors.push(error.message)); await page.goto('/optimizer');
  const trigger = page.getByRole('button', { name: `${items[0].id} 的操作`, exact: true }); await trigger.click(); await page.getByRole('menuitem', { name: '查看详情', exact: true }).click();
  await expect(page.getByRole('dialog')).toBeVisible(); await expect(page.getByRole('dialog')).toContainText('1.1.0.1');
  await page.locator('[data-virtuoso-scroller]').evaluate((element) => { element.scrollTop = 1600; });
  await expect(page.getByRole('dialog')).toBeVisible(); await page.keyboard.press('Escape'); await expect(page.getByRole('dialog')).toHaveCount(0); expect(errors).toEqual([]);
});

test('发布前端合约：大范围删除必须核对差异并持续按住，取消不会提交', async ({ page }) => {
  const id = '10000000-0000-4000-8000-000000000001'; let requests = 0;
  const release = { id, version: 7, trigger: 'manual', status: 'pending', diff: Array.from({ length: 21 }, (_, index) => ({ action: 'delete', name: `test-${index}.example.com`, type: 'A', line: 'default', before: { value: '1.1.1.1' } })) };
  await page.route('**/api/v1/releases?**', (route) => route.fulfill({ json: { items: [release], total: 1 } }));
  await page.route(`**/api/v1/releases/${id}/apply`, async (route) => { requests++; expect(route.request().postDataJSON()).toEqual({ confirmed: true }); expect(route.request().headers()['idempotency-key']).toMatch(/^release-/); await route.fulfill({ status: 202, json: { job_id: 'fixture-job' } }); });
  await page.goto('/releases'); await page.getByRole('button', { name: `${id} 的操作`, exact: true }).click(); await page.getByRole('menuitem', { name: '审查并发布', exact: true }).click();
  const dialog = page.getByRole('dialog'); await expect(dialog.getByText('21 条 DNS 变更')).toBeVisible(); const hold = dialog.getByRole('button', { name: '按住 2 秒确认清理', exact: true }); await expect(hold).toBeDisabled();
  await dialog.getByRole('checkbox').check(); await hold.focus(); await page.keyboard.down('Space'); await page.waitForTimeout(200); await page.keyboard.press('Escape'); await page.keyboard.up('Space'); expect(requests).toBe(0);
  // Escape dismisses the confirmation; reopen and require a full fresh hold.
  await expect(dialog).toHaveCount(0); await page.getByRole('button', { name: `${id} 的操作`, exact: true }).click(); await page.getByRole('menuitem', { name: '审查并发布', exact: true }).click(); await dialog.getByRole('checkbox').check();
  await hold.focus(); await page.keyboard.down('Enter'); await expect.poll(() => requests, { timeout: 4000 }).toBe(1); await page.keyboard.up('Enter'); await expect(dialog).toHaveCount(0);
});

test('SaaS前端合约：TXT和HTTP验证清单并列，证书待激活禁止切流', async ({ page }) => {
  const id = '20000000-0000-4000-8000-000000000001';
  await page.route('**/api/v1/saas/hostnames?**', (route) => route.fulfill({ json: { items: [{ id, hostname: 'customer.example.com', status: 'active', ssl_status: 'pending_validation', automation_mode: 'manual', validation_records: [{ type: 'TXT', name: '_cf-custom-hostname.customer.example.com', value: 'ownership-token', purpose: 'hostname_ownership' }, { type: 'HTTP', url: 'http://customer.example.com/.well-known/pki-validation/token.txt', body: 'http-certificate-token', purpose: 'certificate' }] }], total: 1 } }));
  await page.goto('/saas'); await page.getByRole('button', { name: 'customer.example.com 的操作', exact: true }).click(); await expect(page.getByRole('menuitem', { name: '激活并准备切流', exact: true })).toHaveAttribute('aria-disabled', 'true'); await page.getByRole('menuitem', { name: '查看详情', exact: true }).click();
  await expect(page.getByRole('heading', { name: '验证操作清单' })).toBeVisible(); await expect(page.getByRole('dialog').locator('pre').filter({ hasText: 'http-certificate-token' }).last()).toBeVisible(); await expect(page.getByRole('dialog').locator('pre').filter({ hasText: 'ownership-token' }).last()).toBeVisible();
});

test('五倍慢速弹窗与菜单来源点，减弱动效移除缩放', async ({ page }, testInfo) => {
  await page.goto('/nodes');
  const cdp = await page.context().newCDPSession(page); await cdp.send('Animation.enable'); await cdp.send('Animation.setPlaybackRate', { playbackRate: 0.2 });
  await page.getByRole('button', { name: '注册节点', exact: true }).click(); const dialog = page.getByRole('dialog'); await expect(dialog).toBeVisible();
  const frames: { scale: string; opacity: string; duration: string; property: string }[] = [];
  for (let index = 0; index < 4; index++) {
    frames.push(await dialog.evaluate((element) => { const style = getComputedStyle(element); return { scale: style.scale, opacity: style.opacity, duration: style.transitionDuration, property: style.transitionProperty }; }));
    await testInfo.attach(`dialog-slow-frame-${index}`, { body: await page.screenshot(), contentType: 'image/png' }); await page.waitForTimeout(160);
  }
  expect(frames.every((frame) => frame.duration.includes('0.25s'))).toBe(true); expect(frames.some((frame) => frame.scale !== '1' && frame.scale !== 'none')).toBe(true);
  await testInfo.attach('dialog-slow-computed-styles', { body: Buffer.from(JSON.stringify(frames, null, 2)), contentType: 'application/json' });
  await cdp.send('Animation.setPlaybackRate', { playbackRate: 1 }); await page.keyboard.press('Escape'); await expect(dialog).toHaveCount(0);
  await page.goto('/projects'); const trigger = page.getByRole('button', { name: 'E2E 网络项目 的操作', exact: true }); await trigger.click(); const menu = page.getByRole('menu');
  const origin = await menu.evaluate((element) => { const style = getComputedStyle(element); return { origin: style.transformOrigin.split(' ').map(parseFloat), width: (element as HTMLElement).offsetWidth, height: (element as HTMLElement).offsetHeight, property: style.transitionProperty }; });
  expect(origin.origin[0]).toBeGreaterThanOrEqual(-1); expect(origin.origin[0]).toBeLessThanOrEqual(origin.width + 1); expect(origin.property).toContain('opacity'); await page.keyboard.press('Escape'); await expect(trigger).toBeFocused();
  await page.emulateMedia({ reducedMotion: 'reduce' }); await page.goto('/nodes'); await page.getByRole('button', { name: '注册节点', exact: true }).click();
  const reduced = await dialog.evaluate((element) => { const style = getComputedStyle(element); return { scale: style.scale, transform: style.transform, property: style.transitionProperty }; });
  expect(['1', 'none']).toContain(reduced.scale); expect(reduced.transform).toBe('none'); expect(reduced.property).not.toMatch(/scale|transform/); await page.keyboard.press('Escape');
});

test('全局 HTTP 模板校验与保存保留请求头原名', async ({ page }) => {
  await page.goto('/settings'); const headers = page.getByLabel('默认请求头（JSON）', { exact: true }); await expect(headers).toBeVisible();
  await headers.fill('{invalid'); await page.getByRole('button', { name: '保存设置', exact: true }).click(); await expect(headers).toHaveAttribute('aria-invalid', 'true');
  await headers.fill('{"X_Edge_ID":"stable-key","X-MixedCase":"original-value"}'); await page.getByLabel('默认业务 Host', { exact: true }).fill('example.com'); await page.getByLabel('默认 TLS SNI', { exact: true }).fill('example.com'); await page.getByLabel('默认请求路径', { exact: true }).fill('/health');
  await page.getByLabel('默认预期 HTTP 状态码（JSON）', { exact: true }).fill('[200,204]'); await page.getByLabel('每轮采样次数', { exact: true }).fill('2'); await page.getByLabel('最大重定向次数', { exact: true }).fill('0');
  const response = page.waitForResponse((response) => response.url().endsWith('/api/v1/settings') && response.request().method() === 'PUT'); await page.getByRole('button', { name: '保存设置', exact: true }).click(); expect((await response).ok()).toBe(true);
  await page.reload(); await expect(headers).toHaveValue(/X_Edge_ID/); await expect(page.getByLabel('默认请求路径', { exact: true })).toHaveValue('/health');
  const saved = (await (await page.request.get('/api/v1/settings')).json()).items; const map = Object.fromEntries(saved.map((item: { key: string; value: unknown }) => [item.key, item.value]));
  expect(map['probe.headers']).toEqual({ X_Edge_ID: 'stable-key', 'X-MixedCase': 'original-value' }); expect(map['probe.expected_statuses']).toEqual([200,204]); expect(map['probe.samples']).toBe(2); expect(map['probe.max_redirects']).toBe(0);
});
