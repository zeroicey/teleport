package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/store/storetest"
)

// The tests in this file are the deterministic guards for the locks that
// replaced SQLite's single-writer guarantee.
//
// Why they exist, in the words of the review that produced them: the *statistical*
// concurrency tests (TestConcurrentApplicationsRespectPendingCap and friends) do
// exercise the caps, but they detect a removed lock only when the race happens to
// fire. Measured on a throwaway copy with lockCap calls deleted: the pending-cap
// test failed on some runs and passed on others, and the shared-lock property
// between CreateManualKey and ClaimApplication was not detected at all. A guard
// that reports "safe" when the mechanism is gone is the exact failure mode
// .ai/decisions/2026-10-16-p2-test-strategy.md was written to eliminate ("a cap
// that holds 15 times out of 20 looks like it is limiting, actually is not").
//
// So the tests here assert the *mechanism*, deterministically: while another
// transaction holds a lock, the store method that is supposed to take it must
// block. That cannot pass by luck. Remove the lock call from the store and the
// method returns immediately, and the "it blocked" assertion fails on the first
// run — which is the falsification that makes these worth having.
//
// They assert the mechanism *and* the outcome, because a method could block for
// the wrong reason (an unrelated lock, a full pool). Each case therefore also
// checks the effect the method is supposed to have once unblocked.

// lockHolder occupies a lock in an open transaction until release is called.
type lockHolder struct {
	tx   pgx.Tx
	conn *pgxpool.Conn
}

// release ends the transaction and returns the connection to the pool.
func (h *lockHolder) release() {
	_ = h.tx.Rollback(callCtx)
	h.conn.Release()
}

// holdAdvisoryLock takes an advisory lock in its own transaction, so the lock is
// held for as long as the transaction lives.
func holdAdvisoryLock(t *testing.T, pool *pgxpool.Pool, key int64) *lockHolder {
	t.Helper()
	conn, err := pool.Acquire(callCtx)
	if err != nil {
		t.Fatalf("acquire a connection for the lock holder: %v", err)
	}
	tx, err := conn.Begin(callCtx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin the lock holder transaction: %v", err)
	}
	if _, err := tx.Exec(callCtx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		_ = tx.Rollback(callCtx)
		conn.Release()
		t.Fatalf("take advisory lock %d: %v", key, err)
	}
	return &lockHolder{tx: tx, conn: conn}
}

// holdRenewalRowLock locks one key_renewals row FOR UPDATE, in its own transaction.
//
// This is the row-lock counterpart of holdAdvisoryLock, for the one site where
// the row already exists and a row lock — not an advisory lock — is the correct
// mechanism.
func holdRenewalRowLock(t *testing.T, pool *pgxpool.Pool, renewalID string) *lockHolder {
	t.Helper()
	conn, err := pool.Acquire(callCtx)
	if err != nil {
		t.Fatalf("acquire a connection for the row lock holder: %v", err)
	}
	tx, err := conn.Begin(callCtx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin the row lock holder transaction: %v", err)
	}
	var keyID, status string
	if err := tx.QueryRow(callCtx,
		`SELECT key_id, status FROM key_renewals WHERE id = $1 FOR UPDATE`, renewalID,
	).Scan(&keyID, &status); err != nil {
		_ = tx.Rollback(callCtx)
		conn.Release()
		t.Fatalf("lock renewal %s: %v", renewalID, err)
	}
	return &lockHolder{tx: tx, conn: conn}
}

// assertBlocksThenCompletes runs call on a goroutine and asserts two things:
//
//  1. It does NOT finish while the lock is held. That is the mechanism under
//     test: this is what a removed lock breaks.
//  2. It DOES finish, without error, once the lock is released. That is the
//     outcome, and it rules out "blocked for an unrelated reason" — a method
//     wedged on a full pool would still be wedged after the release.
//
// The lock is released before any failure is reported, so a failing assertion
// does not leave the goroutine and the test binary fighting over the pool.
func assertBlocksThenCompletes(t *testing.T, held *lockHolder, call func() error) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- call() }()

	blocked := true
	select {
	case err := <-done:
		// It got past the lock while the lock was held: the lock is not taken.
		blocked = false
		held.release()
		t.Fatalf("the call completed while the lock was held (err=%v), so it does not "+
			"take that lock at all. This is the deterministic falsification: with the "+
			"lock in place it cannot finish before the holder releases.", err)
	case <-time.After(300 * time.Millisecond):
	}

	held.release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the call failed after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		if !blocked {
			t.Fatal("unreachable")
		}
		t.Fatal("the call never completed after the lock was released; it is blocked on " +
			"something other than the lock (a full pool, or a lock nobody releases)")
	}
}

