import { test, expect, type Locator, type Page } from '@playwright/test';

const nodes = [
  { id: '73000000-0000-4000-8000-000000000001', name: '香港联通主节点（多节点选择与回显验收）', status: 'online', region: '香港', carrier: '联通' },
  { id: '73000000-0000-4000-8000-000000000002', name: '东京移动备用节点', status: 'offline', region: '东京', carrier: '移动' },
  { id: '73000000-0000-4000-8000-000000000003', name: '新加坡电信观察节点', status: 'online', region: '新加坡', carrier: '电信' },
] as const;

function memberPicker(dialog: Locator) {
  return dialog.getByRole('group', { name: '成员节点', exact: true });
}

async function selectTwoNodes(picker: Locator) {
  const first = picker.getByRole('checkbox', { name: nodes[0].name, exact: true });
  const second = picker.getByRole('checkbox', { name: nodes[1].name, exact: true });
  await first.focus();
  await first.press('Space');
  await expect(first).toBeChecked();
  await first.press('Space');
  await expect(first).not.toBeChecked();
  await first.press('Space');
  await expect(first).toBeChecked();
  await second.check();
  await expect(second).toBeChecked();
  await expect(picker.getByText('已选择 2 个节点', { exact: true })).toBeVisible();
  await expect(picker.getByText('香港 · 联通', { exact: true })).toBeVisible();
  await expect(picker.getByText('东京 · 移动', { exact: true })).toBeVisible();
  await expect(picker.getByText('离线', { exact: true })).toBeVisible();
}

async function expectNoHorizontalOverflow(page: Page, dialog: Locator) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
  const bounds = await dialog.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return { left: rect.left, right: rect.right, viewportWidth: innerWidth, width: element.clientWidth, contentWidth: element.scrollWidth };
  });
  expect(bounds.left).toBeGreaterThanOrEqual(0);
  expect(bounds.right).toBeLessThanOrEqual(bounds.viewportWidth + 1);
  expect(bounds.contentWidth).toBeLessThanOrEqual(bounds.width + 1);
  const picker = memberPicker(dialog);
  expect(await picker.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);
  await expect(picker.getByRole('textbox', { name: '搜索节点', exact: true })).toBeVisible();
}

