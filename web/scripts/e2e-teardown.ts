import { readFile } from 'node:fs/promises';
export default async function teardown() {
  if (process.env.NEWNEXUS_E2E_BASE_URL) return;
  const harness = JSON.parse(await readFile(new URL('../../.cache/e2e-harness.json', import.meta.url), 'utf8')) as { url: string; token: string };
  const url = new URL(harness.url);
  if (url.hostname !== '127.0.0.1' || url.pathname !== '/stop') throw new Error('Unexpected E2E harness URL');
  const response = await fetch(url, { method: 'POST', headers: { Authorization: `Bearer ${harness.token}` }, signal: AbortSignal.timeout(5000) });
  if (!response.ok) throw new Error('Could not stop the owned E2E control plane');
}
