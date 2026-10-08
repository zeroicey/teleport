/**
 * Dashboard session cookies.
 *
 * Stateless signed cookies: `base64url(payload).base64url(HMAC-SHA256)`. No
 * server-side session store is needed, which keeps the design serverless.
 *
 * The signature is verified by re-computing the HMAC over the *received*
 * payload bytes and comparing in constant time.
 */
import { timingSafeEqual } from '../lib/util';
import { ApiError } from '../lib/errors';
import type { SessionPayload } from '../types';

export const SESSION_COOKIE_NAME = 'teleport_session';
const DEFAULT_TTL_SECONDS = 60 * 60 * 12; // 12 hours

export async function signSession(
  payload: SessionPayload,
  secret: string,
): Promise<string> {
  const body = base64UrlEncode(new TextEncoder().encode(JSON.stringify(payload)));
  const sig = await hmac(body, secret);
  return `${body}.${sig}`;
}

/** Returns the payload, or null when the cookie is malformed/expired/forged. */
export async function verifySession(
  cookieValue: string | undefined,
  secret: string,
): Promise<SessionPayload | null> {
  if (!cookieValue) return null;

  const dot = cookieValue.indexOf('.');
  if (dot <= 0 || dot === cookieValue.length - 1) return null;

  const body = cookieValue.slice(0, dot);
  const providedSig = cookieValue.slice(dot + 1);
  const expectedSig = await hmac(body, secret);

  if (!timingSafeEqual(providedSig, expectedSig)) return null;

  let payload: SessionPayload;
  try {
    payload = JSON.parse(new TextDecoder().decode(base64UrlDecode(body))) as SessionPayload;
  } catch {
    return null;
  }

  if (typeof payload?.exp !== 'number' || Date.now() >= payload.exp * 1000) return null;
  return payload;
}

export function buildSession(subject: string, ttlSeconds = DEFAULT_TTL_SECONDS): SessionPayload {
  const now = Math.floor(Date.now() / 1000);
  return { sub: subject, iat: now, exp: now + ttlSeconds };
}

/** Serialize a Set-Cookie header value for the session. */
export function sessionCookie(token: string, maxAgeSeconds = DEFAULT_TTL_SECONDS): string {
  return [
    `${SESSION_COOKIE_NAME}=${token}`,
    'Path=/',
    'HttpOnly',
    'Secure',
    'SameSite=Lax',
    `Max-Age=${maxAgeSeconds}`,
  ].join('; ');
}

export function clearSessionCookie(): string {
  return `${SESSION_COOKIE_NAME}=; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0`;
}

/**
 * Verify the `Authorization: Bearer <AGENT_SECRET_KEY>` header.
 *
 * The comparison is constant-time so a wrong key cannot be recovered by timing.
 */
export function assertAgentSecret(header: string | undefined, expected: string): void {
  if (!expected) throw ApiError.internal('Server misconfiguration: AGENT_SECRET_KEY is not set');
  if (!header) throw ApiError.unauthorized('Missing Authorization header');

  const match = /^Bearer\s+(.+)$/i.exec(header.trim());
  const provided = match?.[1];
  if (!provided) throw ApiError.unauthorized('Authorization header must use the Bearer scheme');
  if (!timingSafeEqual(provided, expected)) throw ApiError.unauthorized('Invalid agent secret');
}

async function hmac(data: string, secret: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const sig = await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(data));
  return base64UrlEncode(new Uint8Array(sig));
}

function base64UrlEncode(bytes: Uint8Array): string {
  let binary = '';
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64UrlDecode(value: string): Uint8Array {
  const padded = value.replace(/-/g, '+').replace(/_/g, '/');
  const binary = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
