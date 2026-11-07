import { t } from '@/lib/i18n';

export type AgentArtifact = { arch: 'amd64' | 'arm64'; url: string; sha256: string };
export type AgentTLS = { selfSignedEnabled: boolean; serverName?: string; caUrl?: string; caSha256?: string; caFingerprintSha256?: string; notAfter?: string };
export type ManagedAgentTLS = Required<AgentTLS>;
export type NodeInstallation = { server: string; downloadOrigin?: string; consoleOrigin?: string; insecureLoopback: boolean; artifacts: AgentArtifact[]; tls?: AgentTLS };
export type NodeInstallOptions = {
  name: string; token: string; server: string; origin: string;
  artifacts: AgentArtifact[]; arch: 'auto' | 'amd64' | 'arm64';
  insecureLoopback: boolean; caFile: string; tlsServerName: string;
  tls?: AgentTLS;
  redeploy?: { nodeId: string };
};
export const managedAgentCAFile = '/etc/newnexus/agent-ca.pem';

const agentServiceUnit = `[Unit]
Description=NewNexus outbound probe node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
DynamicUser=yes
StateDirectory=newnexus-agent
StateDirectoryMode=0700
EnvironmentFile=/etc/newnexus/agent.env
Environment=NEWNEXUS_AGENT_DATA_DIR=/var/lib/newnexus-agent
ExecStart=/usr/local/bin/newnexus-agent
Restart=on-failure
RestartSec=10
TimeoutStopSec=15
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
AmbientCapabilities=
UMask=0077

[Install]
WantedBy=multi-user.target`;

