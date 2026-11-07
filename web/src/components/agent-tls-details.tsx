import { CopyButton } from '@/components/common';
import { t } from '@/lib/i18n';
import { managedAgentTLS, normalizeDownloadOrigin, type AgentTLS, type ManagedAgentTLS } from '@/lib/node-install';

export function AgentTLSDetails({ tls, origin }: { tls?: AgentTLS; origin: string }) {
  if (tls?.selfSignedEnabled !== true) return null;
  let certificate: ManagedAgentTLS | null = null;
  try { certificate = managedAgentTLS(tls); }
  catch (error) { return <div role="alert" className="notice error">{(error as Error).message}</div>; }
  if (!certificate) return null;
  const downloadOrigin = normalizeDownloadOrigin(origin);
  const url = downloadOrigin ? new URL(certificate.caUrl, downloadOrigin).href : '';
  const details = [
    { label: t('nodes.tls.serverName'), value: certificate.serverName, copy: t('nodes.tls.copyServerName') },
    { label: t('nodes.tls.pemHash'), value: certificate.caSha256, copy: t('nodes.tls.copyPemHash') },
    { label: t('nodes.tls.fingerprint'), value: certificate.caFingerprintSha256, copy: t('nodes.tls.copyFingerprint') },
  ];
  return <section className="notice stack min-w-0 text-sm" aria-label={t('nodes.tls.detailsTitle')}>
    <strong>{t('nodes.tls.detailsTitle')}</strong>
    <p>{t('nodes.tls.activeHint')}</p>
    <dl className="grid min-w-0 gap-3">
      {details.map((detail) => <div key={detail.label} className="grid min-w-0 gap-1">
        <dt className="text-xs text-muted-foreground">{detail.label}</dt>
        <dd className="flex min-w-0 flex-wrap items-start gap-2"><code className="min-w-0 flex-1 break-all">{detail.value}</code><CopyButton value={detail.value} label={detail.copy} /></dd>
      </div>)}
      <div><dt className="text-xs text-muted-foreground">{t('nodes.tls.expires')}</dt><dd className="flex min-w-0 flex-wrap items-center gap-2"><time className="break-all" dateTime={certificate.notAfter}>{new Date(certificate.notAfter).toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' })}</time><CopyButton value={certificate.notAfter} label={t('nodes.tls.copyExpires')} /></dd></div>
    </dl>
    {url && <div className="flex flex-wrap items-center gap-2"><a className="text-primary underline underline-offset-4" href={url} download="newnexus-agent-ca.pem" target="_blank" rel="noreferrer">{t('nodes.tls.downloadCA')}</a><CopyButton value={url} label={t('nodes.tls.copyDownloadURL')} /></div>}
    <p className="text-xs text-muted-foreground">{t('nodes.tls.existingNodeHint')}</p>
  </section>;
}
