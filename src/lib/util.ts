/**
 * Small, dependency-free helpers shared across the Worker.
 */

/** RFC 4122 v4 UUID from the platform CSPRNG. */
export function uuid(): string {
  return crypto.randomUUID();
}

/**
 * High-entropy, URL-safe share token.
 *
 * 16 random bytes -> 22 base64url chars (~128 bits). That is far beyond
 * brute-force range and still short enough to paste into a chat message.
 * `crypto.getRandomValues` is the platform CSPRNG, never `Math.random`.
 */
export function generateShareToken(bytes = 16): string {
  const buf = new Uint8Array(bytes);
  crypto.getRandomValues(buf);
  return base64UrlEncode(buf);
}

function base64UrlEncode(bytes: Uint8Array): string {
  let binary = '';
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** Current time in epoch milliseconds (matches the D1 column convention). */
export function nowMs(): number {
  return Date.now();
}

/**
 * Convert a duration in hours to an absolute expiry timestamp.
 *
 * Returns 0 ("never expires") for a falsy or non-finite hour count, which is
 * exactly what the `share_tokens.expires_at = 0` convention expects.
 */
export function hoursFromNow(hours: number | undefined | null, fallback = 0): number {
  const h = hours === undefined || hours === null || Number.isNaN(hours) ? fallback : hours;
  if (!Number.isFinite(h) || h <= 0) return 0;
  return Date.now() + Math.round(h * 60 * 60 * 1000);
}

/** True when an `expires_at` value (0 = never) is still in the future. */
export function isExpired(expiresAt: number, at: number = Date.now()): boolean {
  return expiresAt !== 0 && at >= expiresAt;
}

/** Best-effort JSON parse that degrades to a default instead of throwing. */
export function safeJsonParse<T>(value: unknown, fallback: T): T {
  if (typeof value !== 'string' || value.length === 0) return fallback;
  try {
    return JSON.parse(value) as T;
  } catch {
    return fallback;
  }
}

/** Traffic-safe constant-time-ish string comparison (avoids length leaks). */
export function timingSafeEqual(a: string, b: string): boolean {
  const enc = new TextEncoder();
  const ab = enc.encode(a);
  const bb = enc.encode(b);
  // Fold the length difference into the accumulator rather than early-returning.
  let diff = ab.length ^ bb.length;
  const len = Math.max(ab.length, bb.length);
  for (let i = 0; i < len; i++) {
    diff |= (ab[i] ?? 0) ^ (bb[i] ?? 0);
  }
  return diff === 0;
}
