// Package storetest is the PostgreSQL test harness.
//
// It is a normal package rather than a _test.go file because two packages need
// it: internal/store's own tests, and internal/api's, which build a store in
// order to drive the HTTP contract. A helper compiled into a _test.go file is
// invisible outside its own package, so the alternative is two copies of the
// same isolation logic — exactly the kind of duplication that lets one copy rot.
//
// The harness gives every test its **own schema**, created from the embedded
// migrations and dropped when the test ends. See
// .ai/decisions/2026-10-16-p2-test-strategy.md for why. The short version: a
// shared, long-lived test database drifted away from the committed migration
// once already
// (.ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md), and a
// schema whose entire lifetime is one test cannot do that.
//
// This package deliberately does NOT import the package under test. It is
// imported by internal/store's own test files, so importing internal/store here
// is a cycle (`import cycle not allowed in test`). The one thing it needs from
// there — the migrator — arrives as the Migrate parameter instead, which also
// makes explicit that the harness applies *the caller's* migrator and never a
// private copy of the DDL.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrateFunc applies the schema to a freshly created, empty schema. Callers
// pass store.MigratePG.
type MigrateFunc func(context.Context, *pgxpool.Pool) error

// The DSN guard's failure modes, as values rather than as messages, so that the
// decision can be tested without a *testing.T and without a database.
var (
	// ErrDSNNotSet means the caller should skip: no database was offered.
	ErrDSNNotSet = errors.New("TEST_PG_DSN is not set")
	// ErrDSNNotURL means the DSN is not in the URL form this harness requires.
	ErrDSNNotURL = errors.New("TEST_PG_DSN is not a URL-style DSN")
	// ErrDSNDatabaseNotTest means the DSN points at a database that does not
	// look like a test database. This is a refusal, not a skip.
	ErrDSNDatabaseNotTest = errors.New("refusing to run: TEST_PG_DSN names a database that does not contain " + strconv.Quote("test"))
)

// CheckDSN decides whether this harness may run against a DSN.
//
// It is separated from DSN so that the refusal can be asserted directly. The
// guard is the only thing between a mistyped environment variable and a harness
// that creates and drops schemas in the database holding real reports, and a
// refusal reachable only through t.Fatalf can be tested only by making a test
// fail — which fails its parent too, so the suite could never be green.
func CheckDSN(dsn string) error {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ErrDSNNotSet
	}
	if _, err := url.Parse(dsn); err != nil {
		return fmt.Errorf("%w: %v", ErrDSNNotURL, err)
	}
	if name := databaseName(dsn); !strings.Contains(name, "test") {
		return fmt.Errorf("%w (%q)", ErrDSNDatabaseNotTest, name)
	}
	return nil
}

// DSN returns the test database DSN, or skips the test.
//
// Two guards, both deliberate:
//
//  1. Without TEST_PG_DSN the tests skip, so `go test ./...` still works on a
//     machine with no database and the quick gate stays runnable. The real gate
//     is scripts/test-pg.sh, which *refuses to run* without a DSN — a silent
//     skip is how "verified against the real database" became a false claim on
//     2026-10-16.
//  2. The database name must contain "test". This harness creates and drops
//     schemas in whatever database it is pointed at, and a stray environment
//     variable must not be able to do that to the database holding real reports
//     and credentials. A missing database is a visible failure; a
//     wrongly-targeted one is data loss.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_PG_DSN"))
	switch err := CheckDSN(dsn); {
	case errors.Is(err, ErrDSNNotSet):
		t.Skip("TEST_PG_DSN not set; skipping PostgreSQL-backed tests")
	case err != nil:
		t.Fatal(err)
	}
	return dsn
}

// SkipWithoutDatabase skips the calling test when TEST_PG_DSN is unset, and
// refuses (it does not skip) when one is set but unusable.
//
// Pool and Schema perform the same check, so this is only needed when a test
// must skip *before* it can reach them. That is not a nicety: a test that
// obtains its database inside t.Run cannot infer "the subtest was skipped" from
// the return value, because t.Run reports true for a skip — so the outer test
// proceeds and reports a bogus failure. Calling this first makes the whole test
// skip, which is the honest outcome.
func SkipWithoutDatabase(t *testing.T) {
	t.Helper()
	DSN(t)
}

// AdminPool returns a pool on the test database with the *default* search_path.
//
// It is for the few assertions that are about the database rather than about a
// test's data — "does this schema still exist after cleanup?". Ordinary tests
// want Pool, which is confined to their own schema.
func AdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := DSN(t)

	pool, err := openPool(context.Background(), dsn, 1)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Pool returns a pool whose connections resolve unqualified names to a private
