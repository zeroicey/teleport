/**
 * Teleport — Cloudflare Worker entry point.
 *
 * Responsibilities
 *   - compose middleware and route modules
 *   - decide when a request must hit the Worker vs. be served as a static asset
 *   - map thrown errors to a stable JSON envelope
 *
 * Route map
 *   POST   /api/reports                  agent report ingestion      (Bearer)
 *   GET    /api/reports/:id              read a report               (Bearer, see note)
 *   GET    /api/share/:token             public read-only share data (public)
 *   POST   /api/share/:token/revoke      disable a share link        (Bearer)
 *   GET    /s/:token                     public rendered share page  (public)
 *   /api/admin/*                         dashboard API               (session)
 *   everything else                      Workers Assets (SPA)
 *
 * Asset routing is configured in wrangler.toml via
 * `[assets] run_worker_first = ["/api/*", "/s/*"]`.
 */
import { Hono } from 'hono';
import admin from './routes/admin';
import reports from './routes/reports';
import revoke from './routes/revoke';
import share from './routes/share';
import sharePage from './routes/sharePage';
import {
  cors,
  requireAgentAuth,
  requestId,
  securityHeaders,
  notFound,
  onError,
} from './middleware/index';
import type { AppEnv } from './types';

const app = new Hono<AppEnv>();

// ---------------------------------------------------------------------------
// Global middleware (order matters: outermost first)
// ---------------------------------------------------------------------------
app.use('*', requestId);
app.use('*', cors);
app.use('*', securityHeaders);

// ---------------------------------------------------------------------------
// Health check — cheap, unauthenticated, useful for uptime probes.
// ---------------------------------------------------------------------------
app.get('/api/health', async (c) => {
  let database = 'unknown';
  try {
    const row = await c.env.DB.prepare('SELECT 1 AS ok').first<{ ok: number }>();
    database = row?.ok === 1 ? 'ok' : 'degraded';
  } catch (err) {
    console.error('Health check DB probe failed:', err);
    database = 'error';
  }

  return c.json({
    ok: database === 'ok',
    service: 'teleport',
    environment: c.env.ENVIRONMENT ?? 'development',
    database,
    timestamp: Date.now(),
  });
});

// ---------------------------------------------------------------------------
// Agent API — the only route that mutates reports.
// ---------------------------------------------------------------------------
app.route('/api/reports', reports);

// ---------------------------------------------------------------------------
// Share routes.
//   `/revoke` is mounted first so POST /api/share/:token/revoke resolves to the
//   authenticated handler rather than falling through to the public reader.
// ---------------------------------------------------------------------------
app.route('/api/share', revoke);
app.route('/api/share', share);

// ---------------------------------------------------------------------------
// Private dashboard API.
// ---------------------------------------------------------------------------
app.route('/api/admin', admin);

// ---------------------------------------------------------------------------
// Public share page (server-rendered so it can return real 404/410).
// ---------------------------------------------------------------------------
app.route('/s', sharePage);

// ---------------------------------------------------------------------------
// Static assets fallthrough.
//
// Requests for /api/* or /s/* that reach here matched no Worker route: they are
// API/asset-namespace paths, so a JSON 404 is more correct than the SPA shell.
// Everything else is handed to the Assets binding, which serves `public/` with
// the SPA fallback configured in wrangler.toml.
// ---------------------------------------------------------------------------
app.notFound(async (c) => {
  const { pathname } = new URL(c.req.url);

  if (pathname.startsWith('/api/')) {
    return notFound(c);
  }

  if (c.env.ASSETS) {
    return c.env.ASSETS.fetch(c.req.raw);
  }

  return notFound(c);
});

app.onError(onError);

export default app;
