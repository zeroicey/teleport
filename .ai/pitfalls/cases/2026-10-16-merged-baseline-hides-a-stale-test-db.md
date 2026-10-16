# A merged baseline migration hides a stale database, and the drift guards cannot see it

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

P1 was recorded as "✅ 8 个真库测试在真库上验证". A day later, running the same suite
against the same database:

```text
--- FAIL: TestPGMetadataCheckRejectsInvalidJSON (0.03s)
    pg_test.go:155: invalid JSON produced ERROR: invalid input syntax for type json
    (SQLSTATE 22P02) (SQLSTATE 22P02), want a CHECK violation 23514.
```

Both drift guards (`TestPGSnapshotMatchesMigrations`, the shell snapshot comparison in
`scripts/check.sh`) stayed **green** while this failed.

## Root cause

Three facts compose:

1. `migrations_pg/0001_init.sql` is a **merged baseline**: one file that creates the whole
   schema. `MigratePG` records it by **filename** in `schema_migrations` and never re-applies it.
2. The test database `teleport_test` is **long-lived**. It was created while the baseline was
   being authored, from an *uncommitted* intermediate revision whose CHECK read
   `(metadata)::json IS NOT NULL`.
3. The file was then corrected to `CHECK (metadata IS JSON)` before commit, and
   `CREATE TABLE` (no `IF NOT EXISTS`) would have applied that correctly — but only to a
   database that had never recorded `0001_init.sql`. `teleport_test` had.

So the live database kept DDL that **no commit ever contained**:

| | metadata CHECK |
| --- | --- |
| live `teleport_test` | `CHECK (((metadata)::json IS NOT NULL))` |
| committed migration | `CHECK (metadata IS JSON)` |

Both committed revisions (`ab229ec`, `a58ae2d`) already say `IS JSON`, which is the proof
that the drift came from the working tree, not from git.

**The guards could not catch it because they compare files to files.**
`schema.pg.sql` vs `migrations_pg/*.sql` is a pure text comparison; a live database is not a
participant. A green guard therefore means "the snapshot matches the migrations", never
"the database matches the migrations".

Note the failure mode was *inverted* from the usual one: the stale schema is the **stricter**
one (a cast raises 22P02 instead of rejecting the row), so nothing silently accepted bad data.
The damage is to the **claim of verification** — "verified against the real database" was
true of a database that no longer existed.

## Fix

Rebuild the test database from the committed files and re-verify:

```bash
docker exec -e PGPASSWORD="$PW" postgres psql -U postgres \
  -c "DROP DATABASE IF EXISTS teleport_test;" \
  -c "CREATE DATABASE teleport_test OWNER teleport;"
TEST_PG_DSN='postgres://teleport:$PW@127.0.0.1:5432/teleport_test?sslmode=disable' \
  go test ./internal/store/ -count=1 -run TestPG
# → 8/8 PASS
```

The production-intent database `teleport` was still **empty** (no `schema_migrations`, no
tables), so nothing in P5/P6 is affected — but the same edit would have poisoned it
permanently once it had been migrated.

## Guard

**Structural, not a new assertion:** the P2 test harness creates a **fresh schema per test
from the embedded migrations** (see `.ai/decisions/2026-10-16-p2-test-strategy.md`). A schema
that is created and dropped inside one test cannot drift from the files that created it, so
this failure mode stops existing rather than being detected.

Until that harness lands, the rule is: **after editing `migrations_pg/*.sql`, drop and
recreate every database that has already recorded it** — including `teleport_test`. Editing a
merged baseline is not a migration.

Deliberately *not* the guard: a test that compares the live `reports_metadata_check` text to
the migration. It would pin one constraint and miss the next one.