// schema that was just created by applying the embedded migrations.
//
// Everything a test does is therefore confined to a schema that did not exist a
// moment ago and is dropped when the test ends, so two tests — including two
// running at once — cannot see each other's rows, or each other's *counts*. That
// last part is what the quota and cap tests need, and what a TRUNCATE-based
// shared database cannot promise.
func Pool(t *testing.T, migrate MigrateFunc) *pgxpool.Pool {
	t.Helper()
	pool, _ := Schema(t, migrate)
	return pool
}

// Schema is Pool, plus the schema's name, for tests that need to name it.
func Schema(t *testing.T, migrate MigrateFunc) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	dsn := DSN(t)

	admin, err := openPool(ctx, dsn, 1)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	schema := "t_" + randomHex(t, 8)

	// The schema is created BEFORE the scoped pool exists. This ordering is
	// load-bearing: unqualified DDL resolves to the first *existing* schema in
	// search_path, so if the scoped pool connected first, pgxpool's connections
	// would carry search_path=<schema that does not exist yet> and the
	// migrations would silently create every table in the default schema
	// instead. See assertObjectsLandedIn below, which fails loudly if that ever
	// happens for any other reason.
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	// LIFO ordering, declared here so it is not a puzzle at the bottom of the
	// function: pool.Close (registered below) runs first, then this drop, then
	// admin.Close (registered above).
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	scopedDSN, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	pool, err := openPool(ctx, scopedDSN, 4)
	if err != nil {
		t.Fatalf("open pool on schema %s: %v", schema, err)
	}
	t.Cleanup(pool.Close)

	// The migrations are the only source of the schema. There is no checked-in
	// dump to fall out of date.
	if err := migrate(ctx, pool); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	assertObjectsLandedIn(t, pool, schema)

	return pool, schema
}

// assertObjectsLandedIn fails if the migrations did not create their tables in
// the schema this test owns.
//
// Without it, a misapplied search_path produces the worst possible outcome: the
// test passes, against a shared schema, while looking perfectly isolated. This is
// the same shape of mistake as the stale-database incident — the assertion is
// about *where* the objects are, not just that a query worked.
func assertObjectsLandedIn(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"reports", "share_tokens", "agent_keys", "schema_migrations"} {
		var landed int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = $1 AND table_name = $2`, schema, table,
		).Scan(&landed); err != nil {
			t.Fatalf("look for %s in schema %s: %v", table, schema, err)
		}
		if landed == 0 {
			t.Fatalf("table %q was not created in schema %q: search_path did not take effect, "+
				"so the migrations ran against another schema", table, schema)
		}
	}
}

// openPool opens a pgx pool with a deliberately SMALL connection budget.
//
// This database lives on a PostgreSQL instance shared with other services
// (clip/relay, registry, serenique), whose max_connections is 100 for everyone
// together. A test harness is not entitled to a large slice of that, and the
// quick way to take one by accident is to open a pool inside a loop: the pools
// are released by t.Cleanup, which runs when the *test* ends, not when the
// iteration does — so N iterations hold N pools open at once. Measured: 20
// iterations x two pools exhausted the instance with
// `FATAL: sorry, too many clients already (SQLSTATE 53300)`, which would have
// denied connections to the other services on the same instance.
//
// Two consequences, both enforced by the callers here:
//
//  1. The budget is as small as the tests allow (1 for the admin pool, 4 for a
//     test's own pool). Concurrency tests still oversubscribe it — 24 goroutines
//     share 4 connections — which is what makes them meaningful, since the thing
//     under test is whether the *database* serialises them, not whether the pool
//     does.
//  2. A test that needs an empty database more than once must truncate its own
//     tables, not call Schema again. See
//     TestPGAdvisoryLockedCapIsExactUnderContention for the pattern.
func openPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// withSearchPath returns the DSN with search_path pinned to exactly one schema.
//
// Pinning it to the test schema *alone* — never "<schema>, public" — is what
// makes a missing object an error instead of a silent fallback to a shared
// schema. pgx forwards any query parameter it does not recognise as a connection
// runtime parameter, so this reaches the server as a startup parameter and
// applies to every connection the pool opens, not just the first.
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// databaseName extracts the database name from a URL-style DSN.
func databaseName(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// randomHex returns n random hex characters, for schema names that cannot
// collide between tests.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(buf)[:n]
}
