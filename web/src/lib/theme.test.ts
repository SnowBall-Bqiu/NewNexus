import { beforeEach, expect, it, vi } from 'vitest';
import { applyTheme, saveTheme, storedTheme } from './theme';
beforeEach(() => { localStorage.clear(); document.documentElement.classList.remove('dark'); vi.restoreAllMocks(); });
it('defaults to system and responds to its color scheme', () => {
  vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true })); expect(storedTheme()).toBe('system'); applyTheme('system'); expect(document.documentElement.dataset.theme).toBe('dark');
  applyTheme('light'); expect(document.documentElement.classList.contains('dark')).toBe(false);
});
it('persists explicit preference and uses it after reload', () => { saveTheme('dark'); expect(localStorage.getItem('newnexus.theme')).toBe('dark'); expect(storedTheme()).toBe('dark'); expect(document.documentElement.style.colorScheme).toBe('dark'); });
it('keeps preference usable when browser storage is blocked', () => { vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); }); vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); }); saveTheme('light'); expect(storedTheme()).toBe('light'); });
