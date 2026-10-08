#!/usr/bin/env node
/**
 * Generate an ADMIN_PASSWORD_HASH value for wrangler secrets / .dev.vars.
 *
 * Usage:
 *   node scripts/hash-password.mjs 'my super secret password'
 *   npx wrangler secret put ADMIN_PASSWORD_HASH   # then paste the output
 *
 * Must stay byte-compatible with src/services/password.ts
 * (PBKDF2-HMAC-SHA256, 210000 iterations, 16-byte salt, 256-bit key).
 */
import { webcrypto as crypto } from 'node:crypto';

const ITERATIONS = 210_000;
const KEY_BITS = 256;
const SALT_BYTES = 16;

const password = process.argv[2];
if (!password) {
  console.error("Usage: node scripts/hash-password.mjs '<password>'");
  process.exit(1);
}

const salt = new Uint8Array(SALT_BYTES);
crypto.getRandomValues(salt);

const keyMaterial = await crypto.subtle.importKey(
  'raw',
  new TextEncoder().encode(password),
  'PBKDF2',
  false,
  ['deriveBits'],
);

const derived = new Uint8Array(
  await crypto.subtle.deriveBits(
    { name: 'PBKDF2', salt, iterations: ITERATIONS, hash: 'SHA-256' },
    keyMaterial,
    KEY_BITS,
  ),
);

const b64url = (bytes) =>
  Buffer.from(bytes).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

console.log(`pbkdf2$${ITERATIONS}$${b64url(salt)}$${b64url(derived)}`);
