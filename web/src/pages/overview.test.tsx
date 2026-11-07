import { cleanup, render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, list } from '@/lib/api'
import { OverviewPage } from './overview'

vi.mock('@/lib/api', async (original) => ({ ...await original<typeof import('@/lib/api')>(), api: vi.fn(), list: vi.fn() }))

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  return render(<QueryClientProvider client={client}><MemoryRouter><OverviewPage /></MemoryRouter></QueryClientProvider>)
}

beforeEach(() => {
  vi.mocked(api).mockReset()
  vi.mocked(list).mockReset()
  vi.mocked(list).mockResolvedValue({ data: [], total: 0 })
})
afterEach(cleanup)

describe('OverviewPage', () => {
  it('uses real camel-cased API metrics and shows honest empty states', async () => {
    vi.mocked(api).mockResolvedValue({ counts: { rules: 12, candidates: 2048, nodes: 4, saasHostnames: 20, alerts: 0 }, healthyRules: 11, onlineNodes: 3, latencySeries: [], database: 'sqlite', version: '0.1.0' })
    renderPage()
    expect(await screen.findByText('2,048')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /SaaS 主机名/ })).toHaveTextContent('20')
    expect(screen.getByRole('link', { name: /CNAME 线路/ })).toHaveAttribute('href', '/rules')
    expect(screen.getByText('暂无探测数据')).toBeInTheDocument()
    expect(screen.getByText('还没有作业记录')).toBeInTheDocument()
    expect(screen.getByText('没有待处理的告警')).toBeInTheDocument()
    expect(api).toHaveBeenCalledWith('/overview')
    expect(list).toHaveBeenCalledWith('/alerts?limit=5&status=open')
  })

  it('reports an API failure without inventing healthy data', async () => {
    vi.mocked(api).mockRejectedValue(new Error('服务离线'))
    renderPage()
    expect(await screen.findByText('无法获取运行数据')).toBeInTheDocument()
    expect(screen.queryByText('服务已连接')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
  })

  it('translates live job action and retry status using its resource type', async () => {
    vi.mocked(api).mockResolvedValue({ counts: {}, latencySeries: [] })
    vi.mocked(list).mockImplementation(async (path) => path.startsWith('/jobs') ? { data: [{ id: 'source-job', type: 'sync', resourceType: 'sources', status: 'retrying' }, { id: 'connection-job', type: 'sync', resourceType: 'connections', status: 'completed' }], total: 2 } as never : { data: [], total: 0 })
    renderPage()
    expect(await screen.findByText('同步候选源')).toBeInTheDocument()
    expect(screen.getByText('同步连接')).toBeInTheDocument()
    expect(screen.getByText('重试中')).toBeInTheDocument()
    expect(screen.getByText('已完成')).toBeInTheDocument()
  })
})