// TestCreateApplicationTakesThePendingCapLock is the deterministic guard for
// LockPendingApplications.
func TestCreateApplicationTakesThePendingCapLock(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	// A queue that is empty, so the ONLY thing that can stop the call is the lock
	// — not the cap itself.
	held := holdAdvisoryLock(t, s.Pool(), LockPendingApplications)

	assertBlocksThenCompletes(t, held, func() error {
		_, err := s.CreateApplication(ApplicationInput{
			Label: "locked-out", ClaimHash: agentkey.HashToken("h"), TTL: 60_000,
		}, 50, now)
		return err
	})

	// The outcome: it ran, under the cap, and landed.
	if n, err := s.CountApplications("pending"); err != nil || n != 1 {
		t.Fatalf("pending applications = %d (err %v), want 1: the call unblocked but did not do its work", n, err)
	}
}

// TestCreateManualKeyTakesTheActiveCapLock is the deterministic guard for the
// CreateManualKey half of LockActiveKeys.
func TestCreateManualKeyTakesTheActiveCapLock(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	held := holdAdvisoryLock(t, s.Pool(), LockActiveKeys)

	assertBlocksThenCompletes(t, held, func() error {
		_, _, err := s.CreateManualKey("minted-under-lock", 1, "", 100, now)
		return err
	})

	if n, err := s.CountActiveKeys(now); err != nil || n != 1 {
		t.Fatalf("active keys = %d (err %v), want 1", n, err)
	}
}

// TestClaimApplicationTakesTheActiveCapLock is the other half of the shared-lock
// property, and the one no existing test covered.
//
// The cap is enforced inside ClaimApplication only while maxActive > 0, so a
// regression that locked one minting path and not the other would let the two
// paths each spend the last free slot. Both halves are asserted separately, on
// the same key constant, which is what makes "they share one lock" testable.
func TestClaimApplicationTakesTheActiveCapLock(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60_000, now); err != nil {
		t.Fatal(err)
	}

	held := holdAdvisoryLock(t, s.Pool(), LockActiveKeys)

	assertBlocksThenCompletes(t, held, func() error {
		// maxActive > 0 is what turns the cap check (and therefore the lock) on.
		_, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now)
		return err
	})

	// The outcome: exactly one credential exists, and it is the claimed one.
	keys, err := s.ListAgentKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("agent_keys = %d rows (err %v), want exactly 1", len(keys), err)
	}
	if appAfter, err := s.GetApplication(app.ID); err != nil || appAfter.Status != domain.ApplicationClaimed {
		t.Fatalf("application status = %v (err %v), want claimed", appAfter, err)
	}
}

// TestCreateRenewalTakesThePendingRenewalLock is the deterministic guard for
// LockPendingRenewals, whose whole point is "at most one pending renewal per key".
func TestCreateRenewalTakesThePendingRenewalLock(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	key, _, err := s.CreateManualKey("renewable", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}

	held := holdAdvisoryLock(t, s.Pool(), LockPendingRenewals)

	assertBlocksThenCompletes(t, held, func() error {
		_, err := s.CreateRenewal(key.ID, 48, now)
		return err
	})

	renewals, err := s.ListRenewals("pending", 10)
	if err != nil || len(renewals) != 1 {
		t.Fatalf("pending renewals = %d (err %v), want exactly 1", len(renewals), err)
	}
}

