# P2.3: port `internal/store` to pgx behind an interface, and run the same suite against both

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户（"B：一次性大爆炸重写"）
· **Supersedes:** none
· **Related:** `2026-10-16-rewrite-on-gin-and-postgres.md` (P2), `2026-10-16-p2-test-strategy.md`

## Context

P2's completion condition is "`internal/store` tests all green against a real database". The
hard part is not writing pgx calls; it is **not losing properties the SQLite implementation
already proves**. This project's own history is the evidence: P0 found the most dangerous
contract (the three agent-key derivation labels) had *zero* test coverage, and P1's
verification claim turned out to rest on a database that had drifted.

The current shape:

- `Store` is one concrete type holding `*sql.DB`; `queries.go` and `keys.go` add ~40 methods.
- `internal/api` holds a `*store.Store` concrete value and calls those methods.
- The store tests are `package store` (internal) and some reach into `s.db` directly. They run
  on a temp-file SQLite database per test.
- 14 frozen contracts have to survive this port byte-for-byte.

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| **A. Introduce a `Store` interface, port to a second implementation (`PGStore`), and drive the *existing* test bodies against both until the PostgreSQL one is green — then delete SQLite** | Every assertion is executed against both implementations, so "the new one is green" means the same thing the old one meant. A lost property fails on the PG side immediately, in the test that has always guarded it. The interface also documents the frozen surface. | The largest test refactor of the three: test bodies must be expressed against the interface, and the handful that touch `s.db` need a raw-handle hook. Both implementations live in the tree for the duration. | Easy to abandon: if it stalls, the SQLite implementation is untouched and still green. |
| **B. Big-bang: rewrite `queries.go`/`keys.go` to pgx in one pass, adapt the tests, then fix what breaks** | Fewest moving parts; one reviewable commit. | The moment of truth is "everything at once": a broken property and a broken port are indistinguishable, and the failures arrive in bulk with no reference implementation to compare against. Directly contradicts the phase plan's "each stage must be able to run the old tests". | Hard — there is no halfway state. |
| **C. Port method-group by method-group, writing *new* PG tests as we go, keeping SQLite green until the end** | Small steps; the tree is green after every step. | The new tests are written by the same person making the port, so a shared misunderstanding of what the old code guaranteed is baked into both. This is the failure mode the whole P0/P1 effort exists to prevent. | Easy. |

## Decision

**Option B** — rewrite `queries.go` and `keys.go` to pgx in one change and adapt the tests,
as decided by the user. Option A was recommended and not chosen; the reasoning is below so
that the trade-off is not silently reversed later.

**Why A was recommended:** it is the only option where every existing assertion is executed
against a second implementation, so "the PostgreSQL port is green" means the same thing
"the SQLite implementation is green" used to mean. This project has already paid twice for
assuming that — P0 found the most dangerous frozen contract had no test at all, and P1's
verification claim turned out to rest on a database that had drifted.

**Why B was chosen:** it is one reviewable change with no period during which two
implementations must both be understood, and the SQLite implementation is being deleted
anyway — building an interface purely to delete it days later is work with no surviving
artifact.

### Mitigations required by this decision

B removes the reference implementation, not the evidence. These are not optional, because
they are the only things standing where Option A's cross-check would have stood:

1. **The test bodies are frozen, not rewritten.** The existing assertions are the
   specification. The fixture changes (temp-file SQLite → per-test schema); assertions do
   not. A failing assertion is investigated individually — never bulk-edited to match the
   port's output. If an assertion must change, the commit message must say which frozen
   contract it belongs to and why the assertion was wrong.
2. **Frozen-contract tests are the gate, not the regression suite.**
   `internal/api/frozen_contract_test.go` and `internal/agentkey/frozen_contract_test.go`
   must be green against the ported store before anything else is trusted.
3. **The three cap sites get the validated advisory locks**, since `cap_lock_pg_test.go`
   already proves the naive pattern is broken in PostgreSQL and that the lock fixes it. This
   is the one place where a silent behaviour change is *known* to be waiting.
4. **`SELECT ... FOR UPDATE` is not an acceptable substitute**: it cannot lock rows that do
   not exist, so it does not constrain a count that new rows can join.
5. **The ported code is not deployed on the strength of unit tests alone** — P4 (full chain +
   real browser) and P5 (row-count reconciliation) stay in the plan as the external checks.

## Consequences

- **Easier:** one reviewable change; no period in which two implementations must both be
  understood; nothing is built solely to be deleted.
- **Harder:** failures arrive in bulk and a broken property is initially indistinguishable
  from a mistyped dialect. The mitigations above are what has to carry that risk.
- **Explicitly lost:** the mechanical cross-check. "The port is green" now means "the frozen
  assertions pass against the port", not "the old and new implementations agree". If a
  property was never asserted, its loss will not be caught here — which is why P0 pinned the
  14 contracts first and why P4/P5 stay in the plan.
- Removes the SQLite driver, WAL and `_txlock=immediate` reasoning from the deployment.
  `database/sql` leaves `internal/store` entirely, and the two SQLite-only hazards
  (`sql.ErrNoRows` vs `pgx.ErrNoRows`, `_txlock=immediate`) become historical notes.
- The `schema_migrations` drift problem from 2026-10-16 is fixed at the source rather than
  guarded after the fact: `MigratePG` will record a content hash of each applied migration
  and refuse to start when a recorded migration's body has changed.

## Revisit when

- The bulk-failure risk materialises: if the port cannot be brought to green, the fallback is
  to reinstate the cross-check selectively — port *one* method group with both implementations
  present (Option A applied locally) rather than continuing to debug blind.
- `sqlc` turns out unable to express a query the store needs (the candidate is the two
  dynamic `SET`-list updates); PostgreSQL `COALESCE($n::type, column)` should express them as
  single static statements, and if it cannot, the fallback is hand-written pgx for those two
  methods with a note saying why.
