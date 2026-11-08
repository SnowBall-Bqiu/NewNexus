import { describe, expect, it } from 'vitest';
import { spreadsheetCsv } from './csv';

describe('spreadsheet CSV export', () => {
  it.each(['=1+1', '+SUM(A1:A2)', '-1+2', '@SUM(A1:A2)', '＝1+1', '＋1+1', '－1+2', '＠SUM(A1:A2)'])('exports untrusted formula text as an Excel text cell: %s', (value) => {
    expect(spreadsheetCsv([[value]])).toBe(`\ufeff"\t${value}"`);
  });

  it.each([' =1+1', '\t=1+1', '\r\n=1+1', '\u0000\u001f+1+1', '\u00a0\u200b\ufeff@SUM(A1:A2)'])('detects formulas after leading whitespace or controls: %j', (value) => {
    expect(spreadsheetCsv([[value]])).toBe(`\ufeff"\t${value}"`);
  });

  it('preserves typed numeric measurements, including negative values, while protecting text', () => {
    expect(spreadsheetCsv([[0, 12.5, -12.5, -1e30, '-12.5']])).toBe('\ufeff"0","12.5","-12.5","-1e+30","\t-12.5"');
  });

  it('keeps quotes, separators and line breaks inside their original CSV cells', () => {
    expect(spreadsheetCsv([
      ['name', 'version', 'metadata'],
      ['edge,"hk"\nsecond line', '=HYPERLINK("https://example.invalid/","open")', { note: 'a,b' }],
    ])).toBe('\ufeff"name","version","metadata"\r\n"edge,""hk""\nsecond line","\t=HYPERLINK(""https://example.invalid/"",""open"")","{""note"":""a,b""}"');
  });
});
