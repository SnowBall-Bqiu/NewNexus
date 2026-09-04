import { expect, test } from '@playwright/test';

const routes = ['/overview', '/projects', '/saas', '/optimizer', '/routes', '/nodes', '/releases', '/jobs', '/alerts', '/audit', '/connections', '/settings'];

test('all Kumo console routes render without horizontal overflow', async ({ page }) => {
  const browserErrors: string[] = [];
  page.on('console', (message) => {
    const text = message.text();
    const isUnhandledError = message.type() === 'error' && !text.startsWith('Failed to load resource:');
    const isKumoWarning = message.type() === 'warning' && /^\[kumo/i.test(text);
    if (isUnhandledError || isKumoWarning) browserErrors.push(text);
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));
  page.on('response', (response) => { if (response.status() >= 400) browserErrors.push(`HTTP ${response.status()} ${response.url()}`); });
  for (const route of routes) {
    await page.goto(route);
    await expect(page.locator('main')).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);
    expect(overflow, `${route} should not overflow horizontally`).toBe(false);
  }
  expect(browserErrors).toEqual([]);
});

test('theme and navigation preferences behave by viewport', async ({ page }, testInfo) => {
  await page.goto('/overview');
  await page.getByRole('radio', { name: '深色主题' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-mode', 'dark');
  await page.getByRole('radio', { name: '黑白高对比' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'mono');
  await expect(page.locator('html')).toHaveAttribute('data-mode', 'light');
  await page.getByRole('radio', { name: '深色主题' }).click();
  if (testInfo.project.name === 'mobile') {
    await page.getByRole('button', { name: '打开导航' }).click();
    await expect(page.getByRole('link', { name: '总览' })).toBeVisible();
    await page.reload();
    await expect(page.getByRole('link', { name: '总览' })).toBeHidden();
  } else {
    const sidebarWidths = await page.locator('[data-sidebar-wrapper]').evaluate((element) => {
      const styles = getComputedStyle(element);
      return {
        expanded: styles.getPropertyValue('--sidebar-width').trim(),
        collapsed: styles.getPropertyValue('--sidebar-width-icon').trim(),
      };
    });
    expect(sidebarWidths).toEqual({ expanded: '240px', collapsed: '57px' });
    const sidebarViewport = await page.locator('.newnexus-sidebar').evaluate((element) => {
      const rect = element.getBoundingClientRect();
      return {
        height: Math.round(rect.height),
        position: getComputedStyle(element).position,
        viewportHeight: window.innerHeight,
      };
    });
    expect(sidebarViewport.position).toBe('sticky');
    expect(Math.abs(sidebarViewport.height - sidebarViewport.viewportHeight)).toBeLessThanOrEqual(1);
    await page.getByRole('button', { name: /折叠导航|展开导航/ }).last().click();
    await page.reload();
    expect(await page.evaluate(() => localStorage.getItem('newnexus.sidebar.open'))).toBe('false');
  }
});

test('settings tab is represented in the URL', async ({ page }) => {
  await page.goto('/settings');
  await page.getByRole('tab', { name: 'API Key' }).click();
  await expect(page).toHaveURL(/\/settings\?tab=keys/);
});
