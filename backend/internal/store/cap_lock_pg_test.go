package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/teleport/backend/internal/store/storetest"
)

// The tests in this file exist to settle one question with evidence rather than
// assumption: **does SQLite's single-writer guarantee actually stop protecting
// the caps in PostgreSQL?**
//
// The P2 plan says the pending cap and the active-key cap "must be explicitly
// locked". That is a claim about behaviour, and the whole point of P0/P1 was to
// stop accepting such claims on faith. So this file first *demonstrates* the
// hazard deterministically, then validates the lock that will replace it.
//
// They deliberately drive raw SQL rather than store methods: internal/store is
// still the SQLite implementation at this stage, so the store cannot be the
// subject yet. What is pinned here is the *pattern* the ported store must use,
// keyed on the exported Lock* constants in pg.go — the same constants
// CreateApplication and friends will pass to pg_advisory_xact_lock.

const (
	// countPendingApplicationsSQL is the exact predicate CreateApplication uses
	// to enforce KEY_MAX_PENDING.
	countPendingApplicationsSQL = `SELECT COUNT(*) FROM key_applications WHERE status = 'pending'`

	// insertPendingApplicationSQL mirrors the column list of CreateApplication.
	insertPendingApplicationSQL = `
		INSERT INTO key_applications
		  (id, claim_hash, label, purpose, requested_hours, status, created_at,
		   expires_at, decided_at, claim_deadline, issued_key_id, requester_ip,
		   user_agent, approved_name, approved_hours, approved_note)
		VALUES ($1, 'h', 'label', '', 1, 'pending', $2, $2, 0, 0, '', '', '', '', 0, '')`

	// takeAdvisoryLockSQL blocks until this transaction holds the key's lock, and
	// releases it automatically when the transaction ends.
	takeAdvisoryLockSQL = `SELECT pg_advisory_xact_lock($1)`
)

// TestPGCapIsNotProtectedByReadCommitted is the deterministic proof of the
// hazard, and it is deterministic on purpose.
//
// A statistical version — "run 24 goroutines and see whether the cap is
// exceeded" — would be flaky in the other direction: the race not reproducing
// would be read as "there is no problem". Here the interleaving is forced, so
// the hazard is either present or the test fails for a stated reason.
func TestPGCapIsNotProtectedByReadCommitted(t *testing.T) {
	pool := storetest.Pool(t, MigratePG)
	ctx := context.Background()
	now := NowMS()
	const cap = 1

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
	defer tx1.Rollback(ctx)
	tx2, err := second.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)

	// Step 1: both transactions count the queue in the same instant, and both
	// see it empty. Neither read is blocked, because a plain COUNT takes no lock
	// — that is the whole difficulty.
	var observed []int
	for i, tx := range []pgx.Tx{tx1, tx2} {
		var pending int
		if err := tx.QueryRow(ctx, countPendingApplicationsSQL).Scan(&pending); err != nil {
			t.Fatalf("transaction %d count: %v", i+1, err)
		}
		observed = append(observed, pending)
	}
	if observed[0] != 0 || observed[1] != 0 {
		t.Fatalf("both transactions must observe an empty queue; saw %v", observed)
	}

	// Step 2: each independently concludes it is under the cap of 1, and inserts.
	if _, err := tx1.Exec(ctx, insertPendingApplicationSQL, "racer-1", now); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := tx2.Exec(ctx, insertPendingApplicationSQL, "racer-2", now); err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("second commit: %v", err)
	}

	// The cap of 1 was exceeded, and nothing anywhere reported an error.
	var stored int
	if err := pool.QueryRow(ctx, countPendingApplicationsSQL).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("this test is the proof of a hazard that no longer exists: expected 2 rows "+
			"past a cap of 1 under READ COMMITTED, got %d. If a lock was added to the *predicate* "+
			"itself, re-read this file — the demonstration is deterministic, so %d means the "+
			"interleaving changed, not that the cap is now safe", stored, stored)
	}
	if cap != 1 {
		t.Fatal("unreachable")
	}
}

