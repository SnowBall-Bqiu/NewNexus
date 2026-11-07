import { test, expect, type Page } from '@playwright/test';
import { createHmac } from 'node:crypto';
async function open(page: Page, path: string) { await page.goto(path); await expect(page.getByRole('heading', { level: 1 })).toBeVisible(); }
async function write(page: Page, path: string, data: unknown, method = 'POST') { const me = await (await page.request.get('/api/v1/auth/me')).json(); return page.request.fetch(`/api/v1${path}`, { method, data, headers: { 'X-CSRF-Token': me.csrf_token } }); }

test('所有中文功能模块可直达并刷新', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', (error) => errors.push(error.message));
  for (const path of ['/overview', '/projects', '/projects?tab=sites', '/connections', '/optimizer', '/optimizer?tab=sources', '/optimizer?tab=profiles', '/optimizer?tab=optimized', '/rules', '/saas', '/saas?tab=saasZones', '/nodes', '/nodes?tab=groups', '/releases', '/jobs', '/alerts', '/audit', '/settings', '/settings?tab=scoring', '/settings?tab=routing', '/settings?tab=retention', '/settings?tab=notifications', '/settings?tab=security']) {
    await open(page, path); await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1); await expect(page.getByText('无法连接控制面')).toHaveCount(0);
  }
  expect(errors).toEqual([]);
});

test('项目创建、键盘关闭抽屉、筛选与菜单焦点', async ({ page }) => {
  await open(page, '/projects');
  const create = page.getByRole('button', { name: '新建项目', exact: true }); await create.click();
  await page.getByLabel('项目名称').fill('E2E 网络项目'); await page.getByLabel('说明', { exact: true }).fill('真实后端持久化验证');
  await page.getByRole('button', { name: '保存配置', exact: true }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0); await expect(page.getByRole('button', { name: /E2E 网络项目/ }).first()).toBeVisible();
  await create.click(); await page.keyboard.press('Escape'); await expect(page.getByRole('dialog')).toHaveCount(0); await expect(create).toBeFocused();
  await page.getByPlaceholder('搜索已载入的项目…').fill('不存在的资源'); await expect(page.getByText('没有匹配的结果')).toBeVisible();
  await page.getByPlaceholder('搜索已载入的项目…').clear();
  const menu = page.getByRole('button', { name: 'E2E 网络项目 的操作', exact: true }); await menu.click(); await expect(page.getByRole('menuitem', { name: '编辑', exact: true })).toBeVisible();
  const bounds = await page.getByRole('menu').boundingBox(); expect(bounds?.x).toBeGreaterThanOrEqual(0); expect((bounds?.x ?? 0) + (bounds?.width ?? 0)).toBeLessThanOrEqual(1440);
  await page.keyboard.press('Escape'); await expect(menu).toBeFocused();
});

test('连接凭据加密边界与服务端权限', async ({ page }) => {
  await open(page, '/connections'); await page.getByRole('button', { name: '新建厂商连接', exact: true }).click();
  await page.getByRole('textbox', { name: '连接名称', exact: true }).fill('E2E Cloudflare'); await page.getByLabel('Cloudflare API Token', { exact: true }).fill('e2e-sensitive-token-never-return');
  await page.getByRole('button', { name: '保存配置', exact: true }).click(); await expect(page.getByRole('dialog')).toHaveCount(0);
  const connections = await (await page.request.get('/api/v1/connections')).text(); expect(connections).not.toContain('e2e-sensitive-token-never-return'); expect(connections).not.toContain('credentials_encrypted');
  const missingCSRF = await page.request.post('/api/v1/projects', { data: { name: 'bad' } }); expect(missingCSRF.status()).toBe(403);
  const readOnly = await write(page, '/releases', { status: 'applied', diff: [] }); expect(readOnly.ok()).toBe(false);
});

