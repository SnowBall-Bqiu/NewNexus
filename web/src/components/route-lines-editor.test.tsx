import { useState } from 'react';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { list } from '@/lib/api';
import { RouteLinesEditor } from './route-lines-editor';

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), list: vi.fn() }));
beforeEach(() => { vi.mocked(list).mockImplementation(async (path) => path === '/settings' ? { data: [{ key: 'routing.min_nodes', value: 3 }, { key: 'routing.quorum_percent', value: 80 }], total: 2 } as never : { data: [], total: 0 }); });
afterEach(cleanup);

function Editor({ initial = [], changed = () => {} }: { initial?: Record<string, unknown>[]; changed?: (value: unknown) => void }) {
  const [value, setValue] = useState<unknown>(initial);
  return <form aria-label="Rule"><RouteLinesEditor value={value} values={{}} onChange={(next) => { setValue(next); changed(next); }} /></form>;
}
function renderEditor(initial?: Record<string, unknown>[], changed?: (value: unknown) => void) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}><Editor initial={initial} changed={changed} /></QueryClientProvider>);
}

describe('line threshold inheritance', () => {
  it('creates a line without baking current global thresholds into its payload', async () => {
    const user = userEvent.setup(); const changed = vi.fn(); renderEditor([], changed);
    await user.click(screen.getByRole('button', { name: '添加线路' }));
    const line = changed.mock.lastCall?.[0][0];
    expect(line).not.toHaveProperty('minNodes'); expect(line).not.toHaveProperty('quorumPercent');
    await waitFor(() => expect(screen.getByRole('spinbutton', { name: '最少仲裁节点（可选）' })).toHaveAttribute('placeholder', '继承全局（3）'));
    expect(screen.getByRole('spinbutton', { name: '同意比例（%，可选）' })).toHaveAttribute('placeholder', '继承全局（80）');
  });
  it('clears existing overrides explicitly and accepts valid custom thresholds', async () => {
    const user = userEvent.setup(); const changed = vi.fn(); renderEditor([{ id: 'line', alias: 'Default', providerLineCode: 'default', minNodes: 4, quorumPercent: 90, targets: [] }], changed);
    const minNodes = screen.getByRole('spinbutton', { name: '最少仲裁节点（可选）' });
    await user.clear(minNodes);
    expect(changed.mock.lastCall?.[0][0]).toMatchObject({ minNodes: null, quorumPercent: 90 });
    await user.type(minNodes, '3');
    expect(changed.mock.lastCall?.[0][0]).toMatchObject({ minNodes: 3 });
    const quorum = screen.getByRole('spinbutton', { name: '同意比例（%，可选）' });
    await user.clear(quorum);
    expect(changed.mock.lastCall?.[0][0]).toMatchObject({ quorumPercent: null });
    expect((screen.getByRole('form', { name: 'Rule' }) as HTMLFormElement).checkValidity()).toBe(true);
  });
  it('blocks thresholds below the configured safety bounds with an accessible error', async () => {
    const user = userEvent.setup(); renderEditor([{ id: 'line', alias: 'Default', providerLineCode: 'default', targets: [] }]);
    const quorum = screen.getByRole('spinbutton', { name: '同意比例（%，可选）' });
    await user.type(quorum, '50');
    expect(quorum).toHaveAttribute('aria-invalid', 'true');
    expect(quorum).toHaveAccessibleDescription(/至少为三分之二/);
    expect((screen.getByRole('form', { name: 'Rule' }) as HTMLFormElement).checkValidity()).toBe(false);
  });
});
