import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const root = new URL('..', import.meta.url).pathname.replace(/^\/(.:)/, '$1');
const packageJson = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
const forbiddenPackages = ['@base-ui-components/react', 'lucide-react', 'sonner', 'recharts', 'class-variance-authority'];
const violations = [];

for (const name of forbiddenPackages) {
  if (packageJson.dependencies?.[name] || packageJson.devDependencies?.[name]) violations.push(`禁止依赖旧 UI 包：${name}`);
}

function filesUnder(directory) {
  return readdirSync(directory).flatMap((name) => {
    const path = join(directory, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}

for (const path of filesUnder(join(root, 'src')).filter((path) => /\.(css|tsx?|jsx?)$/.test(path))) {
  const source = readFileSync(path, 'utf8');
  if (/from\s+['"](?:lucide-react|sonner|recharts|@base-ui-components\/react)/.test(source)) violations.push(`${path}: 仍引用旧 UI 包`);
  if (/\{[^}\n]+&&\s*<[A-Z][A-Za-z0-9]*Dialog\b/.test(source)) violations.push(`${path}: Dialog 必须常驻并通过 open 控制`);
  if (/\bfont-bold\b/.test(source)) violations.push(`${path}: 使用 semibold/medium，禁止 font-bold`);
  if (/\bfont-weight\s*:\s*(?:700|800|900|bold)\b/.test(source)) violations.push(`${path}: 使用 semibold/medium，禁止粗体字重`);
  if (/\btracking-[\w-[\]]+/.test(source)) violations.push(`${path}: 禁止修改字体 tracking`);
  if (/letter-spacing\s*:/.test(source)) violations.push(`${path}: 禁止应用层修改字体 tracking`);
  if (/transition(?:-property)?\s*:[^;]*(?:color|background-color|all)/.test(source) || /\btransition-colors\b/.test(source)) violations.push(`${path}: 应用层颜色变化必须即时完成`);
  if (/text-transform\s*:\s*uppercase/.test(source)) violations.push(`${path}: 标题必须使用 sentence case`);
  if (path.endsWith('.css')) {
    for (const block of source.matchAll(/([^{}]+)\{([^{}]+)\}/g)) {
      const selector = block[1].trim();
      const declarations = block[2];
      const hasVisibleBorder = /(?:^|[;\s])border(?:-(?:top|right|bottom|left))?\s*:\s*(?:[1-9]|thin|medium|thick)/.test(declarations);
      const shadowMatch = declarations.match(/box-shadow\s*:\s*([^;]+)/);
      const hasDropShadow = shadowMatch ? shadowMatch[1].split(',').some((shadow) => !/\binset\b/.test(shadow)) : false;
      if (/position\s*:\s*sticky/.test(declarations) && !/border(?:-(?:top|right|bottom|left))?\s*:/.test(declarations) && !/\.route-list|\.project-list|\.newnexus-sidebar/.test(selector)) violations.push(`${path}: sticky 元素必须有边界分隔：${selector}`);
      if (hasVisibleBorder && hasDropShadow && !selector.includes('.route-title-band')) violations.push(`${path}: 不应同时使用实体 border 与 drop shadow：${selector}`);
    }
  }
}

if (!readFileSync(join(root, 'src', 'main.tsx'), 'utf8').includes("@cloudflare/kumo/styles/standalone")) {
  violations.push('入口必须加载 @cloudflare/kumo/styles/standalone');
}

if (violations.length) {
  console.error(violations.join('\n'));
  process.exit(1);
}

console.log('Kumo migration policy passed.');
