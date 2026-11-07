import { test, expect } from '@playwright/test';
import { mkdir } from 'node:fs/promises';
test('首次安装创建唯一管理员并进入真实控制面', async ({ page }) => {
  const status = await page.request.get('/api/v1/bootstrap/status');
  const initialized = (await status.json()).initialized;
  await page.goto('/');
  await page.getByLabel('管理员用户名', { exact: true }).fill('newnexus-e2e');
  await page.getByLabel('密码', { exact: true }).fill('NewNexus-E2E-Only!26');
  if (!initialized) await page.getByLabel('确认密码', { exact: true }).fill('NewNexus-E2E-Only!26');
  await page.getByRole('button', { name: initialized ? '登录控制台' : '创建管理员并进入控制台', exact: true }).click();
  await expect(page.getByRole('navigation', { name: '主导航', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: '运行总览', exact: true })).toBeVisible();
  const duplicate = await page.request.post('/api/v1/bootstrap', { data: { username: 'second-admin', password: 'Another-E2E-Only!26' } }); expect(duplicate.ok()).toBe(false);
  await mkdir('../.cache', { recursive: true }); await page.context().storageState({ path: '../.cache/e2e-state.json' });
});
