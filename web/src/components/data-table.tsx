import { forwardRef, useEffect, useId, useMemo, useRef, useState, type ComponentProps, type ReactNode } from 'react'
import { ArrowDown, ArrowUp, ArrowUpDown, ChevronLeft, ChevronRight, Search, SlidersHorizontal } from 'lucide-react'
import {
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type RowSelectionState,
  type SortingState,
  type VisibilityState,
} from '@tanstack/react-table'
import { TableVirtuoso, type TableComponents } from 'react-virtuoso'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRoot, TableRow } from '@/components/ui/table'
import { t } from '@/lib/i18n'

export interface DataTableProps<TData extends object> {
  columns: ColumnDef<TData, unknown>[]
  data: TData[]
  getRowId?: (row: TData, index: number) => string
  searchPlaceholder?: string
  virtualized?: boolean
  isLoading?: boolean
  toolbar?: ReactNode
  emptyMessage?: string
  enableSelection?: boolean
  onSelectionChange?: (rows: TData[]) => void
  initialPageSize?: number
}

function defaultRowId<TData extends object>(row: TData, index: number) {
  const id = (row as { id?: unknown }).id
  return typeof id === 'string' || typeof id === 'number' ? String(id) : String(index)
}

const virtualComponents: TableComponents<unknown> = {
  Table: ({ style, ...props }) => <TableRoot {...props} style={{ ...style, borderCollapse: 'separate', borderSpacing: 0 }} />,
  TableHead: forwardRef<HTMLTableSectionElement, ComponentProps<'thead'>>((props, ref) => <TableHeader {...props} ref={ref} className="bg-background" />),
  TableBody: forwardRef<HTMLTableSectionElement, ComponentProps<'tbody'>>((props, ref) => <TableBody {...props} ref={ref} />),
  TableRow: ({ item, ...props }) => <TableRow {...props} data-state={(item as { getIsSelected?: () => boolean } | undefined)?.getIsSelected?.() ? 'selected' : undefined} />,
}

