import type { Resource } from '@/lib/api';
import { t } from '@/lib/i18n';

const record = (value: unknown): Record<string, unknown> => value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
export interface SaasCapability { available: boolean; reason: string; customOrigin: boolean; customOriginReason: string; detail?: string }

/** Only a capability returned for this zone authorizes its operations. */
export function saasCapability(connection: Resource | undefined, zoneId: string): SaasCapability {
  const capabilities = record(record(connection?.zoneCapabilities)[zoneId]);
  const detail = String(record(connection?.capabilityErrors)[zoneId] ?? '');
  const available = connection?.provider === 'cloudflare' && capabilities.saas === true;
  const customOrigin = available && capabilities.customOrigin === true;
  const reason = !connection || !zoneId ? t('saas.capability.selectZone') : connection.provider !== 'cloudflare' ? t('saas.capability.providerUnsupported') : capabilities.saas === false ? t('saas.capability.unavailable') : !available ? t('saas.capability.unknown') : '';
  return { available, reason, customOrigin, customOriginReason: !available ? reason : capabilities.customOrigin === false ? t('saas.capability.customOriginUnavailable') : !customOrigin ? t('saas.capability.customOriginUnknown') : '', detail };
}

export function resolveSaasCapability(kind: string, item: Record<string, unknown>, connections: Resource[], zones: Resource[]): SaasCapability {
  const zone = kind === 'hostnames' ? zones.find((zone) => zone.id === item.saasZoneId) : item;
  return saasCapability(connections.find((connection) => connection.id === zone?.connectionId), String(zone?.zoneId ?? ''));
}
