/**
 * Typed wrapper around the dashboard API.
 *
 * Every response from the Go backend uses the envelope
 *   { ok: true, data } | { ok: false, error: { code, message } }
 * so this module unwraps it once and throws `ApiError` for the UI to catch.
 */
import { ref } from 'vue';

/**
 * Application base path, derived from Vite's `base` (see vite.config.ts).
 *
 * This deployment does not live at an origin root: the app is mounted under a
 * route prefix (`/yeciorez/teleport/`) on a host shared with other services, so
 * a root-relative `/api/...` would hit the wrong service entirely. `BASE_URL`
 * is exactly that prefix, with a trailing slash, so trimming it gives the
 * string to prepend to every request path.
 *
 * Because it comes from `base`, the prefix is configured in one place and
 * cannot drift between the HTML, the bundles and the API calls.
 */
export const APP_BASE = import.meta.env.BASE_URL.replace(/\/+$/, '');

/**
 * Optional API origin override, for pointing a build at a different host
 * (e.g. a local backend while debugging against a deployed frontend).
 *
 * Production leaves `VITE_API_BASE` unset: every request is same-origin, which
 * keeps the session cookie first-party and therefore immune to third-party
 * cookie blocking. Setting it switches `request()` to credentialed
 * cross-origin fetches.
 */
const API_ORIGIN = (import.meta.env.VITE_API_BASE ?? '').replace(/\/+$/, '');

/** True when requests leave this origin and must opt into sending cookies. */
export const isCrossOrigin = API_ORIGIN !== '';

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
  /**
   * The agent_keys.id that published this report, or '' for the root
   * (break-glass) credential and for rows that predate ownership.
   *
   * Only the dashboard list carries this: the agent-facing list omits it because
   * every row there is already the caller's own.
   */
  owner_key_id?: string;
  /** Human label for `owner_key_id`, resolved server-side. */
  owner_name?: string;
}

export interface ReportDetail extends ReportSummary {
  content: string;
  share_tokens: ShareToken[];
}

// -- agent keys (panel) -------------------------------------------------------
//
// Response field names are snake_case, mirroring the domain json tags
// (`created_at`, `expires_at`, `token_prefix`, `requested_hours`, ...), and the
// list endpoints return bare JSON arrays — same as `GET /api/admin/reports`.
// Request bodies keep the contract's camelCase (`expiresInHours`, `revoked`).
//
// The server-side token hash is neither requested nor modelled: the API never
// returns it, so no field for it exists here.

/** One record as it arrives on the wire. */
type RawRecord = Record<string, unknown>;

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

/** Panel list responses are bare arrays; anything else means "no rows". */
function asRecords(data: unknown): RawRecord[] {
  return Array.isArray(data)
    ? data.filter((v): v is RawRecord => !!v && typeof v === 'object')
    : [];
}

/** A stored key. `expires_at`/`revoked_at`/`last_used_at` are epoch ms; 0 = never. */
export interface AgentKey {
  id: string;
  name: string;
  token_prefix: string;
  created_at: number;
  expires_at: number;
  revoked_at: number;
  last_used_at: number;
  request_count: number;
  note: string;
}

/** A key application from `GET /api/admin/key-applications`. */
export interface KeyApplication {
  id: string;
  label: string;
  purpose: string;
  requested_hours: number;
  status: string;
  created_at: number;
  expires_at: number;
  decided_at: number;
  claim_deadline: number;
  issued_key_id: string;
  requester_ip: string;
  user_agent: string;
  /** Decision fields; empty while the application is still pending. */
  approved_name: string;
  approved_hours: number;
  approved_note: string;
}

/** A renewal request from `GET /api/admin/key-renewals`. */
export interface KeyRenewal {
  id: string;
  key_id: string;
  requested_hours: number;
  status: string;
  created_at: number;
  decided_at: number;
  granted_expires_at: number;
}

/**
 * Response of `POST /api/admin/keys`: `{"token": "<plaintext>", "key": {…}}`.
 *
 * The plaintext is nested on purpose: a one-time secret kept structurally apart
 * from the persisted record cannot be forwarded by accident when `key` is passed
 * around. It is rendered once and dropped from memory when the dialog closes.
 */
export interface IssuedAgentKey {
  token: string;
  key: AgentKey | null;
}

function toAgentKey(raw: RawRecord): AgentKey {
  return {
    id: asString(raw.id),
    name: asString(raw.name),
    token_prefix: asString(raw.token_prefix),
    created_at: asNumber(raw.created_at),
    expires_at: asNumber(raw.expires_at),
    revoked_at: asNumber(raw.revoked_at),
    last_used_at: asNumber(raw.last_used_at),
    request_count: asNumber(raw.request_count),
    note: asString(raw.note),
  };
}

function toKeyApplication(raw: RawRecord): KeyApplication {
  return {
    id: asString(raw.id),
    label: asString(raw.label),
    purpose: asString(raw.purpose),
    requested_hours: asNumber(raw.requested_hours),
    status: asString(raw.status) || 'pending',
    created_at: asNumber(raw.created_at),
    expires_at: asNumber(raw.expires_at),
    decided_at: asNumber(raw.decided_at),
    claim_deadline: asNumber(raw.claim_deadline),
    issued_key_id: asString(raw.issued_key_id),
    requester_ip: asString(raw.requester_ip),
    user_agent: asString(raw.user_agent),
    approved_name: asString(raw.approved_name),
    approved_hours: asNumber(raw.approved_hours),
    approved_note: asString(raw.approved_note),
  };
}

