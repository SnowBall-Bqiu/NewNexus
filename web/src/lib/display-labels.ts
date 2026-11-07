import { t } from '@/lib/i18n';

type StatusTone = 'good' | 'bad' | 'warn' | 'neutral';
const statuses: Record<string, { label: string; tone: StatusTone }> = {};
const registerStatus = (values: Record<string, string>, tone: StatusTone) => {
  for (const [value, label] of Object.entries(values)) statuses[value] = { label, tone };
};
registerStatus({ active: t('common.statuses.active'), healthy: t('common.statuses.healthy'), online: t('common.statuses.online'), applied: t('common.statuses.applied'), succeeded: t('common.statuses.succeeded'), success: t('common.statuses.success'), completed: t('common.statuses.completed'), done: t('common.statuses.completed'), validated: t('common.statuses.validated'), valid: t('common.statuses.validated'), resolved: t('common.statuses.resolved'), synced: t('common.statuses.synced'), published: t('common.statuses.published') }, 'good');
registerStatus({ failed: t('common.statuses.failed'), offline: t('common.statuses.offline'), revoked: t('common.statuses.revoked'), unhealthy: t('common.statuses.unhealthy'), critical: t('alert.severity.critical'), error: t('alert.severity.error'), rollback_failed: t('common.statuses.rollbackFailed'), provisioning_failed: t('common.statuses.provisioningFailed'), fail_stale: t('common.statuses.failStale'), unavailable: t('common.statuses.unavailable') }, 'bad');
registerStatus({ pending: t('common.statuses.pending'), open: t('common.statuses.pending'), warning: t('alert.severity.warning'), degraded: t('common.statuses.degraded'), paused: t('common.statuses.paused'), pending_validation: t('common.statuses.pendingValidation'), retrying: t('common.statuses.retrying'), limited: t('common.statuses.limited'), drifted: t('common.statuses.drifted') }, 'warn');
registerStatus({ pending_issuance: t('common.statuses.pendingIssuance'), initializing: t('common.statuses.initializing'), unknown: t('common.statuses.unknown'), queued: t('common.statuses.queued'), accepted: t('common.statuses.accepted'), running: t('common.statuses.running'), applying: t('common.statuses.applying'), acknowledged: t('common.statuses.acknowledged'), rolled_back: t('common.statuses.rolledBack'), inventory: t('common.statuses.inventory'), ranked: t('common.statuses.ranked'), standby: t('common.statuses.standby'), draft: t('common.statuses.draft'), manual: t('common.statuses.manual'), auto: t('common.statuses.auto'), disabled: t('common.statuses.disabled'), unverified: t('common.statuses.unverified'), cooldown: t('common.statuses.cooldown'), unchanged: t('common.statuses.unchanged'), deleted: t('common.statuses.deleted'), cancelled: t('overview.jobStates.cancelled.label'), info: t('alert.severity.info') }, 'neutral');

export function statusInfo(value: unknown) {
  const status = String(value ?? 'unknown');
  return Object.hasOwn(statuses, status) ? statuses[status] : { label: status, tone: 'neutral' as const };
}

const jobNames: Record<string, string> = {
  'sources.sync': t('jobs.actions.sourcesSync'), 'connections.sync': t('jobs.actions.connectionsSync'), 'connections.validate': t('jobs.actions.connectionsValidate'),
  'saas/zones.sync': t('jobs.actions.saasZonesSync'), 'saas/zones.create': t('jobs.actions.saasZonesCreate'),
  'saas/hostnames.sync': t('jobs.actions.hostnamesSync'), 'saas/hostnames.create': t('jobs.actions.hostnamesCreate'), 'saas/hostnames.delete': t('jobs.actions.hostnamesDelete'), 'saas/hostnames.activate': t('jobs.actions.hostnamesActivate'), 'saas/hostnames.resume-automation': t('jobs.actions.hostnamesResume'),
  'releases.apply': t('jobs.actions.releasesApply'), 'releases.rollback': t('jobs.actions.releasesRollback'), 'candidates.probe': t('overview.jobNames.probeCandidates'), 'candidates.aggregate': t('jobs.actions.candidatesAggregate'), 'alerts.notify': t('jobs.actions.alertsNotify'),
  sync: t('jobs.actions.sync'), create: t('jobs.actions.create'), delete: t('common.delete'), validate: t('jobs.actions.validate'), activate: t('jobs.actions.activate'), 'resume-automation': t('jobs.actions.resumeAutomation'), apply: t('jobs.actions.apply'), rollback: t('jobs.actions.rollback'), aggregate: t('jobs.actions.aggregate'), notify: t('jobs.actions.notify'), evaluate: t('jobs.actions.evaluate'),
  probe: t('overview.jobNames.probe'), probe_candidates: t('overview.jobNames.probeCandidates'), candidate_probe: t('overview.jobNames.probeCandidates'), source_sync: t('overview.jobNames.sourceSync'), sync_source: t('overview.jobNames.sourceSync'), route_publish: t('overview.jobNames.routePublish'), route_apply: t('overview.jobNames.routePublish'), optimizer_publish: t('overview.jobNames.optimizerPublish'), hostname_sync: t('overview.jobNames.hostnameSync'), saas_sync: t('overview.jobNames.saasSync'), node_rotate: t('overview.jobNames.nodeRotate'),
};

