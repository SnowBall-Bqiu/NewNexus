import { describe, expect, it } from 'vitest';
import { t } from './i18n';

describe('Chinese navigation messages', () => {
  it('resolves stable i18n keys', () => {
    expect(t('overview')).toBe('总览');
    expect(t('routes')).toBe('CNAME 线路');
  });
});
