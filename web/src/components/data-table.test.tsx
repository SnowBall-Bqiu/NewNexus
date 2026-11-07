import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ColumnDef } from '@tanstack/react-table'
import { DataTable } from './data-table'

interface Row { id: string; name: string; score: number }
const columns: ColumnDef<Row, unknown>[] = [{ accessorKey: 'name', header: '名称' }, { accessorKey: 'score', header: '评分' }]
const rows: Row[] = [{ id: 'a', name: 'Alpha', score: 10 }, { id: 'b', name: 'Beta', score: 30 }, { id: 'c', name: 'Charlie', score: 20 }]
afterEach(cleanup)

describe('DataTable', () => {
  it('filters instantly and paginates the filtered row model', async () => {
    const user = userEvent.setup()
    render(<DataTable columns={columns} data={rows} initialPageSize={2} />)
    expect(screen.getByText('Alpha')).toBeInTheDocument()
    expect(screen.queryByText('Charlie')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '下一页' }))
    expect(screen.getByText('Charlie')).toBeInTheDocument()
    await user.type(screen.getByRole('textbox', { name: '搜索当前列表…' }), 'Beta')
    expect(screen.getByText('Beta')).toBeInTheDocument()
    expect(screen.queryByText('Charlie')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下一页' })).toBeDisabled()
    await user.clear(screen.getByRole('textbox'))
    await user.type(screen.getByRole('textbox'), '不存在')
    expect(screen.getByText('没有匹配的结果，请调整筛选条件。')).toBeInTheDocument()
  })

  it('keeps selection attached to resource IDs after sorting and API data reordering', async () => {
    const user = userEvent.setup()
    const onSelectionChange = vi.fn()
    const { rerender } = render(<DataTable columns={columns} data={rows} onSelectionChange={onSelectionChange} />)
    await user.click(screen.getByRole('checkbox', { name: '选择行 a' }))
    await user.click(screen.getByRole('button', { name: '排序：评分' }))
    const bodyRows = within(screen.getByRole('table')).getAllByRole('row').slice(1)
    expect(within(bodyRows[0]).getByText('Alpha')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '排序：评分' }))
    rerender(<DataTable columns={columns} data={[rows[2], rows[0], rows[1]]} onSelectionChange={onSelectionChange} />)
    expect(screen.getByRole('checkbox', { name: '选择行 a' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: '选择行 c' })).not.toBeChecked()
    expect(onSelectionChange).toHaveBeenLastCalledWith([rows[0]])
  })

  it('changes visible columns through the keyboard-accessible Base UI menu', async () => {
    const user = userEvent.setup()
    render(<DataTable columns={columns} data={rows} />)
    screen.getByRole('button', { name: '显示列' }).focus()
    await user.keyboard('{Enter}')
    await user.click(await screen.findByRole('menuitemcheckbox', { name: '评分' }))
    expect(screen.queryByRole('columnheader', { name: '排序：评分' })).not.toBeInTheDocument()
    expect(screen.getByText('Alpha')).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('button', { name: '显示列' })).toHaveFocus()
  })

  it('changes page size through the Base UI select and restores trigger focus', async () => {
    const user = userEvent.setup()
    render(<DataTable columns={columns} data={rows} initialPageSize={2} />)
    expect(screen.queryByText('Charlie')).not.toBeInTheDocument()
    const trigger = screen.getByRole('combobox', { name: '每页' })
    trigger.focus()
    await user.keyboard('{ArrowDown}')
    await user.click(await screen.findByRole('option', { name: '25' }))
    expect(screen.getByText('Charlie')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下一页' })).toBeDisabled()
    expect(trigger).toHaveFocus()
  })
})
