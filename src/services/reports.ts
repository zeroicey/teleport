/**
 * D1 data access for `reports` and `share_tokens`.
 *
 * All SQL lives here. Prepared statements with `?` placeholders are used
 * everywhere — values are never interpolated into SQL text.
 */
import { hoursFromNow, generateShareToken, nowMs, safeJsonParse } from '../lib/util';
import type {
  CreateReportInput,
  PublicSharePayload,
  ReportFormat,
  ReportRow,
  ShareTokenPublic,
  ShareTokenRow,
} from '../types';

export interface CreateReportResult {
  report: ReportRow;
  share: ShareTokenPublic | null;
}

export class ReportsService {
  constructor(private readonly db: D1Database) {}

  // -- writes -----------------------------------------------------------------

  /**
   * Insert a report, optionally creating its first share token.
   *
   * `D1Database.batch()` wraps the statements in an implicit transaction, so a
   * failure while creating the token rolls the report insert back too — we never
   * persist a report whose promised share link does not exist.
   */
  async create(input: CreateReportInput, defaultShareHours = 0): Promise<CreateReportResult> {
    const id = crypto.randomUUID();
    const ts = nowMs();
    const format: ReportFormat = input.format ?? 'markdown';
    const metadataJson = JSON.stringify(input.metadata ?? {});

    const statements: D1PreparedStatement[] = [
      this.db
        .prepare(
          `INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        )
        .bind(id, input.title, input.category ?? 'general', format, input.content, metadataJson, ts, ts),
    ];

    const shareHours = input.autoShareHours ?? defaultShareHours;
    let token: string | null = null;
    let expiresAt = 0;

    if (shareHours !== undefined && shareHours !== null) {
      token = generateShareToken();
      expiresAt = hoursFromNow(shareHours, defaultShareHours);
      statements.push(
        this.db
          .prepare(
            `INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count)
             VALUES (?, ?, ?, ?, 1, 0)`,
          )
          .bind(token, id, ts, expiresAt),
      );
    }

    await this.db.batch(statements);

    const report: ReportRow = {
      id,
      title: input.title,
      category: input.category ?? 'general',
      format,
      content: input.content,
      metadata: metadataJson,
      created_at: ts,
      updated_at: ts,
    };

    return {
      report,
      share: token
        ? { token, expires_at: expiresAt, is_active: true, view_count: 0, created_at: ts }
        : null,
    };
  }

  /** Idempotent revoke: returns false when the token does not exist. */
  async revokeToken(token: string): Promise<boolean> {
    const result = await this.db
      .prepare(`UPDATE share_tokens SET is_active = 0 WHERE token = ? AND is_active = 1`)
      .bind(token)
      .run();
    if ((result.meta.changes ?? 0) > 0) return true;

    // Distinguish "already revoked" (still a success for the caller) from "unknown".
    const existing = await this.db
      .prepare(`SELECT token FROM share_tokens WHERE token = ?`)
      .bind(token)
      .first<{ token: string }>();
    return existing !== null;
  }

  /** Create an additional share token for an existing report. */
  async createShareToken(
    reportId: string,
    expiresInHours: number,
    at: number = nowMs(),
  ): Promise<ShareTokenPublic | null> {
    const exists = await this.db
      .prepare(`SELECT id FROM reports WHERE id = ?`)
      .bind(reportId)
      .first<{ id: string }>();
    if (!exists) return null;

    const token = generateShareToken();
    const expiresAt = hoursFromNow(expiresInHours, 0);

    await this.db
      .prepare(
        `INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active, view_count)
         VALUES (?, ?, ?, ?, 1, 0)`,
      )
      .bind(token, reportId, at, expiresAt)
      .run();

    return { token, expires_at: expiresAt, is_active: true, view_count: 0, created_at: at };
  }

  // -- reads ------------------------------------------------------------------

  async getReport(id: string): Promise<ReportRow | null> {
    return await this.db
      .prepare(`SELECT * FROM reports WHERE id = ?`)
      .bind(id)
      .first<ReportRow>();
  }

  async listReports(limit = 50, offset = 0, category?: string): Promise<ReportRow[]> {
    // Columns are projected explicitly so a future schema addition cannot
    // silently start shipping large content blobs to the list endpoint.
    const base = `SELECT id, title, category, format, metadata, created_at, updated_at FROM reports`;
    const stmt = category
      ? this.db
          .prepare(`${base} WHERE category = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`)
          .bind(category, limit, offset)
      : this.db.prepare(`${base} ORDER BY created_at DESC LIMIT ? OFFSET ?`).bind(limit, offset);

    const { results } = await stmt.all<ReportRow>();
    return results ?? [];
  }

  /** Share tokens for one report, newest first. */
  async listShareTokens(reportId: string): Promise<ShareTokenRow[]> {
    const { results } = await this.db
      .prepare(`SELECT * FROM share_tokens WHERE report_id = ? ORDER BY created_at DESC`)
      .bind(reportId)
      .all<ShareTokenRow>();
    return results ?? [];
  }

  /**
   * Resolve a share token to its report.
   *
   * Returns a discriminated result so the route can map "unknown" -> 404 and
   * "expired/revoked" -> 410 without a second query.
   */
  async resolveShare(token: string): Promise<
    | { status: 'ok'; payload: PublicSharePayload }
    | { status: 'missing' }
    | { status: 'gone' }
  > {
    const row = await this.db
      .prepare(
        `SELECT
           t.token, t.expires_at, t.is_active, t.view_count,
           r.id AS r_id, r.title, r.category, r.format, r.content,
           r.metadata, r.created_at AS r_created_at, r.updated_at AS r_updated_at
         FROM share_tokens t
         JOIN reports r ON r.id = t.report_id
         WHERE t.token = ?`,
      )
      .bind(token)
      .first<{
        token: string;
        expires_at: number;
        is_active: number;
        view_count: number;
        r_id: string;
        title: string;
        category: string;
        format: ReportFormat;
        content: string;
        metadata: string;
        r_created_at: number;
        r_updated_at: number;
      }>();

    // Unknown token and revoked token are indistinguishable to the caller.
    if (!row || row.is_active !== 1) return { status: 'missing' };

    const now = nowMs();
    if (row.expires_at !== 0 && now >= row.expires_at) return { status: 'gone' };

    return {
      status: 'ok',
      payload: {
        report: {
          id: row.r_id,
          title: row.title,
          category: row.category,
          format: row.format,
          content: row.content,
          metadata: safeJsonParse<Record<string, unknown>>(row.metadata, {}),
          created_at: row.r_created_at,
          updated_at: row.r_updated_at,
        },
        share: { token: row.token, expires_at: row.expires_at, view_count: row.view_count },
      },
    };
  }

  /**
   * Increment a view counter.
   *
   * Intentionally fire-and-forget from the reader's perspective: analytics must
   * never make a page view fail. Pass `ctx.waitUntil(...)` in the route.
   */
  async recordView(token: string): Promise<void> {
    await this.db
      .prepare(`UPDATE share_tokens SET view_count = view_count + 1 WHERE token = ?`)
      .bind(token)
      .run();
  }

  /** Housekeeping: drop tokens that expired before `beforeMs`. */
  async purgeExpiredTokens(beforeMs: number = nowMs()): Promise<number> {
    const result = await this.db
      .prepare(`DELETE FROM share_tokens WHERE is_active = 0 AND expires_at != 0 AND expires_at < ?`)
      .bind(beforeMs)
      .run();
    return result.meta.changes ?? 0;
  }
}
