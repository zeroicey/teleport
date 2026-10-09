#!/usr/bin/env node
/**
 * Guard: `schema.sql` and the migrations embedded in the Go backend must not drift.
 *
 * The backend embeds its migrations (`internal/store/migrations/*.sql`) into the
 * compiled binary, so that copy is the one that actually runs. `schema.sql` at
 * the repo root is the readable snapshot for humans and for running against a
 * bare `sqlite3` shell. If the two diverge, the snapshot lies about what the
 * service creates — hence this check.
 *
 * Compares the concatenation of *every* migration, in filename order (the same
 * order the Go migrator applies them), against the snapshot, with comments and
 * blank lines stripped and whitespace normalized. Headers and comments may
 * differ; the actual DDL may not.
 *
 * NOTE: this used to name `0001_init.sql` explicitly. That was correct while
 * there was one migration and became a silent hole the moment a second one was
 * added: a new migration whose DDL was never added to `schema.sql` would still
 * pass, because nobody was comparing it. Reading the directory is what keeps the
 * guard honest as the schema grows.
 *
 * Usage: node scripts/check-schema-sync.mjs
 */
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const MIGRATIONS_DIR = 'backend/internal/store/migrations';
const SNAPSHOT = 'schema.sql';

const normalize = (sql) =>
  sql
    .split('\n')
    .map((line) => line.replace(/--.*$/, '').trim())
    .filter((line) => line.length > 0)
    .join('\n')
    .replace(/\s+/g, ' ')
    .trim();

let migrationFiles;
try {
  migrationFiles = readdirSync(MIGRATIONS_DIR)
    .filter((name) => name.endsWith('.sql'))
    .sort();
} catch (error) {
  console.error(`✖ Could not read ${MIGRATIONS_DIR}: ${error.message}`);
  process.exit(1);
}

if (migrationFiles.length === 0) {
  console.error(`✖ No migrations found in ${MIGRATIONS_DIR}`);
  process.exit(1);
}

let migration;
let snapshot;
try {
  migration = normalize(
    migrationFiles.map((name) => readFileSync(join(MIGRATIONS_DIR, name), 'utf8')).join('\n'),
  );
  snapshot = normalize(readFileSync(SNAPSHOT, 'utf8'));
} catch (error) {
  console.error(`✖ Could not read a schema file: ${error.message}`);
  process.exit(1);
}

if (migration !== snapshot) {
  console.error(`✖ Schema drift detected between ${MIGRATIONS_DIR}/ and ${SNAPSHOT}`);
  console.error(`  Compared ${migrationFiles.length} migration(s): ${migrationFiles.join(', ')}`);
  console.error('  Update both files so the DDL stays identical.');
  process.exit(1);
}

console.log(
  `✔ ${migrationFiles.length} migration(s) (${migrationFiles.join(', ')}) are in sync with ${SNAPSHOT}`,
);