// TestDecideRenewalTakesTheRowLockBeforeDoingAnyWork is the deterministic guard
// for the FOR UPDATE on the renewal row.
//
// The naive version of this test asserted only that DecideRenewal *blocks* while
// another transaction holds the renewal row. That is not a guard: with the
// FOR UPDATE removed the call still blocks, just later — on the final guarded
// UPDATE, which conflicts with the same row. Mutation-tested, it passed. So the
// assertion here is about WHERE the call stops: while the renewal row is held, the
// decision must not have touched the key yet.
//
// That is observable. The key's row is only written by the extension, and an
// unwritten key row is not locked; a *written but uncommitted* one is. So a
// probe from another connection can tell the two apart with FOR UPDATE NOWAIT:
//
//   - row lock held by the decision  -> the decision already extended the key, so
//     the FOR UPDATE is absent and the loser has done work it will have to undo;
//   - probe succeeds                 -> the decision is waiting on the renewal row
//     before it has touched anything, which is the property under test.
func TestDecideRenewalTakesTheRowLockBeforeDoingAnyWork(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody else holds the renewal row.
	held := holdRenewalRowLock(t, s.Pool(), renewal.ID)

	done := make(chan error, 1)
	go func() {
		_, err := s.DecideRenewal(renewal.ID, true, 48, now)
		done <- err
	}()

	// It must not complete while the row is held — and, more precisely, it must
	// not have written the key. Polled rather than sampled once so that a slow
	// scheduler cannot make a broken implementation look correct.
	extendedEarly := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			held.release()
			t.Fatalf("the decision completed while the renewal row was locked (err=%v)", err)
		default:
		}
		if agentKeyRowIsLocked(t, s.Pool(), key.ID) {
			extendedEarly = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	held.release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the decision failed after the row lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the decision never completed after the row lock was released")
	}

	if extendedEarly {
		t.Fatal("the decision had already extended the key while the renewal row was still " +
			"locked: it does not take the row lock before doing its work, so a second " +
			"decider can extend the key and only be refused afterwards")
	}

	// And it worked: exactly one extension, measured from the key's old expiry.
	after, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := now + 3600_000 + 48*3600_000; after.ExpiresAt != want {
		t.Fatalf("key expires_at = %d, want %d: one renewal must extend the key exactly once", after.ExpiresAt, want)
	}
}

