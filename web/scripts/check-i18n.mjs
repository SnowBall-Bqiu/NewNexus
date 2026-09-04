import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

function filesUnder(directory) {
  return readdirSync(directory).flatMap((name) => {
    const path = join(directory, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}

const root = new URL('../src', import.meta.url).pathname.replace(/^\/(.:)/, '$1');
const violations = [];
const forbiddenPrototypeCopy = ['星桥网络', '生产工作区', 'SQLite · 本地模式', 'nav-item__count">2'];

for (const path of filesUnder(root).filter((item) => /\.(tsx?|jsx?)$/.test(item))) {
  const source = readFileSync(path, 'utf8');
  for (const copy of forbiddenPrototypeCopy) {
    if (source.includes(copy)) violations.push(`${path}: 仍包含原型硬编码文案“${copy}”`);
  }
}

if (violations.length) {
  console.error(violations.join('\n'));
  process.exit(1);
}
console.log('UI copy policy passed.');