// The replacement token is bound to the existing node by the control plane. Only
// this standard installation layout is supported; custom services need manual migration.
function redeployPreflight(nodeId: string, token: string, manualCAFile: string): string {
  return `for tool in stat readlink cmp mv cp grep flock ln; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing required tool: $tool" >&2; exit 1; }
done
fail() { echo "Redeployment refused: $*" >&2; exit 1; }
safe_directory() {
  [ ! -L "$1" ] || fail "Directory is a symlink: $1"
  if [ -e "$1" ]; then
    [ -d "$1" ] && [ "$(stat -c %u "$1")" -eq 0 ] || fail "Directory must be root-owned: $1"
    directory_mode=$(stat -c %a "$1")
    [ $((0$directory_mode & 0022)) -eq 0 ] || fail "Directory is writable by another user: $1"
  fi
}
safe_file() {
  [ ! -L "$1" ] || fail "File is a symlink: $1"
  if [ -e "$1" ]; then
    [ -f "$1" ] && [ "$(stat -c %u "$1")" -eq 0 ] || fail "File must be a root-owned regular file: $1"
    file_mode=$(stat -c %a "$1")
    [ $((0$file_mode & 0022)) -eq 0 ] || fail "File is writable by another user: $1"
  fi
}
for directory in /run /usr /usr/local /usr/local/bin /etc /etc/newnexus /etc/systemd /etc/systemd/system /var /var/lib /var/lib/private /var/backups /var/backups/newnexus-agent; do safe_directory "$directory"; done
safe_file /run/newnexus-agent-redeploy.lock
exec 9>/run/newnexus-agent-redeploy.lock
flock -n 9 || fail 'Another Agent redeployment is running.'
for file in /usr/local/bin/newnexus-agent /etc/newnexus/agent.env /etc/newnexus/agent-ca.pem /etc/newnexus/agent-deployment /etc/systemd/system/newnexus-agent.service; do safe_file "$file"; done
deployment_fingerprint=$(printf '%s' ${shellQuote(token)} | sha256sum)
deployment_fingerprint=${shellQuote(nodeId)}:\${deployment_fingerprint%% *}
if [ -f /etc/newnexus/agent-deployment ] && grep -Fxq "$deployment_fingerprint" /etc/newnexus/agent-deployment; then
  fail 'This redeployment command was already applied. Generate a new command before redeploying again.'
fi
cat > "$tmp/expected.service" <<'NEWNEXUS_EXPECTED_UNIT'
${agentServiceUnit}
NEWNEXUS_EXPECTED_UNIT
if systemctl cat newnexus-agent.service >/dev/null 2>&1; then
  fragment=$(systemctl show newnexus-agent.service --property=FragmentPath --value)
  drop_ins=$(systemctl show newnexus-agent.service --property=DropInPaths --value)
  need_reload=$(systemctl show newnexus-agent.service --property=NeedDaemonReload --value)
else
  [ ! -e /etc/systemd/system/newnexus-agent.service ] || fail 'The existing service could not be inspected through systemd.'
  fragment=; drop_ins=; need_reload=
fi
[ -z "$drop_ins" ] || fail 'Custom systemd drop-ins are not supported.'
[ "$need_reload" != yes ] || fail 'Run systemctl daemon-reload and inspect the active service before redeployment.'
if [ -n "$fragment" ] || [ -e /etc/systemd/system/newnexus-agent.service ]; then
  [ "$fragment" = /etc/systemd/system/newnexus-agent.service ] || fail 'The active service has a custom unit path.'
  cmp -s /etc/systemd/system/newnexus-agent.service "$tmp/expected.service" || fail 'The service differs from the standard NewNexus unit.'
fi
if [ -f /etc/newnexus/agent.env ] && grep -Fq NEWNEXUS_AGENT_DATA_DIR /etc/newnexus/agent.env; then
  fail 'A custom Agent data directory override requires manual migration.'
fi
validate_data() {
  data_path=
  data_link=0
  [ ! -L /var/lib/private/newnexus-agent ] || fail 'Private Agent data directory is a symlink.'
  if [ -e /var/lib/private/newnexus-agent ]; then
    [ -d /var/lib/private/newnexus-agent ] || fail 'Private Agent data path is not a directory.'
    data_path=/var/lib/private/newnexus-agent
  fi
  if [ -L /var/lib/newnexus-agent ]; then
    [ "$(readlink -f /var/lib/newnexus-agent)" = /var/lib/private/newnexus-agent ] || fail 'Agent data symlink has an unexpected destination.'
    data_link=1
  elif [ -e /var/lib/newnexus-agent ]; then
    [ -d /var/lib/newnexus-agent ] && [ -z "$data_path" ] || fail 'Agent data paths are ambiguous or not directories.'
    data_path=/var/lib/newnexus-agent
  fi
}
validate_data
validate_manual_ca() {
${manualCAFile ? `  manual_ca_real=$(readlink -f -- ${shellQuote(manualCAFile)}) || fail 'The manual CA path could not be resolved. Move the CA to /etc/newnexus before redeployment.'
  case "$manual_ca_real" in
    /var/lib/newnexus-agent|/var/lib/newnexus-agent/*|/var/lib/private/newnexus-agent|/var/lib/private/newnexus-agent/*)
      fail 'The manual CA is inside Agent state that will be archived. Move it to /etc/newnexus before redeployment.' ;;
  esac` : '  :'}
}
validate_manual_ca
backup=
redeploy_started=0
data_reset=0
old_data_path=
old_data_link=0
finish_redeploy() {
  result=$?
  trap - 0
  if [ "$result" -ne 0 ] && [ "$redeploy_started" -eq 1 ]; then
    set +e
    if systemctl stop newnexus-agent; then
      # Keep failed new credentials for inspection too; never recursively delete state.
      if [ "$data_reset" -eq 1 ] && (validate_data); then
        validate_data
        if [ -n "$data_path" ]; then mv -- "$data_path" "$backup/failed-new-data"; fi
        if [ "$data_link" -eq 1 ]; then rm -f -- /var/lib/newnexus-agent; fi
        if [ -d "$backup/data" ] && [ -n "$old_data_path" ]; then mv -- "$backup/data" "$old_data_path"; fi
        if [ "$old_data_link" -eq 1 ]; then ln -s private/newnexus-agent /var/lib/newnexus-agent; fi
      elif [ "$data_reset" -eq 1 ]; then
        echo 'State paths changed unexpectedly; state was left untouched for manual recovery.' >&2
      fi
      for pair in binary:/usr/local/bin/newnexus-agent environment:/etc/newnexus/agent.env ca:/etc/newnexus/agent-ca.pem marker:/etc/newnexus/agent-deployment unit:/etc/systemd/system/newnexus-agent.service; do
        source_name=\${pair%%:*}; target=\${pair#*:}
        if [ -f "$backup/$source_name" ]; then cp -p -- "$backup/$source_name" "$target"; else rm -f -- "$target"; fi
      done
      if [ ! -f "$backup/unit" ]; then systemctl disable newnexus-agent >/dev/null 2>&1; fi
      systemctl daemon-reload
      echo 'Previous files were restored where possible; the service is stopped. Old node credentials were revoked by the control plane. Generate a new redeployment token before retrying.' >&2
    else
      echo 'Could not stop the new service; automatic rollback was skipped to preserve its files and data.' >&2
    fi
    echo "Recovery backup: $backup" >&2
  elif [ "$result" -ne 0 ] && [ -n "$backup" ]; then
    echo "Redeployment did not replace the installation. Backup: $backup" >&2
  fi
  rm -rf -- "$tmp"
  exit "$result"
}
trap finish_redeploy 0
`;
}

const redeployBackup = `# Recheck path safety after downloads, before stopping the old service.
for directory in /usr/local/bin /etc/newnexus /etc/systemd/system /var/lib /var/lib/private /var/backups /var/backups/newnexus-agent; do safe_directory "$directory"; done
for file in /usr/local/bin/newnexus-agent /etc/newnexus/agent.env /etc/newnexus/agent-ca.pem /etc/newnexus/agent-deployment /etc/systemd/system/newnexus-agent.service; do safe_file "$file"; done
validate_data
validate_manual_ca
old_data_path=$data_path
old_data_link=$data_link
if [ ! -d /var/backups ]; then install -d -m 0755 /var/backups; fi
if [ ! -d /var/backups/newnexus-agent ]; then install -d -m 0700 /var/backups/newnexus-agent; fi
[ "$(stat -c %a /var/backups/newnexus-agent)" = 700 ] || fail 'Backup parent must have permissions 0700.'
backup=$(mktemp -d /var/backups/newnexus-agent/redeploy.XXXXXXXX)
for pair in binary:/usr/local/bin/newnexus-agent environment:/etc/newnexus/agent.env ca:/etc/newnexus/agent-ca.pem marker:/etc/newnexus/agent-deployment unit:/etc/systemd/system/newnexus-agent.service; do
  source_name=\${pair%%:*}; target=\${pair#*:}
  if [ -f "$target" ]; then cp -p -- "$target" "$backup/$source_name"; fi
done
if [ -n "$fragment" ]; then
  systemctl stop newnexus-agent || { echo 'Could not stop the existing Agent; installation and state were not replaced.' >&2; exit 1; }
  case "$(systemctl show newnexus-agent.service --property=ActiveState --value)" in inactive|failed) ;; *) fail 'The existing service has not stopped.' ;; esac
fi
redeploy_started=1
if [ -n "$old_data_path" ]; then mv -- "$old_data_path" "$backup/data"; data_reset=1; fi
if [ "$old_data_link" -eq 1 ]; then rm -f -- /var/lib/newnexus-agent; data_reset=1; fi
data_reset=1
echo "Previous Agent installation backed up to: $backup"
`;

export function serverHost(server: string): string {
  return server.match(/^(?:\[([^\]]+)\]|([^:[\]]+)):\d+$/)?.slice(1).find(Boolean) ?? '';
}

export function isLoopbackServer(server: string): boolean {
  const host = serverHost(server).toLowerCase();
  return host === '::1' || /^127(?:\.\d{1,3}){3}$/.test(host);
}

/** Lexical precheck; the installer repeats this against readlink -f on the host. */
export function manualCAInsideAgentData(caFile: string): boolean {
  if (!caFile.startsWith('/')) return false;
  const segments: string[] = [];
  for (const part of caFile.split('/')) {
    if (part === '..') segments.pop();
    else if (part && part !== '.') segments.push(part);
  }
  const normalized = `/${segments.join('/')}`;
  return ['/var/lib/newnexus-agent', '/var/lib/private/newnexus-agent'].some((directory) => normalized === directory || normalized.startsWith(`${directory}/`));
}

export function validAgentServer(server: string): boolean {
  const host = serverHost(server);
  const port = Number(server.slice(server.lastIndexOf(':') + 1));
  if (!host || /[\s/\\?#@'"\x00-\x1f\x7f]/.test(server) || port < 1 || port > 65535) return false;
  if (server.startsWith('[') && !host.includes(':')) return false;
  if (host.includes(':')) {
    try { new URL(`https://[${host}]:${port}`); } catch { return false; }
    return host !== '::';
  }
  if (/^[\d.]+$/.test(host)) return host.split('.').length === 4 && host.split('.').every((part) => /^(?:0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255) && host !== '0.0.0.0';
  return host.length <= 253 && host.split('.').every((part) => /^[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?$/i.test(part));
}

export function normalizeDownloadOrigin(value: string): string {
  const candidate = value.trim();
  if (!/^https?:\/\/[^/?#]+\/?$/i.test(candidate) || /[\s\\\x00-\x1f\x7f]/.test(candidate)) return '';
  try {
    const url = new URL(candidate);
    if (!url.hostname || url.username || url.password || url.pathname !== '/' || url.search || url.hash || url.port === '0') return '';
    return url.origin;
  } catch { return ''; }
}

/** A managed certificate is usable only as one complete, validated bundle. */
export function managedAgentTLS(tls?: AgentTLS): ManagedAgentTLS | null {
  if (tls?.selfSignedEnabled !== true) return null;
  const { serverName, caUrl, caSha256, caFingerprintSha256, notAfter } = tls;
  const fingerprint = typeof caFingerprintSha256 === 'string' ? caFingerprintSha256.replaceAll(':', '') : '';
  const endpoint = typeof serverName === 'string' && serverName.includes(':') ? `[${serverName}]:443` : `${serverName}:443`;
  if (typeof serverName !== 'string' || !serverName || !validAgentServer(endpoint) || caUrl !== '/api/v1/agent-ca.pem'
    || typeof caSha256 !== 'string' || !/^[a-f0-9]{64}$/i.test(caSha256)
    || !fingerprint || !/^[a-f0-9]{64}$/i.test(fingerprint)
    || typeof notAfter !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(notAfter) || !Number.isFinite(Date.parse(notAfter)) || Date.parse(notAfter) <= Date.now()) {
    throw new Error(t('nodes.tls.invalidMetadata'));
  }
  return { selfSignedEnabled: true, serverName, caUrl, caSha256: caSha256.toLowerCase(), caFingerprintSha256: fingerprint.toLowerCase(), notAfter };
}

export const shellQuote = (value: string): string => `'${value.replaceAll("'", "'\"'\"'")}'`;
// EnvironmentFile uses its own quoting rules; it is not sourced by a shell.
const envQuote = (value: string): string => `"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;

export function buildNodeInstallCommand(options: NodeInstallOptions): string {
  const { name, token, server, origin, arch, caFile, tlsServerName } = options;
  const redeploy = options.redeploy;
  if (redeploy && (typeof redeploy.nodeId !== 'string' || !/^[a-z\d][a-z\d_-]{0,127}$/i.test(redeploy.nodeId))) throw new Error(t('nodes.enrollment.invalidValue'));
  const managedTLS = managedAgentTLS(options.tls);
  if (!validAgentServer(server)) throw new Error(t('nodes.enrollment.invalidServer'));
  const downloadOrigin = normalizeDownloadOrigin(origin);
  if (!downloadOrigin) throw new Error(t('nodes.enrollment.invalidDownloadOrigin'));
  if (!name.trim() || !token || [name, token, ...(!managedTLS ? [caFile, tlsServerName] : [])].some((value) => /[\x00-\x1f\x7f]/.test(value))) throw new Error(t('nodes.enrollment.invalidValue'));
  if (!managedTLS && caFile && !caFile.startsWith('/')) throw new Error(t('nodes.enrollment.invalidCA'));
  const insecure = !managedTLS && options.insecureLoopback && isLoopbackServer(server);
  if (redeploy && !managedTLS && !insecure && manualCAInsideAgentData(caFile)) throw new Error(t('nodes.enrollment.caInsideData'));
  const artifacts = options.artifacts.filter((item) => arch === 'auto' || item.arch === arch);
  if (!artifacts.length) throw new Error(t('nodes.enrollment.artifactsMissing'));
  const cases = artifacts.map((item) => {
    if (!['amd64', 'arm64'].includes(item.arch) || !/^[a-f0-9]{64}$/i.test(item.sha256) || item.url !== `/api/v1/agent-downloads/linux-${item.arch}`) throw new Error(t('nodes.enrollment.invalidArtifact'));
    const url = new URL(item.url, downloadOrigin);
    if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password) throw new Error(t('nodes.enrollment.invalidArtifact'));
    return `  ${item.arch === 'amd64' ? 'x86_64|amd64' : 'aarch64|arm64'}) url=${shellQuote(url.href)}; checksum=${shellQuote(item.sha256)} ;;`;
  });
  const effectiveCAFile = managedTLS ? managedAgentCAFile : caFile;
  const effectiveServerName = managedTLS ? managedTLS.serverName : tlsServerName;
  const environment = {
    NEWNEXUS_AGENT_SERVER: server,
    NEWNEXUS_AGENT_ENROLLMENT_TOKEN: token,
    NEWNEXUS_AGENT_NAME: name,
    ...(insecure ? { NEWNEXUS_AGENT_INSECURE_LOOPBACK: 'true' } : {}),
    ...(!insecure && effectiveCAFile ? { NEWNEXUS_AGENT_CA_FILE: effectiveCAFile } : {}),
    ...(!insecure && effectiveServerName ? { NEWNEXUS_AGENT_TLS_SERVER_NAME: effectiveServerName } : {}),
  };
  return `(
set -eu
[ "$(id -u)" -eq 0 ] || { echo 'Run this installation as root (sudo -i).' >&2; exit 1; }
[ "$(uname -s)" = Linux ] || { echo 'Linux is required.' >&2; exit 1; }
for tool in curl sha256sum systemctl install mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing required tool: $tool" >&2; exit 1; }
done
[ -d /run/systemd/system ] || { echo 'A running systemd host is required.' >&2; exit 1; }
${!redeploy ? `if [ -e /usr/local/bin/newnexus-agent ] || [ -L /usr/local/bin/newnexus-agent ] || [ -e /etc/newnexus/agent.env ] || [ -L /etc/newnexus/agent.env ] || [ -e /etc/newnexus/agent-deployment ] || [ -L /etc/newnexus/agent-deployment ] || [ -e /etc/systemd/system/newnexus-agent.service ] || [ -e /var/lib/newnexus-agent ] || [ -L /var/lib/newnexus-agent ] || [ -e /var/lib/private/newnexus-agent ]${managedTLS ? ` || [ -e ${managedAgentCAFile} ] || [ -L ${managedAgentCAFile} ]` : ''} || systemctl cat newnexus-agent.service >/dev/null 2>&1; then
  echo 'An Agent installation or data directory already exists. Preserve its credentials; inspect it before reinstalling.' >&2
  exit 1
fi
` : ''}case "$(uname -m)" in
${cases.join('\n')}
  *) echo 'No matching Agent binary for this CPU architecture.' >&2; exit 1 ;;
esac
umask 077
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' 0
${redeploy ? redeployPreflight(redeploy.nodeId, token, !managedTLS && !insecure ? caFile : '') : ''}curl --fail --silent --show-error --location --connect-timeout 15 --max-time 300 --proto '=http,https' --proto-redir '=https' "$url" -o "$tmp/newnexus-agent"
printf '%s  %s\\n' "$checksum" "$tmp/newnexus-agent" | sha256sum -c -
${managedTLS ? `curl --fail --silent --show-error --location --connect-timeout 15 --max-time 60 --proto '=http,https' --proto-redir '=https' ${shellQuote(new URL(managedTLS.caUrl, downloadOrigin).href)} -o "$tmp/agent-ca.pem"
printf '%s  %s\\n' ${shellQuote(managedTLS.caSha256)} "$tmp/agent-ca.pem" | sha256sum -c -
` : ''}${redeploy ? redeployBackup : ''}install -m 0755 "$tmp/newnexus-agent" /usr/local/bin/newnexus-agent
if [ ! -d /etc/newnexus ]; then install -d -m 0755 /etc/newnexus; fi
${managedTLS ? `install -m 0644 "$tmp/agent-ca.pem" ${managedAgentCAFile}
` : ''}cat > "$tmp/agent.env" <<'NEWNEXUS_AGENT_ENV'
${Object.entries(environment).map(([key, value]) => `${key}=${envQuote(value)}`).join('\n')}
NEWNEXUS_AGENT_ENV
install -m 0600 "$tmp/agent.env" /etc/newnexus/agent.env
cat > "$tmp/newnexus-agent.service" <<'NEWNEXUS_AGENT_SERVICE'
${agentServiceUnit}
NEWNEXUS_AGENT_SERVICE
install -m 0644 "$tmp/newnexus-agent.service" /etc/systemd/system/newnexus-agent.service
${redeploy ? `if [ -f /etc/newnexus/agent-deployment ]; then cp -p -- /etc/newnexus/agent-deployment "$tmp/agent-deployment"; else : > "$tmp/agent-deployment"; fi
printf '%s\\n' "$deployment_fingerprint" >> "$tmp/agent-deployment"
install -m 0600 "$tmp/agent-deployment" /etc/newnexus/agent-deployment
` : ''}systemctl daemon-reload
systemctl enable --now newnexus-agent
systemctl --no-pager --full status newnexus-agent
${redeploy ? `echo "Redeployment installed; verify that the node is online in the console. Recovery backup: $backup"
` : ''})
`;
}
