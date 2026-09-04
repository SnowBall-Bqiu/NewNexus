import { afterEach, describe, expect, it } from 'vitest';
import { applyTheme, saveTheme, storedTheme } from './theme';

describe('theme bridge', () => {
  afterEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    document.documentElement.removeAttribute('data-mode');
    document.documentElement.style.colorScheme = '';
  });

  it('stores and applies monochrome mode through the Kumo light token set', () => {
    saveTheme('mono');
    expect(storedTheme()).toBe('mono');
    expect(document.documentElement.dataset.theme).toBe('mono');
    expect(document.documentElement.dataset.mode).toBe('light');
    expect(document.documentElement.style.colorScheme).toBe('light');
  });

  it('applies dark mode to both theme markers', () => {
    applyTheme('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(document.documentElement.dataset.mode).toBe('dark');
    expect(document.documentElement.style.colorScheme).toBe('dark');
  });
});