test('IP来源同步生成真实候选，私网订阅被拒绝', async ({ page }) => {
  await open(page, '/optimizer?tab=sources'); await page.getByRole('button', { name: '新建候选来源', exact: true }).click();
  await page.getByRole('textbox', { name: '来源名称', exact: true }).fill('E2E 公网候选'); await page.getByLabel('地址或内容', { exact: true }).fill('1.1.1.1\n8.8.8.8');
  await page.getByRole('button', { name: '保存配置', exact: true }).click(); await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByRole('button', { name: 'E2E 公网候选 的操作', exact: true }).click(); await page.getByRole('menuitem', { name: '立即同步候选' }).click();
  await expect.poll(async () => (await (await page.request.get('/api/v1/candidates')).json()).total, { timeout: 20_000 }).toBeGreaterThanOrEqual(2);
  await open(page, '/optimizer'); await expect(page.getByRole('button', { name: /1\.1\.1\.1/ })).toBeVisible();
  const denied = await write(page, '/sources', { name: 'SSRF denied', type: 'url', location: 'http://169.254.169.254/latest/meta-data/' }); expect(denied.ok()).toBe(false);
});

test('明暗主题、移动导航和减弱动效可读且不溢出', async ({ page }) => {
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 950 });
    for (const theme of ['浅色主题', '深色主题']) {
      await open(page, '/overview'); await page.getByRole('radio', { name: theme, exact: true }).click();
      for (const path of ['/overview', '/optimizer', '/rules', '/saas', '/nodes']) {
        await page.goto(path); await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true);
        await page.screenshot({ path: `../output/playwright/${path.slice(1)}-${width}-${theme === '浅色主题' ? 'light' : 'dark'}.png`, fullPage: true });
      }
    }
    if (width === 390) { await page.getByRole('button', { name: '打开导航' }).click(); const nav = page.getByRole('navigation', { name: '移动端主导航' }); await expect(nav).toBeVisible(); await nav.getByRole('link', { name: 'CNAME 线路', exact: true }).click(); await expect(nav).toHaveCount(0); }
  }
  await page.emulateMedia({ reducedMotion: 'reduce' }); await open(page, '/nodes'); await page.getByRole('button', { name: '注册节点', exact: true }).click();
  const animation = await page.getByRole('dialog').evaluate((element) => ({ duration: getComputedStyle(element).transitionDuration, property: getComputedStyle(element).transitionProperty }));
  expect(animation.property).not.toContain('transform'); await page.keyboard.press('Escape');
});

test('TOTP绑定与新会话登录，恢复码只使用一次', async ({ page, browser, baseURL }) => {
  await open(page, '/settings?tab=security'); await page.getByRole('button', { name: '配置 TOTP', exact: true }).click();
  const secret = (await page.locator('pre').first().innerText()).trim();
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'; const bits = [...secret].map((c) => alphabet.indexOf(c).toString(2).padStart(5, '0')).join('');
  const key = Buffer.from(bits.match(/.{8}/g)!.map((b) => parseInt(b, 2))); const counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30_000))); const digest = createHmac('sha1', key).update(counter).digest(); const offset = digest[19] & 15; const code = ((digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).toString().padStart(6, '0');
  await page.getByLabel('验证器六位验证码', { exact: true }).fill(code); await page.getByRole('button', { name: '验证并启用', exact: true }).click();
  await expect(page.getByText('恢复码只显示这一次。请保存到安全位置，每个恢复码仅可使用一次。')).toBeVisible(); const recovery = (await page.locator('pre').first().innerText()).trim().split('\n')[0];
  const context = await browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } }); const login = await context.newPage(); await login.goto('/'); await login.getByLabel('管理员用户名', { exact: true }).fill('newnexus-e2e'); await login.getByLabel('密码', { exact: true }).fill('NewNexus-E2E-Only!26'); await login.getByLabel('验证码或恢复码', { exact: true }).fill(recovery); await login.getByRole('button', { name: '登录控制台', exact: true }).click(); await expect(login.getByRole('navigation', { name: '主导航', exact: true })).toBeVisible();
  const consumed = await page.request.post('/api/v1/auth/login', { data: { username: 'newnexus-e2e', password: 'NewNexus-E2E-Only!26', totp_code: recovery } }); expect(consumed.status()).toBe(401); await context.close();
});
