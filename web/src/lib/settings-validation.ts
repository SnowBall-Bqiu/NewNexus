import { t } from '@/lib/i18n';
import { normalizeDownloadOrigin, validAgentServer } from '@/lib/node-install';

export interface NumericSettingField { key: string; label: string; type?: string; min?: number; max?: number }
export const secretSettingKeys = ['alerts.smtp_password', 'alerts.webhook_secret'];
export function settingsValues(rows: readonly { key: string; value: unknown }[]): Record<string, unknown> { return Object.fromEntries(rows.map(({ key, value }) => [key, secretSettingKeys.includes(key) ? '' : value])); }
const httpMethods = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'];
export function parseSettingJson(value: unknown): unknown { return typeof value === 'string' ? JSON.parse(value) : value; }

export function validateHttpTemplate(values: Record<string, unknown>): Record<string, string> {
  const errors: Record<string, string> = {};
  if (values['probe.method'] !== undefined && !httpMethods.includes(String(values['probe.method']))) errors['probe.method'] = t('settings.validation.httpMethod');
  if (values['probe.path'] !== undefined && (typeof values['probe.path'] !== 'string' || (values['probe.path'] !== '' && !/^\/[^?#\r\n]*$/.test(values['probe.path'])))) errors['probe.path'] = t('settings.validation.httpPath');
  for (const key of ['probe.host', 'probe.sni']) if (values[key] !== undefined && (typeof values[key] !== 'string' || /[\r\n]/.test(values[key]))) errors[key] = t('settings.validation.httpText');
  if (values['probe.headers'] !== undefined) {
    try {
      const headers = parseSettingJson(values['probe.headers']);
      if (!headers || typeof headers !== 'object' || Array.isArray(headers) || Object.entries(headers).some(([key, value]) => !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(key) || typeof value !== 'string' || /[\r\n]/.test(value))) errors['probe.headers'] = t('settings.validation.httpHeaders');
    } catch { errors['probe.headers'] = t('settings.validation.httpHeaders'); }
  }
  if (values['probe.expected_statuses'] !== undefined) {
    try {
      const statuses = parseSettingJson(values['probe.expected_statuses']);
      if (!Array.isArray(statuses) || !statuses.length || statuses.length > 100 || statuses.some((value) => typeof value !== 'number' || !Number.isInteger(value) || value < 200 || value > 599)) errors['probe.expected_statuses'] = t('settings.validation.httpStatuses');
    } catch { errors['probe.expected_statuses'] = t('settings.validation.httpStatuses'); }
  }
  return errors;
}

export function settingsPayload(values: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(values).filter(([key, value]) => !secretSettingKeys.includes(key) || Boolean(value) && value !== '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022').map(([key, value]) => [key, ['probe.headers', 'probe.expected_statuses'].includes(key) ? parseSettingJson(value) : value]));
}
const maximums: Record<string, number> = {
  'probe.discovery_interval_seconds': 604800, 'probe.ranked_interval_seconds': 86400, 'probe.active_interval_seconds': 3600,
  'probe.min_samples': 1000, 'probe.freshness_seconds': 604800, 'probe.max_bytes': 16 * 1024 * 1024,
  'routing.min_nodes': 1000, 'routing.failure_rounds': 1000, 'routing.recovery_rounds': 1000, 'routing.performance_rounds': 1000,
  'routing.improvement_percent': 1000, 'routing.cooldown_seconds': 604800, 'publish.large_change_threshold': 10000,
  'retention.raw_days': 3650, 'retention.five_minute_days': 3650, 'retention.hourly_days': 3650,
};

/** Validate before JSON serialization can turn NaN into null. Zero remains valid where configured. */
export function validateSettings(values: Record<string, unknown>, fields: readonly NumericSettingField[]): Record<string, string> {
  const errors = validateHttpTemplate(values);
  if (values['agent_tls.self_signed_enabled'] !== undefined && typeof values['agent_tls.self_signed_enabled'] !== 'boolean') errors['agent_tls.self_signed_enabled'] = t('settings.addresses.invalidSelfSigned');
  for (const key of ['addresses.api_origin', 'addresses.console_origin', 'addresses.agent_endpoint']) {
    const value = values[key];
    if (value === undefined || value === '') continue;
    const valid = typeof value === 'string' && (value.trim() === '' || (key === 'addresses.agent_endpoint' ? validAgentServer(value.trim()) : Boolean(normalizeDownloadOrigin(value))));
    if (!valid) errors[key] = t(key === 'addresses.agent_endpoint' ? 'settings.addresses.invalidAgent' : 'settings.addresses.invalidOrigin');
  }
  for (const field of fields) {
    if (field.type !== 'number' || !Object.hasOwn(values, field.key)) continue;
    const value = values[field.key];
    const maximum = field.max ?? maximums[field.key];
    if (typeof value !== 'number' || !Number.isFinite(value)) errors[field.key] = t('settings.validation.number', { label: field.label });
    else if ((field.min !== undefined && value < field.min) || (maximum !== undefined && value > maximum)) errors[field.key] = t('resource.validation.outOfRange', { label: field.label });
    else if (!['probe.minimum_success_rate', 'routing.quorum_percent'].includes(field.key) && !Number.isInteger(value)) errors[field.key] = t('settings.validation.integer', { label: field.label });
  }
  const weights = values['score.weights'];
  if (weights !== undefined) {
    if (!weights || typeof weights !== 'object' || Array.isArray(weights)) errors['score.weights'] = t('settings.validation.weights');
    else {
      let total = 0;
      for (const [key, value] of Object.entries(weights)) {
        if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || value > 100) errors[`score.weights.${key}`] = t('settings.validation.weights');
        else total += value;
      }
      if (!Object.keys(errors).some((key) => key.startsWith('score.weights.')) && total === 0) errors['score.weights'] = t('settings.validation.positiveWeight');
    }
  }
  return errors;
}
