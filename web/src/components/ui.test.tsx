import { describe, expect, it } from 'vitest';
import { statusVariant } from '../lib/status';

describe('status presentation', () => {
  it('maps operational states to semantic Kumo variants', () => {
    expect(statusVariant('healthy')).toBe('success');
    expect(statusVariant('pending')).toBe('warning');
    expect(statusVariant('failed')).toBe('error');
    expect(statusVariant('unknown')).toBe('neutral');
  });
});
