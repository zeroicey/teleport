/**
 * Cross-cutting middleware: request ids, CORS, security headers, error mapping.
 */
import type { Context, Next } from 'hono';
import { ApiError, fail } from '../lib/errors';
import { readPublicConfig } from '../lib/config';
import { assertAgentSecret, SESSION_COOKIE_NAME, verifySession } from '../services/auth';
import type { AppEnv } from '../types';

/** Attach a correlation id to every request and echo it back. */
export async function requestId(c: Context<AppEnv>, next: Next) {
  const id = c.req.header('cf-ray') ?? crypto.randomUUID();
  c.set('requestId', id);
  c.header('X-Request-Id', id);
  await next();
}

/**
 * Strict-ish CORS.
 *
 * The agent API is called server-to-server, so it needs no CORS at all. The
 * dashboard is same-origin. We still answer preflight for browser tooling but
 * never reflect an arbitrary Origin with credentials attached.
 */
export async function cors(c: Context<AppEnv>, next: Next) {
  const origin = c.req.header('Origin');
  if (origin) {
    // Only the configured public origin is echoed back — never an arbitrary
    // Origin. Note this must not require secrets: public share reads must work
    // even if the agent/admin secrets are unset.
    const { publicBaseUrl } = readPublicConfig(c.env);
    if (publicBaseUrl && origin === publicBaseUrl) {
      c.header('Access-Control-Allow-Origin', origin);
      c.header('Vary', 'Origin');
      c.header('Access-Control-Allow-Methods', 'GET,POST,PATCH,DELETE,OPTIONS');
      c.header('Access-Control-Allow-Headers', 'Authorization,Content-Type');
      c.header('Access-Control-Max-Age', '86400');
    }
  }

  if (c.req.method === 'OPTIONS') return c.body(null, 204);
  await next();
}

/** Baseline hardening headers. */
export async function securityHeaders(c: Context<AppEnv>, next: Next) {
  await next();
  c.header('X-Content-Type-Options', 'nosniff');
  c.header('Referrer-Policy', 'no-referrer');
  c.header('X-Frame-Options', 'DENY');
  // The share pages are public but must never be indexed or cached by shared
  // proxies — a revoked link should not stay readable from a cache.
  c.header('X-Robots-Tag', 'noindex, nofollow, noarchive');
}

/**
 * Require a valid agent Bearer token.
 * Apply to POST /api/reports and every /api/admin/* route.
 */
export async function requireAgentAuth(c: Context<AppEnv>, next: Next) {
  assertAgentSecret(c.req.header('Authorization'), c.env.AGENT_SECRET_KEY);
  await next();
}

/**
 * Require a valid dashboard session cookie.
 *
 * Cloudflare Access (Zero Trust) can front /dashboard instead; if the
 * `Cf-Access-Authenticated-User-Email` header is present and the request came
 * through an Access-protected hostname, that is accepted as a session too.
 */
export async function requireSession(c: Context<AppEnv>, next: Next) {
  const accessEmail = c.req.header('Cf-Access-Authenticated-User-Email');
  if (accessEmail) {
    c.set('session', {
      sub: accessEmail,
      iat: Math.floor(Date.now() / 1000),
      exp: Math.floor(Date.now() / 1000) + 60,
    });
    return next();
  }

  const cookie = readCookie(c.req.header('Cookie'), SESSION_COOKIE_NAME);
  const session = await verifySession(cookie, c.env.SESSION_SECRET);
  if (!session) {
    throw ApiError.unauthorized('Dashboard session required');
  }

  c.set('session', session);
  await next();
}

/** Parse a single cookie value without pulling in a cookie library. */
export function readCookie(header: string | undefined, name: string): string | undefined {
  if (!header) return undefined;
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq === -1) continue;
    if (part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim();
  }
  return undefined;
}

/** Map thrown ApiErrors (and unexpected errors) to a consistent JSON envelope. */
export function onError(err: Error, c: Context<AppEnv>) {
  const requestId = c.get('requestId');

  if (err instanceof ApiError) {
    return c.json(fail(err.code, err.message, err.details, requestId), err.status as 400);
  }

  // Log the real cause server-side, return an opaque message to the client.
  console.error(`[${requestId}] Unhandled error:`, err);
  return c.json(fail('internal_error', 'Internal server error', undefined, requestId), 500);
}

export function notFound(c: Context<AppEnv>) {
  return c.json(
    fail('not_found', 'No route matches this request', undefined, c.get('requestId')),
    404,
  );
}
