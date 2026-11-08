const leadingIgnoredCharacters = /^[\s\p{Cc}\p{Cf}]*/u;
const formulaPrefix = /^[=+\-@＝＋－＠]/u;

function spreadsheetCell(value: unknown): string {
  const text = typeof value === 'object' ? JSON.stringify(value) ?? '' : value === undefined ? '' : String(value);
  const numeric = typeof value === 'number' && Number.isFinite(value);
  // This export is for spreadsheet viewing. OWASP's Excel-resistant mitigation
  // puts a tab inside the quoted field; keep typed numbers (including negatives)
  // numeric. The added tab is data, and other CSV consumers may treat it differently.
  // https://community.owasp.org/attacks/CSV_Injection
  const safe = !numeric && formulaPrefix.test(text.replace(leadingIgnoredCharacters, '')) ? `\t${text}` : text;
  return `"${safe.replaceAll('"', '""')}"`;
}

export function spreadsheetCsv(rows: readonly (readonly unknown[])[]): string {
  return '\ufeff' + rows.map((row) => row.map(spreadsheetCell).join(',')).join('\r\n');
}
