/**
 * Private dashboard API.
 *
 *   POST   /api/admin/login                  — password -> session cookie
 *   POST   /api/admin/logout
 *   GET    /api/admin/reports                — list all reports
 *   GET    /api/admin/reports/:id            — full report + its share tokens
 *   POST   /api/admin/reports/:id/shares     — mint a new share token
 *   PATCH  /api/admin/shares/:token          — adjust expiry / active state
 *   DELETE /api/admin/shares/:token          — revoke (soft delete)
 *
 * Auth: signed session cookie, or a Cloudflare Access identity header.
 */
import { Hono } from 'hono';
import { ApiError, ok } from '../lib/errors';
import { parseJsonBody, requiredParam, requireString } from '../lib/validate';
import { readConfig } from '../lib/config';
import {
  buildSession,
  clearSessionCookie,
  sessionCookie,
  signSession,
} from '../services/auth';
import { assertPasswordHashConfigured, verifyPassword } from '../services/password';
import { requireSession } from '../middleware/index';
import { ReportsService } from '../services/reports';
import { safeJsonParse } from '../lib/util';
import type { AppEnv, ShareTokenPublic, ShareTokenRow } from '../types';

const admin = new Hono<AppEnv>();

// -- session ------------------------------------------------------------------

admin.post('/login', async (c) => {
  const config = readConfig(c.env);
  assertPasswordHashConfigured(config.adminPasswordHash);

  const raw = await c.req.text();
  const body = parseJsonBody(raw);
  if (typeof body !== 'object' || body === null) {
    throw ApiError.badRequest('Request body must be a JSON object');
  }
  const password = requireString((body as Record<string, unknown>).password, 'password', 1, 512);

  const valid = await verifyPassword(password, config.adminPasswordHash);
  if (!valid) {
    // Deliberately vague: do not reveal whether the account or the password was wrong.
    throw ApiError.unauthorized('Invalid credentials');
  }

  const session = buildSession('admin');
  const cookie = await signSession(session, config.sessionSecret);

  c.header('Set-Cookie', sessionCookie(cookie, session.exp - session.iat));
  return c.json(ok({ authenticated: true, expires_at: session.exp * 1000 }, c.get('requestId')));
});

admin.post('/logout', (c) => {
  c.header('Set-Cookie', clearSessionCookie());
  return c.json(ok({ authenticated: false }, c.get('requestId')));
});

admin.get('/session', requireSession, (c) =>
  c.json(ok({ authenticated: true, subject: c.get('session')?.sub }, c.get('requestId'))),
);

// -- reports ------------------------------------------------------------------

admin.get('/reports', requireSession, async (c) => {
  const limit = clampInt(c.req.query('limit'), 50, 1, 200);
  const offset = clampInt(c.req.query('offset'), 0, 0, 100_000);
  const category = c.req.query('category') || undefined;

  const service = new ReportsService(c.env.DB);
  const rows = await service.listReports(limit, offset, category);

  return c.json(
    ok(
      rows.map((r) => ({
        id: r.id,
        title: r.title,
        category: r.category,
        format: r.format,
        metadata: safeJsonParse<Record<string, unknown>>(r.metadata, {}),
        created_at: r.created_at,
        updated_at: r.updated_at,
      })),
      c.get('requestId'),
    ),
  );
});

admin.get('/reports/:id', requireSession, async (c) => {
  const service = new ReportsService(c.env.DB);
  const report = await service.getReport(requiredParam(c.req.param('id'), 'id'));
  if (!report) throw ApiError.notFound('Report not found');

  const tokens = await service.listShareTokens(report.id);

  return c.json(
    ok(
      {
        ...report,
        metadata: safeJsonParse<Record<string, unknown>>(report.metadata, {}),
        share_tokens: tokens.map(toPublicToken),
      },
      c.get('requestId'),
    ),
  );
});

/** Mint an additional share token for a report. */
admin.post('/reports/:id/shares', requireSession, async (c) => {
  const service = new ReportsService(c.env.DB);
  const reportId = requiredParam(c.req.param('id'), 'id');

  const raw = await c.req.text();
  const body = raw ? parseJsonBody(raw) : {};
  const hours = readHours(body);

  const created = await service.createShareToken(reportId, hours);
  if (!created) throw ApiError.notFound('Report not found');

  const config = readConfig(c.env);
  return c.json(
    ok({ ...created, url: `${config.publicBaseUrl}/s/${created.token}` }, c.get('requestId')),
    201,
  );
});

