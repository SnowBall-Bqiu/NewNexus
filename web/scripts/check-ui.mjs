import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';
const root = fileURLToPath(new URL('../src', import.meta.url));
const files = (dir) => readdirSync(dir, { withFileTypes: true }).flatMap((entry) => entry.isDirectory() ? files(join(dir, entry.name)) : [join(dir, entry.name)]);
const errors = [];
for (const file of files(root).filter((file) => /\.(tsx?|css)$/.test(file) && !file.includes('.test.'))) {
  const source = readFileSync(file, 'utf8');
  for (const [pattern, label] of [[/@cloudflare\/kumo|@radix-ui\//, '仅允许 Base UI 组件体系'], [/transition\s*:\s*all\b/, '禁止 transition: all'], [/\bease-in(?=\s|;|,)/, '禁止 ease-in'], [/scale\(\s*0\s*\)/, '禁止 scale(0)']]) if (pattern.test(source)) errors.push(`${file}: ${label}`);
  if (file.endsWith('.css') && /(?:transition|animation)\s*:/.test(source) && !source.includes('prefers-reduced-motion')) errors.push(`${file}: 缺少减弱动效规则`);
}
if (errors.length) { console.error(errors.join('\n')); process.exit(1); }
console.log('Base UI, theme and motion checks passed.');
await import('./check-i18n.mjs');
