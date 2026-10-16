# A renewal granted the key's extension twice, because the status read was not locked

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

No error, no log line. Two concurrent `DecideRenewal` calls on the *same* renewal
request both succeeded, and the key's `expires_at` moved twice:

```text
both transactions read status=pending (the plain SELECT takes no lock)
tx1 committed: grant=1700176400000, decide rows=1
tx2 decide rows=0 (the guard DID fire), grant=1700349200000, base=1700176400000
key expires_at=1700349200000, want 1700176400000 — two decisions both extended the key
```

One renewal request bought two extensions. The second decider's guarded `UPDATE`
matched zero rows — and the transaction was committed anyway, so the extension it
had already written stayed.

## Root cause

`DecideRenewal` is a **read-then-write** decision, not a single conditional
statement:

1. `SELECT key_id, status FROM key_renewals WHERE id = ?` — a plain read, which
   takes **no lock**. Both transactions therefore observe `pending`.
2. read the key, compute the grant, `UPDATE agent_keys SET expires_at = ?`.
3. `UPDATE key_renewals SET status = ... WHERE id = ? AND status = 'pending'` —
   guarded, so exactly one matches.
4. commit.

Under SQLite with `_txlock=immediate` this was safe for free: the transaction took
the write lock when it **began**, so step 1 of the second decider could not run
until the first had committed. PostgreSQL grants nothing of the kind, and P2.3's
plan only named the three **count-then-insert** sites (the caps) as needing locks.
Step 3's guard was mistaken for the safety property — but it fires *after* step 2,
so it can only refuse the decision, not undo the extension.

Measured detection by the store's own tests before the fix: an 8-racer
`DecideRenewal` test passed **5 runs out of 5**. The window needs two transactions
to both read before either commits, and with a tiny critical section that is rare.
A statistical test would have shipped this.

## Fix

Two changes in `backend/internal/store`:

1. The status read is `SELECT ... FOR UPDATE` (`queries/keys.sql`,
   `LockRenewalForDecision`). The row already exists, so a row lock is the right
   tool — unlike the caps, where the rows that would violate the limit do not exist
   yet and `FOR UPDATE` cannot help. The loser now stops before doing any work.
2. The guarded `UPDATE`'s row count is checked, and a zero means the transaction is
   discarded rather than committed. This is the mechanism that actually prevents the
   leak; the `FOR UPDATE` is the early-out. (Verified both ways: removing the
   `FOR UPDATE` still refuses the decision, but only after the wasted extension —
   which is why both are kept.)

## Guard

- `TestDecideRenewalRefusedDecisionLeavesNoExtension` — the property: with the
  renewal row held by another transaction and then flipped to `approved`, the losing
  decision must return `ErrApplicationDecided` **and leave the key's expiry
  unchanged**. The pre-fix code fails this on every run, because with no row lock its
  status read is never blocked, so it always reads `pending` before the flip.
- `TestDecideRenewalTakesTheRowLockBeforeDoingAnyWork` — the mechanism: while the row
  is held, the decision must not have written the key yet. Detected by probing
  `agent_keys` with `FOR UPDATE NOWAIT` from a third connection: an unwritten row is
  unlocked, a written-but-uncommitted one is locked.
- `TestConcurrentRenewalDecisionsGrantExactlyOnce` — the realistic shape, plus the
  decisive assertion that the final expiry reflects **one** grant. Deliberately not
  the primary guard: it is flaky against the broken code.

**The naive version of the row-lock guard passed against a mutant with `FOR UPDATE`
removed** — it asserted only that the call blocked, and the call still blocked, just
later, on the same row. A guard must be mutation-tested; "it blocked somewhere" is
not a property.

## The generalisable rule

SQLite's single-writer guarantee was silently protecting **every** read-then-write
decision in this store, not just the ones that look like counters. When porting off
it, enumerate decisions by their *shape* — "read something, then write based on it"
— not by whether they contain `COUNT(*)`. The three caps were found because they
count; this one was missed because it reads a status.
