# A test harness exhausted the shared PostgreSQL instance's connection slots

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

A scratch measurement test (20 rounds, each round calling `storetest.Pool` once)
died part-way through with:

```text
failed to connect to `user=teleport database=teleport_test`: 127.0.0.1:5432:
server error: FATAL: sorry, too many clients already (SQLSTATE 53300)
server error: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute
```

Round 10 recorded `wins=3 errs=16` — the test had stopped measuring the thing it was
measuring and started measuring connection starvation.

## Root cause

`storetest.Schema` opens **two** pools per call (a 1-connection admin pool plus a
4-connection test pool) and releases them with `t.Cleanup`.

`t.Cleanup` runs when the **test** ends — not when the loop iteration that created the
pool ends. So 20 iterations held 20 pools open simultaneously, and the instance's
`max_connections` is **100 shared with every other service on it**:

```text
$ select setting from pg_settings where name='max_connections';
100
```

That 100 is not teleport's budget. The same instance serves clip (`relay`), `registry`,
`serenique_dev` and others. The failure mode was therefore not "this test failed" — it was
**"this test denied connections to unrelated production services"**, which would have
looked like a clip outage rather than a test problem.

The tempting reading — "the harness is broken" — is wrong. The harness behaved exactly as
documented; the test misused it. That is precisely the kind of mistake a harness must make
hard, because the blast radius is outside the repository.

## Fix

1. **Small connection budgets**, chosen as small as the tests allow: 1 for the admin pool,
   4 for a test's own pool. Concurrency tests still oversubscribe (24 goroutines over 4
   connections), which is what makes them meaningful — the thing under test is whether the
   *database* serialises the racers, not whether the pool does.
2. **A test that needs an empty database twice truncates, it does not re-open.** See
   `TestPGAdvisoryLockedCapIsExactUnderContention`, which now runs its 5 rounds against one
   pool with `TRUNCATE key_applications CASCADE` between them.
3. The constraint and the reason are written down at `storetest.openPool`, where the next
   person will actually be when they add a loop.

Measured after the fix, sampling `pg_stat_activity` throughout a full `internal/store` run:
**peak 14 total connections** (baseline 9 from the other services), i.e. teleport used ~5.
The contention test also got ~19x faster (2.48s → 0.13s), because it stopped paying for
schema creation and pool setup on every round.

## Guard

No new assertion. The guard is the **connection budget itself**: with `MaxConns` at 1 and 4,
a runaway loop is throttled by `pool.Begin` waiting for a free connection instead of
exhausting the server. That converts a cross-service outage into local slowness, which is
the correct failure direction.

Deliberately *not* a guard: asserting `pg_stat_activity` stays under some number. It would
be racy against the other services and would fail for reasons that have nothing to do with
this repository.
