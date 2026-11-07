import { t } from '@/lib/i18n';
export type Resource = { id: string; createdAt?: string; updatedAt?: string; [key: string]: unknown };
export type ListResult<T> = { data: T[]; total: number; nextCursor?: string };
let csrfToken = '';
export function setCsrfToken(token: string) { csrfToken = token; }
function transformKeys(value: unknown, snake: boolean): unknown {
  if (Array.isArray(value)) return value.map((item) => transformKeys(item, snake));
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => {
    const name = snake ? key.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`) : key.replace(/_([a-z])/g, (_, letter: string) => letter.toUpperCase());
    // Header names, settings values and provider zone IDs are data, rather than model field names.
    const settingValue = key === 'value' && typeof (value as Record<string, unknown>).key === 'string' && String((value as Record<string, unknown>).key).includes('.');
    if (settingValue || ['headers', 'settings', 'credentials', 'capabilities'].includes(key)) return [name, item];
    if (['zone_capabilities', 'zoneCapabilities', 'capability_errors', 'capabilityErrors', 'lines'].includes(key) && item && typeof item === 'object' && !Array.isArray(item)) return [name, Object.fromEntries(Object.entries(item).map(([id, entry]) => [id, transformKeys(entry, snake)]))];
    return [name, transformKeys(item, snake)];
  }));
  return value;
}
export class ApiError extends Error {
  constructor(message: string, public status: number, public code: string) { super(message); this.name = 'ApiError'; }
}
export async function api<T = unknown>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers);
  if (options.body) headers.set('Content-Type', 'application/json');
  if (csrfToken && !['GET', 'HEAD'].includes(options.method ?? 'GET')) headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(`/api/v1${path}`, { ...options, credentials: 'same-origin', headers });
  const text = await response.text();
  let data: unknown;
  try { data = text ? JSON.parse(text) : undefined; } catch { throw new ApiError(t("api.invalidResponse"), response.status, 'invalid_response'); }
  if (!response.ok) { const error = (data as { error?: { message?: string; code?: string } })?.error; throw new ApiError(error?.message ?? t("api.requestFailed", { status: response.status }), response.status, error?.code ?? 'request_failed'); }
  return transformKeys(data, false) as T;
}
export function jsonBody(value: unknown, method = 'POST'): RequestInit { return { method, body: JSON.stringify(transformKeys(value, true)) }; }
export async function list<T>(path: string): Promise<ListResult<T>> { const value = await api<{ items?: T[]; data?: T[]; total?: number; nextCursor?: string }>(path); return { data: value.items ?? value.data ?? [], total: value.total ?? (value.items ?? value.data ?? []).length, nextCursor: value.nextCursor }; }
export function formatDate(value?: string) { if (!value) return '—'; const date = new Date(value); return Number.isNaN(date.valueOf()) ? '—' : new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date); }
export function formatNumber(value?: number) { return new Intl.NumberFormat('zh-CN').format(value ?? 0); }
