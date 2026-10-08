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
export const DEFAULT_ITERATIONS = 210_000;
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
