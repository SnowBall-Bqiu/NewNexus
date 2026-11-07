import { useEffect, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { toast } from 'sonner';
import { CopyButton, ErrorState } from '@/components/common';
import { AgentTLSDetails } from '@/components/agent-tls-details';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { api, formatDate, jsonBody, type Resource } from '@/lib/api';
import { t } from '@/lib/i18n';
import { buildNodeInstallCommand, isLoopbackServer, managedAgentCAFile, managedAgentTLS, manualCAInsideAgentData, normalizeDownloadOrigin, validAgentServer, type NodeInstallation, type NodeInstallOptions } from '@/lib/node-install';

type EnrollmentProps = { onClose: () => void; onCreated: () => void; redeployNode?: Resource; returnFocus?: () => HTMLElement | null };

export function NodeEnrollmentDialog({ open, onClose, onCreated, redeployNode, returnFocus }: EnrollmentProps & { open: boolean }) {
  return <Dialog open={open} onOpenChange={(value) => { if (!value) onClose(); }}>
    {open && <EnrollmentContent key={redeployNode?.id ?? 'new'} onClose={onClose} onCreated={onCreated} redeployNode={redeployNode} returnFocus={returnFocus} />}
  </Dialog>;
}

function EnrollmentContent({ onClose, onCreated, redeployNode, returnFocus }: EnrollmentProps) {
  const [name, setName] = useState(String(redeployNode?.name ?? ''));
  const [confirmed, setConfirmed] = useState(false);
  const [server, setServer] = useState('');
  const [serverEdited, setServerEdited] = useState(false);
  const [downloadOrigin, setDownloadOrigin] = useState(window.location.origin);
  const [downloadEdited, setDownloadEdited] = useState(false);
  const [arch, setArch] = useState<NodeInstallOptions['arch']>('auto');
  const [caFile, setCAFile] = useState('');
  const [tlsServerName, setTLSServerName] = useState('');
  const [result, setResult] = useState<{ token: string; expiresAt: string }>();
  const installation = useQuery({ queryKey: ['nodes', 'installation'], queryFn: () => api<NodeInstallation>('/nodes/installation'), retry: false });
  useEffect(() => { if (installation.data && !serverEdited) setServer(installation.data.server); }, [installation.data, serverEdited]);
  useEffect(() => { if (installation.data && !downloadEdited) setDownloadOrigin(installation.data.downloadOrigin || window.location.origin); }, [installation.data, downloadEdited]);
  const create = useMutation({
    mutationFn: () => redeployNode
      ? api<{ token: string; expiresAt: string }>(`/nodes/${encodeURIComponent(redeployNode.id)}/redeploy`, jsonBody({ confirmed: true, expiresInSeconds: 900 }))
      : api<{ token: string; expiresAt: string }>('/nodes/enrollment-tokens', jsonBody({ name: name.trim(), expiresInSeconds: 900 })),
    onSuccess: (value) => { setResult(value); onCreated(); },
    onError: (error) => toast.error(error.message),
  });
  const serverValid = validAgentServer(server);
  const normalizedDownloadOrigin = normalizeDownloadOrigin(downloadOrigin);
  const downloadHost = normalizedDownloadOrigin ? new URL(normalizedDownloadOrigin).hostname : '';
  const managedEnabled = installation.data?.tls?.selfSignedEnabled === true;
  let managedTLS: ReturnType<typeof managedAgentTLS> = null;
  let invalidTLS = false;
  try { managedTLS = managedAgentTLS(installation.data?.tls); } catch { invalidTLS = true; }
  const caInsideData = Boolean(redeployNode && !managedEnabled && manualCAInsideAgentData(caFile));
  const valuesValid = Boolean(name.trim()) && !invalidTLS && !caInsideData && ![name, ...(!managedEnabled ? [caFile, tlsServerName] : [])].some((value) => /[\x00-\x1f\x7f]/.test(value)) && (managedEnabled || !caFile || caFile.startsWith('/'));
  const canGenerate = serverValid && Boolean(normalizedDownloadOrigin) && valuesValid && (!redeployNode || confirmed);
  const available = installation.data?.artifacts.filter((artifact) => arch === 'auto' || artifact.arch === arch) ?? [];
  const loopback = isLoopbackServer(server) || server.startsWith('localhost:');
  const localDownload = ['localhost', '[::1]'].includes(downloadHost) || /^127\./.test(downloadHost);
  const insecureLoopback = Boolean(!managedEnabled && installation.data?.insecureLoopback && server === installation.data.server && isLoopbackServer(server));
  const consoleOrigin = normalizeDownloadOrigin(installation.data?.consoleOrigin || window.location.origin);
  let command = '';
  let commandError = '';
  if (result && installation.data && serverValid && normalizedDownloadOrigin && valuesValid && available.length) {
    try { command = buildNodeInstallCommand({ name: name.trim(), token: result.token, server, origin: normalizedDownloadOrigin, artifacts: installation.data.artifacts, arch, caFile, tlsServerName, insecureLoopback, tls: installation.data.tls, ...(redeployNode ? { redeploy: { nodeId: redeployNode.id } } : {}) }); }
    catch (error) { commandError = (error as Error).message; }
  }
  return <DialogContent className="max-w-3xl" finalFocus={returnFocus}>
    <DialogHeader><DialogTitle>{t(redeployNode ? 'nodes.redeploy.title' : 'nodes.enrollment.title')}</DialogTitle><DialogDescription>{t(redeployNode ? 'nodes.redeploy.description' : 'nodes.enrollment.description')}</DialogDescription></DialogHeader>
    <div className="stack">
      {installation.isPending && <p role="status" className="text-sm text-muted-foreground">{t('nodes.enrollment.loading')}</p>}
      {installation.error && <ErrorState error={installation.error} retry={() => { void installation.refetch(); }} />}
      <form id="node-enrollment" className="stack" onSubmit={(event) => { event.preventDefault(); if (canGenerate && available.length && !result && !create.isPending) create.mutate(); }}>
        {redeployNode && <div className="notice warning"><p>{t('nodes.redeploy.warning')}</p><p className="mt-2 text-xs">{t('nodes.redeploy.nodeId')} <code className="break-all">{redeployNode.id}</code></p>{!result && <Label className="mt-3 flex items-start gap-2 leading-5" htmlFor="node-redeploy-confirm"><Checkbox id="node-redeploy-confirm" checked={confirmed} disabled={create.isPending} onCheckedChange={(value) => setConfirmed(value === true)} /><span>{t('nodes.redeploy.confirm')}</span></Label>}</div>}
        <div className="field-grid">
          <div className="field"><Label htmlFor="node-name">{t('resources.nodes.fields.name.label')}</Label><Input id="node-name" required maxLength={200} value={name} disabled={Boolean(result) || Boolean(redeployNode)} onChange={(event) => setName(event.target.value)} placeholder="hk-edge-01" /></div>
          <div className="field"><Label htmlFor="node-arch">{t('nodes.enrollment.arch')}</Label><Select value={arch} onValueChange={(value) => { if (value) setArch(value as NodeInstallOptions['arch']); }}><SelectTrigger id="node-arch"><SelectValue>{arch === 'auto' ? t('nodes.enrollment.archAuto') : arch === 'amd64' ? 'Linux x86_64 / amd64' : 'Linux ARM64 / aarch64'}</SelectValue></SelectTrigger><SelectContent><SelectItem value="auto">{t('nodes.enrollment.archAuto')}</SelectItem><SelectItem value="amd64">Linux x86_64 / amd64</SelectItem><SelectItem value="arm64">Linux ARM64 / aarch64</SelectItem></SelectContent></Select></div>
        </div>
        <div className="field"><Label htmlFor="node-download-origin">{t('nodes.enrollment.downloadOrigin')}</Label><Input id="node-download-origin" value={downloadOrigin} required autoComplete="off" spellCheck={false} placeholder="https://console.example.com" aria-invalid={Boolean(downloadOrigin && !normalizedDownloadOrigin)} aria-describedby="node-download-origin-hint" onChange={(event) => { setDownloadEdited(true); setDownloadOrigin(event.target.value.trim()); }} /><small id="node-download-origin-hint">{t('nodes.enrollment.downloadOriginHint')}</small>{downloadOrigin && !normalizedDownloadOrigin && <small className="field-error">{t('nodes.enrollment.invalidDownloadOrigin')}</small>}</div>
        <div className="field"><Label htmlFor="node-server">{t('nodes.enrollment.server')}</Label><Input id="node-server" value={server} required autoComplete="off" spellCheck={false} placeholder="agents.example.com:443" aria-invalid={Boolean(server && !serverValid)} aria-describedby="node-server-hint" onChange={(event) => { setServerEdited(true); setServer(event.target.value.trim()); }} /><small id="node-server-hint">{t('nodes.enrollment.serverHint')}</small>{server && !serverValid && <small className="field-error">{t('nodes.enrollment.invalidServer')}</small>}</div>
        {!insecureLoopback && <div className="field-grid">
          <div className="field"><Label htmlFor="node-ca">{t('nodes.enrollment.caFile')}</Label><Input id="node-ca" value={managedEnabled ? managedAgentCAFile : caFile} disabled={managedEnabled} placeholder="/etc/newnexus/ca.pem" aria-invalid={caInsideData || (!managedEnabled && Boolean(caFile && !caFile.startsWith('/')))} aria-describedby={caInsideData ? 'node-ca-error' : undefined} onChange={(event) => setCAFile(event.target.value)} /><small>{t(managedEnabled ? 'nodes.tls.automaticCA' : 'nodes.enrollment.caHint')}</small>{caInsideData && <small id="node-ca-error" className="field-error" role="alert">{t('nodes.enrollment.caInsideData')}</small>}</div>
          <div className="field"><Label htmlFor="node-tls-name">{t('nodes.enrollment.tlsServerName')}</Label><Input id="node-tls-name" value={managedEnabled ? managedTLS?.serverName ?? '' : tlsServerName} disabled={managedEnabled} placeholder="agents.example.com" onChange={(event) => setTLSServerName(event.target.value)} /><small>{t(managedEnabled ? 'nodes.tls.automaticSNI' : 'nodes.enrollment.tlsHint')}</small></div>
        </div>}
      </form>
      <AgentTLSDetails tls={installation.data?.tls} origin={downloadOrigin} />
      {loopback && <div role="note" className="notice warning">{t('nodes.enrollment.loopbackHint')}</div>}
      {localDownload && <div role="note" className="notice warning">{t('nodes.enrollment.localDownloadHint')}</div>}
      {insecureLoopback && <p className="text-xs text-muted-foreground">{t('nodes.enrollment.insecureHint')}</p>}
      {installation.data && !available.length && <div role="alert" className="notice warning">{t('nodes.enrollment.artifactsMissing')}</div>}
      {consoleOrigin && <p className="text-xs text-muted-foreground">{t('nodes.enrollment.consoleHint')} <a className="text-primary underline underline-offset-4" href={`${consoleOrigin}/nodes`} target="_blank" rel="noreferrer">{t('settings.addresses.openConsole')}</a></p>}
      {result && <>
        <div className="notice warning">{t('nodes.enrollment.tokenExpiry', { expiresAt: formatDate(result.expiresAt) })}</div>
        <div className="flex items-start justify-between gap-3"><code className="min-w-0 break-all text-xs" aria-label={t('nodes.enrollment.tokenLabel')}>{result.token}</code><CopyButton value={result.token} label={t('nodes.enrollment.copyToken')} /></div>
        <div className="stack"><div className="flex flex-wrap items-center justify-between gap-2"><h3 className="text-sm font-medium">{t(redeployNode ? 'nodes.redeploy.installTitle' : 'nodes.enrollment.installTitle')}</h3>{command && <CopyButton value={command} label={t(redeployNode ? 'nodes.redeploy.copyCommand' : 'nodes.enrollment.copyCommand')} />}</div><p className="text-xs leading-5 text-muted-foreground">{t(redeployNode ? 'nodes.redeploy.installHint' : 'nodes.enrollment.installHint')}</p>{command && <pre className="code-block" tabIndex={0} aria-label={t(redeployNode ? 'nodes.redeploy.installTitle' : 'nodes.enrollment.installTitle')}>{command}</pre>}{commandError && <p role="alert" className="field-error">{commandError}</p>}<small className="text-muted-foreground">{t('nodes.enrollment.afterInstall')}</small></div>
      </>}
    </div>
    <DialogFooter><Button variant="outline" onClick={onClose}>{t('common.close')}</Button>{!result && <Button form="node-enrollment" type="submit" disabled={!canGenerate || !available.length || create.isPending}>{create.isPending ? t('common.loading') : t(redeployNode ? 'nodes.redeploy.generate' : 'nodes.enrollment.generate')}</Button>}</DialogFooter>
  </DialogContent>;
}