async function editGroup(page: Page, name: string) {
  const trigger = page.getByRole('button', { name: `${name} 的操作`, exact: true });
  await trigger.click();
  await page.getByRole('menuitem', { name: '编辑', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  return { trigger, dialog, picker: memberPicker(dialog) };
}

test('节点组多选通过真实后台保存字符串ID，搜索保留已选且明暗移动布局与键盘可用', async ({ page }, testInfo) => {
  // Only the existing-node catalogue is mocked. Group CRUD goes to the isolated
  // E2E server, so persisted node_ids and editing defaults cross the real API.
  await page.route(/\/api\/v1\/nodes(?:\?|\/|$)/, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() !== 'GET') { await route.continue(); return; }
    if (url.pathname === '/api/v1/nodes') {
      const search = (url.searchParams.get('q') ?? '').trim().toLocaleLowerCase();
      const items = nodes.filter((node) => !search || `${node.id} ${node.name} ${node.region} ${node.carrier}`.toLocaleLowerCase().includes(search));
      await route.fulfill({ json: { items, total: items.length } });
      return;
    }
    const node = nodes.find((item) => url.pathname === `/api/v1/nodes/${item.id}`);
    if (node) { await route.fulfill({ json: node }); return; }
    await route.continue();
  });

  const groupName = `E2E 节点多选 ${testInfo.parallelIndex}-${Date.now()}`;
  let groupId: string | undefined;
  try {
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 950 });
      for (const theme of ['浅色主题', '深色主题']) {
        await page.goto('/nodes?tab=groups');
        await page.getByRole('radio', { name: theme, exact: true }).click();
        const trigger = page.getByRole('button', { name: '新建节点组', exact: true });
        await trigger.click();
        const dialog = page.getByRole('dialog');
        await expect(dialog).toBeVisible();
        await dialog.getByRole('textbox', { name: '节点组名称', exact: true }).fill(groupName);
        await selectTwoNodes(memberPicker(dialog));
        await expectNoHorizontalOverflow(page, dialog);
        const screenshot = `node-group-picker-${width}-${theme === '浅色主题' ? 'light' : 'dark'}`;
        await testInfo.attach(screenshot, { body: await page.screenshot({ path: testInfo.outputPath(`${screenshot}.png`), animations: 'disabled' }), contentType: 'image/png' });
        await page.keyboard.press('Escape');
        await expect(dialog).toHaveCount(0);
        await expect(trigger).toBeFocused();
      }
    }

    await page.getByRole('button', { name: '新建节点组', exact: true }).click();
    const createDialog = page.getByRole('dialog');
    await createDialog.getByRole('textbox', { name: '节点组名称', exact: true }).fill(groupName);
    await selectTwoNodes(memberPicker(createDialog));
    const createResponse = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/v1/node-groups' && response.request().method() === 'POST');
    await createDialog.getByRole('button', { name: '保存配置', exact: true }).click();
    const created = await createResponse;
    expect(created.ok()).toBe(true);
    const createPayload = created.request().postDataJSON() as { node_ids: unknown };
    expect(createPayload.node_ids).toEqual([nodes[0].id, nodes[1].id]);
    expect(Array.isArray(createPayload.node_ids) && createPayload.node_ids.every((id) => typeof id === 'string')).toBe(true);
    groupId = (await created.json() as { id: string }).id;
    expect(groupId).toBeTruthy();
    await expect(createDialog).toHaveCount(0);
    const persisted = await page.request.get(`/api/v1/node-groups/${groupId}`);
    expect(persisted.ok()).toBe(true);
    expect((await persisted.json()).node_ids).toEqual([nodes[0].id, nodes[1].id]);

    const editing = await editGroup(page, groupName);
    await expect(editing.picker.getByRole('checkbox', { name: nodes[0].name, exact: true })).toBeChecked();
    await expect(editing.picker.getByRole('checkbox', { name: nodes[1].name, exact: true })).toBeChecked();
    await expect(editing.picker.getByText('已选择 2 个节点', { exact: true })).toBeVisible();
    const search = editing.picker.getByRole('textbox', { name: '搜索节点', exact: true });
    await search.fill('新加坡');
    await expect(editing.picker.getByRole('checkbox', { name: nodes[2].name, exact: true })).toBeVisible();
    await expect(editing.picker.getByRole('checkbox', { name: nodes[0].name, exact: true })).toHaveCount(0);
    await expect(editing.picker.getByRole('checkbox', { name: nodes[1].name, exact: true })).toHaveCount(0);
    await expect(editing.picker.getByText('已选择 2 个节点', { exact: true })).toBeVisible();
    const selected = editing.picker.getByRole('list', { name: '已选择的节点', exact: true });
    await expect(selected.getByRole('button', { name: `移除 ${nodes[0].name}`, exact: true })).toBeVisible();
    await expect(selected.getByRole('button', { name: `移除 ${nodes[1].name}`, exact: true })).toBeVisible();

    await selected.getByRole('button', { name: `移除 ${nodes[0].name}`, exact: true }).click();
    await expect(editing.picker.getByText('已选择 1 个节点', { exact: true })).toBeVisible();
    await expect(selected.getByRole('button', { name: `移除 ${nodes[0].name}`, exact: true })).toHaveCount(0);
    await search.clear();
    await expect(editing.picker.getByRole('checkbox', { name: nodes[0].name, exact: true })).not.toBeChecked();
    await expect(editing.picker.getByRole('checkbox', { name: nodes[1].name, exact: true })).toBeChecked();
    const updateResponse = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/node-groups/${groupId}` && response.request().method() === 'PUT');
    await editing.dialog.getByRole('button', { name: '保存配置', exact: true }).click();
    const updated = await updateResponse;
    expect(updated.ok()).toBe(true);
    expect(updated.request().postDataJSON().node_ids).toEqual([nodes[1].id]);
    await expect(editing.dialog).toHaveCount(0);

    await page.reload();
    const reloaded = await editGroup(page, groupName);
    await expect(reloaded.picker.getByText('已选择 1 个节点', { exact: true })).toBeVisible();
    await expect(reloaded.picker.getByRole('checkbox', { name: nodes[0].name, exact: true })).not.toBeChecked();
    await expect(reloaded.picker.getByRole('checkbox', { name: nodes[1].name, exact: true })).toBeChecked();
    await reloaded.picker.getByRole('button', { name: '清空选择', exact: true }).click();
    await expect(reloaded.picker.getByText('已选择 0 个节点', { exact: true })).toBeVisible();
    await expect(reloaded.picker.getByRole('list', { name: '已选择的节点', exact: true })).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(reloaded.dialog).toHaveCount(0);
    await expect(reloaded.trigger).toBeFocused();
    const afterCancel = await page.request.get(`/api/v1/node-groups/${groupId}`);
    expect(afterCancel.ok()).toBe(true);
    expect((await afterCancel.json()).node_ids).toEqual([nodes[1].id]);
  } finally {
    if (groupId) {
      const meResponse = await page.request.get('/api/v1/auth/me');
      expect(meResponse.ok()).toBe(true);
      const me = await meResponse.json() as { csrf_token: string };
      const deleted = await page.request.delete(`/api/v1/node-groups/${groupId}`, { headers: { 'X-CSRF-Token': me.csrf_token } });
      expect(deleted.ok()).toBe(true);
    }
  }
});