function toKeyRenewal(raw: RawRecord): KeyRenewal {
  return {
    id: asString(raw.id),
    key_id: asString(raw.key_id),
    requested_hours: asNumber(raw.requested_hours),
    status: asString(raw.status) || 'pending',
    created_at: asNumber(raw.created_at),
    decided_at: asNumber(raw.decided_at),
    granted_expires_at: asNumber(raw.granted_expires_at),
  };
}

function toIssuedAgentKey(data: unknown): IssuedAgentKey {
  const raw = (data && typeof data === 'object' ? data : {}) as RawRecord;
  const nested = raw.key;
  return {
    token: asString(raw.token),
    key: nested && typeof nested === 'object' ? toAgentKey(nested as RawRecord) : null,
  };
}

/** Raised when the session expired mid-session, so the app can redirect. */
export const sessionExpired = ref(false);

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${API_ORIGIN}${APP_BASE}${path}`, {
      // Same-origin (production) keeps the default first-party cookie rules;
      // an explicit cross-origin API_ORIGIN must ask for cookies explicitly
      // (`include`), because `same-origin` would silently drop the session.
      credentials: isCrossOrigin ? 'include' : 'same-origin',
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

  /**
   * Update a report in place.
   *
   * `patch` is a genuine partial update: omitted keys are left untouched, so
   * sending only `content` does not blank the title. `metadata` is the
   * exception — it REPLACES the stored object rather than merging, because
   * merging cannot express "remove this key". Send `{}` to clear it.
   *
   * Two of these keys are deliberately absent:
   *
   *  - `id` / `created_at`: identity is not editable.
   *  - `owner_key_id`: ownership is a security boundary, not data. The server
   *    rejects a body carrying it rather than ignoring it.
   *
   * Sending a field the server does not know is a 400, not a silent no-op.
   * That is on purpose: a typo (the classic one being `Content`, since request
   * bodies are camelCase while responses are snake_case) would otherwise return
   * 200 while changing nothing.
   */
  async updateReport(
    id: string,
    patch: {
      title?: string;
      category?: string;
      format?: 'markdown' | 'html';
      content?: string;
      metadata?: Record<string, unknown>;
    },
  ) {
    return request<ReportDetail>(`/api/admin/reports/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  },

  /**
   * Delete a report and, with it, every share link pointing at it.
   *
   * The cascade is not a side effect to be worked around — it is the meaning of
   * deleting a report. Anyone holding one of those links gets a 404 from the
   * next request, so the confirmation in the UI has to say so before the fact.
   * There is no undo and no recycle bin.
   */
  async deleteReport(id: string) {
    return request<{ id: string; deleted: boolean }>(
      `/api/admin/reports/${encodeURIComponent(id)}`,
      { method: 'DELETE' },
    );
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

  // -- agent keys: approval queue -------------------------------------------
  async listKeyApplications(status = 'pending') {
    const data = await request<unknown>(
      `/api/admin/key-applications?status=${encodeURIComponent(status)}`,
    );
    return asRecords(data).map(toKeyApplication);
  },

  /** `expiresInHours: 0` means never expires. */
  async approveKeyApplication(
    id: string,
    body: { name?: string; expiresInHours?: number; note?: string } = {},
  ) {
    return request<unknown>(`/api/admin/key-applications/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      body: JSON.stringify(body),
    });
  },

  async rejectKeyApplication(id: string, body: { reason?: string } = {}) {
    return request<unknown>(`/api/admin/key-applications/${encodeURIComponent(id)}/reject`, {
      method: 'POST',
      body: JSON.stringify(body),
    });
  },

  // -- agent keys: key list ---------------------------------------------------
  async listAgentKeys() {
    const data = await request<unknown>('/api/admin/keys');
    return asRecords(data).map(toAgentKey);
  },

  /** Manual hand-off path: the response carries the plaintext token exactly once. */
  async createAgentKey(body: { name: string; expiresInHours: number }) {
    return toIssuedAgentKey(
      await request<unknown>('/api/admin/keys', {
        method: 'POST',
        body: JSON.stringify(body),
      }),
    );
  },

  /** Changing `expiresInHours` is the user-side renewal; `revoked: true` revokes. */
  async updateAgentKey(
    id: string,
    patch: { name?: string; expiresInHours?: number; revoked?: boolean },
  ) {
    const data = await request<unknown>(`/api/admin/keys/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
    return data && typeof data === 'object' ? toAgentKey(data as RawRecord) : null;
  },

  // -- agent keys: renewal queue ---------------------------------------------
  async listKeyRenewals(status = 'pending') {
    const data = await request<unknown>(
      `/api/admin/key-renewals?status=${encodeURIComponent(status)}`,
    );
    return asRecords(data).map(toKeyRenewal);
  },

  /** Omitted `expiresInHours` lets the server fall back to `requestedHours`. */
  async approveKeyRenewal(id: string, body: { expiresInHours?: number } = {}) {
    return request<unknown>(`/api/admin/key-renewals/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      body: JSON.stringify(body),
    });
  },

  async rejectKeyRenewal(id: string) {
    return request<unknown>(`/api/admin/key-renewals/${encodeURIComponent(id)}/reject`, {
      method: 'POST',
      body: JSON.stringify({}),
    });
  },
};
