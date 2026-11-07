import { test, expect } from '@playwright/test';

test('节点安装长指令在桌面和390px明暗主题中可读，关闭后恢复注册按钮焦点', async ({ page }, testInfo) => {
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 });
    for (const theme of ['浅色主题', '深色主题']) {
      await page.goto('/nodes'); await page.getByRole('radio', { name: theme, exact: true }).click();
      const trigger = page.getByRole('button', { name: '注册节点', exact: true }); await trigger.click();
      const dialog = page.getByRole('dialog'); await expect(dialog).toBeVisible();
      const name = dialog.getByRole('textbox', { name: '节点名称', exact: true }); await name.fill(`e2e-layout-${width}-${theme}`);
      const generate = dialog.getByRole('button', { name: '生成安装指令', exact: true }); await expect(generate).toBeEnabled(); await generate.click();
      const script = dialog.locator('pre[aria-label="Linux 安装指令"]'); await expect(script).toContainText('systemctl enable --now newnexus-agent');
      await script.focus(); await expect(script).toBeFocused();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true);
      const bounds = await dialog.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: element.clientWidth, contentWidth: element.scrollWidth, viewportWidth: innerWidth, viewportHeight: innerHeight };
      });
      expect(bounds.left).toBeGreaterThanOrEqual(0); expect(bounds.right).toBeLessThanOrEqual(bounds.viewportWidth);
      expect(bounds.top).toBeGreaterThanOrEqual(0); expect(bounds.bottom).toBeLessThanOrEqual(bounds.viewportHeight);
      expect(bounds.contentWidth).toBeLessThanOrEqual(bounds.width + 1);
      const screenshot = `node-installation-${width}-${theme === '浅色主题' ? 'light' : 'dark'}`;
      await testInfo.attach(screenshot, { body: await page.screenshot({ path: testInfo.outputPath(`${screenshot}.png`) }), contentType: 'image/png' });
      await page.keyboard.press('Escape'); await expect(dialog).toHaveCount(0); await expect(trigger).toBeFocused();
    }
  }
});