// -- tokens -------------------------------------------------------------------

/**
 * Update a token's expiry and/or active flag.
 *
 * Body (all optional, but at least one required):
 *   expiresInHours?: number  — set expiry to now + N hours; 0 = never expires
 *   expiresAt?: number       — absolute epoch ms; 0 = never expires
 *   isActive?: boolean       — manually disable / re-enable the link
 *
 * `expiresInHours` and `expiresAt` are mutually exclusive; `expiresInHours`
 * wins when both are present.
 */
admin.patch('/shares/:token', requireSession, async (c) => {
  const token = requiredParam(c.req.param('token'), 'token');
  const raw = await c.req.text();
  const body = parseJsonBody(raw);
  if (typeof body !== 'object' || body === null) {
    throw ApiError.badRequest('Request body must be a JSON object');
  }
  const patch = body as Record<string, unknown>;

  const sets: string[] = [];
  const binds: unknown[] = [];

  if (patch.expiresInHours !== undefined) {
    const hours = Number(patch.expiresInHours);
    if (!Number.isFinite(hours) || hours < 0) {
      throw ApiError.badRequest('`expiresInHours` must be a non-negative number', {
        field: 'expiresInHours',
      });
    }
    sets.push('expires_at = ?');
    binds.push(hours === 0 ? 0 : Date.now() + Math.round(hours * 3_600_000));
  } else if (patch.expiresAt !== undefined) {
    const expiresAt = Number(patch.expiresAt);
    if (!Number.isFinite(expiresAt) || expiresAt < 0) {
      throw ApiError.badRequest('`expiresAt` must be epoch ms (0 = never expires)', {
        field: 'expiresAt',
      });
    }
    sets.push('expires_at = ?');
    binds.push(Math.round(expiresAt));
  }

  if (patch.isActive !== undefined) {
    if (typeof patch.isActive !== 'boolean') {
      throw ApiError.badRequest('`isActive` must be a boolean', { field: 'isActive' });
    }
    sets.push('is_active = ?');
    binds.push(patch.isActive ? 1 : 0);
  }

  if (sets.length === 0) {
    throw ApiError.badRequest('Provide at least one of: expiresInHours, expiresAt, isActive');
  }

  const result = await c.env.DB
    .prepare(`UPDATE share_tokens SET ${sets.join(', ')} WHERE token = ?`)
    .bind(...binds, token)
    .run();

  if ((result.meta.changes ?? 0) === 0) {
    // Could be "no such token" or "no-op update" — check existence.
    const exists = await c.env.DB
      .prepare(`SELECT token FROM share_tokens WHERE token = ?`)
      .bind(token)
      .first<{ token: string }>();
    if (!exists) throw ApiError.notFound('Share token not found');
  }

  const updated = await c.env.DB
    .prepare(`SELECT * FROM share_tokens WHERE token = ?`)
    .bind(token)
    .first<ShareTokenRow>();

  if (!updated) throw ApiError.notFound('Share token not found');

  const config = readConfig(c.env);
  const pub = toPublicToken(updated);
  return c.json(
    ok({ ...pub, url: `${config.publicBaseUrl}/s/${updated.token}` }, c.get('requestId')),
  );
});

admin.delete('/shares/:token', requireSession, async (c) => {
  const token = requiredParam(c.req.param('token'), 'token');
  const service = new ReportsService(c.env.DB);
  const found = await service.revokeToken(token);
  if (!found) throw ApiError.notFound('Share token not found');

  return c.json(ok({ token, is_active: false }, c.get('requestId')));
});

// -- helpers ------------------------------------------------------------------

function toPublicToken(row: ShareTokenRow): ShareTokenPublic {
  return {
    token: row.token,
    expires_at: row.expires_at,
    is_active: row.is_active === 1,
    view_count: row.view_count,
    created_at: row.created_at,
  };
}

function readHours(body: unknown): number {
  if (typeof body !== 'object' || body === null) return 0;
  const value = (body as Record<string, unknown>).expiresInHours;
  if (value === undefined || value === null) return 0;
  const hours = Number(value);
  if (!Number.isFinite(hours) || hours < 0) {
    throw ApiError.badRequest('`expiresInHours` must be a non-negative number');
  }
  return hours;
}

function clampInt(value: string | undefined, fallback: number, min: number, max: number): number {
  const parsed = Number.parseInt(value ?? '', 10);
  if (!Number.isFinite(parsed)) return fallback;
  return Math.min(Math.max(parsed, min), max);
}

export default admin;
