/**
 * Agent-facing report ingestion API.
 *
 *   POST /api/reports   — Bearer AGENT_SECRET_KEY
 */
import { Hono } from 'hono';
import { ApiError, ok } from '../lib/errors';
import { parseCreateReportInput, parseJsonBody, requiredParam } from '../lib/validate';
import { readConfig } from '../lib/config';
import { requireAgentAuth } from '../middleware/index';
import { ReportsService } from '../services/reports';
import { safeJsonParse } from '../lib/util';
import type { AppEnv } from '../types';

const reports = new Hono<AppEnv>();

// Every route on this router is agent-only. Applying the guard at the router
// level (rather than per-handler) means a newly added route is protected by
// default instead of silently public.
reports.use('*', requireAgentAuth);

reports.post('/', async (c) => {
  const config = readConfig(c.env);

  const raw = await c.req.text();
  if (raw.length === 0) throw ApiError.badRequest('Request body is required');
  const input = parseCreateReportInput(parseJsonBody(raw), {
    maxContentBytes: config.maxContentBytes,
  });

  const service = new ReportsService(c.env.DB);
  const { report, share } = await service.create(input, config.defaultShareHours);

  // Build the absolute share URL server-side so callers never have to know the
  // public origin.
  const shareUrl = share ? `${config.publicBaseUrl}/s/${share.token}` : null;

  return c.json(
    ok(
      {
        id: report.id,
        title: report.title,
        category: report.category,
        format: report.format,
        created_at: report.created_at,
        updated_at: report.updated_at,
        share: share ? { ...share, url: shareUrl } : null,
      },
      c.get('requestId'),
    ),
    201,
  );
});

/** Convenience: fetch a single report's source (used by agents to verify). */
reports.get('/:id', async (c) => {
  const service = new ReportsService(c.env.DB);
  const report = await service.getReport(requiredParam(c.req.param('id'), 'id'));
  if (!report) throw ApiError.notFound('Report not found');

  return c.json(
    ok(
      {
        ...report,
        metadata: safeJsonParse<Record<string, unknown>>(report.metadata, {}),
      },
      c.get('requestId'),
    ),
  );
});

export default reports;
