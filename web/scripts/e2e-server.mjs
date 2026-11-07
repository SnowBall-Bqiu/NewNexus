import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
const root = fileURLToPath(new URL('../../', import.meta.url));
await mkdir(resolve(root, '.cache'), { recursive: true });
const data = await mkdtemp(resolve(root, '.cache/e2e-'));
const binary = resolve(root, `bin/newnexus-server${process.platform === 'win32' ? '.exe' : ''}`);
const token = randomBytes(32).toString('hex');
// The test harness owns this child. A private, authenticated stop endpoint lets
// Playwright close it before Windows terminates the parent shell/process group.
let child;
const harness = createServer((request, response) => {
  if (request.method !== 'POST' || request.url !== '/stop' || request.headers.authorization !== `Bearer ${token}`) { response.writeHead(403).end(); return; }
  response.writeHead(200).end('stopping');
  child?.kill('SIGTERM');
  harness.close();
});
await new Promise((resolve) => harness.listen(0, '127.0.0.1', resolve));
await writeFile(resolve(root, '.cache/e2e-harness.json'), JSON.stringify({ url: `http://127.0.0.1:${harness.address().port}/stop`, token }), { mode: 0o600 });
child = spawn(binary, [], { cwd: root, stdio: 'inherit', windowsHide: true, env: { ...process.env, NEWNEXUS_ENV: 'development', NEWNEXUS_DATA_DIR: data, NEWNEXUS_DATABASE_URL: process.env.NEWNEXUS_E2E_DATABASE_URL || resolve(data, 'newnexus.db'), NEWNEXUS_HTTP_ADDR: '127.0.0.1:18080', NEWNEXUS_GRPC_ADDR: '127.0.0.1:19090', NEWNEXUS_ALLOW_INSECURE_AGENT: '1', NEWNEXUS_COOKIE_SECURE: '0', NEWNEXUS_DEV_AUTH: '0', NEWNEXUS_TLS_CERT: '', NEWNEXUS_TLS_KEY: '' } });
child.on('error', (error) => { console.error(error); process.exit(1); });
child.on('exit', (code) => process.exit(code ?? 0));
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => child.kill(signal));
process.on('exit', () => { if (!child.killed) child.kill(); });
