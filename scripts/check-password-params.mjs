#!/usr/bin/env node
/**
 * Guard: the PBKDF2 iteration count must agree everywhere it appears, and must
 * stay within the Cloudflare Workers limit.
 *
 * WHY THIS EXISTS
 *   Cloudflare's runtime rejects PBKDF2 with more than 100,000 iterations:
 *
 *     NotSupportedError: Pbkdf2 failed: iteration counts above 100000 are not
 *     supported (requested 210000).
 *
 *   `wrangler dev` does NOT enforce this limit, so a too-high count passes all
 *   local testing and then makes production login return an opaque HTTP 500.
 *   This exact bug shipped once; this check exists so it cannot ship again.
 *
 * Checks:
 *   1. src/services/password.ts DEFAULT_ITERATIONS <= 100,000
 *   2. scripts/hash-password.mjs ITERATIONS matches it
 *      (a mismatch means a generated hash cannot be verified by the Worker)
 *
 * Usage: node scripts/check-password-params.mjs
 */
import { readFileSync } from 'node:fs';

const WORKERS_MAX_ITERATIONS = 100_000;
const SERVICE = 'src/services/password.ts';
const SCRIPT = 'scripts/hash-password.mjs';

function readIterations(path, label) {
  const source = readFileSync(path, 'utf8');
  // Match `const NAME = 100_000` / `const DEFAULT_ITERATIONS = 100_000;`
  const m = /const\s+(?:DEFAULT_)?ITERATIONS\s*=\s*([\d_]+)/.exec(source);
  if (!m) {
    console.error(`✖ Could not find an ITERATIONS constant in ${path}`);
    console.error(`  ${label} must declare one so this check can verify it.`);
    process.exit(1);
  }
  return Number(m[1].replace(/_/g, ''));
}

const serviceIterations = readIterations(SERVICE, 'The Worker verifier');
const scriptIterations = readIterations(SCRIPT, 'The hash generator');

let failed = false;

if (serviceIterations > WORKERS_MAX_ITERATIONS) {
  console.error(
    `✖ ${SERVICE} uses ${serviceIterations} PBKDF2 iterations, above the ` +
      `Cloudflare Workers limit of ${WORKERS_MAX_ITERATIONS}.`,
  );
  console.error('  Production login will fail with an opaque HTTP 500.');
  failed = true;
}

if (scriptIterations > WORKERS_MAX_ITERATIONS) {
  console.error(
    `✖ ${SCRIPT} generates ${scriptIterations} iterations, above the ` +
      `Cloudflare Workers limit of ${WORKERS_MAX_ITERATIONS}.`,
  );
  failed = true;
}

if (serviceIterations !== scriptIterations) {
  console.error(
    `✖ Iteration mismatch: ${SERVICE} uses ${serviceIterations} but ` +
      `${SCRIPT} generates ${scriptIterations}.`,
  );
  console.error('  Hashes produced by the script would not verify in the Worker.');
  failed = true;
}

if (failed) process.exit(1);

console.log(
  `✔ PBKDF2 iterations consistent at ${serviceIterations} ` +
    `(Workers limit ${WORKERS_MAX_ITERATIONS})`,
);
