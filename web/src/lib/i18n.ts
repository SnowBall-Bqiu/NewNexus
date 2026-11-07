import zhCN from '@/locales/zh-CN.json';

export const locale = 'zh-CN';
export const messages: Readonly<Record<string, string>> = zhCN;
export type TranslationValues = Readonly<Record<string, string | number | boolean | null | undefined>>;

/** Keep named placeholders intact when data is missing; values are never re-parsed. */
export function interpolate(message: string, values: TranslationValues = {}): string {
  return message.replace(/\{([A-Za-z][A-Za-z0-9_]*)\}/g, (placeholder, name: string) => {
    const value = values[name];
    return Object.hasOwn(values, name) && value !== undefined ? String(value ?? '') : placeholder;
  });
}

export function t(key: string, values?: TranslationValues): string;
export function t(key: string, fallback?: string, values?: TranslationValues): string;
export function t(key: string, fallbackOrValues?: string | TranslationValues, values?: TranslationValues): string {
  const fallback = typeof fallbackOrValues === 'string' ? fallbackOrValues : undefined;
  return interpolate(Object.hasOwn(messages, key) ? messages[key] : fallback ?? key, typeof fallbackOrValues === 'object' ? fallbackOrValues : values);
}
