import { describe, expect, it } from 'vitest';
import { resolveSaasCapability, saasCapability } from './saas-capability';

describe('zone SaaS capabilities', () => {
  it('keeps missing, failed and unsupported capabilities unavailable', () => {
    expect(saasCapability(undefined, '').available).toBe(false);
    expect(saasCapability({ id: 'cf', provider: 'cloudflare', capabilities: { saas: true } }, 'zone').available).toBe(false);
    expect(saasCapability({ id: 'cf', provider: 'cloudflare', capabilityErrors: { zone: 'Permission denied' } }, 'zone')).toMatchObject({ available: false, detail: 'Permission denied' });
    expect(saasCapability({ id: 'other', provider: 'dnspod', zoneCapabilities: { zone: { saas: true } } }, 'zone').available).toBe(false);
  });
  it('checks custom origin independently and resolves hostname ownership through its SaaS zone', () => {
    const connection = { id: 'cf', provider: 'cloudflare', zoneCapabilities: { zone: { saas: true } } };
    const zones = [{ id: 'saas-zone', connectionId: 'cf', zoneId: 'zone' }];
    expect(resolveSaasCapability('hostnames', { saasZoneId: 'saas-zone' }, [connection], zones)).toMatchObject({ available: true, customOrigin: false });
    expect(saasCapability({ ...connection, zoneCapabilities: { zone: { saas: true, customOrigin: true } } }, 'zone')).toMatchObject({ available: true, customOrigin: true });
    expect(saasCapability(connection, 'another-zone').available).toBe(false);
  });
});
