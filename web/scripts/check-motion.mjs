import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

function filesUnder(directory) {
  return readdirSync(directory).flatMap((name) => {
    const path = join(directory, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}

const files = filesUnder(new URL('../src', import.meta.url).pathname.replace(/^\/(.:)/, '$1'))
  .filter((path) => /\.(css|tsx?|jsx?)$/.test(path));
const violations = [];

for (const path of files) {
  const source = readFileSync(path, 'utf8');
  const checks = [
    [/transition\s*:\s*all\b/i, '禁止使用 transition: all'],
    [/\bease-in(?=\s|;|,)/i, '禁止使用独立 ease-in'],
    [/scale\(\s*0\s*\)/i, '禁止使用 scale(0)'],
  ];
  for (const [pattern, message] of checks) {
    if (pattern.test(source)) violations.push(`${path}: ${message}`);
  }
  if (path.endsWith('.css') && /\b(?:transition|animation)\s*:/.test(source) && !source.includes('prefers-reduced-motion')) {
    violations.push(`${path}: 动效样式缺少 prefers-reduced-motion 分支`);
  }
}

if (violations.length) {
  console.error(violations.join('\n'));
  process.exit(1);
}
console.log(`Motion policy passed (${files.length} source files checked).`);
