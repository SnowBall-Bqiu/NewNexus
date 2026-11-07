import { useQuery } from '@tanstack/react-query';
import { useCallback } from 'react';
import { list, type Resource } from '@/lib/api';
import { resolveSaasCapability, type SaasCapability } from '@/lib/saas-capability';
import { t } from '@/lib/i18n';

export function useSaasCapabilityInventory(kind: string, enabled = true) {
  const active = enabled && ['saasZones', 'hostnames'].includes(kind);
  const connections = useQuery({ queryKey: ['options', '/connections'], queryFn: () => list<Resource>('/connections?limit=500'), enabled: active, staleTime: 30_000 });
  const zones = useQuery({ queryKey: ['options', '/saas/zones'], queryFn: () => list<Resource>('/saas/zones?limit=500'), enabled: active && kind === 'hostnames', staleTime: 30_000 });
  const capability = useCallback((item: Record<string, unknown>) => resolveSaasCapability(kind, item, connections.data?.data ?? [], zones.data?.data ?? []), [kind, connections.data, zones.data]);
  return {
    capability,
    error: connections.error ?? zones.error,
  };
}

export function SaasCapabilityNotice({ capability }: { capability: SaasCapability }) {
  return <>{!capability.available && <div className="notice warning" role="status"><div><p>{capability.reason}</p><p className="mt-1">{t('saas.capability.syncHint')}</p>{capability.detail && <p className="mt-1">{capability.detail}</p>}</div></div>}{capability.available && capability.detail && <div className="notice warning"><span>{capability.detail}</span></div>}</>;
}
