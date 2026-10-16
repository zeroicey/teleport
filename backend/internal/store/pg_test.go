package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPGPool returns a pool against the *test* database, or skips.
//
// Two guards, both deliberate:
//
//  1. Without TEST_PG_DSN the PostgreSQL tests skip, so `go test ./...` works on
//     a machine with no database and the gate stays green.
//  2. The database name must contain "test". These tests TRUNCATE tables, and a
//     stray environment variable must not be able to point them at the database
//     that holds real reports and credentials. A missing database is a visible
//     failure; a wrongly-targeted one is data loss.
func testPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_PG_DSN"))
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set; skipping PostgreSQL-backed tests")
	}
	name := ""
	if u, err := url.Parse(dsn); err == nil {
		name = strings.TrimPrefix(u.Path, "/")
	}
	if !strings.Contains(name, "test") {
		t.Fatalf("refusing to run: TEST_PG_DSN names database %q, which does not contain \"test\"", name)
	}

	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// agent_keys cascades to key_renewals; reports cascades to share_tokens.
	if _, err := pool.Exec(ctx, `TRUNCATE reports, agent_keys, key_applications CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// TestPGMigrationsApplyAndAreIdempotent proves the baseline builds a schema and
// that re-running it is a no-op rather than an error.
func TestPGMigrationsApplyAndAreIdempotent(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()

	var version string
	if err := pool.QueryRow(ctx,
		`SELECT version FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !strings.HasSuffix(version, ".sql") {
		t.Fatalf("migration version %q does not look like a file name", version)
	}

	for _, table := range []string{"reports", "share_tokens", "agent_keys", "key_applications", "key_renewals"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %q was not created", table)
		}
	}

	if err := MigratePG(ctx, pool); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
}

// TestPGTimestampsAreBigint is the guard for the single most likely translation
// mistake: SQLite's INTEGER is 64-bit, PostgreSQL's is 32-bit, and a millisecond
// timestamp does not fit in 32 bits. With INTEGER here, every insert of a real
// report would fail with "integer out of range" — so this asserts a *current*
// epoch-millisecond value round-trips, not just that the column exists.
func TestPGTimestampsAreBigint(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	if now < 1<<31 {
		t.Fatalf("this test is meaningless before 2038: now=%d", now)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		VALUES ('r1', 't', 'general', 'markdown', 'body', '{}', $1, $1)`, now); err != nil {
		t.Fatalf("insert with an epoch-millisecond timestamp: %v (is created_at INTEGER instead of BIGINT?)", err)
	}

	var got int64
	if err := pool.QueryRow(ctx, `SELECT created_at FROM reports WHERE id = 'r1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != now {
		t.Fatalf("created_at round-tripped as %d, want %d", got, now)
	}

	// And a counter must survive past the int32 ceiling too.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_keys (id, name, token_hash, token_prefix, created_at, request_count)
		VALUES ('k1', 'n', 'h1', 'p', $1, $2)`, now, int64(3_000_000_000)); err != nil {
		t.Fatalf("request_count above 2^31: %v (is it INTEGER instead of BIGINT?)", err)
	}
}

// TestPGMetadataCheckRejectsInvalidJSON pins the `IS JSON` translation.
//
// The SQLite schema used `json_valid(metadata)`. Translating that to a cast
// inside the CHECK would raise an exception rather than rejecting the row, which
// changes what a constraint violation looks like to the caller — so this asserts
// a violation, not a particular error string.
func TestPGMetadataCheckRejectsInvalidJSON(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	insert := func(metadata string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
			VALUES ($1, 't', 'general', 'markdown', 'body', $2, $3, $3)`,
			"meta-"+metadata, metadata, now)
		return err
	}

	if err := insert(`{"target":"host"}`); err != nil {
		t.Fatalf("valid JSON was rejected: %v", err)
	}
	// The assertion is on the SQLSTATE, not on "an error happened". Translating
	// `json_valid(x)` to a cast inside the CHECK also produces an error — it
	// raises one instead of returning false — so a bare `err != nil` check passes
	// for BOTH translations and pins nothing. 23514 is check_violation: the
	// constraint rejected the row, which is what `json_valid` means.
	err := insert(`not json at all`)
	if err == nil {
		t.Fatal("invalid JSON was accepted; the metadata IS JSON check is not active")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("invalid JSON produced %v (SQLSTATE %s), want a CHECK violation 23514.\n"+
			"A cast inside the CHECK raises an exception instead of rejecting the row — "+
			"that is a different contract from json_valid()", err, sqlStateOf(err))
	}
}

// sqlStateOf is a test-only helper so the failure message names the SQLSTATE
// even when the error is not a *pgconn.PgError.
func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "n/a"
}

