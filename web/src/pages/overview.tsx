import { useId, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Activity, ArrowDownToLine, ArrowRight, Bell, CheckCircle2, CircleAlert, Clock3, Globe2, Layers3, Network, RefreshCw, Server, ShieldCheck } from 'lucide-react'
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { api, formatDate, formatNumber, list } from '@/lib/api'
import { t } from '@/lib/i18n'
import { jobLabel, statusInfo } from '@/lib/display-labels'

interface MetricPoint { time: string; latency: number; success: number; samples?: number }
interface Overview {
  counts?: Record<string, number>
  healthyRules?: number
  onlineNodes?: number
  latencySeries?: MetricPoint[]
  updatedAt?: string
  database?: string
  version?: string
}
interface RecentJob { id: string; type: string; resourceType?: string; status: string; progress?: number; createdAt?: string; errorMessage?: string }
interface RecentAlert { id: string; title: string; message?: string; severity: string; status: string; createdAt?: string }

const messages = {
  title: t("nav.overview"), subtitle: t("overview.subtitle"),
  refresh: t("overview.refresh"), autoRefresh: t("overview.autoRefresh"),
  route: t("nav.rules"), candidate: t("resources.candidates.title"), hostname: t("overview.hostname"), node: t("overview.node"),
  routeHint: t("overview.routeHint"), candidateHint: t("overview.candidateHint"), hostnameHint: t("overview.hostnameHint"), nodeHint: t("overview.nodeHint"),
  chartTitle: t("overview.chartTitle"), chartDescription: t("overview.chartDescription"),
  latency: t("metrics.latency"), success: t("metrics.successRate"), noMetrics: t("overview.noMetrics"), noMetricsHint: t("overview.noMetricsHint"),
  infrastructure: t("overview.infrastructure"), infrastructureHint: t("overview.infrastructureHint"),
  nodeOnline: t("overview.nodeOnline"), routeHealthy: t("overview.routeHealthy"), openAlerts: t("overview.openAlerts"), alertsLink: t("shell.alertsLabel"),
  recentJobs: t("overview.recentJobs"), recentJobsHint: t("overview.recentJobsHint"), allJobs: t("overview.allJobs"),
  recentAlerts: t("overview.recentAlerts"), recentAlertsHint: t("overview.recentAlertsHint"), allAlerts: t("overview.allAlerts"),
  noJobs: t("overview.noJobs"), noJobsHint: t("overview.noJobsHint"),
  noAlerts: t("overview.noAlerts"), noAlertsHint: t("overview.noAlertsHint"),
  loadError: t("overview.loadError"), loadErrorHint: t("overview.loadErrorHint"),
  sectionError: t("overview.sectionError"), retry: t("common.retry"), updated: t("overview.updated"), noUpdate: t("overview.noUpdate"),
  database: t("storage.database"), systemVersion: t("resources.nodes.columns.version.label"), noValue: '—',
}

function text(key: keyof typeof messages) { return messages[key] }
function metricNumber(value: number) { return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 }).format(value) }

function EmptyState({ icon, title, description }: { icon: ReactNode; title: string; description?: string }) {
  return <div className="flex min-h-44 flex-col items-center justify-center gap-2 px-6 py-8 text-center">
    <div className="mb-1 flex size-10 items-center justify-center rounded-lg border bg-muted/50 text-muted-foreground">{icon}</div>
    <p className="text-sm font-medium">{title}</p>
    {description && <p className="max-w-72 text-xs leading-5 text-muted-foreground">{description}</p>}
  </div>
}

function Metric({ title, value, hint, icon, href, loading }: { title: string; value: string; hint: string; icon: ReactNode; href: string; loading: boolean }) {
  return <Link to={href} className="group block rounded-lg border bg-card p-5 outline-none hover:border-primary/40 focus-visible:ring-2 focus-visible:ring-ring">
    <div className="mb-4 flex items-center justify-between text-muted-foreground"><span className="text-xs font-medium">{title}</span>{icon}</div>
    <p className="text-3xl font-semibold leading-none tracking-tight tabular-nums" aria-busy={loading}>{loading ? '—' : value}</p>
    <div className="mt-3 flex items-center justify-between gap-2"><p className="text-xs text-muted-foreground">{hint}</p><ArrowRight className="size-3.5 text-muted-foreground group-hover:text-primary" /></div>
  </Link>
}

