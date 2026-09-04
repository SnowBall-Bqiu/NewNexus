export type ThemeMode = 'light' | 'dark' | 'mono' | 'system';
const storageKey = 'newnexus.theme';
export function storedTheme(): ThemeMode { try { const value = localStorage.getItem(storageKey); return value === 'dark' || value === 'mono' || value === 'system' ? value : 'light'; } catch { return 'light'; } }
export function applyTheme(mode: ThemeMode) { const resolved = mode === 'system' ? (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light') : mode; const root = document.documentElement; root.dataset.theme = resolved; root.dataset.mode = resolved === 'mono' ? 'light' : resolved; root.style.colorScheme = resolved === 'dark' ? 'dark' : 'light'; window.dispatchEvent(new CustomEvent('newnexus-theme')); }
export function saveTheme(mode: ThemeMode) { try { localStorage.setItem(storageKey, mode); } catch { /* Browser storage can be disabled. */ } applyTheme(mode); }
export function initializeTheme() { applyTheme(storedTheme()); }
export function watchSystemTheme(callback: () => void) { const media = matchMedia('(prefers-color-scheme: dark)'); const listener = () => callback(); media.addEventListener('change', listener); return () => media.removeEventListener('change', listener); }