// TestDecideRenewalRefusedDecisionLeavesNoExtension is the store-level property
// guard for the bug this port introduced: one renewal request must buy exactly one
// extension, even when two decisions overlap.
//
// It forces the overlap that no concurrency test can force reliably. A holder
// transaction takes the renewal row, and while the store's decision is in flight
// the holder flips the row to 'approved' and commits — which is precisely the
// interleaving a second decider would produce, arranged deterministically.
//
// Determinism, both ways round:
//
//   - The pre-fix store (unlocked status read, no row-count check) reads 'pending'
//     immediately — it does not block on the read — then extends the key and only
//     then fails the guarded UPDATE. Committing that leaves the extension behind,
//     and this test fails on the key's expiry. The read cannot "win the race" the
//     other way: it is not blocked, so it always happens before the flip below.
//   - The current store refuses, either at the locking read or at the guarded
//     UPDATE, and the key is untouched. It passes on every run.
func TestDecideRenewalRefusedDecisionLeavesNoExtension(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatal(err)
	}

	held := holdRenewalRowLock(t, s.Pool(), renewal.ID)

	done := make(chan error, 1)
	go func() {
		_, err := s.DecideRenewal(renewal.ID, true, 48, now)
		done <- err
	}()

	// Let the decision reach the row (it blocks there in the current code, and
	// sails past in the unguarded one).
	time.Sleep(300 * time.Millisecond)

	// The other decider wins the row and commits its decision.
	if _, err := held.tx.Exec(callCtx,
		`UPDATE key_renewals SET status = 'approved', decided_at = $1 WHERE id = $2`,
		now, renewal.ID); err != nil {
		held.release()
		t.Fatalf("the holder could not decide the renewal: %v", err)
	}
	if err := held.tx.Commit(callCtx); err != nil {
		held.conn.Release()
		t.Fatalf("the holder could not commit: %v", err)
	}
	held.conn.Release()

	select {
	case err := <-done:
		if !errors.Is(err, ErrApplicationDecided) {
			t.Fatalf("the losing decision returned %v, want ErrApplicationDecided", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the losing decision never returned")
	}

	// The property: the refusal must not have left an extension behind.
	after, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExpiresAt != before.ExpiresAt {
		t.Fatalf("one renewal request changed the expiry twice: %d -> %d. The losing "+
			"decision extended the key and was refused afterwards, and its rollback did not "+
			"undo the extension — this is the double grant in .ai/pitfalls/cases/"+
			"2026-10-16-decide-renewal-granted-twice.md",
			before.ExpiresAt, after.ExpiresAt)
	}
}

// isLockNotAvailable reports whether err is PostgreSQL's lock_not_available
// (SQLSTATE 55P03), which is what FOR UPDATE NOWAIT raises instead of waiting.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// agentKeyRowIsLocked reports whether some other transaction holds the key's row
// lock, using FOR UPDATE NOWAIT so the probe never blocks.
func agentKeyRowIsLocked(t *testing.T, pool *pgxpool.Pool, keyID string) bool {
	t.Helper()
	conn, err := pool.Acquire(callCtx)
	if err != nil {
		t.Fatalf("acquire a probe connection: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(callCtx)
	if err != nil {
		t.Fatalf("begin the probe transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(callCtx) }()

	var expiresAt int64
	err = tx.QueryRow(callCtx,
		`SELECT expires_at FROM agent_keys WHERE id = $1 FOR UPDATE NOWAIT`, keyID).Scan(&expiresAt)
	switch {
	case err == nil:
		return false
	case isLockNotAvailable(err):
		return true
	default:
		t.Fatalf("probe the key row: %v", err)
		return false
	}
}

// TestRenewalDoubleGrantPattern pins the *pattern*, not the store — read the
// distinction carefully, because it is the difference between a guard and a
// diagram.
//
// The store's own properties are guarded by
// TestDecideRenewalRefusedDecisionLeavesNoExtension (no extension leaks) and
// TestDecideRenewalTakesTheRowLockBeforeDoingAnyWork (the FOR UPDATE). This test
// drives the two transactions' statements by hand, in the store's exact order, to
// document the hazard itself and to show that discarding a refused transaction is
// what prevents it. It is the same shape as the cap demonstrations in
// cap_lock_pg_test.go, and like them it cannot fail because of a store change —
// which is exactly why it does not replace the two tests above.
//
// Both directions are asserted, so the file proves which half is load-bearing:
//
//   - the naive pattern (read status, extend, guarded UPDATE, commit regardless)
//     applies TWO extensions for ONE renewal request;
//   - adding only the rollback-on-refusal brings it back to one.
//
// That second case is why the store keeps the row-count check even though the
// FOR UPDATE makes it unreachable today: it is the mechanism that would still hold
// if the locking read were ever refactored away, and the ADR requires the
// `RowsAffected` semantics of a conditional UPDATE to be preserved.
func TestRenewalDoubleGrantPattern(t *testing.T) {
	const now = int64(1_700_000_000_000)
	// One renewal of 48h on a key with 1h left must land here.
	const single = now + 3600_000 + 48*3600_000

	for _, tc := range []struct {
		name             string
		rollbackOnRefuse bool
		wantExpiry       int64
	}{
		{"naive pattern doubles the grant", false, now + 3600_000 + 96*3600_000},
		{"the store's pattern grants once", true, single},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := storetest.Pool(t, MigratePG)
			ctx := callCtx

			if _, err := pool.Exec(ctx, `INSERT INTO agent_keys
				(id, name, token_hash, token_prefix, created_at, expires_at)
				VALUES ('k1', 'k', 'h1', 'p', $1, $2)`, now, now+3600_000); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO key_renewals
				(id, key_id, requested_hours, status, created_at)
				VALUES ('r1', 'k1', 48, 'pending', $1)`, now); err != nil {
				t.Fatal(err)
			}

			first, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Release()
			second, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Release()
			tx1, err := first.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx1.Rollback(ctx) }()
			tx2, err := second.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx2.Rollback(ctx) }()

			// The store's statements, verbatim.
			const readStatus = `SELECT key_id, status FROM key_renewals WHERE id = 'r1'`
			const readKey = `SELECT expires_at FROM agent_keys WHERE id = 'k1'`
			const decide = `UPDATE key_renewals
				SET status = 'approved', decided_at = $1, granted_expires_at = $2
				WHERE id = 'r1' AND status = 'pending'`
			const extend = `UPDATE agent_keys SET expires_at = $1 WHERE id = 'k1'`

			// Both transactions read the status while it is still 'pending'. This is
			// the crux: a plain SELECT takes no lock, which is why SQLite's
			// transaction-level write lock was doing the work before.
			var keyID, status string
			if err := tx1.QueryRow(ctx, readStatus).Scan(&keyID, &status); err != nil {
				t.Fatal(err)
			}
			if err := tx2.QueryRow(ctx, readStatus).Scan(&keyID, &status); err != nil {
				t.Fatal(err)
			}
			if status != "pending" {
				t.Fatalf("both transactions must observe 'pending'; got %q", status)
			}

			// Transaction 1 decides and commits.
			var base int64
			if err := tx1.QueryRow(ctx, readKey).Scan(&base); err != nil {
				t.Fatal(err)
			}
			if _, err := tx1.Exec(ctx, extend, base+48*3600_000); err != nil {
				t.Fatal(err)
			}
			if tag, err := tx1.Exec(ctx, decide, now, base+48*3600_000); err != nil {
				t.Fatal(err)
			} else if tag.RowsAffected() != 1 {
				t.Fatalf("the first decision should match its row, got %d", tag.RowsAffected())
			}
			if err := tx1.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			// Transaction 2 proceeds on its stale read and extends the key again,
			// reading the expiry the winner just committed.
			if err := tx2.QueryRow(ctx, readKey).Scan(&base); err != nil {
				t.Fatal(err)
			}
			if _, err := tx2.Exec(ctx, extend, base+48*3600_000); err != nil {
				t.Fatal(err)
			}
			tag, err := tx2.Exec(ctx, decide, now, base+48*3600_000)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("the second decision must match nothing (the winner took the row), got %d", tag.RowsAffected())
			}

			// The whole difference between the two cases: the store discards a
			// transaction whose decision was refused.
			if tc.rollbackOnRefuse {
				if err := tx2.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := tx2.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			var final int64
			if err := pool.QueryRow(ctx, `SELECT expires_at FROM agent_keys WHERE id = 'k1'`).Scan(&final); err != nil {
				t.Fatal(err)
			}
			if final != tc.wantExpiry {
				t.Fatalf("key expires_at = %d, want %d", final, tc.wantExpiry)
			}
		})
	}
}

// TestConcurrentRenewalDecisionsGrantExactlyOnce is the realistic version of the
// same property. With the store's rollback in place it is deterministic in the
// direction that matters for a gate — exactly one winner and exactly one
// extension, on every run — but it is deliberately NOT the guard for the double
// grant: the pre-fix code survived repeated runs of this shape, which is why the
// forced interleaving in TestRenewalDoubleGrantPattern is the one that may be
// trusted to fail.
func TestConcurrentRenewalDecisionsGrantExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatal(err)
	}

	const racers = 8

	results := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			_, err := s.DecideRenewal(renewal.ID, true, 48, now)
			results <- err
		}()
	}
	close(start)

	wins, refused := 0, 0
	for range racers {
		switch err := <-results; {
		case err == nil:
			wins++
		case errors.Is(err, ErrApplicationDecided):
			refused++
		default:
			t.Errorf("a losing decision failed with %v, want ErrApplicationDecided", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d concurrent decisions succeeded, want exactly 1", wins, racers)
	}
	if refused != racers-1 {
		t.Errorf("%d decisions were refused, want %d", refused, racers-1)
	}

	// The decisive assertion: ONE renewal request bought ONE extension. A double
	// grant is exactly what an unlocked status read produces, and it is what this
	// test measures — the win count alone would not catch it, because the loser
	// can be refused *after* its extension has already been applied.
	after, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	single := now + 3600_000 + 48*3600_000
	if after.ExpiresAt != single {
		t.Fatalf("key expires_at = %d, want %d: more than one extension was applied for one renewal request",
			after.ExpiresAt, single)
	}
}
