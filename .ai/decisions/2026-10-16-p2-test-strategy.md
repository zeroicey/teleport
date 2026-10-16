# P2 test strategy: one throwaway schema per test, not one shared database

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户（"B：每测一个 schema（推荐）"）
· **Supersedes:** none
· **Related:** `2026-10-16-rewrite-on-gin-and-postgres.md` (P2), `.ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md`

## Context

P2 rewrites `internal/store/` from `database/sql` + SQLite to `pgx` + PostgreSQL. Today the
247 tests run against a **temp-file SQLite database per test** (`newTestStore` →
`t.TempDir()`), which gives isolation, no shared state and no cleanup problem. PostgreSQL has
no in-process equivalent, so the harness has to change, and the rewrite's own迭代速度 depends
on that harness being fast and trustworthy. The handoff therefore requires this decision to be
written down **before** P2 starts.

Two facts narrow the choice:

1. **A long-lived test database has already lied to us once.** `teleport_test` was created
   from an uncommitted intermediate revision of the merged baseline, so the committed file
   and the live schema disagreed while every file-to-file drift guard stayed green
   (`TestPGMetadataCheckRejectsInvalidJSON`: expected SQLSTATE 23514, got 22P02). The DB had
   to be dropped and recreated by hand. Any strategy that keeps a long-lived test database
   keeps that failure mode.
2. **The three places that must be explicitly locked** (pending cap, active-key cap,
   share-token counting) are exactly the places where SQLite's single-writer guarantee was
   doing the work for free. Their tests are **concurrent**, and they need a database where a
   stray row from a sibling test cannot change the count.

Measured cost of a fresh schema + full baseline migration in `teleport_test`
(`scripts`-level, via `docker exec psql`, i.e. an **upper bound** — in-process pgx removes the
~100ms container startup): **10 runs in 1.103s ≈ 110ms each**; the DDL alone is one `Exec` of
a ~12KB file.

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| **A. Shared test DB + `TRUNCATE` per test** (what `testPGPool` does today, and what clip does) | Least new code; one migration per whole run; already proven in this repo | No `t.Parallel`; a long-lived DB can drift from the files (pitfall above); concurrency tests share the table they are counting in, so a leaked row changes an outcome; cleanup ordering becomes load-bearing | easy to change later, but every P2 iteration pays the serialisation |
| **B. One schema per test, created from the embedded migrations, dropped at cleanup** | Isolation **by construction** — a schema created and dropped inside one test cannot drift; `t.Parallel`-safe; each concurrency test gets a private `agent_keys` to count; no cross-test ordering | A migration run per test (~110ms upper bound, expected single-digit ms in-process); ~30 lines of harness; needs a role with `CREATE` on the test DB (the `teleport` role owns `teleport_test`, so this holds) | easy — the harness is localised to `_test.go` files |
| **C. testcontainers: a fresh PostgreSQL per `go test` run** | Cleanest possible isolation; no shared cluster at all | Heavy (container per run); needs image pulls, which are slow/flaky from the mainland for exactly the reason this project cares about; the gate would become unrunnable on a machine with no container runtime | hard — every test helper changes |

## Decision

**Option B.** Every test that needs a database builds its own schema from the embedded
migrations and drops it at cleanup. With these rules:

- The DSN env var stays `TEST_PG_DSN`, and the existing guard is **kept verbatim**: the
  database name must contain `test`, or the tests refuse to run at all. These tests create and
  drop schemas in whatever database they are pointed at; a mistyped DSN must not be able to
  destroy real reports.
- A per-test schema is named `t_<random>`, created with `CREATE SCHEMA`, and dropped with
  `DROP SCHEMA ... CASCADE` in `t.Cleanup`.
- The pool is pointed at it with `search_path` as a **connection runtime parameter**
  (`?search_path=t_<random>`), which pgx sends on every connection startup — verified against
  `pgconn`'s config parsing, which puts every unrecognised query parameter into
  `RuntimeParams`. Nothing in the store's SQL is schema-qualified, so this is transparent.
- The embedded migrations are the only source of the schema: the harness calls `MigratePG`
  against the fresh schema, never a checked-in dump.

The gate is **two-tier**, as decided alongside:

- `scripts/check.sh` may skip the Postgres-backed tests when `TEST_PG_DSN` is unset, so the
  gate still runs on a machine with no database.
- `scripts/test-pg.sh` **refuses to run** (non-zero exit) when `TEST_PG_DSN` is unset, and is
  the required step before deploying anything that touches storage.

Rationale for accepting B over the status quo: the option's cost is bounded and measured, and
its benefit is the removal of a failure mode that has already produced a false claim of
verification once.

## Consequences

- **Easier:** every P2 iteration is a normal `go test` run; concurrency tests become
  trustworthy because they count in a private schema; the stale-DB pitfall stops being
  possible rather than being guarded against; `t.Parallel` is available if the suite grows.
- **Harder:** the existing 8 `pg_test.go` tests are refactored from `testPGPool` (TRUNCATE the
  shared DB) to the per-test-schema harness — they must keep asserting the same things.
  `TestPGMigrationsApplyAndAreIdempotent` in particular now asserts re-application inside a
  private schema.
- **New invariant:** the role in `TEST_PG_DSN` must be able to `CREATE SCHEMA` in the test
  database. For `teleport_test` it is the owner, so this is already true.
- **New invariant:** connection budgets are small on purpose (1 admin, 4 per test) because
  the instance's `max_connections` of 100 is shared with clip, registry and serenique. A test
  that needs an empty database twice truncates; it does not open another pool. Measured
  peak across a full `internal/store` run after the fix: 14 total connections, ~5 of them
  teleport's.
- **Gate change:** `scripts/check.sh` reports loudly when it skips the Postgres tests, and
  `scripts/test-pg.sh` is the two-tier gate from the decision above — it refuses to run
  without a DSN, and self-checks that the harness still refuses a non-test database.
- The three locking sites get their concurrency tests written against the **real** new store
  first, so the lock is introduced to satisfy a failing invariant rather than on faith.
  (Done early, in `cap_lock_pg_test.go` — see below.)

## Evidence recorded for P2.3 (the store port)

Written after the decision, before the store rewrite, so that the locking work is driven by a
measured hazard rather than by a prediction. `cap_lock_pg_test.go`:

- **The hazard is real, and proven deterministically.** Two overlapping transactions, each
  `SELECT COUNT(*)` of pending applications (both observe 0 against a cap of 1), each then
  inserting: **two rows land past a cap of 1**, and nothing reports an error. The
  interleaving is forced rather than raced, because a statistical test would "pass" on the
  rounds where the race did not fire and read as "no problem".
- **A statistical run confirms the magnitude and the flakiness**: 24 racers for a cap of 3,
  20 rounds — most rounds landed on exactly 3, while rounds 0, 5 and 8 landed on 4, 5 and 4.
  A cap that holds 15 times out of 20 is exactly the "looks like it is limiting, actually
  is not" outcome this decision is about.
- **`pg_advisory_xact_lock` removes it.** The second transaction demonstrably blocks while
  the first holds the key, then observes the first one's insert and refuses; and 24 racers ×
  5 rounds against a cap of 3 yields exactly 3 winners every round, with zero callers getting
  a raw error instead of a clean refusal.
- `SELECT ... FOR UPDATE` was rejected: it cannot lock rows that do not exist yet, so it does
  not constrain a count over a set that new rows can join (a phantom).

**Correction to the handoff's list of sites.** The handoff names three: pending cap, key cap,
"share-link counting". The actual sites, verified by grepping every `COUNT(*)` in
`internal/store`, are: (1) `CreateApplication`'s `KEY_MAX_PENDING`; (2) `KEY_MAX_ACTIVE`,
shared by `ClaimApplication` **and** `CreateManualKey`, so both must take the same key;
(3) `CreateRenewal`'s "at most one pending renewal per key". There is **no share-link
quota** anywhere — `RecordView` is a single `view_count = view_count + 1`, which is already
atomic in PostgreSQL — so a search for one wastes time.

## Revisit when

- A per-test migration run becomes a measurable fraction of the gate's wall time (say the
  suite spends more than ~5s creating schemas) — then a connection-local template database
  (`CREATE DATABASE ... TEMPLATE`) or a migrated-then-cloned schema replaces it.
- PostgreSQL 16 gains a usable in-process testing story.