/** Keep row dialogs and sheets mounted at page level, outside virtualized cells. */
export function DataTable<TData extends object>({
  columns,
  data,
  getRowId = defaultRowId,
  searchPlaceholder = t('table.search', '搜索当前列表…'),
  virtualized = false,
  isLoading = false,
  toolbar,
  emptyMessage = t('table.empty', '暂无数据'),
  enableSelection = true,
  onSelectionChange,
  initialPageSize = 25,
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useState<SortingState>([])
  const [globalFilter, setGlobalFilter] = useState('')
  const [columnVisibility, setColumnVisibility] = useState<VisibilityState>({})
  const [rowSelection, setRowSelection] = useState<RowSelectionState>({})
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: initialPageSize })
  const selectionCallback = useRef(onSelectionChange)
  selectionCallback.current = onSelectionChange

  const tableColumns = useMemo<ColumnDef<TData, unknown>[]>(() => enableSelection ? [{
    id: 'select',
    size: 40,
    enableSorting: false,
    enableHiding: false,
    enableGlobalFilter: false,
    header: ({ table }) => <Checkbox
      aria-label={t('table.selectPage', '选择当前页')}
      checked={table.getIsAllPageRowsSelected() || (table.getIsSomePageRowsSelected() && 'indeterminate')}
      onCheckedChange={(checked) => table.toggleAllPageRowsSelected(checked)}
    />,
    cell: ({ row }) => <Checkbox
      aria-label={`${t('table.selectRow', '选择行')} ${row.id}`}
      checked={row.getIsSelected()}
      onCheckedChange={(checked) => row.toggleSelected(checked)}
    />,
  }, ...columns] : columns, [columns, enableSelection])

  const table = useReactTable({
    sortDescFirst: false,
    data,
    columns: tableColumns,
    getRowId,
    state: { sorting, globalFilter, columnVisibility, rowSelection, pagination },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    onColumnVisibilityChange: setColumnVisibility,
    onRowSelectionChange: setRowSelection,
    onPaginationChange: setPagination,
    enableRowSelection: enableSelection,
    globalFilterFn: (row, columnId, value: string) => String(row.getValue(columnId) ?? '').toLocaleLowerCase().includes(value.toLocaleLowerCase()),
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  })

  useEffect(() => {
    selectionCallback.current?.(data.filter((row, index) => rowSelection[getRowId(row, index)]))
  }, [data, getRowId, rowSelection])

  const rows = table.getRowModel().rows
  const columnCount = table.getVisibleLeafColumns().length
  const filteredCount = table.getFilteredRowModel().rows.length
  const selectedCount = table.getFilteredSelectedRowModel().rows.length
  const pageCount = Math.max(table.getPageCount(), 1)
  const tableRegion = useRef<HTMLDivElement>(null)
  const focusedElement = useRef<HTMLElement | null>(null)
  const pageSizeId = useId()

  const headers = () => table.getHeaderGroups().map((group) => <TableRow key={group.id}>
    {group.headers.map((header) => <TableHead key={header.id} style={{ width: header.column.id === 'select' ? 40 : undefined }} aria-sort={header.column.getIsSorted() === 'asc' ? 'ascending' : header.column.getIsSorted() === 'desc' ? 'descending' : undefined}>
      {header.isPlaceholder ? null : header.column.getCanSort() ? <Button
        variant="ghost"
        size="sm"
        className="-ml-2 h-8 gap-1.5 whitespace-nowrap px-2 text-xs font-medium"
        onClick={header.column.getToggleSortingHandler()}
        aria-label={`${t('table.sortBy', '排序：')}${typeof header.column.columnDef.header === 'string' ? header.column.columnDef.header : header.column.id}`}
      >
        {flexRender(header.column.columnDef.header, header.getContext())}
        {header.column.getIsSorted() === 'asc' ? <ArrowUp className="size-3.5" /> : header.column.getIsSorted() === 'desc' ? <ArrowDown className="size-3.5" /> : <ArrowUpDown className="size-3.5 text-muted-foreground" />}
      </Button> : flexRender(header.column.columnDef.header, header.getContext())}
    </TableHead>)}
  </TableRow>)

  const cells = (index: number) => rows[index].getVisibleCells().map((cell) => <TableCell key={cell.id} className="h-12">{flexRender(cell.column.columnDef.cell, cell.getContext())}</TableCell>)

  return <div className="data-table space-y-3">
    <div className="data-table-toolbar flex flex-wrap items-center gap-2">
      <div className="relative min-w-40 flex-1 sm:max-w-80">
        <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
        <Input className="pl-9" aria-label={searchPlaceholder} placeholder={searchPlaceholder} value={globalFilter} onChange={(event) => table.setGlobalFilter(event.target.value)} />
      </div>
      {toolbar}
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button variant="outline" size="sm" className="ml-auto gap-2" />}>
          <SlidersHorizontal className="size-3.5" />{t('table.columns', '显示列')}
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuGroup>
          <DropdownMenuLabel>{t('table.visibleColumns', '选择显示的列')}</DropdownMenuLabel>
          <DropdownMenuSeparator />
          {table.getAllLeafColumns().filter((column) => column.getCanHide()).map((column) => <DropdownMenuCheckboxItem
            key={column.id}
            checked={column.getIsVisible()}
            onCheckedChange={(checked) => column.toggleVisibility(checked)}
            disabled={column.getIsVisible() && table.getVisibleLeafColumns().filter((item) => item.id !== 'select').length <= 1}
          >{(column.columnDef.meta as { label?: string } | undefined)?.label ?? (typeof column.columnDef.header === 'string' ? column.columnDef.header : column.id)}</DropdownMenuCheckboxItem>)}
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
    <div ref={tableRegion} tabIndex={-1} onFocusCapture={(event) => { focusedElement.current = event.target as HTMLElement }} className="data-table-frame overflow-hidden rounded-lg border bg-card outline-none focus-visible:ring-2 focus-visible:ring-ring" aria-busy={isLoading} aria-label={t('table.dataRegion', '数据列表')}>
      {virtualized && rows.length > 0 && !isLoading ? <TableVirtuoso
        style={{ height: Math.min(Math.max(rows.length * 49 + 42, 180), 540) }}
        data={rows}
        components={virtualComponents as TableComponents<(typeof rows)[number]>}
        computeItemKey={(_index, row) => row.id}
        fixedHeaderContent={headers}
        itemContent={(index) => cells(index)}
        rangeChanged={() => {
          // A focused virtual row can disappear during keyboard or pointer scrolling.
          // Restore a stable focus target; page-level overlays retain their own focus.
          requestAnimationFrame(() => {
            if (document.activeElement === document.body && focusedElement.current && !focusedElement.current.isConnected) tableRegion.current?.focus({ preventScroll: true })
          })
        }}
      /> : <Table>
        <TableHeader>{headers()}</TableHeader>
        <TableBody>
          {isLoading ? <TableRow><TableCell colSpan={columnCount} className="h-40 text-center text-muted-foreground" role="status">{t('table.loading', '正在加载数据…')}</TableCell></TableRow> : rows.length > 0 ? rows.map((row, index) => <TableRow key={row.id} data-state={row.getIsSelected() ? 'selected' : undefined}>{cells(index)}</TableRow>) : <TableRow><TableCell colSpan={columnCount} className="h-40 text-center text-muted-foreground">{globalFilter ? t('table.noMatch', '没有匹配的结果，请调整筛选条件。') : emptyMessage}</TableCell></TableRow>}
        </TableBody>
      </Table>}
    </div>
    <div className="data-table-pagination flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
      <p role="status" aria-live="polite">{enableSelection ? `${t('table.selected', '已选')} ${selectedCount} / ${filteredCount} ${t('table.items', '项')}` : `${filteredCount} ${t('table.items', '项')}`}</p>
      <div className="flex items-center gap-2">
        <div className="hidden items-center gap-2 sm:flex"><label htmlFor={pageSizeId}>{t('table.pageSize', '每页')}</label>
          <Select value={String(pagination.pageSize)} onValueChange={(value) => { if (value) table.setPageSize(Number(value)) }}>
            <SelectTrigger id={pageSizeId} size="sm" className="w-auto min-w-16" aria-label={t('table.pageSize', '每页')}><SelectValue /></SelectTrigger>
            <SelectContent>{[...new Set([25, 50, 100, 250, initialPageSize])].sort((a, b) => a - b).map((size) => <SelectItem key={size} value={String(size)}>{size}</SelectItem>)}</SelectContent>
          </Select>
        </div>
        <span className="min-w-16 text-center tabular-nums">{Math.min(pagination.pageIndex + 1, pageCount)} / {pageCount}</span>
        <Button variant="outline" size="icon" className="size-8" aria-label={t('table.previousPage', '上一页')} disabled={!table.getCanPreviousPage() || isLoading} onClick={() => table.previousPage()}><ChevronLeft className="size-4" /></Button>
        <Button variant="outline" size="icon" className="size-8" aria-label={t('table.nextPage', '下一页')} disabled={!table.getCanNextPage() || isLoading} onClick={() => table.nextPage()}><ChevronRight className="size-4" /></Button>
      </div>
    </div>
  </div>
}
