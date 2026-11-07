import { test, expect, type Locator, type Page } from '@playwright/test';

const addressFields = [
  { key: 'addresses.api_origin', label: '默认 API 地址', value: 'https://api.addresses-e2e.newnexus.example:8443' },
  { key: 'addresses.agent_endpoint', label: '默认 Agent 连接地址', value: 'agents.addresses-e2e.newnexus.example:9443' },
  { key: 'addresses.console_origin', label: '控制台访问地址', value: 'https://console.addresses-e2e.newnexus.example:8443' },
] as const;

type AddressKey = typeof addressFields[number]['key'];
type AddressSetting = { key: AddressKey; value: string; effective_value?: string; source?: string };
type AddressSettings = Record<AddressKey, AddressSetting>;

async function readAddressSettings(page: Page): Promise<AddressSettings> {
  const response = await page.request.get('/api/v1/settings');
  expect(response.ok()).toBe(true);
  const { items } = await response.json() as { items: AddressSetting[] };
  return Object.fromEntries(addressFields.map(({ key }) => {
    const setting = items.find((item) => item.key === key);
    expect(setting, `设置接口应返回 ${key}`).toBeDefined();
    expect(typeof setting?.value).toBe('string');
    return [key, setting!];
  })) as AddressSettings;
}

async function expectReadableInput(input: Locator) {
  await expect(input).toBeVisible();
  const bounds = await input.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return { left: rect.left, right: rect.right, width: rect.width, viewportWidth: innerWidth, fontSize: Number.parseFloat(getComputedStyle(element).fontSize) };
  });
  expect(bounds.left).toBeGreaterThanOrEqual(0);
  expect(bounds.right).toBeLessThanOrEqual(bounds.viewportWidth + 1);
  expect(bounds.width).toBeGreaterThan(0);
  expect(bounds.fontSize).toBeGreaterThanOrEqual(12);
}

test('访问地址保存后刷新保留，节点注册继承三个入口且390px地址可读', async ({ page }, testInfo) => {
  // This test uses the isolated server owned by the E2E harness. Restore only
  // these three settings so other browser tests retain their original defaults.
  const original = await readAddressSettings(page);
  let enrollmentPosts = 0;
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/v1/nodes/enrollment-tokens') enrollmentPosts++;
  });

  try {
    await page.goto('/settings?tab=addresses');
    for (const field of addressFields) await page.getByRole('textbox', { name: field.label, exact: true }).fill(field.value);
    const savedResponse = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/v1/settings' && response.request().method() === 'PUT');
    await page.getByRole('button', { name: '保存设置', exact: true }).click();
    expect((await savedResponse).ok()).toBe(true);
    await expect(page.getByRole('button', { name: '保存设置', exact: true })).toBeDisabled();

    await page.reload();
    for (const field of addressFields) await expect(page.getByRole('textbox', { name: field.label, exact: true })).toHaveValue(field.value);
    const persisted = await readAddressSettings(page);
    for (const field of addressFields) {
      expect(persisted[field.key].value).toBe(field.value);
      expect(persisted[field.key].effective_value).toBe(field.value);
      expect(persisted[field.key].source).toBe('database');
    }

    const metadataResponse = await page.request.get('/api/v1/nodes/installation');
    expect(metadataResponse.ok()).toBe(true);
    const metadata = await metadataResponse.json() as { download_origin: string; server: string; console_origin: string; insecure_loopback: boolean };
    expect(metadata.download_origin).toBe(addressFields[0].value);
    expect(metadata.server).toBe(addressFields[1].value);
    expect(metadata.console_origin).toBe(addressFields[2].value);
    expect(metadata.insecure_loopback).toBe(false);

    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 950 });
      await page.goto('/settings?tab=addresses');
      for (const field of addressFields) {
        const input = page.getByRole('textbox', { name: field.label, exact: true });
        await expect(input).toHaveValue(field.value);
        await expectReadableInput(input);
      }
      const effectiveAddresses = page.locator('[aria-label="当前已生效地址"]');
      for (const field of addressFields) await expect(effectiveAddresses.getByText(field.value, { exact: true })).toBeVisible();
      await expect(effectiveAddresses.getByRole('link', { name: '打开控制台', exact: true })).toHaveAttribute('href', addressFields[2].value);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
      expect(await effectiveAddresses.locator('dd').evaluateAll((elements) => elements.every((element) => element.scrollWidth <= element.clientWidth + 1))).toBe(true);
      const settingsShot = `addresses-settings-${width}`;
      await testInfo.attach(settingsShot, { body: await page.screenshot({ path: testInfo.outputPath(`${settingsShot}.png`), fullPage: true, animations: 'disabled' }), contentType: 'image/png' });

      await page.goto('/nodes');
      const trigger = page.getByRole('button', { name: '注册节点', exact: true });
      await trigger.click();
      const dialog = page.getByRole('dialog');
      await expect(dialog).toBeVisible();
      const apiOrigin = dialog.getByRole('textbox', { name: '安装包下载入口', exact: true });
      const agentEndpoint = dialog.getByRole('textbox', { name: '控制面 gRPC 地址', exact: true });
      await expect(apiOrigin).toHaveValue(addressFields[0].value);
      await expect(agentEndpoint).toHaveValue(addressFields[1].value);
      await expectReadableInput(apiOrigin);
      await expectReadableInput(agentEndpoint);
      await expect(dialog.getByRole('link', { name: '打开控制台', exact: true })).toHaveAttribute('href', `${addressFields[2].value}/nodes`);
      await expect(dialog.getByRole('button', { name: '生成安装指令', exact: true })).toBeDisabled();
      await expect(dialog.locator('code[aria-label="一次性注册码"]')).toHaveCount(0);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
      const dialogBounds = await dialog.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        return { left: rect.left, right: rect.right, viewportWidth: innerWidth, width: element.clientWidth, contentWidth: element.scrollWidth };
      });
      expect(dialogBounds.left).toBeGreaterThanOrEqual(0);
      expect(dialogBounds.right).toBeLessThanOrEqual(dialogBounds.viewportWidth + 1);
      expect(dialogBounds.contentWidth).toBeLessThanOrEqual(dialogBounds.width + 1);
      const dialogShot = `addresses-node-defaults-${width}`;
      await testInfo.attach(dialogShot, { body: await page.screenshot({ path: testInfo.outputPath(`${dialogShot}.png`), animations: 'disabled' }), contentType: 'image/png' });
      await page.keyboard.press('Escape');
      await expect(dialog).toHaveCount(0);
      await expect(trigger).toBeFocused();
    }
    expect(enrollmentPosts).toBe(0);
  } finally {
    const meResponse = await page.request.get('/api/v1/auth/me');
    expect(meResponse.ok()).toBe(true);
    const me = await meResponse.json() as { csrf_token: string };
    const restoredResponse = await page.request.put('/api/v1/settings', {
      headers: { 'X-CSRF-Token': me.csrf_token },
      data: Object.fromEntries(addressFields.map(({ key }) => [key, original[key].value])),
    });
    expect(restoredResponse.ok()).toBe(true);
    const restored = await readAddressSettings(page);
    for (const { key } of addressFields) {
      expect(restored[key].value).toBe(original[key].value);
      expect(restored[key].effective_value).toBe(original[key].effective_value);
      expect(restored[key].source).toBe(original[key].source);
    }
  }
});
