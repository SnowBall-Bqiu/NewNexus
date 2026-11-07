import type { ReactNode } from 'react';
import { useQueries, useQueryClient, type InfiniteData } from '@tanstack/react-query';
import { StatusBadge } from '@/components/common';
import { api, ApiError, formatDate, formatNumber, type ListResult, type Resource } from '@/lib/api';
import { auditFieldValue } from '@/lib/display-labels';
import { t } from '@/lib/i18n';
import type { ResourceSpec } from '@/lib/resources';

const text = (value: unknown) => value === undefined || value === null ? '' : String(value);
const commonLabels: Record<string, string> = {
  id: t('resource.resourceDetails.id.dt'), status: t('resources.projects.columns.status.label'),
  createdAt: t('resources.releases.columns.createdAt.label'), updatedAt: t('resource.resourceDetails.updatedAt.dt'),
  errorMessage: t('resource.resourceDetails.errorMessage.dt'), statusMessage: t('resource.resourceDetails.statusMessage.dt'),
  capabilities: t('resource.resourceDetails.capabilities.dt'), detectedCapabilities: t('resource.resourceDetails.capabilities.dt'),
  validationName: t('resource.resourceDetails.validationName.dt'), validationValue: t('resource.resourceDetails.validationValue.dt'),
  validationMethod: t('resources.hostnames.fields.validationMethod.label'), sslStatus: t('resources.hostnames.columns.sslStatus.label'),
  desired: t('resource.resourceDetails.desired.dt'), observed: t('resource.resourceDetails.observed.dt'),
  fingerprint: t('resource.resourceDetails.fingerprint.dt'), lastSyncedAt: t('resource.resourceDetails.lastSyncedAt.dt'),
  drifted: t('resource.resourceDetails.drifted.dt'), resourceId: t('audit.fields.resourceId'), ipAddress: t('audit.fields.ipAddress'),
};
const nodeLabels: Record<string, string> = {
  agentConcurrency: t('nodes.details.agentConcurrency'), agentMode: t('nodes.details.agentMode'),
  architecture: t('nodes.details.architecture'), backpressure: t('nodes.details.backpressure'),
  backpressureAt: t('nodes.details.backpressureAt'), backpressureRetrySeconds: t('nodes.details.backpressureRetrySeconds'),
  backpressureReason: t('nodes.details.backpressureReason'), os: t('nodes.details.os'),
  outboxBytes: t('nodes.details.outboxBytes'), outboxSize: t('nodes.details.outboxSize'), pendingTasks: t('nodes.details.pendingTasks'),
  probeSamplesFailed: t('nodes.details.probeSamplesFailed'), probeSamplesTotal: t('nodes.details.probeSamplesTotal'),
  protocolVersion: t('nodes.details.protocolVersion'), protocols: t('nodes.details.protocols'), failureRate: t('nodes.details.failureRate'),
  revokedAt: t('nodes.details.revokedAt'), capabilities: t('nodes.details.capabilities'),
};
const capabilityNames: Record<string, string> = {
  tcp: t('nodes.details.capability.tcp'), tls: t('nodes.details.capability.tls'),
  http: t('nodes.details.capability.http'), download: t('nodes.details.capability.download'),
  ipv4: 'IPv4', ipv6: 'IPv6',
};

export function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const unit = value === 0 ? 0 : Math.max(0, Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1));
  return `${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 }).format(value / 1024 ** unit)} ${units[unit]}`;
}

function listValue(value: unknown): ReactNode {
  const values = Array.isArray(value) ? value : typeof value === 'string' ? value.split(',').map((item) => item.trim()).filter(Boolean) : [];
  if (!values.length) return '—';
  return <ul className="flex flex-wrap gap-x-3 gap-y-1">{values.map((item, index) => <li key={`${text(item)}-${index}`}>{typeof item === 'string' ? capabilityNames[item] ?? item : displayValue(item)}</li>)}</ul>;
}

function displayValue(value: unknown): ReactNode {
  if (typeof value === 'boolean') return t(value ? 'common.yes' : 'common.no');
  if (value === undefined || value === null || value === '') return '—';
  if (typeof value === 'number') return Number.isFinite(value) ? formatNumber(value) : '—';
  if (Array.isArray(value) && value.every((item) => typeof item !== 'object')) return <ul className="space-y-1">{value.map((item, index) => <li key={index}>{displayValue(item)}</li>)}</ul>;
  if (typeof value === 'object') return <pre className="code-block">{JSON.stringify(value, null, 2)}</pre>;
  return String(value);
}