// TestPGAdvisoryLockSerialisesTheCapCheck is the other half: the lock that will
// replace SQLite's single writer must actually block the second transaction, and
// the second transaction must then see the first one's insert.
func TestPGAdvisoryLockSerialisesTheCapCheck(t *testing.T) {
	pool := storetest.Pool(t, MigratePG)
	ctx := context.Background()
	now := NowMS()
	const cap = 1

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
	defer tx1.Rollback(ctx)

	if _, err := tx1.Exec(ctx, takeAdvisoryLockSQL, LockPendingApplications); err != nil {
		t.Fatalf("first transaction could not take the lock: %v", err)
	}
	var pending int
	if err := tx1.QueryRow(ctx, countPendingApplicationsSQL).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending >= cap {
		t.Fatalf("the queue should be empty, saw %d", pending)
	}
	if _, err := tx1.Exec(ctx, insertPendingApplicationSQL, "holder", now); err != nil {
		t.Fatal(err)
	}

	// The second transaction must block. That blocking *is* the mechanism that
	// replaces SQLite's single writer, so it is asserted rather than assumed.
	tx2, err := second.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan error, 1)
	go func() {
		_, err := tx2.Exec(ctx, takeAdvisoryLockSQL, LockPendingApplications)
		locked <- err
	}()

	select {
	case err := <-locked:
		t.Fatalf("the second transaction took the lock while the first held it (err=%v); "+
			"the cap check is not serialised", err)
	case <-time.After(250 * time.Millisecond):
		// Still waiting, which is the required behaviour.
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	select {
	case err := <-locked:
		if err != nil {
			t.Fatalf("the second transaction failed to take the lock after the first committed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second transaction never acquired the lock after the first committed")
	}

	// And now, having waited, it must see the first transaction's insert and
	// refuse — which is the point of serialising.
	if err := tx2.QueryRow(ctx, countPendingApplicationsSQL).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending < cap {
		t.Errorf("the second transaction counted %d pending against a cap of %d; "+
			"it would have inserted past the cap even though it waited for the lock", pending, cap)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("second rollback: %v", err)
	}

	var stored int
	if err := pool.QueryRow(ctx, countPendingApplicationsSQL).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != cap {
		t.Errorf("%d applications stored, want exactly %d", stored, cap)
	}
}

// TestPGAdvisoryLockedCapIsExactUnderContention is the realistic version: 24
// applicants racing for 3 slots, which is the shape of the flood the cap exists
// to absorb. Unlike the naive pattern, this must be exact every time — and it is
// exact *because* of the lock, not because the race is rare.
//
// Rounds reuse ONE pool and truncate between them. That is not a style choice:
// each pool holds its connections until the end of the test, this PostgreSQL
// instance is shared with other services, and opening a pool per round is how a
// test starves them. See storetest.openPool.
func TestPGAdvisoryLockedCapIsExactUnderContention(t *testing.T) {
	ctx := context.Background()
	now := NowMS()
	const (
		cap     = 3
		racers  = 24
		rounds  = 5
		lockKey = LockPendingApplications
	)
	pool := storetest.Pool(t, MigratePG)

	for round := 0; round < rounds; round++ {
		if _, err := pool.Exec(ctx, `TRUNCATE key_applications CASCADE`); err != nil {
			t.Fatalf("round %d: truncate: %v", round, err)
		}

		var wins int64
		var failures int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				won, err := applyUnderAdvisoryLock(ctx, pool, lockKey, cap, fmt.Sprintf("r%d-a%d", round, i), now)
				if err != nil {
					atomic.AddInt64(&failures, 1)
					return
				}
				if won {
					atomic.AddInt64(&wins, 1)
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if failures > 0 {
			t.Errorf("round %d: %d of %d applicants failed with an error rather than a clean "+
				"refusal; a raw error here becomes a 500 in production under exactly the load "+
				"the cap exists to absorb", round, failures, racers)
		}
		if wins != cap {
			t.Errorf("round %d: %d of %d applicants won, want exactly %d", round, wins, racers, cap)
		}
		var stored int
		if err := pool.QueryRow(ctx, countPendingApplicationsSQL).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != cap {
			t.Errorf("round %d: %d applications stored, want %d", round, stored, cap)
		}
	}
}

// applyUnderAdvisoryLock performs exactly the sequence the ported
// CreateApplication must perform: take the lock, count, insert only if under the
// cap, commit. It returns whether this caller won a slot.
//
// A refusal is a clean `false, nil` — never an error — because the API answers
// 429/503 for a cap, not 500.
func applyUnderAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, lockKey int64, cap int, id string, now int64) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, takeAdvisoryLockSQL, lockKey); err != nil {
		return false, err
	}
	var pending int
	if err := tx.QueryRow(ctx, countPendingApplicationsSQL).Scan(&pending); err != nil {
		return false, err
	}
	if pending >= cap {
		return false, nil
	}
	if _, err := tx.Exec(ctx, insertPendingApplicationSQL, id, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