function HealthBar({ label, current, total }: { label: string; current: number; total: number }) {
  const percentage = total > 0 ? Math.min(100, Math.max(0, current / total * 100)) : 0
  return <div className="space-y-2">
    <div className="flex items-center justify-between gap-2 text-xs"><span className="text-muted-foreground">{label}</span><span className="font-medium tabular-nums">{formatNumber(current)}<span className="ml-1 text-muted-foreground">/ {formatNumber(total)}</span></span></div>
    <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={label} aria-valuenow={current} aria-valuemax={Math.max(total, current, 1)} aria-valuemin={0}><div className="h-full rounded-full bg-primary" style={{ width: `${percentage}%` }} /></div>
  </div>
}

export function OverviewPage() {
  const queryClient = useQueryClient()
  const [metric, setMetric] = useState<'latency' | 'success'>('latency')
  const gradientId = `overview-chart-${useId().replace(/:/g, '')}`
  const overview = useQuery({ queryKey: ['overview'], queryFn: () => api<Overview>('/overview'), refetchInterval: 30_000 })
  const jobs = useQuery({ queryKey: ['jobs', 'overview'], queryFn: () => list<RecentJob>('/jobs?limit=5'), refetchInterval: 30_000 })
  const alerts = useQuery({ queryKey: ['alerts', 'overview'], queryFn: () => list<RecentAlert>('/alerts?limit=5&status=open'), refetchInterval: 30_000 })
  const summary = overview.data
  const counts = summary?.counts ?? {}
  const displayCount = (key: string) => summary ? formatNumber(counts[key] ?? 0) : text('noValue')
  const series = summary?.latencySeries ?? []
  // An empty hourly bucket is a gap, rather than a zero-latency measurement.
  const chartData = series.map((point) => ({ ...point, latency: point.samples === 0 ? null : point.latency, success: point.samples === 0 ? null : point.success }))
  const hasMeasurements = chartData.some((point) => point.latency !== null || point.success !== null)
  const latestPoint = [...chartData].reverse().find((point) => point[metric] !== null)
  const activeMetricValue = latestPoint?.[metric]
  const refreshing = overview.isFetching || jobs.isFetching || alerts.isFetching
  const refresh = () => { void queryClient.invalidateQueries({ queryKey: ['overview'] }); void queryClient.invalidateQueries({ queryKey: ['jobs'] }); void queryClient.invalidateQueries({ queryKey: ['alerts'] }) }

  return <div className="space-y-6">
    <div className="flex flex-wrap items-start justify-between gap-4">
      <div><h1 className="text-2xl font-semibold tracking-tight">{text('title')}</h1><p className="mt-1.5 text-sm text-muted-foreground">{text('subtitle')}</p></div>
      <div className="flex items-center gap-3"><span className="hidden items-center gap-1.5 text-xs text-muted-foreground sm:flex"><Clock3 className="size-3.5" />{text('autoRefresh')}</span><Button variant="outline" size="sm" onClick={refresh} disabled={refreshing}><RefreshCw className="mr-2 size-3.5" />{text('refresh')}</Button></div>
    </div>

    {overview.isError && <div role="alert" className="flex flex-wrap items-center gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-4"><CircleAlert className="size-5 shrink-0 text-destructive" /><div className="min-w-0 flex-1"><p className="text-sm font-medium">{text('loadError')}</p><p className="mt-1 text-xs text-muted-foreground">{text('loadErrorHint')}</p></div><Button size="sm" variant="outline" onClick={() => { void overview.refetch() }}>{text('retry')}</Button></div>}

    <div className="overview-metrics grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <Metric title={text('route')} value={displayCount('rules')} hint={`${summary ? formatNumber(summary.healthyRules ?? 0) : '—'} ${text('routeHint')}`} icon={<Network className="size-4" />} href="/rules" loading={overview.isPending} />
      <Metric title={text('candidate')} value={displayCount('candidates')} hint={text('candidateHint')} icon={<Layers3 className="size-4" />} href="/optimizer" loading={overview.isPending} />
      <Metric title={text('hostname')} value={displayCount('saasHostnames')} hint={text('hostnameHint')} icon={<Globe2 className="size-4" />} href="/saas" loading={overview.isPending} />
      <Metric title={text('node')} value={summary ? formatNumber(summary.onlineNodes ?? 0) : text('noValue')} hint={`${formatNumber(counts.nodes ?? 0)} ${text('nodeHint')}`} icon={<Server className="size-4" />} href="/nodes" loading={overview.isPending} />
    </div>

    <div className="overview-grid grid gap-4 xl:grid-cols-[minmax(0,2.15fr)_minmax(260px,1fr)]">
      <Card className="min-w-0">
        <CardHeader className="flex flex-wrap items-start justify-between gap-4 space-y-0 border-b">
          <div><CardTitle>{text('chartTitle')}</CardTitle><CardDescription className="mt-1.5">{text('chartDescription')}</CardDescription></div>
          <div className="inline-flex rounded-md border bg-muted/40 p-0.5" role="group" aria-label={text('chartTitle')}>
            {(['latency', 'success'] as const).map((key) => <Button key={key} variant={metric === key ? 'secondary' : 'ghost'} size="sm" className="h-7 px-2.5 text-xs" aria-pressed={metric === key} onClick={() => setMetric(key)}>{text(key)}</Button>)}
          </div>
        </CardHeader>
        <CardContent className="pt-5">
          <div className="mb-2 flex items-baseline gap-1.5"><span className="text-2xl font-semibold tracking-tight tabular-nums">{activeMetricValue == null ? '—' : metricNumber(activeMetricValue)}</span><span className="text-xs text-muted-foreground">{metric === 'latency' ? 'ms' : '%'}</span><span className="ml-auto text-xs text-muted-foreground">{latestPoint?.time ?? '—'} UTC</span></div>
          {overview.isPending ? <div className="flex h-56 items-center justify-center text-sm text-muted-foreground" role="status">{t('table.loading', '正在加载数据…')}</div> : hasMeasurements ? <div className="h-56 w-full min-w-0" aria-label={`${text(metric)} ${text('chartDescription')}`} role="img">
            <ResponsiveContainer width="100%" height="100%" minWidth={0}>
              <AreaChart data={chartData} margin={{ top: 16, left: -24, right: 8, bottom: 0 }} accessibilityLayer>
                <defs><linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor="var(--chart-1, var(--primary))" stopOpacity={0.18} /><stop offset="100%" stopColor="var(--chart-1, var(--primary))" stopOpacity={0.01} /></linearGradient></defs>
                <CartesianGrid vertical={false} stroke="var(--border)" strokeDasharray="3 4" />
                <XAxis dataKey="time" tickLine={false} axisLine={false} minTickGap={36} tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }} tickMargin={10} />
                <YAxis tickLine={false} axisLine={false} domain={metric === 'success' ? [0, 100] : [0, 'auto']} tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }} tickCount={5} />
                <Tooltip isAnimationActive={false} cursor={{ stroke: 'var(--border)' }} content={({ active, payload, label }) => active && payload?.length && payload[0].value != null ? <div className="rounded-md border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-sm"><p className="mb-1 text-muted-foreground">{label} UTC</p><p className="font-medium">{text(metric)} <span className="ml-2 tabular-nums">{metricNumber(Number(payload[0].value))} {metric === 'latency' ? 'ms' : '%'}</span></p></div> : null} />
                <Area type="linear" dataKey={metric} name={text(metric)} stroke="var(--chart-1, var(--primary))" strokeWidth={2} fill={`url(#${gradientId})`} isAnimationActive={false} connectNulls={false} dot={false} activeDot={{ r: 3, strokeWidth: 2, stroke: 'var(--background)' }} />
              </AreaChart>
            </ResponsiveContainer>
          </div> : <EmptyState icon={<Activity className="size-5" />} title={text('noMetrics')} description={text('noMetricsHint')} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="border-b"><div><CardTitle>{text('infrastructure')}</CardTitle><CardDescription>{text('infrastructureHint')}</CardDescription></div></CardHeader>
        <CardContent className="space-y-6 pt-5">
          <HealthBar label={text('nodeOnline')} current={summary?.onlineNodes ?? 0} total={counts.nodes ?? 0} />
          <HealthBar label={text('routeHealthy')} current={summary?.healthyRules ?? 0} total={counts.rules ?? 0} />
          <div className="flex items-center justify-between border-t pt-5"><div className="flex items-center gap-2 text-sm"><Bell className="size-4 text-muted-foreground" />{text('openAlerts')}<Badge variant={(counts.alerts ?? 0) > 0 ? 'warning' : 'secondary'}>{displayCount('alerts')}</Badge></div><Link className="text-xs font-medium text-primary hover:underline" to="/alerts">{text('alertsLink')}</Link></div>
          <dl className="space-y-2 border-t pt-4 text-xs"><div className="flex justify-between gap-3"><dt className="text-muted-foreground">{text('database')}</dt><dd>{summary?.database ?? '—'}</dd></div><div className="flex justify-between gap-3"><dt className="text-muted-foreground">{text('systemVersion')}</dt><dd className="font-mono">{summary?.version ?? '—'}</dd></div></dl>
        </CardContent>
      </Card>
    </div>

    <div className="grid gap-4 xl:grid-cols-2">
      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-3 space-y-0 border-b"><div><CardTitle>{text('recentJobs')}</CardTitle><CardDescription className="mt-1.5">{text('recentJobsHint')}</CardDescription></div><Link className="flex shrink-0 items-center gap-1 text-xs font-medium text-primary hover:underline" to="/jobs">{text('allJobs')}<ArrowRight className="size-3.5" /></Link></CardHeader>
        <CardContent className="p-0">
          {jobs.isError ? <p role="alert" className="p-6 text-sm text-muted-foreground">{text('sectionError')}</p> : jobs.isPending ? <p role="status" className="p-6 text-sm text-muted-foreground">{t('table.loading', '正在加载数据…')}</p> : jobs.data?.data.length ? <ul className="divide-y">{jobs.data.data.slice(0, 5).map((job) => {
            const status = statusInfo(job.status)
            const variant = status.tone === 'good' ? 'success' : status.tone === 'bad' ? 'destructive' : status.tone === 'warn' ? 'warning' : 'secondary'
            return <li key={job.id} className="flex items-center gap-3 px-5 py-3.5"><div className="flex size-8 shrink-0 items-center justify-center rounded-md border text-muted-foreground"><ArrowDownToLine className="size-4" /></div><div className="min-w-0 flex-1"><p className="truncate text-sm font-medium">{jobLabel(job.type, job.resourceType)}</p><p className="mt-1 truncate text-xs text-muted-foreground">{formatDate(job.createdAt)}{job.errorMessage ? ` · ${job.errorMessage}` : ''}</p></div><Badge variant={variant}>{status.label}</Badge></li>
          })}</ul> : <EmptyState icon={<Clock3 className="size-5" />} title={text('noJobs')} description={text('noJobsHint')} />}
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-3 space-y-0 border-b"><div><CardTitle>{text('recentAlerts')}</CardTitle><CardDescription className="mt-1.5">{text('recentAlertsHint')}</CardDescription></div><Link className="flex shrink-0 items-center gap-1 text-xs font-medium text-primary hover:underline" to="/alerts">{text('allAlerts')}<ArrowRight className="size-3.5" /></Link></CardHeader>
        <CardContent className="p-0">
          {alerts.isError ? <p role="alert" className="p-6 text-sm text-muted-foreground">{text('sectionError')}</p> : alerts.isPending ? <p role="status" className="p-6 text-sm text-muted-foreground">{t('table.loading', '正在加载数据…')}</p> : alerts.data?.data.length ? <ul className="divide-y">{alerts.data.data.slice(0, 5).map((alert) => <li key={alert.id} className="flex items-start gap-3 px-5 py-3.5"><CircleAlert className={`mt-0.5 size-4 shrink-0 ${['critical', 'error'].includes(alert.severity) ? 'text-destructive' : 'text-warning'}`} /><div className="min-w-0 flex-1"><p className="truncate text-sm font-medium">{alert.title}</p><p className="mt-1 line-clamp-2 text-xs leading-5 text-muted-foreground">{alert.message || formatDate(alert.createdAt)}</p></div><Badge variant={['critical', 'error'].includes(alert.severity) ? 'destructive' : 'warning'}>{statusInfo(alert.severity).label}</Badge></li>)}</ul> : <EmptyState icon={<ShieldCheck className="size-5" />} title={text('noAlerts')} description={text('noAlertsHint')} />}
        </CardContent>
      </Card>
    </div>
    <div className="flex items-center justify-between gap-3 border-t pt-4 text-xs text-muted-foreground"><span>{summary?.updatedAt ? `${text('updated')} ${formatDate(summary.updatedAt)}` : text('noUpdate')}</span>{summary && !overview.isError && <span className="flex items-center gap-1.5"><CheckCircle2 className="size-3.5 text-success" />{t('overview.connected', '服务已连接')}</span>}</div>
  </div>
}
