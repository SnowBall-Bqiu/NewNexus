import { readFileSync } from 'node:fs';

const css = readFileSync(new URL('../src/tool-theme.css', import.meta.url), 'utf8');
const overview = readFileSync(new URL('../src/pages/OverviewPage.tsx', import.meta.url), 'utf8');
const violations = [];

const requiredCSS = [
  ['--tool-ground: var(--color-kumo-canvas);', '画布必须直接使用 Kumo 主题 token'],
  ['--tool-panel: var(--color-kumo-base);', '主要表面必须直接使用 Kumo 主题 token'],
  ['--tool-text: var(--text-color-kumo-default);', '正文必须直接使用 Kumo 文字 token'],
  ['--brand-green: #138a58;', '品牌绿色必须独立于主操作色'],
  ['background: var(--tool-recessed);', '表头与次级表面必须使用主题 token'],
  ['border-bottom: 1px solid var(--tool-hairline);', '资源行必须使用主题分隔 token'],
  ['background: var(--color-kumo-success-tint);', '健康状态必须使用 Kumo 语义 token'],
];

for (const [source, message] of requiredCSS) {
  if (!css.includes(source)) violations.push(message);
}

const forbiddenComponentColors = [
  [/<(?:CartesianGrid|Area|stop)\b[^>]*(?:stroke|stopColor)="#/s, '图表 SVG 颜色不得硬编码'],
  [/tick=\{\{[^}]*fill:\s*['"]#/s, '图表坐标文字颜色不得硬编码'],
  [/contentStyle=\{\{[^}]*#(?:[0-9a-f]{3}){1,2}/is, '图表 Tooltip 颜色不得硬编码'],
];

for (const [pattern, message] of forbiddenComponentColors) {
  if (pattern.test(overview)) violations.push(message);
}

if (violations.length) {
  console.error(violations.join('\n'));
  process.exit(1);
}

console.log('Theme policy passed.');
