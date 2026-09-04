import { useEffect, useState } from 'react';
import { CircleHalf, Monitor, Moon, Sun } from './icons';
import { applyTheme, saveTheme, storedTheme, watchSystemTheme, type ThemeMode } from '../lib/theme';
const choices: { value: ThemeMode; label: string; Icon: typeof Sun }[] = [
  { value: 'light', label: '浅色', Icon: Sun },
  { value: 'dark', label: '深色', Icon: Moon },
  { value: 'mono', label: '黑白', Icon: CircleHalf },
  { value: 'system', label: '跟随系统', Icon: Monitor },
];
export function ThemeSwitcher() { const [mode, setMode] = useState(storedTheme); useEffect(() => watchSystemTheme(() => { if (storedTheme() === 'system') applyTheme('system'); }), []); return <div className="theme-switcher" role="radiogroup" aria-label="界面主题">{choices.map(({ value, label, Icon }) => <button key={value} type="button" className={mode === value ? 'theme-choice is-active' : 'theme-choice'} role="radio" aria-checked={mode === value} aria-label={label} title={label} onClick={() => { setMode(value); saveTheme(value); }}><Icon size={16} /></button>)}</div>; }
