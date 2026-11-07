import { describe, expect, it } from 'vitest';
import { interpolate, t } from './i18n';

describe('local translations', () => {
  it('resolves registered Chinese messages and preserves the fallback API', () => {
    expect(t('common.save')).toBe('保存');
    expect(t('unregistered.key', 'Fallback')).toBe('Fallback');
    expect(t('unregistered.key')).toBe('unregistered.key');
  });

  it('interpolates named values including zero without evaluating their contents', () => {
    expect(t('resource.createNamed', { singular: '项目' })).toBe('新建项目');
    expect(interpolate('{count} / {total} · {name}', { count: 0, total: 8, name: '$& <node> {count}' })).toBe('0 / 8 · $& <node> {count}');
    expect(t('unregistered.key', 'Version {version}', { version: 3 })).toBe('Version 3');
  });

  it('keeps missing placeholders visible and does not replace inherited properties', () => {
    expect(interpolate('{missing} {undefinedValue} {empty} {flag}', { undefinedValue: undefined, empty: null, flag: false })).toBe('{missing} {undefinedValue}  false');
    expect(interpolate('{toString}')).toBe('{toString}');
    expect(interpolate('{"Accept":"text/plain"}')).toBe('{"Accept":"text/plain"}');
  });
});
