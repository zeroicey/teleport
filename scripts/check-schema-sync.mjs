#!/usr/bin/env node
/**
 * Guard: `schema.sql` and `migrations/0001_init.sql` must not drift.
 *
 * Compares both files with comments/blank lines stripped and whitespace
 * normalized, so headers and comments may differ but the actual DDL may not.
 *
 * Usage: node scripts/check-schema-sync.mjs
 */
import { readFileSync } from 'node:fs';

const MIGRATION = 'migrations/0001_init.sql';
const SNAPSHOT = 'schema.sql';

const normalize = (sql) =>
  sql
    .split('\n')
    .map((line) => line.replace(/--.*$/, '').trim())
    .filter((line) => line.length > 0)
    .join('\n')
    .replace(/\s+/g, ' ')
    .trim();

const migration = normalize(readFileSync(MIGRATION, 'utf8'));
const snapshot = normalize(readFileSync(SNAPSHOT, 'utf8'));

if (migration !== snapshot) {
  console.error(`✖ Schema drift detected between ${MIGRATION} and ${SNAPSHOT}`);
  console.error('  Update both files so the DDL stays identical.');
  process.exit(1);
}

console.log(`✔ ${MIGRATION} and ${SNAPSHOT} are in sync`);
