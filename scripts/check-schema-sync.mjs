#!/usr/bin/env node
/**
 * Guard: `schema.sql` and the migration embedded in the Go backend must not drift.
 *
 * The backend embeds its migrations (`internal/store/migrations/*.sql`) into the
 * compiled binary, so that copy is the one that actually runs. `schema.sql` at
 * the repo root is the readable snapshot for humans and for running against a
 * bare `sqlite3` shell. If the two diverge, the snapshot lies about what the
 * service creates — hence this check.
 *
 * Compares both files with comments/blank lines stripped and whitespace
 * normalized, so headers and comments may differ but the actual DDL may not.
 *
 * Usage: node scripts/check-schema-sync.mjs
 */
import { readFileSync } from 'node:fs';

const MIGRATION = 'backend/internal/store/migrations/0001_init.sql';
const SNAPSHOT = 'schema.sql';

const normalize = (sql) =>
  sql
    .split('\n')
    .map((line) => line.replace(/--.*$/, '').trim())
    .filter((line) => line.length > 0)
    .join('\n')
    .replace(/\s+/g, ' ')
    .trim();

let migration;
let snapshot;
try {
  migration = normalize(readFileSync(MIGRATION, 'utf8'));
  snapshot = normalize(readFileSync(SNAPSHOT, 'utf8'));
} catch (error) {
  console.error(`✖ Could not read a schema file: ${error.message}`);
  process.exit(1);
}

if (migration !== snapshot) {
  console.error(`✖ Schema drift detected between ${MIGRATION} and ${SNAPSHOT}`);
  console.error('  Update both files so the DDL stays identical.');
  process.exit(1);
}

console.log(`✔ ${MIGRATION} and ${SNAPSHOT} are in sync`);
