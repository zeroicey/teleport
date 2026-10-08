/**
 * Public read-only share API.
 *
 *   GET /api/share/:token   — no authentication
 */
import { Hono } from 'hono';
import { ApiError, ok } from '../lib/errors';
import { parseShareToken, requiredParam } from '../lib/validate';
import { ReportsService } from '../services/reports';
import type { AppEnv } from '../types';

const share = new Hono<AppEnv>();

share.get('/:token', async (c) => {
  const token = parseShareToken(requiredParam(c.req.param('token'), 'token'));
  const service = new ReportsService(c.env.DB);

  const result = await service.resolveShare(token);

  if (result.status === 'missing') throw ApiError.notFound();
  if (result.status === 'gone') throw ApiError.gone();

  // Analytics must not be able to fail the request.
  c.executionCtx.waitUntil(service.recordView(token).catch(() => undefined));

  return c.json(ok(result.payload, c.get('requestId')));
});

export default share;