export function jobLabel(type: string, resourceType?: string) {
  const scoped = `${resourceType}.${type}`;
  return Object.hasOwn(jobNames, scoped) ? jobNames[scoped] : Object.hasOwn(jobNames, type) ? jobNames[type] : type;
}

const resourceNames: Record<string, string> = {
  admin: t('audit.resources.admin'), settings: t('nav.settings'),
  projects: t('nav.projects'), sites: t('tabs.sites'), connections: t('nav.connections'), sources: t('tabs.sources'), candidates: t('resources.candidates.title'), nodes: t('nav.nodes'), 'node-groups': t('tabs.groups'), 'probe-profiles': t('tabs.profiles'), rules: t('nav.rules'), releases: t('nav.releases'), 'saas/zones': t('tabs.saasZones'), 'saas/hostnames': t('tabs.hostnames'), 'optimized-hostnames': t('tabs.optimized'), jobs: t('nav.jobs'), alerts: t('nav.alerts'), audit: t('nav.audit'),
};
export function resourceLabel(value: string) { return Object.hasOwn(resourceNames, value) ? resourceNames[value] : value; }

const auditActions: Record<string, string> = {
  'auth.login': t('audit.actions.login'), 'totp.enable': t('audit.actions.enableTotp'),
  'settings.update': t('audit.actions.updateSettings'), update: t('resource.actionLabels.update'),
  preview: t('audit.actions.preview'), bulk: t('audit.actions.bulk'),
  revoke: t('resources.nodes.actions.revoke.label'), rotate: t('resources.nodes.actions.rotate.label'), redeploy: t('nodes.redeploy.action'),
  acknowledge: t('resources.alerts.actions.acknowledge.label'), resolve: t('resources.alerts.actions.resolve.label'),
  retry: t('resources.jobs.actions.retry.label'),
};
const auditSummaries: Record<string, string> = {
  'administrator signed in': t('audit.events.signedIn'),
  'TOTP enabled': t('audit.events.totpEnabled'),
  'resource configuration updated': t('audit.events.resourceUpdated'),
  'resource deleted': t('audit.events.resourceDeleted'),
  'resource created': t('audit.events.resourceCreated'),
  'node redeployment token issued and previous credentials revoked': t('audit.events.nodeRedeploy'),
  'DNS change preview generated': t('audit.events.dnsPreview'),
  'operation queued': t('audit.events.operationQueued'),
  'system settings updated': t('audit.events.settingsUpdated'),
  'Custom hostname created by bulk import': t('audit.events.hostnameImported'),
};

/** Translate presentation values without rewriting the original audit evidence. */
export function auditFieldValue(key: string, event: Readonly<Record<string, unknown>>): unknown {
  const value = event[key];
  if (typeof value !== 'string') return value;
  if (key === 'resourceType') return resourceLabel(value);
  if (key === 'action') return Object.hasOwn(auditActions, value) ? auditActions[value] : jobLabel(value, String(event.resourceType ?? ''));
  if (key === 'summary') {
    if (Object.hasOwn(auditSummaries, value)) return auditSummaries[value];
    const bulk = value.match(/^(\d+) of (\d+) bulk items queued$/);
    if (bulk) return t('audit.events.bulkQueued', { accepted: bulk[1], total: bulk[2] });
  }
  return value;
}
