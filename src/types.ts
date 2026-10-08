/**
 * Application-level types.
 *
 * The `Env` interface (D1 binding, assets binding, vars and secrets) is
 * generated from wrangler.toml by `npm run cf-typegen` into
 * `worker-configuration.d.ts` — do not hand-write it here.
 */

/** A row in the `reports` table. */
export interface ReportRow {
  id: string;
  title: string;
  category: string;
  format: ReportFormat;
  content: string;
  /** JSON-encoded string as stored in D1. Parse with `parseMetadata`. */
  metadata: string;
  created_at: number;
  updated_at: number;
}

export type ReportFormat = 'markdown' | 'html';

/** A row in the `share_tokens` table. */
export interface ShareTokenRow {
  token: string;
  report_id: string;
  created_at: number;
  /** Epoch ms, or 0 for "never expires". */
  expires_at: number;
  /** SQLite has no boolean type: 0 | 1. */
  is_active: number;
  view_count: number;
}

/** `reports` + its sharing state, as returned by the admin API. */
export interface ReportWithShares extends Omit<ReportRow, 'metadata'> {
  metadata: Record<string, unknown>;
  share_tokens: ShareTokenPublic[];
}

/** A share token as exposed over the wire (no internal columns). */
export interface ShareTokenPublic {
  token: string;
  expires_at: number;
  is_active: boolean;
  view_count: number;
  created_at: number;
}

/** Response body of `GET /api/share/:token` and the share page data source. */
export interface PublicSharePayload {
  report: {
    id: string;
    title: string;
    category: string;
    format: ReportFormat;
    content: string;
    metadata: Record<string, unknown>;
    created_at: number;
    updated_at: number;
  };
  share: {
    token: string;
    expires_at: number;
    view_count: number;
  };
}

/** Body accepted by `POST /api/reports`. */
export interface CreateReportInput {
  title: string;
  category?: string;
  format?: ReportFormat;
  content: string;
  metadata?: Record<string, unknown>;
  /** When present and > 0, auto-create a share link valid for N hours. */
  autoShareHours?: number;
}

/** Hono environment: bindings + per-request variables. */
export type AppEnv = {
  Bindings: Env;
  Variables: {
    requestId: string;
    /** Present only on admin-authenticated requests. */
    session?: SessionPayload;
  };
};

/** Contents of the signed dashboard session cookie. */
export interface SessionPayload {
  sub: string;
  iat: number;
  exp: number;
}