// TestPGUpdatedAtTriggerMatchesTheSQLiteCondition pins the trigger's behaviour,
// including the condition that makes it almost never fire.
func TestPGUpdatedAtTriggerMatchesTheSQLiteCondition(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	created := time.Now().Add(-time.Hour).UnixMilli()

	if _, err := pool.Exec(ctx, `
		INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		VALUES ('r1', 't', 'general', 'markdown', 'body', '{}', $1, $1)`, created); err != nil {
		t.Fatal(err)
	}

	// Case 1: the caller leaves updated_at alone -> the trigger stamps it.
	if _, err := pool.Exec(ctx,
		`UPDATE reports SET title = 'changed' WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	var stamped int64
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM reports WHERE id = 'r1'`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped <= created {
		t.Errorf("updated_at was not stamped by the trigger: %d (created_at %d)", stamped, created)
	}
	if stamped < time.Now().Add(-time.Minute).UnixMilli() {
		t.Errorf("the trigger stamped a stale value: %d", stamped)
	}

	// Case 2: the caller sets updated_at explicitly -> the trigger must NOT
	// overwrite it. This is the condition the Go update path relies on: it always
	// changes the column (GREATEST(now, updated_at+1)) precisely so this trigger
	// never fires and cannot re-stamp from a coarser clock.
	explicit := stamped + 1
	if _, err := pool.Exec(ctx,
		`UPDATE reports SET title = 'changed again', updated_at = $1 WHERE id = 'r1'`, explicit); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM reports WHERE id = 'r1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != explicit {
		t.Errorf("the trigger overwrote an explicit updated_at: got %d, want %d", after, explicit)
	}
}

// TestPGDoublePrecisionHours pins the REAL → DOUBLE PRECISION translation. With
// PostgreSQL's float4 REAL, the value below would be rounded on the way in and
// the Go code (which scans float64) would silently see a different number.
func TestPGDoublePrecisionHours(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	const hours = 24.123456789012345
	if _, err := pool.Exec(ctx, `
		INSERT INTO key_applications (id, claim_hash, label, requested_hours, status, created_at, expires_at)
		VALUES ('a1', 'h', 'label', $1, 'pending', $2, $2)`, hours, now); err != nil {
		t.Fatal(err)
	}
	var got float64
	if err := pool.QueryRow(ctx,
		`SELECT requested_hours FROM key_applications WHERE id = 'a1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != hours {
		t.Errorf("requested_hours round-tripped as %v, want %v (is it REAL instead of DOUBLE PRECISION?)", got, hours)
	}
}

// TestPGIntegerFlagsStayIntegers pins the decision NOT to translate the 0/1 flags
// to BOOLEAN: the Go queries compare `is_active = 1`, and a BOOLEAN column would
// make every one of those statements a type error.
func TestPGIntegerFlagsStayIntegers(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if _, err := pool.Exec(ctx, `
		INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		VALUES ('r1', 't', 'general', 'markdown', 'body', '{}', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active)
		VALUES ('tok', 'r1', $1, 0, 1)`, now); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM share_tokens WHERE is_active = 1`).Scan(&n); err != nil {
		t.Fatalf("the Go idiom `is_active = 1` does not work: %v", err)
	}
	if n != 1 {
		t.Errorf("is_active = 1 matched %d rows, want 1", n)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE share_tokens SET is_active = 2 WHERE token = 'tok'`); err == nil {
		t.Error("is_active accepted 2; the CHECK is not active")
	}
}

// TestPGCascadeAndView pins the two remaining structural translations: the
// cascade that deletes a report's links, and the view whose liveness predicate
// was rewritten from strftime to EXTRACT.
func TestPGCascadeAndView(t *testing.T) {
	pool := testPGPool(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if _, err := pool.Exec(ctx, `
		INSERT INTO reports (id, title, category, format, content, metadata, created_at, updated_at)
		VALUES ('r1', 't', 'general', 'markdown', 'body', '{}', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	// One live link, one that expired an hour ago, and one that never expires.
	for _, tok := range []struct {
		name    string
		expires int64
	}{{"live", now + time.Hour.Milliseconds()}, {"gone", now - time.Hour.Milliseconds()}, {"forever", 0}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO share_tokens (token, report_id, created_at, expires_at, is_active)
			VALUES ($1, 'r1', $2, $3, 1)`, tok.name, now, tok.expires); err != nil {
			t.Fatal(err)
		}
	}

	var notExpired int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM v_live_shares WHERE not_expired`).Scan(&notExpired); err != nil {
		t.Fatalf("query the view: %v (is not_expired still 0/1 instead of boolean?)", err)
	}
	if notExpired != 2 {
		t.Errorf("v_live_shares reports %d live links, want 2 (live + forever)", notExpired)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM reports WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM share_tokens`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d share_tokens survived the report deletion; the cascade is not active", left)
	}
}
