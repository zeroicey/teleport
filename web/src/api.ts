/**
 * Typed wrapper around the dashboard API.
 *
 * Every response from the Worker uses the envelope
 *   { ok: true, data } | { ok: false, error: { code, message } }
 * so this module unwraps it once and throws `ApiError` for the UI to catch.
 */
import { ref } from 'vue';

export interface ApiErrorBody {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: Record<string, unknown>;

  constructor(status: number, code: string, message: string, details?: Record<string, unknown>) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }

  /** True when the failure is "you are not logged in". */
  get isAuthError(): boolean {
    return this.status === 401;
  }
}

export interface ShareToken {
  token: string;
  expires_at: number;
  is_active: boolean;
  view_count: number;
  created_at: number;
  url?: string;
}

export interface ReportSummary {
  id: string;
  title: string;
  category: string;
  format: 'markdown' | 'html';
  metadata: Record<string, unknown>;
  created_at: number;
  updated_at: number;
}

export interface ReportDetail extends ReportSummary {
  content: string;
  share_tokens: ShareToken[];
}

/** Raised when the session expired mid-session, so the app can redirect. */
export const sessionExpired = ref(false);

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      credentials: 'same-origin',
      ...init,
      headers: {
        ...(init.body ? { 'Content-Type': 'application/json' } : {}),
        ...(init.headers ?? {}),
      },
    });
  } catch {
    throw new ApiError(0, 'network_error', '无法连接到服务器，请检查网络');
  }

  let body: unknown = null;
  const text = await response.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      throw new ApiError(response.status, 'bad_response', '服务器返回了非预期的响应');
    }
  }

  const envelope = body as { ok?: boolean; data?: T; error?: ApiErrorBody } | null;

  if (!response.ok || envelope?.ok === false) {
    const error = envelope?.error;
    if (response.status === 401) sessionExpired.value = true;
    throw new ApiError(
      response.status,
      error?.code ?? 'unknown',
      error?.message ?? `请求失败 (HTTP ${response.status})`,
      error?.details,
    );
  }

  return envelope?.data as T;
}

export const api = {
  // -- session ---------------------------------------------------------------
  async login(password: string) {
    return request<{ authenticated: boolean; expires_at: number }>('/api/admin/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
    });
  },

  async logout() {
    return request<{ authenticated: boolean }>('/api/admin/logout', { method: 'POST' });
  },

  async checkSession() {
    return request<{ authenticated: boolean; subject?: string }>('/api/admin/session');
  },

  // -- reports ---------------------------------------------------------------
  async listReports(params: { limit?: number; offset?: number; category?: string } = {}) {
    const query = new URLSearchParams();
    if (params.limit !== undefined) query.set('limit', String(params.limit));
    if (params.offset !== undefined) query.set('offset', String(params.offset));
    if (params.category) query.set('category', params.category);
    const suffix = query.toString() ? `?${query}` : '';
    return request<ReportSummary[]>(`/api/admin/reports${suffix}`);
  },

  async getReport(id: string) {
    return request<ReportDetail>(`/api/admin/reports/${encodeURIComponent(id)}`);
  },

  // -- share tokens ----------------------------------------------------------
  async createShare(reportId: string, expiresInHours: number) {
    return request<ShareToken>(
      `/api/admin/reports/${encodeURIComponent(reportId)}/shares`,
      { method: 'POST', body: JSON.stringify({ expiresInHours }) },
    );
  },

  async updateShare(
    token: string,
    patch: { expiresInHours?: number; expiresAt?: number; isActive?: boolean },
  ) {
    return request<ShareToken>(`/api/admin/shares/${encodeURIComponent(token)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  },

  async revokeShare(token: string) {
    return request<{ token: string; is_active: boolean }>(
      `/api/admin/shares/${encodeURIComponent(token)}`,
      { method: 'DELETE' },
    );
  },
};
