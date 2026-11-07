import { describe, expect, it } from 'vitest';
import { buildNodeInstallCommand, managedAgentTLS, manualCAInsideAgentData, normalizeDownloadOrigin, validAgentServer, type AgentTLS, type NodeInstallOptions } from './node-install';

const options: NodeInstallOptions = {
  name: 'hk-edge-01', token: 'one-time-test-token', server: 'agents.example.com:443',
  origin: 'https://console.example.com', arch: 'auto', insecureLoopback: false, caFile: '', tlsServerName: '',
  artifacts: [
    { arch: 'amd64', url: '/api/v1/agent-downloads/linux-amd64', sha256: 'a'.repeat(64) },
    { arch: 'arm64', url: '/api/v1/agent-downloads/linux-arm64', sha256: 'b'.repeat(64) },
  ],
};
const managedTLS: AgentTLS = {
  selfSignedEnabled: true, serverName: 'newnexus-agent.internal', caUrl: '/api/v1/agent-ca.pem',
  caSha256: 'c'.repeat(64), caFingerprintSha256: 'd'.repeat(64), notAfter: '2046-01-01T00:00:00Z',
};

describe('node installation commands', () => {
  it('keeps fresh installs non-destructive and enables replacements only with a bound node ID', () => {
    const fresh = buildNodeInstallCommand(options);
    expect(fresh).toContain('An Agent installation or data directory already exists');
    expect(fresh).not.toContain('systemctl stop');
    expect(fresh).not.toContain('/var/backups/newnexus-agent');
    const command = buildNodeInstallCommand({ ...options, redeploy: { nodeId: 'd5802c51-58b3-4c19-9cae-f515d3dad14b' }, tls: managedTLS });
    expect(command).not.toContain('An Agent installation or data directory already exists');
    expect(command).toContain('NEWNEXUS_AGENT_ENROLLMENT_TOKEN="one-time-test-token"');
    expect(command).not.toContain('NEWNEXUS_AGENT_NODE_ID=');
    const verifiedCA = command.indexOf(`'${managedTLS.caSha256}' "$tmp/agent-ca.pem" | sha256sum -c -`);
    const stop = command.indexOf('\n  systemctl stop newnexus-agent ||');
    expect(verifiedCA).toBeGreaterThan(0);
    expect(stop).toBeGreaterThan(verifiedCA);
    expect(command.indexOf('mv -- "$old_data_path" "$backup/data"')).toBeGreaterThan(stop);
    expect(command.indexOf('install -m 0755 "$tmp/newnexus-agent"')).toBeGreaterThan(stop);
    expect(command).toContain('mktemp -d /var/backups/newnexus-agent/redeploy.XXXXXXXX');
    expect(command).toContain('install -d -m 0700 /var/backups/newnexus-agent');
    expect(command).toContain('cp -p -- "$target" "$backup/$source_name"');
    expect(command).toContain('mv -- "$data_path" "$backup/failed-new-data"');
    expect(command).toContain('Old node credentials were revoked');
    expect(command).not.toMatch(/rm\s+-rf[^\n]*(?:\/var\/lib|\$data_path|\$old_data_path|\$backup)/);
  });

  it('guards systemd layout, DynamicUser state, root ownership and duplicate replacement tokens', () => {
    const command = buildNodeInstallCommand({ ...options, redeploy: { nodeId: 'bound-node' } });
    expect(command).toContain('systemctl show newnexus-agent.service --property=DropInPaths --value');
    expect(command).toContain('cmp -s /etc/systemd/system/newnexus-agent.service "$tmp/expected.service"');
    expect(command).toContain('grep -Fq NEWNEXUS_AGENT_DATA_DIR');
    expect(command).toContain('readlink -f /var/lib/newnexus-agent');
    expect(command).toContain('[ ! -L /var/lib/private/newnexus-agent ]');
    expect(command).toContain('Directory must be root-owned');
    expect(command).toContain('flock -n 9');
    expect(command).toContain("deployment_fingerprint=$(printf '%s' 'one-time-test-token' | sha256sum)");
    expect(command).toContain("deployment_fingerprint='bound-node':${deployment_fingerprint%% *}");
    expect(command).toContain('grep -Fxq "$deployment_fingerprint" /etc/newnexus/agent-deployment');
    expect(command).toContain('install -m 0600 "$tmp/agent-deployment" /etc/newnexus/agent-deployment');
    expect(command).toContain("printf '%s\\n' \"$deployment_fingerprint\"");
  });

  it('preserves an existing manual CA at the managed filename during replacement', () => {
    const command = buildNodeInstallCommand({ ...options, redeploy: { nodeId: 'bound-node' }, caFile: '/etc/newnexus/agent-ca.pem', tlsServerName: 'proxy.example.com' });
    expect(command).toContain('NEWNEXUS_AGENT_CA_FILE="/etc/newnexus/agent-ca.pem"');
    expect(command).toContain('ca:/etc/newnexus/agent-ca.pem');
    expect(command).not.toContain('mv -- /etc/newnexus/agent-ca.pem');
    expect(command).not.toContain('install -m 0644 "$tmp/agent-ca.pem"');
  });

  it.each([
    '/var/lib/newnexus-agent', '/var/lib/newnexus-agent/private-ca.pem', '/var/lib/private/newnexus-agent/private-ca.pem',
    '//var//lib/./newnexus-agent/private-ca.pem', '/var/lib/other/../newnexus-agent/private-ca.pem',
    '/var/lib/private/newnexus-agent/nested/../private-ca.pem',
  ])('refuses a manual CA inside state before producing a replacement command: %s', (caFile) => {
    expect(manualCAInsideAgentData(caFile)).toBe(true);
    expect(() => buildNodeInstallCommand({ ...options, caFile, redeploy: { nodeId: 'bound-node' } })).toThrow();
    expect(() => buildNodeInstallCommand({ ...options, caFile })).not.toThrow();
    expect(() => buildNodeInstallCommand({ ...options, caFile, tls: managedTLS, redeploy: { nodeId: 'bound-node' } })).not.toThrow();
  });

  it.each(['/etc/newnexus/private-ca.pem', '/var/lib/newnexus-agent-other/private-ca.pem', '/var/lib/private/newnexus-agent-other/ca.pem', '/var/lib/newnexus-agent/../../etc/ca.pem', 'relative-ca.pem'])('does not mistake another directory for Agent state: %s', (caFile) => {
    expect(manualCAInsideAgentData(caFile)).toBe(false);
  });

  it('resolves an external manual CA on the target host and checks it again before stopping the service', () => {
    const command = buildNodeInstallCommand({ ...options, caFile: '/etc/newnexus/external-ca-link.pem', redeploy: { nodeId: 'bound-node' } });
    expect(command).toContain("manual_ca_real=$(readlink -f -- '/etc/newnexus/external-ca-link.pem')");
    expect(command).toContain('/var/lib/newnexus-agent|/var/lib/newnexus-agent/*|/var/lib/private/newnexus-agent|/var/lib/private/newnexus-agent/*)');
    expect(command).toContain('Move it to /etc/newnexus before redeployment.');
    const initialCheck = command.indexOf('\nvalidate_manual_ca\n');
    const finalCheck = command.lastIndexOf('\nvalidate_manual_ca\n');
    expect(initialCheck).toBeLessThan(command.indexOf('\ncurl '));
    expect(finalCheck).toBeGreaterThan(initialCheck);
    expect(finalCheck).toBeLessThan(command.indexOf('\n  systemctl stop newnexus-agent ||'));
  });

  it.each(['', '../node', 'node/one', "node'$(touch /tmp/injected)", 'node\nother', 'x'.repeat(129)])('rejects malformed replacement ID %j', (nodeId) => {
    expect(() => buildNodeInstallCommand({ ...options, redeploy: { nodeId } })).toThrow();
  });

  it('uses real enrollment values and verified downloads for both CPU families', () => {
    const command = buildNodeInstallCommand(options);
    expect(command).toContain('https://console.example.com/api/v1/agent-downloads/linux-amd64');
    expect(command).toContain('https://console.example.com/api/v1/agent-downloads/linux-arm64');
    expect(command).toContain('NEWNEXUS_AGENT_ENROLLMENT_TOKEN="one-time-test-token"');
    expect(command).toContain('NEWNEXUS_AGENT_SERVER="agents.example.com:443"');
    expect(command.indexOf('sha256sum -c -')).toBeLessThan(command.indexOf('install -m 0755'));
    expect(command).toContain('install -m 0600');
    expect(command).toContain('DynamicUser=yes');
    expect(command).toContain('systemctl cat newnexus-agent.service');
    expect(command).not.toContain('--enrollment-token');
    expect(command).not.toContain('INSECURE_LOOPBACK');
  });

  it('keeps public API downloads independent from the local console and Agent endpoint', () => {
    const command = buildNodeInstallCommand({
      ...options,
      origin: 'https://api.example.com:8443/',
      server: 'agent.example.com:443',
    });
    expect(command).toContain("url='https://api.example.com:8443/api/v1/agent-downloads/linux-amd64'");
    expect(command).toContain("url='https://api.example.com:8443/api/v1/agent-downloads/linux-arm64'");
    expect(command).toContain('NEWNEXUS_AGENT_SERVER="agent.example.com:443"');
    expect(command).not.toContain(window.location.origin);
    expect(command).not.toContain('https://console.example.com');
    expect(command).not.toContain("url='https://agent.example.com");
  });

  it('preserves shell-looking node names as EnvironmentFile data', () => {
    const command = buildNodeInstallCommand({ ...options, name: `edge '$HOME' $(touch /tmp/injected) "quoted" \\ end` });
    expect(command).toContain("<<'NEWNEXUS_AGENT_ENV'");
    expect(command).toContain('NEWNEXUS_AGENT_NAME="edge \'$HOME\' $(touch /tmp/injected) \\"quoted\\" \\\\ end"');
    expect(() => buildNodeInstallCommand({ ...options, name: 'edge\nNEWNEXUS_AGENT_ENV\ntouch /tmp/injected' })).toThrow();
    expect(() => buildNodeInstallCommand({ ...options, caFile: '/etc/ca\rBAD=true' })).toThrow();
  });

  it('allows explicit development plaintext only for literal loopback targets', () => {
    expect(buildNodeInstallCommand({ ...options, server: '127.0.0.1:9091', insecureLoopback: true })).toContain('NEWNEXUS_AGENT_INSECURE_LOOPBACK="true"');
    expect(buildNodeInstallCommand({ ...options, server: 'localhost:9091', insecureLoopback: true })).not.toContain('INSECURE_LOOPBACK');
    expect(buildNodeInstallCommand({ ...options, server: 'agents.example.com:9091', insecureLoopback: true })).not.toContain('INSECURE_LOOPBACK');
  });

  it('keeps private CA/SNI configuration and restricts selected architecture', () => {
    const command = buildNodeInstallCommand({ ...options, arch: 'arm64', caFile: '/etc/newnexus/ca.pem', tlsServerName: 'agents.example.com' });
    expect(command).toContain('NEWNEXUS_AGENT_CA_FILE="/etc/newnexus/ca.pem"');
    expect(command).toContain('NEWNEXUS_AGENT_TLS_SERVER_NAME="agents.example.com"');
    expect(command).not.toContain('/linux-amd64');
    expect(() => buildNodeInstallCommand({ ...options, artifacts: [] })).toThrow();
    expect(() => buildNodeInstallCommand({ ...options, artifacts: [{ ...options.artifacts[0], sha256: 'invalid' }] })).toThrow();
    expect(() => buildNodeInstallCommand({ ...options, artifacts: [{ ...options.artifacts[0], url: 'https://other.example/file' }] })).toThrow();
  });

  it('verifies binary and managed CA before installing anything, with stable SNI over an IP or TCP forward', () => {
    const command = buildNodeInstallCommand({ ...options, server: '198.51.100.9:19443', origin: 'https://api.example.com', caFile: '/manual/ca.pem', tlsServerName: 'manual.example.com', tls: managedTLS });
    expect(command).toContain("'https://api.example.com/api/v1/agent-ca.pem' -o \"$tmp/agent-ca.pem\"");
    const binaryCheck = command.indexOf('"$tmp/newnexus-agent" | sha256sum -c -');
    const caCheck = command.indexOf(`'${managedTLS.caSha256}' "$tmp/agent-ca.pem" | sha256sum -c -`);
    const firstInstall = command.indexOf('\ninstall -m ');
    expect(binaryCheck).toBeGreaterThan(0);
    expect(caCheck).toBeGreaterThan(binaryCheck);
    expect(caCheck).toBeLessThan(firstInstall);
    expect(command).toContain('install -m 0644 "$tmp/agent-ca.pem" /etc/newnexus/agent-ca.pem');
    expect(command).toContain('NEWNEXUS_AGENT_SERVER="198.51.100.9:19443"');
    expect(command).toContain('NEWNEXUS_AGENT_CA_FILE="/etc/newnexus/agent-ca.pem"');
    expect(command).toContain('NEWNEXUS_AGENT_TLS_SERVER_NAME="newnexus-agent.internal"');
    expect(command).toContain('NEWNEXUS_AGENT_ENROLLMENT_TOKEN="one-time-test-token"');
    expect(command).toContain('[ -L /etc/newnexus/agent-ca.pem ]');
    expect(command).toContain('install -m 0600 "$tmp/agent.env"');
    expect(command).not.toContain('/manual/ca.pem');
    expect(command).not.toContain('manual.example.com');
    expect(command).not.toContain(managedTLS.caFingerprintSha256);
    expect(command).not.toMatch(/--insecure|curl[^\n]*\s-k(?:\s|$)/);
    expect(command).not.toContain('INSECURE_LOOPBACK');
  });

  it('never advertises plaintext when managed TLS is enabled, even with inconsistent loopback metadata', () => {
    const command = buildNodeInstallCommand({ ...options, server: '127.0.0.1:9443', insecureLoopback: true, tls: managedTLS });
    expect(command).not.toContain('INSECURE_LOOPBACK');
    expect(command).toContain('NEWNEXUS_AGENT_CA_FILE="/etc/newnexus/agent-ca.pem"');
  });

  it('ignores inactive certificate fields and preserves explicit manual private-CA configuration', () => {
    const inactive = { ...managedTLS, selfSignedEnabled: false, caUrl: 'https://attacker.example/ca' };
    expect(managedAgentTLS(inactive)).toBeNull();
    const command = buildNodeInstallCommand({ ...options, caFile: '/etc/private-proxy/ca.pem', tlsServerName: 'proxy.example.com', tls: inactive });
    expect(command).not.toContain('/api/v1/agent-ca.pem');
    expect(command).not.toContain('$tmp/agent-ca.pem');
    expect(command).not.toContain('attacker.example');
    expect(command).toContain('NEWNEXUS_AGENT_CA_FILE="/etc/private-proxy/ca.pem"');
    expect(command).toContain('NEWNEXUS_AGENT_TLS_SERVER_NAME="proxy.example.com"');
    expect(command).not.toMatch(/--insecure|curl[^\n]*\s-k(?:\s|$)/);
  });

  it.each(['https://attacker.example/ca', '//attacker.example/ca', '/api/v1/agent-ca.pem?download=1', '/api/v1/agent-ca.pem#ca', '/api/v1/../secrets'])('refuses a managed CA outside its fixed public route: %s', (caUrl) => {
    expect(() => buildNodeInstallCommand({ ...options, tls: { ...managedTLS, caUrl } })).toThrow();
  });

  it('rejects incomplete, expired or malformed certificate bundles before producing a script', () => {
    for (const invalid of [
      { caSha256: '' }, { caSha256: 'not-a-hash' }, { caFingerprintSha256: 'not-a-fingerprint' },
      { serverName: 'https://agent.example.com' }, { serverName: 'agent.example.com:443' },
      { serverName: 'agent.example.com\nINSECURE=true' }, { serverName: '*.example.com' },
      { notAfter: '2000-01-01T00:00:00Z' }, { notAfter: '2046' }, { notAfter: '' },
    ]) expect(() => buildNodeInstallCommand({ ...options, tls: { ...managedTLS, ...invalid } })).toThrow();
    expect(managedAgentTLS({ ...managedTLS, caFingerprintSha256: Array(32).fill('DD').join(':') })?.caFingerprintSha256).toBe('d'.repeat(64));
  });
});

