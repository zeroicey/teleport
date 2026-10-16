# A store method that opens a second connection while its transaction is still open deadlocks a small pool

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

`go test ./internal/store/` hung and never finished. `-timeout 120s` produced a goroutine
dump in which every goroutine was parked in one of two places:

```text
goroutine 323 [select]:
golang.org/x/sync/semaphore.(*Weighted).Acquire(...)
github.com/jackc/puddle/v2.(*Pool[...]).Acquire(...)
github.com/jackc/pgx/v5/pgxpool.(*Pool).Acquire(...)
github.com/jackc/pgx/v5/pgxpool.(*Pool).QueryRow(...)
github.com/zeroicey/teleport/backend/internal/store/storedb.(*Queries).GetApplication(...)
github.com/zeroicey/teleport/backend/internal/store.(*Store).GetApplication(...)
github.com/zeroicey/teleport/backend/internal/store.(*Store).diagnoseClaim(...)
github.com/zeroicey/teleport/backend/internal/store.(*Store).ClaimApplication(...)
github.com/zeroicey/teleport/backend/internal/store.TestConcurrentClaimHasExactlyOneWinner.func1()
```

No error, no timeout, no log line. The suite simply stopped.

## Root cause

`ClaimApplication` opened a transaction, lost the conditional `UPDATE`, and then called
`s.diagnoseClaim(id, nowMS)` — which called `s.GetApplication(id)`, which went through
`s.q` (bound to the **pool**) instead of the transaction-bound query set `q`.

That asks the pool for a **second** connection while the first one is still held by the
open transaction. The pool is deliberately tiny (`storetest.Pool` uses `MaxConns=4`, per
`.ai/pitfalls/cases/2026-10-16-test-harness-exhausted-shared-postgres-connections.md`), and
the test runs 8 concurrent claimers. Each loser held its transaction's connection and
waited for a 9th connection that could never exist, because the 8 holders were all waiting
for it. Classic hold-and-wait with no timeout: pgxpool's `Acquire` blocks indefinitely.

The bug was invisible under SQLite for two reasons that are worth naming:

1. `database/sql`'s SQLite pool in the old implementation had 8 connections and, more
   importantly, no transaction *held* a connection while a second query ran — the same
   `*sql.DB` served both, so there was nothing to deadlock on.
2. The ported code *looked* correct: `diagnoseClaim` was a plain read, and a read that goes
   through the pool is the normal thing to write. The defect is not in the read, it is in
   **where it is called from**.

A second, quieter wrongness hid inside the same call: `diagnoseClaim` was documented as
reading "outside the failed transaction on purpose", on the theory that a rolled-back
transaction has nothing to say about why it failed. That reasoning is backwards. The
conditional `UPDATE` returning zero rows can only happen *after* a competing writer
committed, so a fresh statement inside the same transaction observes the winning state
under READ COMMITTED — which is exactly the state being diagnosed.

## Fix

`diagnoseClaim` now takes the transaction's query set and reads through it:

```go
if n == 0 {
    return nil, "", diagnoseClaim(q, id, nowMS)
}
...
func diagnoseClaim(q *storedb.Queries, id string, nowMS int64) error
```

With `q` bound to the `pgx.Tx`, no second connection is requested, so there is nothing to
wait for. The comment at the function now states the rule instead of the reverse of it.

## Guard

The concurrency tests that found it stay as the guard, because they are the only thing that
exercises "many losers, one winner, tiny pool" at once:

- `TestConcurrentClaimHasExactlyOneWinner` (8 racers, same application)
- `TestConcurrentClaimsRespectActiveCap` (6 racers, `KEY_MAX_ACTIVE=1`)
- `TestConcurrentApplicationsRespectPendingCap` (24 racers, `KEY_MAX_PENDING=3`)

The general rule to check on any new store method: **inside a transaction, only ever use the
`WithTx` query set.** Grep-able form — for every method that calls `s.pool.Begin`, there must
be no use of `s.q.` or `s.pool.` after that line. Worth re-checking whenever a method grows a
helper call.

TODO (not done, deliberately): a per-call timeout on the store's context would convert this
class of hang from "forever" into "an error after N seconds". It is deferred because the
store's method signatures carry no `context.Context` (frozen for the port — see
`2026-10-16-p2-store-port-technique.md`), so the timeout has nowhere to come from until P3
threads a request context through. Until then, a nested acquire is an unbounded hang, which
is why the grep above is the guard rather than a timeout.
