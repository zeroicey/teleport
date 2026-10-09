/**
 * Password hashing for the dashboard login (PBKDF2-HMAC-SHA256 via WebCrypto).
 *
 * Stored format (single line, safe in a `.dev.vars` file):
 *
 *     pbkdf2$<iterations>$<base64url salt>$<base64url derivedKey>
 *
 * Generate one with:  node scripts/hash-password.mjs 'your password'
 */
import { timingSafeEqual } from '../lib/util';
import { ApiError } from '../lib/errors';

const ALGORITHM = 'PBKDF2';
const HASH = 'SHA-256';

/**
 * PBKDF2 iteration count.
 *
 * HARD CEILING: Cloudflare's Workers runtime rejects PBKDF2 with more than
 * 100,000 iterations — `crypto.subtle.deriveBits` throws `NotSupportedError`
 * ("Pbkdf2 failed: iteration counts above 100000 are not supported").
 *
 * This is a production-only failure: `wrangler dev` (a local workerd) accepts
 * a higher count, so an over-limit value passes every local test and then
 * returns 500 on the live deployment. Earlier revisions used 210,000, which is
 * a reasonable OWASP-style figure but simply cannot run on Workers.
 *
 * Keep this at or below 100_000, and keep scripts/hash-password.mjs in sync —
 * a mismatch here is what breaks dashboard login.
 */
export const DEFAULT_ITERATIONS = 100_000;
const KEY_BITS = 256;
const SALT_BYTES = 16;

export async function hashPassword(
  password: string,
  iterations = DEFAULT_ITERATIONS,
): Promise<string> {
  const salt = new Uint8Array(SALT_BYTES);
  crypto.getRandomValues(salt);
  const key = await derive(password, salt, iterations);
  return [
    'pbkdf2',
    String(iterations),
    base64UrlEncode(salt),
    base64UrlEncode(new Uint8Array(key)),
  ].join('$');
}

/**
 * Cloudflare refuses PBKDF2 above this many iterations, so anything higher is
 * a configuration bug rather than a password mismatch. Detect it explicitly
 * instead of letting an opaque 500 escape.
 */
const MAX_WORKERS_ITERATIONS = 100_000;

/**
 * Verify a password against a stored hash.
 *
 * Returns false (never throws) for malformed stored hashes so a corrupt config
 * cannot be distinguished from a wrong password by an attacker.
 */
export async function verifyPassword(
  password: string,
  stored: string,
): Promise<boolean> {
  if (!stored) return false;

  const parts = stored.split('$');
  if (parts.length !== 4 || parts[0] !== 'pbkdf2') return false;

  const iterationsPart = parts[1];
  const saltPart = parts[2];
  const hashPart = parts[3];
  if (!iterationsPart || !saltPart || !hashPart) return false;

  const iterations = Number.parseInt(iterationsPart, 10);
  if (!Number.isFinite(iterations) || iterations <= 0) return false;

  // Fail loudly and specifically: a hash generated with a higher count works in
  // `wrangler dev` but throws in production, which is otherwise very hard to
  // diagnose (it surfaces as an indistinguishable HTTP 500).
  if (iterations > MAX_WORKERS_ITERATIONS) {
    throw ApiError.internal(
      `Server misconfiguration: ADMIN_PASSWORD_HASH uses ${iterations} PBKDF2 ` +
        `iterations, but the Workers runtime supports at most ${MAX_WORKERS_ITERATIONS}. ` +
        `Regenerate it with scripts/hash-password.mjs.`,
    );
  }

  let salt: Uint8Array;
  let expected: Uint8Array;
  try {
    salt = base64UrlDecode(saltPart);
    expected = base64UrlDecode(hashPart);
  } catch {
    return false;
  }

  const actual = new Uint8Array(await derive(password, salt, iterations));
  if (actual.length !== expected.length) return false;

  return timingSafeEqual(bytesToLatin1(actual), bytesToLatin1(expected));
}

/** Guard used by the login route to fail fast on missing configuration. */
export function assertPasswordHashConfigured(stored: string | undefined): asserts stored is string {
  if (!stored) {
    throw ApiError.internal('Server misconfiguration: ADMIN_PASSWORD_HASH is not set');
  }
}

async function derive(
  password: string,
  salt: Uint8Array,
  iterations: number,
): Promise<ArrayBuffer> {
  const keyMaterial = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(password),
    ALGORITHM,
    false,
    ['deriveBits'],
  );
  return crypto.subtle.deriveBits(
    { name: ALGORITHM, salt: salt as unknown as BufferSource, iterations, hash: HASH },
    keyMaterial,
    KEY_BITS,
  );
}

function bytesToLatin1(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += String.fromCharCode(b);
  return out;
}

function base64UrlEncode(bytes: Uint8Array): string {
  return btoa(bytesToLatin1(bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64UrlDecode(value: string): Uint8Array {
  const padded = value.replace(/-/g, '+').replace(/_/g, '/');
  const binary = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