describe('installation download origins', () => {
  it.each([
    ['https://api.example.com', 'https://api.example.com'],
    ['https://API.EXAMPLE.COM:443/', 'https://api.example.com'],
    ['http://127.0.0.1:8080/', 'http://127.0.0.1:8080'],
    ['https://api.example.com:8443/', 'https://api.example.com:8443'],
    [' https://api.example.com/ ', 'https://api.example.com'],
    ['https://[2001:db8::1]:8443/', 'https://[2001:db8::1]:8443'],
  ])('normalizes the HTTP(S) origin %s', (value, expected) => {
    expect(normalizeDownloadOrigin(value)).toBe(expected);
  });

  it.each([
    '', 'api.example.com', '//api.example.com', 'https:/api.example.com',
    'ftp://api.example.com', 'file:///tmp/agent', 'javascript:alert(1)', 'wss://api.example.com',
    'https://user:password@api.example.com', 'https://user@api.example.com',
    'https://api.example.com/api', 'https://api.example.com//',
    'https://api.example.com?token=secret', 'https://api.example.com#fragment',
    'https://api.example.com/?', 'https://api.example.com/#',
    'https://api.example.com:0', 'https://api.example.com:65536',
    'https://api.example.com:-1', 'https://api.example.com:port',
    'https://api.example.com/\\other', 'https://api. example.com', 'https://api.example.com\n/path',
  ])('rejects %j before generating an installation command', (origin) => {
    expect(normalizeDownloadOrigin(origin)).toBe('');
    expect(() => buildNodeInstallCommand({ ...options, origin })).toThrow();
  });
});

describe('Agent endpoint validation', () => {
  it.each(['agents.example.com:443', '127.0.0.1:9091', '[::1]:9091', '[2001:db8::1]:443'])('accepts %s', (server) => expect(validAgentServer(server)).toBe(true));
  it.each(['https://agents.example.com:443', '0.0.0.0:9090', '[::]:9090', 'agents.example.com:0', 'agents.example.com:65536', '127.0.0.01:9091', '999.1.1.1:443', 'host:443/path', 'host\n:443', '[invalid]:443'])('rejects %s', (server) => expect(validAgentServer(server)).toBe(false));
});
