import ts from 'typescript';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';

const root = fileURLToPath(new URL('../src/', import.meta.url));
const dictionary = JSON.parse(readFileSync(join(root, 'locales/zh-CN.json'), 'utf8'));
const files = (directory) => readdirSync(directory, { withFileTypes: true }).flatMap((entry) => entry.isDirectory() ? files(join(directory, entry.name)) : [join(directory, entry.name)]);
const cjk = /[\u3400-\u9fff]/;
const errors = [];
const isLiteral = (node) => node && (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node));
const isTranslation = (node) => ts.isCallExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === 't';

for (const [key, message] of Object.entries(dictionary)) {
  if (!/^[a-z][A-Za-z0-9]*(?:\.[a-zA-Z][A-Za-z0-9]*)*$/.test(key)) errors.push(`Invalid translation key: ${key}`);
  if (typeof message !== 'string' || !message.trim()) errors.push(`Empty translation: ${key}`);
}

// The pre-JavaScript HTML keeps readable metadata and must match the local dictionary.
const html = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
if (html.match(/<title>([^<]*)<\/title>/)?.[1] !== dictionary['app.title']) errors.push('index.html title differs from app.title');
if (html.match(/<meta\s+name="description"\s+content="([^"]*)"/)?.[1] !== dictionary['app.metaDescription']) errors.push('index.html description differs from app.metaDescription');

for (const file of files(root).filter((file) => /\.tsx?$/.test(file) && !/\.test\./.test(file) && !file.endsWith('i18n.ts'))) {
  const source = ts.createSourceFile(file, readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
  const report = (node, message) => { const position = source.getLineAndCharacterOfPosition(node.getStart(source)); errors.push(`${relative(root, file)}:${position.line + 1}:${position.character + 1} ${message}`); };
  const visit = (node) => {
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node) || ts.isImportTypeNode(node)) return;
    if (isTranslation(node) && isLiteral(node.arguments[0])) {
      const key = node.arguments[0].text;
      if (!Object.hasOwn(dictionary, key)) report(node, `Missing zh-CN key: ${key}`);
      if (isLiteral(node.arguments[1]) && dictionary[key] !== undefined && node.arguments[1].text !== dictionary[key]) report(node.arguments[1], `Fallback differs from zh-CN: ${key}`);
      const values = ts.isObjectLiteralExpression(node.arguments[1] ?? {}) ? node.arguments[1] : node.arguments[2];
      const placeholders = [...(dictionary[key] ?? '').matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map((match) => match[1]);
      if (placeholders.length && values && ts.isObjectLiteralExpression(values) && !values.properties.some(ts.isSpreadAssignment)) {
        const names = new Set(values.properties.filter((property) => !ts.isSpreadAssignment(property)).map((property) => property.name?.getText(source).replace(/^['"]|['"]$/g, '')));
        for (const placeholder of placeholders) if (!names.has(placeholder)) report(values, `Missing {${placeholder}} value: ${key}`);
      } else if (placeholders.length && !values) report(node, `Translation requires named values: ${key}`);
    }
    if (isLiteral(node) && cjk.test(node.text)) {
      const fallback = isTranslation(node.parent) && node.parent.arguments[1] === node && isLiteral(node.parent.arguments[0]) && dictionary[node.parent.arguments[0].text] === node.text;
      const typeOrProperty = ts.isLiteralTypeNode(node.parent) || ((ts.isPropertyAssignment(node.parent) || ts.isPropertySignature(node.parent)) && node.parent.name === node);
      if (!fallback && !typeOrProperty) report(node, 'Product text must use a registered translation key.');
    }
    if (ts.isJsxText(node) && cjk.test(node.text)) report(node, 'JSX text must use a registered translation key.');
    if (ts.isTemplateExpression(node) && [node.head.text, ...node.templateSpans.map((span) => span.literal.text)].some((part) => cjk.test(part))) report(node, 'Template text must use named translation values.');
    ts.forEachChild(node, visit);
  };
  visit(source);
}
if (errors.length) { console.error(errors.join('\n')); process.exitCode = 1; }
else console.log(`i18n checks passed (${Object.keys(dictionary).length} zh-CN messages).`);
