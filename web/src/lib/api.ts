export class ApiError extends Error { constructor(public status: number, message: string, public code?: string) { super(message); this.name = 'ApiError'; } }

async function request<T>(path: string, init?: RequestInit): Promise<{ value: T; meta?: { total?: number; nextCursor?: string } }> {
  const response = await fetch(path.startsWith('/api/') ? path : `/api/v1${path}`, { credentials: 'include', ...init, headers: { Accept: 'application/json', ...(init?.headers ?? {}) } });
  const body = await response.json().catch(() => ({})) as { data?: T; meta?: { total?: number; nextCursor?: string }; error?: { message?: string; code?: string } };
  if (!response.ok) throw new ApiError(response.status, body.error?.message ?? `请求失败（${response.status}）`, body.error?.code);
  return { value: (body.data ?? body) as T, meta: body.meta };
}

export async function api<T = void>(path: string, init?: RequestInit): Promise<T> { return (await request<T>(path, init)).value; }
export async function list<T>(path: string, init?: RequestInit): Promise<import('../types').ListResult<T>> { const result = await request<T[]>(path, init); return { data: result.value ?? [], total: result.meta?.total ?? result.value?.length ?? 0, nextCursor: result.meta?.nextCursor }; }
export const jsonBody = (body: unknown, init: RequestInit = {}): RequestInit => ({ ...init, method: init.method ?? 'POST', headers: { 'Content-Type': 'application/json', ...(init.headers ?? {}) }, body: JSON.stringify(body) });
export function formatDate(value?: string) { if (!value || value.startsWith('0001-')) return '尚未'; const date = new Date(value); return Number.isNaN(date.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date); }
export function formatNumber(value: number | undefined) { return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 }).format(value ?? 0); }
