import { useSyncExternalStore } from 'react';
export type ThemeMode = 'light' | 'dark' | 'system';
const key = 'newnexus.theme';
let memoryMode: ThemeMode = 'system';
export function storedTheme(): ThemeMode { try { const value = localStorage.getItem(key); memoryMode = value === 'light' || value === 'dark' ? value : 'system'; } catch { /* Keep the current session preference if storage is blocked. */ } return memoryMode; }
export function applyTheme(mode: ThemeMode) { const dark = mode === 'dark' || (mode === 'system' && matchMedia('(prefers-color-scheme: dark)').matches); document.documentElement.classList.toggle('dark', dark); document.documentElement.dataset.theme = dark ? 'dark' : 'light'; document.documentElement.style.colorScheme = dark ? 'dark' : 'light'; }
export function saveTheme(mode: ThemeMode) { memoryMode = mode; try { localStorage.setItem(key, mode); } catch { /* Storage may be blocked by browser policy. */ } applyTheme(mode); window.dispatchEvent(new Event('newnexus:theme')); }
export function initializeTheme() { applyTheme(storedTheme()); const media = matchMedia('(prefers-color-scheme: dark)'); media.addEventListener('change', () => { if (storedTheme() === 'system') applyTheme('system'); }); window.addEventListener('storage', () => { applyTheme(storedTheme()); window.dispatchEvent(new Event('newnexus:theme')); }); }
export function useTheme() { const mode = useSyncExternalStore((listener) => { window.addEventListener('newnexus:theme', listener); return () => window.removeEventListener('newnexus:theme', listener); }, storedTheme, () => 'system' as ThemeMode); return { mode, setTheme: saveTheme }; }
