/**
 * Share-token management.
 *
 *   POST /api/share/:token/revoke   — Bearer AGENT_SECRET_KEY (also used by the dashboard)
 *
 * Mounted under the same `/api/share` prefix as the public reader. The revoke
 * route carries its own `requireAgentAuth` middleware so the public GET stays
 * unauthenticated.
 */
import { Hono } from 'hono';
import { ApiError, ok } from '../lib/errors';
import { parseShareToken, requiredParam } from '../lib/validate';
import { requireAgentAuth } from '../middleware/index';
import { ReportsService } from '../services/reports';
import type { AppEnv } from '../types';

const revoke = new Hono<AppEnv>();

revoke.post('/:token/revoke', requireAgentAuth, async (c) => {
  const token = parseShareToken(requiredParam(c.req.param('token'), 'token'));
  const service = new ReportsService(c.env.DB);

  const found = await service.revokeToken(token);
  if (!found) throw ApiError.notFound('Share token not found');

  return c.json(ok({ token, is_active: false }, c.get('requestId')));
});

export default revoke;
