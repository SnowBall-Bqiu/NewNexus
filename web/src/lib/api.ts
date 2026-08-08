export class APIError extends Error {
  code: string;
  data: unknown;
  status: number;
  constructor(status: number, code: string, message: string, data?: unknown) {
    super(message);
    this.name = 'APIError';
    this.status = status;
    this.code = code;
    this.data = data;
  }
}

function csrfToken() {
  const entry = document.cookie.split('; ').find((part) => part.startsWith('newnexus_csrf='));
  return entry ? decodeURIComponent(entry.split('=').slice(1).join('=')) : '';
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json');
  const csrf = csrfToken();
  if (csrf) headers.set('X-CSRF-Token', csrf);
  const response = await fetch(`/api/v1${path}`, { ...init, headers, credentials: 'same-origin' });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new APIError(response.status, payload.error?.code ?? 'request_failed', payload.error?.message ?? `请求失败 (${response.status})`, payload.error?.data);
  }
  return payload.data as T;
}

export async function list<T>(path: string): Promise<{ data: T[]; total: number }> {
  const response = await fetch(`/api/v1${path}`, { credentials: 'same-origin' });
  const payload = await response.json();
  if (!response.ok) throw new APIError(response.status, payload.error?.code, payload.error?.message);
  return { data: payload.data ?? [], total: payload.meta?.total ?? 0 };
}

export function jsonBody(value: unknown): RequestInit {
  return { method: 'POST', body: JSON.stringify(value) };
}

export function formatDate(value?: string) {
  if (!value) return '尚未';
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(new Date(value));
}

export function formatNumber(value: number) {
  return new Intl.NumberFormat('zh-CN', { notation: value >= 10000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value);
}

export function formatBytesPerSecond(value: number) {
  if (!value) return '-';
  return `${(value / 1_000_000).toFixed(1)} MB/s`;
}