function NodeMembers({ value }: { value: unknown }) {
  const client = useQueryClient();
  const ids = [...new Set(Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string' && Boolean(item)) : [])];
  const cachedNodes = client.getQueriesData<InfiniteData<ListResult<Resource>>>({ queryKey: ['node-options'] }).flatMap(([, data]) => data?.pages.flatMap((page) => page.data) ?? []);
  const members = useQueries({ queries: ids.map((id) => ({
    queryKey: ['node-option', id], queryFn: () => api<Resource>(`/nodes/${encodeURIComponent(id)}`),
    initialData: () => cachedNodes.find((node) => node.id === id), staleTime: 30_000, refetchOnMount: 'always' as const, retry: false,
  })) });
  if (!ids.length) return <span className="text-muted-foreground">{t('nodes.details.noMembers')}</span>;
  return <ul className="space-y-3" aria-label={t('nodes.details.membersLabel')}>{ids.map((id, index) => {
    const query = members[index];
    const missing = query.error instanceof ApiError && query.error.status === 404;
    const node = missing ? undefined : query.data;
    return <li key={id} className="min-w-0 space-y-1">
      <div className="flex flex-wrap items-center gap-2"><span className="font-medium">{node ? text(node.name) || t('nodes.details.unnamedNode') : t(query.isPending ? 'nodes.details.memberLoading' : missing ? 'nodes.details.memberMissing' : 'nodes.details.memberUnavailable')}</span>{node && <StatusBadge value={node.status} />}</div>
      <code className="block break-all text-[11px] text-muted-foreground">{id}</code>
      {node && query.error && <small className="text-muted-foreground">{t('nodes.details.memberUnavailable')}</small>}
    </li>;
  })}</ul>;
}

function fieldValue(spec: ResourceSpec, item: Resource, key: string, value: unknown): ReactNode {
  if (spec.key === 'groups' && key === 'nodeIds') return <NodeMembers value={value} />;
  if (key.endsWith('At')) return formatDate(text(value));
  if (key === 'status' || key === 'sslStatus') return <StatusBadge value={value} />;
  if (['capabilities', 'detectedCapabilities'].includes(key) && (Array.isArray(value) || typeof value === 'string')) return listValue(value);
  if (spec.key === 'nodes') {
    if (key === 'protocols') return listValue(value);
    if (key === 'mode' || key === 'agentMode') {
      const modes: Record<string, string> = { all: 'TCP + HTTP', tcp: t('resources.nodes.fields.mode.options.tcp.label'), http: t('resources.nodes.fields.mode.options.http.label') };
      return modes[text(value)] ?? displayValue(value);
    }
    if (key === 'backpressure' && typeof value === 'boolean') return t(value ? 'nodes.details.backpressureActive' : 'nodes.details.backpressureNormal');
    if (key === 'outboxBytes' && typeof value === 'number') return <span title={`${formatNumber(value)} B`}>{formatBytes(value)}</span>;
    if (key === 'failureRate' && typeof value === 'number') return `${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 }).format(value * 100)}%`;
    if (key === 'backpressureRetrySeconds' && typeof value === 'number') return t('nodes.details.seconds', { value: formatNumber(value) });
    if (key === 'architecture') return ({ amd64: 'x86_64 / amd64', arm64: 'ARM64 / aarch64', '386': 'x86 / 386', arm: 'ARM' } as Record<string, string>)[text(value)] ?? displayValue(value);
    if (key === 'os') return ({ linux: 'Linux', windows: 'Windows', darwin: 'macOS', freebsd: 'FreeBSD' } as Record<string, string>)[text(value)] ?? displayValue(value);
  }
  return displayValue(spec.key === 'audit' ? auditFieldValue(key, item) : value);
}

export function ResourceDetails({ spec, item }: { spec: ResourceSpec; item?: Resource }) {
  if (!item) return null;
  const labels = { ...commonLabels, ...Object.fromEntries([...spec.fields, ...spec.columns].map((field) => [field.key, field.label])), ...(spec.key === 'nodes' ? nodeLabels : {}) };
  const hidden = ['credentialsEncrypted', 'credential', 'password', 'passwordHash', 'token', 'secret', 'csrfToken'];
  return <dl className="details-grid">{Object.entries(item).filter(([key]) => !hidden.includes(key)).map(([key, value]) => <div className="details-row" key={key}><dt>{labels[key] ?? key}</dt><dd>{fieldValue(spec, item, key, value)}</dd></div>)}</dl>;
}
